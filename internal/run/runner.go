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
	SeedNewSources bool

	// Rescan re-evaluates every stored posting that was never notified —
	// including seeded backlog — against the current matcher chain. Run it
	// once after changing the rules to sweep the existing board contents.
	Rescan bool

	// DryRun evaluates and reports but never persists state, so the same
	// jobs are re-evaluated next run. Good for tuning the matcher.
	DryRun bool
}

// RunOnce performs a single poll cycle. One company failing to fetch is
// logged and skipped — it must not block alerts from the others.
func (r *Runner) RunOnce(ctx context.Context) error {
	type fetched struct {
		src  source.Source
		jobs []model.Job
		err  error
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
			jobs, err := s.Fetch(ctx)
			results[i] = fetched{src: s, jobs: jobs, err: err}
		}(i, s)
	}
	wg.Wait()

	var matches []notify.Match
	var matchedIDs []string
	totalJobs, newJobs, retries, failures, partials := 0, 0, 0, 0, 0
	newSources, seededJobs := 0, 0
	for _, res := range results {
		if res.err != nil {
			if len(res.jobs) == 0 {
				failures++
				r.Log.Printf("fetch %s: %v", res.src.Company(), res.err)
				continue
			}
			partials++
			r.Log.Printf("fetch %s (partial; keeping %d jobs): %v", res.src.Company(), len(res.jobs), res.err)
		}
		totalJobs += len(res.jobs)

		seedSource := r.SeedOnly
		if (r.SeedOnly || r.SeedNewSources) && !r.DryRun {
			markerID := "__jobwatch_source__/" + source.Identity(res.src)
			if !r.Store.Seen(markerID) {
				knownSource := r.Store.HasPrefix(source.StatePrefix(res.src))
				for _, job := range res.jobs {
					if r.Store.Seen(job.ID) {
						knownSource = true
						break
					}
				}
				if r.SeedNewSources && !r.SeedOnly && !knownSource {
					seedSource = true
					newSources++
				}
				r.Store.Add(markerID, store.Record{
					FirstSeen: time.Now(),
					Title:     "source: " + res.src.Company(),
				})
			}
		}

		for _, job := range res.jobs {
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
				}
				continue
			}

			verdict := r.Matcher.Match(job)
			if verdict.Matched {
				matches = append(matches, notify.Match{Job: job, Reason: verdict.Reason})
				matchedIDs = append(matchedIDs, job.ID)
			}
			if !r.DryRun {
				r.Store.Add(job.ID, store.Record{
					FirstSeen: rec.FirstSeen,
					Title:     job.Company + ": " + job.Title,
					Matched:   verdict.Matched,
				})
			}
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i].Job, matches[j].Job
		if a.Company != b.Company {
			return a.Company < b.Company
		}
		return a.Title < b.Title
	})

	r.Log.Printf("run complete: %d sources (%d failed, %d partial), %d open jobs, %d new, %d matched (%d retried)",
		len(r.Sources), failures, partials, totalJobs, newJobs, len(matches), retries)
	if r.SeedNewSources && newSources > 0 {
		r.Log.Printf("seeded %d new sources (%d current postings) without notifying", newSources, seededJobs)
	}
	if failures == len(r.Sources) && len(r.Sources) > 0 {
		return fmt.Errorf("all %d sources failed to fetch", failures)
	}

	if r.DryRun {
		if len(matches) > 0 {
			r.deliver(ctx, matches)
		}
		r.Log.Printf("dry run: state not saved")
		return nil
	}

	// Persist first: matches are now on disk as pending (Notified=false),
	// so a crash below retries them instead of losing them.
	if err := r.Store.Save(); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	if r.SeedOnly {
		r.Log.Printf("seeded %d postings without notifying; future runs alert on new ones", seededJobs)
		return nil
	}
	if len(matches) == 0 {
		return nil
	}

	if err := r.deliver(ctx, matches); err != nil {
		// Leave the matches pending; the next cycle re-delivers them.
		return err
	}
	for _, id := range matchedIDs {
		rec, _ := r.Store.Get(id)
		rec.Notified = true
		r.Store.Add(id, rec)
	}
	return r.Store.Save()
}

// deliver sends matches through every notifier. All must succeed for the
// batch to count as delivered; a notifier that already succeeded may see
// the same batch again on retry (documented at-least-once behavior).
func (r *Runner) deliver(ctx context.Context, matches []notify.Match) error {
	for _, n := range r.Notifiers {
		if err := n.Notify(ctx, matches); err != nil {
			return fmt.Errorf("notifier %s failed (matches stay pending, retried next run): %w", n.Name(), err)
		}
	}
	return nil
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
