// jobwatch polls company job boards, matches new postings against your
// criteria (by default: experience requirement of at most 1 year), and
// notifies you by email.
//
// Usage:
//
//	jobwatch -config config.yaml -seed        # first run: baseline, no emails
//	jobwatch -config config.yaml              # poll once (ideal under cron)
//	jobwatch -config config.yaml -interval 1h # keep running, poll hourly
//	jobwatch -config config.yaml -dry-run     # print matches, change nothing
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"jobwatch/internal/config"
	"jobwatch/internal/match"
	"jobwatch/internal/notify"
	"jobwatch/internal/run"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

func main() {
	var (
		configPath = flag.String("config", "config.yaml", "path to config file")
		interval   = flag.Duration("interval", 0, "poll repeatedly at this interval (e.g. 1h); 0 runs once and exits")
		seed       = flag.Bool("seed", false, "record all current postings as seen without notifying (recommended first run)")
		seedNew    = flag.Bool("seed-new-sources", false, "baseline only boards not previously recorded; known boards still notify")
		rescan     = flag.Bool("rescan", false, "re-evaluate stored postings that were never notified (seeded backlog) with the current rules")
		dryRun     = flag.Bool("dry-run", false, "evaluate and print matches to the console; send no email, save no state")
		statePath  = flag.String("state", "", "override the state file location from config (store.path)")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "jobwatch ", log.LstdFlags)
	if *rescan && (*seed || *seedNew || *interval > 0) {
		logger.Fatal("-rescan is a one-shot sweep: combine it only with -dry-run or -state")
	}
	if *seed && *interval > 0 {
		logger.Fatal("-seed cannot be combined with -interval: run once with -seed first, then start the watcher")
	}
	if *seed && *dryRun {
		logger.Fatal("-seed cannot be combined with -dry-run: seeding is only useful when state is saved")
	}
	if *seed && *seedNew {
		logger.Fatal("-seed cannot be combined with -seed-new-sources")
	}
	if *seedNew && *dryRun {
		logger.Fatal("-seed-new-sources cannot be combined with -dry-run: seeding is only useful when state is saved")
	}

	runner, err := build(*configPath, *statePath, logger, *seed, *seedNew, *dryRun)
	if err != nil {
		logger.Fatal(err)
	}
	runner.Rescan = *rescan
	defer runner.Store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// After the first signal cancels ctx, restore default signal handling
	// so a second Ctrl-C terminates even if something is slow to unwind.
	go func() {
		<-ctx.Done()
		stop()
	}()

	if *interval > 0 {
		logger.Printf("watching %d companies every %s", len(runner.Sources), interval)
		runner.RunEvery(ctx, *interval)
		return
	}
	if err := runner.RunOnce(ctx); err != nil {
		logger.Fatal(err)
	}
}

// describeMatcher renders the configured matcher tree as e.g.
// "all(experience, employment, keywords, llm)" for the startup log line.
func describeMatcher(p config.Plugin) string {
	if len(p.Of) == 0 {
		return p.Name
	}
	var kids []string
	for _, child := range p.Of {
		kids = append(kids, describeMatcher(child))
	}
	return p.Name + "(" + strings.Join(kids, ", ") + ")"
}

// matcherSpec converts the config's matcher block (possibly a nested
// combinator tree) into the match package's Spec.
func matcherSpec(p config.Plugin) match.Spec {
	s := match.Spec{Name: p.Name, Params: p.Params}
	for _, child := range p.Of {
		s.Of = append(s.Of, matcherSpec(child))
	}
	return s
}

// absorber is one company entry that declared previous_state_prefix.
type absorber struct {
	company  string
	identity string
	prefix   string // where this board's postings live now
	previous string // the namespace it is claiming
}

// checkPreviousStatePrefixes validates the migration escape hatch and returns
// the identity -> absorbed-prefix map the runner applies.
//
// The line it validates is one the user PASTES out of a migration email, into a
// file, by hand, maybe twice a decade — so the realistic failure is not misuse
// but a typo, and the blast radius of a typo is the whole point. The runner
// executes this as a bare prefix swap over every key: `workday/warnerbros/`
// instead of `workday/warnerbros/global/` would silently drag a still-configured
// board's entire history onto another board's keys, and the state branch would
// accept it, because a declared move is exactly what authorizes those removals.
//
// So the rule is stronger than "must differ from a live prefix": a claimed
// namespace may not OVERLAP a live one in either direction. Containment is the
// dangerous shape, equality is just its degenerate case, and the check costs one
// comparison per configured board at startup. An orphaned board's prefix cannot
// legitimately overlap a live board's — the 263 catalog prefixes are pairwise
// non-overlapping — so nothing correct is rejected here.
func checkPreviousStatePrefixes(absorbers []absorber, livePrefixes map[string]string) (map[string]string, error) {
	if len(absorbers) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(absorbers))
	claimed := make(map[string]string, len(absorbers))
	for _, a := range absorbers {
		if a.prefix == "" {
			// The board does not namespace its postings, so there is no
			// destination to move them to. The state branch validator rejects
			// this shape too; failing here says why.
			return nil, fmt.Errorf("company %q: previous_state_prefix is set but this source type does not namespace its postings, so there is nothing to move them into",
				a.company)
		}
		// Sorted, and other boards before this one: a truncated paste like
		// "greenhouse/" overlaps its own prefix AND every sibling's, and which
		// of those a map-ordered scan reported first would change between runs.
		// The sibling is also the one worth naming — it is the board about to
		// lose its history.
		live := make([]string, 0, len(livePrefixes))
		for prefix := range livePrefixes {
			if prefix != a.prefix && prefixOverlaps(a.previous, prefix) {
				live = append(live, prefix)
			}
		}
		sort.Strings(live)
		if len(live) > 0 {
			return nil, fmt.Errorf("company %q: previous_state_prefix %q overlaps the live state prefix %q of %q; adopting it would move that board's history onto %s",
				a.company, a.previous, live[0], livePrefixes[live[0]], a.identity)
		}
		if prefixOverlaps(a.previous, a.prefix) {
			return nil, fmt.Errorf("company %q: previous_state_prefix %q is this board's own current state prefix; it names the OLD prefix the board's history is stranded under, and once the move is done jobwatch stops finding anything there",
				a.company, a.previous)
		}
		if owner, dup := claimed[a.previous]; dup {
			// Two boards claiming one history have no way to split it, and the
			// state branch refuses the resulting removals anyway. Better to say
			// so before the first fetch than to fail at publish time.
			return nil, fmt.Errorf("companies %q and %q both claim previous_state_prefix %q; only one board can absorb it",
				owner, a.company, a.previous)
		}
		claimed[a.previous] = a.company
		out[a.identity] = a.previous
	}
	return out, nil
}

// prefixOverlaps reports whether two state-prefix namespaces intersect. Either
// containment direction means one board's keys live inside the other's.
func prefixOverlaps(a, b string) bool {
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// build assembles the runner from config: sources, matcher, notifiers, store.
func build(configPath, statePath string, logger *log.Logger, seed, seedNew, dryRun bool) (*run.Runner, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	if statePath != "" {
		cfg.Store.Path = statePath
	}

	client := &http.Client{Timeout: time.Duration(cfg.Poll.TimeoutSeconds) * time.Second}

	var sources []source.Source
	identities := make(map[string]string)
	livePrefixes := make(map[string]string)
	var absorbers []absorber
	for _, c := range cfg.Companies {
		s, err := source.New(c.Source, c.Name, c.Params, client)
		if err != nil {
			return nil, fmt.Errorf("company %q: %w", c.Name, err)
		}
		identity := source.Identity(s)
		if previous, exists := identities[identity]; exists {
			return nil, fmt.Errorf("company %q duplicates ATS board %q already used by %q", c.Name, identity, previous)
		}
		identities[identity] = c.Name
		if prefix := source.StatePrefix(s); prefix != "" {
			livePrefixes[prefix] = c.Name
		}
		if c.PreviousStatePrefix != "" {
			absorbers = append(absorbers, absorber{
				company: c.Name, identity: identity,
				prefix: source.StatePrefix(s), previous: c.PreviousStatePrefix,
			})
		}
		sources = append(sources, s)
	}
	previousStatePrefixes, err := checkPreviousStatePrefixes(absorbers, livePrefixes)
	if err != nil {
		return nil, err
	}

	matcher, err := match.Build(matcherSpec(cfg.Matcher))
	if err != nil {
		return nil, fmt.Errorf("matcher: %w", err)
	}

	var notifiers []notify.Notifier
	if dryRun {
		// Dry runs always report to the console and never email.
		console, err := notify.New("console", nil)
		if err != nil {
			return nil, err
		}
		notifiers = []notify.Notifier{console}
	} else {
		for _, n := range cfg.Notifiers {
			notifier, err := notify.New(n.Name, n.Params)
			if err != nil {
				return nil, fmt.Errorf("notifier %q: %w", n.Name, err)
			}
			notifiers = append(notifiers, notifier)
		}
	}

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return nil, fmt.Errorf("opening state store: %w", err)
	}

	var notifierNames []string
	reporters := 0
	for _, n := range notifiers {
		notifierNames = append(notifierNames, n.Name())
		if _, ok := n.(notify.Reporter); ok {
			reporters++
		}
	}
	logger.Printf("starting: %d companies | matcher %s | notifiers %v | state %s (%d records)",
		len(sources), describeMatcher(cfg.Matcher), notifierNames, cfg.Store.Path, st.Len())
	if reporters == 0 {
		// The whole point of board-health reporting is that a broken adapter
		// stops being invisible. A configuration where no channel can carry
		// those reports puts it right back to invisible, so say so out loud
		// rather than letting the gap be discovered during an incident.
		logger.Printf("warning: none of the configured notifiers %v can deliver board-health reports "+
			"(no notify.Reporter); a board that stops answering will go unreported — add the console or email notifier",
			notifierNames)
	}

	for identity, previous := range previousStatePrefixes {
		// Said out loud because it is the one config line that MOVES existing
		// records rather than describing how to fetch new ones. After the first
		// run it is a no-op (nothing is left under the old prefix), so seeing it
		// logged run after run with no "moved N record(s)" line is how the user
		// learns the prefix they pasted was not the one holding the history.
		logger.Printf("previous_state_prefix: %s will absorb any records still under %q", identity, previous)
	}

	return &run.Runner{
		Sources:               sources,
		Matcher:               matcher,
		Notifiers:             notifiers,
		Store:                 st,
		Log:                   logger,
		Concurrency:           cfg.Poll.Concurrency,
		SeedOnly:              seed,
		SeedNewSources:        seedNew,
		DryRun:                dryRun,
		PreviousStatePrefixes: previousStatePrefixes,
	}, nil
}
