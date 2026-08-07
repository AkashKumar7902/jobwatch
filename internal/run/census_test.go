package run

// Tests for the board census and the two repairs it points at.
//
// Almost everything here asserts SILENCE. The census's value is entirely in
// what it does NOT say: it fires on one conjunction — a board with surviving
// history disappeared AND a board of the same ATS type appeared with none — and
// that conjunction is worth having only because neither thing the user actually
// does (adding a company, removing a company) can produce it alone. A census
// that also fired on those would be a census the user filters away, taking the
// real migration with it.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/notify"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

var censusEpoch = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// seedMarker pre-seeds an adopted board. An empty prefix writes a LEGACY
// marker — one from before markers recorded what they owned — which is what
// every marker in the live state file looks like on the run that ships this.
func seedMarker(st *store.Store, identity, prefix string) {
	rec := store.Record{FirstSeen: censusEpoch.Add(-30 * 24 * time.Hour), Title: "source: " + identity}
	if prefix != "" {
		rec.Marker = &store.Marker{StatePrefix: prefix}
	}
	st.Add(sourceMarkerPrefix+identity, rec)
}

func seedPosting(st *store.Store, id string, notified bool) {
	st.Add(id, store.Record{
		FirstSeen: censusEpoch.Add(-30 * 24 * time.Hour),
		Title:     "Acme: Junior Dev",
		Matched:   notified,
		Notified:  notified,
	})
}

func board(company, identity, prefix string) *fakeSource {
	return &fakeSource{company: company, identity: identity, statePrefix: prefix}
}

// moveReports filters out the health digest, which is due on every one of these
// first runs and is not what any of them is about.
func moveReports(reports []notify.Report) []notify.Report {
	var out []notify.Report
	for _, r := range reports {
		if strings.Contains(r.Subject, "history orphaned") {
			out = append(out, r)
		}
	}
	return out
}

// The whole design in one table: exactly one row announces.
func TestCensusAnnouncesOnlyOnTheMigrationSignature(t *testing.T) {
	type marker struct{ identity, prefix string }
	tests := []struct {
		name     string
		markers  []marker
		postings []string
		sources  []source.Source
		announce bool
		swept    []string // markers the run must have forgotten
		kept     []string // markers the run must have left in place
	}{
		{
			// THE signature: an adopted board with real history vanished from
			// the config, and a board of the same ATS type turned up owning
			// nothing. Only an EDIT produces both halves in one run.
			name:     "host move: orphan with history plus a fresh board of the same type",
			markers:  []marker{{"workday/old/site", "workday/old/site/"}},
			postings: []string{"workday/old/site/1", "workday/old/site/2"},
			sources:  []source.Source{board("Warner Bros", "workday/new/site", "workday/new/site/")},
			announce: true,
			kept:     []string{"workday/old/site"},
		},
		{
			// Adding a company is the single most common config edit there is.
			// It creates a fresh board and no orphan, so there is no conjunction
			// and nothing to say.
			name:     "pure addition",
			markers:  []marker{{"workday/acme/site", "workday/acme/site/"}},
			postings: []string{"workday/acme/site/1"},
			sources: []source.Source{
				board("Acme", "workday/acme/site", "workday/acme/site/"),
				board("Newco", "workday/newco/site", "workday/newco/site/"),
			},
			announce: false,
		},
		{
			// Removing a company creates an orphan and no fresh board. The
			// history is deliberately KEPT — the user may add the board back —
			// but they already know they removed it, so it is not news.
			name: "pure deletion",
			markers: []marker{
				{"workday/gone/site", "workday/gone/site/"},
				{"workday/kept/site", "workday/kept/site/"},
			},
			postings: []string{"workday/gone/site/1", "workday/kept/site/1"},
			sources:  []source.Source{board("Kept", "workday/kept/site", "workday/kept/site/")},
			announce: false,
			kept:     []string{"workday/gone/site"},
		},
		{
			// A removal and an addition in the same run, but of different
			// vendors. An ATS does not move a tenant from Workday to
			// Greenhouse; that is two unrelated edits.
			name:     "removal and addition of different ATS types",
			markers:  []marker{{"workday/old/site", "workday/old/site/"}},
			postings: []string{"workday/old/site/1"},
			sources:  []source.Source{board("Newco", "greenhouse/us/newco", "greenhouse/newco/")},
			announce: false,
			kept:     []string{"workday/old/site"},
		},
		{
			// Nothing was lost, so there is nothing to report — and the marker
			// is swept, because after the key repair HasPostingPrefix alone
			// answers "do we know this board?" correctly.
			name:     "orphan that never owned any history is forgotten silently",
			markers:  []marker{{"workday/old/site", "workday/old/site/"}},
			postings: nil,
			sources:  []source.Source{board("Newco", "workday/new/site", "workday/new/site/")},
			announce: false,
			swept:    []string{"workday/old/site"},
		},
		{
			// Two scoped views of ONE board (two eightfold query= entries) share
			// a state prefix. Deleting one orphans a marker whose records a
			// configured board still owns and still matches on. Announcing would
			// mail a previous_state_prefix line naming a live board's namespace
			// — which main.go refuses to start with, so the "fix" would take the
			// watcher down.
			name: "deleted scoped view whose prefix a live board still owns",
			markers: []marker{
				{"eightfold/acme//query=a", "eightfold/acme/"},
				{"eightfold/acme//query=b", "eightfold/acme/"},
			},
			postings: []string{"eightfold/acme/1"},
			sources: []source.Source{
				board("Acme", "eightfold/acme//query=b", "eightfold/acme/"),
				board("Newco", "eightfold/newco", "eightfold/newco/"),
			},
			announce: false,
			kept:     []string{"eightfold/acme//query=a"},
		},
		{
			// On the run that ships this change EVERY stored marker is legacy:
			// no recorded prefix, and an identity string that cannot be parsed
			// back into one. Such a marker can prove neither that it owned
			// history nor that it did not, so it is left strictly alone.
			name:     "legacy markers without a recorded prefix are never announced or swept",
			markers:  []marker{{"workday/old/site", ""}},
			postings: []string{"workday/old/site/1"},
			sources:  []source.Source{board("Newco", "workday/new/site", "workday/new/site/")},
			announce: false,
			kept:     []string{"workday/old/site"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &reportingNotifier{}
			r, st := newRunner(t, n, false, false)
			r.now = (&testClock{t: censusEpoch}).now
			for _, m := range tc.markers {
				seedMarker(st, m.identity, m.prefix)
			}
			for _, id := range tc.postings {
				seedPosting(st, id, false)
			}
			r.Sources = tc.sources

			if err := r.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}

			got := moveReports(n.reports)
			switch {
			case tc.announce && len(got) != 1:
				t.Fatalf("migration signature was not announced; reports: %v", subjects(n.reports))
			case !tc.announce && len(got) != 0:
				t.Fatalf("announced %v, want silence", subjects(got))
			}
			for _, identity := range tc.swept {
				if st.Seen(sourceMarkerPrefix + identity) {
					t.Errorf("marker %q survived; a zero-record orphan should be forgotten", identity)
				}
			}
			for _, identity := range tc.kept {
				if !st.Seen(sourceMarkerPrefix + identity) {
					t.Errorf("marker %q was swept while it still owned history or could not prove it did not", identity)
				}
			}
		})
	}
}

// The conjunction is "both halves in the SAME run", and the table above can
// only ever check one run. The halves are not symmetric: a fresh board is fresh
// for exactly one cycle, but an orphan stays an orphan forever, so without a
// latch on the orphan side the rule silently decays into "a board was EVER
// removed AND one appeared now". 193 of 260 boards sit behind five vendors, so
// a deletion in March and an unrelated addition in September collide on ATS type
// as a matter of course — and the mail would offer a previous_state_prefix line
// that grafts the deleted board's history onto a company that never had it.
func TestAdjudicatedOrphanCannotPairWithALaterAddition(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	clk := &testClock{t: censusEpoch}
	r.now = clk.now
	seedMarker(st, "greenhouse/us/keeper", "greenhouse/keeper/")
	seedPosting(st, "greenhouse/keeper/1", true)
	seedMarker(st, "greenhouse/us/gone", "greenhouse/gone/")
	seedPosting(st, "greenhouse/gone/1", true)

	keeper := board("Keeper", "greenhouse/us/keeper", "greenhouse/keeper/")
	r.Sources = []source.Source{keeper}

	// The deletion, standing alone, across several cycles.
	for i := 0; i < 3; i++ {
		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Hour)
	}
	if got := moveReports(n.reports); len(got) != 0 {
		t.Fatalf("a pure deletion announced: %v", subjects(got))
	}
	rec, _ := st.Get(sourceMarkerPrefix + "greenhouse/us/gone")
	if rec.Marker == nil || rec.Marker.OrphanedAt.IsZero() {
		t.Fatalf("the census did not record that it had already adjudicated the orphan: %+v", rec.Marker)
	}

	// Runs later, a completely unrelated company of the same ATS type is added.
	r.Sources = []source.Source{keeper, board("Newco", "greenhouse/us/newco", "greenhouse/newco/")}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := moveReports(n.reports); len(got) != 0 {
		t.Fatalf("a stale orphan was paired with an unrelated later addition: %v", subjects(got))
	}
}

// The latch describes one episode of absence, not the board. A board removed,
// restored and removed again is a fresh question, and a marker that stayed
// latched would make it permanently unannounceable — turning a bound on false
// alarms into a hole in the real detection.
func TestReAddedBoardIsJudgedAfreshWhenItLeavesAgain(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	clk := &testClock{t: censusEpoch}
	r.now = clk.now
	seedMarker(st, "workday/old/site", "workday/old/site/")
	seedPosting(st, "workday/old/site/1", true)
	keeper := board("Kept", "workday/kept/site", "workday/kept/site/")
	seedMarker(st, "workday/kept/site", "workday/kept/site/")
	seedPosting(st, "workday/kept/site/1", true)
	r.Sources = []source.Source{keeper}

	// Gone: adjudicated, nothing to pair with.
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)

	// Back in the config, so the adjudication no longer describes anything.
	r.Sources = []source.Source{keeper, board("Old", "workday/old/site", "workday/old/site/")}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get(sourceMarkerPrefix + "workday/old/site")
	if rec.Marker == nil || !rec.Marker.OrphanedAt.IsZero() {
		t.Fatalf("a reconfigured board kept a stale adjudication stamp: %+v", rec.Marker)
	}
	clk.advance(time.Hour)

	// And now the real thing: the host moves, so the board is replaced.
	r.Sources = []source.Source{keeper, board("Old", "workday/new/site", "workday/new/site/")}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := moveReports(n.reports); len(got) != 1 {
		t.Fatalf("a genuine move after a re-add was not announced: %v", subjects(n.reports))
	}
}

// A board that fails on every single fetch never completes a baseline, so it
// never gets a marker, and having recorded nothing it owns no postings either.
// Left unqualified that makes it a candidate successor FOREVER, and removing any
// board of its ATS type would announce a move on its own — a pure deletion
// firing without the edit that is supposed to be its other half. Its
// seed-in-progress record, written before the fetch is even attempted, is the
// durable proof that the board is not new to us.
func TestBoardThatNeverAnswersIsNotAnEligibleSuccessor(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	clk := &testClock{t: censusEpoch}
	r.now = clk.now
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: censusEpoch.Add(-time.Hour), Title: "exact source registry v2"})
	seedMarker(st, "greenhouse/us/keeper", "greenhouse/keeper/")
	seedPosting(st, "greenhouse/keeper/1", true)

	keeper := board("Keeper", "greenhouse/us/keeper", "greenhouse/keeper/")
	broken := board("Broken", "greenhouse/us/broken", "greenhouse/broken/")
	broken.err = errors.New("dial tcp: no route to host")

	// The broken board is added and never once answers.
	r.Sources = []source.Source{keeper, broken}
	for i := 0; i < 3; i++ {
		if err := r.RunOnce(context.Background()); err == nil {
			t.Fatal("a board that never answered did not surface its baseline failure")
		}
		clk.advance(time.Hour)
	}
	if !st.Seen(sourceSeedInProgressPrefix + "greenhouse/us/broken") {
		t.Fatal("a board whose first fetch failed did not record a resumable baseline")
	}

	// Much later, a pure deletion — all by itself.
	r.Sources = []source.Source{broken}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("a board that never answered did not surface its baseline failure")
	}
	if got := moveReports(n.reports); len(got) != 0 {
		t.Fatalf("a pure deletion was paired with a board that has never answered: %v", subjects(got))
	}
}

// The announcement's whole payload is a line the user pastes, so the exact
// bytes of that line — and the counts that justify pasting it — are part of the
// contract, not decoration.
func TestMoveAnnouncementNamesBothPrefixesAndPastableLine(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	r.now = (&testClock{t: censusEpoch}).now
	seedMarker(st, "workday/old/site", "workday/old/site/")
	seedPosting(st, "workday/old/site/1", true)
	seedPosting(st, "workday/old/site/2", false)
	r.Sources = []source.Source{board("Warner Bros", "workday/new/site", "workday/new/site/")}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := moveReports(n.reports)
	if len(got) != 1 {
		t.Fatalf("reports: %v", subjects(n.reports))
	}
	body := strings.Join(got[0].Lines, "\n")
	for _, want := range []string{
		`workday/old/site`, // the old identity
		`2 stored posting(s) under "workday/old/site/`, // what is at stake
		`workday/new/site/`,                            // the new namespace
		`previous_state_prefix: "workday/old/site/"`,   // the pastable line
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report body is missing %q:\n%s", want, body)
		}
	}
	// It must not be filed under the match headline: this is the one message
	// whose point is that the user has to edit a file.
	if strings.Contains(got[0].Subject, "matching job") {
		t.Errorf("move announcement reuses the match subject: %q", got[0].Subject)
	}

	// The census is pure state arithmetic, so it must be decided before the
	// first fetch — a network outage cannot be allowed to suppress it.
	if got[0].Subject != n.reports[0].Subject {
		t.Errorf("move announcement was queued behind %q; it must go first", n.reports[0].Subject)
	}
}

// At-least-once, and the reason it needs its own latch: by the time a retry
// runs, the successor board has a marker of its own and is no longer fresh, so
// the signature itself is not recomputable. marker.moved_to is what carries the
// verdict across the failure.
func TestMoveAnnouncementRetriedUntilDelivered(t *testing.T) {
	n := &reportingNotifier{reportFailures: 1}
	r, st := newRunner(t, n, false, false)
	clk := &testClock{t: censusEpoch}
	r.now = clk.now
	seedMarker(st, "workday/old/site", "workday/old/site/")
	seedPosting(st, "workday/old/site/1", false)
	r.Sources = []source.Source{board("Warner Bros", "workday/new/site", "workday/new/site/")}

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("an undelivered announcement should surface as a run error")
	}
	if got := moveReports(n.reports); len(got) != 0 {
		t.Fatalf("failed delivery recorded %v", subjects(got))
	}
	rec, ok := st.Get(sourceMarkerPrefix + "workday/old/site")
	if !ok || rec.Marker == nil {
		t.Fatal("orphan marker disappeared")
	}
	if !rec.Marker.AnnouncedAt.IsZero() {
		t.Fatalf("stamped announced despite the channel failing: %+v", rec.Marker)
	}
	if rec.Marker.MovedTo != "workday/new/site" {
		t.Fatalf("raised announcement was not recorded for retry: %+v", rec.Marker)
	}

	// The successor is adopted by now, so this retry is served entirely by
	// moved_to.
	clk.advance(time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := moveReports(n.reports); len(got) != 1 {
		t.Fatalf("announcement was not retried: %v", subjects(n.reports))
	}
	rec, _ = st.Get(sourceMarkerPrefix + "workday/old/site")
	if rec.Marker.AnnouncedAt.IsZero() {
		t.Fatal("delivered announcement was not stamped")
	}

	// An orphan stays an orphan forever, so a condition-driven report would
	// mail every half hour until the user gave up on the whole channel.
	for i := 0; i < 3; i++ {
		clk.advance(time.Hour)
		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := moveReports(n.reports); len(got) != 1 {
		t.Fatalf("announcement repeated: %v", subjects(got))
	}
}

// The escape hatch: the line the announcement told the user to paste actually
// reunites the board with its history, and pasting it once is the same as
// leaving it in the file forever.
func TestPreviousStatePrefixMovesHistoryAndIsIdempotent(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	r.now = (&testClock{t: censusEpoch}).now
	seedPosting(st, "icims/old-tenant/1", true)  // already emailed
	seedPosting(st, "icims/old-tenant/2", false) // seen, never matched
	r.Sources = []source.Source{board("MaxLinear", "icims/new-tenant", "icims/new-tenant/")}
	r.PreviousStatePrefixes = map[string]string{"icims/new-tenant": "icims/old-tenant/"}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"icims/old-tenant/1", "icims/old-tenant/2"} {
		if st.Seen(id) {
			t.Errorf("%q was left behind under the old prefix", id)
		}
	}
	moved, ok := st.Get("icims/new-tenant/1")
	if !ok {
		t.Fatal("history was not moved onto the live board's keys")
	}
	// The one invariant that must never bend: a posting the user was already
	// emailed about must not come back as pending.
	if !moved.Notified || !moved.Matched {
		t.Fatalf("delivery flag lost in the move: %+v", moved)
	}
	if !st.Seen("icims/new-tenant/2") {
		t.Error("un-notified history was dropped rather than moved")
	}
	// The marker records the absorbed prefix: that record, not the config, is
	// what authorizes the resulting removals at publish time.
	rec, ok := st.Get(sourceMarkerPrefix + "icims/new-tenant")
	if !ok || rec.Marker == nil || rec.Marker.PreviousStatePrefix != "icims/old-tenant/" {
		t.Fatalf("marker does not record the declared move: %+v", rec.Marker)
	}

	// Idempotent by construction: nothing is left under the old prefix, so the
	// second run is a no-op and the line can stay in config forever.
	before := snapshot(st)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := snapshot(st)
	for id, rec := range before {
		if got, ok := after[id]; !ok || got != rec {
			t.Fatalf("second run changed %q: %+v -> %+v", id, rec, after[id])
		}
	}
	if len(after) != len(before) {
		t.Fatalf("second run changed the record count: %d -> %d", len(before), len(after))
	}
}

// A declared move needs somewhere to land. Until the board has been adopted
// there is no marker to record the move in, and the state-branch gate reads
// that marker — not config — to authorize the removals it produces. So the
// swap defers rather than running unrecorded.
func TestPreviousStatePrefixDefersUntilTheBoardIsAdopted(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true // production's flag: markerless boards baseline first
	clk := &testClock{t: censusEpoch}
	r.now = clk.now
	seedPosting(st, "icims/old-tenant/1", true)
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: censusEpoch, Title: "exact source registry v2"})
	src := board("MaxLinear", "icims/new-tenant", "icims/new-tenant/")
	src.jobs = []model.Job{{ID: "icims/new-tenant/9", Company: "MaxLinear", Title: "Dev"}}
	r.Sources = []source.Source{src}
	r.PreviousStatePrefixes = map[string]string{"icims/new-tenant": "icims/old-tenant/"}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !st.Seen("icims/old-tenant/1") {
		t.Fatal("history was moved before the board was adopted; the removal would have nothing to authorize it")
	}

	// The baseline wrote the marker, so the next run performs the swap.
	clk.advance(time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.Seen("icims/old-tenant/1") {
		t.Fatal("adopted board did not absorb the declared prefix on the following run")
	}
	rec, ok := st.Get("icims/new-tenant/1")
	if !ok || !rec.Notified {
		t.Fatalf("absorbed record lost its delivery flag: %+v", rec)
	}
}

func snapshot(st *store.Store) map[string]store.Record {
	out := map[string]store.Record{}
	st.Range(func(id string, rec store.Record) bool {
		// Health rides along on every run by design and is not what these
		// tests are about.
		if !strings.HasPrefix(id, "__jobwatch_health__/") {
			out[id] = rec
		}
		return true
	})
	return out
}

// The census can only tell an orphaned board from a board that never had
// history if the marker recorded its prefix while the board was still
// configured. Production runs -seed-new-sources, so a marker refreshed only on
// the ordinary path would leave the census blind on exactly the boards that
// have been watched longest.
func TestMarkersRecordStatePrefixOnNonSeedRuns(t *testing.T) {
	for _, tc := range []struct {
		name           string
		seedNewSources bool
	}{
		{"ordinary run", false},
		{"seed-new-sources run", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &reportingNotifier{}
			r, st := newRunner(t, n, false, false)
			r.SeedNewSources = tc.seedNewSources
			r.now = (&testClock{t: censusEpoch}).now
			// A legacy marker: adopted long ago, describing nothing.
			seedMarker(st, "workday/acme/site", "")
			adopted := censusEpoch.Add(-30 * 24 * time.Hour)
			r.Sources = []source.Source{board("Acme", "workday/acme/site", "workday/acme/site/")}

			if err := r.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			rec, ok := st.Get(sourceMarkerPrefix + "workday/acme/site")
			if !ok {
				t.Fatal("marker disappeared")
			}
			if rec.Marker == nil || rec.Marker.StatePrefix != "workday/acme/site/" {
				t.Fatalf("marker did not record what the board owns: %+v", rec.Marker)
			}
			// The marker's age is a real fact about when we adopted the board
			// and the only thing that survives a re-add; rewriting it every run
			// would destroy it.
			if !rec.FirstSeen.Equal(adopted) {
				t.Errorf("refresh reset the adoption date to %s, want %s", rec.FirstSeen, adopted)
			}
		})
	}
}

// Refreshing a marker is not adopting a board. A marker written for a board
// that is about to be baselined tells the NEXT run "already watched", and an
// interrupted first pass then turns into a full-board notification blast.
func TestMarkerRefreshDoesNotAdoptAnUnbaselinedBoard(t *testing.T) {
	n := &reportingNotifier{}
	r, st := newRunner(t, n, false, false)
	r.SeedNewSources = true
	r.now = (&testClock{t: censusEpoch}).now
	st.Add(sourceRegistryV2ID, store.Record{FirstSeen: censusEpoch, Title: "exact source registry v2"})
	src := board("Newco", "workday/newco/site", "workday/newco/site/")
	src.err = errNoRoute
	r.Sources = []source.Source{src}

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("a board that could not be fetched should surface an error")
	}
	if st.Seen(sourceMarkerPrefix + "workday/newco/site") {
		t.Fatal("the marker refresh pass claimed a board that was never walked; its backlog would be mailed as news")
	}
}

var errNoRoute = errors.New("dial tcp: no route to host")
