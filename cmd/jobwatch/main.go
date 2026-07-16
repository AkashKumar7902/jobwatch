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

// matcherSpec converts the config's matcher block (possibly a nested
// combinator tree) into the match package's Spec.
func matcherSpec(p config.Plugin) match.Spec {
	s := match.Spec{Name: p.Name, Params: p.Params}
	for _, child := range p.Of {
		s.Of = append(s.Of, matcherSpec(child))
	}
	return s
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
		sources = append(sources, s)
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

	return &run.Runner{
		Sources:        sources,
		Matcher:        matcher,
		Notifiers:      notifiers,
		Store:          st,
		Log:            logger,
		Concurrency:    cfg.Poll.Concurrency,
		SeedOnly:       seed,
		SeedNewSources: seedNew,
		DryRun:         dryRun,
	}, nil
}
