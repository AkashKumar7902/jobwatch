package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/match"
	"jobwatch/internal/model"
	"jobwatch/internal/notify"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

type fakeSource struct {
	identity    string
	statePrefix string
	jobs        []model.Job
	err         error
}

func (f *fakeSource) Company() string                              { return "acme" }
func (f *fakeSource) Fetch(_ context.Context) ([]model.Job, error) { return f.jobs, f.err }
func (f *fakeSource) Identity() string {
	if f.identity == "" {
		return "test/acme"
	}
	return f.identity
}
func (f *fakeSource) StatePrefix() string { return f.statePrefix }

type matchAll struct{}

func (matchAll) Name() string { return "all" }
func (matchAll) Match(context.Context, model.Job) (match.Result, error) {
	return match.Result{Matched: true, Reason: "test"}, nil
}

// flakyNotifier fails until failuresLeft hits zero, then succeeds.
type flakyNotifier struct {
	failuresLeft int
	batches      [][]notify.Match
}

func (f *flakyNotifier) Name() string { return "flaky" }
func (f *flakyNotifier) Notify(_ context.Context, m []notify.Match) error {
	if f.failuresLeft > 0 {
		f.failuresLeft--
		return errors.New("smtp down")
	}
	f.batches = append(f.batches, m)
	return nil
}

func newRunner(t *testing.T, n notify.Notifier, seed, dry bool) (*Runner, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return &Runner{
		Sources:     nil,
		Matcher:     matchAll{},
		Notifiers:   []notify.Notifier{n},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
		SeedOnly:    seed,
		DryRun:      dry,
	}, st
}

var testJob = model.Job{ID: "test/acme/1", Company: "Acme", Title: "Junior Dev", URL: "https://x"}

// A failed delivery must be retried on the next cycle, and a successful
// delivery must not repeat on the cycle after that.
func TestFailedDeliveryRetriesThenStops(t *testing.T) {
	n := &flakyNotifier{failuresLeft: 1}
	r, st := newRunner(t, n, false, false)
	r.Sources = testSources()

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("first run should report the notifier failure")
	}
	if len(n.batches) != 0 {
		t.Fatalf("no batch should be delivered yet, got %d", len(n.batches))
	}
	rec, ok := st.Get(testJob.ID)
	if !ok || !rec.Matched || rec.Notified {
		t.Fatalf("after failed delivery, record should be pending: %+v (ok=%v)", rec, ok)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("second run should deliver: %v", err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 {
		t.Fatalf("second run should deliver the pending match once, got %v", n.batches)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 {
		t.Fatalf("third run must not re-deliver, got %d batches", len(n.batches))
	}
}

// A completed non-match is durable across process restarts and is not
// evaluated again during a normal run.
func TestNoMatchIsNotRetriedAfterStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var evaluated []string
	n := &flakyNotifier{}

	openRunner := func() (*Runner, *store.Store) {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return &Runner{
			Sources:     testSources(),
			Matcher:     countingMatcher{ids: &evaluated},
			Notifiers:   []notify.Notifier{n},
			Store:       st,
			Log:         log.New(io.Discard, "", 0),
			Concurrency: 1,
		}, st
	}

	r, st := openRunner()
	if err := r.RunOnce(context.Background()); err != nil {
		st.Close()
		t.Fatal(err)
	}
	rec, ok := st.Get(testJob.ID)
	if !ok || rec.Matched || rec.Notified {
		st.Close()
		t.Fatalf("non-match should be recorded as processed: %+v (ok=%v)", rec, ok)
	}
	st.Close()

	r, st = openRunner()
	defer st.Close()
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(evaluated) != 1 {
		t.Fatalf("matcher evaluated %v, want the job exactly once", evaluated)
	}
	if len(n.batches) != 0 {
		t.Fatalf("non-match must never be delivered, got %v", n.batches)
	}
}

type matcherFunc struct {
	name string
	fn   func(context.Context, model.Job) (match.Result, error)
}

func (m matcherFunc) Name() string { return m.name }
func (m matcherFunc) Match(ctx context.Context, job model.Job) (match.Result, error) {
	return m.fn(ctx, job)
}

func TestMatcherErrorsDeferOnlyIndeterminateJobs(t *testing.T) {
	deferredJob := model.Job{ID: "test/acme/deferred", Company: "Acme", Title: "Deferred Dev", URL: "https://x/deferred"}
	matchedJob := model.Job{ID: "test/acme/matched", Company: "Acme", Title: "Matched Dev", URL: "https://x/matched"}
	rejectedJob := model.Job{ID: "test/acme/rejected", Company: "Acme", Title: "Rejected Dev", URL: "https://x/rejected"}

	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.Sources = []source.Source{&fakeSource{jobs: []model.Job{deferredJob, matchedJob, rejectedJob}}}
	var calls []string
	r.Matcher = matcherFunc{name: "mixed", fn: func(_ context.Context, job model.Job) (match.Result, error) {
		calls = append(calls, job.ID)
		switch job.ID {
		case deferredJob.ID:
			return match.Result{}, errors.New("provider unavailable")
		case matchedJob.ID:
			return match.Result{Matched: true, Reason: "fits"}, nil
		default:
			return match.Result{Matched: false, Reason: "too senior"}, nil
		}
	}}

	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 matcher evaluation(s) deferred") {
		t.Fatalf("RunOnce = %v, want one deferred matcher error", err)
	}
	if _, ok := st.Get(deferredJob.ID); ok {
		t.Fatal("indeterminate job was recorded instead of deferred")
	}
	matched, ok := st.Get(matchedJob.ID)
	if !ok || !matched.Matched || !matched.Notified {
		t.Fatalf("confirmed match was not delivered and persisted: %+v (ok=%v)", matched, ok)
	}
	rejected, ok := st.Get(rejectedJob.ID)
	if !ok || rejected.Matched || rejected.Notified {
		t.Fatalf("confirmed rejection was not persisted: %+v (ok=%v)", rejected, ok)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != matchedJob.ID {
		t.Fatalf("delivered batches = %v, want only confirmed match", n.batches)
	}

	calls = nil
	r.Matcher = matcherFunc{name: "recovered", fn: func(_ context.Context, job model.Job) (match.Result, error) {
		calls = append(calls, job.ID)
		return match.Result{Matched: true, Reason: "provider recovered"}, nil
	}}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if len(calls) != 1 || calls[0] != deferredJob.ID {
		t.Fatalf("recovery evaluated %v, want only deferred job", calls)
	}
	if len(n.batches) != 2 || len(n.batches[1]) != 1 || n.batches[1][0].Job.ID != deferredJob.ID {
		t.Fatalf("recovery batches = %v", n.batches)
	}
}

func TestPendingDeliveryMatcherErrorPreservesThenRecovers(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.Sources = testSources()
	original := store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "Acme: Junior Dev", Matched: true}
	st.Add(testJob.ID, original)
	calls := 0
	r.Matcher = matcherFunc{name: "unavailable", fn: func(context.Context, model.Job) (match.Result, error) {
		calls++
		return match.Result{}, errors.New("provider unavailable")
	}}

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("provider error should make the run fail visibly")
	}
	if calls != 1 {
		t.Fatalf("pending delivery called matcher %d time(s), want 1", calls)
	}
	if len(n.batches) != 0 {
		t.Fatalf("indeterminate pending delivery sent %v", n.batches)
	}
	got, _ := st.Get(testJob.ID)
	if got != original {
		t.Fatalf("pending record changed after provider error: got %+v, want %+v", got, original)
	}

	r.Matcher = matcherFunc{name: "recovered", fn: func(context.Context, model.Job) (match.Result, error) {
		calls++
		return match.Result{Matched: true, Reason: "still fits"}, nil
	}}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("matcher calls after recovery = %d, want 2", calls)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != testJob.ID {
		t.Fatalf("recovered pending delivery batches = %v", n.batches)
	}
	got, _ = st.Get(testJob.ID)
	if !got.Matched || !got.Notified || !got.FirstSeen.Equal(original.FirstSeen) {
		t.Fatalf("pending record after recovery = %+v", got)
	}
}

func TestPendingDeliveryFreshRejectionClearsPendingState(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.Sources = testSources()
	st.Add(testJob.ID, store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "Acme: Junior Dev", Matched: true})
	r.Matcher = matcherFunc{name: "recovered", fn: func(context.Context, model.Job) (match.Result, error) {
		return match.Result{Matched: false, Reason: "freshly rejected"}, nil
	}}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get(testJob.ID)
	if rec.Matched || rec.Notified {
		t.Fatalf("fresh rejection did not clear poisoned pending state: %+v", rec)
	}
	if len(n.batches) != 0 {
		t.Fatalf("freshly rejected pending record delivered %v", n.batches)
	}
}

func TestMatcherErrorDuringRescanPreservesStoredReject(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.Sources = testSources()
	r.Rescan = true
	original := store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "Acme: Junior Dev"}
	st.Add(testJob.ID, original)
	r.Matcher = matcherFunc{name: "unavailable", fn: func(context.Context, model.Job) (match.Result, error) {
		return match.Result{}, errors.New("provider unavailable")
	}}

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("rescan should report the deferred matcher error")
	}
	got, _ := st.Get(testJob.ID)
	if got != original {
		t.Fatalf("stored reject changed on indeterminate rescan: got %+v, want %+v", got, original)
	}
	if len(n.batches) != 0 {
		t.Fatalf("indeterminate rescan delivered %v", n.batches)
	}
}

func TestDeferredErrorSummaryIsBounded(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	jobs := make([]model.Job, 7)
	for i := range jobs {
		jobs[i] = model.Job{ID: fmt.Sprintf("test/acme/%d", i), Company: "Acme", Title: fmt.Sprintf("Job %d", i)}
	}
	r.Sources = []source.Source{&fakeSource{jobs: jobs}}
	r.Matcher = matcherFunc{name: "unavailable", fn: func(_ context.Context, job model.Job) (match.Result, error) {
		return match.Result{}, fmt.Errorf("failure for %s with %s", job.ID, strings.Repeat("x", 1000))
	}}

	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "7 matcher evaluation(s) deferred (showing first 5)") {
		t.Fatalf("RunOnce = %v, want bounded seven-error summary", err)
	}
	if strings.Contains(err.Error(), "Job 5") || strings.Contains(err.Error(), "Job 6") {
		t.Fatalf("summary includes samples beyond limit: %v", err)
	}
	if len(err.Error()) > 3000 {
		t.Fatalf("bounded summary is %d bytes", len(err.Error()))
	}
	if st.Len() != 0 {
		t.Fatalf("deferred jobs were persisted: %d records", st.Len())
	}
}

// Seeding records jobs as baseline without evaluating or notifying:
// nothing is delivered now or on later runs.
func TestSeedSuppressesDeliveryForever(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, true, false)
	r.Sources = testSources()

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := st.Get(testJob.ID)
	if !ok || rec.Matched || rec.Notified {
		t.Fatalf("seeded job should be recorded unevaluated: %+v (ok=%v)", rec, ok)
	}

	r.SeedOnly = false
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 0 {
		t.Fatalf("seeded jobs must never be delivered, got %v", n.batches)
	}
}

func TestSeedPreservesPendingDelivery(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, true, false)
	r.Sources = testSources()
	st.Add(testJob.ID, store.Record{Matched: true})

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get(testJob.ID)
	if !rec.Matched || rec.Notified {
		t.Fatalf("seed must preserve pending delivery, got %+v", rec)
	}

	r.SeedOnly = false
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 {
		t.Fatalf("pending job should deliver after seed, got %v", n.batches)
	}
}

type lazySource struct {
	fakeSource
	detailCalls int
	detailErr   error
}

func (l *lazySource) Detail(_ context.Context, job *model.Job) error {
	l.detailCalls++
	if l.detailErr != nil {
		return l.detailErr
	}
	job.Description = "detailed description"
	return nil
}

// Lazy-detail sources fetch full postings only for jobs being evaluated,
// and a failed detail leaves the job unseen so it retries next run.
func TestLazyDetailOnlyForEvaluatedJobs(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	src := &lazySource{fakeSource: fakeSource{jobs: []model.Job{testJob}}}
	r.Sources = []source.Source{src}

	src.detailErr = errors.New("detail endpoint 502")
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, seen := st.Get(testJob.ID); seen {
		t.Fatal("job with failed detail must stay unseen for retry")
	}

	src.detailErr = nil
	for i := 0; i < 2; i++ { // second run evaluates; third must skip
		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if src.detailCalls != 2 { // 1 failed + 1 successful; none on the skip run
		t.Fatalf("detail calls = %d, want 2", src.detailCalls)
	}
	if len(n.batches) != 1 {
		t.Fatalf("expected one delivery, got %v", n.batches)
	}
}

// A rescan gives seeded backlog a fresh verdict and delivers it once.
func TestRescanSweepsSeededBacklog(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, true, false)
	r.Sources = testSources()

	if err := r.RunOnce(context.Background()); err != nil { // seed
		t.Fatal(err)
	}
	r.SeedOnly = false
	r.Rescan = true
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 {
		t.Fatalf("rescan should deliver the seeded match once, got %v", n.batches)
	}
	rec, _ := st.Get(testJob.ID)
	if !rec.Matched || !rec.Notified {
		t.Fatalf("rescanned match should be recorded delivered: %+v", rec)
	}

	r.Rescan = false
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 {
		t.Fatalf("normal run after rescan must not re-deliver, got %d batches", len(n.batches))
	}
}

func TestSeedNewSourcesBaselinesThenAlerts(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	src := &fakeSource{identity: "test/new-board", jobs: []model.Job{testJob}}
	r.Sources = []source.Source{src}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 0 {
		t.Fatalf("current jobs from a new source must be seeded, got %v", n.batches)
	}
	if _, ok := st.Get(testJob.ID); !ok {
		t.Fatal("seeded posting was not stored")
	}

	src.jobs = append(src.jobs, model.Job{
		ID: "test/acme/2", Company: "Acme", Title: "Another Junior Dev", URL: "https://x/2",
	})
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != "test/acme/2" {
		t.Fatalf("later posting from a seeded source should alert once, got %v", n.batches)
	}
}

func TestSeedNewSourcesDoesNotSuppressKnownBoard(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	st.Add("test/acme/closed", store.Record{Title: "historical posting"})
	newJob := model.Job{ID: "test/acme/2", Company: "Acme", Title: "New Junior Dev", URL: "https://x/2"}
	r.Sources = []source.Source{&fakeSource{
		identity:    "test/existing-board",
		statePrefix: "test/acme/",
		jobs:        []model.Job{newJob},
	}}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != newJob.ID {
		t.Fatalf("new posting on known board should not be seeded, got %v", n.batches)
	}
}

func TestPartialSourceResultsAreProcessed(t *testing.T) {
	n := &flakyNotifier{}
	r, _ := newRunner(t, n, false, false)
	r.Sources = []source.Source{&fakeSource{
		jobs: []model.Job{testJob},
		err:  errors.New("one detail endpoint returned 502"),
	}}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 {
		t.Fatalf("healthy jobs from partial source should deliver, got %v", n.batches)
	}
}

// Dry runs must not mutate state: the same job is re-evaluated every cycle.
func TestDryRunPersistsNothing(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, true)
	r.Sources = testSources()

	for i := 0; i < 2; i++ {
		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if st.Len() != 0 {
		t.Fatalf("dry run should not record anything, store has %d records", st.Len())
	}
	if len(n.batches) != 2 {
		t.Fatalf("dry run should re-report each cycle, got %d batches", len(n.batches))
	}
}

func testSources() []source.Source {
	return []source.Source{&fakeSource{jobs: []model.Job{testJob}}}
}

// cancellingMatcher cancels the run's context from inside a match call,
// simulating a CI timeout or SIGTERM landing mid-evaluation.
type cancellingMatcher struct {
	cancel  context.CancelFunc
	matched bool
}

func (cancellingMatcher) Name() string { return "cancelling" }
func (m cancellingMatcher) Match(context.Context, model.Job) (match.Result, error) {
	m.cancel()
	return match.Result{Matched: m.matched, Reason: "test"}, nil
}

// A cancelled run must persist the evaluations it already made, so an
// interrupted sweep resumes where it stopped instead of repeating the
// identical (and possibly expensive) matcher work forever.
func TestCancelledRunCheckpointsProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job2 := model.Job{ID: "test/acme/2", Company: "Acme", Title: "Backend Dev", URL: "https://x/2"}
	r := &Runner{
		Sources:     []source.Source{&fakeSource{jobs: []model.Job{testJob, job2}}},
		Matcher:     cancellingMatcher{cancel: cancel},
		Notifiers:   []notify.Notifier{&flakyNotifier{}},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
	}

	if err := r.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want context.Canceled", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no state was checkpointed before unwinding: %v", err)
	}
	if !strings.Contains(string(data), testJob.ID) {
		t.Errorf("evaluated job %s missing from checkpointed state", testJob.ID)
	}
	if strings.Contains(string(data), job2.ID) {
		t.Errorf("job %s was never evaluated and should not be in state", job2.ID)
	}
}

func TestCancellationAfterLastVerdictPersistsWithoutDelivery(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.Sources = testSources()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Matcher = cancellingMatcher{cancel: cancel, matched: true}

	if err := r.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want context.Canceled", err)
	}
	rec, ok := st.Get(testJob.ID)
	if !ok || !rec.Matched || rec.Notified {
		t.Fatalf("completed verdict was not preserved as pending: %+v (ok=%v)", rec, ok)
	}
	if len(n.batches) != 0 {
		t.Fatalf("canceled run delivered %v", n.batches)
	}
}

// diskPeekMatcher checks, while matching peekID, whether lookForID is
// already durable in the state file on disk.
type diskPeekMatcher struct {
	path      string
	peekID    string
	lookForID string
	sawOnDisk *bool
}

func (diskPeekMatcher) Name() string { return "diskpeek" }
func (m diskPeekMatcher) Match(_ context.Context, j model.Job) (match.Result, error) {
	if j.ID == m.peekID {
		data, _ := os.ReadFile(m.path)
		*m.sawOnDisk = strings.Contains(string(data), m.lookForID)
	}
	return match.Result{Matched: false, Reason: "test"}, nil
}

// Each board's evaluations must reach disk once that board is done, not
// only at the end of the whole cycle — a kill mid-catalog then costs one
// board's progress at most.
func TestStateCheckpointedAfterEachBoard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	job1 := model.Job{ID: "test/one/1", Company: "One", Title: "Dev", URL: "https://x/1"}
	job2 := model.Job{ID: "test/two/1", Company: "Two", Title: "Dev", URL: "https://x/2"}
	sawOnDisk := false
	r := &Runner{
		Sources: []source.Source{
			&fakeSource{identity: "test/one", jobs: []model.Job{job1}},
			&fakeSource{identity: "test/two", jobs: []model.Job{job2}},
		},
		Matcher:     diskPeekMatcher{path: path, peekID: job2.ID, lookForID: job1.ID, sawOnDisk: &sawOnDisk},
		Notifiers:   []notify.Notifier{&flakyNotifier{}},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sawOnDisk {
		t.Error("first board's evaluations were not on disk while the second board was being matched")
	}
}

// The interval save must make a long board's evaluations durable while the
// board is still being walked, because a hard kill (SIGKILL after the CI
// grace window) never reaches the cancellation checkpoint — at most
// SaveEvery of matcher work may be lost.
func TestIntervalCheckpointDuringLongBoard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	j1 := model.Job{ID: "test/one/1", Company: "One", Title: "Dev", URL: "https://x/1"}
	j2 := model.Job{ID: "test/one/2", Company: "One", Title: "Dev II", URL: "https://x/2"}
	sawOnDisk := false
	r := &Runner{
		Sources:     []source.Source{&fakeSource{jobs: []model.Job{j1, j2}}},
		Matcher:     diskPeekMatcher{path: path, peekID: j2.ID, lookForID: j1.ID, sawOnDisk: &sawOnDisk},
		Notifiers:   []notify.Notifier{&flakyNotifier{}},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
		SaveEvery:   time.Nanosecond,
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sawOnDisk {
		t.Error("first job was not durable on disk while a later job of the same board was being matched")
	}
}

// A new board's baseline marker must never be checkpointed ahead of its
// seeded postings: if a kill lands between the two, the next run would
// treat the whole board as already baselined and email its entire backlog.
func TestSeedMarkerNotPersistedBeforeBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	// Board A is already known (marker + one recorded posting); its new job
	// a2 is what the matcher is evaluating — and cancelling on — when the
	// walk reaches brand-new board B.
	st.Add("__jobwatch_source__/test/a", store.Record{Title: "source: A"})
	st.Add("test/a/1", store.Record{Title: "A: old posting"})
	a2 := model.Job{ID: "test/a/2", Company: "A", Title: "New Dev", URL: "https://a/2"}
	b1 := model.Job{ID: "test/b/1", Company: "B", Title: "Dev", URL: "https://b/1"}
	b2 := model.Job{ID: "test/b/2", Company: "B", Title: "Dev II", URL: "https://b/2"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Runner{
		Sources: []source.Source{
			&fakeSource{identity: "test/a", jobs: []model.Job{{ID: "test/a/1", Company: "A", Title: "Old"}, a2}},
			&fakeSource{identity: "test/b", jobs: []model.Job{b1, b2}},
		},
		Matcher:        cancellingMatcher{cancel: cancel},
		Notifiers:      []notify.Notifier{&flakyNotifier{}},
		Store:          st,
		Log:            log.New(io.Discard, "", 0),
		Concurrency:    1,
		SeedNewSources: true,
	}
	if err := r.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want context.Canceled", err)
	}
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "__jobwatch_source__/test/b") {
		t.Fatal("board B's marker was checkpointed before its baseline")
	}

	// The next, uninterrupted run must seed board B silently.
	n := &flakyNotifier{}
	r.Matcher = matchAll{}
	r.Notifiers = []notify.Notifier{n}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, batch := range n.batches {
		for _, m := range batch {
			if m.Job.ID == b1.ID || m.Job.ID == b2.ID {
				t.Fatalf("board B posting %s was delivered instead of seeded", m.Job.ID)
			}
		}
	}
}

// A cancelled dry run must leave no state file behind: DryRun's contract is
// that nothing persists and the same jobs are re-evaluated next run.
func TestDryRunNeverCheckpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job2 := model.Job{ID: "test/acme/2", Company: "Acme", Title: "Backend Dev", URL: "https://x/2"}
	r := &Runner{
		Sources:     []source.Source{&fakeSource{jobs: []model.Job{testJob, job2}}},
		Matcher:     cancellingMatcher{cancel: cancel, matched: true},
		Notifiers:   []notify.Notifier{&flakyNotifier{}},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
		DryRun:      true,
		SaveEvery:   time.Nanosecond,
	}

	if err := r.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("dry run wrote a state file (stat err = %v)", err)
	}
}

// countingMatcher records which jobs were evaluated.
type countingMatcher struct{ ids *[]string }

func (countingMatcher) Name() string { return "counting" }
func (m countingMatcher) Match(_ context.Context, j model.Job) (match.Result, error) {
	*m.ids = append(*m.ids, j.ID)
	return match.Result{Reason: "test"}, nil
}

// A failing interim save must not abort the sweep: remaining boards still
// run, and the error surfaces through the final Save.
func TestCheckpointFailureDoesNotAbortSweep(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root unix user so directory permissions make Save fail")
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	var evaluated []string
	r := &Runner{
		Sources: []source.Source{
			&fakeSource{identity: "test/one", jobs: []model.Job{{ID: "test/one/1", Company: "One", Title: "Dev", URL: "https://x/1"}}},
			&fakeSource{identity: "test/two", jobs: []model.Job{{ID: "test/two/1", Company: "Two", Title: "Dev", URL: "https://x/2"}}},
		},
		Matcher:     countingMatcher{ids: &evaluated},
		Notifiers:   []notify.Notifier{&flakyNotifier{}},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
		SaveEvery:   time.Nanosecond,
	}

	err = r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "saving state") {
		t.Fatalf("RunOnce = %v, want the final saving state error", err)
	}
	if len(evaluated) != 2 {
		t.Errorf("evaluated %v, want both boards despite failing checkpoints", evaluated)
	}
}
