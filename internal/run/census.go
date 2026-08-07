package run

// The board census: noticing that a job board MOVED rather than that it was
// removed, and repairing the state keys that made a move destructive.
//
// THE FAILURE THIS EXISTS FOR
//
// An ATS moves a tenant between shards. Workday does it unilaterally
// (wd3 -> wd5 -> wd103), and the only thing the user can do is edit `host` in
// config.yaml. Until this change, that one edit changed the board's identity,
// its state prefix and every one of its job IDs at once, so all three "have I
// seen this board?" guards missed together: no source marker, no posting under
// the prefix, no seen job ID. The board was silently re-baselined — zero emails
// from it for postings it had already collected — and 100% of its history was
// orphaned. 57 Workday boards across 8 shards are ~47% of all state.
//
// internal/source/coords.go stops NEW keys from embedding transport, and
// internal/source/staterekey.go repairs the keys already on disk. Those two
// make a host edit harmless for the seven adapters whose params are provably
// transport. This file covers what they cannot: the boards where the host IS
// the tenant (icims, successfactors, ...), a vendor coordinate nobody predicted,
// or simply a user who edited something else. For those, the move is still
// destructive — but it is DETECTABLE, and the user can repair it in one line.
//
// THE SIGNAL
//
// The census compares two sets, both already in the state file:
//
//	live   = every configured board, with its identity and state prefix
//	stored = every "__jobwatch_source__/" marker, each carrying the prefix it
//	         recorded when it was adopted
//
// An ORPHAN is a stored board that is no longer configured. A FRESH board is a
// configured board with no marker, no baseline under way, and no records under
// its prefix. Then:
//
//	orphan with no surviving records            -> forget it, silently
//	orphan with records + a fresh board of the
//	  SAME ATS type appeared in the same run    -> ANNOUNCE
//	orphan with records, nothing else           -> keep it, silently
//
// The announcement condition is the only conjunction, and it is deliberately
// not producible by either thing the user actually does. Adding a company
// creates a fresh board and no orphan. Removing one creates an orphan and no
// fresh board. Only an EDIT — one board's coordinates replaced by another's in
// the same commit — produces both at once. There are no counts, no ratios, no
// time windows and nothing to tune; replayed against all 26 config commits in
// this repository's history it would have sent zero false alarms.
//
// "In the same run" is the load-bearing half of that argument, and neither side
// of it holds by itself. Freshness expires on its own — the next cycle gives the
// board a marker — but only once a cycle actually reaches it, so a board that
// fails every fetch is excluded by its seed-in-progress record instead.
// Orphanhood never expires at all, so the first census to find a board gone
// stamps marker.orphaned_at and no later census may pair it with anything.
// Without both, the rule decays into "a board was EVER removed AND one appeared
// now" — which a plain deletion and, months later, a plain addition satisfy
// between them, and with 193 of 260 boards behind five vendors they collide on
// ATS type more often than not.
//
// Everything here runs before any fetch: it is pure state arithmetic, so a
// network outage cannot suppress it and it costs nothing on the 99.9% of runs
// where nothing moved.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"jobwatch/internal/health"
	"jobwatch/internal/notify"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

// maxAnnouncedBoards bounds the per-board detail in one announcement. A
// migration is a handful of boards by nature — a vendor moving an entire
// customer base in one run would be the config author's own edit, and the count
// in the subject line already says that. Twenty-four matches the incident cap
// in report.go for the same reason: past that, the list stops being read.
const maxAnnouncedBoards = 24

// liveBoard is one configured board as the census sees it.
type liveBoard struct {
	identity string
	company  string
	prefix   string
	srcType  string
}

// orphanBoard is one adopted board that the config no longer mentions.
type orphanBoard struct {
	identity string
	company  string

	// described is false for a marker written before markers recorded their
	// state prefix. Such a marker cannot prove that its board owned history
	// and cannot prove that it did not, and its key — an identity string — is
	// not parseable back into a prefix. It is therefore left strictly alone:
	// never swept, never announced. Sweeping it would delete the only record
	// that the board was ever adopted, which is what stops a later re-add from
	// silently re-baselining it; announcing it would mail "0 postings under
	// """ the first time the user adds any board of the same ATS type.
	described   bool
	prefix      string
	movedTo     string
	announcedAt time.Time
	orphanedAt  time.Time
	records     int
}

// announcement pairs an orphan with the fresh board it appears to have become.
type announcement struct {
	orphan    orphanBoard
	successor liveBoard
	// others counts additional fresh boards of the same ATS type in this run.
	// With more than one candidate the pairing is a guess, and saying so is
	// cheaper than being confidently wrong in an email.
	others int
}

// migrateStateKeys applies the frozen transport-in-the-key repair to every
// stored key, and drops the keys no adapter can produce any more.
//
// It runs at the top of every cycle rather than as a one-shot script because
// it is IDEMPOTENT BY CONSTRUCTION: every rule's output lies outside its own
// input language, so the second and every later run move nothing at all (see
// internal/source/staterekey.go, and TestRekeyRulesAreIdempotent which proves
// it). A migration that lives in the runner is also the only kind that repairs
// a state file the user restored from an old branch, or a board whose host was
// edited months before this shipped.
func (r *Runner) migrateStateKeys() (moved, merged, dropped int) {
	moved, merged = r.Store.Rekey(source.RekeyStateKey)

	// Collect before deleting: Range is read-only, and mutating the map mid
	// iteration makes which keys are visited depend on Go's map order.
	var junk []string
	r.Store.Range(func(id string, rec store.Record) bool {
		// A notified junk key would mean the user was emailed about a posting
		// that is only a board base URL. None exists (all 18 are un-notified),
		// and if one ever did, dropping it is the one thing this package must
		// never do — the state-branch gate refuses that push anyway, so leave
		// it in place and let it be inert.
		if source.IsJunkStateKey(id) && !rec.Notified {
			junk = append(junk, id)
		}
		return true
	})
	for _, id := range junk {
		r.Store.Delete(id)
	}
	return moved, merged, len(junk)
}

// census performs the pre-pass. It writes to the store (sweeping dead markers,
// recording a raised announcement) and returns the reports this cycle owes,
// committed only after delivery — the same at-least-once discipline the health
// reports and the match batch use.
func (r *Runner) census(now time.Time) []reportCommit {
	live := make(map[string]liveBoard, len(r.Sources))
	livePrefixes := make([]string, 0, len(r.Sources))
	for _, src := range r.Sources {
		identity := source.Identity(src)
		prefix := source.StatePrefix(src)
		live[identity] = liveBoard{
			identity: identity,
			company:  src.Company(),
			prefix:   prefix,
			srcType:  health.SrcType(identity),
		}
		if prefix != "" {
			livePrefixes = append(livePrefixes, prefix)
		}
	}

	var orphans []orphanBoard
	adopted := make(map[string]bool, len(live))
	r.Store.Range(func(id string, rec store.Record) bool {
		if !strings.HasPrefix(id, sourceMarkerPrefix) {
			return true
		}
		identity := strings.TrimPrefix(id, sourceMarkerPrefix)
		adopted[identity] = true
		if _, isLive := live[identity]; isLive {
			return true
		}
		o := orphanBoard{identity: identity, company: markerCompany(rec, identity)}
		if rec.Marker != nil {
			o.described = true
			o.prefix, o.movedTo = rec.Marker.StatePrefix, rec.Marker.MovedTo
			o.announcedAt, o.orphanedAt = rec.Marker.AnnouncedAt, rec.Marker.OrphanedAt
		}
		orphans = append(orphans, o)
		return true
	})
	if len(orphans) == 0 {
		return nil
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].identity < orphans[j].identity })

	// Fresh boards: configured, never adopted, never even STARTED a baseline,
	// and owning nothing. The zero-record restriction is what keeps this from
	// firing on the very run that ships the identity change — those 83 boards
	// have new identities and no markers, but migrateStateKeys just moved their
	// records under the new prefixes, so they are not fresh and nothing is
	// announced.
	//
	// The seed-in-progress test is what stops freshness from being permanent. A
	// board whose fetch fails on every single run never completes a baseline and
	// so never gets a marker, and having recorded nothing it owns no records
	// either — it would stay an eligible successor forever, and deleting ANY
	// board of its ATS type would then announce a move on its own. That marker
	// is written before the fetch is even attempted, so it is exactly the
	// durable proof that this board is not new to us.
	freshByType := map[string][]liveBoard{}
	for _, b := range live {
		if adopted[b.identity] || r.Store.Seen(sourceSeedInProgressPrefix+b.identity) {
			continue
		}
		if r.Store.CountPostingPrefix(b.prefix) > 0 {
			continue
		}
		freshByType[b.srcType] = append(freshByType[b.srcType], b)
	}
	for _, list := range freshByType {
		sort.Slice(list, func(i, j int) bool { return list[i].identity < list[j].identity })
	}

	var announcements []announcement
	for _, o := range orphans {
		o.records = r.Store.CountPostingPrefix(o.prefix)
		switch {
		case !o.described:
			// See orphanBoard.described. Silent on purpose, including in the
			// log: on the run that ships this change EVERY marker is one of
			// these, and a per-board line would bury the output.
		case o.records == 0:
			// Nothing was lost, so there is nothing to say. This is also the
			// sweep for markers left behind by an identity change: their keys
			// embed an old identity that cannot be parsed back into anything,
			// and once the postings have moved, HasPostingPrefix alone answers
			// "do we know this board?" correctly.
			r.Store.Delete(sourceMarkerPrefix + o.identity)
			r.Log.Printf("census: forgetting board %q — no longer configured and no records under %q", o.identity, o.prefix)
		case overlapsAny(o.prefix, livePrefixes):
			// Its records are not orphaned: a board that is STILL configured
			// owns that namespace. This is what deleting one of two scoped views
			// of a single board looks like (two eightfold `query=` entries, two
			// avature search paths) — same state prefix, different identities —
			// and the surviving view keeps matching on every one of those
			// records.
			//
			// Announcing here would be worse than noise. The mail's whole
			// payload is a previous_state_prefix line, and a line naming a
			// namespace a live board still owns is one main.go refuses to start
			// with: pasting the fix we sent would take the watcher down. The
			// marker is kept rather than swept for the mirror-image reason —
			// the state-branch gate refuses to drop a marker while records
			// still live under its prefix.
			r.Log.Printf("census: board %q is no longer configured; its %d record(s) under %q stay with a configured board that shares the prefix",
				o.identity, o.records, o.prefix)
		case !o.announcedAt.IsZero():
			// Already reported once. The condition is permanent (an orphan
			// stays an orphan), so re-raising it would mail every cycle
			// forever.
		case o.movedTo != "":
			// Raised on an earlier cycle but never delivered. Re-raise it with
			// the successor recorded then: the evidence is gone by now, since
			// that board got its own marker during the run that raised this.
			announcements = append(announcements, announcement{orphan: o, successor: r.successor(live, o.movedTo)})
		default:
			// BOTH halves must be new in this run. Freshness already is — a
			// fresh board has a marker by the end of the cycle that first sees
			// it — but orphanhood is permanent, so the orphan half needs a latch
			// of its own or the conjunction degrades to "a board was EVER
			// removed AND one appeared now", which a plain deletion followed
			// months later by a plain addition of the same ATS type satisfies.
			// 193 of 260 boards sit behind five vendors, so that collision is
			// the common case rather than the exotic one.
			fresh := freshByType[health.SrcType(o.identity)]
			if o.orphanedAt.IsZero() && len(fresh) > 0 {
				announcements = append(announcements, announcement{orphan: o, successor: fresh[0], others: len(fresh) - 1})
				r.markMovedTo(o.identity, fresh[0].identity)
				continue
			}
			if o.orphanedAt.IsZero() {
				// First census to find it gone, and nothing turned up to
				// explain it. Record that we have adjudicated it, so no later
				// unrelated addition can be paired with it.
				r.markOrphaned(o.identity, now)
			}
			r.Log.Printf("census: board %q is no longer configured but still owns %d record(s); keeping them",
				o.identity, o.records)
		}
	}
	if len(announcements) == 0 {
		return nil
	}
	return []reportCommit{{
		report: moveReport(announcements),
		commit: func() {
			for _, a := range announcements {
				r.stampAnnounced(a.orphan.identity, now)
			}
		},
	}}
}

// overlapsAny reports whether prefix shares a namespace with any of them.
// Containment either way counts: a live prefix that CONTAINS the orphan's means
// every record the orphan can see is still a live board's, and one contained BY
// it means absorbing the orphan would drag a live board's history along. Both
// are answered the same way — say nothing — so they need not be told apart.
func overlapsAny(prefix string, live []string) bool {
	if prefix == "" {
		return false
	}
	for _, p := range live {
		if strings.HasPrefix(prefix, p) || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// successor describes the board a pending announcement points at. It may no
// longer be configured by the time delivery succeeds (the user can edit again),
// in which case the identity is all we can honestly print.
func (r *Runner) successor(live map[string]liveBoard, identity string) liveBoard {
	if b, ok := live[identity]; ok {
		return b
	}
	return liveBoard{identity: identity, company: identity}
}

// markMovedTo records a RAISED announcement. It is written before delivery and
// saved with everything else, so a crash or an SMTP failure re-raises the same
// announcement instead of losing it — the successor board will have its own
// marker by then, so the signature itself is no longer recomputable.
func (r *Runner) markMovedTo(identity, successor string) {
	id := sourceMarkerPrefix + identity
	rec, ok := r.Store.Get(id)
	if !ok {
		return
	}
	m := markerOf(rec)
	m.MovedTo = successor
	rec.Marker = &m
	r.Store.Add(id, rec)
}

// markOrphaned records that a census has already adjudicated this orphan and
// found nothing to pair it with. Unlike stampAnnounced it is NOT gated on
// delivery: there is no message to deliver, and the fact being recorded is
// about this run's config, which no later run can reconstruct.
func (r *Runner) markOrphaned(identity string, at time.Time) {
	id := sourceMarkerPrefix + identity
	rec, ok := r.Store.Get(id)
	if !ok {
		return
	}
	m := markerOf(rec)
	m.OrphanedAt = at
	rec.Marker = &m
	r.Store.Add(id, rec)
}

// stampAnnounced records a DELIVERED announcement.
func (r *Runner) stampAnnounced(identity string, at time.Time) {
	id := sourceMarkerPrefix + identity
	rec, ok := r.Store.Get(id)
	if !ok {
		return
	}
	m := markerOf(rec)
	m.AnnouncedAt = at
	rec.Marker = &m
	r.Store.Add(id, rec)
}

func markerOf(rec store.Record) store.Marker {
	if rec.Marker != nil {
		return *rec.Marker
	}
	return store.Marker{}
}

// markerCompany recovers a display name from a marker record. Markers are
// titled "source: <company>"; one written before that convention, or by a
// future change, degrades to the identity rather than to an empty string.
func markerCompany(rec store.Record, identity string) string {
	if name := strings.TrimPrefix(rec.Title, "source: "); name != "" && name != rec.Title {
		return name
	}
	return identity
}

// writeMarker records "we have adopted this board" and, from this change on,
// WHAT we adopted: the prefix its postings live under. Every non-dry cycle
// refreshes it for every configured board, not just the ones being baselined,
// because the prefix has to be on file BEFORE the board disappears — an orphan
// that never recorded one cannot be told from a board that never had history,
// and the census would sweep it silently.
//
// It preserves FirstSeen and the announcement stamps: the marker's age is a
// real fact about when we adopted the board, and rewriting it every run would
// destroy the only record of that.
func (r *Runner) writeMarker(src source.Source) bool {
	identity := source.Identity(src)
	id := sourceMarkerPrefix + identity
	rec, existed := r.Store.Get(id)
	want := store.Marker{StatePrefix: source.StatePrefix(src), PreviousStatePrefix: r.PreviousStatePrefixes[identity]}
	if existed {
		m := markerOf(rec)
		m.StatePrefix, m.PreviousStatePrefix = want.StatePrefix, want.PreviousStatePrefix
		// The board is configured right now, so any adjudication a census made
		// while it was gone is stale: one that leaves, comes back and leaves
		// again gets judged afresh rather than being permanently ineligible.
		m.OrphanedAt = time.Time{}
		want = m
	} else {
		rec.FirstSeen = time.Now()
	}
	title := "source: " + src.Company()
	if existed && rec.Title == title && rec.Marker != nil && *rec.Marker == want {
		return false // nothing to write; 260 identical records per run is pure noise
	}
	rec.Title, rec.Marker = title, &want
	r.Store.Add(id, rec)
	return true
}

// applyPreviousStatePrefixes performs the configured prefix swaps: the escape
// hatch for a move the frozen rules cannot infer, expected to be used about
// twice a decade.
//
// It is idempotent because the old prefix is empty after one run, and it is
// gated on the board already having a MARKER because that marker is what makes
// the swap legal downstream: the state-branch gate authorizes the resulting
// removals from marker.previous_state_prefix alone, without reading config. A
// board with no marker has not been adopted yet, so there is nothing to absorb
// into — it gets baselined this run and swaps on the next one.
func (r *Runner) applyPreviousStatePrefixes() (moved, merged int) {
	for _, src := range r.Sources {
		identity := source.Identity(src)
		from := r.PreviousStatePrefixes[identity]
		to := source.StatePrefix(src)
		if from == "" || to == "" || from == to {
			continue
		}
		rec, ok := r.Store.Get(sourceMarkerPrefix + identity)
		if !ok || rec.Marker == nil || rec.Marker.PreviousStatePrefix != from {
			r.Log.Printf("previous_state_prefix %q for %s: board not adopted yet, deferring the move to the next run", from, identity)
			continue
		}
		n, m := r.Store.Rekey(func(id string) string {
			if strings.HasPrefix(id, "__jobwatch_") || !strings.HasPrefix(id, from) {
				return "" // "" means UNCHANGED here, never "delete" — see store.Rekey
			}
			return to + strings.TrimPrefix(id, from)
		})
		if n > 0 {
			r.Log.Printf("previous_state_prefix: moved %d record(s) from %q to %q for %s (%d merged into existing records)",
				n, from, to, identity, m)
		}
		moved, merged = moved+n, merged+m
	}
	return moved, merged
}

// moveReport renders every orphaned board as ONE email.
//
// It has its own subject on purpose: this is the one message whose whole point
// is that the user must edit a file, and a report that arrives titled like a
// job alert is a report that gets opened for the wrong reason and closed
// unread. It is also sent regardless of whether anything matched this cycle —
// a board that lost its history is exactly a board with no matches to report.
func moveReport(announcements []announcement) notify.Report {
	lines := []string{
		"A job board that jobwatch had been watching is no longer in your config,",
		"and its stored history is now orphaned. In the same run, a board of the same",
		"ATS type appeared with no history at all — which is what an ATS host or shard",
		"move looks like from here.",
		"",
	}
	shown := announcements
	if len(shown) > maxAnnouncedBoards {
		shown = shown[:maxAnnouncedBoards]
	}
	for _, a := range shown {
		lines = append(lines,
			fmt.Sprintf("%s — %s", a.orphan.company, a.orphan.identity),
			fmt.Sprintf("  %d stored posting(s) under %s", a.orphan.records, quoted(a.orphan.prefix)),
			fmt.Sprintf("  new this run: %s (%s), whose postings will be stored under %s",
				a.successor.company, a.successor.identity, quoted(a.successor.prefix)),
		)
		if a.others > 0 {
			lines = append(lines, fmt.Sprintf("  %d other new %s board(s) also appeared this run, so check which one it is",
				a.others, health.SrcType(a.orphan.identity)))
		}
		lines = append(lines,
			fmt.Sprintf("  if those are the same board, add this line to the %q entry in config.yaml:", a.successor.company),
			"    previous_state_prefix: "+quoted(a.orphan.prefix),
			"")
	}
	if len(announcements) > len(shown) {
		lines = append(lines, fmt.Sprintf("... and %d more.", len(announcements)-len(shown)), "")
	}
	lines = append(lines,
		"That line moves the stored history onto the new board's keys on the next run,",
		"including which postings you were already emailed about. It is removed from",
		"the config once the move is done; leaving it in place is harmless.",
		"",
		"Doing nothing is safe but lossy: the new board keeps its empty history, so",
		"postings it already had will never be mailed to you. This message is sent",
		"once per board — it will not repeat.",
	)
	return notify.Report{Subject: moveSubject(announcements), Lines: lines}
}

func moveSubject(announcements []announcement) string {
	var names []string
	for _, a := range announcements {
		if len(names) == 3 {
			break
		}
		names = append(names, a.orphan.company)
	}
	subject := fmt.Sprintf("%d job board%s look%s to have moved: %s",
		len(announcements), plural(len(announcements)), verbEnding(len(announcements)), strings.Join(names, ", "))
	if len(announcements) > len(names) {
		subject += fmt.Sprintf(" and %d more", len(announcements)-len(names))
	}
	return subject + " (history orphaned)"
}

func verbEnding(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}

// quoted renders a prefix the way the config file wants it, so the
// previous_state_prefix line in the mail can be pasted verbatim.
func quoted(s string) string { return `"` + s + `"` }

// prePass is everything that happens before a single HTTP request: repair the
// keys, then take the census. It is deliberately network-free, so an outage
// cannot suppress the one message that tells the user their history moved.
func (r *Runner) prePass() ([]reportCommit, error) {
	moved, merged, dropped := r.migrateStateKeys()
	if moved > 0 || dropped > 0 {
		// Logged once, and only when something actually happened: on every run
		// after the first, all three counts are zero.
		r.Log.Printf("state key migration: moved %d record(s) off vendor transport keys (%d merged into existing records), dropped %d unrecreatable record(s)",
			moved, merged, dropped)
	}
	if r.DryRun {
		// A dry run keeps the migration (its whole job is to show what a real
		// run would decide, and unmigrated keys would make every Workday
		// posting look unseen) but writes nothing else and reports nothing.
		return nil, nil
	}
	if moved > 0 || dropped > 0 {
		if err := r.Store.Save(); err != nil {
			return nil, fmt.Errorf("saving migrated state keys: %w", err)
		}
	}
	return r.census(r.clock()), nil
}
