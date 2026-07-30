package source

// Nation With NaMo publishes one recurring GILP Impact Fellowship opening.
// The main careers page is the authoritative application schedule, while the
// dedicated fellowship page contains the role details. The cohort year is
// part of the stable ID so a new annual intake is reported as a new posting.
//
//	GET https://gilp.nationwithnamo.com/
//	GET https://gilp.nationwithnamo.com/fellowship/

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	nationWithNamoBaseURL       = "https://gilp.nationwithnamo.com"
	nationWithNamoBodyLimit     = 1 << 20
	nationWithNamoTitle         = "GILP: Impact Fellowship"
	nationWithNamoMaxFAQEntries = 32
)

var (
	nationWithNamoCycleRE = regexp.MustCompile(`\b([0-9]{4})-([0-9]{4}) Deadlines\b`)
	nationWithNamoBatchRE = regexp.MustCompile(
		`There are limited openings available for the GILP: Impact Fellowship batch of ([0-9]{4})\.`,
	)
	nationWithNamoDateRE = regexp.MustCompile(
		`^([1-9]|[12][0-9]|3[01])(st|nd|rd|th) ([A-Za-z]+) ([0-9]{4})(?: [A-Za-z]+)?$`,
	)
	nationWithNamoOnclickRE = regexp.MustCompile(
		`(?:^|[;\s])window\.location\.href\s*=\s*['"](?:\./|/)apply\.html['"]\s*;?\s*$`,
	)
	nationWithNamoIST = time.FixedZone("IST", 5*60*60+30*60)
)

func init() {
	Register("nationwithnamo", func(company string, p params.Map, client *http.Client) (Source, error) {
		if len(p) != 0 {
			keys := make([]string, 0, len(p))
			for key := range p {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf(
				"nationwithnamo source accepts no params (got %s)",
				strings.Join(keys, ", "),
			)
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &nationWithNamo{
			company: company,
			base:    nationWithNamoBaseURL,
			client:  client,
			now:     time.Now,
		}, nil
	})
}

type nationWithNamo struct {
	company string
	base    string
	client  *http.Client
	now     func() time.Time
}

type nationWithNamoSchedule struct {
	cohortYear     int
	opensAt        time.Time
	closesAt       time.Time
	open           bool
	hasApplyAction bool
}

func (s *nationWithNamo) Company() string { return s.company }

func (s *nationWithNamo) Fetch(ctx context.Context) ([]model.Job, error) {
	landingURL := strings.TrimRight(s.base, "/") + "/"
	landing, err := fetchNationWithNamoPage(ctx, s.client, landingURL)
	if err != nil {
		return nil, fmt.Errorf("nationwithnamo: careers page: %w", err)
	}
	schedule, err := parseNationWithNamoSchedule(string(landing))
	if err != nil {
		return nil, fmt.Errorf("nationwithnamo: careers page: %w", err)
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if !schedule.open || now.Before(schedule.opensAt) || now.After(schedule.closesAt) {
		return []model.Job{}, nil
	}

	applyURL := strings.TrimRight(s.base, "/") + "/apply.html"
	application, err := fetchNationWithNamoPage(ctx, s.client, applyURL)
	if err != nil {
		return nil, fmt.Errorf("nationwithnamo: application page: %w", err)
	}
	applicationOpen, err := parseNationWithNamoApplication(string(application), applyURL)
	if err != nil {
		return nil, fmt.Errorf("nationwithnamo: application page: %w", err)
	}
	if !applicationOpen {
		return []model.Job{}, nil
	}
	if !schedule.hasApplyAction {
		return nil, fmt.Errorf(
			"nationwithnamo: careers page: open application has no active Apply Now action",
		)
	}

	detailURL := strings.TrimRight(s.base, "/") + "/fellowship/"
	detail, err := fetchNationWithNamoPage(ctx, s.client, detailURL)
	if err != nil {
		return nil, fmt.Errorf("nationwithnamo: fellowship detail: %w", err)
	}
	description, err := parseNationWithNamoDetail(string(detail), schedule)
	if err != nil {
		return nil, fmt.Errorf("nationwithnamo: fellowship detail: %w", err)
	}

	return []model.Job{{
		ID:             fmt.Sprintf("nationwithnamo/gilp-impact-fellowship/%d", schedule.cohortYear),
		Company:        s.company,
		Title:          nationWithNamoTitle,
		Location:       "India (Hybrid)",
		URL:            applyURL,
		EmploymentType: "Fellowship",
		Description:    description,
		PostedAt:       schedule.opensAt,
	}}, nil
}

func fetchNationWithNamoPage(
	ctx context.Context,
	client *http.Client,
	endpoint string,
) ([]byte, error) {
	expected, err := url.Parse(endpoint)
	if err != nil || expected.User != nil ||
		(expected.Scheme != "http" && expected.Scheme != "https") ||
		expected.Host == "" || expected.RawQuery != "" || expected.Fragment != "" ||
		expected.RawPath != "" || expected.Opaque != "" || expected.ForceQuery {
		return nil, fmt.Errorf("invalid first-party URL %q", endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, expected.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := clientWithoutRedirects(client).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil ||
		resp.Request.URL.String() != expected.String() {
		finalURL := "<unknown>"
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		return nil, fmt.Errorf("GET %s: redirected to unexpected URL %q", endpoint, finalURL)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return nil, fmt.Errorf(
			"GET %s: unexpected Content-Type %q",
			endpoint, resp.Header.Get("Content-Type"),
		)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, nationWithNamoBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if len(body) > nationWithNamoBodyLimit {
		return nil, fmt.Errorf(
			"GET %s: response exceeds %d-byte safety limit",
			endpoint, nationWithNamoBodyLimit,
		)
	}
	return body, nil
}

func parseNationWithNamoSchedule(document string) (nationWithNamoSchedule, error) {
	document, err := stripNationWithNamoComments(document)
	if err != nil {
		return nationWithNamoSchedule{}, err
	}
	if err := validateNationWithNamoTitle(document); err != nil {
		return nationWithNamoSchedule{}, err
	}
	hasApplyAction, err := parseNationWithNamoApplyAction(document)
	if err != nil {
		return nationWithNamoSchedule{}, err
	}

	sections := nationWithNamoElementsByID(document, "app-deadlines")
	if len(sections) != 1 || sections[0].tag != "div" {
		return nationWithNamoSchedule{}, fmt.Errorf(
			"expected one div#app-deadlines, found %d",
			len(sections),
		)
	}
	section := sections[0]
	sectionText := cleanHTMLFragment(section.inner)
	sectionFlat := strings.Join(strings.Fields(sectionText), " ")
	if !strings.Contains(sectionFlat, "Application deadlines") ||
		!strings.Contains(
			sectionFlat,
			"Candidates must submit their completed application and latest resume by 12:00 PM IST on the deadline date",
		) {
		return nationWithNamoSchedule{}, fmt.Errorf("application deadline contract is missing")
	}

	cycles := nationWithNamoCycleRE.FindAllStringSubmatch(sectionFlat, -1)
	if len(cycles) != 1 {
		return nationWithNamoSchedule{}, fmt.Errorf(
			"expected one application cycle heading, found %d",
			len(cycles),
		)
	}
	cycleStart, _ := strconv.Atoi(cycles[0][1])
	cycleEnd, _ := strconv.Atoi(cycles[0][2])
	if cycleEnd != cycleStart+1 {
		return nationWithNamoSchedule{}, fmt.Errorf(
			"application cycle %d-%d is not consecutive",
			cycleStart, cycleEnd,
		)
	}

	batches := nationWithNamoBatchRE.FindAllStringSubmatch(sectionFlat, -1)
	if len(batches) != 1 {
		return nationWithNamoSchedule{}, fmt.Errorf(
			"expected one limited-openings cohort declaration, found %d",
			len(batches),
		)
	}
	cohortYear, _ := strconv.Atoi(batches[0][1])
	if cohortYear != cycleEnd {
		return nationWithNamoSchedule{}, fmt.Errorf(
			"cohort year %d does not match application cycle %d-%d",
			cohortYear, cycleStart, cycleEnd,
		)
	}

	calendars := htmlElementsByClass(section.inner, "calendar")
	var labels, values []htmlElement
	for _, calendar := range calendars {
		candidateLabels := htmlElementsByClass(calendar.inner, "days")
		if len(candidateLabels) != 6 {
			continue
		}
		if labels != nil {
			return nationWithNamoSchedule{}, fmt.Errorf(
				"found more than one canonical application calendar",
			)
		}
		labels = candidateLabels
		values = htmlElementsByClass(calendar.inner, "day")
	}
	if labels == nil {
		return nationWithNamoSchedule{}, fmt.Errorf("canonical application calendar is missing")
	}
	if len(values) != len(labels) {
		return nationWithNamoSchedule{}, fmt.Errorf(
			"application calendar has %d labels but %d values",
			len(labels), len(values),
		)
	}
	wantLabels := []string{
		"Status",
		"Applications open",
		"Deadline for application",
		"Shortlist announcement",
		"Interview process",
		"Offer notification",
	}
	for index, want := range wantLabels {
		if got := cleanHTMLFragment(labels[index].inner); got != want {
			return nationWithNamoSchedule{}, fmt.Errorf(
				"application calendar label %d is %q, want %q",
				index, got, want,
			)
		}
		if cleanHTMLFragment(values[index].inner) == "" {
			return nationWithNamoSchedule{}, fmt.Errorf(
				"application calendar value %d is empty",
				index,
			)
		}
	}

	open, err := nationWithNamoOpenStatus(cleanHTMLFragment(values[0].inner))
	if err != nil {
		return nationWithNamoSchedule{}, err
	}
	opensAt, err := parseNationWithNamoDate(cleanHTMLFragment(values[1].inner), 0)
	if err != nil {
		return nationWithNamoSchedule{}, fmt.Errorf("applications-open date: %w", err)
	}
	closesAt, err := parseNationWithNamoDate(cleanHTMLFragment(values[2].inner), 12)
	if err != nil {
		return nationWithNamoSchedule{}, fmt.Errorf("application deadline: %w", err)
	}
	if opensAt.Year() != cycleStart || closesAt.Year() != cycleStart {
		return nationWithNamoSchedule{}, fmt.Errorf(
			"application dates do not match cycle start year %d",
			cycleStart,
		)
	}
	if !closesAt.After(opensAt) {
		return nationWithNamoSchedule{}, fmt.Errorf(
			"application deadline does not follow the opening date",
		)
	}

	return nationWithNamoSchedule{
		cohortYear:     cohortYear,
		opensAt:        opensAt,
		closesAt:       closesAt,
		open:           open,
		hasApplyAction: hasApplyAction,
	}, nil
}

func parseNationWithNamoApplyAction(document string) (bool, error) {
	var applyAnchors []htmlElement
	for _, anchor := range htmlAnchors(document) {
		if strings.EqualFold(cleanHTMLFragment(anchor.inner), "Apply Now") {
			applyAnchors = append(applyAnchors, anchor)
		}
	}
	if len(applyAnchors) == 0 {
		return false, nil
	}
	if len(applyAnchors) != 1 {
		return false, fmt.Errorf("expected at most one active Apply Now action, found %d", len(applyAnchors))
	}
	anchor := applyAnchors[0]
	href := strings.TrimSpace(anchor.attrs["href"])
	switch {
	case href == "javascript:void(0)" || href == "javascript:void(0);":
		if !nationWithNamoOnclickRE.MatchString(anchor.attrs["onclick"]) {
			return false, fmt.Errorf("Apply Now JavaScript action does not target /apply.html")
		}
	default:
		resolved, err := canonicalNationWithNamoFirstPartyURL(
			nationWithNamoBaseURL+"/",
			href,
		)
		if err != nil {
			return false, fmt.Errorf("invalid Apply Now href: %w", err)
		}
		if resolved != nationWithNamoBaseURL+"/apply.html" {
			return false, fmt.Errorf("Apply Now href resolves to %q, want /apply.html", resolved)
		}
	}
	return true, nil
}

func parseNationWithNamoApplication(document, applicationURL string) (bool, error) {
	document, err := stripNationWithNamoComments(document)
	if err != nil {
		return false, err
	}
	containers := htmlElementsByClass(document, "apply_c")
	if len(containers) != 1 || containers[0].tag != "div" {
		return false, fmt.Errorf("expected one div.apply_c application container, found %d", len(containers))
	}
	container := containers[0]
	text := strings.Join(strings.Fields(cleanHTMLFragment(container.inner)), " ")
	forms := nationWithNamoElementsByTag(container.inner, "form")
	closed := ""
	for _, marker := range []string{"All Aplications are Closed", "All Applications are Closed"} {
		if strings.Contains(text, marker) {
			closed = marker
			break
		}
	}
	if closed != "" {
		if text != closed || len(forms) != 0 {
			return false, fmt.Errorf("application page mixes an explicit closed state with an active form")
		}
		return false, nil
	}
	if len(forms) != 1 {
		return false, fmt.Errorf("open application container has %d forms, want 1", len(forms))
	}
	form := forms[0]
	if !strings.EqualFold(strings.TrimSpace(form.attrs["method"]), http.MethodPost) {
		return false, fmt.Errorf("application form method is %q, want POST", form.attrs["method"])
	}
	_, err = canonicalNationWithNamoFirstPartyURL(applicationURL, form.attrs["action"])
	if err != nil {
		return false, fmt.Errorf("invalid application form action: %w", err)
	}
	if !strings.Contains(cleanHTMLFragment(form.inner), nationWithNamoTitle) {
		return false, fmt.Errorf("application form does not identify %q", nationWithNamoTitle)
	}
	var submitButtons []htmlElement
	for _, button := range nationWithNamoElementsByTag(form.inner, "button") {
		buttonType := strings.ToLower(strings.TrimSpace(button.attrs["type"]))
		if buttonType == "" || buttonType == "submit" {
			submitButtons = append(submitButtons, button)
		}
	}
	if len(submitButtons) != 1 {
		return false, fmt.Errorf(
			"application form has %d submit buttons, want 1",
			len(submitButtons),
		)
	}
	submitText := strings.ToLower(cleanHTMLFragment(submitButtons[0].inner))
	if !strings.Contains(submitText, "apply") && !strings.Contains(submitText, "submit") {
		return false, fmt.Errorf("application form submit button has unsupported label %q", submitText)
	}
	return true, nil
}

func canonicalNationWithNamoFirstPartyURL(baseURL, raw string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.User != nil || base.Host == "" ||
		(base.Scheme != "http" && base.Scheme != "https") {
		return "", fmt.Errorf("invalid base URL %q", baseURL)
	}
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || ref.User != nil || ref.Opaque != "" ||
		(ref.Scheme != "" && ref.Scheme != "http" && ref.Scheme != "https") {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	resolved := base.ResolveReference(ref)
	if resolved.User != nil || !strings.EqualFold(resolved.Scheme, base.Scheme) ||
		!strings.EqualFold(resolved.Host, base.Host) {
		return "", fmt.Errorf("URL %q leaves first-party origin %s", raw, baseURL)
	}
	if resolved.Fragment != "" || resolved.RawPath != "" || resolved.Opaque != "" ||
		resolved.ForceQuery {
		return "", fmt.Errorf("URL %q contains unsupported URL components", raw)
	}
	return resolved.String(), nil
}

func parseNationWithNamoDetail(
	document string,
	schedule nationWithNamoSchedule,
) (string, error) {
	document, err := stripNationWithNamoComments(document)
	if err != nil {
		return "", err
	}
	if err := validateNationWithNamoTitle(document); err != nil {
		return "", err
	}

	summaries := htmlElementsByClass(document, "hero_section_p")
	if len(summaries) != 1 || summaries[0].tag != "p" {
		return "", fmt.Errorf("expected one fellowship hero summary, found %d", len(summaries))
	}
	summary := cleanHTMLFragment(summaries[0].inner)
	if !strings.Contains(summary, "structured, campus-based fellowship") ||
		!strings.Contains(summary, "nation-building projects") {
		return "", fmt.Errorf("fellowship hero summary changed or is incomplete")
	}

	workSections := nationWithNamoElementsByID(document, "what_you_work")
	if len(workSections) != 1 || workSections[0].tag != "section" {
		return "", fmt.Errorf(
			"expected one section#what_you_work, found %d",
			len(workSections),
		)
	}
	work := cleanHTMLFragment(workSections[0].inner)
	for _, marker := range []string{
		"Focus on creating meaningful impact",
		"consulting-style bootcamp",
		"Understand India at its core",
		"Consulting residential bootcamp and immersion experiences",
		"Insights built straight from ground up",
		"Mentorship and career acceleration",
		"pre-placement opportunities (PPOs) at Nation with NaMo",
	} {
		if !strings.Contains(work, marker) {
			return "", fmt.Errorf("work-details section omitted %q", marker)
		}
	}

	var faqSections []htmlElement
	for _, section := range nationWithNamoElementsByTag(document, "section") {
		if strings.Contains(cleanHTMLFragment(section.inner), "Frequently asked questions") {
			faqSections = append(faqSections, section)
		}
	}
	if len(faqSections) != 1 {
		return "", fmt.Errorf(
			"expected one Frequently asked questions section, found %d",
			len(faqSections),
		)
	}
	cards := htmlElementsByClass(faqSections[0].inner, "card")
	if len(cards) < 8 || len(cards) > nationWithNamoMaxFAQEntries {
		return "", fmt.Errorf(
			"FAQ entry count %d is outside expected range 8-%d",
			len(cards), nationWithNamoMaxFAQEntries,
		)
	}
	faqParts := make([]string, 0, len(cards))
	questions := make(map[string]struct{}, len(cards))
	for index, card := range cards {
		buttons := htmlElementsByClass(card.inner, "btn-collapse")
		bodies := htmlElementsByClass(card.inner, "card-body")
		if len(buttons) != 1 || buttons[0].tag != "button" ||
			len(bodies) != 1 || bodies[0].tag != "div" {
			return "", fmt.Errorf(
				"FAQ entry %d does not contain exactly one question and answer",
				index,
			)
		}
		question := cleanHTMLFragment(buttons[0].inner)
		answer := cleanHTMLFragment(bodies[0].inner)
		if question == "" || answer == "" {
			return "", fmt.Errorf("FAQ entry %d has an empty question or answer", index)
		}
		if _, duplicate := questions[question]; duplicate {
			return "", fmt.Errorf("duplicate FAQ question %q", question)
		}
		questions[question] = struct{}{}
		faqParts = append(faqParts, question+"\n"+answer)
	}
	for _, required := range []string{
		"Who can apply for the fellowship?",
		"Do I need prior experience in consulting or policy?",
		"Can I pursue this with my academics?",
		"What is the weekly time commitment required for the fellowship?",
		"What is the planned duration for the fellowship?",
		"When will the applications close for the Fellowship?",
		"What are the eligibility criteria for applying to the fellowship?",
		"Will I receive any stipend or financial aid?",
	} {
		if _, ok := questions[required]; !ok {
			return "", fmt.Errorf("required FAQ question %q is missing", required)
		}
	}
	if !strings.Contains(
		strings.Join(faqParts, "\n"),
		"hybrid model",
	) {
		return "", fmt.Errorf("fellowship details omitted the hybrid-work statement")
	}

	window := fmt.Sprintf(
		"Application window: %s through %s (12:00 PM IST deadline). Cohort: %d.",
		schedule.opensAt.Format("2 January 2006"),
		schedule.closesAt.Format("2 January 2006"),
		schedule.cohortYear,
	)
	return joinDescriptionParts(
		summary,
		work,
		strings.Join(faqParts, "\n\n"),
		window,
	), nil
}

func validateNationWithNamoTitle(document string) error {
	headings := htmlElementsByClass(document, "hero_section_heading")
	if len(headings) != 1 || headings[0].tag != "h1" {
		return fmt.Errorf("expected one fellowship hero heading, found %d", len(headings))
	}
	if title := cleanHTMLFragment(headings[0].inner); title != nationWithNamoTitle {
		return fmt.Errorf("fellowship hero heading is %q, want %q", title, nationWithNamoTitle)
	}
	return nil
}

func nationWithNamoOpenStatus(raw string) (bool, error) {
	switch strings.ToLower(strings.Join(strings.Fields(raw), " ")) {
	case "open for all":
		return true, nil
	case "closed", "applications closed", "applications are closed":
		return false, nil
	default:
		return false, fmt.Errorf("unsupported application status %q", raw)
	}
}

func parseNationWithNamoDate(raw string, hour int) (time.Time, error) {
	normalized := strings.Join(strings.Fields(raw), " ")
	match := nationWithNamoDateRE.FindStringSubmatch(normalized)
	if match == nil {
		return time.Time{}, fmt.Errorf("unsupported date %q", raw)
	}
	day, _ := strconv.Atoi(match[1])
	if suffix := match[2]; suffix != nationWithNamoOrdinalSuffix(day) {
		return time.Time{}, fmt.Errorf("invalid ordinal suffix in date %q", raw)
	}
	month, ok := map[string]time.Month{
		"jan": time.January, "january": time.January,
		"feb": time.February, "february": time.February,
		"mar": time.March, "march": time.March,
		"apr": time.April, "april": time.April,
		"may": time.May,
		"jun": time.June, "june": time.June,
		"jul": time.July, "july": time.July,
		"aug": time.August, "august": time.August,
		"sep": time.September, "sept": time.September, "september": time.September,
		"oct": time.October, "october": time.October,
		"nov": time.November, "november": time.November,
		"dec": time.December, "december": time.December,
	}[strings.ToLower(match[3])]
	if !ok {
		return time.Time{}, fmt.Errorf("unsupported month in date %q", raw)
	}
	year, _ := strconv.Atoi(match[4])
	parsed := time.Date(year, month, day, hour, 0, 0, 0, nationWithNamoIST)
	if parsed.Year() != year || parsed.Month() != month || parsed.Day() != day {
		return time.Time{}, fmt.Errorf("invalid calendar date %q", raw)
	}
	return parsed, nil
}

func nationWithNamoOrdinalSuffix(day int) string {
	if day%100 >= 11 && day%100 <= 13 {
		return "th"
	}
	switch day % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}

func stripNationWithNamoComments(document string) (string, error) {
	var cleaned strings.Builder
	for {
		start := strings.Index(document, "<!--")
		if start < 0 {
			cleaned.WriteString(document)
			return cleaned.String(), nil
		}
		cleaned.WriteString(document[:start])
		document = document[start+len("<!--"):]
		end := strings.Index(document, "-->")
		if end < 0 {
			return "", fmt.Errorf("unterminated HTML comment")
		}
		document = document[end+len("-->"):]
	}
}

func nationWithNamoElementsByID(document, id string) []htmlElement {
	var elements []htmlElement
	offset := 0
	for offset < len(document) {
		match := htmlOpenTagRe.FindStringSubmatchIndex(document[offset:])
		if match == nil {
			break
		}
		absolute := nationWithNamoAbsoluteMatch(match, offset)
		openEnd := absolute[1]
		attrs := parseHTMLAttrs(document[absolute[4]:absolute[5]])
		if attrs["id"] != id {
			offset = openEnd
			continue
		}
		tag := strings.ToLower(document[absolute[2]:absolute[3]])
		closeStart, closeEnd, ok := matchingHTMLClose(document, tag, openEnd)
		if ok {
			elements = append(elements, htmlElement{
				tag: tag, attrs: attrs, inner: document[openEnd:closeStart],
				start: absolute[0], end: closeEnd,
			})
		}
		offset = openEnd
	}
	return elements
}

func nationWithNamoElementsByTag(document, wanted string) []htmlElement {
	var elements []htmlElement
	offset := 0
	for offset < len(document) {
		match := htmlOpenTagRe.FindStringSubmatchIndex(document[offset:])
		if match == nil {
			break
		}
		absolute := nationWithNamoAbsoluteMatch(match, offset)
		tag := strings.ToLower(document[absolute[2]:absolute[3]])
		openEnd := absolute[1]
		if wanted != "" && tag != wanted {
			offset = openEnd
			continue
		}
		closeStart, closeEnd, ok := matchingHTMLClose(document, tag, openEnd)
		if !ok {
			offset = openEnd
			continue
		}
		elements = append(elements, htmlElement{
			tag:   tag,
			attrs: parseHTMLAttrs(document[absolute[4]:absolute[5]]),
			inner: document[openEnd:closeStart],
			start: absolute[0],
			end:   closeEnd,
		})
		offset = openEnd
	}
	return elements
}

func nationWithNamoAbsoluteMatch(match []int, offset int) []int {
	absolute := make([]int, len(match))
	for index, value := range match {
		if value >= 0 {
			absolute[index] = offset + value
		} else {
			absolute[index] = -1
		}
	}
	return absolute
}
