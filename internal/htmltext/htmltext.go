// Package htmltext converts HTML job descriptions into plain text that the
// matchers can scan. It is deliberately simple — job boards emit basic
// markup, not arbitrary documents.
package htmltext

import (
	"html"
	"regexp"
	"strings"
)

var (
	// Closing block-level tags and <br> become newlines so list items and
	// paragraphs don't run into each other ("2 years" + "401k benefits").
	blockRe = regexp.MustCompile(`(?i)</(?:p|div|li|ul|ol|h[1-6]|tr|table|section|article)>|<br\s*/?>`)
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe = regexp.MustCompile(`[ \t\r\x{00a0}]+`)
	blankRe = regexp.MustCompile(`\n{3,}`)
)

// ToText strips tags from an HTML fragment and decodes entities.
func ToText(h string) string {
	// Some ATS APIs (Greenhouse) ship the description HTML-escaped, i.e.
	// the payload literally contains "&lt;p&gt;". Unescape first so the
	// tag stripper sees real tags.
	if !strings.Contains(h, "<") && strings.Contains(h, "&lt;") {
		h = html.UnescapeString(h)
	}
	h = blockRe.ReplaceAllString(h, "\n")
	h = tagRe.ReplaceAllString(h, " ")
	h = html.UnescapeString(h)
	h = spaceRe.ReplaceAllString(h, " ")

	lines := strings.Split(h, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	h = strings.Join(lines, "\n")
	h = blankRe.ReplaceAllString(h, "\n\n")
	return strings.TrimSpace(h)
}
