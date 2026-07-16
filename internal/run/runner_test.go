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

type fakeSource struct{ jobs []model.Job }

func (f *fakeSource) Company() string                                { return "acme" }
func (f *fakeSource) Fetch(_ context.Context) ([]model.Job, error) { return f.jobs, nil }

type matchAll struct{}

func (matchAll) Name() string                  { return "all" }
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
