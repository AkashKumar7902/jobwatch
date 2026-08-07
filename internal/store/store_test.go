package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, text string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return parsed
}

// realWorldRecords mirrors what production state actually contains: the
// reserved runner markers, a delivered match, a pending match, and postings
// with nanosecond and non-UTC timestamps.
func realWorldRecords(t *testing.T) map[string]Record {
	t.Helper()
	return map[string]Record{
		"__jobwatch_source__/amazon/IND": {
			FirstSeen: mustTime(t, "2026-08-01T12:48:28.623520366Z"),
			Title:     "source: Amazon India",
		},
		"__jobwatch_source__/ashby/openai": {
			FirstSeen: mustTime(t, "2026-07-16T19:27:53.859972364Z"),
			Title:     "source: OpenAI",
		},
		"__jobwatch_source_seed_in_progress__/ashby/openai": {
			FirstSeen: mustTime(t, "2026-08-05T04:05:06.123456789Z"),
			Title:     "source seed in progress: OpenAI",
		},
		"__jobwatch_source_registry_v2__": {
			FirstSeen: mustTime(t, "2026-07-16T19:27:53.859972364Z"),
			Title:     "exact source registry v2",
		},
		"workday/nvidia/9001": {
			FirstSeen: mustTime(t, "2026-08-05T01:02:03Z"),
			Title:     "NVIDIA: Systems Software Engineer",
			Matched:   true,
			Notified:  true,
		},
		"greenhouse/hubspot/4455": {
			FirstSeen: mustTime(t, "2026-08-05T01:03:04.123456789+05:30"),
			Title:     "HubSpot: Software Engineer I",
			Matched:   true,
		},
		"smartrecruiters/visa/9002": {
			FirstSeen: mustTime(t, "2026-08-05T01:03:04Z"),
			Title:     "Visa: Staff Engineer",
		},
	}
}

// TestRecordRoundTripsUnchanged is the migration guarantee for adding Health
// and Run to Record. Every posting the watcher has ever seen — tens of
// thousands of records — is rewritten and pushed on every single run, so the
// new fields must be invisible on records that do not use them. If this test
// ever fails, the change it is guarding would rewrite the entire state file
// and inflate every push from then on.
func TestRecordRoundTripsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for id, rec := range realWorldRecords(t) {
		first.Add(id, rec)
	}
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	first.Close()

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A reload followed by a save is exactly what one CI cycle does to a
	// record it does not touch.
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("state changed across a load/save cycle:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	for _, key := range []string{`"health"`, `"run"`} {
		if strings.Contains(string(after), key) {
			t.Fatalf("plain records serialized %s:\n%s", key, after)
		}
	}

	// The on-the-wire shape of one record, spelled out: the state branch
	// validator rejects unknown fields, so this is the contract.
	var raw map[string]map[string]any
	if err := json.Unmarshal(after, &raw); err != nil {
		t.Fatal(err)
	}
	rec, ok := raw["workday/nvidia/9001"]
	if !ok {
		t.Fatalf("record missing from %s", after)
	}
	if len(rec) != 4 {
		t.Fatalf("record has %d fields, want exactly 4: %v", len(rec), rec)
	}
	for _, field := range []string{"first_seen", "title", "matched", "notified"} {
		if _, ok := rec[field]; !ok {
			t.Fatalf("record missing field %q: %v", field, rec)
		}
	}

	loaded, ok := second.Get("workday/nvidia/9001")
	if !ok {
		t.Fatal("record not reloaded")
	}
	if loaded.Health != nil || loaded.Run != nil {
		t.Fatalf("reloaded record grew health/run: %+v", loaded)
	}
}

// TestHealthRecordRoundTrips covers the other direction: a record that DOES
// carry health must survive a save/load cycle intact, since the alarm state
// machine reads back what an earlier process wrote.
func TestHealthRecordRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	const id = "__jobwatch_health__/greenhouse/us/hubspot"
	want := Record{
		FirstSeen: mustTime(t, "2026-07-16T19:27:53.859972364Z"),
		Title:     "health: HubSpot",
		Health: &Health{
			Company:       "HubSpot",
			SrcType:       "greenhouse",
			FirstFetch:    mustTime(t, "2026-07-16T19:27:53.859972364Z"),
			LastOK:        mustTime(t, "2026-08-05T01:02:03Z"),
			LastNonEmpty:  mustTime(t, "2026-08-01T01:02:03Z"),
			LastNonEmptyN: 173,
			NonEmptyDays:  6,
			Fetches:       244,
			Recent:        []int{173, 0, 0, 0},
			Nonzero:       []int{160, 173},
			Typical:       166,
			ZeroRuns:      13,
			ZeroSince:     mustTime(t, "2026-08-02T01:02:03Z"),
			Kind:          "dead", // internal/health.Dead, spelled out: store must not import it
			RaisedAt:      mustTime(t, "2026-08-05T01:02:03Z"),
		},
		Run: &Run{
			LastRunAt:    mustTime(t, "2026-08-05T01:02:03Z"),
			AllFailRuns:  1,
			DigestSentAt: mustTime(t, "2026-07-16T19:27:53Z"),
		},
	}
	st.Add(id, want)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st.Close()

	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	got, ok := reloaded.Get(id)
	if !ok {
		t.Fatal("health record not reloaded")
	}
	if got.Health == nil || got.Run == nil {
		t.Fatalf("health/run lost: %+v", got)
	}
	if !got.Health.LastNonEmpty.Equal(want.Health.LastNonEmpty) ||
		got.Health.LastNonEmptyN != want.Health.LastNonEmptyN ||
		got.Health.Kind != want.Health.Kind ||
		got.Health.ZeroRuns != want.Health.ZeroRuns ||
		len(got.Health.Recent) != len(want.Health.Recent) ||
		got.Run.AllFailRuns != want.Run.AllFailRuns {
		t.Fatalf("health round trip lost data:\ngot  %+v / %+v\nwant %+v / %+v",
			got.Health, got.Run, want.Health, want.Run)
	}
}

func TestRangeVisitsEveryRecordAndStopsEarly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	records := realWorldRecords(t)
	for id, rec := range records {
		st.Add(id, rec)
	}

	seen := map[string]string{}
	st.Range(func(id string, r Record) bool {
		seen[id] = r.Title
		return true
	})
	if len(seen) != len(records) {
		t.Fatalf("Range visited %d records, want %d", len(seen), len(records))
	}
	for id, rec := range records {
		if seen[id] != rec.Title {
			t.Fatalf("Range reported %q for %q, want %q", seen[id], id, rec.Title)
		}
	}

	visits := 0
	st.Range(func(string, Record) bool {
		visits++
		return false
	})
	if visits != 1 {
		t.Fatalf("Range continued after f returned false: %d visits", visits)
	}
}

// newStore opens an empty store in a temp dir.
func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestRekeyMovesAndCounts(t *testing.T) {
	st := newStore(t)
	for id, rec := range realWorldRecords(t) {
		st.Add(id, rec)
	}
	before := st.Len()

	moved, merged := st.Rekey(func(id string) string {
		if strings.HasPrefix(id, "greenhouse/") {
			return "gh/" + strings.TrimPrefix(id, "greenhouse/")
		}
		return ""
	})
	if moved != 1 || merged != 0 {
		t.Fatalf("moved=%d merged=%d, want 1 and 0", moved, merged)
	}
	if st.Len() != before {
		t.Fatalf("Rekey changed the record count: %d -> %d", before, st.Len())
	}
	if st.Seen("greenhouse/hubspot/4455") {
		t.Error("the old key survived the move")
	}
	rec, ok := st.Get("gh/hubspot/4455")
	if !ok || rec.Title != "HubSpot: Software Engineer I" || !rec.Matched {
		t.Fatalf("moved record = %+v (ok=%v)", rec, ok)
	}

	// The empty string means "leave it alone", not "delete it". A rewrite
	// function that fails to match returns "" for every key, and that must be
	// a no-op rather than an emptied state file.
	moved, merged = st.Rekey(func(string) string { return "" })
	if moved != 0 || merged != 0 || st.Len() != before {
		t.Fatalf("empty-string rekey was not a no-op: moved=%d merged=%d len=%d", moved, merged, st.Len())
	}
}

// TestRekeyPreservesNotified is the invariant that outranks everything else
// in a merge. Two old keys can name one posting (the same board recorded
// under two vendor hosts), and if the surviving record came back with
// Notified=false the user would be emailed a job they already read — and the
// state branch would reject the push for rolling a notified ID back to
// pending.
func TestRekeyPreservesNotified(t *testing.T) {
	cases := []struct {
		name string
		a, b Record
	}{
		{
			name: "delivered record is the older one",
			a: Record{FirstSeen: mustTime(t, "2026-01-01T00:00:00Z"), Title: "Acme: Engineer",
				Matched: true, Notified: true},
			b: Record{FirstSeen: mustTime(t, "2026-06-01T00:00:00Z"), Title: "Acme: Engineer"},
		},
		{
			name: "delivered record is the newer one",
			a:    Record{FirstSeen: mustTime(t, "2026-01-01T00:00:00Z"), Title: "Acme: Engineer"},
			b: Record{FirstSeen: mustTime(t, "2026-06-01T00:00:00Z"), Title: "Acme: Engineer",
				Matched: true, Notified: true},
		},
		{
			name: "pending match merges with an unmatched sighting",
			a: Record{FirstSeen: mustTime(t, "2026-01-01T00:00:00Z"), Title: "Acme: Engineer",
				Matched: true},
			b: Record{FirstSeen: mustTime(t, "2026-06-01T00:00:00Z"), Title: "Acme: Engineer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			st.Add("old/a/1", tc.a)
			st.Add("old/b/1", tc.b)

			moved, merged := st.Rekey(func(string) string { return "new/1" })
			if moved != 2 || merged != 1 {
				t.Fatalf("moved=%d merged=%d, want 2 and 1", moved, merged)
			}
			if st.Len() != 1 {
				t.Fatalf("store holds %d records, want 1", st.Len())
			}
			got, _ := st.Get("new/1")
			wantNotified := tc.a.Notified || tc.b.Notified
			if got.Notified != wantNotified {
				t.Errorf("Notified = %v, want %v", got.Notified, wantNotified)
			}
			if got.Notified && !got.Matched {
				t.Error("merged record is notified but not matched; the state validator rejects that")
			}
			if got.Matched != (tc.a.Matched || tc.b.Matched) {
				t.Errorf("Matched = %v, want the OR of the inputs", got.Matched)
			}
			// The earlier sighting is the true first-seen.
			if !got.FirstSeen.Equal(mustTime(t, "2026-01-01T00:00:00Z")) {
				t.Errorf("FirstSeen = %s, want the earlier of the two", got.FirstSeen)
			}
			if strings.TrimSpace(got.Title) == "" {
				t.Error("merged record lost its title; the state validator rejects that")
			}
		})
	}
}

// TestRekeyMergeIsOrderIndependent: Go randomizes map iteration, so a merge
// that depended on which key was reached first would produce a different
// state file on every run and a permanently churning state branch.
func TestRekeyMergeIsOrderIndependent(t *testing.T) {
	build := func() *Store {
		st := newStore(t)
		st.Add("old/aaa", Record{FirstSeen: mustTime(t, "2026-03-03T00:00:00Z"), Title: "C"})
		st.Add("old/bbb", Record{FirstSeen: mustTime(t, "2026-01-01T00:00:00Z"), Title: "A", Matched: true})
		st.Add("old/ccc", Record{FirstSeen: mustTime(t, "2026-02-02T00:00:00Z"), Title: "B", Matched: true, Notified: true})
		return st
	}
	var first Record
	for i := range 20 {
		st := build()
		if moved, merged := st.Rekey(func(string) string { return "new/1" }); moved != 3 || merged != 2 {
			t.Fatalf("moved=%d merged=%d, want 3 and 2", moved, merged)
		}
		got, ok := st.Get("new/1")
		if !ok {
			t.Fatal("merged record is missing")
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("merge result varied between runs:\n %+v\n %+v", got, first)
		}
	}
	if !first.Notified || !first.Matched || first.Title != "A" ||
		!first.FirstSeen.Equal(mustTime(t, "2026-01-01T00:00:00Z")) {
		t.Fatalf("merged record = %+v", first)
	}
}

func TestCountPostingPrefixIgnoresRunnerMetadata(t *testing.T) {
	st := newStore(t)
	for id, rec := range realWorldRecords(t) {
		st.Add(id, rec)
	}
	st.Add("workday/nvidia/9002", Record{FirstSeen: mustTime(t, "2026-08-06T00:00:00Z"), Title: "NVIDIA: SRE"})

	if got := st.CountPostingPrefix("workday/nvidia/"); got != 2 {
		t.Errorf("CountPostingPrefix(board) = %d, want 2", got)
	}
	if got := st.CountPostingPrefix("greenhouse/hubspot/"); got != 1 {
		t.Errorf("CountPostingPrefix(single) = %d, want 1", got)
	}
	if got := st.CountPostingPrefix("workday/citi/"); got != 0 {
		t.Errorf("CountPostingPrefix(unknown board) = %d, want 0", got)
	}
	// Markers must not be counted: the census asks "did this board have real
	// history", and its own marker is not history.
	if got := st.CountPostingPrefix("__jobwatch_source__/"); got != 0 {
		t.Errorf("CountPostingPrefix(markers) = %d, want 0", got)
	}
	// "" is what StatePrefix returns for a source type that does not
	// namespace its jobs; counting every record for it would report history
	// for a board that has none.
	if got := st.CountPostingPrefix(""); got != 0 {
		t.Errorf("CountPostingPrefix(\"\") = %d, want 0", got)
	}
}

func TestDeleteRemovesOneRecord(t *testing.T) {
	st := newStore(t)
	for id, rec := range realWorldRecords(t) {
		st.Add(id, rec)
	}
	before := st.Len()
	st.Delete("__jobwatch_source__/ashby/openai")
	if st.Seen("__jobwatch_source__/ashby/openai") {
		t.Error("Delete did not remove the record")
	}
	if st.Len() != before-1 {
		t.Fatalf("Len = %d, want %d", st.Len(), before-1)
	}
	st.Delete("does/not/exist") // must not panic or change anything
	if st.Len() != before-1 {
		t.Fatalf("deleting a missing key changed Len to %d", st.Len())
	}
}
