package statebranch

// Tests for the removal gate.
//
// This is the single highest-consequence piece of the migration. The state
// branch is append-only, and 39,410 of 71,582 keys are about to move; if the
// gate is wrong in the permissive direction the branch silently loses records
// and the user is re-emailed jobs they already read, and if it is wrong in the
// strict direction the push fails every cycle and the watcher stops remembering
// anything at all. Both failures are invisible until they are expensive, so
// each accepted explanation and each refusal is pinned here.

import (
	"strings"
	"testing"
)

// rec renders one state record. The gate only ever reads first_seen, title,
// notified and the marker prefixes, so everything else is held constant.
func rec(notified bool) string {
	matched := "false"
	if notified {
		matched = "true"
	}
	return `{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","matched":` + matched +
		`,"notified":` + boolText(notified) + `}`
}

// marker renders a source-marker record. An empty previous is omitted entirely,
// which is what a board that never declared a move looks like on disk.
func marker(prefix, previous string) string {
	m := `"state_prefix":"` + prefix + `"`
	if previous != "" {
		m += `,"previous_state_prefix":"` + previous + `"`
	}
	return `{"first_seen":"2026-08-05T01:02:03Z","title":"source: Acme","matched":false,"notified":false,` +
		`"marker":{` + m + `,"announced_at":"0001-01-01T00:00:00Z"}}`
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func state(t *testing.T, entries ...string) map[string]stateRecord {
	t.Helper()
	records, err := validateState([]byte("{" + strings.Join(entries, ",") + "}"))
	if err != nil {
		t.Fatalf("fixture is not valid state: %v", err)
	}
	return records
}

const (
	// The real shape of a Workday key with the vendor's shard baked in, and
	// where the frozen rule moves it.
	workdayOld = `"workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/citi/JR123"`
	workdayNew = `"workday/citi/citi/JR123"`

	// A bare fetch base URL with no posting path: one of the 18 records no
	// adapter alive can produce.
	workdayJunk = `"workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/citi"`
)

func TestCheckRemovalsAcceptsOnlyExplainedRemovals(t *testing.T) {
	tests := []struct {
		name    string
		base    []string
		cand    []string
		wantErr string
	}{
		{
			name: "nothing removed",
			base: []string{workdayNew + ":" + rec(true)},
			cand: []string{workdayNew + ":" + rec(true)},
		},
		{
			// The migration itself: the key moved to exactly where the frozen
			// pure function says it went.
			name: "rekeyed to the frozen rule's output",
			base: []string{workdayOld + ":" + rec(true)},
			cand: []string{workdayNew + ":" + rec(true)},
		},
		{
			// The invariant that matters most: a record that was emailed must
			// not reappear as pending, however it moved.
			name:    "rekeyed but rolled back to pending",
			base:    []string{workdayOld + ":" + rec(true)},
			cand:    []string{workdayNew + ":" + rec(false)},
			wantErr: "rolled it back to pending",
		},
		{
			name:    "removed with no destination at all",
			base:    []string{workdayOld + ":" + rec(false)},
			cand:    []string{`"unrelated/1":` + rec(false)},
			wantErr: "removed existing job ID",
		},
		{
			// A key that looks rekeyable is not a licence to drop it: the rule
			// names where it must land, and it must be there.
			name:    "rekeyable key dropped instead of moved",
			base:    []string{workdayOld + ":" + rec(false)},
			cand:    []string{},
			wantErr: "removed existing job ID",
		},
		{
			name: "provably unrecreatable bare-base key",
			base: []string{workdayJunk + ":" + rec(false)},
			cand: []string{},
		},
		{
			// Junk is defined by its shape, not by permission to lose mail. If
			// one were ever notified, dropping it would be a silent re-send.
			name:    "notified bare-base key",
			base:    []string{workdayJunk + ":" + rec(true)},
			cand:    []string{},
			wantErr: "removed notified job ID",
		},
		{
			// The census sweep: a board left the config owning nothing, so its
			// marker is derived bookkeeping with nothing left to describe.
			name: "marker swept while owning no history",
			base: []string{`"__jobwatch_source__/test/gone":` + marker("test/gone/", "")},
			cand: []string{`"test/kept/1":` + rec(false)},
		},
		{
			// The census never does this, and a bug that made it would erase
			// the only proof the board was adopted — after which the next run
			// re-baselines it and its whole backlog goes silent.
			name:    "marker swept while its history survives",
			base:    []string{`"__jobwatch_source__/test/gone":` + marker("test/gone/", "")},
			cand:    []string{`"test/gone/1":` + rec(false)},
			wantErr: "still live under",
		},
		{
			// A marker from before prefixes were recorded cannot prove what it
			// owned and cannot be re-derived (identities are not parseable), so
			// refusing it would mean a pre-schema marker can never be dropped.
			name: "legacy marker with no recorded prefix",
			base: []string{`"__jobwatch_source__/test/gone":` + rec(false)},
			cand: []string{`"test/gone/1":` + rec(false)},
		},
		{
			// Health records, seed-in-progress markers and the registry are
			// load-bearing: nothing explains their disappearance.
			name:    "runner bookkeeping dropped",
			base:    []string{`"__jobwatch_health__/test/acme":` + rec(false)},
			cand:    []string{},
			wantErr: "runner bookkeeping is never dropped",
		},
		{
			// The escape hatch, authorized entirely from the candidate's own
			// marker — no config, no clock, no network.
			name: "declared previous_state_prefix move",
			base: []string{`"icims/old-tenant/1":` + rec(true)},
			cand: []string{
				`"__jobwatch_source__/icims/new-tenant":` + marker("icims/new-tenant/", "icims/old-tenant/"),
				`"icims/new-tenant/1":` + rec(true),
			},
			wantErr: "",
		},
		{
			name: "declared move that rolls delivery back",
			base: []string{`"icims/old-tenant/1":` + rec(true)},
			cand: []string{
				`"__jobwatch_source__/icims/new-tenant":` + marker("icims/new-tenant/", "icims/old-tenant/"),
				`"icims/new-tenant/1":` + rec(false),
			},
			wantErr: "rolled it back to pending",
		},
		{
			name: "undeclared prefix move",
			base: []string{`"icims/old-tenant/1":` + rec(false)},
			cand: []string{`"icims/new-tenant/1":` + rec(false)},
			// No marker declares it, so from the gate's side this is just a
			// record that vanished and another that appeared.
			wantErr: "removed existing job ID",
		},
		{
			// The two hops compose: this is the exact sequence a user follows
			// after pasting previous_state_prefix out of a migration email, and
			// the resulting key matches neither hop on its own.
			name: "frozen rekey followed by a declared move",
			base: []string{workdayOld + ":" + rec(true)},
			cand: []string{
				`"__jobwatch_source__/workday/newco/site":` + marker("workday/newco/site/", "workday/citi/citi/"),
				`"workday/newco/site/JR123":` + rec(true),
			},
		},
		{
			// Two boards claiming one history have no way to split it, so the
			// removals it would authorize are refused rather than resolved.
			name: "two markers claiming the same absorbed prefix",
			base: []string{`"icims/old-tenant/1":` + rec(false)},
			cand: []string{
				`"__jobwatch_source__/icims/a":` + marker("icims/a/", "icims/old-tenant/"),
				`"__jobwatch_source__/icims/b":` + marker("icims/b/", "icims/old-tenant/"),
			},
			wantErr: "moving to both",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRemovals(state(t, tc.base...), state(t, tc.cand...))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("legitimate removal refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// A marker's two prefixes are the only nested values the gate reads, and one of
// them AUTHORIZES removals. A shape the gate cannot act on coherently must be
// rejected at decode time rather than reasoned about later.
func TestMarkerPrefixesAreValidatedBeforeTheyCanAuthorizeAnything(t *testing.T) {
	tests := map[string]string{
		"previous with no destination": `{"__jobwatch_source__/x":{"first_seen":"2026-08-05T01:02:03Z","title":"source: X",` +
			`"matched":false,"notified":false,"marker":{"state_prefix":"","previous_state_prefix":"old/"}}}`,
		"previous equal to destination": `{"__jobwatch_source__/x":{"first_seen":"2026-08-05T01:02:03Z","title":"source: X",` +
			`"matched":false,"notified":false,"marker":{"state_prefix":"a/","previous_state_prefix":"a/"}}}`,
		"marker on a posting": `{"lever/acme/1":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer",` +
			`"matched":false,"notified":false,"marker":{"state_prefix":"a/"}}}`,
		"unknown marker field": `{"__jobwatch_source__/x":{"first_seen":"2026-08-05T01:02:03Z","title":"source: X",` +
			`"matched":false,"notified":false,"marker":{"state_prefix":"a/","moved_from":"b/"}}}`,
		"duplicate marker field": `{"__jobwatch_source__/x":{"first_seen":"2026-08-05T01:02:03Z","title":"source: X",` +
			`"matched":false,"notified":false,"marker":{"state_prefix":"a/","state_prefix":"b/"}}}`,
		"announced_at not a timestamp": `{"__jobwatch_source__/x":{"first_seen":"2026-08-05T01:02:03Z","title":"source: X",` +
			`"matched":false,"notified":false,"marker":{"state_prefix":"a/","announced_at":"soon"}}}`,
		"orphaned_at not a timestamp": `{"__jobwatch_source__/x":{"first_seen":"2026-08-05T01:02:03Z","title":"source: X",` +
			`"matched":false,"notified":false,"marker":{"state_prefix":"a/","orphaned_at":"lately"}}}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := validateState([]byte(input)); err == nil {
				t.Fatal("validateState accepted a marker the gate cannot act on")
			}
		})
	}
}

// The shapes the runner actually writes must survive validation, or the branch
// stops advancing on the first run after deployment.
func TestMarkerShapesTheRunnerWritesAreAccepted(t *testing.T) {
	records := state(t,
		`"__jobwatch_source__/workday/citi/citi":`+marker("workday/citi/citi/", ""),
		`"__jobwatch_source__/icims/new":`+marker("icims/new/", "icims/old/"),
		// An announced orphan: raised, delivered, stamped.
		`"__jobwatch_source__/workday/old/site":{"first_seen":"2026-08-05T01:02:03Z","title":"source: Old",`+
			`"matched":false,"notified":false,"marker":{"state_prefix":"workday/old/site/",`+
			`"moved_to":"workday/new/site","announced_at":"2026-08-07T12:00:00Z"}}`,
		// An orphan the census adjudicated and found nothing to pair with. It
		// is written by a code path that never delivers anything, so it must
		// validate on its own rather than only alongside moved_to.
		`"__jobwatch_source__/greenhouse/us/gone":{"first_seen":"2026-08-05T01:02:03Z","title":"source: Gone",`+
			`"matched":false,"notified":false,"marker":{"state_prefix":"greenhouse/gone/",`+
			`"announced_at":"0001-01-01T00:00:00Z","orphaned_at":"2026-08-07T12:00:00Z"}}`,
		// A legacy marker, which is what every marker looks like today.
		`"__jobwatch_source__/lever/acme":`+rec(false),
	)
	if len(records) != 5 {
		t.Fatalf("record count = %d, want 5", len(records))
	}
	if got := records["__jobwatch_source__/icims/new"].previousStatePrefix; got != "icims/old/" {
		t.Fatalf("previous_state_prefix = %q, want it lifted for the gate", got)
	}
	if got := records["__jobwatch_source__/workday/citi/citi"].statePrefix; got != "workday/citi/citi/" {
		t.Fatalf("state_prefix = %q, want it lifted for the gate", got)
	}
}
