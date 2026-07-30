package source

// HyperVerge publishes every current opening in the server-rendered
// department panes on its first-party careers page. Some roles appear in
// more than one pane, and applications are hosted by LinkedIn or Google
// Forms, so this source validates and canonicalizes those links before
// deduplicating postings.
//
//	GET https://hyperverge.co/careers/

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	hyperVergeBodyLimit = 2 << 20
	hyperVergeMaxJobs   = 1_000
	hyperVergeMaxPanes  = 64
)

var (
	hyperVergeSelectRE = regexp.MustCompile(`(?is)<select\b([^>]*)>(.*?)</select>`)
	hyperVergeOptionRE = regexp.MustCompile(`(?is)<option\b([^>]*)>(.*?)</option>`)
	hyperVergeSpanRE   = regexp.MustCompile(`(?is)<span\b[^>]*>(.*?)</span>`)
	hyperVergePaneIDRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	hyperVergeLinkedIn = regexp.MustCompile(`^/jobs/view/([1-9][0-9]{5,})/?$`)
	hyperVergeFormPath = regexp.MustCompile(`^/forms/d/e/([A-Za-z0-9_-]{20,})/viewform/?$`)
)

func init() {
	Register("hyperverge", func(company string, p params.Map, client *http.Client) (Source, error) {
		if len(p) != 0 {
			keys := make([]string, 0, len(p))
			for key := range p {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("hyperverge source accepts no params (got %s)", strings.Join(keys, ", "))
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &hyperVerge{
			company: company,
			base:    "https://hyperverge.co",
			client:  client,
		}, nil
	})
}

type hyperVerge struct {
	company string
	base    string
	client  *http.Client
}

func (s *hyperVerge) Company() string { return s.company }

func (s *hyperVerge) Fetch(ctx context.Context) ([]model.Job, error) {
	endpoint := s.base + "/careers/"
	body, err := fetchHyperVergePage(ctx, s.client, endpoint)
	if err != nil {
		return nil, err
	}
	jobs, err := s.parseCareersPage(string(body))
	if err != nil {
		return nil, fmt.Errorf("hyperverge: parsing %s: %w", endpoint, err)
	}
	return jobs, nil
}

func fetchHyperVergePage(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return nil, fmt.Errorf("GET %s: unexpected Content-Type %q", endpoint, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, hyperVergeBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if len(body) > hyperVergeBodyLimit {
		return nil, fmt.Errorf("GET %s: response exceeds %d-byte safety limit", endpoint, hyperVergeBodyLimit)
	}
	return body, nil
}

func (s *hyperVerge) parseCareersPage(document string) ([]model.Job, error) {
	document, err := stripHyperVergeComments(document)
	if err != nil {
		return nil, err
	}
	sections := htmlElementsByClass(document, "section-openings")
	if len(sections) != 1 || sections[0].tag != "section" || sections[0].attrs["id"] != "jobOpenings" {
		return nil, fmt.Errorf("expected one section.section-openings#jobOpenings, found %d", len(sections))
	}
	section := sections[0]

	expectedPanes, err := hyperVergeSelectPanes(section.inner)
	if err != nil {
		return nil, err
	}
	tabContents := htmlElementsByClass(section.inner, "tab-content")
	if len(tabContents) != 1 {
		return nil, fmt.Errorf("expected one tab-content element, found %d", len(tabContents))
	}
	panes := htmlElementsByClass(tabContents[0].inner, "tab-pane")
	if len(panes) != len(expectedPanes) {
		return nil, fmt.Errorf(
			"department selector declares %d panes but tab content contains %d",
			len(expectedPanes), len(panes),
		)
	}
	if len(panes) > hyperVergeMaxPanes {
		return nil, fmt.Errorf("department pane count %d exceeds safety limit %d", len(panes), hyperVergeMaxPanes)
	}

	jobs := make([]model.Job, 0)
	seenPanes := make(map[string]struct{}, len(panes))
	seenJobs := make(map[string]model.Job)
	for paneIndex, pane := range panes {
		paneID := strings.TrimSpace(pane.attrs["id"])
		if pane.tag != "div" || pane.attrs["role"] != "tabpanel" || !hyperVergePaneIDRE.MatchString(paneID) {
			return nil, fmt.Errorf("department pane %d has invalid tag, role, or id %q", paneIndex, paneID)
		}
		if _, duplicate := seenPanes[paneID]; duplicate {
			return nil, fmt.Errorf("duplicate department pane %q", paneID)
		}
		seenPanes[paneID] = struct{}{}
		if _, declared := expectedPanes[paneID]; !declared {
			return nil, fmt.Errorf("department pane %q is not declared by the selector", paneID)
		}

		columns := htmlElementsByClass(pane.inner, "job-col")
		noOpenings := htmlElementsByClass(pane.inner, "no-openings")
		if len(columns) == 0 {
			if len(noOpenings) != 1 ||
				cleanHTMLFragment(noOpenings[0].inner) != "Currently no openings under this role" {
				return nil, fmt.Errorf("department pane %q has neither jobs nor an explicit empty state", paneID)
			}
			continue
		}
		if len(noOpenings) != 0 {
			return nil, fmt.Errorf("department pane %q mixes jobs with an empty state", paneID)
		}
		for columnIndex, column := range columns {
			job, err := s.parseJobColumn(column)
			if err != nil {
				return nil, fmt.Errorf("department pane %q job %d: %w", paneID, columnIndex, err)
			}
			if prior, duplicate := seenJobs[job.ID]; duplicate {
				if !sameHyperVergeJob(prior, job) {
					return nil, fmt.Errorf("job id %q is reused with conflicting fields", job.ID)
				}
				continue
			}
			if len(jobs) >= hyperVergeMaxJobs {
				return nil, fmt.Errorf("job count exceeds safety limit %d", hyperVergeMaxJobs)
			}
			seenJobs[job.ID] = job
			jobs = append(jobs, job)
		}
	}
	for paneID := range expectedPanes {
		if _, found := seenPanes[paneID]; !found {
			return nil, fmt.Errorf("department selector pane %q is missing", paneID)
		}
	}
	return jobs, nil
}

func hyperVergeSelectPanes(section string) (map[string]struct{}, error) {
	var selector string
	for _, match := range hyperVergeSelectRE.FindAllStringSubmatch(section, -1) {
		if parseHTMLAttrs(match[1])["id"] != "selectBox" {
			continue
		}
		if selector != "" {
			return nil, fmt.Errorf("found more than one department selector")
		}
		selector = match[2]
	}
	if selector == "" {
		return nil, fmt.Errorf("department selector #selectBox is missing")
	}
	options := hyperVergeOptionRE.FindAllStringSubmatch(selector, -1)
	if len(options) == 0 || len(options) > hyperVergeMaxPanes {
		return nil, fmt.Errorf("department selector has invalid option count %d", len(options))
	}
	panes := make(map[string]struct{}, len(options))
	for index, option := range options {
		value := strings.TrimSpace(parseHTMLAttrs(option[1])["value"])
		label := cleanHTMLFragment(option[2])
		if !hyperVergePaneIDRE.MatchString(value) || label == "" {
			return nil, fmt.Errorf("department option %d has invalid value %q or an empty label", index, value)
		}
		if _, duplicate := panes[value]; duplicate {
			return nil, fmt.Errorf("duplicate department option %q", value)
		}
		panes[value] = struct{}{}
	}
	return panes, nil
}

func (s *hyperVerge) parseJobColumn(column htmlElement) (model.Job, error) {
	titles := htmlElementsByClass(column.inner, "job-title")
	if len(titles) != 1 {
		return model.Job{}, fmt.Errorf("expected one job title, found %d", len(titles))
	}
	title := cleanHTMLFragment(titles[0].inner)
	if title == "" || len(title) > 300 {
		return model.Job{}, fmt.Errorf("invalid title length %d", len(title))
	}

	metadata := htmlElementsByClass(column.inner, "job-meta")
	if len(metadata) != 1 {
		return model.Job{}, fmt.Errorf("expected one job metadata block, found %d", len(metadata))
	}
	spans := hyperVergeSpanRE.FindAllStringSubmatch(metadata[0].inner, -1)
	if len(spans) != 2 {
		return model.Job{}, fmt.Errorf("expected location and employment type spans, found %d", len(spans))
	}
	location := cleanHTMLFragment(spans[0][1])
	employmentType := cleanHTMLFragment(spans[1][1])
	if location == "" || len(location) > 300 {
		return model.Job{}, fmt.Errorf("invalid location length %d", len(location))
	}
	if employmentType == "" || len(employmentType) > 100 {
		return model.Job{}, fmt.Errorf("invalid employment type length %d", len(employmentType))
	}

	var applyAnchors []htmlElement
	for _, anchor := range htmlAnchors(metadata[0].inner) {
		if hasClass(anchor.attrs, "btn") {
			applyAnchors = append(applyAnchors, anchor)
		}
	}
	if len(applyAnchors) != 1 || cleanHTMLFragment(applyAnchors[0].inner) != "Apply Now" {
		return model.Job{}, fmt.Errorf("expected one Apply Now link, found %d", len(applyAnchors))
	}
	applyURL, id, err := canonicalHyperVergeApplyURL(
		applyAnchors[0].attrs["href"], title, location,
	)
	if err != nil {
		return model.Job{}, err
	}

	return model.Job{
		ID:             id,
		Company:        s.company,
		Title:          title,
		Location:       location,
		URL:            applyURL,
		EmploymentType: employmentType,
	}, nil
}

func canonicalHyperVergeApplyURL(raw, title, location string) (string, string, error) {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	const docsBase = "https://docs.google.com"
	if strings.HasPrefix(raw, docsBase) {
		if second := strings.Index(raw[len(docsBase):], "https://"); second >= 0 {
			second += len(docsBase)
			if raw[:second] != raw[second:] {
				return "", "", fmt.Errorf("Google Forms apply URL contains conflicting concatenated URLs")
			}
			raw = raw[:second]
		}
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Scheme != "https" ||
		parsed.Port() != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
		parsed.Opaque != "" || parsed.ForceQuery {
		return "", "", fmt.Errorf("invalid apply URL %q", raw)
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "www.linkedin.com":
		match := hyperVergeLinkedIn.FindStringSubmatch(parsed.EscapedPath())
		if match == nil {
			return "", "", fmt.Errorf("invalid LinkedIn job URL %q", raw)
		}
		return "https://www.linkedin.com/jobs/view/" + match[1] + "/",
			"hyperverge/linkedin/" + match[1], nil
	case "docs.google.com":
		match := hyperVergeFormPath.FindStringSubmatch(parsed.EscapedPath())
		if match == nil {
			return "", "", fmt.Errorf("invalid Google Forms job URL %q", raw)
		}
		formID := match[1]
		stableInput := formID + "\x00" + normalizeHyperVergeIdentityText(title) +
			"\x00" + normalizeHyperVergeIdentityText(location)
		digest := sha256.Sum256([]byte(stableInput))
		return docsBase + "/forms/d/e/" + formID + "/viewform",
			"hyperverge/google-form/" + formID + "/" + hex.EncodeToString(digest[:]), nil
	default:
		return "", "", fmt.Errorf("unsupported apply URL host %q", parsed.Hostname())
	}
}

func normalizeHyperVergeIdentityText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func stripHyperVergeComments(document string) (string, error) {
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

func sameHyperVergeJob(a, b model.Job) bool {
	return a.ID == b.ID &&
		a.Company == b.Company &&
		a.Title == b.Title &&
		a.Location == b.Location &&
		a.URL == b.URL &&
		a.EmploymentType == b.EmploymentType
}
