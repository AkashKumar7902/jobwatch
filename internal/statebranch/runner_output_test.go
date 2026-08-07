package statebranch

// The end-to-end contract between the two halves of the migration: whatever a
// real RunOnce writes, this package must be willing to publish.
//
// Both sides were reasoned about separately and each is tested separately, but
// they are separate CODE — the runner decides to move a key, and the gate
// independently decides whether that move is explained. If those two ever
// disagree the symptom is the worst one this project has: the run succeeds, the
// push fails, the state branch stops advancing, and the watcher quietly forgets
// which jobs it already emailed. Nothing else in the repository joins the two
// halves, so this test drives the actual runner and validates its actual output
// file with the actual gate.
//
// It lives here rather than in internal/run because validateState and
// checkRemovals are unexported, and exporting a gate so it can be tested from
// the code it is meant to police would defeat its purpose.

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"jobwatch/internal/match"
	"jobwatch/internal/model"
	"jobwatch/internal/notify"
	"jobwatch/internal/run"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

type fakeBoard struct {
	company     string
	identity    string
	statePrefix string
	jobs        []model.Job
}

func (f *fakeBoard) Company() string                            { return f.company }
func (f *fakeBoard) Identity() string                           { return f.identity }
func (f *fakeBoard) StatePrefix() string                        { return f.statePrefix }
func (f *fakeBoard) Fetch(context.Context) ([]model.Job, error) { return f.jobs, nil }

type rejectAll struct{}

func (rejectAll) Name() string { return "reject" }
func (rejectAll) Match(context.Context, model.Job) (match.Result, error) {
	return match.Result{Matched: false, Reason: "test"}, nil
}

type silentNotifier struct{}

func (silentNotifier) Name() string                                 { return "silent" }
func (silentNotifier) Notify(context.Context, []notify.Match) error { return nil }
func (silentNotifier) Report(context.Context, notify.Report) error  { return nil }

// A state file in exactly the shape the live branch is in today: Workday keys
// with the vendor's shard baked in, one bare-base junk key, and legacy markers
// that describe nothing.
const preMigrationState = `{
 "__jobwatch_source__/workday/wd5.myworkdayjobs.com/citi/citi": {
  "first_seen": "2026-07-01T10:00:00Z",
  "title": "source: Citi",
  "matched": false,
  "notified": false
 },
 "__jobwatch_source__/test/departed": {
  "first_seen": "2026-07-01T10:00:00Z",
  "title": "source: Departed",
  "matched": false,
  "notified": false,
  "marker": {
   "state_prefix": "test/departed/",
   "announced_at": "0001-01-01T00:00:00Z"
  }
 },
 "workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/citi/JR100": {
  "first_seen": "2026-07-02T10:00:00Z",
  "title": "Citi: Analyst",
  "matched": true,
  "notified": true
 },
 "workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/citi/JR101": {
  "first_seen": "2026-07-02T10:00:00Z",
  "title": "Citi: Engineer",
  "matched": false,
  "notified": false
 },
 "workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/citi": {
  "first_seen": "2026-07-02T10:00:00Z",
  "title": "Citi: ",
  "matched": false,
  "notified": false
 }
}`

// TestRunnerOutputPassesTheRemovalGate is the one that would have caught the
// blocker: a migration that runs inside RunOnce removes tens of thousands of
// IDs, and until the gate learned the four explanations, every push after
// deployment would have failed.
func TestRunnerOutputPassesTheRemovalGate(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(preMigrationState), 0o600); err != nil {
		t.Fatal(err)
	}
	baseRecords, err := validateState([]byte(preMigrationState))
	if err != nil {
		t.Fatalf("fixture is not valid state: %v", err)
	}

	st, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	runner := &run.Runner{
		// The board the user still watches, now identified without the shard.
		// A board that DEPARTED (its marker is in the fixture) is deliberately
		// absent, so the census sweeps or keeps its marker in the same run.
		Sources: []source.Source{&fakeBoard{
			company: "Citi", identity: "workday/citi/citi", statePrefix: "workday/citi/citi/",
			jobs: []model.Job{
				{ID: "workday/citi/citi/JR100", Company: "Citi", Title: "Analyst"},
				{ID: "workday/citi/citi/JR101", Company: "Citi", Title: "Engineer"},
			},
		}},
		Matcher:     rejectAll{},
		Notifiers:   []notify.Notifier{silentNotifier{}},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	candidate, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	candidateRecords, err := validateState(candidate)
	if err != nil {
		// The runner wrote a state file this package cannot even parse — every
		// push would fail from here on.
		t.Fatalf("runner wrote unpublishable state: %v", err)
	}
	if err := checkRemovals(baseRecords, candidateRecords); err != nil {
		t.Fatalf("runner output would be refused by the publish gate: %v", err)
	}

	// The migration actually happened (a gate that passes because nothing moved
	// would prove nothing).
	if _, ok := candidateRecords["workday/citi/citi/JR100"]; !ok {
		t.Fatal("history was not moved off the vendor transport key")
	}
	if _, ok := candidateRecords["workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/citi/JR100"]; ok {
		t.Fatal("the old transport key survived the migration")
	}
	if !candidateRecords["workday/citi/citi/JR100"].notified {
		t.Fatal("a job the user was already emailed came back as pending")
	}
	if _, ok := candidateRecords["workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/citi"]; ok {
		t.Fatal("the unrecreatable bare-base key was not dropped")
	}
	// The census swept a marker in the same run, so the gate's marker rule was
	// exercised too rather than merely being available.
	if _, ok := candidateRecords["__jobwatch_source__/test/departed"]; ok {
		t.Fatal("the census did not sweep the marker of a departed board that owned nothing")
	}
	// ...and it kept the legacy marker it cannot describe, which is what stops
	// the run that ships this from re-baselining every board it has ever seen.
	if _, ok := candidateRecords["__jobwatch_source__/workday/wd5.myworkdayjobs.com/citi/citi"]; !ok {
		t.Fatal("a legacy marker was swept even though it could not prove it owned nothing")
	}

	// And the second run is a no-op against its own output: the rules are
	// idempotent by construction, so a re-run must neither move nor remove.
	secondBase := candidateRecords
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	secondRecords, err := validateState(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRemovals(secondBase, secondRecords); err != nil {
		t.Fatalf("second run would be refused by the publish gate: %v", err)
	}
}

// The runner's own error path must not be the thing that keeps state
// publishable: a board that cannot be fetched still leaves a file the gate
// accepts, because the alternative is an outage that also freezes the branch.
func TestFailedRunStillWritesPublishableState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(preMigrationState), 0o600); err != nil {
		t.Fatal(err)
	}
	baseRecords, _ := validateState([]byte(preMigrationState))

	st, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	runner := &run.Runner{
		Sources:     []source.Source{&failingBoard{}},
		Matcher:     rejectAll{},
		Notifiers:   []notify.Notifier{silentNotifier{}},
		Store:       st,
		Log:         log.New(io.Discard, "", 0),
		Concurrency: 1,
	}
	if err := runner.RunOnce(context.Background()); err == nil {
		t.Fatal("a board that cannot be fetched should surface an error")
	}
	candidate, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	candidateRecords, err := validateState(candidate)
	if err != nil {
		t.Fatalf("failed run wrote unpublishable state: %v", err)
	}
	if err := checkRemovals(baseRecords, candidateRecords); err != nil {
		t.Fatalf("failed run's state would be refused by the publish gate: %v", err)
	}
}

type failingBoard struct{}

func (failingBoard) Company() string     { return "Citi" }
func (failingBoard) Identity() string    { return "workday/citi/citi" }
func (failingBoard) StatePrefix() string { return "workday/citi/citi/" }
func (failingBoard) Fetch(context.Context) ([]model.Job, error) {
	return nil, errors.New("dial tcp: no route to host")
}
