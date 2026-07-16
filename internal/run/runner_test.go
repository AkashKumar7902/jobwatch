package run

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"testing"

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

func (matchAll) Name() string                 { return "all" }
func (matchAll) Match(model.Job) match.Result { return match.Result{Matched: true, Reason: "test"} }

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
