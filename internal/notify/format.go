package notify

// Shared message rendering. Every channel delivers the same content, so
// the formatting lives here once — a new integration only has to implement
// delivery (see webhook.go for the pattern).

import (
	"fmt"
	"strings"
)

// Headline summarizes a batch in one line: "3 new matching job(s)".
func Headline(matches []Match) string {
	return fmt.Sprintf("%d new matching job(s)", len(matches))
}

// Text renders a batch as plain text, one numbered block per job.
func Text(matches []Match) string {
	var b strings.Builder
	for i, m := range matches {
		b.WriteString(Block(i+1, m))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// Block renders a single match as a numbered plain-text block.
func Block(n int, m Match) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d. %s - %s\n", n, OneLine(m.Job.Company), OneLine(m.Job.Title))
	if m.Job.Location != "" {
		fmt.Fprintf(&b, "   Location: %s\n", OneLine(m.Job.Location))
	}
	fmt.Fprintf(&b, "   Why: %s\n", OneLine(m.Reason))
	fmt.Fprintf(&b, "   Apply: %s\n", OneLine(m.Job.URL))
	return b.String()
}

// OneLine collapses whitespace (including CR/LF) and strips control
// characters. Job fields are remote-controlled text; forcing them onto one
// line stops them injecting structure into email headers, chat payloads,
// or anything else a notifier builds.
func OneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
