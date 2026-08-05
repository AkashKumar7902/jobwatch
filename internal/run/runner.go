// Package run orchestrates one polling cycle:
// fetch (concurrently) → filter to unprocessed jobs → match → persist as
// pending → notify → mark delivered.
//
// Delivery is at-least-once: state is saved with matches marked pending
// BEFORE notifying, and they are only marked notified (and saved again)
// after every notifier succeeds. A crash or SMTP failure in between means
// the next cycle retries those same jobs instead of losing them.
package run

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"jobwatch/internal/match"
	"jobwatch/internal/model"
	"jobwatch/internal/notify"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

const (
	sourceMarkerPrefix          = "__jobwatch_source__/"
	sourceSeedInProgressPrefix  = "__jobwatch_source_seed_in_progress__/"
	sourceRegistryV2ID          = "__jobwatch_source_registry_v2__"
	seedFailureErrorSampleLimit = 5
)

// Runner wires the pluggable pieces together for one or more cycles.
type Runner struct {
	Sources     []source.Source
	Matcher     match.Matcher
	Notifiers   []notify.Notifier
	Store       *store.Store
	Log         *log.Logger
	Concurrency int

	// SeedOnly records every current posting as seen WITHOUT notifying.
	// Use it on the first run so you don't get emailed a company's entire
	// historical job board.
	SeedOnly bool

	// SeedNewSources baselines boards that have never appeared in the state
	// store, while known boards continue matching normally. It is safe to leave
	// enabled so catalog additions do not cause a one-time notification blast.
	// Once a baseline starts, its durable progress marker forces silent resumption
	// even if a later invocation omits this flag.
	SeedNewSources bool

	// Rescan re-evaluates every stored posting that was never notified —
	// including seeded backlog — against the current matcher chain. Run it
	// once after changing the rules to sweep the existing board contents.
	Rescan bool

	// DryRun evaluates and reports but never persists state, so the same
	// jobs are re-evaluated next run. Good for tuning the matcher.
	DryRun bool

	// SaveEvery bounds how much matcher work a killed process can lose:
	// state is checkpointed to disk at least this often while evaluating
	// jobs. LLM matching can spend seconds per posting, so a big board
	// sweep may run for hours — without interim saves, a CI timeout or
	// SIGTERM would discard all of it and the next run would repeat the
	// identical work forever. Zero means the one-minute default.
	SaveEvery time.Duration
}

// RunOnce performs a single poll cycle. One company failing to fetch is
// logged and skipped — it must not block alerts from the others.
func (r *Runner) RunOnce(ctx context.Context) error {
	stateWasEmpty := r.Store.Len() == 0
	if !r.DryRun && !r.SeedOnly && !r.SeedNewSources {
		changed := false
		for _, src := range r.Sources {
			identity := source.Identity(src)
			if !r.Store.Seen(sourceMarkerPrefix+identity) && !r.Store.Seen(sourceSeedInProgressPrefix+identity) {
				r.Store.Add(sourceMarkerPrefix+identity, store.Record{
					FirstSeen: time.Now(),
					Title:     "source: " + src.Company(),
				})
				changed = true
			}
		}
		if !r.Store.Seen(sourceRegistryV2ID) {
			r.Store.Add(sourceRegistryV2ID, store.Record{
				FirstSeen: time.Now(),
				Title:     "exact source registry v2",
			})
			changed = true
		}
		if changed {
			// Ordinary mode explicitly means every configured source is watched.
			// Persist the complete classification before any fetch or job work so
			// interruption cannot leave later sources looking newly added.
			if err := r.Store.Save(); err != nil {
				return fmt.Errorf("saving watched-source registry: %w", err)
			}
		}
	}

	if r.SeedNewSources && !r.SeedOnly && !r.DryRun && !r.Store.Seen(sourceRegistryV2ID) {
		for _, src := range r.Sources {
			identity := source.Identity(src)
			if r.Store.Seen(sourceMarkerPrefix+identity) || r.Store.Seen(sourceSeedInProgressPrefix+identity) {
				continue
			}
			postingPrefix := source.StatePrefix(src)
			if postingPrefix == "" && r.Store.Len() > 0 {
				return fmt.Errorf("cannot seed new source %q: existing state has no exact marker and this source has no posting prefix to prove it is new; run the unchanged source list once without -seed-new-sources, or seed this source explicitly", identity)
			}
			if r.Store.HasPostingPrefix(postingPrefix) {
				return fmt.Errorf("cannot seed new source %q: existing posting records share its prefix but no exact source marker exists; run the unchanged source list once without -seed-new-sources, or seed this source explicitly", identity)
			}
		}
		// Every markerless configured source is now proven to have no legacy
		// posting records. Persist that classification atomically before any
		// baseline work, so interruption cannot create a mixed ambiguous state.
		r.Store.Add(sourceRegistryV2ID, store.Record{
			FirstSeen: time.Now(),
			Title:     "exact source registry v2",
		})
		if err := r.Store.Save(); err != nil {
			return fmt.Errorf("saving exact source registry: %w", err)
		}
	}

	type fetched struct {
		src  source.Source
		jobs []model.Job
		err  error
		dur  time.Duration
	}
	results := make([]fetched, len(r.Sources))

	sem := make(chan struct{}, max(1, r.Concurrency))
	var wg sync.WaitGroup
	for i, s := range r.Sources {
		wg.Add(1)
		go func(i int, s source.Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			jobs, err := s.Fetch(ctx)
			results[i] = fetched{src: s, jobs: jobs, err: err, dur: time.Since(start).Round(time.Millisecond)}
		}(i, s)
	}
	wg.Wait()

	var matches []notify.Match
	var matchedIDs []string
	totalJobs, newJobs, retries, failures, partials, deferred := 0, 0, 0, 0, 0, 0
	newSources, seededJobs := 0, 0
	var matchFailures deferredErrors
	var seedFailures seedErrors

	// checkpoint persists progress accumulated so far (dirty tracks unsaved
	// Store.Add calls). Mid-sweep callers ignore the returned error — it is
	// logged here, and one failed interim write must not abort a sweep the
	// final Save may still persist. The cancellation path does propagate it,
	// because there no later Save will run.
	saveEvery := r.SaveEvery
	if saveEvery <= 0 {
		saveEvery = time.Minute
	}
	lastSave := time.Now()
	dirty := 0
	checkpoint := func() error {
		if r.DryRun || dirty == 0 {
			return nil
		}
		lastSave = time.Now() // set even on failure so a bad disk isn't retried every job
		if err := r.Store.Save(); err != nil {
			r.Log.Printf("checkpoint save failed: %v", err)
			return err
		}
		dirty = 0
		return nil
	}

	for _, res := range results {
		totalJobs += len(res.jobs)

		seedSource := r.SeedOnly
		seedNewSource := false
		markerID := ""
		inProgressID := ""
		if !r.DryRun {
			identity := source.Identity(res.src)
			id := sourceMarkerPrefix + identity
			progressID := sourceSeedInProgressPrefix + identity
			seedInProgress := r.Store.Seen(progressID)
			if !r.Store.Seen(id) {
				if !r.SeedOnly && (seedInProgress || r.SeedNewSources) {
					seedSource = true
					seedNewSource = true
				}
				if seedSource {
					inProgressID = progressID
					markerID = id
				}
			}
		}

		// An append-only in-progress marker must be durable before any seeded
		// postings can become durable. If a run is interrupted, this marker
		// forces the unseen remainder to stay in baseline mode on the next run,
		// even if that invocation omits the seeding flag.
		if inProgressID != "" && !r.Store.Seen(inProgressID) {
			r.Store.Add(inProgressID, store.Record{
				FirstSeen: time.Now(),
				Title:     "source seed in progress: " + res.src.Company(),
			})
			dirty++
			checkpoint()
		}

		if res.err != nil {
			if len(res.jobs) == 0 {
				failures++
				r.Log.Printf("fetch %s: %v", res.src.Company(), res.err)
			} else {
				partials++
				r.Log.Printf("fetch %s (partial; returned %d jobs): %v", res.src.Company(), len(res.jobs), res.err)
			}

			// A partial response is useful during ordinary polling, but it cannot
			// establish a safe baseline: unseen postings may be missing from it.
			// Keep the progress marker, record none of the returned jobs, and do
			// not write the completion marker. A later full fetch resumes seeding.
			if r.SeedOnly || seedNewSource {
				seedFailures.add(res.src, res.err)
				if len(res.jobs) > 0 {
					r.Log.Printf("seed %s incomplete: ignoring %d jobs from partial fetch", res.src.Company(), len(res.jobs))
				}
				checkpoint()
				continue
			}
			if len(res.jobs) == 0 {
				continue
			}
		}

		srcNew, srcMatched := 0, 0
		for _, job := range res.jobs {
			// Matching can be slow (the llm matcher makes an API call per
			// job); honor cancellation between jobs so Ctrl-C actually
			// stops a long sweep instead of only stopping future fetches.
			select {
			case <-ctx.Done():
				// Save everything already evaluated before unwinding, so an
				// interrupted sweep resumes where it stopped instead of
				// starting over. This is the last chance to persist — a
				// failed save here must not masquerade as a clean interrupt.
				if err := checkpoint(); err != nil {
					return errors.Join(ctx.Err(), fmt.Errorf("checkpoint at interruption: %w", err))
				}
				return ctx.Err()
			default:
			}
			rec, seen := r.Store.Get(job.ID)
			if seen {
				processed := !rec.Matched || rec.Notified
				if r.Rescan {
					// A rescan only trusts the Notified flag; seeded and
					// previously-unmatched postings get a fresh verdict.
					processed = rec.Notified
				}
				if processed {
					continue
				}
			}
			if seen {
				retries++ // matched earlier but delivery was never confirmed
			} else {
				newJobs++
				srcNew++
				rec.FirstSeen = time.Now()
			}

			// Seeding never notifies, so evaluating would be wasted work —
			// and with an llm matcher configured, wasted API spend.
			if seedSource {
				// Never overwrite a pending delivery when somebody runs a seed
				// against an existing state file.
				if !seen {
					seededJobs++
					r.Store.Add(job.ID, store.Record{
						FirstSeen: rec.FirstSeen,
						Title:     job.Company + ": " + job.Title,
					})
					dirty++
					if time.Since(lastSave) >= saveEvery {
						checkpoint()
					}
				}
				continue
			}

			// Sources with lazy details (SmartRecruiters, BambooHR) list
			// whole boards cheaply; fetch the full posting only now that
			// we know this job actually needs evaluating.
			if job.Description == "" {
				if det, ok := res.src.(source.Detailer); ok {
					if err := det.Detail(ctx, &job); err != nil {
						// Not recorded as seen, so the next run retries it.
						r.Log.Printf("detail %s — %s: %v (retried next run)", job.Company, job.Title, err)
						continue
					}
				}
			}

			verdict, err := r.Matcher.Match(ctx, job)
			if err != nil {
				if ctx.Err() != nil {
					if saveErr := checkpoint(); saveErr != nil {
						return errors.Join(ctx.Err(), fmt.Errorf("checkpoint at interruption: %w", saveErr))
					}
					return ctx.Err()
				}
				deferred++
				matchFailures.add(job, err)
				r.Log.Printf("match deferred %s — %s: %s (retried next run)",
					job.Company, job.Title, clip(err.Error(), 300))
				continue
			}
			if verdict.Matched {
				srcMatched++
				matches = append(matches, notify.Match{Job: job, Reason: verdict.Reason})
				matchedIDs = append(matchedIDs, job.ID)
				r.Log.Printf("MATCH   %s — %s (%s): %s", job.Company, job.Title, job.Location, clip(verdict.Reason, 300))
			} else {
				r.Log.Printf("no-match %s — %s: %s", job.Company, job.Title, clip(verdict.Reason, 180))
			}
			if !r.DryRun {
				r.Store.Add(job.ID, store.Record{
					FirstSeen: rec.FirstSeen,
					Title:     job.Company + ": " + job.Title,
					Matched:   verdict.Matched,
				})
				dirty++
				if time.Since(lastSave) >= saveEvery {
					checkpoint()
				}
			}
		}
		// The marker is recorded only once the whole board has been walked:
		// a checkpoint must never persist "board baselined" ahead of the
		// baseline itself, or an interrupted first pass would turn into a
		// full-board notification blast on the next run.
		if markerID != "" {
			r.Store.Add(markerID, store.Record{
				FirstSeen: time.Now(),
				Title:     "source: " + res.src.Company(),
			})
			dirty++
			if seedNewSource {
				newSources++
			}
		}
		// One line per board keeps CI logs scannable: what was fetched,
		// how long it took, and whether anything new turned up.
		r.Log.Printf("fetch %s: %d open jobs in %s (%d new, %d matched)",
			res.src.Company(), len(res.jobs), res.dur, srcNew, srcMatched)
		// A board's work is durable the moment the board is done; the
		// in-loop interval save covers long boards, this covers short ones.
		checkpoint()
	}
	if ctx.Err() != nil {
		if err := checkpoint(); err != nil {
			return errors.Join(ctx.Err(), fmt.Errorf("checkpoint at interruption: %w", err))
		}
		return ctx.Err()
	}
	seedErr := seedFailures.err()
	if !r.DryRun && r.SeedOnly && stateWasEmpty && !r.Store.Seen(sourceRegistryV2ID) && seedErr == nil {
		// Only a complete bootstrap from an empty state can establish the global
		// registry through SeedOnly. Seeding one source inside an existing state
		// must not certify omitted legacy sources as classified.
		r.Store.Add(sourceRegistryV2ID, store.Record{
			FirstSeen: time.Now(),
			Title:     "exact source registry v2",
		})
		dirty++
		if err := checkpoint(); err != nil {
			return fmt.Errorf("saving exact source registry: %w", err)
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i].Job, matches[j].Job
		if a.Company != b.Company {
			return a.Company < b.Company
		}
		return a.Title < b.Title
	})

	r.Log.Printf("run complete: %d sources (%d failed, %d partial), %d open jobs, %d new, %d matched, %d deferred (%d retried)",
		len(r.Sources), failures, partials, totalJobs, newJobs, len(matches), deferred, retries)
	if newSources > 0 {
		r.Log.Printf("seeded %d new sources (%d previously unseen postings) without notifying", newSources, seededJobs)
	}
	deferredErr := matchFailures.err()
	if failures == len(r.Sources) && len(r.Sources) > 0 {
		allFailedErr := fmt.Errorf("all %d sources failed to fetch", failures)
		// A global seed must save its safe progress before reporting any
		// incomplete source. Ordinary polling has no progress to preserve when
		// every fetch failed, so retain its existing early return.
		if !r.SeedOnly && seedErr == nil {
			return errors.Join(allFailedErr, deferredErr, seedErr)
		}
		seedErr = errors.Join(allFailedErr, seedErr)
	}

	if r.DryRun {
		var deliverErr error
		if len(matches) > 0 {
			deliverErr = r.deliver(ctx, matches)
		}
		r.Log.Printf("dry run: state not saved")
		return errors.Join(deliverErr, deferredErr, seedErr)
	}

	// Persist first: matches are now on disk as pending (Notified=false),
	// so a crash below retries them instead of losing them.
	if err := r.Store.Save(); err != nil {
		return errors.Join(fmt.Errorf("saving state: %w", err), deferredErr, seedErr)
	}
	r.Log.Printf("state saved: %d records", r.Store.Len())
	if r.SeedOnly {
		if seedErr != nil {
			r.Log.Printf("seed incomplete: saved %d postings from complete sources; retry before enabling alerts", seededJobs)
		} else {
			r.Log.Printf("seeded %d postings without notifying; future runs alert on new ones", seededJobs)
		}
		return errors.Join(deferredErr, seedErr)
	}
	if len(matches) == 0 {
		return errors.Join(deferredErr, seedErr)
	}

	if err := r.deliver(ctx, matches); err != nil {
		// Leave the matches pending; the next cycle re-delivers them.
		return errors.Join(err, deferredErr, seedErr)
	}
	for _, id := range matchedIDs {
		rec, _ := r.Store.Get(id)
		rec.Notified = true
		r.Store.Add(id, rec)
	}
	return errors.Join(r.Store.Save(), deferredErr, seedErr)
}

const deferredErrorSampleLimit = 5

type deferredErrors struct {
	count   int
	samples []string
}

type seedErrors struct {
	count   int
	samples []string
}

func (s *seedErrors) add(src source.Source, err error) {
	s.count++
	if len(s.samples) >= seedFailureErrorSampleLimit {
		return
	}
	s.samples = append(s.samples, clip(src.Company(), 160)+": "+clip(err.Error(), 300))
}

func (s *seedErrors) err() error {
	if s.count == 0 {
		return nil
	}
	summary := fmt.Sprintf("%d source baseline(s) incomplete", s.count)
	if s.count > len(s.samples) {
		summary += fmt.Sprintf(" (showing first %d)", len(s.samples))
	}
	samples := make([]error, 0, len(s.samples))
	for _, sample := range s.samples {
		samples = append(samples, errors.New(sample))
	}
	return fmt.Errorf("%s: %w", summary, errors.Join(samples...))
}

func (d *deferredErrors) add(job model.Job, err error) {
	d.count++
	if len(d.samples) >= deferredErrorSampleLimit {
		return
	}
	label := clip(job.Company+" — "+job.Title, 160)
	d.samples = append(d.samples, label+": "+clip(err.Error(), 300))
}

func (d *deferredErrors) err() error {
	if d.count == 0 {
		return nil
	}
	summary := fmt.Sprintf("%d matcher evaluation(s) deferred", d.count)
	if d.count > len(d.samples) {
		summary += fmt.Sprintf(" (showing first %d)", len(d.samples))
	}
	samples := make([]error, 0, len(d.samples))
	for _, sample := range d.samples {
		samples = append(samples, errors.New(sample))
	}
	return fmt.Errorf("%s: %w", summary, errors.Join(samples...))
}

// deliver sends matches through every notifier. All must succeed for the
// batch to count as delivered; a notifier that already succeeded may see
// the same batch again on retry (documented at-least-once behavior).
func (r *Runner) deliver(ctx context.Context, matches []notify.Match) error {
	for _, n := range r.Notifiers {
		start := time.Now()
		if err := n.Notify(ctx, matches); err != nil {
			return fmt.Errorf("notifier %s failed (matches stay pending, retried next run): %w", n.Name(), err)
		}
		r.Log.Printf("notify %s: delivered %d match(es) in %s", n.Name(), len(matches), time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// clip shortens s for log lines.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// RunEvery calls RunOnce immediately and then repeatedly, waiting interval
// between the END of one cycle and the start of the next, until ctx is
// cancelled. Cycle errors are logged, not fatal — a watcher should survive
// transient network and SMTP failures.
func (r *Runner) RunEvery(ctx context.Context, interval time.Duration) {
	for {
		if err := r.RunOnce(ctx); err != nil {
			r.Log.Printf("run failed: %v", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
