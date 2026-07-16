package notify

// The "console" notifier prints matches to stdout. Useful on its own for
// cron jobs (cron mails stdout) and as the channel used by --dry-run.

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
	fmt.Fprintf(os.Stdout, "\n%d matching job(s):\n\n", len(matches))
	for i, m := range matches {
		fmt.Fprintf(os.Stdout, "%2d. %s — %s\n", i+1, m.Job.Company, m.Job.Title)
		if m.Job.Location != "" {
			fmt.Fprintf(os.Stdout, "    Location: %s\n", m.Job.Location)
		}
		fmt.Fprintf(os.Stdout, "    Why: %s\n", m.Reason)
		fmt.Fprintf(os.Stdout, "    %s\n\n", m.Job.URL)
	}
	return nil
}
