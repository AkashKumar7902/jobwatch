package match

// The "experience" matcher implements the rule: notify when a posting
// explicitly lists a required experience level of at most max_years —
// "0+ years", "1+ year", "0-2 years", "6 months", "entry level", and so on.
//
// When a posting mentions several figures ("5+ years backend, 1+ years
// Kubernetes"), the LOWEST figure wins. That is a deliberate,
// recall-favoring choice: missing a real entry-level job costs more than an
// occasional extra email. Swap the algorithm by writing a new Matcher.
//
// Config:
//
//	matcher:
//	  name: experience
//	  params:
//	    max_years: 1                 # notify when required experience <= this
//	    notify_when_unlisted: false  # also notify when nothing is listed

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("experience", func(p params.Map, children []Matcher) (Matcher, error) {
		if err := RequireNoChildren("experience", children); err != nil {
			return nil, err
		}
		maxYears, err := p.Float("max_years", 1)
		if err != nil {
			return nil, err
		}
		unlisted, err := p.Bool("notify_when_unlisted", false)
		if err != nil {
			return nil, err
		}
		return &Experience{MaxYears: maxYears, NotifyWhenUnlisted: unlisted}, nil
	})
}

// Experience matches jobs whose stated experience requirement is at most
// MaxYears. Jobs that don't state one match only if NotifyWhenUnlisted.
type Experience struct {
	MaxYears           float64
	NotifyWhenUnlisted bool
}

func (e *Experience) Name() string { return "experience" }

func (e *Experience) Match(job model.Job) Result {
	text := job.Title + "\n" + job.Description
	mentions := extractMentions(text)

	if len(mentions) == 0 {
		return Result{
			Matched: e.NotifyWhenUnlisted,
			Reason:  "no experience requirement listed",
		}
	}

	best := mentions[0]
	for _, m := range mentions[1:] {
		if m.minYears < best.minYears {
			best = m
		}
	}
	reason := fmt.Sprintf("asks for %s experience: %q", fmtYears(best.minYears), best.snippet)
	return Result{Matched: best.minYears <= e.MaxYears, Reason: reason}
}

// mention is one experience figure found in the posting text.
type mention struct {
	minYears float64 // the lower bound the posting asks for
	snippet  string  // surrounding text, for the notification
}

var (
	// "0-2 years", "1 to 3 yrs", "0.5-2 years" — the lower bound counts.
	rangeRe = regexp.MustCompile(`(?i)\b(\d{1,2}(?:\.\d+)?)\s*(?:-|–|—|to)\s*\d{1,2}(?:\.\d+)?\s*\+?\s*(?:years?|yrs?)\b`)
	// "3+ years", "1 year", "2yrs", "1.5+ years"
	numRe = regexp.MustCompile(`(?i)\b(\d{1,2}(?:\.\d+)?)\s*\+?\s*(?:years?|yrs?)\b`)
	// "two years", "one+ year"
	wordRe = regexp.MustCompile(`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten)\s*\+?\s+(?:years?|yrs?)\b`)
	// "one to three years" — the lower bound counts.
	wordRangeRe = regexp.MustCompile(`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten)(?:\s*[-–—]\s*|\s+to\s+)(?:one|two|three|four|five|six|seven|eight|nine|ten)\s*\+?\s+(?:years?|yrs?)\b`)
	// "6 months", "18+ months"
	monthsRe = regexp.MustCompile(`(?i)\b(\d{1,2})\s*\+?\s*months?\b`)
	// Entry-level wording counts as an explicit 0-year requirement.
	entryRe = regexp.MustCompile(`(?i)\bno (?:prior |previous )?experience (?:required|needed|necessary)|entry[\s-]level|\bfreshers?\b|fresh graduates?|new grad(?:s|uates?)?\b|recent graduates?`)
	// "NOT an entry-level position" must not count as entry level.
	negatedRe = regexp.MustCompile(`(?i)\b(?:not|isn'?t|never|no longer)\s+(?:an?\s+)?$`)

	// A year figure only counts as an experience requirement when a word
	// like this appears nearby — filters out "10 years ago", "2 years of
	// runway", "25 days of vacation".
	yearsContextRe = regexp.MustCompile(`(?i)\bexperience|\bexp\b|work(?:ing|ed)?\b|background|track record|hands-on|industry|professional|career`)
	// Month figures are usually onboarding talk ("in your first 3
	// months..."), so they need the word "experience" itself very close.
	monthsContextRe = regexp.MustCompile(`(?i)\bexperience`)
	// "in your first 2 years", "within 6 months" — growth/onboarding
	// schedules, not requirements. Checked against the preceding text.
	scheduleRe = regexp.MustCompile(`(?i)(?:first|within|next|initial|past|last|every)\s*$`)
	// "18 years of age", "founded 12 years ago" — never requirements.
	notRequirementRe = regexp.MustCompile(`(?i)^\s*(?:of age|old\b|ago\b)`)
)

var wordValues = map[string]float64{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
}

func extractMentions(text string) []mention {
	var mentions []mention

	// window is how far (in bytes) from the figure a context word may sit.
	add := func(loc []int, years float64, contextRe *regexp.Regexp, window int) {
		start, end := loc[0], loc[1]
		if notRequirementRe.MatchString(text[end:min(end+12, len(text))]) {
			return
		}
		if contextRe != nil {
			winStart := max(0, start-window)
			winEnd := min(len(text), end+window)
			if !contextRe.MatchString(text[winStart:winEnd]) {
				return
			}
		}
		mentions = append(mentions, mention{minYears: years, snippet: snippet(text, start, end)})
	}

	// onSchedule reports whether the figure follows growth/onboarding
	// wording ("in your first 2 years...") rather than a requirement.
	onSchedule := func(start int) bool {
		return scheduleRe.MatchString(text[max(0, start-12):start])
	}

	for _, loc := range rangeRe.FindAllStringSubmatchIndex(text, -1) {
		if !onSchedule(loc[0]) {
			add(loc, parseNum(text[loc[2]:loc[3]]), yearsContextRe, 100)
		}
	}
	for _, loc := range numRe.FindAllStringSubmatchIndex(text, -1) {
		if !onSchedule(loc[0]) {
			add(loc, parseNum(text[loc[2]:loc[3]]), yearsContextRe, 100)
		}
	}
	for _, loc := range wordRe.FindAllStringSubmatchIndex(text, -1) {
		if !onSchedule(loc[0]) {
			add(loc, wordValues[strings.ToLower(text[loc[2]:loc[3]])], yearsContextRe, 100)
		}
	}
	for _, loc := range wordRangeRe.FindAllStringSubmatchIndex(text, -1) {
		if !onSchedule(loc[0]) {
			add(loc, wordValues[strings.ToLower(text[loc[2]:loc[3]])], yearsContextRe, 100)
		}
	}
	for _, loc := range monthsRe.FindAllStringSubmatchIndex(text, -1) {
		if !onSchedule(loc[0]) {
			add(loc, parseNum(text[loc[2]:loc[3]])/12, monthsContextRe, 40)
		}
	}
	for _, loc := range entryRe.FindAllStringIndex(text, -1) {
		if !negatedRe.MatchString(text[max(0, loc[0]-20):loc[0]]) {
			add(loc, 0, nil, 0)
		}
	}
	return mentions
}

func parseNum(s string) float64 {
	var n float64
	fmt.Sscanf(s, "%f", &n)
	return n
}

// snippet returns the text around [start,end), trimmed to word boundaries
// and collapsed to one line, for use in notifications.
func snippet(text string, start, end int) string {
	s := max(0, start-60)
	e := min(len(text), end+60)
	// Regex indices are byte offsets; don't slice through a multibyte rune.
	for s > 0 && !utf8.RuneStart(text[s]) {
		s--
	}
	for e < len(text) && !utf8.RuneStart(text[e]) {
		e++
	}
	frag := text[s:e]
	if s > 0 {
		if i := strings.IndexAny(frag, " \n"); i >= 0 {
			frag = "…" + frag[i+1:]
		}
	}
	if e < len(text) {
		if i := strings.LastIndexAny(frag, " \n"); i >= 0 {
			frag = frag[:i] + "…"
		}
	}
	return strings.Join(strings.Fields(frag), " ")
}

func fmtYears(y float64) string {
	switch {
	case y == 0:
		return "0 years (entry level)"
	case y < 1:
		return fmt.Sprintf("%.0f months", y*12)
	case y == 1:
		return "~1 year"
	default:
		return fmt.Sprintf("~%.0f years", y)
	}
}
