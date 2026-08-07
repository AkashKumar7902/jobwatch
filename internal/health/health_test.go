package health

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"jobwatch/internal/store"
)

// now is an arbitrary fixed instant; nothing here depends on the wall clock.
var now = time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

// quiet describes a board that returned lastN postings on its last non-empty
// fetch, has been proven non-empty on days distinct UTC days, and has been
// empty for zeroAge across zeroRuns successful fetches.
func quiet(lastN, days, zeroRuns int, zeroAge time.Duration) store.Health {
	h := store.Health{
		Company:       "Example",
		SrcType:       "greenhouse",
		LastNonEmptyN: lastN,
		NonEmptyDays:  days,
		Fetches:       200,
	}
	if zeroRuns > 0 {
		h.ZeroRuns = zeroRuns
		h.ZeroSince = now.Add(-zeroAge)
		h.LastNonEmpty = h.ZeroSince
	}
	return h
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		in    store.Health
		want  string
		check func(t *testing.T, got store.Health)
	}{
		{
			// Bitly, Eyeo, Olark, Shogun and ClearGlass have been empty
			// since 2026-07-16 and were never observed with an opening.
			// Requiring two proven non-empty days silences all five without
			// an allowlist to maintain.
			name: "legitimately empty board never fires",
			in:   quiet(0, 0, 3000, 3000*time.Hour),
			want: "",
		},
		{
			// A one-posting board is not evidence of anything. 30 days of
			// zeros still cannot clear the cliff floor.
			name: "single posting board never fires",
			in:   quiet(1, 9, 400, 30*24*time.Hour),
			want: "",
		},
		{
			name: "healthy board with no zero streak",
			in:   quiet(173, 6, 0, 0),
			want: "",
		},
		{
			// 71h of zeros: under the 72h floor that keeps a Friday-evening
			// freeze from mailing on Sunday.
			name: "zeros below the time floor",
			in:   quiet(173, 6, 40, 71*time.Hour),
			want: "",
		},
		{
			// Time alone is not enough: eight observations across three days
			// means the cadence itself is starved, and a starved cadence is
			// not evidence about the board.
			name: "zeros below the run floor",
			in:   quiet(173, 6, 8, 73*time.Hour),
			want: "",
		},
		{
			name: "dead board trips",
			in:   quiet(173, 6, 13, 73*time.Hour),
			want: Dead,
			check: func(t *testing.T, got store.Health) {
				if got.Kind != Dead {
					t.Fatalf("standing kind = %q, want %q", got.Kind, Dead)
				}
				if !got.RaisedAt.Equal(now) {
					t.Fatalf("RaisedAt = %v, want %v", got.RaisedAt, now)
				}
				if !got.SentAt.IsZero() {
					t.Fatal("Classify must not stamp SentAt; the caller does that after delivery")
				}
			},
		},
		{
			name: "cliff boundary at exactly ten postings",
			in:   quiet(MinCliffPostings, 2, MinZeroRuns, ZeroFor),
			want: Dead,
		},
		{
			name: "cliff boundary one posting below",
			in:   quiet(MinCliffPostings-1, 2, MinZeroRuns, ZeroFor),
			want: "",
		},
		{
			name: "one proven day is not two",
			in:   quiet(173, 1, 40, 96*time.Hour),
			want: "",
		},
		{
			name: "erroring board trips",
			in: store.Health{
				ErrRuns:  MinErrRuns,
				ErrSince: now.Add(-ErrFor),
				LastErr:  "unexpected status 404",
			},
			want: Erroring,
		},
		{
			name: "errors below the run floor",
			in: store.Health{
				ErrRuns:  MinErrRuns - 1,
				ErrSince: now.Add(-96 * time.Hour),
			},
			want: "",
		},
		{
			// The error text names the actual failure, which is strictly
			// more actionable than "went quiet".
			name: "erroring outranks dead when both hold",
			in: func() store.Health {
				h := quiet(173, 6, 24, 96*time.Hour)
				h.ErrRuns, h.ErrSince = 24, now.Add(-96*time.Hour)
				h.LastErr = "dial tcp: i/o timeout"
				return h
			}(),
			want: Erroring,
		},
		{
			name: "recovery clears the condition and starts the cooldown",
			in: store.Health{
				LastNonEmptyN: 173,
				NonEmptyDays:  6,
				Kind:          Dead,
				RaisedAt:      now.Add(-2 * time.Hour),
				SentAt:        now.Add(-2 * time.Hour),
			},
			want: "",
			check: func(t *testing.T, got store.Health) {
				if got.Kind != "" || !got.RaisedAt.IsZero() || !got.SentAt.IsZero() {
					t.Fatalf("recovery left condition behind: %+v", got)
				}
				if want := now.Add(ReAlertAfter); !got.EligibleAt.Equal(want) {
					t.Fatalf("EligibleAt = %v, want %v", got.EligibleAt, want)
				}
			},
		},
		{
			name: "second death inside the cooldown is suppressed",
			in: func() store.Health {
				h := quiet(173, 6, 13, 73*time.Hour)
				h.EligibleAt = now.Add(11 * 24 * time.Hour)
				return h
			}(),
			want: "",
			check: func(t *testing.T, got store.Health) {
				// Suppressed for delivery, but still standing — the digest
				// lists it, and it fires the moment the cooldown expires.
				if got.Kind != Dead {
					t.Fatalf("suppressed condition must still stand: %+v", got)
				}
			},
		},
		{
			name: "an already reported episode does not repeat",
			in: func() store.Health {
				h := quiet(173, 6, 13, 73*time.Hour)
				h.Kind, h.RaisedAt, h.SentAt = Dead, now.Add(-time.Hour), now.Add(-time.Hour)
				return h
			}(),
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, kind := Classify(tc.in, now)
			if kind != tc.want {
				t.Fatalf("Classify kind = %q, want %q", kind, tc.want)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// TestClassifyTaperIsNotDeath walks the shape that makes "compare against the
// last non-empty fetch" load-bearing: a company winding down hiring. Measured
// against an all-time peak of 12 this would fire; against its own last state
// of 1 it correctly never can.
func TestClassifyTaperIsNotDeath(t *testing.T) {
	start := now.Add(-30 * 24 * time.Hour)
	h, at := observe(store.Health{}, start, 24*time.Hour, 12, 7, 3, 1)
	if h.LastNonEmptyN != 1 || h.NonEmptyDays != 4 {
		t.Fatalf("taper folded wrong: %+v", h)
	}

	// Four days of emptiness at a realistic cadence, well past every floor
	// except the cliff.
	for i := 0; i < 48; i++ {
		at = at.Add(2 * time.Hour)
		h = Observe(h, Observation{Count: 0, At: at})
	}
	if h.ZeroRuns < MinZeroRuns || at.Sub(h.ZeroSince) < ZeroFor {
		t.Fatalf("zero streak did not reach the floors: runs=%d age=%v", h.ZeroRuns, at.Sub(h.ZeroSince))
	}
	if _, kind := Classify(h, at); kind != "" {
		t.Fatalf("a tapering board reported %q; only a cliff may alarm", kind)
	}
}

// TestClassifyAlternatingErrorsAndZeros is the case that fixes the streak
// semantics: neither streak may reset the other, or a board that fails half
// the time and returns an empty page the other half looks healthy forever.
func TestClassifyAlternatingErrorsAndZeros(t *testing.T) {
	start := now.Add(-40 * 24 * time.Hour)
	h, at := observe(store.Health{}, start, 24*time.Hour, 173, 160)

	boom := errors.New("unexpected status 404")
	firstErr, firstZero := time.Time{}, time.Time{}
	for i := 0; i < 48; i++ { // 96h at a 2h cadence
		at = at.Add(2 * time.Hour)
		if i%2 == 0 {
			h = Observe(h, Observation{Err: boom, At: at})
			if firstErr.IsZero() {
				firstErr = at
			}
			continue
		}
		h = Observe(h, Observation{Count: 0, At: at})
		if firstZero.IsZero() {
			firstZero = at
		}
	}

	if !h.ZeroSince.Equal(firstZero) {
		t.Fatalf("errors reset the zero streak: ZeroSince = %v, want %v", h.ZeroSince, firstZero)
	}
	if h.ZeroRuns != 24 {
		t.Fatalf("ZeroRuns = %d, want 24", h.ZeroRuns)
	}
	if !h.ErrSince.Equal(firstErr) || h.ErrRuns != 24 {
		t.Fatalf("empty successes reset the error streak: %+v", h)
	}

	got, kind := Classify(h, at)
	if kind != Erroring {
		t.Fatalf("Classify kind = %q, want %q", kind, Erroring)
	}
	if !got.ZeroSince.Equal(firstZero) {
		t.Fatal("Classify must not disturb the zero streak")
	}
}

// TestClassifyCooldownSequence walks the flapping story end to end: a board
// dies, recovers, dies again three days later, and mails only once until the
// two-week cooldown that started at recovery has expired.
func TestClassifyCooldownSequence(t *testing.T) {
	dead := quiet(173, 6, 13, 73*time.Hour)

	h, kind := Classify(dead, now)
	if kind != Dead {
		t.Fatalf("first death: kind = %q, want %q", kind, Dead)
	}
	h.SentAt = now // the caller stamps this after Report returns nil

	// Recovery.
	recovered := now.Add(time.Hour)
	h = Observe(h, Observation{Count: 140, At: recovered})
	h, kind = Classify(h, recovered)
	if kind != "" || h.Kind != "" {
		t.Fatalf("recovery: kind = %q, standing = %q", kind, h.Kind)
	}
	if want := recovered.Add(ReAlertAfter); !h.EligibleAt.Equal(want) {
		t.Fatalf("EligibleAt = %v, want %v", h.EligibleAt, want)
	}

	// Dead again three days later: same incident, no second email.
	secondDeath := recovered.Add(3 * 24 * time.Hour)
	h.ZeroRuns, h.ZeroSince = 13, secondDeath.Add(-73*time.Hour)
	h, kind = Classify(h, secondDeath)
	if kind != "" {
		t.Fatalf("second death inside cooldown reported %q, want silence", kind)
	}
	if h.Kind != Dead {
		t.Fatalf("standing condition = %q, want %q", h.Kind, Dead)
	}

	// Still dead at fifteen days: the cooldown has expired, so it mails.
	later := recovered.Add(15 * 24 * time.Hour)
	if _, kind = Classify(h, later); kind != Dead {
		t.Fatalf("after the cooldown: kind = %q, want %q", kind, Dead)
	}
}

func TestObserve(t *testing.T) {
	t.Run("windows stay capped and keep the newest entries", func(t *testing.T) {
		h := store.Health{}
		at := now
		for i := 1; i <= 60; i++ {
			at = at.Add(time.Hour)
			h = Observe(h, Observation{Count: i, At: at})
		}
		if len(h.Recent) != RecentCounts {
			t.Fatalf("len(Recent) = %d, want %d", len(h.Recent), RecentCounts)
		}
		if len(h.Nonzero) != NonzeroCounts {
			t.Fatalf("len(Nonzero) = %d, want %d", len(h.Nonzero), NonzeroCounts)
		}
		if h.Recent[0] != 13 || h.Recent[len(h.Recent)-1] != 60 {
			t.Fatalf("Recent = %v, want the last %d counts oldest first", h.Recent, RecentCounts)
		}
		if h.Nonzero[0] != 45 || h.Nonzero[len(h.Nonzero)-1] != 60 {
			t.Fatalf("Nonzero = %v", h.Nonzero)
		}
		// 45..60 -> median of the two middle values, 52 and 53.
		if h.Typical != 52 {
			t.Fatalf("Typical = %d, want 52", h.Typical)
		}
		if h.Fetches != 60 {
			t.Fatalf("Fetches = %d, want 60", h.Fetches)
		}
	})

	t.Run("errors leave the success windows alone", func(t *testing.T) {
		h, at := observe(store.Health{}, now, time.Hour, 40, 0, 0)
		before := h

		at = at.Add(time.Hour)
		got := Observe(h, Observation{Err: errors.New("dial tcp: i/o timeout"), At: at})

		if len(got.Recent) != len(before.Recent) {
			t.Fatalf("an error appended to Recent: %v -> %v", before.Recent, got.Recent)
		}
		if got.ZeroRuns != before.ZeroRuns || !got.ZeroSince.Equal(before.ZeroSince) {
			t.Fatalf("an error moved the zero streak: %d/%v -> %d/%v",
				before.ZeroRuns, before.ZeroSince, got.ZeroRuns, got.ZeroSince)
		}
		if got.Fetches != before.Fetches || !got.LastOK.Equal(before.LastOK) {
			t.Fatal("an error counted as a successful fetch")
		}
		if got.ErrRuns != 1 || !got.ErrSince.Equal(at) {
			t.Fatalf("error streak = %d since %v", got.ErrRuns, got.ErrSince)
		}
		if got.LastErr == "" {
			t.Fatal("LastErr not recorded")
		}
	})

	t.Run("non-empty clears both streaks", func(t *testing.T) {
		h := store.Health{
			ZeroRuns: 30, ZeroSince: now.Add(-100 * time.Hour),
			ErrRuns: 30, ErrSince: now.Add(-100 * time.Hour), LastErr: "boom",
		}
		got := Observe(h, Observation{Count: 12, At: now})
		if got.ZeroRuns != 0 || !got.ZeroSince.IsZero() {
			t.Fatalf("zero streak survived a non-empty fetch: %+v", got)
		}
		if got.ErrRuns != 0 || !got.ErrSince.IsZero() || got.LastErr != "" {
			t.Fatalf("error streak survived a non-empty fetch: %+v", got)
		}
	})

	t.Run("non-empty days count distinct UTC days", func(t *testing.T) {
		h := store.Health{}
		day := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
		for i := 0; i < 5; i++ { // five fetches inside one UTC day
			h = Observe(h, Observation{Count: 3, At: day.Add(time.Duration(i) * 4 * time.Hour)})
		}
		if h.NonEmptyDays != 1 {
			t.Fatalf("NonEmptyDays = %d after one day, want 1", h.NonEmptyDays)
		}
		// 23:30 IST on the 2nd is 18:00 UTC on the 2nd: still a new day.
		ist := time.FixedZone("IST", 5*3600+1800)
		h = Observe(h, Observation{Count: 3, At: time.Date(2026, 8, 2, 23, 30, 0, 0, ist)})
		if h.NonEmptyDays != 2 {
			t.Fatalf("NonEmptyDays = %d after a second day, want 2", h.NonEmptyDays)
		}
	})

	t.Run("identity and first fetch are recorded once", func(t *testing.T) {
		h := Observe(store.Health{}, Observation{
			Company: "HubSpot", SrcType: "greenhouse", Err: errors.New("boom"), At: now,
		})
		if h.Company != "HubSpot" || h.SrcType != "greenhouse" {
			t.Fatalf("identity not recorded: %+v", h)
		}
		if !h.FirstFetch.Equal(now) {
			t.Fatalf("FirstFetch = %v, want %v (attempts, not successes)", h.FirstFetch, now)
		}
		h = Observe(h, Observation{Count: 5, At: now.Add(time.Hour)})
		if !h.FirstFetch.Equal(now) {
			t.Fatal("FirstFetch moved")
		}
	})

	t.Run("does not alias the caller's windows", func(t *testing.T) {
		h, _ := observe(store.Health{}, now, time.Hour, 1, 2, 3)
		snapshot := append([]int(nil), h.Recent...)
		Observe(h, Observation{Count: 99, At: now.Add(10 * time.Hour)})
		for i := range snapshot {
			if h.Recent[i] != snapshot[i] {
				t.Fatalf("Observe mutated the caller's Recent: %v, want %v", h.Recent, snapshot)
			}
		}
	})

	t.Run("stores error text bounded and valid", func(t *testing.T) {
		// A fetch error routinely quotes a response body, so it can carry
		// both hostile length and invalid bytes. Invalid UTF-8 in state
		// would make the state branch validator reject the whole push.
		long := Observe(store.Health{}, Observation{
			Err: errors.New(strings.Repeat("☃", 300)), At: now,
		})
		if len(long.LastErr) > MaxErrBytes+len("…") {
			t.Fatalf("LastErr is %d bytes, want at most %d", len(long.LastErr), MaxErrBytes+len("…"))
		}
		if !utf8.ValidString(long.LastErr) {
			t.Fatalf("LastErr is not valid UTF-8: %q", long.LastErr)
		}
		garbage := Observe(store.Health{}, Observation{
			Err: errors.New("body: \xff\xfe bad"), At: now,
		})
		if !utf8.ValidString(garbage.LastErr) {
			t.Fatalf("LastErr is not valid UTF-8: %q", garbage.LastErr)
		}
	})
}

// The stillborn rule is the only one that can fire on day one, so the thing
// worth pinning is what it REFUSES to fire on. Baselined is the whole safety
// property: it says we watched this board from the moment we adopted it, which
// is what separates "this token was already wrong" from "this established
// board happened to be quiet today".
func TestClassifyStillborn(t *testing.T) {
	adopted := store.Health{
		Company: "Newco", SrcType: "greenhouse",
		Baselined: true, Fetches: 1, Recent: []int{0},
		ZeroRuns: 1, ZeroSince: now,
	}

	tests := []struct {
		name string
		in   store.Health
		want string
	}{
		{
			name: "board we adopted that returned nothing",
			in:   adopted,
			want: Stillborn,
		},
		{
			// The five boards legitimately empty since 2026-07-16 are in
			// exactly this state. Switching health tracking on must not
			// accuse a single one of them.
			name: "inherited board that has only ever been empty",
			in: func() store.Health {
				h := adopted
				h.Baselined = false
				h.Fetches, h.ZeroRuns = 3000, 3000
				return h
			}(),
			want: "",
		},
		{
			name: "adopted board that has produced a posting is not stillborn",
			in: func() store.Health {
				h := adopted
				h.LastNonEmpty, h.LastNonEmptyN, h.NonEmptyDays = now.Add(-time.Hour), 4, 1
				return h
			}(),
			want: "",
		},
		{
			name: "adopted board with no successful fetch yet is not stillborn",
			in: func() store.Health {
				h := adopted
				h.Fetches, h.ZeroRuns, h.Recent = 0, 0, nil
				h.ErrRuns, h.ErrSince, h.LastErr = 1, now, "dial tcp: i/o timeout"
				return h
			}(),
			want: "",
		},
		{
			// The error names the actual failure; "returned nothing" does not.
			name: "erroring outranks stillborn",
			in: func() store.Health {
				h := adopted
				h.ErrRuns, h.ErrSince, h.LastErr = MinErrRuns, now.Add(-ErrFor), "unexpected status 404"
				return h
			}(),
			want: Erroring,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, kind := Classify(tc.in, now); kind != tc.want {
				t.Fatalf("Classify kind = %q, want %q", kind, tc.want)
			}
		})
	}

	// It is reported once, stays standing for the digest, and clears the
	// moment the board finally produces a posting.
	h, kind := Classify(adopted, now)
	if kind != Stillborn {
		t.Fatalf("first classification = %q", kind)
	}
	h.SentAt = now // what the runner stamps after delivery
	if _, kind := Classify(h, now.Add(time.Hour)); kind != "" {
		t.Fatalf("delivered stillborn report repeated: %q", kind)
	}
	alive := Observe(h, Observation{Count: 7, At: now.Add(2 * time.Hour)})
	revived, kind := Classify(alive, now.Add(2*time.Hour))
	if kind != "" || revived.Kind != "" {
		t.Fatalf("a board that came alive is still stillborn: kind=%q standing=%q", kind, revived.Kind)
	}
}

// The digest predicates exist so that everything the alarm rule deliberately
// declines to watch is still SAID OUT LOUD once a month. A coverage gap the
// user cannot see is one they will assume does not exist.
func TestDigestPredicates(t *testing.T) {
	t.Run("below normal never overlaps the alarm", func(t *testing.T) {
		// Halved, which is what a layoff or a hiring freeze looks like. It
		// belongs in a monthly line, never in an email.
		halved := store.Health{Typical: 40, LastNonEmptyN: 18, NonEmptyDays: 9, Recent: []int{40, 18}}
		if !BelowNormal(halved) {
			t.Error("a board at 18 of a typical 40 is not flagged for the digest")
		}
		if _, kind := Classify(halved, now); kind != "" {
			t.Errorf("a partial drop raised %q; ratios must never alarm", kind)
		}
		if BelowNormal(store.Health{Typical: 40, Recent: []int{40, 30}}) {
			t.Error("a board at 30 of 40 was called well below normal")
		}
		// A tiny board has no meaningful "normal" to be below.
		if BelowNormal(store.Health{Typical: MinCliffPostings - 1, Recent: []int{0}}) {
			t.Error("a sub-threshold board was measured against its own noise")
		}
	})

	t.Run("never proven alive needs both age and volume", func(t *testing.T) {
		alive := func(fetches int, age time.Duration) store.Health {
			return store.Health{Fetches: fetches, FirstFetch: now.Add(-age)}
		}
		if !NeverProvenAlive(alive(NeverFetches, NeverAge), now) {
			t.Error("a board watched long and often with no posting is not listed")
		}
		if NeverProvenAlive(alive(NeverFetches-1, NeverAge), now) {
			t.Error("too few fetches was still called never-alive")
		}
		if NeverProvenAlive(alive(NeverFetches, NeverAge-time.Hour), now) {
			t.Error("too young a board was still called never-alive")
		}
		h := alive(NeverFetches, NeverAge)
		h.LastNonEmpty = now.Add(-time.Hour)
		if NeverProvenAlive(h, now) {
			t.Error("a board that has produced a posting was called never-alive")
		}
	})

	t.Run("unmonitored is exactly the cliff floor", func(t *testing.T) {
		if !Unmonitored(store.Health{LastNonEmptyN: MinCliffPostings - 1}) {
			t.Error("a board under the cliff floor is not declared unmonitored")
		}
		if Unmonitored(store.Health{LastNonEmptyN: MinCliffPostings}) {
			t.Error("a board at the cliff floor was declared unmonitored")
		}
	})

	t.Run("digest cadence", func(t *testing.T) {
		if !DigestDue(store.Run{}, now) {
			t.Error("the first digest must go out immediately as install confirmation")
		}
		if DigestDue(store.Run{DigestSentAt: now.Add(-DigestEvery + time.Minute)}, now) {
			t.Error("digest sent early")
		}
		if !DigestDue(store.Run{DigestSentAt: now.Add(-DigestEvery)}, now) {
			t.Error("digest not due at the interval")
		}
	})

	t.Run("last count reads the newest successful fetch", func(t *testing.T) {
		if got := LastCount(store.Health{Recent: []int{40, 12, 0}}); got != 0 {
			t.Errorf("LastCount = %d, want 0", got)
		}
		if got := LastCount(store.Health{}); got != 0 {
			t.Errorf("LastCount of an unobserved board = %d, want 0", got)
		}
	})
}

func TestSrcType(t *testing.T) {
	tests := map[string]string{
		"greenhouse/us/hubspot":       "greenhouse",
		"workday/host/tenant/site":    "workday",
		"atlassian":                   "atlassian",
		"eightfold/host/domain/query": "eightfold",
	}
	for identity, want := range tests {
		if got := SrcType(identity); got != want {
			t.Fatalf("SrcType(%q) = %q, want %q", identity, got, want)
		}
	}
}

func TestMedian(t *testing.T) {
	tests := []struct {
		in   []int
		want int
	}{
		{nil, 0},
		{[]int{7}, 7},
		{[]int{3, 1, 2}, 2},
		{[]int{4, 1, 3, 2}, 2},
		{[]int{102, 26, 8, 429, 1}, 26},
	}
	for _, tc := range tests {
		before := append([]int(nil), tc.in...)
		if got := median(tc.in); got != tc.want {
			t.Fatalf("median(%v) = %d, want %d", tc.in, got, tc.want)
		}
		for i := range before {
			if tc.in[i] != before[i] {
				t.Fatalf("median sorted its argument in place: %v, want %v", tc.in, before)
			}
		}
	}
}

// observe folds a sequence of successful fetches spaced step apart and returns
// the health plus the time of the last observation.
func observe(h store.Health, start time.Time, step time.Duration, counts ...int) (store.Health, time.Time) {
	at := start
	for i, n := range counts {
		if i > 0 {
			at = at.Add(step)
		}
		h = Observe(h, Observation{Company: "Example", SrcType: "greenhouse", Count: n, At: at})
	}
	return h, at
}
