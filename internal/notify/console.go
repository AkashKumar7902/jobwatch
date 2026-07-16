package notify

// The "console" notifier prints matches to stdout. Useful on its own for
// cron jobs (cron mails stdout) and as the channel used by --dry-run.
// It is also the smallest possible notifier — copy it to start a new one.

import (
	"context"
	"fmt"
	"os"

	"jobwatch/internal/params"
)

func init() {
	Register("console", func(params.Map) (Notifier, error) {
		return console{}, nil
	})
}

type console struct{}

func (console) Name() string { return "console" }

func (console) Notify(_ context.Context, matches []Match) error {
	_, err := fmt.Fprintf(os.Stdout, "\n%s:\n\n%s", Headline(matches), Text(matches))
	return err
}
