// Package health decides whether a job board has stopped working.
//
// The problem it exists to solve: roughly 41 of ~50 ATS adapters answer a
// renamed slug, token or tenant with HTTP 200 and an empty list. That is
// byte-identical to a company with genuinely no openings, and the runner only
// reports an error when EVERY board fails — so 259 of 260 boards can be dead
// while CI stays green. The user reads email only; a red check or a step
// summary never reaches them.
//
// Everything here is a pure fold over a store.Health value: Observe takes one
// fetch outcome and returns the updated record, Classify reads that record and
// says whether it should be reported. No I/O, no clock of its own, no
// backfill. That last part is what makes cold start silent by construction —
// a fresh record cannot satisfy MinNonEmptyDays on its first day no matter
// what the board returns, so there is no flag day and no initialization mode.
//
// The one rule that can raise an alarm is the cliff test in Classify. It never
// looks at ratios or partial drops: a board falling from 400 postings to 40 is
// what a hiring freeze and a layoff look like, and both are far more common
// than a broken adapter. Partial drops belong in the monthly digest, where
// being wrong costs a line of text instead of a false alarm.
package health

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

// Reserved key space for health bookkeeping. It is deliberately NOT the
// source-marker namespace: the mere existence of a "__jobwatch_source__/"
// record means "this board is already baselined", so writing health under it
// would suppress a legitimate seeding pass — or, if it were removed later,
// trigger a full-board notification blast.
const (
	KeyPrefix = "__jobwatch_health__/"

	// RunKey holds the one fleet-wide record. "@" cannot collide with a
	// board identity because every identity starts with an adapter name.
	RunKey = KeyPrefix + "@run"
)

// The standing conditions a board can be in. Anything else is healthy.
const (
	Dead     = "dead"
	Erroring = "erroring"

	// Stillborn is a board WE baselined that has never once returned a
	// posting. It is the only condition that can fire on day one, and the
	// only one that catches the incident this whole package exists for: a
	// greenhouse token renamed before it was ever watched answers HTTP 200
	// with an empty board forever, and probing the endpoint does not help
	// (a live probe of /v1/boards/hubspot returns 200 with a real board
	// name). Waiting for the cliff test would take 21-30 days; this takes
	// one run.
	Stillborn = "stillborn"
)

// Shipped thresholds. These are policy, not configuration: config.go uses
// KnownFields(true), so every knob is a permanent schema commitment, and a
// threshold the user can turn down is a threshold that will be turned down
// after the first surprising email. The numbers below were measured against
// the live fleet — do not re-derive them from intuition.
const (
	// MinCliffPostings is how many openings the LAST NON-EMPTY fetch must
	// have had before emptiness is allowed to mean anything.
	//
	// Measured across 128 live boards: p05=1 p10=2 p25=8 med=26 p75=102
	// p95=429 max=2547. Roughly 30% of boards sit below 10 today, so this
	// threshold openly declines to monitor them — the monthly digest names
	// them as unmonitored rather than pretending the coverage exists.
	//
	// The comparison is against the LAST non-empty fetch, never an all-time
	// peak, because a company tapering 12 -> 7 -> 3 -> 1 -> 0 has only 1-3
	// postings at the cliff. Against a peak it would fire; against its own
	// last state it correctly cannot.
	MinCliffPostings = 10

	// MinNonEmptyDays is how many DISTINCT UTC days the board must have been
	// seen non-empty on. Two days of proof is what structurally silences the
	// five boards that have been legitimately empty since 2026-07-16 (Bitly,
	// Eyeo, Olark, Shogun, ClearGlass): they have never been observed with a
	// posting, so no amount of emptiness can ever accuse them. No tuning, no
	// allowlist to maintain.
	MinNonEmptyDays = 2

	// ZeroFor is how long emptiness must persist. 72h rather than 24h
	// because the dominant benign explanation is a hiring freeze or ATS
	// maintenance over a weekend, and 48h does not span Friday evening to
	// Monday morning. The failure being detected is permanent, so waiting an
	// extra two days costs nothing and removes the whole weekend class of
	// false positives.
	ZeroFor = 72 * time.Hour

	// MinZeroRuns is the observation floor that must hold TOGETHER with
	// ZeroFor, never instead of it: elapsed time alone would fire off two
	// unlucky samples, and a run count alone would fire within an hour of a
	// dense catch-up burst.
	//
	// 12, not 39: GitHub Actions delivers only about 13 of 48 scheduled runs
	// per day on a busy repository, so a high run floor becomes binding
	// exactly when cadence is already starved — the alarm would go quiet
	// precisely when the watcher is least healthy.
	MinZeroRuns = 12

	// ErrFor and MinErrRuns are the mirror image for a board that keeps
	// failing to answer at all. Same reasoning, same numbers: three days and
	// a dozen observations separate a real outage from a flaky afternoon.
	ErrFor      = 72 * time.Hour
	MinErrRuns  = 12
	MaxErrBytes = 200

	// ReAlertAfter keeps a flapping board from mailing on every swing. The
	// cooldown starts when a board RECOVERS, so an adapter that dies, comes
	// back and dies again inside two weeks is reported once; the standing
	// condition still appears in every monthly digest in the meantime.
	ReAlertAfter = 14 * 24 * time.Hour

	// RecentCounts and NonzeroCounts bound the two rolling windows. Recent
	// exists so a human opening the state file can see the shape of a drop;
	// Nonzero feeds Typical. Both are capped because state is pushed in full
	// on every run — an unbounded history would be paid for forever.
	RecentCounts  = 48
	NonzeroCounts = 16

	// NeverFetches and NeverAge define "never proven alive" for the digest:
	// a board watched for a week across 60 successful fetches that has never
	// once returned a posting is far more likely to be a typo in the catalog
	// than a company that simply is not hiring. It is a digest line, never
	// an alert, because a genuinely quiet company looks identical.
	NeverFetches = 60
	NeverAge     = 7 * 24 * time.Hour

	// AllFailRuns is how many consecutive cycles must fail completely before
	// mailing. Two, not one, because a single cycle can lose the network;
	// two consecutive failures mean the watcher itself is not working, which
	// today shows up only as a red check nobody looks at.
	AllFailRuns = 2

	// DigestEvery is the heartbeat interval. Monthly makes SILENCE
	// meaningful — twelve mails a year get read, whereas weekly (52) becomes
	// filter fodder within two months, and a heartbeat nobody reads proves
	// nothing.
	DigestEvery = 30 * 24 * time.Hour

	// BelowNormalHalving is the digest's "well below normal" test: fewer than
	// Typical/2 openings. It is stated as a divisor rather than a ratio so no
	// float rounding is involved. This NEVER alerts and never will — a board
	// halving is what a hiring freeze and a layoff look like, and both are far
	// more common than a broken adapter. Being wrong here costs one line of
	// text in a monthly mail; being wrong in an alert costs the alert's
	// credibility.
	BelowNormalHalving = 2
)

// Key returns the reserved health record key for a source.
func Key(s source.Source) string { return KeyPrefix + source.Identity(s) }

// SrcType extracts the adapter type from a board identity ("greenhouse" from
// "greenhouse/us/hubspot"). Reports group by it because a single vendor
// change explains many boards at once — 193 of 260 boards sit behind five
// vendors, so "47 Greenhouse boards" is one incident, not 47.
func SrcType(identity string) string {
	if i := strings.IndexByte(identity, '/'); i >= 0 {
		return identity[:i]
	}
	return identity
}

// Observation is one board's outcome from one cycle.
type Observation struct {
	Company string
	SrcType string

	// Count is how many postings the fetch returned. It is ignored when Err
	// is set: a partial fetch (the runner's "returned N jobs but also
	// failed" case) is not trustworthy evidence of board size, since the
	// missing part may be the whole interesting part.
	Count int
	Err   error
	At    time.Time
}

// Observe folds one cycle's outcome into a board's health and returns the
// updated value. It is a pure function so the runner can decide separately
// whether to persist the result — dry runs must write nothing.
//
// The two streaks are deliberately symmetric, and the rule for both is that
// only a NON-EMPTY successful fetch is proof the adapter still works:
//
//   - Errors are neutral for the zero streak. A network outage produces
//     errors, not zeros, so a fleet-wide "every board went quiet" alarm is
//     structurally unreachable rather than merely unlikely.
//   - An empty success is neutral for the error streak, for the same reason
//     in reverse: emptiness is the exact symptom under investigation, so
//     treating it as evidence of recovery would let a board alternating
//     errors and empty pages look healthy forever.
func Observe(h store.Health, o Observation) store.Health {
	if o.Company != "" {
		h.Company = o.Company
	}
	if o.SrcType != "" {
		h.SrcType = o.SrcType
	}
	// FirstFetch counts attempts, not successes, so a board that has only
	// ever failed still has an age the digest can report.
	if h.FirstFetch.IsZero() {
		h.FirstFetch = o.At
	}

	if o.Err != nil {
		h.ErrRuns++
		if h.ErrSince.IsZero() {
			h.ErrSince = o.At
		}
		h.LastErr = clip(o.Err.Error(), MaxErrBytes)
		return h
	}

	h.Fetches++
	h.LastOK = o.At
	h.Recent = window(h.Recent, o.Count, RecentCounts)

	if o.Count == 0 {
		h.ZeroRuns++
		if h.ZeroSince.IsZero() {
			h.ZeroSince = o.At
		}
		return h
	}

	// A distinct UTC day is detected by comparing against the PREVIOUS
	// non-empty fetch, which saves storing a separate "last counted day":
	// LastNonEmpty still holds it at this point.
	if h.LastNonEmpty.IsZero() || !sameUTCDay(h.LastNonEmpty, o.At) {
		h.NonEmptyDays++
	}
	h.LastNonEmpty = o.At
	h.LastNonEmptyN = o.Count
	h.Nonzero = window(h.Nonzero, o.Count, NonzeroCounts)
	h.Typical = median(h.Nonzero)

	h.ZeroRuns, h.ZeroSince = 0, time.Time{}
	h.ErrRuns, h.ErrSince, h.LastErr = 0, time.Time{}, ""
	return h
}

// Classify applies the cliff test and folds the alarm state machine forward.
// It returns the updated health and the kind that should be REPORTED right
// now — "" meaning stay silent, which is the answer almost every time.
//
// The returned kind is not the same thing as h.Kind: h.Kind is the standing
// condition (what the digest lists), while the return value fires only on the
// transition into a condition that is not still in cooldown. Classify never
// stamps SentAt; the caller does that after the notifier returns nil, so a
// failed send is retried on the next cycle instead of being lost.
func Classify(h store.Health, now time.Time) (store.Health, string) {
	cond := condition(h, now)

	if cond == "" {
		if h.Kind != "" {
			// Recovery. The cooldown starts here rather than at the alert,
			// so a board that dies again two days later is not a second
			// email — it is the same incident still resolving.
			h.Kind, h.RaisedAt, h.SentAt = "", time.Time{}, time.Time{}
			h.EligibleAt = now.Add(ReAlertAfter)
		}
		return h, ""
	}

	// A different condition is a different diagnosis and starts a fresh
	// episode, so a board that spent three days erroring and then three days
	// answering empty pages reports twice. That is deliberate: the second
	// report says something the first one could not.
	if h.Kind != cond {
		h.Kind = cond
		h.RaisedAt = now
		h.SentAt = time.Time{}
	}
	if !h.SentAt.IsZero() {
		return h, "" // already reported this episode
	}
	if now.Before(h.EligibleAt) {
		return h, "" // still inside the post-recovery cooldown
	}
	return h, cond
}

// condition is the cliff test: the only rule in this package that can lead to
// an unsolicited email.
func condition(h store.Health, now time.Time) string {
	// Erroring outranks dead when both hold — as it does for a board that
	// alternates errors and empty pages, since neither streak resets the
	// other. An error message names the actual failure ("404 board not
	// found"), which is strictly more useful to act on than "went quiet".
	if h.ErrRuns >= MinErrRuns && elapsed(h.ErrSince, now) >= ErrFor {
		return Erroring
	}
	// Stillborn needs no waiting period because it is not a change in the
	// board's behaviour that has to be told apart from a weekend — it is a
	// statement about our whole relationship with the board: we adopted it and
	// it has never had a posting. Baselined is what keeps this from firing on
	// established boards, and because the condition persists until the board
	// actually produces a posting, an undelivered report is simply raised
	// again next cycle rather than needing its own retry latch.
	if h.Baselined && h.Fetches > 0 && h.LastNonEmpty.IsZero() {
		return Stillborn
	}
	if h.LastNonEmptyN >= MinCliffPostings &&
		h.NonEmptyDays >= MinNonEmptyDays &&
		h.ZeroRuns >= MinZeroRuns &&
		elapsed(h.ZeroSince, now) >= ZeroFor {
		return Dead
	}
	return ""
}

// The predicates below feed the monthly digest ONLY. They are kept here, next
// to the alarm rule, so that the difference between "this is worth an email"
// and "this is worth a line in a monthly summary" is visible in one file
// rather than scattered through the runner.

// LastCount returns the most recent SUCCESSFUL fetch's posting count, which is
// zero both for a board that answered empty and for one that has only ever
// failed. The digest says which by also printing the standing condition.
func LastCount(h store.Health) int {
	if len(h.Recent) == 0 {
		return 0
	}
	return h.Recent[len(h.Recent)-1]
}

// BelowNormal reports a board that is answering but with far fewer postings
// than usual. Digest only — see BelowNormalHalving.
func BelowNormal(h store.Health) bool {
	return h.Typical >= MinCliffPostings && LastCount(h)*BelowNormalHalving < h.Typical
}

// NeverProvenAlive reports a board that has answered many times over many days
// and never once returned a posting. Unlike Stillborn this makes no claim
// about how the board was adopted, so it cannot be trusted enough to mail: a
// company that genuinely is not hiring looks exactly the same. Naming it in
// the digest is the honest middle: the user can check it in ten seconds, and
// we never cried wolf.
func NeverProvenAlive(h store.Health, now time.Time) bool {
	return h.LastNonEmpty.IsZero() && h.Fetches >= NeverFetches &&
		!h.FirstFetch.IsZero() && now.Sub(h.FirstFetch) >= NeverAge
}

// Unmonitored reports a board the cliff test declines to watch, because its
// last non-empty fetch was too small for emptiness to mean anything. This is
// roughly 30% of the fleet. The digest names them all, deliberately: a
// coverage gap the user cannot see is a coverage gap they will assume does not
// exist.
func Unmonitored(h store.Health) bool { return h.LastNonEmptyN < MinCliffPostings }

// DigestDue reports whether the heartbeat is owed. The first one goes out
// immediately — it doubles as install confirmation, and it is the only way to
// learn on day one that reports can be delivered at all, rather than a month
// later when the first real incident silently fails to send.
func DigestDue(r store.Run, now time.Time) bool {
	return r.DigestSentAt.IsZero() || now.Sub(r.DigestSentAt) >= DigestEvery
}

// elapsed reports how long a streak has run. A zero start means no streak, so
// it must read as no time at all rather than as time since year 1.
func elapsed(since, now time.Time) time.Duration {
	if since.IsZero() {
		return 0
	}
	return now.Sub(since)
}

func sameUTCDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// window appends v and keeps at most limit entries, oldest first. It always
// allocates: Observe receives its store.Health by value, but a slice header
// copy still shares the backing array, and appending in place would mutate
// the caller's record behind its back — which matters because a dry run must
// leave the loaded state untouched.
func window(prev []int, v, limit int) []int {
	start := 0
	if len(prev) >= limit {
		start = len(prev) - limit + 1
	}
	out := make([]int, 0, limit)
	out = append(out, prev[start:]...)
	return append(out, v)
}

// median returns the middle of counts, averaging the two middle values when
// there is an even number. Typical is cosmetic — it appears in the digest so
// a human can see "usually about 26 openings" — so the tie rule is not
// load-bearing, and by design no alarm ever reads it.
func median(counts []int) int {
	if len(counts) == 0 {
		return 0
	}
	sorted := append([]int(nil), counts...)
	sort.Ints(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// clip bounds remote-controlled error text before it reaches state, and makes
// it safe to store. Fetch errors routinely quote response bodies, so they can
// carry arbitrary bytes; the state-branch validator rejects a push whose JSON
// is not valid UTF-8, which would stop the watcher entirely over one hostile
// endpoint. So invalid bytes are dropped and the cut lands on a rune
// boundary. This is a storage bound only — anything RENDERING LastErr must
// still pass it through notify.OneLine.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
