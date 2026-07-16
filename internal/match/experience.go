package match

// The "experience" matcher answers: does YOUR experience fall inside the
// range the posting asks for? Each experience mention in the text is
// parsed into an interval:
//
//	"0-1 years"        -> [0, 1]
//	"1 to 3 yrs"       -> [1, 3]
//	"1+ years"         -> [1, ∞)
//	"2 years"          -> [2, ∞)   (a bare figure is a minimum)
//	"up to 2 years"    -> [0, 2]
//	"6 months"         -> [0.5, ∞)
//	"6-12 months"      -> [0.5, 1]
//	"one to three yrs" -> [1, 3]
//	"entry level"      -> [0, ∞)
//
// The job matches when `years` (your experience) is inside ANY mention's
// interval — recall-favoring on multi-requirement postings ("5+ years
// backend, 1+ years Kubernetes" fits a 1-year candidate via the second
// mention). Jobs listing nothing match only when notify_when_unlisted.
//
// Config:
//
//	matcher:
//	  name: experience
//	  params:
//	    years: 1                     # YOUR experience
//	    notify_when_unlisted: false  # also notify when nothing is listed

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
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
		if p.Get("max_years") != "" {
			return nil, fmt.Errorf(`param "max_years" was replaced by "years": set it to YOUR experience, and the job matches when that falls inside the posting's stated range`)
		}
		years, err := p.Float("years", 1)
		if err != nil {
			return nil, err
		}
		unlisted, err := p.Bool("notify_when_unlisted", false)
		if err != nil {
			return nil, err
		}
		return &Experience{Years: years, NotifyWhenUnlisted: unlisted}, nil
	})
}

// Experience matches jobs whose stated experience range contains Years.
// Jobs that don't state one match only if NotifyWhenUnlisted.
type Experience struct {
	Years              float64
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

	for _, m := range mentions {
		if m.lo <= e.Years && e.Years <= m.hi {
			return Result{
				Matched: true,
				Reason:  fmt.Sprintf("asks for %s experience, fits your %s: %q", m.describe(), fmtYears(e.Years), m.snippet),
			}
		}
	}
	// Nothing fits; report the most lenient ask so the user sees how close it was.
	best := mentions[0]
	for _, m := range mentions[1:] {
		if m.lo < best.lo {
			best = m
		}
	}
	return Result{
		Matched: false,
		Reason:  fmt.Sprintf("asks for %s experience, outside your %s: %q", best.describe(), fmtYears(e.Years), best.snippet),
	}
}

// mention is one experience requirement found in the posting text, as the
// interval of candidate experience it accepts. hi is +Inf when the posting
// states only a floor ("3+ years", bare "3 years").
type mention struct {
	lo, hi  float64
	snippet string
}

func (m mention) describe() string {
	if math.IsInf(m.hi, 1) {
		if m.lo > 0 && m.lo < 1 {
			return fmtNum(m.lo*12) + "+ months"
		}
		return fmtNum(m.lo) + "+ years"
	}
	return fmt.Sprintf("%s-%s years", fmtNum(m.lo), fmtNum(m.hi))
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'g', 3, 64) }

func fmtYears(y float64) string {
	if y == 1 {
		return "1 year"
	}
	if y > 0 && y < 1 {
		return fmtNum(y*12) + " months"
	}
	return fmtNum(y) + " years"
}

const num = `(\d{1,2}(?:\.\d+)?)` // e.g. 3, 0.5, 12

var (
	// Word numbers are normalized to digits before parsing, so one set of
	// digit patterns covers "one to three years", "up to two years",
	// "between one and three years"... Snippets show the normalized text.
	wordNumRe  = regexp.MustCompile(`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten)\b`)
	wordDigits = map[string]string{
		"one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
		"six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10",
	}

	// Specific patterns run first and claim their text span, so the
	// general patterns below can't re-parse a fragment of them (the "1"
	// inside "0-1 years" must not also become a bare "1+ years" floor).
	betweenRe   = regexp.MustCompile(`(?i)\bbetween\s+` + num + `\s+and\s+` + num + `\s*\+?\s*(?:years?|yrs?)\b`)
	rangeRe     = regexp.MustCompile(`(?i)\b` + num + `\s*(?:-|–|—|to)\s*` + num + `\s*(\+)?\s*(?:years?|yrs?)\b`)
	upToRe      = regexp.MustCompile(`(?i)\b(?:up to|at most|no more than|less than|fewer than|max(?:imum)?(?: of)?)\s+` + num + `\s*(?:years?|yrs?)\b`)
	monthsRange = regexp.MustCompile(`(?i)\b` + num + `\s*(?:-|–|—|to)\s*` + num + `\s*\+?\s*months?\b`)

	// General floor patterns: "3+ years", "3 years", "6 months".
	numRe    = regexp.MustCompile(`(?i)\b` + num + `\s*\+?\s*(?:years?|yrs?)\b`)
	monthsRe = regexp.MustCompile(`(?i)\b` + num + `\s*\+?\s*months?\b`)

	// Entry-level wording accepts anyone: [0, ∞).
	entryRe = regexp.MustCompile(`(?i)\bno (?:prior |previous )?experience (?:required|needed|necessary)|entry[\s-]level|\bfreshers?\b|fresh graduates?|new grad(?:s|uates?)?\b|recent graduates?`)
	// "NOT an entry-level position", "No freshers", "not for new grads",
	// "isn’t entry-level" — negations kill the entry mention. Up to two
	// short words may sit between the negator and the term.
	negatedRe = regexp.MustCompile(`(?i)\b(?:not|no|isn['’]?t|never|no longer)[\s,]+(?:\w+[\s-]+){0,2}$`)

	// A year figure only counts as an experience requirement when a word
	// like this appears nearby IN THE SAME SENTENCE — filters out "10
	// years ago", "2 years of runway", "25 days of vacation".
	yearsContextRe = regexp.MustCompile(`(?i)\bexperience|\bexp\b|\bwork(?:ing|ed)?\b|\bbackground|\btrack record|\bhands-on|\bindustry\b|\bprofessional|\bcareer`)
	// Month figures are usually onboarding/benefits talk, so they need
	// the word "experience" itself (not "experienced") very close.
	monthsContextRe = regexp.MustCompile(`(?i)\bexperience\b`)
	// Preceding-text guard: growth/onboarding schedules ("in your first 2
	// years") and durations of something else ("visas valid for 1 year").
	preGuardRe = regexp.MustCompile(`(?i)(?:first|within|next|initial|past|last|every|valid\s+for|expires?\s+in)\s*$`)
	// Following-text guard: age limits, company history ("10 years we
	// have..."), funding/benefits durations — never requirements.
	postGuardRe = regexp.MustCompile(`(?i)^['’s]*\s*(?:of age|or older|and older|old\b|ago\b|we\b|our\b|warranty|guarantee|of\s+(?:runway|funding|revenue|onboarding|training|probation|notice)\b|(?:of\s+)?(?:paid\s+)?(?:parental|maternity|paternity)\b)`)
)

// sentenceBounds returns the bounds of the sentence containing [start,end).
func sentenceBounds(text string, start, end int) (int, int) {
	s := strings.LastIndexAny(text[:start], ".!?;\n")
	if s < 0 {
		s = 0
	} else {
		s++
	}
	e := strings.IndexAny(text[end:], ".!?;\n")
	if e < 0 {
		e = len(text)
	} else {
		e += end
	}
	return s, e
}

func extractMentions(original string) []mention {
	text := wordNumRe.ReplaceAllStringFunc(original, func(w string) string {
		return wordDigits[strings.ToLower(w)]
	})

	var mentions []mention
	var claimed [][2]int

	overlapsClaimed := func(start, end int) bool {
		for _, c := range claimed {
			if start < c[1] && end > c[0] {
				return true
			}
		}
		return false
	}

	// guarded reports whether surrounding text marks the figure as
	// something other than an experience requirement.
	guarded := func(start, end int) bool {
		return preGuardRe.MatchString(text[max(0, start-16):start]) ||
			postGuardRe.MatchString(text[end:min(end+28, len(text))])
	}

	// inContext requires a context word near the figure without crossing
	// a sentence boundary — "experience" in the next sentence must not
	// legitimize a benefits figure in this one.
	inContext := func(start, end, window int, re *regexp.Regexp) bool {
		sentStart, sentEnd := sentenceBounds(text, start, end)
		winStart := max(sentStart, start-window)
		winEnd := min(sentEnd, end+window)
		return re.MatchString(text[winStart:winEnd])
	}

	// add claims the span and, when the guards pass, records the mention.
	// Claiming even on guard rejection stops general patterns from
	// re-parsing fragments of an expression that was recognized but ruled
	// out (e.g. the "2 years" inside a rejected "first 0-2 years").
	add := func(loc []int, lo, hi float64, contextRe *regexp.Regexp, window int) {
		start, end := loc[0], loc[1]
		claimed = append(claimed, [2]int{start, end})
		if guarded(start, end) {
			return
		}
		if contextRe != nil && !inContext(start, end, window, contextRe) {
			return
		}
		if hi < lo { // "3-1 years" is noise, not a range
			return
		}
		mentions = append(mentions, mention{lo: lo, hi: hi, snippet: snippet(text, start, end)})
	}

	inf := math.Inf(1)

	// Specific patterns first (they claim their spans)...
	for _, loc := range betweenRe.FindAllStringSubmatchIndex(text, -1) {
		add(loc, parseNum(text[loc[2]:loc[3]]), parseNum(text[loc[4]:loc[5]]), yearsContextRe, 100)
	}
	for _, loc := range rangeRe.FindAllStringSubmatchIndex(text, -1) {
		if overlapsClaimed(loc[0], loc[1]) {
			continue
		}
		hi := parseNum(text[loc[4]:loc[5]])
		if loc[6] >= 0 { // trailing "+": "2-3+ years" states only a floor
			hi = inf
		}
		add(loc, parseNum(text[loc[2]:loc[3]]), hi, yearsContextRe, 100)
	}
	for _, loc := range monthsRange.FindAllStringSubmatchIndex(text, -1) {
		add(loc, parseNum(text[loc[2]:loc[3]])/12, parseNum(text[loc[4]:loc[5]])/12, monthsContextRe, 40)
	}
	for _, loc := range upToRe.FindAllStringSubmatchIndex(text, -1) {
		if overlapsClaimed(loc[0], loc[1]) {
			continue
		}
		add(loc, 0, parseNum(text[loc[2]:loc[3]]), yearsContextRe, 100)
	}

	// ...then general floor patterns, skipping claimed text.
	for _, loc := range numRe.FindAllStringSubmatchIndex(text, -1) {
		if overlapsClaimed(loc[0], loc[1]) {
			continue
		}
		add(loc, parseNum(text[loc[2]:loc[3]]), inf, yearsContextRe, 100)
	}
	for _, loc := range monthsRe.FindAllStringSubmatchIndex(text, -1) {
		if overlapsClaimed(loc[0], loc[1]) {
			continue
		}
		add(loc, parseNum(text[loc[2]:loc[3]])/12, inf, monthsContextRe, 40)
	}

	for _, loc := range entryRe.FindAllStringIndex(text, -1) {
		if overlapsClaimed(loc[0], loc[1]) || negatedRe.MatchString(text[max(0, loc[0]-30):loc[0]]) {
			continue
		}
		claimed = append(claimed, [2]int{loc[0], loc[1]})
		mentions = append(mentions, mention{lo: 0, hi: inf, snippet: snippet(text, loc[0], loc[1])})
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
