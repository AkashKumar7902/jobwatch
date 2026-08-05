// statebranch restores and publishes jobwatch's Git-backed state without
// treating transport failures as an empty baseline or discarding history.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"jobwatch/internal/statebranch"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "statebranch:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: statebranch restore|publish [flags]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore default handling so a second signal terminates promptly
	}()

	switch args[0] {
	case "restore":
		return runRestore(ctx, args[1:])
	case "publish":
		return runPublish(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; want restore or publish", args[0])
	}
}

func runRestore(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("statebranch restore", flag.ContinueOnError)
	remote := flags.String("remote", "origin", "configured Git remote name")
	branch := flags.String("branch", "state", "state branch name")
	statePath := flags.String("state", "state.json", "destination state file")
	allowBootstrap := flags.Bool("allow-bootstrap", false, "initialize only when the remote state branch is absent")
	githubOutput := flags.String("github-output", "", "optional GitHub Actions output file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("restore accepts no positional arguments")
	}
	result, err := statebranch.Restore(ctx, statebranch.RestoreOptions{
		Remote:         *remote,
		Branch:         *branch,
		StatePath:      *statePath,
		AllowBootstrap: *allowBootstrap,
		GitHubOutput:   *githubOutput,
	})
	if err != nil {
		return err
	}
	fmt.Printf("state %s: %d records (base %s)\n", result.Mode, result.Count, displaySHA(result.BaseSHA))
	return nil
}

func runPublish(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("statebranch publish", flag.ContinueOnError)
	remote := flags.String("remote", "origin", "configured Git remote name")
	branch := flags.String("branch", "state", "state branch name")
	statePath := flags.String("state", "state.json", "candidate state file")
	mode := flags.String("mode", "", "restore mode: restored or bootstrap")
	baseSHA := flags.String("base-sha", "", "exact restored state commit")
	sourceSHA := flags.String("source-sha", "", "exact source commit that produced this state")
	githubOutput := flags.String("github-output", "", "optional GitHub Actions output file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("publish accepts no positional arguments")
	}
	result, err := statebranch.Publish(ctx, statebranch.PublishOptions{
		Remote:       *remote,
		Branch:       *branch,
		StatePath:    *statePath,
		Mode:         *mode,
		BaseSHA:      *baseSHA,
		SourceSHA:    *sourceSHA,
		GitHubOutput: *githubOutput,
	})
	if err != nil {
		return err
	}
	fmt.Printf("state publish: changed=%t sha=%s\n", result.Changed, result.PublishSHA)
	return nil
}

func displaySHA(sha string) string {
	if sha == "" {
		return "none"
	}
	return sha
}
