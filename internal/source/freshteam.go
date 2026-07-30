package source

// Freshteam career portals publish every open role on one server-rendered
// /jobs page. Each card links to a first-party detail page containing a
// schema.org JobPosting; details are fetched lazily.
//
// Config:
//
//	- name: Kaleyra
//	  source: freshteam
//	  params:
//	    company_slug: kaleyra-talent

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var (
	freshteamCompanySlugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	freshteamOpaqueIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{12}$`)
	freshteamListSlugRe    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	freshteamCountRe       = regexp.MustCompile(`(?i)^#\s*([0-9]+)\s+jobs?\b`)
	freshteamWordRe        = regexp.MustCompile(`[^a-z0-9]+`)
)

func init() {
	Register("freshteam", func(company string, p params.Map, client *http.Client) (Source, error) {
		slug, err := p.Require("company_slug")
		if err != nil {
			return nil, err
		}
		if !freshteamCompanySlugRe.MatchString(slug) {
			return nil, fmt.Errorf(
				`param "company_slug": expected a lowercase Freshteam hostname prefix, got %q`,
				slug,
			)
		}
		host := slug + ".freshteam.com"
		return &freshteam{
			company: company,
			slug:    slug,
			host:    host,
			base:    "https://" + host,
			client:  client,
		}, nil
	})
}

type freshteam struct {
	company string
	slug    string
	host    string
	base    string
	client  *http.Client
}

func (s *freshteam) Company() string { return s.company }

func (s *freshteam) Fetch(ctx context.Context) ([]model.Job, error) {
	body, finalURL, status, err := s.fetchPage(ctx, s.base+"/jobs")
	if err != nil {
		return nil, fmt.Errorf("freshteam %s jobs: %w", s.slug, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("freshteam %s jobs: GET /jobs returned %s", s.slug, http.StatusText(status))
	}
	if finalURL.EscapedPath() != "/jobs" || finalURL.RawQuery != "" {
		return nil, fmt.Errorf("freshteam %s jobs: unexpected final URL %q", s.slug, finalURL.String())
	}

	doc := string(body)
	lists := htmlElementsByClass(doc, "job-role-list")
	if len(lists) != 1 {
		return nil, fmt.Errorf("freshteam %s jobs: expected one job-role-list, found %d", s.slug, len(lists))
	}
	if !strings.Contains(strings.ToLower(cleanHTMLFragment(doc)), "open positions") {
		return nil, fmt.Errorf("freshteam %s jobs: missing Open Positions marker", s.slug)
	}
	if marker := freshteamPaginationMarker(doc); marker != "" {
		return nil, fmt.Errorf("freshteam %s jobs: unexpected pagination marker %q", s.slug, marker)
	}

	roleList := lists[0].inner
	titleElements := htmlElementsByClass(roleList, "job-title")
	jobs := make([]model.Job, 0, len(titleElements))
	seen := make(map[string]struct{}, len(titleElements))
	for anchorIndex, anchor := range htmlAnchors(roleList) {
		ref, parseErr := url.Parse(strings.TrimSpace(html.UnescapeString(anchor.attrs["href"])))
		hasTitle := len(htmlElementsByClass(anchor.inner, "job-title")) > 0
		jobShaped := parseErr == nil && strings.HasPrefix(ref.EscapedPath(), "/jobs/")
		if !hasTitle && !jobShaped {
			continue
		}
		if parseErr != nil {
			return nil, fmt.Errorf("freshteam %s jobs card %d: invalid href: %w", s.slug, anchorIndex, parseErr)
		}
		jobURL, opaqueID, err := s.parsePostingURL(anchor.attrs["href"], true)
		if err != nil {
			return nil, fmt.Errorf("freshteam %s jobs card %d: %w", s.slug, anchorIndex, err)
		}
		titleElement, ok := firstHTMLClass(anchor.inner, "job-title")
		if !ok {
			return nil, fmt.Errorf("freshteam %s job %s: missing job-title", s.slug, opaqueID)
		}
		title := compactSpaces(cleanHTMLFragment(titleElement.inner))
		if title == "" {
			return nil, fmt.Errorf("freshteam %s job %s: empty title", s.slug, opaqueID)
		}
		if _, duplicate := seen[opaqueID]; duplicate {
			return nil, fmt.Errorf("freshteam %s jobs: duplicate job ID %q", s.slug, opaqueID)
		}
		seen[opaqueID] = struct{}{}
		location, employmentType := freshteamListFields(anchor)
		jobs = append(jobs, model.Job{
			ID:             fmt.Sprintf("freshteam/%s/%s", s.slug, opaqueID),
			Company:        s.company,
			Title:          title,
			Location:       location,
			URL:            jobURL.String(),
			EmploymentType: employmentType,
		})
	}
	if len(jobs) != len(titleElements) {
		return nil, fmt.Errorf(
			"freshteam %s jobs: parsed %d cards but found %d job titles",
			s.slug, len(jobs), len(titleElements),
		)
	}
	if reported, ok := freshteamReportedCount(doc); ok && reported != len(jobs) {
		return nil, fmt.Errorf(
			"freshteam %s jobs: parsed %d unique jobs but page reported %d",
			s.slug, len(jobs), reported,
		)
	}
	if len(jobs) == 0 && !freshteamEmptyBoard(doc) {
		return nil, fmt.Errorf("freshteam %s jobs: no job cards and no empty-board marker", s.slug)
	}
	return jobs, nil
}

func freshteamListFields(anchor htmlElement) (string, string) {
	location := compactSpaces(anchor.attrs["data-portal-location"])
	var employmentType string
	if block, ok := firstHTMLClass(anchor.inner, "job-location"); ok {
		for _, element := range htmlElementsByClass(block.inner, "paragraph") {
			if !hasClass(element.attrs, "location-info") {
				employmentType = compactSpaces(cleanHTMLFragment(element.inner))
				break
			}
		}
		if info, ok := firstHTMLClass(block.inner, "location-info"); ok {
			lines := nonEmptyLines(cleanHTMLFragment(info.inner))
			if location == "" && len(lines) > 0 {
				location = lines[0]
			}
			if employmentType == "" && len(lines) > 1 {
				employmentType = lines[len(lines)-1]
			}
		}
	}
	return location, employmentType
}

func nonEmptyLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line = compactSpaces(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func freshteamPaginationMarker(doc string) string {
	for _, match := range htmlOpenTagRe.FindAllStringSubmatch(doc, -1) {
		attrs := parseHTMLAttrs(match[2])
		for _, class := range strings.Fields(strings.ToLower(attrs["class"])) {
			switch class {
			case "pagination", "pager", "load-more", "load_more", "next-page", "next_page":
				return class
			}
		}
		for _, rel := range strings.Fields(strings.ToLower(attrs["rel"])) {
			if rel == "next" {
				return "rel=next"
			}
		}
		href := strings.TrimSpace(attrs["href"])
		if href == "" {
			continue
		}
		parsed, err := url.Parse(html.UnescapeString(href))
		if err != nil {
			continue
		}
		for _, key := range []string{"page", "offset", "cursor"} {
			if parsed.Query().Has(key) {
				return key
			}
		}
	}
	return ""
}

func freshteamReportedCount(doc string) (int, bool) {
	for _, match := range htmlOpenTagRe.FindAllStringSubmatch(doc, -1) {
		if !strings.EqualFold(match[1], "meta") {
			continue
		}
		attrs := parseHTMLAttrs(match[2])
		if !strings.EqualFold(strings.TrimSpace(attrs["property"]), "og:description") {
			continue
		}
		count := freshteamCountRe.FindStringSubmatch(compactSpaces(attrs["content"]))
		if count == nil {
			return 0, false
		}
		n, err := strconv.Atoi(count[1])
		return n, err == nil
	}
	return 0, false
}

func freshteamEmptyBoard(doc string) bool {
	lower := strings.ToLower(cleanHTMLFragment(doc))
	return strings.Contains(lower, "no jobs found") ||
		strings.Contains(lower, "no open positions are currently available")
}

func (s *freshteam) Detail(ctx context.Context, job *model.Job) error {
	prefix := "freshteam/" + s.slug + "/"
	if job == nil {
		return fmt.Errorf("freshteam %s: nil job", s.slug)
	}
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("freshteam %s: job ID does not belong to this board", s.slug)
	}
	opaqueID := strings.TrimPrefix(job.ID, prefix)
	if !freshteamOpaqueIDRe.MatchString(opaqueID) {
		return fmt.Errorf("freshteam %s: invalid job ID %q", s.slug, job.ID)
	}
	if job.Company != s.company {
		return fmt.Errorf("freshteam %s job %s: company does not match list record", s.slug, opaqueID)
	}
	detailURL, urlID, err := s.parsePostingURL(job.URL, true)
	if err != nil {
		return fmt.Errorf("freshteam %s job %s: invalid detail URL: %w", s.slug, opaqueID, err)
	}
	if urlID != opaqueID {
		return fmt.Errorf("freshteam %s job %s: detail URL contains job ID %q", s.slug, opaqueID, urlID)
	}

	body, finalURL, status, err := s.fetchPage(ctx, detailURL.String())
	if err != nil {
		return fmt.Errorf("freshteam %s job %s detail: %w", s.slug, opaqueID, err)
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return fmt.Errorf(
			"freshteam %s job %s closed before detail fetch (%s)",
			s.slug, opaqueID, http.StatusText(status),
		)
	}
	if status != http.StatusOK {
		return fmt.Errorf("freshteam %s job %s detail: GET returned %s", s.slug, opaqueID, http.StatusText(status))
	}
	_, finalID, err := s.parsePostingURL(finalURL.String(), false)
	if err != nil || finalID != opaqueID {
		return fmt.Errorf("freshteam %s job %s detail: unexpected final URL %q", s.slug, opaqueID, finalURL.String())
	}

	posting, err := extractFreshteamPosting(string(body))
	if err != nil {
		return fmt.Errorf("freshteam %s job %s detail: %w", s.slug, opaqueID, err)
	}
	canonicalURL, canonicalID, err := s.parsePostingURL(posting.URL, false)
	if err != nil {
		return fmt.Errorf("freshteam %s job %s detail: invalid canonical URL: %w", s.slug, opaqueID, err)
	}
	if canonicalID != opaqueID {
		return fmt.Errorf(
			"freshteam %s job %s detail: canonical URL contains job ID %q",
			s.slug, opaqueID, canonicalID,
		)
	}
	title := compactSpaces(posting.Title)
	if title == "" || title != compactSpaces(job.Title) {
		return fmt.Errorf(
			"freshteam %s job %s detail: schema title %q does not match list title %q",
			s.slug, opaqueID, title, job.Title,
		)
	}
	description := cleanHTMLFragment(posting.Description)
	if description == "" {
		return fmt.Errorf("freshteam %s job %s detail: schema omitted description", s.slug, opaqueID)
	}
	if compactSpaces(posting.HiringOrganization) == "" {
		return fmt.Errorf("freshteam %s job %s detail: schema omitted hiringOrganization", s.slug, opaqueID)
	}
	if !strings.EqualFold(compactSpaces(posting.HiringOrganization), compactSpaces(job.Company)) {
		return fmt.Errorf(
			"freshteam %s job %s detail: hiringOrganization %q does not match company %q",
			s.slug, opaqueID, posting.HiringOrganization, job.Company,
		)
	}
	employmentType := compactSpaces(posting.EmploymentType)
	if employmentType == "" {
		return fmt.Errorf("freshteam %s job %s detail: schema omitted employmentType", s.slug, opaqueID)
	}
	if job.EmploymentType != "" &&
		freshteamComparable(job.EmploymentType) != freshteamComparable(employmentType) {
		return fmt.Errorf(
			"freshteam %s job %s detail: employmentType %q does not match list value %q",
			s.slug, opaqueID, employmentType, job.EmploymentType,
		)
	}
	location := compactSpaces(posting.Location)
	if location == "" {
		return fmt.Errorf("freshteam %s job %s detail: schema omitted jobLocation", s.slug, opaqueID)
	}
	if !freshteamLocationsAgree(job.Location, location) {
		return fmt.Errorf(
			"freshteam %s job %s detail: jobLocation %q does not match list value %q",
			s.slug, opaqueID, location, job.Location,
		)
	}
	postedAt, err := parseFreshteamPostingDate(posting.DatePosted)
	if err != nil {
		return fmt.Errorf("freshteam %s job %s detail: %w", s.slug, opaqueID, err)
	}
	if postedAt.IsZero() {
		return fmt.Errorf("freshteam %s job %s detail: schema omitted datePosted", s.slug, opaqueID)
	}

	updated := *job
	updated.URL = canonicalURL.String()
	updated.Title = title
	updated.Description = description
	updated.EmploymentType = employmentType
	updated.Location = location
	updated.PostedAt = postedAt
	*job = updated
	return nil
}

func freshteamComparable(value string) string {
	return freshteamWordRe.ReplaceAllString(strings.ToLower(value), "")
}

func freshteamLocationsAgree(listValue, schemaValue string) bool {
	listValue = freshteamComparable(listValue)
	schemaValue = freshteamComparable(schemaValue)
	if listValue == "" || strings.Contains(listValue, "remote") ||
		strings.Contains(listValue, "multiplelocations") {
		return true
	}
	return strings.Contains(schemaValue, listValue) || strings.Contains(listValue, schemaValue)
}

func parseFreshteamPostingDate(raw string) (time.Time, error) {
	raw = compactSpaces(raw)
	for _, layout := range []string{
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05 -0700",
	} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, nil
		}
	}
	return parsePostingDate(raw)
}

type freshteamPosting struct {
	URL                string
	Title              string
	Description        string
	DatePosted         string
	EmploymentType     string
	HiringOrganization string
	Location           string
}

func extractFreshteamPosting(doc string) (freshteamPosting, error) {
	for _, match := range jsonLDScript.FindAllStringSubmatch(doc, -1) {
		attrs := parseHTMLAttrs(match[1])
		if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(attrs["type"], ";")[0])); mediaType != "application/ld+json" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(strings.TrimSpace(match[2])), &value); err != nil {
			continue
		}
		object := findJobPostingObject(value)
		if object == nil {
			continue
		}
		return freshteamPosting{
			URL:                jsonString(object["url"]),
			Title:              jsonString(object["title"]),
			Description:        jsonString(object["description"]),
			DatePosted:         jsonString(object["datePosted"]),
			EmploymentType:     jsonStrings(object["employmentType"]),
			HiringOrganization: freshteamOrganizationName(object["hiringOrganization"]),
			Location:           jsonLocation(object),
		}, nil
	}
	return freshteamPosting{}, fmt.Errorf("no JobPosting JSON-LD found")
}

func freshteamOrganizationName(value any) string {
	switch value := value.(type) {
	case string:
		return compactSpaces(html.UnescapeString(value))
	case map[string]any:
		return jsonString(value["name"])
	case []any:
		for _, item := range value {
			if name := freshteamOrganizationName(item); name != "" {
				return name
			}
		}
	}
	return ""
}

func (s *freshteam) parsePostingURL(raw string, requireListSlug bool) (*url.URL, string, error) {
	ref, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
	if err != nil {
		return nil, "", err
	}
	baseURL, _ := url.Parse(s.base)
	resolved := baseURL.ResolveReference(ref)
	if err := s.validateTrustedURL(resolved); err != nil {
		return nil, "", err
	}
	if resolved.RawQuery != "" || resolved.Fragment != "" {
		return nil, "", fmt.Errorf("posting URL must not contain a query or fragment")
	}
	segments := strings.Split(strings.Trim(resolved.EscapedPath(), "/"), "/")
	if len(segments) != 3 || segments[0] != "jobs" ||
		!freshteamOpaqueIDRe.MatchString(segments[1]) || segments[2] == "" {
		return nil, "", fmt.Errorf("invalid Freshteam posting path %q", resolved.EscapedPath())
	}
	if requireListSlug && !freshteamListSlugRe.MatchString(segments[2]) {
		return nil, "", fmt.Errorf("invalid Freshteam posting slug in %q", resolved.EscapedPath())
	}
	return resolved, segments[1], nil
}

func (s *freshteam) validateTrustedURL(candidate *url.URL) error {
	if candidate == nil || candidate.Scheme != "https" || !strings.EqualFold(candidate.Host, s.host) ||
		candidate.User != nil {
		return fmt.Errorf("URL must use HTTPS on %s", s.host)
	}
	return nil
}

func (s *freshteam) fetchPage(ctx context.Context, rawURL string) ([]byte, *url.URL, int, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, 0, err
	}
	if err := s.validateTrustedURL(target); err != nil {
		return nil, nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	guarded := *client
	previousRedirectCheck := client.CheckRedirect
	guarded.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if err := s.validateTrustedURL(next.URL); err != nil {
			return err
		}
		if previousRedirectCheck != nil {
			return previousRedirectCheck(next, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	resp, err := guarded.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	finalURL := target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL
	}
	if err := s.validateTrustedURL(finalURL); err != nil {
		return nil, nil, 0, fmt.Errorf("untrusted final URL: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, finalURL, resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, htmlBodyLimit+1))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("reading response: %w", err)
	}
	if len(body) > htmlBodyLimit {
		return nil, nil, 0, fmt.Errorf("response exceeds %d bytes", htmlBodyLimit)
	}
	return body, finalURL, resp.StatusCode, nil
}
