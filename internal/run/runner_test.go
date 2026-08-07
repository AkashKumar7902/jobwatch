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

	"jobwatch/internal/health"
	"jobwatch/internal/match"
	"jobwatch/internal/model"
	"jobwatch/internal/notify"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

type fakeSource struct {
	company     string
	identity    string
	statePrefix string
	jobs        []model.Job
	err         error
}

func (f *fakeSource) Company() string {
	if f.company == "" {
		return "acme"
	}
	return f.company
}
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
	for _, job := range jobs {
		if st.Seen(job.ID) {
			t.Fatalf("deferred job %s was persisted", job.ID)
		}
	}
	if !st.Seen(sourceMarkerPrefix + "test/acme") {
		t.Fatal("ordinary run did not preserve its watched-source marker")
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

func TestSeedOnlyIncompleteFetchReturnsErrorAfterSaving(t *testing.T) {
	for _, tc := range []struct {
		name    string
		badJobs []model.Job
	}{
		{
			name: "partial",
			badJobs: []model.Job{{
				ID: "test/incomplete/1", Company: "Incomplete", Title: "Possibly incomplete", URL: "https://x/incomplete/1",
			}},
		},
		{name: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			st, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}

			healthyJob := model.Job{ID: "test/healthy/1", Company: "Healthy", Title: "Dev", URL: "https://x/healthy/1"}
			bad := &fakeSource{
				company:     "Incomplete",
				identity:    "test/incomplete",
				statePrefix: "test/incomplete/",
				jobs:        tc.badJobs,
				err:         errors.New("fetch did not complete"),
			}
			var evaluated []string
			var logs strings.Builder
			n := &flakyNotifier{}
			r := &Runner{
				Sources: []source.Source{
					&fakeSource{
						company: "Healthy", identity: "test/healthy", statePrefix: "test/healthy/", jobs: []model.Job{healthyJob},
					},
					bad,
				},
				Matcher:     countingMatcher{ids: &evaluated},
				Notifiers:   []notify.Notifier{n},
				Store:       st,
				Log:         log.New(&logs, "", 0),
				Concurrency: 1,
				SeedOnly:    true,
			}

			err = r.RunOnce(context.Background())
			if err == nil || !strings.Contains(err.Error(), "1 source baseline(s) incomplete") {
				t.Fatalf("RunOnce = %v, want one incomplete baseline", err)
			}
			if len(evaluated) != 0 || len(n.batches) != 0 {
				t.Fatalf("incomplete seed evaluated %v or notified %v", evaluated, n.batches)
			}
			if !strings.Contains(logs.String(), "seed incomplete: saved 1 postings from complete sources") {
				t.Fatalf("seed log did not report the safe saved count:\n%s", logs.String())
			}

			st.Close()
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatalf("saved state could not be reopened: %v", err)
			}
			t.Cleanup(reopened.Close)
			if _, ok := reopened.Get(healthyJob.ID); !ok {
				t.Fatal("complete source was not saved before RunOnce returned its error")
			}
			if !reopened.Seen(sourceMarkerPrefix + "test/healthy") {
				t.Fatal("complete source marker was not saved")
			}
			for _, job := range tc.badJobs {
				if reopened.Seen(job.ID) {
					t.Fatalf("job %s from incomplete baseline was recorded", job.ID)
				}
			}
			if reopened.Seen(sourceMarkerPrefix + "test/incomplete") {
				t.Fatal("incomplete source received a completion marker")
			}
			if !reopened.Seen(sourceSeedInProgressPrefix + "test/incomplete") {
				t.Fatal("incomplete source did not retain its in-progress marker")
			}
			if reopened.Seen(sourceRegistryV2ID) {
				t.Fatal("incomplete global seed prematurely certified the exact source registry")
			}
		})
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

func TestOrdinaryRunThenSeedNewSourcesKeepsExistingSourceAlerting(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	oldJob := model.Job{ID: "test/existing/1", Company: "Existing", Title: "Old Dev", URL: "https://x/existing/1"}
	newJob := model.Job{ID: "test/existing/2", Company: "Existing", Title: "New Dev", URL: "https://x/existing/2"}
	newBoardJob := model.Job{ID: "test/new/1", Company: "New", Title: "Current Dev", URL: "https://x/new/1"}
	existing := &fakeSource{
		company: "Existing", identity: "test/existing", statePrefix: "test/existing/", jobs: []model.Job{oldJob},
	}
	r.Sources = []source.Source{existing}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !st.Seen(sourceMarkerPrefix + "test/existing") {
		t.Fatal("ordinary run did not durably mark the source as already watched")
	}
	if !st.Seen(sourceRegistryV2ID) {
		t.Fatal("completed ordinary run did not atomically certify the exact source registry")
	}

	r.SeedNewSources = true
	existing.jobs = []model.Job{oldJob, newJob}
	r.Sources = []source.Source{
		existing,
		&fakeSource{company: "New", identity: "test/new", statePrefix: "test/new/", jobs: []model.Job{newBoardJob}},
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(n.batches) != 2 || len(n.batches[1]) != 1 || n.batches[1][0].Job.ID != newJob.ID {
		t.Fatalf("existing source's new job should alert while the new board is seeded, got %v", n.batches)
	}
	if !st.Seen(newBoardJob.ID) || !st.Seen(sourceMarkerPrefix+"test/new") {
		t.Fatal("new board did not receive a silent baseline and exact completion marker")
	}
}

func TestInterruptedOrdinaryRunStillMarksSourceAsWatched(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	firstJob := model.Job{ID: "test/interrupted/1", Company: "Interrupted", Title: "First Dev", URL: "https://x/interrupted/1"}
	secondJob := model.Job{ID: "test/interrupted/2", Company: "Interrupted", Title: "Second Dev", URL: "https://x/interrupted/2"}
	laterSourceJob := model.Job{ID: "test/later/1", Company: "Later", Title: "Later Dev", URL: "https://x/later/1"}
	r.Sources = []source.Source{
		&fakeSource{company: "Interrupted", identity: "test/interrupted", statePrefix: "test/interrupted/", jobs: []model.Job{firstJob, secondJob}},
		&fakeSource{company: "Later", identity: "test/later", statePrefix: "test/later/", jobs: []model.Job{laterSourceJob}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.Matcher = cancellingMatcher{cancel: cancel, matched: true}

	if err := r.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want context.Canceled", err)
	}
	if !st.Seen(sourceMarkerPrefix + "test/interrupted") {
		t.Fatal("interrupted ordinary run lost its watched-source marker")
	}
	if !st.Seen(sourceMarkerPrefix + "test/later") {
		t.Fatal("interruption before a later source left its watched identity unclassified")
	}
	if !st.Seen(sourceRegistryV2ID) {
		t.Fatal("interrupted ordinary run lost its atomic source registry")
	}
	if st.Seen(sourceSeedInProgressPrefix + "test/interrupted") {
		t.Fatal("ordinary run was incorrectly marked as a baseline in progress")
	}

	r.SeedNewSources = true
	r.Matcher = matchAll{}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 3 {
		t.Fatalf("retry and unseen remainder should both alert after interruption, got %v", n.batches)
	}
	foundSecond := false
	foundLaterSource := false
	for _, notification := range n.batches[0] {
		if notification.Job.ID == secondJob.ID {
			foundSecond = true
		}
		if notification.Job.ID == laterSourceJob.ID {
			foundLaterSource = true
		}
	}
	if !foundSecond {
		t.Fatalf("unseen remainder %s was silently seeded: %v", secondJob.ID, n.batches)
	}
	if !foundLaterSource {
		t.Fatalf("later watched source %s was silently seeded: %v", laterSourceJob.ID, n.batches)
	}
}

func TestSeedNewSourcesRejectsAmbiguousMarkerlessLegacyState(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	oldJob := model.Job{ID: "test/legacy/1", Company: "Legacy", Title: "Old Dev", URL: "https://x/legacy/1"}
	newJob := model.Job{ID: "test/legacy/2", Company: "Legacy", Title: "New Dev", URL: "https://x/legacy/2"}
	st.Add(oldJob.ID, store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "Legacy: Old Dev"})
	legacy := &fakeSource{
		company: "Legacy", identity: "test/legacy", statePrefix: "test/legacy/", jobs: []model.Job{oldJob, newJob},
	}
	r.Sources = []source.Source{legacy}

	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "existing posting records share its prefix") {
		t.Fatalf("RunOnce = %v, want markerless-state migration error", err)
	}
	if len(n.batches) != 0 || st.Seen(newJob.ID) || st.Seen(sourceMarkerPrefix+"test/legacy") {
		t.Fatalf("ambiguous markerless state was mutated or notified: batches=%v new=%v marker=%v",
			n.batches, st.Seen(newJob.ID), st.Seen(sourceMarkerPrefix+"test/legacy"))
	}

	// The explicit safe migration is an ordinary run with the unchanged
	// source list: existing records stay deduplicated, unseen jobs alert, and
	// the exact identity marker becomes durable for later catalog additions.
	r.SeedNewSources = false
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != newJob.ID {
		t.Fatalf("ordinary migration should alert the unseen job, got %v", n.batches)
	}
	if !st.Seen(sourceMarkerPrefix + "test/legacy") {
		t.Fatal("ordinary migration did not append an exact identity marker")
	}
	if st.Seen(sourceSeedInProgressPrefix + "test/legacy") {
		t.Fatal("ordinary migration was incorrectly treated as a new baseline")
	}
	if !st.Seen(sourceRegistryV2ID) {
		t.Fatal("ordinary migration did not atomically certify the exact source registry")
	}
}

func TestSeedNewSourcesRejectsMixedMarkerlessLegacyState(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	st.Add(sourceMarkerPrefix+"test/a", store.Record{FirstSeen: time.Now(), Title: "source: A"})
	st.Add("test/b/old", store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "B: Old Dev"})
	newJob := model.Job{ID: "test/b/new", Company: "B", Title: "New Dev", URL: "https://x/b/new"}
	r.Sources = []source.Source{&fakeSource{
		company: "B", identity: "test/b", statePrefix: "test/b/",
		jobs: []model.Job{{ID: "test/b/old", Company: "B", Title: "Old Dev"}, newJob},
	}}

	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "existing posting records share its prefix") {
		t.Fatalf("RunOnce = %v, want mixed-state migration error", err)
	}
	if len(n.batches) != 0 || st.Seen(newJob.ID) || st.Seen(sourceMarkerPrefix+"test/b") || st.Seen(sourceRegistryV2ID) {
		t.Fatalf("mixed markerless state was mutated or notified: batches=%v new=%v marker=%v registry=%v",
			n.batches, st.Seen(newJob.ID), st.Seen(sourceMarkerPrefix+"test/b"), st.Seen(sourceRegistryV2ID))
	}
}

func TestSubsetSeedDoesNotCertifyOmittedLegacySources(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, true, false)
	legacyOld := model.Job{ID: "test/shared/legacy-old", Company: "Legacy", Title: "Old Dev", URL: "https://x/legacy-old"}
	legacyNew := model.Job{ID: "test/shared/legacy-new", Company: "Legacy", Title: "New Dev", URL: "https://x/legacy-new"}
	newScopeJob := model.Job{ID: "test/shared/new-scope", Company: "Scoped", Title: "Current Dev", URL: "https://x/new-scope"}
	st.Add(legacyOld.ID, store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "Legacy: Old Dev"})
	r.Sources = []source.Source{&fakeSource{
		company: "Scoped", identity: "test/scoped", statePrefix: "test/shared/", jobs: []model.Job{newScopeJob},
	}}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !st.Seen(sourceMarkerPrefix+"test/scoped") || !st.Seen(newScopeJob.ID) {
		t.Fatal("explicit subset seed did not baseline its source")
	}
	if st.Seen(sourceRegistryV2ID) {
		t.Fatal("subset seed against nonempty state certified omitted legacy sources")
	}

	r.SeedOnly = false
	r.SeedNewSources = true
	r.Sources = []source.Source{
		&fakeSource{company: "Legacy", identity: "test/legacy", statePrefix: "test/shared/", jobs: []model.Job{legacyOld, legacyNew}},
		&fakeSource{company: "Scoped", identity: "test/scoped", statePrefix: "test/shared/", jobs: []model.Job{newScopeJob}},
	}
	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "existing posting records share its prefix") {
		t.Fatalf("RunOnce = %v, want omitted-legacy-source migration error", err)
	}
	if len(n.batches) != 0 || st.Seen(legacyNew.ID) || st.Seen(sourceMarkerPrefix+"test/legacy") || st.Seen(sourceRegistryV2ID) {
		t.Fatalf("omitted legacy source was mutated or certified: batches=%v new=%v marker=%v registry=%v",
			n.batches, st.Seen(legacyNew.ID), st.Seen(sourceMarkerPrefix+"test/legacy"), st.Seen(sourceRegistryV2ID))
	}
}

func TestSeedNewSourcePartialBaselineDefersThenRecovers(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	job1 := model.Job{ID: "test/recover/1", Company: "Recover", Title: "Dev", URL: "https://x/recover/1"}
	job2 := model.Job{ID: "test/recover/2", Company: "Recover", Title: "Dev II", URL: "https://x/recover/2"}
	src := &fakeSource{
		company: "Recover", identity: "test/recover", statePrefix: "test/recover/",
		jobs: []model.Job{job1}, err: errors.New("page two returned 503"),
	}
	r.Sources = []source.Source{src}
	var evaluated []string
	r.Matcher = matcherFunc{name: "counting match", fn: func(_ context.Context, job model.Job) (match.Result, error) {
		evaluated = append(evaluated, job.ID)
		return match.Result{Matched: true, Reason: "test"}, nil
	}}

	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 source baseline(s) incomplete") {
		t.Fatalf("partial baseline RunOnce = %v, want incomplete-baseline error", err)
	}
	if st.Seen(job1.ID) {
		t.Fatal("job returned by partial baseline was recorded")
	}
	if st.Seen(sourceMarkerPrefix + "test/recover") {
		t.Fatal("partial baseline received a completion marker")
	}
	if !st.Seen(sourceSeedInProgressPrefix + "test/recover") {
		t.Fatal("partial baseline did not persist its in-progress marker")
	}
	if len(evaluated) != 0 || len(n.batches) != 0 {
		t.Fatalf("partial baseline evaluated %v or notified %v", evaluated, n.batches)
	}

	src.err = nil
	src.jobs = []model.Job{job1, job2}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("full recovery run: %v", err)
	}
	for _, job := range []model.Job{job1, job2} {
		if !st.Seen(job.ID) {
			t.Fatalf("recovery did not seed %s", job.ID)
		}
	}
	if !st.Seen(sourceMarkerPrefix + "test/recover") {
		t.Fatal("full recovery did not append the completion marker")
	}
	if !st.Seen(sourceSeedInProgressPrefix + "test/recover") {
		t.Fatal("full recovery deleted the append-only in-progress marker")
	}
	if len(evaluated) != 0 || len(n.batches) != 0 {
		t.Fatalf("full recovery evaluated %v or notified %v", evaluated, n.batches)
	}

	job3 := model.Job{ID: "test/recover/3", Company: "Recover", Title: "New Dev", URL: "https://x/recover/3"}
	src.jobs = append(src.jobs, job3)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("post-completion run: %v", err)
	}
	if len(evaluated) != 1 || evaluated[0] != job3.ID {
		t.Fatalf("completed source evaluated %v, want only %s", evaluated, job3.ID)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != job3.ID {
		t.Fatalf("completed source delivered %v, want only %s", n.batches, job3.ID)
	}
}

func TestOrdinaryRunResumesPersistedSeedInProgressMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	oldJob := model.Job{ID: "test/resume/1", Company: "Resume", Title: "Old Dev", URL: "https://x/resume/1"}
	newJob := model.Job{ID: "test/resume/2", Company: "Resume", Title: "New Dev", URL: "https://x/resume/2"}
	progressID := sourceSeedInProgressPrefix + "test/resume"
	st.Add(progressID, store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "source seed in progress: Resume"})
	st.Add(oldJob.ID, store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "Resume: Old Dev"})
	if err := st.Save(); err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()

	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var evaluated []string
	n := &flakyNotifier{}
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Resume", identity: "test/resume", statePrefix: "test/resume/", jobs: []model.Job{oldJob, newJob},
		}},
		Matcher: matcherFunc{name: "must not run", fn: func(_ context.Context, job model.Job) (match.Result, error) {
			evaluated = append(evaluated, job.ID)
			return match.Result{Matched: true}, nil
		}},
		Notifiers:   []notify.Notifier{n},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(evaluated) != 0 || len(n.batches) != 0 {
		t.Fatalf("resumed baseline evaluated %v or notified %v", evaluated, n.batches)
	}
	if !st.Seen(newJob.ID) {
		t.Fatal("resumed baseline did not seed its unseen remainder")
	}
	if !st.Seen(sourceMarkerPrefix + "test/resume") {
		t.Fatal("resumed baseline did not append its completion marker")
	}
	if !st.Seen(progressID) {
		t.Fatal("completion deleted the append-only in-progress marker")
	}

	laterJob := model.Job{ID: "test/resume/3", Company: "Resume", Title: "Later Dev", URL: "https://x/resume/3"}
	r.Sources = []source.Source{&fakeSource{
		company: "Resume", identity: "test/resume", statePrefix: "test/resume/", jobs: []model.Job{oldJob, newJob, laterJob},
	}}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(evaluated) != 1 || evaluated[0] != laterJob.ID {
		t.Fatalf("post-completion ordinary run evaluated %v, want only %s", evaluated, laterJob.ID)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != laterJob.ID {
		t.Fatalf("post-completion ordinary run delivered %v, want only %s", n.batches, laterJob.ID)
	}
}

func TestSeedNewSourcesRejectsAmbiguousSharedPostingPrefix(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	st.Add(sourceMarkerPrefix+"test/existing-scope", store.Record{FirstSeen: time.Now(), Title: "source: existing scope"})
	st.Add("test/shared/closed", store.Record{FirstSeen: time.Now(), Title: "historical posting"})
	newJob := model.Job{ID: "test/shared/new-scope/2", Company: "Acme", Title: "Scoped Junior Dev", URL: "https://x/2"}
	r.Sources = []source.Source{&fakeSource{
		identity:    "test/new-scope",
		statePrefix: "test/shared/",
		jobs:        []model.Job{newJob},
	}}

	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "existing posting records share its prefix") {
		t.Fatalf("RunOnce = %v, want shared-prefix ambiguity error", err)
	}
	if len(n.batches) != 0 || st.Seen(newJob.ID) || st.Seen(sourceMarkerPrefix+"test/new-scope") {
		t.Fatalf("ambiguous scoped source was mutated or notified: batches=%v new=%v marker=%v",
			n.batches, st.Seen(newJob.ID), st.Seen(sourceMarkerPrefix+"test/new-scope"))
	}

	// Explicitly seeding this source alone resolves the ambiguity without
	// either alerting its backlog or conflating it with the existing scope.
	r.SeedNewSources = false
	r.SeedOnly = true
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 0 || !st.Seen(newJob.ID) || !st.Seen(sourceMarkerPrefix+"test/new-scope") || st.Seen(sourceRegistryV2ID) {
		t.Fatalf("explicit scoped baseline failed or certified the subset: batches=%v new=%v marker=%v registry=%v",
			n.batches, st.Seen(newJob.ID), st.Seen(sourceMarkerPrefix+"test/new-scope"), st.Seen(sourceRegistryV2ID))
	}

	r.SeedOnly = false
	r.SeedNewSources = true
	r.Sources = []source.Source{
		&fakeSource{company: "Existing", identity: "test/existing-scope", statePrefix: "test/shared/", jobs: []model.Job{{ID: "test/shared/closed", Company: "Existing", Title: "Closed"}}},
		&fakeSource{identity: "test/new-scope", statePrefix: "test/shared/", jobs: []model.Job{newJob}},
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !st.Seen(sourceRegistryV2ID) {
		t.Fatal("restored full exact-marker config did not certify the source registry")
	}
}

// A board whose IDENTITY changed is not a new board, and seeding it is
// destructive rather than merely silent.
//
// This is the shape of the run that ships the transport-coordinate rekey: 83 of
// the 260 catalog boards get a brand-new identity, marker keys embed the OLD one
// and are deliberately never rewritten, and migrateStateKeys has already moved
// the postings under the new prefix. Production polls with -seed-new-sources, so
// without adoption every one of those boards would take the baseline path and
// write the postings that appeared since the previous cycle as history —
// unevaluated, unmailed, and permanently skipped afterwards because a seeded
// record reads as processed forever.
func TestIdentityChangeAdoptsInsteadOfReseeding(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	clk := &testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	r.now = clk.now

	// State exactly as production has it: the registry is long since certified,
	// the marker still names the pre-rekey identity, and the history has already
	// been moved under the current prefix.
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "exact source registry v2"})
	st.Add(sourceMarkerPrefix+"workday/redhat.wd5.myworkdayjobs.com/redhat/jobs",
		store.Record{FirstSeen: time.Now().Add(-30 * 24 * time.Hour), Title: "source: Red Hat"})
	st.Add("workday/redhat/jobs/job/A", store.Record{
		FirstSeen: time.Now().Add(-24 * time.Hour), Title: "Red Hat: Analyst", Matched: true, Notified: true,
	})

	appeared := model.Job{ID: "workday/redhat/jobs/job/NEW", Company: "Red Hat", Title: "Junior Dev", URL: "https://x/new"}
	src := &fakeSource{
		company: "Red Hat", identity: "workday/redhat/jobs", statePrefix: "workday/redhat/jobs/",
		jobs: []model.Job{{ID: "workday/redhat/jobs/job/A", Company: "Red Hat", Title: "Analyst"}, appeared},
	}
	r.Sources = []source.Source{src}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != appeared.ID {
		t.Fatalf("the posting that appeared since the last cycle was swallowed by a re-seed: %v", n.batches)
	}
	if rec, _ := st.Get(appeared.ID); !rec.Matched || !rec.Notified {
		t.Fatalf("delivered posting was recorded as unevaluated history: %+v", rec)
	}
	if !st.Seen(sourceMarkerPrefix + "workday/redhat/jobs") {
		t.Fatal("the adopted board did not record a marker under its new identity")
	}
	if st.Seen(sourceSeedInProgressPrefix + "workday/redhat/jobs") {
		t.Fatal("an inherited board was treated as a baseline in progress")
	}
	// Baselined means "we watched this board from birth" and is the ONLY guard
	// keeping the one-run stillborn rule off established boards. Claiming it for
	// a board that inherited thousands of records turns that rule loose on 47%
	// of the fleet the first time one of them has no openings.
	if h := boardHealth(t, st, "workday/redhat/jobs"); h.Baselined || h.Kind != "" {
		t.Fatalf("an inherited board was marked as baselined by us: %+v", h)
	}
}

// The mirror-image case, and the reason adoption cannot simply trust
// HasPostingPrefix: two scoped views of ONE board share a state prefix (two
// eightfold `query=` entries, two avature search paths). A genuinely new view
// must still be baselined in silence, or adding one mails its whole backlog —
// the exact blast -seed-new-sources exists to prevent.
func TestNewScopedViewOfAnAdoptedBoardIsStillSeeded(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "exact source registry v2"})
	st.Add(sourceMarkerPrefix+"test/scope-a", store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "source: Scope A"})
	st.Add("test/shared/1", store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "Acme: Analyst", Matched: true, Notified: true})

	backlog := model.Job{ID: "test/shared/2", Company: "Acme", Title: "Scoped Junior Dev", URL: "https://x/2"}
	r.Sources = []source.Source{
		&fakeSource{company: "Acme", identity: "test/scope-a", statePrefix: "test/shared/",
			jobs: []model.Job{{ID: "test/shared/1", Company: "Acme", Title: "Analyst"}}},
		&fakeSource{company: "Acme", identity: "test/scope-b", statePrefix: "test/shared/", jobs: []model.Job{backlog}},
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 0 {
		t.Fatalf("a newly added scoped view mailed the backlog it shares a prefix with: %v", n.batches)
	}
	if rec, ok := st.Get(backlog.ID); !ok || rec.Matched {
		t.Fatalf("the new scope's backlog was not baselined: %+v (stored=%v)", rec, ok)
	}
	if !st.Seen(sourceSeedInProgressPrefix + "test/scope-b") {
		t.Fatal("a genuine baseline did not record its resumable in-progress marker")
	}
}

// An explicit -seed-only is a request for a BASELINE and must never be quietly
// downgraded to adoption, however much history the board owns. The in-progress
// marker is the whole point: without it an interrupted baseline resumes as an
// ordinary run and mails the half of the board it never reached.
func TestSeedOnlyIsNeverDowngradedToAdoption(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, true, false)
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "exact source registry v2"})
	st.Add("test/legacy/1", store.Record{FirstSeen: time.Now().Add(-time.Hour), Title: "Legacy: Old Dev"})

	newJob := model.Job{ID: "test/legacy/2", Company: "Legacy", Title: "New Dev", URL: "https://x/legacy/2"}
	r.Sources = []source.Source{&fakeSource{
		company: "Legacy", identity: "test/legacy", statePrefix: "test/legacy/",
		jobs: []model.Job{{ID: "test/legacy/1", Company: "Legacy", Title: "Old Dev"}, newJob},
	}}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 0 {
		t.Fatalf("an explicit seed notified: %v", n.batches)
	}
	if !st.Seen(sourceSeedInProgressPrefix + "test/legacy") {
		t.Fatal("an explicit seed skipped the in-progress marker that makes interruption resumable")
	}
	if rec, _ := st.Get(newJob.ID); rec.Matched {
		t.Fatalf("an explicit seed evaluated a posting instead of recording it: %+v", rec)
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

func TestKnownPartialSourceStillProcessesHealthyJobs(t *testing.T) {
	n := &flakyNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	st.Add(sourceMarkerPrefix+"test/known", store.Record{FirstSeen: time.Now(), Title: "source: Known"})
	st.Add("test/known/closed", store.Record{Title: "historical posting"})
	newJob := model.Job{ID: "test/known/open", Company: "Known", Title: "Dev", URL: "https://x/known/open"}
	r.Sources = []source.Source{&fakeSource{
		company:     "Known",
		identity:    "test/known",
		statePrefix: "test/known/",
		jobs:        []model.Job{newJob},
		err:         errors.New("one detail endpoint returned 502"),
	}}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.batches) != 1 || len(n.batches[0]) != 1 || n.batches[0][0].Job.ID != newJob.ID {
		t.Fatalf("healthy jobs from known partial source should deliver, got %v", n.batches)
	}
	if !st.Seen(sourceMarkerPrefix + "test/known") {
		t.Fatal("exactly marked source lost its completion marker")
	}
	if st.Seen(sourceSeedInProgressPrefix + "test/known") {
		t.Fatal("known source was incorrectly treated as a baseline in progress")
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

// ---------------------------------------------------------------------------
// Board health
//
// The failure these tests pin down is the one that produces no symptom at all:
// an ATS answers a renamed slug with HTTP 200 and an empty list, which reads
// exactly like a company with no openings, and RunOnce stays green. Everything
// below is about making that observable WITHOUT inventing alarms — so several
// of these assert silence just as hard as they assert delivery.
// ---------------------------------------------------------------------------

// reportingNotifier is a channel that opted into notify.Reporter. Embedding
// flakyNotifier keeps the match path identical to the other tests, so a
// regression in reporting can never be mistaken for one in delivery.
type reportingNotifier struct {
	flakyNotifier
	reportFailures int
	reports        []notify.Report
}

func (r *reportingNotifier) Name() string { return "reporting" }

func (r *reportingNotifier) Report(_ context.Context, rep notify.Report) error {
	if r.reportFailures > 0 {
		r.reportFailures--
		return errors.New("smtp down")
	}
	r.reports = append(r.reports, rep)
	return nil
}

// testClock drives ONLY the health rules, so a 72-hour threshold and a 30-day
// cadence can both be crossed inside one test without sleeping.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func subjects(reports []notify.Report) []string {
	out := make([]string, 0, len(reports))
	for _, r := range reports {
		out = append(out, r.Subject)
	}
	return out
}

func boardHealth(t *testing.T, st *store.Store, identity string) store.Health {
	t.Helper()
	rec, ok := st.Get(health.KeyPrefix + identity)
	if !ok || rec.Health == nil {
		t.Fatalf("no health record for %s", identity)
	}
	return *rec.Health
}

// A board whose fetch failed must be OBSERVED but must never be marked
// baselined. This is the one interaction where getting health wrong causes
// real damage rather than a missed alert: a source marker written for a board
// that was never walked tells the next run "already baselined", and the run
// after that emails the company's entire back catalogue.
func TestFailedFetchNeverCreatesMarker(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	clk := &testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	r.now = clk.now
	src := &fakeSource{
		company: "Acme", identity: "test/acme", statePrefix: "test/acme/",
		err: errors.New("dial tcp: no route to host"),
	}
	r.Sources = []source.Source{src}

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("a board that could not be fetched at all should surface an error")
	}
	if st.Seen(sourceMarkerPrefix + "test/acme") {
		t.Fatal("a failed fetch wrote a completion marker; the next run would seed nothing and then email the whole board")
	}
	h := boardHealth(t, st, "test/acme")
	if h.ErrRuns != 1 || h.LastErr == "" {
		t.Fatalf("hard-failing board was not observed above the failure continue: %+v", h)
	}
	if h.Baselined || h.Fetches != 0 {
		t.Fatalf("a failed fetch counted as an adoption or a success: %+v", h)
	}
	if len(n.reports) != 1 || !strings.Contains(n.reports[0].Subject, "monthly board report") {
		t.Fatalf("first run should send exactly the install-confirmation digest, got %v", subjects(n.reports))
	}

	// Recovery must still baseline the board SILENTLY — the backlog it now
	// returns is history, not news.
	clk.advance(time.Hour)
	src.err = nil
	src.jobs = []model.Job{testJob}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if len(n.batches) != 0 {
		t.Fatalf("recovered board delivered its backlog instead of baselining it: %v", n.batches)
	}
	if !st.Seen(sourceMarkerPrefix+"test/acme") || !st.Seen(testJob.ID) {
		t.Fatal("recovery did not baseline the board")
	}
	h = boardHealth(t, st, "test/acme")
	if !h.Baselined || h.ErrRuns != 0 || h.LastNonEmptyN != 1 {
		t.Fatalf("recovery was not folded into health: %+v", h)
	}
}

// The stillborn check is the ONLY mechanism that catches the incident this
// feature exists for: a greenhouse token renamed before the board was ever
// watched answers 200 with a real board name and zero postings, so probing the
// endpoint proves nothing. Caught at adoption it costs one run; left to the
// cliff test it would take 21-30 days, and the cliff test can never fire on a
// board that was never seen non-empty.
func TestNewBoardWithZeroPostingsReports(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	clk := &testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	r.now = clk.now
	r.Sources = []source.Source{&fakeSource{
		company: "HubSpot", identity: "greenhouse/us/hubspot", statePrefix: "greenhouse/hubspot/",
	}}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var trip *notify.Report
	for i := range n.reports {
		if strings.Contains(n.reports[i].Subject, "returned nothing when added") {
			trip = &n.reports[i]
		}
	}
	if trip == nil {
		t.Fatalf("a board baselined with zero postings was not reported: %v", subjects(n.reports))
	}
	if !strings.Contains(trip.Subject, "HubSpot") {
		t.Errorf("report subject does not name the board: %q", trip.Subject)
	}
	if body := strings.Join(trip.Lines, "\n"); !strings.Contains(body, "greenhouse/us/hubspot") {
		t.Errorf("report body does not name the identity the user has to edit:\n%s", body)
	}
	if h := boardHealth(t, st, "greenhouse/us/hubspot"); h.SentAt.IsZero() || h.Kind != health.Stillborn {
		t.Fatalf("delivered report was not stamped on the health record: %+v", h)
	}

	// It fires ONCE. A standing condition that re-mails every half hour is a
	// condition the user filters away, taking the next real one with it.
	before := len(n.reports)
	for i := 0; i < 3; i++ {
		clk.advance(time.Hour)
		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.reports) != before {
		t.Fatalf("stillborn board reported again: %v", subjects(n.reports[before:]))
	}
}

// Dry runs exist to tune the matcher against real boards. Writing health from
// one would let a laptop experiment stamp SentAt and swallow the report the
// scheduled run was about to send.
func TestDryRunWritesNoHealth(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, true)
	r.now = (&testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}).now
	r.Sources = []source.Source{
		&fakeSource{company: "Acme", identity: "test/acme", jobs: []model.Job{testJob}},
		&fakeSource{company: "Broken", identity: "test/broken", err: errors.New("404")},
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.Len() != 0 {
		var ids []string
		st.Range(func(id string, _ store.Record) bool { ids = append(ids, id); return true })
		t.Fatalf("dry run persisted %v", ids)
	}
	if len(n.reports) != 0 {
		t.Fatalf("dry run delivered reports: %v", subjects(n.reports))
	}
}

// The digest is the heartbeat, and a heartbeat is only meaningful if its
// cadence is exact: too early and it becomes noise, too late (or skipped after
// a failed send) and a month of silence stops proving anything.
func TestDigestCadence(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := &testClock{t: start}
	r.now = clk.now
	r.Sources = testSources()

	// The first one doubles as install confirmation: it is the only way to
	// learn on day one that reports can be delivered at all.
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.reports) != 1 {
		t.Fatalf("first run sent %v, want one install-confirmation digest", subjects(n.reports))
	}

	clk.advance(29 * 24 * time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.reports) != 1 {
		t.Fatalf("digest sent after 29 days: %v", subjects(n.reports))
	}

	// At 31 days it is due — but a failed send must NOT advance the cadence,
	// or one flaky SMTP night silently costs a whole month of heartbeat.
	clk.advance(2 * 24 * time.Hour)
	n.reportFailures = 1
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("a failed report delivery should surface as a run error")
	}
	if len(n.reports) != 1 {
		t.Fatalf("failed digest was recorded as delivered: %v", subjects(n.reports))
	}
	rec, ok := st.Get(health.RunKey)
	if !ok || rec.Run == nil {
		t.Fatal("no run record")
	}
	if !rec.Run.DigestSentAt.Equal(start) {
		t.Fatalf("failed digest advanced the cadence to %s, want it left at %s", rec.Run.DigestSentAt, start)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.reports) != 2 {
		t.Fatalf("digest was not retried after the failure: %v", subjects(n.reports))
	}
	rec, _ = st.Get(health.RunKey)
	if !rec.Run.DigestSentAt.Equal(clk.t) {
		t.Fatalf("delivered digest was not stamped: %+v", rec.Run)
	}
}

// Report delivery follows the same at-least-once discipline as matches: the
// SentAt stamp is written only after the channel accepts the report, so a
// failure retries instead of vanishing.
func TestReportRetriedUntilDelivered(t *testing.T) {
	n := &reportingNotifier{reportFailures: 1}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	clk := &testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	r.now = clk.now
	r.Sources = []source.Source{&fakeSource{
		company: "Newco", identity: "test/newco", statePrefix: "test/newco/",
	}}

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("an undelivered report should surface as a run error")
	}
	if len(n.reports) != 0 {
		t.Fatalf("failed delivery recorded %v", subjects(n.reports))
	}
	if h := boardHealth(t, st, "test/newco"); !h.SentAt.IsZero() {
		t.Fatalf("report stamped delivered despite the channel failing: %+v", h)
	}

	clk.advance(time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.reports) != 2 {
		t.Fatalf("retry delivered %v, want the pending board report and the digest", subjects(n.reports))
	}
	if h := boardHealth(t, st, "test/newco"); h.SentAt.IsZero() {
		t.Fatalf("delivered report was not stamped: %+v", h)
	}

	clk.advance(time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.reports) != 2 {
		t.Fatalf("report re-sent after being delivered: %v", subjects(n.reports))
	}
}

// Reporter is discovered by type assertion, exactly like source.Detailer, so
// that webhook and telegram keep compiling and keep delivering matches without
// growing a method they have no rendering for.
func TestNonReporterNotifierIsSkipped(t *testing.T) {
	plain := &flakyNotifier{}
	if _, ok := notify.Notifier(plain).(notify.Reporter); ok {
		t.Fatal("fixture unexpectedly implements Reporter; this test proves nothing")
	}

	t.Run("mixed", func(t *testing.T) {
		rep := &reportingNotifier{}
		r, _ := newRunner(t, plain, false, false)
		r.Notifiers = []notify.Notifier{plain, rep}
		r.now = (&testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}).now
		r.Sources = testSources()

		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(rep.reports) == 0 {
			t.Fatal("the reporting channel received nothing")
		}
		if len(plain.batches) != 1 {
			t.Fatalf("the non-reporting channel stopped receiving matches: %v", plain.batches)
		}
	})

	t.Run("none", func(t *testing.T) {
		var logs strings.Builder
		only := &flakyNotifier{}
		r, _ := newRunner(t, only, false, false)
		r.Log = log.New(&logs, "", 0)
		r.now = (&testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}).now
		r.Sources = testSources()

		// An unroutable report must not fail the poll — the matches still
		// need to go out — but it must never be silent either.
		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "no configured notifier implements notify.Reporter") {
			t.Fatalf("undeliverable report was dropped silently:\n%s", logs.String())
		}
	})
}

// 193 of 260 boards sit behind five vendors, so one vendor change trips a
// whole cohort in the same cycle. Forty-seven emails about one incident is
// indistinguishable from spam, and a monitoring channel that gets filtered
// takes the next real alert with it.
func TestMassEventCollapsesToOneReport(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	r.now = (&testClock{t: base}).now

	const boards = 8
	var sources []source.Source
	identities := make([]string, 0, boards)
	for i := 0; i < boards; i++ {
		identity := fmt.Sprintf("greenhouse/us/co%d", i)
		identities = append(identities, identity)
		company := fmt.Sprintf("Co %d", i)
		sources = append(sources, &fakeSource{company: company, identity: identity, statePrefix: identity + "/"})
		st.Add(sourceMarkerPrefix+identity, store.Record{FirstSeen: base, Title: "source: " + company})
		// One observation short of the zero-run floor, with the elapsed-time
		// floor already met: this cycle's empty answer is what trips it.
		st.Add(health.KeyPrefix+identity, store.Record{
			FirstSeen: base, Title: "board health: " + company,
			Health: &store.Health{
				Company: company, SrcType: "greenhouse",
				FirstFetch: base.Add(-60 * 24 * time.Hour), LastOK: base.Add(-time.Hour),
				LastNonEmpty: base.Add(-100 * time.Hour), LastNonEmptyN: 40,
				NonEmptyDays: 5, Fetches: 200,
				Recent: []int{40, 0, 0}, Nonzero: []int{40}, Typical: 40,
				ZeroRuns: health.MinZeroRuns - 1, ZeroSince: base.Add(-100 * time.Hour),
			},
		})
	}
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: base, Title: "exact source registry v2"})
	// Heartbeat recently sent, so the only thing this cycle can produce is the
	// incident report.
	st.Add(health.RunKey, store.Record{
		FirstSeen: base, Title: "jobwatch run",
		Run: &store.Run{LastRunAt: base.Add(-time.Hour), DigestSentAt: base.Add(-time.Hour)},
	})
	r.Sources = sources

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.reports) != 1 {
		t.Fatalf("%d simultaneous trips produced %d reports, want 1: %v", boards, len(n.reports), subjects(n.reports))
	}
	rep := n.reports[0]
	if !strings.Contains(rep.Subject, fmt.Sprintf("%d boards went quiet", boards)) {
		t.Errorf("subject does not say how many boards tripped: %q", rep.Subject)
	}
	body := strings.Join(rep.Lines, "\n")
	if !strings.Contains(body, fmt.Sprintf("greenhouse %d", boards)) {
		t.Errorf("report does not group the trips by the adapter they share:\n%s", body)
	}
	for _, identity := range identities {
		h := boardHealth(t, st, identity)
		if h.Kind != health.Dead || h.SentAt.IsZero() {
			t.Fatalf("%s was not recorded as reported: %+v", identity, h)
		}
	}

	// And the collapsed report is not re-sent on the next cycle.
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.reports) != 1 {
		t.Fatalf("collapsed report re-sent: %v", subjects(n.reports))
	}
}

// Two consecutive cycles in which EVERY board failed means the watcher itself
// is not working — until now visible only as a red CI check nobody opens. One
// cycle is not enough: a runner can simply lose the network for a minute.
func TestAllBoardsFailingReportsOncePerOutage(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	clk := &testClock{t: base}
	r.now = clk.now
	src := &fakeSource{company: "Acme", identity: "test/acme", err: errors.New("dial tcp: i/o timeout")}
	r.Sources = []source.Source{src}
	st.Add(sourceMarkerPrefix+"test/acme", store.Record{FirstSeen: base, Title: "source: Acme"})
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: base, Title: "exact source registry v2"})
	st.Add(health.RunKey, store.Record{
		FirstSeen: base, Title: "jobwatch run",
		Run: &store.Run{LastRunAt: base.Add(-time.Hour), DigestSentAt: base.Add(-time.Hour)},
	})

	outage := func() []string {
		clk.advance(30 * time.Minute)
		if err := r.RunOnce(context.Background()); err == nil {
			t.Fatal("a run where every board failed should report an error")
		}
		return subjects(n.reports)
	}

	if got := outage(); len(got) != 0 {
		t.Fatalf("one failed cycle already mailed: %v", got)
	}
	if got := outage(); len(got) != 1 || !strings.Contains(got[0], "not reaching any job board") {
		t.Fatalf("second consecutive total failure did not report: %v", got)
	}
	for i := 0; i < 3; i++ {
		if got := outage(); len(got) != 1 {
			t.Fatalf("outage re-mailed on every cycle: %v", got)
		}
	}

	// Recovery re-arms it: the next outage is a new incident.
	clk.advance(30 * time.Minute)
	src.err = nil
	src.jobs = []model.Job{testJob}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get(health.RunKey)
	if rec.Run.AllFailRuns != 0 || !rec.Run.AllFailSentAt.IsZero() {
		t.Fatalf("recovery did not re-arm the outage report: %+v", rec.Run)
	}
	src.err = errors.New("dial tcp: i/o timeout")
	src.jobs = nil
	outage()
	if got := outage(); len(got) != 2 {
		t.Fatalf("the second outage was not reported: %v", got)
	}
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
			&fakeSource{identity: "test/a", statePrefix: "test/a/", jobs: []model.Job{{ID: "test/a/1", Company: "A", Title: "Old"}, a2}},
			&fakeSource{identity: "test/b", statePrefix: "test/b/", jobs: []model.Job{b1, b2}},
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
	for _, identity := range []string{"test/one", "test/two"} {
		st.Add(sourceMarkerPrefix+identity, store.Record{FirstSeen: time.Now(), Title: "source: known"})
	}
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: time.Now(), Title: "exact source registry v2"})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
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
