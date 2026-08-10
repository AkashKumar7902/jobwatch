package run

// Board-health bookkeeping and the operational reports it produces.
//
// The problem: about 41 of the ~50 ATS adapters answer a renamed slug, token
// or tenant with HTTP 200 and an empty list, which is byte-identical to a
// company with genuinely no openings — and RunOnce only fails when EVERY board
// fails. So 262 of 263 boards can be dead while CI stays green. The user reads
// email; a red check and a step summary never reach them.
//
// The policy (what counts as dead, how long emptiness must persist, what only
// belongs in a monthly summary) lives in internal/health, which is a pure fold
// with no clock and no I/O. This file is the wiring: where in a cycle each
// board is observed, which records are written, and how a report is delivered
// at-least-once. Keeping the two apart is what lets the thresholds be tested
// in microseconds instead of days.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"jobwatch/internal/health"
	"jobwatch/internal/notify"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

// maxReportBoards bounds the per-board list in an INCIDENT report. 196 of 263
// boards sit behind five vendors, so one vendor change can trip a hundred at
// once; past a couple of dozen names the adapter breakdown above the list
// already says everything actionable, and an unbounded list turns the one
// email that matters into something that gets scrolled past. The monthly
// digest is deliberately NOT capped — completeness is its whole job.
const maxReportBoards = 24

// funnel is one cycle's evaluated -> matched -> deferred counts. Accumulated
// on the run record between digests, it is the only signal that can see a
// failure BELOW the fetch layer: every board answering normally while the
// matcher defers all of it looks perfectly healthy from up here.
type funnel struct{ evaluated, matched, deferred int }

// boardTrip is one board whose condition should be reported in this cycle.
type boardTrip struct {
	identity string
	kind     string
	health   store.Health
}

// reportCommit pairs a built report with the state change that records it as
// delivered. The commit runs only after Report returns nil, so a failed send
// leaves the condition standing and the next cycle raises it again — the same
// at-least-once discipline the match path uses.
type reportCommit struct {
	report notify.Report
	commit func()
}

// reporterChannel is a notifier that opted into the optional Reporter
// interface, kept next to its name for logging.
type reporterChannel struct {
	name string
	rep  notify.Reporter
}

// clock is the health subsystem's only source of time. It exists so the 72-hour
// and 30-day rules can be exercised in milliseconds; the runner's other
// time.Now() calls (record timestamps, checkpoint intervals) are deliberately
// left alone, because overriding those would let a test's compressed clock
// change seeding and checkpointing behaviour that has nothing to do with health.
func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// observe folds one board's fetch outcome into its reserved health record.
//
// It must run for boards that HARD-FAILED too, which is why the caller places
// it above the failure `continue`: a board that cannot be reached at all is
// precisely the failure worth hearing about, and it is the one the old code
// dropped on the floor unless every other board failed with it.
//
// The record lives under health.KeyPrefix, never under the source-marker
// namespace. The mere existence of a "__jobwatch_source__/" record means "this
// board is already baselined", so writing health there would suppress a
// legitimate seeding pass — or, removed later, mail the user a company's
// entire back catalogue.
func (r *Runner) observe(src source.Source, count int, err error, at time.Time) {
	id := health.Key(src)
	rec, ok := r.Store.Get(id)
	if !ok {
		rec = store.Record{FirstSeen: at}
	}
	// Health rides on an ordinary record and gets no exemption from the state
	// branch validator: a real title and a non-zero first_seen, or the push is
	// rejected and the watcher stops remembering which jobs it already sent.
	rec.Title = "board health: " + src.Company()
	var h store.Health
	if rec.Health != nil {
		h = *rec.Health
	}
	h = health.Observe(h, health.Observation{
		Company: src.Company(),
		SrcType: health.SrcType(source.Identity(src)),
		Count:   count,
		Err:     err,
		At:      at,
	})
	rec.Health = &h
	r.Store.Add(id, rec)
}

// markBaselined records that WE adopted this board — that this process walked
// it end to end and wrote its source marker. It is the fact that makes the
// stillborn check safe: without it, the first cycle after this feature ships
// would accuse every established board that happened to be empty that day.
func (r *Runner) markBaselined(src source.Source) {
	id := health.Key(src)
	rec, ok := r.Store.Get(id)
	if !ok || rec.Health == nil {
		return // observe() runs first for every board, so this cannot happen
	}
	h := *rec.Health
	h.Baselined = true
	rec.Health = &h
	r.Store.Add(id, rec)
}

func (r *Runner) runRecord() store.Run {
	rec, ok := r.Store.Get(health.RunKey)
	if !ok || rec.Run == nil {
		return store.Run{}
	}
	return *rec.Run
}

func (r *Runner) saveRunRecord(v store.Run, at time.Time) {
	rec, ok := r.Store.Get(health.RunKey)
	if !ok {
		rec = store.Record{FirstSeen: at}
	}
	rec.Title = "jobwatch run"
	rec.Run = &v
	r.Store.Add(health.RunKey, rec)
}

// stampRun applies a delivery stamp to the run record. It re-reads rather than
// closing over a copy, because several commits can run in one cycle.
func (r *Runner) stampRun(at time.Time, f func(*store.Run)) {
	v := r.runRecord()
	f(&v)
	r.saveRunRecord(v, at)
}

func (r *Runner) stampBoardSent(identity string, at time.Time) {
	id := health.KeyPrefix + identity
	rec, ok := r.Store.Get(id)
	if !ok || rec.Health == nil {
		return
	}
	h := *rec.Health
	h.SentAt = at
	rec.Health = &h
	r.Store.Add(id, rec)
}

// healthPass folds every watched board forward, updates the fleet-wide run
// record and returns the reports this cycle owes. It writes to the store but
// never delivers: the caller saves first, so a crash between deciding and
// sending re-decides rather than losing the observation.
func (r *Runner) healthPass(now time.Time, allFailed bool, f funnel) []reportCommit {
	trips := r.classifyBoards(now)

	run := r.runRecord()
	run.LastRunAt = now
	if allFailed {
		run.AllFailRuns++
	} else {
		// One board answering proves the watcher, the network and the runner
		// are all working, so the outage is over even if most boards are still
		// failing individually — those have their own per-board condition.
		run.AllFailRuns, run.AllFailSentAt = 0, time.Time{}
	}
	run.EvaluatedSinceDigest += f.evaluated
	run.MatchedSinceDigest += f.matched
	run.DeferredSinceDigest += f.deferred

	var reports []reportCommit
	if run.AllFailRuns >= health.AllFailRuns && run.AllFailSentAt.IsZero() {
		reports = append(reports, reportCommit{
			report: allFailReport(len(r.Sources), run),
			commit: func() { r.stampRun(now, func(v *store.Run) { v.AllFailSentAt = now }) },
		})
	}
	if len(trips) > 0 {
		// One email per cycle no matter how many boards tripped. A vendor-wide
		// change trips its whole cohort in the same run, and 47 separate mails
		// about one incident is indistinguishable from spam — which is how a
		// monitoring channel dies.
		reports = append(reports, reportCommit{
			report: tripReport(trips, now),
			commit: func() {
				for _, t := range trips {
					r.stampBoardSent(t.identity, now)
				}
			},
		})
	}
	if health.DigestDue(run, now) {
		digest := r.digest(now, run)
		reports = append(reports, reportCommit{
			report: digest,
			commit: func() {
				r.stampRun(now, func(v *store.Run) {
					v.DigestSentAt = now
					v.EvaluatedSinceDigest, v.MatchedSinceDigest, v.DeferredSinceDigest = 0, 0, 0
				})
			},
		})
	}
	r.saveRunRecord(run, now)
	return reports
}

// classifyBoards folds each CONFIGURED board's alarm state forward. Boards no
// longer in the catalog keep their records but are never classified: retiring
// a company should not mail about it.
func (r *Runner) classifyBoards(now time.Time) []boardTrip {
	var trips []boardTrip
	for _, src := range r.Sources {
		id := health.Key(src)
		rec, ok := r.Store.Get(id)
		if !ok || rec.Health == nil {
			continue
		}
		h, kind := health.Classify(*rec.Health, now)
		rec.Health = &h
		r.Store.Add(id, rec)
		if kind != "" {
			trips = append(trips, boardTrip{identity: source.Identity(src), kind: kind, health: h})
		}
	}
	sort.Slice(trips, func(i, j int) bool {
		if trips[i].kind != trips[j].kind {
			return trips[i].kind < trips[j].kind
		}
		if trips[i].health.Company != trips[j].health.Company {
			return trips[i].health.Company < trips[j].health.Company
		}
		return trips[i].identity < trips[j].identity
	})
	return trips
}

// reporters lists the notifiers that opted into notify.Reporter. Channels that
// did not are skipped silently here — main.go warns at startup when NONE did,
// which is the only case where the gap is dangerous.
func (r *Runner) reporters() []reporterChannel {
	var out []reporterChannel
	for _, n := range r.Notifiers {
		if rep, ok := n.(notify.Reporter); ok {
			out = append(out, reporterChannel{name: n.Name(), rep: rep})
		}
	}
	return out
}

// deliverReports sends each report to every reporting channel and stamps it
// delivered only once all of them accepted it. It returns how many were
// committed so the caller knows whether the stamps need saving.
func (r *Runner) deliverReports(ctx context.Context, reports []reportCommit) (int, error) {
	if len(reports) == 0 {
		return 0, nil
	}
	targets := r.reporters()
	if len(targets) == 0 {
		// Logged every cycle, not once: this is a silent-failure mode, and a
		// warning printed only at startup is one nobody will be looking at.
		r.Log.Printf("WARN scope=run index=0 step=report code=no_reporter count=%d", len(reports))
		return 0, nil
	}
	committed := 0
	for _, pending := range reports {
		for _, target := range targets {
			start := time.Now()
			if err := target.rep.Report(ctx, pending.report); err != nil {
				return committed, fmt.Errorf("reporter %s failed (report stays pending, retried next run): %w", target.name, err)
			}
			r.Log.Printf("REPORT status=accepted channel=%s duration_ms=%d", quoteLogField(target.name), millis(time.Since(start)))
		}
		pending.commit()
		committed++
	}
	return committed, nil
}

// allFailReport is the fast path for a total outage. Two consecutive cycles,
// not one, because a single cycle can simply lose the network; two in a row
// mean the watcher is not working — which until now showed up only as a red CI
// check nobody looks at.
func allFailReport(boards int, run store.Run) notify.Report {
	return notify.Report{
		Subject: fmt.Sprintf("jobwatch is not reaching any job board (%d cycles)", run.AllFailRuns),
		Lines: []string{
			fmt.Sprintf("Every one of the %d configured boards has failed to fetch for %d consecutive cycles.", boards, run.AllFailRuns),
			"",
			"That is not a company-side change — it is the watcher, its network, or its",
			"credentials. Until it clears, NO new posting is being seen at all, and the",
			"absence of job mail means nothing.",
			"",
			"This is reported once per outage. The next report comes after it recovers",
			"and something breaks again.",
		},
	}
}

// tripReport renders every board that tripped this cycle as ONE email.
func tripReport(trips []boardTrip, now time.Time) notify.Report {
	lines := []string{
		"These boards are no longer telling us anything useful. Each one is a company",
		"whose new postings jobwatch would silently stop showing you.",
		"",
	}
	if len(trips) >= 5 {
		lines = append(lines, "By adapter: "+adapterBreakdown(trips), "")
	}
	shown := trips
	if len(shown) > maxReportBoards {
		shown = shown[:maxReportBoards]
	}
	for _, t := range shown {
		lines = append(lines, t.kind+" — "+boardLabel(t.health, t.identity))
		lines = append(lines, tripDetail(t, now)...)
		lines = append(lines, "")
	}
	if len(trips) > len(shown) {
		lines = append(lines, fmt.Sprintf("... and %d more, all listed in the next monthly report.", len(trips)-len(shown)), "")
	}
	lines = append(lines,
		"Most likely cause: the board's slug, token or tenant was renamed. An ATS",
		"answers a renamed board with a perfectly valid, perfectly empty 200.",
		"",
		"Each board is reported once. If it comes back and breaks again, the next",
		"report waits 14 days so a flapping adapter cannot take over your inbox.",
	)
	return notify.Report{Subject: tripSubject(trips), Lines: lines}
}

func tripDetail(t boardTrip, now time.Time) []string {
	h := t.health
	switch t.kind {
	case health.Erroring:
		return []string{
			fmt.Sprintf("  no successful fetch for %s across %d attempts", span(now.Sub(h.ErrSince)), h.ErrRuns),
			"  last error: " + h.LastErr,
		}
	case health.Stillborn:
		return []string{
			"  baselined just now, and the board returned no postings at all",
			"  nothing was recorded for it, so nothing about it will ever alert",
		}
	default:
		detail := fmt.Sprintf("  last had %d openings on %s", h.LastNonEmptyN, day(h.LastNonEmpty))
		if h.Typical > 0 {
			detail += fmt.Sprintf(" (usually about %d)", h.Typical)
		}
		return []string{
			detail,
			fmt.Sprintf("  empty for %s across %d successful fetches", span(now.Sub(h.ZeroSince)), h.ZeroRuns),
		}
	}
}

// tripSubject must never reuse the match headline: a report titled like a job
// alert is a report that gets opened for the wrong reason and closed unread.
func tripSubject(trips []boardTrip) string {
	kinds := map[string]bool{}
	for _, t := range trips {
		kinds[t.kind] = true
	}
	phrase := "need attention"
	if len(kinds) == 1 {
		switch trips[0].kind {
		case health.Erroring:
			phrase = "stopped responding"
		case health.Stillborn:
			phrase = "returned nothing when added"
		default:
			phrase = "went quiet"
		}
	}
	subject := fmt.Sprintf("%d board%s %s", len(trips), plural(len(trips)), phrase)
	var names []string
	for _, t := range trips {
		if len(names) == 3 {
			break
		}
		names = append(names, companyOf(t.health, t.identity))
	}
	subject += ": " + strings.Join(names, ", ")
	if len(trips) > len(names) {
		subject += fmt.Sprintf(" and %d more", len(trips)-len(names))
	}
	return subject
}

func adapterBreakdown(trips []boardTrip) string {
	counts := map[string]int{}
	for _, t := range trips {
		counts[health.SrcType(t.identity)]++
	}
	types := make([]string, 0, len(counts))
	for name := range counts {
		types = append(types, name)
	}
	sort.Slice(types, func(i, j int) bool {
		if counts[types[i]] != counts[types[j]] {
			return counts[types[i]] > counts[types[j]]
		}
		return types[i] < types[j]
	})
	parts := make([]string, 0, len(types))
	for _, name := range types {
		parts = append(parts, fmt.Sprintf("%s %d", name, counts[name]))
	}
	return strings.Join(parts, ", ")
}

// digestBoard is one board's line in the monthly report.
type digestBoard struct {
	identity string
	health   store.Health
}

// digest is the heartbeat. Its real job is to make SILENCE meaningful: without
// a mail that arrives on a known schedule, "no alerts" and "the watcher died
// in March" look identical from the inbox. Everything it contains is chosen to
// be true-but-not-alarming — including the coverage gap, which is stated
// outright rather than left for the user to discover during an incident.
func (r *Runner) digest(now time.Time, run store.Run) notify.Report {
	configured := make(map[string]bool, len(r.Sources))
	for _, src := range r.Sources {
		configured[source.Identity(src)] = true
	}

	var (
		watched, retired                      int
		conditions, below, never, unmonitored []digestBoard
	)
	r.Store.Range(func(id string, rec store.Record) bool {
		if rec.Health == nil || !strings.HasPrefix(id, health.KeyPrefix) {
			return true
		}
		b := digestBoard{identity: strings.TrimPrefix(id, health.KeyPrefix), health: *rec.Health}
		if !configured[b.identity] {
			retired++
			return true
		}
		watched++
		// One bucket per board, most serious first, so nothing is counted or
		// listed twice.
		switch {
		case b.health.Kind != "":
			conditions = append(conditions, b)
		case health.NeverProvenAlive(b.health, now):
			never = append(never, b)
		case health.BelowNormal(b.health):
			below = append(below, b)
		case health.Unmonitored(b.health):
			unmonitored = append(unmonitored, b)
		}
		return true
	})
	for _, list := range [][]digestBoard{conditions, below, never, unmonitored} {
		sortDigest(list)
	}

	lines := []string{
		"jobwatch is running. This is the monthly heartbeat — if it ever stops",
		"arriving, the watcher itself has stopped, and quiet weeks stop meaning",
		"\"nothing matched\".",
		"",
		fmt.Sprintf("Boards watched: %d", watched),
		fmt.Sprintf("  with no standing problem: %d", watched-len(conditions)),
		fmt.Sprintf("  needing attention: %d", len(conditions)),
	}
	if retired > 0 {
		lines = append(lines, fmt.Sprintf("  dropped from the catalog (kept for history): %d", retired))
	}
	lines = append(lines,
		"",
		// The funnel is the only line here that can see past the fetch layer.
		// Every board answering while the matcher defers all of it is invisible
		// from every other number in this report.
		fmt.Sprintf("Postings since the last report: %d evaluated, %d matched, %d deferred",
			run.EvaluatedSinceDigest, run.MatchedSinceDigest, run.DeferredSinceDigest),
	)

	lines = append(lines, digestSection("Needing attention", nil, conditions, func(b digestBoard) string {
		return b.health.Kind + " — " + boardLabel(b.health, b.identity) + ": " + digestCondition(b.health, now)
	})...)
	lines = append(lines, digestSection("Well below normal",
		[]string{"(not an alarm, and never will be: a hiring freeze looks exactly like this)"},
		below, func(b digestBoard) string {
			return fmt.Sprintf("%s: %d opening%s now, usually about %d",
				boardLabel(b.health, b.identity), health.LastCount(b.health),
				plural(health.LastCount(b.health)), b.health.Typical)
		})...)
	lines = append(lines, digestSection("Never proven alive",
		[]string{fmt.Sprintf("(answering for over %d days across %d+ fetches, never once a posting)",
			int(health.NeverAge.Hours()/24), health.NeverFetches)},
		never, func(b digestBoard) string {
			return boardLabel(b.health, b.identity)
		})...)
	lines = append(lines, digestSection("NOT MONITORED",
		[]string{
			fmt.Sprintf("(fewer than %d openings on the last non-empty fetch, so emptiness cannot be", health.MinCliffPostings),
			"told from a normal wind-down — these boards can break without this report",
			"noticing, and the number after each one is why)",
		},
		unmonitored, func(b digestBoard) string {
			return fmt.Sprintf("%s: %d", boardLabel(b.health, b.identity), b.health.LastNonEmptyN)
		})...)

	problems := "all clear"
	if len(conditions) > 0 {
		problems = fmt.Sprintf("%d needing attention", len(conditions))
	}
	return notify.Report{
		Subject: fmt.Sprintf("monthly board report: %d watched, %s", watched, problems),
		Lines:   lines,
	}
}

// digestSection renders a titled list, or nothing at all when it is empty —
// a report full of empty headings is a report that stops being read. Notes are
// pre-wrapped because nothing downstream wraps: the body goes out as plain
// text, and a 90-column heading in a phone mail client is a heading nobody
// finishes reading.
func digestSection(title string, note []string, boards []digestBoard, line func(digestBoard) string) []string {
	if len(boards) == 0 {
		return nil
	}
	out := []string{"", title + ":"}
	out = append(out, note...)
	for _, b := range boards {
		out = append(out, "  "+line(b))
	}
	return out
}

func digestCondition(h store.Health, now time.Time) string {
	switch h.Kind {
	case health.Erroring:
		return fmt.Sprintf("failing for %s; last error: %s", span(now.Sub(h.ErrSince)), h.LastErr)
	case health.Stillborn:
		return "has never returned a posting since it was added"
	default:
		return fmt.Sprintf("last had %d openings on %s, empty since", h.LastNonEmptyN, day(h.LastNonEmpty)) +
			" " + day(h.ZeroSince)
	}
}

func sortDigest(boards []digestBoard) {
	sort.Slice(boards, func(i, j int) bool {
		if boards[i].health.Company != boards[j].health.Company {
			return boards[i].health.Company < boards[j].health.Company
		}
		return boards[i].identity < boards[j].identity
	})
}

// boardLabel names a board by company AND identity: the company is what the
// user recognizes, the identity is what they have to edit in config.yaml.
func boardLabel(h store.Health, identity string) string {
	return companyOf(h, identity) + " (" + identity + ")"
}

func companyOf(h store.Health, identity string) string {
	if h.Company != "" {
		return h.Company
	}
	return identity
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func day(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02")
}

// span renders a duration the way a human reads an outage. Sub-hour precision
// is noise for thresholds measured in days.
func span(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	return fmt.Sprintf("%dh", hours)
}
