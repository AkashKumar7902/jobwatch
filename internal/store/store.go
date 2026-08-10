// Package store remembers which postings have already been processed so a
// job is evaluated — and emailed — only once. State is a plain JSON file:
// safe to inspect, and deleting it simply re-processes everything.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Record is what we keep about one posting we've seen. A record with
// Matched=true and Notified=false is a match whose delivery hasn't been
// confirmed yet — the runner retries it on the next cycle.
//
// Health, Run and Marker are pointers with omitempty on purpose. Every posting
// the watcher has ever seen lives in this map (tens of thousands of records,
// all of them pushed to the state branch on every run), and only the few
// hundred reserved bookkeeping keys ever carry them. A pointer that is nil
// marshals to nothing, so existing records re-serialize byte-for-byte — see
// TestRecordRoundTripsUnchanged, which is the migration guarantee. Flat
// fields would not work even with omitempty: encoding/json ignores omitempty
// on a struct like time.Time, so every record would grow a handful of
// "0001-01-01T00:00:00Z" strings (measured: +5.7MB per push).
type Record struct {
	FirstSeen time.Time `json:"first_seen"`
	Title     string    `json:"title"`
	Matched   bool      `json:"matched"`
	Notified  bool      `json:"notified"`
	Health    *Health   `json:"health,omitempty"`
	Run       *Run      `json:"run,omitempty"`
	Marker    *Marker   `json:"marker,omitempty"`
}

// Marker is what we remember about a board we have ADOPTED. It hangs off the
// source-marker key ("__jobwatch_source__/" + identity), never off a posting.
//
// The key alone used to be the whole record: its existence meant "this board
// is baselined, do not seed it again". That is not enough to survive an ATS
// host move. When the vendor moves a tenant between shards the user edits
// `host`, the board's identity changes with it, and the old marker becomes an
// orphan whose ONLY remaining description is an identity string — which is not
// parseable (a real stored marker is literally
// "__jobwatch_source__/eightfold/jobs.ericsson.com/ericsson.com//query=Cradlepoint",
// double slash and all). Recording the state prefix at adoption time is what
// lets a later run ask the one question that matters about an orphan: does it
// still own any history? A prefix with zero records is a marker to sweep; a
// prefix with 4,000 records next to a freshly appeared board of the same ATS
// type is a migration, and the user needs to hear about it.
type Marker struct {
	// StatePrefix is the job-ID namespace this board's postings live under,
	// as of the run that last wrote the marker. Empty means the marker
	// predates this field (or the adapter does not namespace its jobs), which
	// reads as "cannot prove this board owns anything".
	StatePrefix string `json:"state_prefix"`

	// PreviousStatePrefix is the prefix this board ABSORBED via the
	// previous_state_prefix config escape hatch. It is kept after the move so
	// the state-branch gate can authorize the resulting removals from the
	// state file alone, without reading config — see statebranch.
	PreviousStatePrefix string `json:"previous_state_prefix,omitempty"`

	// MovedTo is the identity of the board this one appears to have moved to:
	// set when the census raises an announcement, cleared never. It exists so
	// an undelivered announcement can be RE-RAISED verbatim on the next cycle.
	// Without it the evidence is gone — the successor board gets its own
	// marker during the same run, so it is no longer "fresh" and the signature
	// cannot be recomputed.
	MovedTo string `json:"moved_to,omitempty"`

	// AnnouncedAt is stamped only after an announcement is actually delivered,
	// exactly like Health.SentAt. A failed send leaves it zero and the next
	// cycle raises the same announcement again.
	AnnouncedAt time.Time `json:"announced_at"`

	// OrphanedAt is when a census first found this board gone from the config
	// with nothing to explain it. It is the missing half of the announcement
	// conjunction: a FRESH board is fresh for exactly one run (the next one
	// gives it a marker), but an orphan stays an orphan forever, so without
	// this a company deleted in March would pair with an unrelated company
	// added in September purely because both use Greenhouse — and the mail
	// would offer a previous_state_prefix line that grafts one board's history
	// onto another. Stamping it is what makes "both halves appeared in the
	// same run" true rather than merely documented. It is cleared whenever the
	// board is configured again, so a board that leaves, returns and leaves
	// again is judged afresh.
	OrphanedAt time.Time `json:"orphaned_at"`
}

// Health is what we remember about one job board's ability to answer at all.
// It hangs off a reserved record key, never off a posting.
//
// The point of keeping it in the state file rather than in CI logs is that
// the state file is the only thing that survives between runs, and the user
// reads email only — a workflow log nobody opens cannot notice that a board
// has been silently answering "no openings" since its slug was renamed.
//
// Counters are folded forward one fetch at a time (see internal/health) and
// are never backfilled: a board that has not yet proven itself alive on two
// distinct days simply cannot trip any alarm, which is what makes cold start
// silent by construction.
type Health struct {
	// Company and SrcType are denormalized copies of the source's display
	// name and adapter type. Alerts and the digest are rendered from the
	// stored health alone, so a board dropped from the catalog can still be
	// described in a report instead of showing up as a bare identity string.
	Company string `json:"company"`
	SrcType string `json:"src_type"`

	FirstFetch time.Time `json:"first_fetch"`
	LastOK     time.Time `json:"last_ok"`

	// LastNonEmpty / LastNonEmptyN describe the last fetch that returned any
	// postings at all. The alarm compares against this, never an all-time
	// peak: a company tapering 12 -> 7 -> 3 -> 1 -> 0 is a normal hiring
	// wind-down, and measuring it against its own peak would call that a
	// broken adapter.
	LastNonEmpty  time.Time `json:"last_non_empty"`
	LastNonEmptyN int       `json:"last_non_empty_n"`

	// NonEmptyDays counts DISTINCT UTC days on which this board returned at
	// least one posting. Two days of proof is what separates "the adapter
	// used to work" from "we have never actually seen this board work".
	NonEmptyDays int `json:"non_empty_days"`

	// Fetches counts SUCCESSFUL fetches only. Errors are a different failure
	// mode with their own counters, and mixing them would let an unreachable
	// host masquerade as an observed-empty board.
	Fetches int `json:"fetches"`

	// Baselined records that WE adopted this board — that the runner walked
	// it once and wrote its source marker — rather than inheriting it from an
	// older state file. It is the only thing that distinguishes "a board we
	// have watched since birth and that has never once had a posting" from
	// "an established board that happened to be quiet the first day health
	// tracking existed". Without it, switching this feature on would accuse
	// every board that was empty on day one.
	Baselined bool `json:"baselined"`

	// Recent holds the last few successful counts, oldest first, and Nonzero
	// the last few non-zero ones. Recent exists so a human reading the state
	// file can see the shape of the drop; Nonzero feeds Typical.
	Recent  []int `json:"recent"`
	Nonzero []int `json:"nonzero"`

	// Typical is the median of Nonzero. It is DIGEST ONLY and must never
	// raise an alert: partial drops are what layoffs and hiring freezes look
	// like, and those are far more common than adapter breakage.
	Typical int `json:"typical"`

	// ZeroRuns / ZeroSince track the current run of successful-but-empty
	// fetches. Errors are neutral for this streak — they neither extend nor
	// reset it — so a network outage produces errors, not a fleet-wide
	// "every board went quiet" alarm.
	ZeroRuns  int       `json:"zero_runs"`
	ZeroSince time.Time `json:"zero_since"`

	// ErrRuns / ErrSince / LastErr track the mirror-image condition: a board
	// that keeps failing to answer. LastErr is remote-controlled text and is
	// stored clipped; anything rendering it must still pass it through
	// notify.OneLine.
	ErrRuns  int       `json:"err_runs"`
	ErrSince time.Time `json:"err_since"`
	LastErr  string    `json:"last_err"`

	// Kind is the standing condition: "", "dead", "erroring" or "stillborn".
	// RaisedAt is
	// when it started, SentAt when a report for that episode was actually
	// delivered (stamped only after the notifier returns nil, so a failed
	// send is retried), and EligibleAt the cooldown floor that keeps a
	// flapping board from mailing repeatedly.
	Kind       string    `json:"kind"`
	RaisedAt   time.Time `json:"raised_at"`
	SentAt     time.Time `json:"sent_at"`
	EligibleAt time.Time `json:"eligible_at"`
}

// Run is fleet-wide state that belongs to no single board. It lives on one
// reserved singleton record so the run-level facts are durable across the
// process restarts that CI performs between every cycle.
type Run struct {
	LastRunAt time.Time `json:"last_run_at"`

	// AllFailRuns counts consecutive cycles in which every configured board
	// failed. Today that situation is only a red CI check, which nobody sees.
	// AllFailSentAt marks the outage as already reported: without it the
	// choice is between mailing on every half-hourly cycle of a multi-day
	// outage and losing the report entirely when the first send fails —
	// which, since a total fetch failure usually means the network is gone,
	// is exactly when the first send does fail.
	AllFailRuns   int       `json:"all_fail_runs"`
	AllFailSentAt time.Time `json:"all_fail_sent_at"`

	// DigestSentAt drives the monthly heartbeat. The heartbeat is what makes
	// silence meaningful: without it, "no mail" and "the watcher is dead"
	// look identical from the inbox.
	DigestSentAt time.Time `json:"digest_sent_at"`

	// The evaluated -> matched -> deferred funnel, accumulated since the last
	// digest and reset when one is delivered. A single cycle's numbers cannot
	// tell a quiet week from a broken matcher; a month of "4,000 evaluated, 0
	// matched, 4,000 deferred" can. This is the only thing in the report that
	// can see a failure BELOW the fetch layer.
	EvaluatedSinceDigest int `json:"evaluated_since_digest"`
	MatchedSinceDigest   int `json:"matched_since_digest"`
	DeferredSinceDigest  int `json:"deferred_since_digest"`
}

// Store is a JSON-file-backed set of seen postings, keyed by model.Job.ID.
// Opening takes an exclusive lock so concurrent jobwatch processes can't
// double-notify or clobber each other. It is not safe for concurrent use
// within a process; the runner accesses it from one goroutine only.
type Store struct {
	path            string
	recs            map[string]Record
	release         func()
	revision        uint64
	savedRevision   uint64
	successfulSaves uint64
}

// Persistence reports whether the current in-memory snapshot is durable and
// how many successful saves this Store instance has completed. The runner
// snapshots it at cycle start so a save from an earlier cycle cannot make a
// failed current cycle look persisted.
type Persistence struct {
	Revision        uint64
	SavedRevision   uint64
	SuccessfulSaves uint64
}

func (s *Store) Persistence() Persistence {
	return Persistence{s.revision, s.savedRevision, s.successfulSaves}
}

// Open loads the store at path, creating parent directories as needed.
// A missing file is an empty store, not an error.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	release, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}

	s := &Store{path: path, recs: map[string]Record{}, release: release}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		release()
		return nil, err
	}
	if err := json.Unmarshal(data, &s.recs); err != nil {
		release()
		return nil, fmt.Errorf("state file %s is corrupt (delete it to start fresh): %w", path, err)
	}
	return s, nil
}

// Close releases the inter-process lock. (Exiting the process releases it
// too; Close exists for tidiness and tests.)
func (s *Store) Close() {
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

// Get returns the record for a posting and whether one exists.
func (s *Store) Get(id string) (Record, bool) {
	r, ok := s.recs[id]
	return r, ok
}

// Seen reports whether the posting was processed in an earlier run.
func (s *Store) Seen(id string) bool {
	_, ok := s.recs[id]
	return ok
}

// HasPostingPrefix reports whether a posting record exists under prefix.
// Runner metadata is excluded. Callers use this only to reject ambiguous
// source migrations, never to infer that two scoped source identities match.
func (s *Store) HasPostingPrefix(prefix string) bool {
	if prefix == "" {
		return false
	}
	for id := range s.recs {
		if !strings.HasPrefix(id, "__jobwatch_") && strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

// Add records a posting. It overwrites any previous record.
//
// There is deliberately no Delete of posting records. A posting's presence IS
// the "already processed" fact, so removing one re-notifies the user about a
// job they were already emailed — and the state branch validator rejects a
// push that drops a previously recorded ID for exactly that reason. If a
// future change needs to retire reserved marker keys, it should add a
// narrowly scoped deletion for markers only, never a general one.
func (s *Store) Add(id string, r Record) {
	s.recs[id] = r
	s.revision++
}

// CountPostingPrefix returns how many posting records live under prefix.
// Runner metadata is excluded, exactly as in HasPostingPrefix.
//
// The count, not just the boolean, is what lets the census tell "a board
// disappeared from the config and took real history with it" from "a board
// disappeared that never had any" — the first deserves a report, the second
// is a tidy-up the user already knows about. An empty prefix counts nothing:
// every key would match it, and "" is what StatePrefix returns for a source
// type that does not namespace its jobs.
func (s *Store) CountPostingPrefix(prefix string) int {
	if prefix == "" {
		return 0
	}
	n := 0
	for id := range s.recs {
		if !strings.HasPrefix(id, "__jobwatch_") && strings.HasPrefix(id, prefix) {
			n++
		}
	}
	return n
}

// Delete removes one record. See the warning on Add: deleting a POSTING
// re-notifies the user about a job they were already emailed, and the state
// branch validator rejects a push that drops a previously recorded ID.
//
// It exists for the two cases where a key is provably not a posting the user
// was told about: retiring a reserved marker key, and dropping a record no
// adapter can produce any more (see source.IsJunkStateKey). Callers outside
// those two cases are bugs.
func (s *Store) Delete(id string) {
	if _, ok := s.recs[id]; ok {
		delete(s.recs, id)
		s.revision++
	}
}

// Rekey moves records to the keys fn returns, and reports how many moved and
// how many of those landed on a key that was already occupied.
//
// fn is called once per record with its current key and returns the key that
// record should live under. Returning the same key — or the empty string —
// leaves the record exactly where it is. The empty string deliberately means
// "unchanged" rather than "delete": a buggy rewrite function that fails to
// match a key returns "" for it, and the difference between those two
// readings is the difference between a no-op and a silently emptied state
// file.
//
// MERGING. Two old keys can map to one new key (the whole point of the
// migration is to stop distinguishing boards by transport, and the same board
// may have been recorded under two hosts). When that happens the records are
// combined, never replaced:
//
//   - Notified is ORed. A record that was emailed must NEVER come back as
//     pending — that re-sends a job the user already read, and the state
//     branch rejects the push outright. This is the invariant that matters
//     most here.
//   - Matched is ORed too, which also preserves "notified implies matched".
//   - FirstSeen keeps the EARLIER of the two: the older observation is the
//     true answer to "when did we first see this posting".
//   - Title comes from whichever record kept its FirstSeen, falling back to
//     the other when that one is empty (the validator rejects empty titles).
//   - Health/Run/Marker ride along if either side has them. The rules never
//     rewrite reserved keys, so this is defensive only.
//
// fn is applied in sorted key order so that a merge of three or more records
// is deterministic; Go's randomized map order would otherwise make the
// surviving title depend on the run.
func (s *Store) Rekey(fn func(id string) string) (moved, merged int) {
	ids := make([]string, 0, len(s.recs))
	for id := range s.recs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		rec, ok := s.recs[id]
		if !ok {
			continue // already consumed by an earlier merge
		}
		target := fn(id)
		if target == "" || target == id {
			continue
		}
		moved++
		delete(s.recs, id)
		if existing, clash := s.recs[target]; clash {
			merged++
			rec = mergeRecords(existing, rec)
		}
		s.recs[target] = rec
	}
	if moved > 0 {
		s.revision++
	}
	return moved, merged
}

// mergeRecords combines two records for the same posting. It is commutative:
// the result must not depend on which key the iteration reached first.
func mergeRecords(a, b Record) Record {
	older, newer := a, b
	if preferSecond(a, b) {
		older, newer = b, a
	}
	out := older
	out.Matched = a.Matched || b.Matched
	out.Notified = a.Notified || b.Notified
	if out.Notified {
		out.Matched = true
	}
	if strings.TrimSpace(out.Title) == "" {
		out.Title = newer.Title
	}
	if out.Health == nil {
		out.Health = newer.Health
	}
	if out.Run == nil {
		out.Run = newer.Run
	}
	if out.Marker == nil {
		out.Marker = newer.Marker
	}
	return out
}

// preferSecond reports whether b should supply the fields that cannot be
// combined (FirstSeen, Title). The earlier sighting wins; identical instants
// fall back to the title itself, so that mergeRecords stays commutative
// instead of merely deterministic-in-practice.
func preferSecond(a, b Record) bool {
	switch {
	case b.FirstSeen.IsZero():
		return false
	case a.FirstSeen.IsZero():
		return true
	case b.FirstSeen.Before(a.FirstSeen):
		return true
	case a.FirstSeen.Before(b.FirstSeen):
		return false
	}
	return b.Title < a.Title
}

// Range calls f for each record until f returns false. It is read-only: f
// must not call Add or Save, because mutating the map mid-iteration makes
// which records f sees depend on Go's randomized map order. Order is
// unspecified, so callers that render lists must sort what they collect.
func (s *Store) Range(f func(id string, r Record) bool) {
	for id, r := range s.recs {
		if !f(id, r) {
			return
		}
	}
}

// Len returns the number of recorded postings.
func (s *Store) Len() int { return len(s.recs) }

// Save writes the store atomically: unique temp file, fsync, rename, then
// a best-effort directory fsync — so neither a crash mid-write nor power
// loss right after can leave a truncated state file behind.
func (s *Store) Save() error {
	data, err := json.MarshalIndent(s.recs, "", " ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	s.savedRevision = s.revision
	s.successfulSaves++
	if d, err := os.Open(dir); err == nil {
		d.Sync() // best-effort: makes the rename itself durable
		d.Close()
	}
	return nil
}
