package notify

// Operational reports: messages about the WATCHER rather than about jobs.
// "A board stopped answering" and "here are 3 new matching jobs" are different
// enough that sharing a subject line would make both harder to act on, so they
// travel as a separate shape with their own headline.
//
// Reporter is deliberately NOT part of Notifier. Adding a method to that
// interface would break every channel at once, and two of them (webhook,
// telegram) have no natural rendering for a health report today. The runner
// discovers the capability by type assertion instead, exactly like
// source.Detailer — a channel opts in by growing a Report method, and the ones
// that do not keep compiling and keep delivering matches.

import (
	"context"
	"strings"
)

// Report is one operational message. Lines are rendered as-is, one per line,
// after sanitizing; an empty line is a paragraph break.
type Report struct {
	// Subject is the headline. It must never reuse the match headline: a
	// report that arrives titled "1 new matching job(s)" is a report the user
	// opens expecting a job and closes without reading.
	Subject string
	Lines   []string
}

// Reporter is the optional capability. A Notifier that also implements it can
// deliver operational reports.
type Reporter interface {
	Report(ctx context.Context, r Report) error
}

// ReportText renders a report body as plain text.
//
// Every line goes through OneLine, because report text is not all ours: a
// board's last error message is whatever the remote endpoint returned, and a
// company name comes from a config file. A CRLF inside either would otherwise
// end the Subject header and start one of the attacker's choosing.
//
// Leading spaces are the exception, and they are safe: the indentation is
// generated here, never received, and a run of spaces cannot begin a header
// once OneLine has removed every CR and LF. Measuring the indent before
// sanitizing and re-applying it after is what keeps a hundred-board digest
// readable instead of flat.
func ReportText(r Report) string {
	var b strings.Builder
	for _, line := range r.Lines {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if text := OneLine(line); text != "" {
			b.WriteString(strings.Repeat(" ", indent))
			b.WriteString(text)
		}
		b.WriteString("\n")
	}
	return b.String()
}
