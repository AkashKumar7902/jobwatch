package source

// Jio's first-party careers portal is an ASP.NET WebForms application. The
// category directory establishes an anonymous session and reports the exact
// number of jobs in every function. Each selected function is then paged by
// carrying the server-signed form state forward. Full descriptions live on
// public detail pages and are loaded lazily.
//
// The default scope intentionally covers Jio's technical functions. Importing
// the whole portal would add more than 30,000 mostly sales/freelancer listings
// and require thousands of stateful requests on every poll.
//
//	GET  https://careers.jio.com/frmJobCategories.aspx?n=1
//	GET  https://careers.jio.com/frmfuncwisejob.aspx?...
//	POST https://careers.jio.com/frmfuncwisejob.aspx?...
//	GET  https://careers.jio.com/frmjobdescription.aspx?...

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	jioDefaultFunctions = "0206,0210,0216,0222"
	jioPageSize         = 10
	jioMaxPages         = 1000
	jioBodyLimit        = 2 << 20
)

var (
	jioFunctionCodeRE = regexp.MustCompile(`^[0-9]{4}$`)
	jioExternalIDRE   = regexp.MustCompile(`\(\s*([1-9][0-9]{4,14})\s*\)\s*$`)
	jioInputRE        = regexp.MustCompile(`(?is)<input\b([^>]*)>`)
	jioSpanRE         = regexp.MustCompile(`(?is)<span\b([^>]*)>(.*?)</span>`)
	jioSelectRE       = regexp.MustCompile(`(?is)<select\b([^>]*)>(.*?)</select>`)
	jioOptionRE       = regexp.MustCompile(`(?is)<option\b([^>]*)>(.*?)</option>`)
	jioPageLabelRE    = regexp.MustCompile(`(?i)^Showing\s+([1-9][0-9]*)\s+of\s+([1-9][0-9]*)\s+Pages$`)
)

func init() {
	Register("jio", func(company string, p params.Map, client *http.Client) (Source, error) {
		for key := range p {
			if key != "functions" && key != "max_postings" {
				return nil, fmt.Errorf("jio source: unsupported param %q", key)
			}
		}
		functions, err := canonicalJioFunctions(p.GetDefault("functions", jioDefaultFunctions))
		if err != nil {
			return nil, err
		}
		maxPostings, err := positiveCappedParam(p, "max_postings", 2000, 20000)
		if err != nil {
			return nil, err
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &jio{
			company:     company,
			base:        "https://careers.jio.com",
			functions:   functions,
			maxPostings: maxPostings,
			client:      client,
		}, nil
	})
}

type jio struct {
	company     string
	base        string
	functions   []string
	maxPostings int
	client      *http.Client
}

type jioCategory struct {
	code  string
	label string
	href  string
	count int
}

type jioPage struct {
	jobs       []model.Job
	form       url.Values
	page       int
	totalPages int
}

func (s *jio) Company() string { return s.company }

func canonicalJioFunctions(raw string) ([]string, error) {
	seen := make(map[string]struct{})
	var functions []string
	for _, part := range strings.Split(raw, ",") {
		code := strings.TrimSpace(part)
		if !jioFunctionCodeRE.MatchString(code) {
			return nil, fmt.Errorf("param %q: expected comma-separated four-digit function codes, got %q", "functions", raw)
		}
		if _, duplicate := seen[code]; duplicate {
			return nil, fmt.Errorf("param %q: duplicate function code %q", "functions", code)
		}
		seen[code] = struct{}{}
		functions = append(functions, code)
	}
	if len(functions) == 0 {
		return nil, fmt.Errorf("param %q: expected at least one function code", "functions")
	}
	sort.Strings(functions)
	return functions, nil
}

func jioIdentityFunctions(p params.Map) string {
	functions, _ := canonicalJioFunctions(p.GetDefault("functions", jioDefaultFunctions))
	return strings.Join(functions, "+")
}

func (s *jio) Fetch(ctx context.Context) ([]model.Job, error) {
	session := make(map[string]*http.Cookie)
	homeURL := strings.TrimRight(s.base, "/") + "/"
	if _, err := s.getHTML(ctx, homeURL, session); err != nil {
		return nil, fmt.Errorf("jio: establish anonymous session: %w", err)
	}
	directoryURL := strings.TrimRight(s.base, "/") + "/frmJobCategories.aspx?n=1"
	body, err := s.getHTML(ctx, directoryURL, session)
	if err != nil {
		return nil, fmt.Errorf("jio: category directory: %w", err)
	}
	categories, err := s.parseCategories(string(body))
	if err != nil {
		return nil, err
	}

	selected := make([]jioCategory, 0, len(s.functions))
	total := 0
	for _, code := range s.functions {
		category, ok := categories[code]
		if !ok {
			return nil, fmt.Errorf("jio: configured function %s is absent from the category directory", code)
		}
		if category.count < 0 {
			return nil, fmt.Errorf("jio: function %s has a negative job count", code)
		}
		if total > s.maxPostings-category.count {
			return nil, fmt.Errorf(
				"jio: selected functions report more than max_postings=%d jobs",
				s.maxPostings,
			)
		}
		total += category.count
		selected = append(selected, category)
	}

	jobs := make([]model.Job, 0, total)
	seen := make(map[string]struct{}, total)
	for _, category := range selected {
		if category.count == 0 {
			continue
		}
		categoryJobs, err := s.fetchCategory(ctx, category, session)
		if err != nil {
			return nil, err
		}
		for _, job := range categoryJobs {
			if _, duplicate := seen[job.ID]; duplicate {
				return nil, fmt.Errorf("jio: duplicate posting %s across selected functions", job.ID)
			}
			seen[job.ID] = struct{}{}
			jobs = append(jobs, job)
		}
	}
	if len(jobs) != total {
		return nil, fmt.Errorf("jio: collected %d jobs, category directory reported %d", len(jobs), total)
	}
	return jobs, nil
}

func (s *jio) parseCategories(document string) (map[string]jioCategory, error) {
	blocks := htmlElementsByClass(document, "list-cont")
	if len(blocks) == 0 {
		return nil, fmt.Errorf("jio: category directory omitted category records")
	}
	categories := make(map[string]jioCategory, len(blocks))
	seenIndexes := make(map[string]struct{}, len(blocks))
	for _, block := range blocks {
		values := make(map[string]string)
		index := ""
		for _, match := range jioInputRE.FindAllStringSubmatch(block.inner, -1) {
			attrs := parseHTMLAttrs(match[1])
			id := attrs["id"]
			for _, field := range []string{"hdfunctioncode_", "hdDescription_"} {
				prefix := "MainContent_rptJoblist_" + field
				if !strings.HasPrefix(id, prefix) {
					continue
				}
				candidateIndex := strings.TrimPrefix(id, prefix)
				if candidateIndex == "" {
					return nil, fmt.Errorf("jio: category input %q has no record index", id)
				}
				if index != "" && index != candidateIndex {
					return nil, fmt.Errorf("jio: category block mixes record indexes %s and %s", index, candidateIndex)
				}
				index = candidateIndex
				values[field] = compactSpaces(html.UnescapeString(attrs["value"]))
			}
		}
		if index == "" {
			continue
		}
		if _, duplicate := seenIndexes[index]; duplicate {
			return nil, fmt.Errorf("jio: duplicate category record index %s", index)
		}
		seenIndexes[index] = struct{}{}

		var href, label, countText string
		for _, anchor := range htmlAnchors(block.inner) {
			if anchor.attrs["id"] != "MainContent_rptJoblist_HRFLink_"+index {
				continue
			}
			href = strings.TrimSpace(anchor.attrs["href"])
			for _, match := range jioSpanRE.FindAllStringSubmatch(anchor.inner, -1) {
				attrs := parseHTMLAttrs(match[1])
				switch attrs["id"] {
				case "MainContent_rptJoblist_lblfunctional_" + index:
					label = cleanHTMLFragment(match[2])
				case "MainContent_rptJoblist_lblfunctionjobCount_" + index:
					countText = cleanHTMLFragment(match[2])
				}
			}
		}
		code := values["hdfunctioncode_"]
		description := values["hdDescription_"]
		if !jioFunctionCodeRE.MatchString(code) || description == "" || href == "" || label == "" || countText == "" {
			return nil, fmt.Errorf("jio: category record %s omitted code, description, link, label, or count", index)
		}
		if description != label {
			return nil, fmt.Errorf("jio: category %s description %q disagrees with label %q", code, description, label)
		}
		count, err := strconv.Atoi(countText)
		if err != nil || count < 0 {
			return nil, fmt.Errorf("jio: category %s has invalid job count %q", code, countText)
		}
		resolved, err := s.validateURL(href, "/frmfuncwisejob.aspx", []string{"desc", "flag", "func"})
		if err != nil {
			return nil, fmt.Errorf("jio: category %s: %w", code, err)
		}
		if _, duplicate := categories[code]; duplicate {
			return nil, fmt.Errorf("jio: duplicate function code %s", code)
		}
		categories[code] = jioCategory{code: code, label: label, href: resolved, count: count}
	}
	if len(categories) == 0 {
		return nil, fmt.Errorf("jio: category directory had no parseable records")
	}
	return categories, nil
}

func (s *jio) fetchCategory(
	ctx context.Context,
	category jioCategory,
	session map[string]*http.Cookie,
) ([]model.Job, error) {
	body, err := s.getHTML(ctx, category.href, session)
	if err != nil {
		return nil, fmt.Errorf("jio: function %s (%s): %w", category.code, category.label, err)
	}
	page, err := s.parsePage(string(body), category, 1)
	if err != nil {
		return nil, err
	}
	jobs := append([]model.Job(nil), page.jobs...)
	for next := 2; next <= page.totalPages; next++ {
		body, err = s.postPage(ctx, category.href, page.form, next, session)
		if err != nil {
			return nil, fmt.Errorf("jio: function %s page %d: %w", category.code, next, err)
		}
		page, err = s.parsePage(string(body), category, next)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, page.jobs...)
	}
	if len(jobs) != category.count {
		return nil, fmt.Errorf(
			"jio: function %s collected %d jobs, directory reported %d",
			category.code, len(jobs), category.count,
		)
	}
	return jobs, nil
}

func (s *jio) parsePage(document string, category jioCategory, expectedPage int) (jioPage, error) {
	if strings.Contains(strings.ToLower(document), "session has timed out") {
		return jioPage{}, fmt.Errorf("jio: function %s page %d: anonymous session expired", category.code, expectedPage)
	}
	pageNumber, totalPages, err := parseJioPager(document)
	if err != nil {
		return jioPage{}, fmt.Errorf("jio: function %s page %d: %w", category.code, expectedPage, err)
	}
	wantPages := (category.count + jioPageSize - 1) / jioPageSize
	if totalPages != wantPages || totalPages > jioMaxPages || pageNumber != expectedPage {
		return jioPage{}, fmt.Errorf(
			"jio: function %s page metadata is %d/%d, want %d/%d",
			category.code, pageNumber, totalPages, expectedPage, wantPages,
		)
	}
	if err := validateJioPageSize(document); err != nil {
		return jioPage{}, fmt.Errorf("jio: function %s page %d: %w", category.code, expectedPage, err)
	}

	locations := jioIndexedSpans(document, "MainContent_lstJoblist_Label2_")
	functions := jioIndexedSpans(document, "MainContent_lstJoblist_lblfunctional_")
	dates := jioIndexedSpans(document, "MainContent_lstJoblist_Label1_")
	anchors := make(map[int]htmlElement)
	for _, match := range htmlOpenTagRe.FindAllStringSubmatchIndex(document, -1) {
		if !strings.EqualFold(document[match[2]:match[3]], "a") {
			continue
		}
		attrs := parseHTMLAttrs(document[match[4]:match[5]])
		id := attrs["id"]
		const prefix = "MainContent_lstJoblist_hylUser_"
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
		if err != nil || index < 0 {
			return jioPage{}, fmt.Errorf("jio: function %s page %d has invalid job index %q", category.code, expectedPage, id)
		}
		if _, duplicate := anchors[index]; duplicate {
			return jioPage{}, fmt.Errorf("jio: function %s page %d duplicates job index %d", category.code, expectedPage, index)
		}
		closeStart, closeEnd, ok := matchingHTMLClose(document, "a", match[1])
		if !ok {
			return jioPage{}, fmt.Errorf("jio: function %s page %d job index %d has no closing anchor", category.code, expectedPage, index)
		}
		anchors[index] = htmlElement{
			tag: "a", attrs: attrs, inner: document[match[1]:closeStart],
			start: match[0], end: closeEnd,
		}
	}
	wantItems := jioPageSize
	if remaining := category.count - (expectedPage-1)*jioPageSize; remaining < wantItems {
		wantItems = remaining
	}
	if wantItems <= 0 || len(anchors) != wantItems || len(locations) != wantItems ||
		len(functions) != wantItems || len(dates) != wantItems {
		return jioPage{}, fmt.Errorf(
			"jio: function %s page %d returned incomplete job fields (%d anchors, %d locations, %d functions, %d dates; want %d)",
			category.code, expectedPage, len(anchors), len(locations), len(functions), len(dates), wantItems,
		)
	}

	jobs := make([]model.Job, 0, wantItems)
	for index := 0; index < wantItems; index++ {
		anchor, ok := anchors[index]
		if !ok {
			return jioPage{}, fmt.Errorf("jio: function %s page %d omitted job index %d", category.code, expectedPage, index)
		}
		title := cleanHTMLFragment(anchor.inner)
		idMatch := jioExternalIDRE.FindStringSubmatch(title)
		if idMatch == nil {
			return jioPage{}, fmt.Errorf("jio: function %s page %d job %d has no stable requisition ID", category.code, expectedPage, index)
		}
		location := compactSpaces(locations[index])
		if location == "" || functions[index] != category.label {
			return jioPage{}, fmt.Errorf("jio: function %s page %d job %d has empty location or mismatched function", category.code, expectedPage, index)
		}
		postedAt, err := time.Parse("02 Jan 2006", dates[index])
		if err != nil {
			return jioPage{}, fmt.Errorf("jio: function %s page %d job %d has invalid date %q", category.code, expectedPage, index, dates[index])
		}
		detailURL, err := s.validateURL(
			anchor.attrs["href"],
			"/frmjobdescription.aspx",
			[]string{"JBTITLE", "funcCode", "jbID"},
		)
		if err != nil {
			return jioPage{}, fmt.Errorf("jio: function %s page %d job %d: %w", category.code, expectedPage, index, err)
		}
		jobs = append(jobs, model.Job{
			ID:       "jio/" + idMatch[1],
			Company:  s.company,
			Title:    title,
			Location: location,
			URL:      detailURL,
			PostedAt: postedAt,
		})
	}
	form, err := parseJioForm(document)
	if err != nil {
		return jioPage{}, fmt.Errorf("jio: function %s page %d: %w", category.code, expectedPage, err)
	}
	return jioPage{jobs: jobs, form: form, page: pageNumber, totalPages: totalPages}, nil
}

func parseJioPager(document string) (int, int, error) {
	label, err := jioUniqueSpan(document, "MainContent_lstJoblist_DataPager1_ctl00_CurrentPageLabel")
	if err != nil {
		label = ""
	}
	match := jioPageLabelRE.FindStringSubmatch(label)
	if match == nil {
		return 0, 0, fmt.Errorf(
			"missing or invalid current-page label %q (job-list=%t categories=%t error-page=%t, %d bytes)",
			label,
			strings.Contains(document, "MainContent_lstJoblist_hylUser_"),
			strings.Contains(document, "MainContent_rptJoblist_hdfunctioncode_"),
			strings.Contains(document, "error.aspx"),
			len(document),
		)
	}
	page, _ := strconv.Atoi(match[1])
	total, _ := strconv.Atoi(match[2])

	foundSelect := false
	for _, selectMatch := range jioSelectRE.FindAllStringSubmatch(document, -1) {
		attrs := parseHTMLAttrs(selectMatch[1])
		if attrs["id"] != "MainContent_lstJoblist_DataPager1_ctl00_PageDropDownList" {
			continue
		}
		if foundSelect {
			return 0, 0, fmt.Errorf("duplicate page selector")
		}
		foundSelect = true
		options := jioOptionRE.FindAllStringSubmatch(selectMatch[2], -1)
		if len(options) != total {
			return 0, 0, fmt.Errorf("page selector has %d options, label reports %d", len(options), total)
		}
		selected := 0
		for index, option := range options {
			optionAttrs := parseHTMLAttrs(option[1])
			value, err := strconv.Atoi(optionAttrs["value"])
			if err != nil || value != index+1 {
				return 0, 0, fmt.Errorf("page selector option %d has invalid value %q", index, optionAttrs["value"])
			}
			if _, ok := optionAttrs["selected"]; ok {
				selected = value
			}
		}
		if selected != page {
			return 0, 0, fmt.Errorf("page selector marks page %d, label reports %d", selected, page)
		}
	}
	if !foundSelect {
		return 0, 0, fmt.Errorf("missing page selector")
	}
	return page, total, nil
}

func validateJioPageSize(document string) error {
	for _, selectMatch := range jioSelectRE.FindAllStringSubmatch(document, -1) {
		attrs := parseHTMLAttrs(selectMatch[1])
		if attrs["id"] != "MainContent_ddlentries" {
			continue
		}
		selected := ""
		for _, option := range jioOptionRE.FindAllStringSubmatch(selectMatch[2], -1) {
			optionAttrs := parseHTMLAttrs(option[1])
			if _, ok := optionAttrs["selected"]; ok {
				selected = optionAttrs["value"]
			}
		}
		if selected != strconv.Itoa(jioPageSize) {
			return fmt.Errorf("page size changed to %q, want %d", selected, jioPageSize)
		}
		return nil
	}
	return fmt.Errorf("missing page-size selector")
}

func jioIndexedSpans(document, prefix string) map[int]string {
	values := make(map[int]string)
	for _, match := range htmlOpenTagRe.FindAllStringSubmatchIndex(document, -1) {
		if !strings.EqualFold(document[match[2]:match[3]], "span") {
			continue
		}
		id := parseHTMLAttrs(document[match[4]:match[5]])["id"]
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
		if err != nil || index < 0 {
			continue
		}
		if _, duplicate := values[index]; duplicate {
			values[index] = ""
			continue
		}
		closeStart, _, ok := matchingHTMLClose(document, "span", match[1])
		if !ok {
			values[index] = ""
			continue
		}
		values[index] = cleanHTMLFragment(document[match[1]:closeStart])
	}
	return values
}

func parseJioForm(document string) (url.Values, error) {
	values := make(url.Values)
	for _, match := range jioInputRE.FindAllStringSubmatch(document, -1) {
		attrs := parseHTMLAttrs(match[1])
		if strings.ToLower(strings.TrimSpace(attrs["type"])) != "hidden" {
			continue
		}
		name := strings.TrimSpace(attrs["name"])
		if name == "" {
			continue
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("duplicate hidden form field %q", name)
		}
		values.Set(name, attrs["value"])
	}
	for _, required := range []string{
		"__VIEWSTATE",
		"__VIEWSTATEGENERATOR",
		"__EVENTVALIDATION",
		"ctl00$hdnToken",
		"ctl00$hdnmaibaap",
	} {
		if strings.TrimSpace(values.Get(required)) == "" {
			return nil, fmt.Errorf("missing signed form field %q", required)
		}
	}
	if _, ok := values["ToolkitScriptManager1_HiddenField"]; !ok {
		return nil, fmt.Errorf("missing hidden form field %q", "ToolkitScriptManager1_HiddenField")
	}
	return values, nil
}

func (s *jio) postPage(
	ctx context.Context,
	endpoint string,
	form url.Values,
	page int,
	session map[string]*http.Cookie,
) ([]byte, error) {
	values := make(url.Values, len(form)+8)
	for key, entries := range form {
		values[key] = append([]string(nil), entries...)
	}
	values.Set("__EVENTTARGET", "ctl00$MainContent$lstJoblist$DataPager1$ctl00$PageDropDownList")
	values.Set("__EVENTARGUMENT", "")
	values.Set("__LASTFOCUS", "")
	values.Set("ctl00$MainContent$RefineYourSearch$txtKeyword", "")
	values.Set("ctl00$MainContent$RefineYourSearch$ddlFunction", "0")
	values.Set("ctl00$MainContent$RefineYourSearch$MultiCheckCombo1", "")
	values.Set("ctl00$MainContent$RefineYourSearch$ddlFreshness", "0")
	values.Set("ctl00$MainContent$ddlentries", strconv.Itoa(jioPageSize))
	values.Set("ctl00$MainContent$lstJoblist$DataPager1$ctl00$PageDropDownList", strconv.Itoa(page))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", endpoint)
	return s.doHTML(req, session)
}

func (s *jio) getHTML(
	ctx context.Context,
	endpoint string,
	session map[string]*http.Cookie,
) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	return s.doHTML(req, session)
}

func (s *jio) doHTML(req *http.Request, session map[string]*http.Cookie) ([]byte, error) {
	for _, cookie := range session {
		req.AddCookie(cookie)
	}
	client := *s.client
	originalCheckRedirect := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != req.URL.Scheme ||
			!strings.EqualFold(next.URL.Host, req.URL.Host) ||
			next.URL.Path != req.URL.Path {
			return fmt.Errorf("redirected to an untrusted Jio page %q", next.URL)
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(next, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	for _, cookie := range resp.Cookies() {
		copy := *cookie
		session[cookie.Name] = &copy
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("%s %s: %s: %s", req.Method, req.URL, resp.Status, bytes.TrimSpace(snippet))
	}
	if resp.Request == nil || resp.Request.URL.Scheme != req.URL.Scheme ||
		!strings.EqualFold(resp.Request.URL.Host, req.URL.Host) ||
		resp.Request.URL.Path != req.URL.Path {
		return nil, fmt.Errorf("%s %s: redirected to unexpected page", req.Method, req.URL)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/html") {
		return nil, fmt.Errorf("%s %s: expected text/html, got %q", req.Method, req.URL, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jioBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading response: %w", req.Method, req.URL, err)
	}
	if len(body) > jioBodyLimit {
		return nil, fmt.Errorf("%s %s: response exceeds %d-byte safety limit", req.Method, req.URL, jioBodyLimit)
	}
	return body, nil
}

func (s *jio) validateURL(raw, wantPath string, wantKeys []string) (string, error) {
	base, err := url.Parse(strings.TrimRight(s.base, "/") + "/")
	if err != nil {
		return "", err
	}
	reference, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
	if err != nil {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	resolved := base.ResolveReference(reference)
	if resolved.Scheme != base.Scheme || !strings.EqualFold(resolved.Host, base.Host) ||
		resolved.User != nil || resolved.Path != wantPath || resolved.Fragment != "" ||
		resolved.ForceQuery {
		return "", fmt.Errorf("URL %q is not a trusted Jio %s URL", raw, wantPath)
	}
	query, err := url.ParseQuery(resolved.RawQuery)
	if err != nil || len(query) != len(wantKeys) {
		return "", fmt.Errorf("URL %q has invalid query parameters", raw)
	}
	for _, key := range wantKeys {
		values, ok := query[key]
		if !ok || len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return "", fmt.Errorf("URL %q omitted query parameter %q", raw, key)
		}
	}
	resolved.Fragment = ""
	return resolved.String(), nil
}

func (s *jio) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("jio: nil job")
	}
	const prefix = "jio/"
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("jio: job id %q does not have prefix %q", job.ID, prefix)
	}
	externalID := strings.TrimPrefix(job.ID, prefix)
	if !regexp.MustCompile(`^[1-9][0-9]{4,14}$`).MatchString(externalID) {
		return fmt.Errorf("jio: job id %q has an invalid requisition ID", job.ID)
	}
	endpoint, err := s.validateURL(job.URL, "/frmjobdescription.aspx", []string{"JBTITLE", "funcCode", "jbID"})
	if err != nil {
		return fmt.Errorf("jio: detail %s: %w", externalID, err)
	}
	body, err := s.getHTML(ctx, endpoint, make(map[string]*http.Cookie))
	if err != nil {
		return fmt.Errorf("jio: detail %s: %w", externalID, err)
	}
	document := string(body)
	title, err := jioUniqueSpan(document, "MainContent_lblJobTitle")
	if err != nil || title != job.Title {
		return fmt.Errorf("jio: detail %s title %q does not match list title %q", externalID, title, job.Title)
	}
	idMatch := jioExternalIDRE.FindStringSubmatch(title)
	if idMatch == nil || idMatch[1] != externalID {
		return fmt.Errorf("jio: detail %s title has a mismatched requisition ID", externalID)
	}
	location, err := jioUniqueSpan(document, "MainContent_lblLoc")
	if err != nil || compactSpaces(location) != job.Location {
		return fmt.Errorf("jio: detail %s location %q does not match list location %q", externalID, location, job.Location)
	}
	dateText, err := jioUniqueSpan(document, "MainContent_lblPostedDate")
	if err != nil {
		return fmt.Errorf("jio: detail %s omitted posted date", externalID)
	}
	postedAt, err := time.Parse("02 Jan 2006", dateText)
	if err != nil || (!job.PostedAt.IsZero() && !postedAt.Equal(job.PostedAt)) {
		return fmt.Errorf("jio: detail %s posted date %q does not match list", externalID, dateText)
	}
	responsibilities, err := jioUniqueSpan(document, "MainContent_lblSummRole")
	if err != nil || strings.TrimSpace(responsibilities) == "" {
		return fmt.Errorf("jio: detail %s omitted job responsibilities", externalID)
	}
	structured, err := extractStructuredJobPosting(document)
	if err != nil {
		return fmt.Errorf("jio: detail %s: %w", externalID, err)
	}
	structuredDate, dateErr := time.Parse("2006-01-02", structured.DatePosted)
	if structured.Title != title || dateErr != nil || !structuredDate.Equal(postedAt) ||
		htmltext.ToText(structured.Description) != responsibilities {
		return fmt.Errorf("jio: detail %s JobPosting data disagrees with visible details", externalID)
	}

	description := "Job Responsibilities:\n" + responsibilities
	for _, field := range []struct {
		id    string
		label string
	}{
		{"MainContent_lblEduReq", "Education Requirement"},
		{"MainContent_lblExpReq", "Experience Requirement"},
		{"MainContent_lblSkill", "Skills & Competencies"},
	} {
		value, fieldErr := jioUniqueSpan(document, field.id)
		if fieldErr == nil && value != "" {
			description += "\n\n" + field.label + ":\n" + value
		}
	}
	job.Description = description
	job.PostedAt = postedAt
	job.URL = endpoint
	return nil
}

func jioUniqueSpan(document, id string) (string, error) {
	found := ""
	count := 0
	for _, match := range htmlOpenTagRe.FindAllStringSubmatchIndex(document, -1) {
		if !strings.EqualFold(document[match[2]:match[3]], "span") ||
			parseHTMLAttrs(document[match[4]:match[5]])["id"] != id {
			continue
		}
		count++
		closeStart, _, ok := matchingHTMLClose(document, "span", match[1])
		if !ok {
			return "", fmt.Errorf("span %q has no closing tag", id)
		}
		found = cleanHTMLFragment(document[match[1]:closeStart])
	}
	if count != 1 {
		return "", fmt.Errorf("span %q occurred %d times", id, count)
	}
	return found, nil
}
