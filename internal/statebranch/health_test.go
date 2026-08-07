package statebranch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/store"
)

// TestValidateAcceptsHealthRecordsProducedByStore closes the loop between the
// writer and the gate. Board health is only useful if it survives a push, and
// a validator that rejects what the poller writes does not merely lose the
// health data — it fails the whole state branch update, so the watcher stops
// remembering which jobs it already emailed. Marshaling real store types here
// means adding a field without extending decodeHealth fails in unit tests
// instead of in production.
func TestValidateAcceptsHealthRecordsProducedByStore(t *testing.T) {
	at := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	records := map[string]store.Record{
		"greenhouse/hubspot/4455": {
			FirstSeen: at, Title: "HubSpot: Software Engineer I", Matched: true, Notified: true,
		},
		"__jobwatch_source__/greenhouse/us/hubspot": {
			FirstSeen: at, Title: "source: HubSpot",
		},
		"__jobwatch_health__/greenhouse/us/hubspot": {
			FirstSeen: at,
			Title:     "health: HubSpot",
			Health: &store.Health{
				Company: "HubSpot", SrcType: "greenhouse",
				FirstFetch: at.Add(-30 * 24 * time.Hour), LastOK: at,
				LastNonEmpty: at.Add(-96 * time.Hour), LastNonEmptyN: 173,
				NonEmptyDays: 6, Fetches: 244,
				Recent: []int{173, 0, 0, 0}, Nonzero: []int{160, 173}, Typical: 166,
				ZeroRuns: 13, ZeroSince: at.Add(-96 * time.Hour),
				Kind: "dead", RaisedAt: at,
			},
		},
		// A board observed only through failures: nil windows marshal as
		// null, and the timestamps it has never reached stay zero.
		"__jobwatch_health__/lever/us/example": {
			FirstSeen: at,
			Title:     "health: Example",
			Health: &store.Health{
				Company: "Example", SrcType: "lever",
				FirstFetch: at.Add(-8 * 24 * time.Hour),
				ErrRuns:    14, ErrSince: at.Add(-96 * time.Hour),
				LastErr: "unexpected status 404", Kind: "erroring", RaisedAt: at,
			},
		},
		// A board WE adopted that has never once had a posting. Baselined is
		// what separates it from an established board that was merely quiet
		// the day health tracking switched on, so a validator that dropped the
		// field would turn the first cycle after any schema change into a
		// fleet-wide accusation.
		"__jobwatch_health__/greenhouse/us/newco": {
			FirstSeen: at,
			Title:     "health: Newco",
			Health: &store.Health{
				Company: "Newco", SrcType: "greenhouse",
				FirstFetch: at, LastOK: at, Fetches: 1, Baselined: true,
				Recent: []int{0}, ZeroRuns: 1, ZeroSince: at,
				Kind: "stillborn", RaisedAt: at, SentAt: at,
			},
		},
		"__jobwatch_health__/@run": {
			FirstSeen: at,
			Title:     "health: run",
			Run: &store.Run{
				LastRunAt: at, AllFailRuns: 1, AllFailSentAt: at.Add(-time.Hour),
				DigestSentAt:         at.Add(-20 * 24 * time.Hour),
				EvaluatedSinceDigest: 4120, MatchedSinceDigest: 7, DeferredSinceDigest: 3,
			},
		},
	}

	// The same encoding Store.Save uses.
	data, err := json.MarshalIndent(records, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"health"`) || !strings.Contains(string(data), `"run"`) {
		t.Fatalf("fixture did not exercise the new fields:\n%s", data)
	}
	decoded, err := validateState(data)
	if err != nil {
		t.Fatalf("validateState rejected state written by store: %v\n%s", err, data)
	}
	if len(decoded) != len(records) {
		t.Fatalf("decoded %d records, want %d", len(decoded), len(records))
	}
}

func TestValidateStateRejectsInvalidHealth(t *testing.T) {
	const healthID = "__jobwatch_health__/greenhouse/us/hubspot"
	wrap := func(id, extra string) string {
		return `{"` + id + `":{"first_seen":"2026-08-05T01:02:03Z","title":"health: HubSpot",` +
			`"matched":false,"notified":false,` + extra + `}}`
	}
	tests := map[string]string{
		"unknown health field":   wrap(healthID, `"health":{"zero_runs":3,"mystery":1}`),
		"duplicate health field": wrap(healthID, `"health":{"zero_runs":3,"zero_runs":4}`),
		"health not an object":   wrap(healthID, `"health":[]`),
		"health on a posting":    wrap("greenhouse/hubspot/4455", `"health":{"zero_runs":3}`),
		"health on source marker": wrap("__jobwatch_source__/greenhouse/us/hubspot",
			`"health":{"zero_runs":3}`),
		"run outside the singleton": wrap(healthID, `"run":{"all_fail_runs":1}`),
		"unknown run field":         wrap("__jobwatch_health__/@run", `"run":{"mystery":1}`),
		"unknown kind":              wrap(healthID, `"health":{"kind":"broken"}`),
		"kind not a string":         wrap(healthID, `"health":{"kind":3}`),
		"baselined not a boolean":   wrap(healthID, `"health":{"baselined":"yes"}`),
		"run count not a number":    wrap("__jobwatch_health__/@run", `"run":{"evaluated_since_digest":"3"}`),
		"run timestamp not a time":  wrap("__jobwatch_health__/@run", `"run":{"all_fail_sent_at":"soon"}`),
		"count not a number":        wrap(healthID, `"health":{"zero_runs":"3"}`),
		"fractional count":          wrap(healthID, `"health":{"zero_runs":3.5}`),
		"negative count":            wrap(healthID, `"health":{"zero_runs":-1}`),
		"bad timestamp":             wrap(healthID, `"health":{"last_ok":"yesterday"}`),
		"timestamp not a string":    wrap(healthID, `"health":{"last_ok":1}`),
		"recent not an array":       wrap(healthID, `"health":{"recent":3}`),
		"recent element not a count": wrap(healthID,
			`"health":{"recent":[1,"2"]}`),
		"recent window overflow":  wrap(healthID, `"health":{"recent":`+counts(maxRecentCounts+1)+`}`),
		"nonzero window overflow": wrap(healthID, `"health":{"nonzero":`+counts(maxNonzeroCounts+1)+`}`),
		"oversized last_err": wrap(healthID,
			`"health":{"last_err":"`+strings.Repeat("x", maxLastErrBytes+1)+`"}`),
		// The record's own invariants still apply to health keys: they are
		// ordinary records that happen to carry an extra object.
		"health record with empty title": `{"` + healthID + `":{"first_seen":"2026-08-05T01:02:03Z",` +
			`"title":"","matched":false,"notified":false,"health":{"zero_runs":3}}}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := validateState([]byte(input)); err == nil {
				t.Fatalf("validateState unexpectedly accepted invalid input: %s", input)
			}
		})
	}
}

// TestValidateAcceptsSparseHealth documents that health fields are individually
// optional: zero is a meaningful value here (a board that has never returned a
// posting genuinely has no last_non_empty), so absence must decode rather than
// fail, unlike the four fields every record must carry.
func TestValidateAcceptsSparseHealth(t *testing.T) {
	state := `{"__jobwatch_health__/greenhouse/us/hubspot":{` +
		`"first_seen":"2026-08-05T01:02:03Z","title":"health: HubSpot",` +
		`"matched":false,"notified":false,"health":{},"run":null}}`
	if _, err := validateState([]byte(state)); err == nil {
		t.Fatal("run on a non-singleton key must still be rejected")
	}

	state = `{"__jobwatch_health__/greenhouse/us/hubspot":{` +
		`"first_seen":"2026-08-05T01:02:03Z","title":"health: HubSpot",` +
		`"matched":false,"notified":false,"health":{"recent":null,"kind":""}}}`
	if _, err := validateState([]byte(state)); err != nil {
		t.Fatalf("sparse health rejected: %v", err)
	}
}

func counts(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "1"
	}
	return "[" + strings.Join(parts, ",") + "]"
}
