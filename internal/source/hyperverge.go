package source

// HyperVerge publishes every current opening in the server-rendered
// department panes on its first-party careers page. Some roles appear in
// more than one pane, and applications are hosted by LinkedIn or Google
// Forms, so this source validates and canonicalizes those links before
// deduplicating postings.
//
//	GET https://hyperverge.co/careers/
//	GET https://www.linkedin.com/jobs-guest/jobs/api/jobPosting/{id}
//	GET https://docs.google.com/forms/d/e/{id}/viewform

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
	"sync"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	hyperVergeBodyLimit              = 2 << 20
	hyperVergeLinkedInBodyLimit      = 1 << 20
	hyperVergeGoogleFormBodyLimit    = 512 << 10
	hyperVergeMaxDescriptionLength   = 256 << 10
	hyperVergeLinkedInConcurrency    = 4
	hyperVergeMaxJobs                = 1_000
	hyperVergeMaxPanes               = 64
	hyperVergeLinkedInGuestBase      = "https://www.linkedin.com/jobs-guest/jobs/api/jobPosting"
	hyperVergeGoogleFormsBase        = "https://docs.google.com/forms/d/e"
	hyperVergeClosedApplicationsText = "No longer accepting applications"
)

var (
	hyperVergeSelectRE       = regexp.MustCompile(`(?is)<select\b([^>]*)>(.*?)</select>`)
	hyperVergeOptionRE       = regexp.MustCompile(`(?is)<option\b([^>]*)>(.*?)</option>`)
	hyperVergeSpanRE         = regexp.MustCompile(`(?is)<span\b[^>]*>(.*?)</span>`)
	hyperVergePaneIDRE       = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	hyperVergeLinkedIn       = regexp.MustCompile(`^/jobs/view/([1-9][0-9]{5,})/?$`)
	hyperVergeFormPath       = regexp.MustCompile(`^/forms/d/e/([A-Za-z0-9_-]{20,})/viewform/?$`)
	hyperVergeLinkedInJobID  = regexp.MustCompile(`^hyperverge/linkedin/([1-9][0-9]{5,})$`)
	hyperVergeTitleTokenRE   = regexp.MustCompile(`[a-z0-9]+`)
	hyperVergeGooglePopupURL = regexp.MustCompile(
		`(?is)\bdata-popup-url\s*=\s*(?:"([^"]+)"|'([^']+)'|([^\s>]+))`,
	)
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
			company:         company,
			base:            "https://hyperverge.co",
			linkedinBase:    hyperVergeLinkedInGuestBase,
			googleFormsBase: hyperVergeGoogleFormsBase,
			client:          client,
		}, nil
	})
}

type hyperVerge struct {
	company         string
	base            string
	linkedinBase    string
	googleFormsBase string
	client          *http.Client
}

func (s *hyperVerge) Company() string { return s.company }

func (s *hyperVerge) Fetch(ctx context.Context) ([]model.Job, error) {
	endpoint := s.base + "/careers/"
	body, err := fetchHyperVergePage(ctx, s.client, endpoint)
	if err != nil {
		return nil, err
	}
	candidates, err := s.parseCareersPage(string(body))
	if err != nil {
		return nil, fmt.Errorf("hyperverge: parsing %s: %w", endpoint, err)
	}
	jobs, err := s.actionableJobs(ctx, candidates)
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *hyperVerge) actionableJobs(
	ctx context.Context,
	candidates []model.Job,
) ([]model.Job, error) {
	linkedInIndexes := make([]int, 0, len(candidates))
	seenForms := make(map[string]struct{})
	for index := range candidates {
		job := candidates[index]
		switch {
		case strings.HasPrefix(job.ID, "hyperverge/linkedin/"):
			if !hyperVergeLinkedInJobID.MatchString(job.ID) {
				return nil, fmt.Errorf("hyperverge: malformed LinkedIn candidate id %q", job.ID)
			}
			linkedInIndexes = append(linkedInIndexes, index)
		case strings.HasPrefix(job.ID, "hyperverge/google-form/"):
			formID, err := hyperVergeGoogleFormID(job)
			if err != nil {
				return nil, fmt.Errorf("hyperverge: Google Form candidate %q: %w", job.ID, err)
			}
			if _, duplicate := seenForms[formID]; duplicate {
				continue
			}
			seenForms[formID] = struct{}{}
			if err := s.validateGoogleFormBoundary(ctx, formID, job.URL); err != nil {
				return nil, fmt.Errorf("hyperverge: Google Form %s: %w", formID, err)
			}
			return nil, fmt.Errorf(
				"hyperverge: Google Form %s requires sign-in, so its role description cannot be hydrated",
				formID,
			)
		default:
			return nil, fmt.Errorf("hyperverge: candidate has unsupported id %q", job.ID)
		}
	}

	type detailResult struct {
		index      int
		job        model.Job
		actionable bool
		err        error
	}
	tasks := make(chan int, len(linkedInIndexes))
	results := make(chan detailResult, len(linkedInIndexes))
	for _, index := range linkedInIndexes {
		tasks <- index
	}
	close(tasks)

	workers := hyperVergeLinkedInConcurrency
	if workers > len(linkedInIndexes) {
		workers = len(linkedInIndexes)
	}
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for range workers {
		go func() {
			defer workersDone.Done()
			for index := range tasks {
				job, actionable, err := s.fetchLinkedInDetail(ctx, candidates[index])
				results <- detailResult{
					index: index, job: job, actionable: actionable, err: err,
				}
			}
		}()
	}
	go func() {
		workersDone.Wait()
		close(results)
	}()

	hydrated := make(map[int]model.Job, len(linkedInIndexes))
	errorsByIndex := make(map[int]error)
	for result := range results {
		if result.err != nil {
			errorsByIndex[result.index] = result.err
			continue
		}
		if result.actionable {
			hydrated[result.index] = result.job
		}
	}
	for _, index := range linkedInIndexes {
		if err := errorsByIndex[index]; err != nil {
			return nil, fmt.Errorf(
				"hyperverge: LinkedIn detail for %s: %w",
				candidates[index].ID,
				err,
			)
		}
	}

	jobs := make([]model.Job, 0, len(hydrated))
	for index := range candidates {
		if job, ok := hydrated[index]; ok {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

func (s *hyperVerge) fetchLinkedInDetail(
	ctx context.Context,
	job model.Job,
) (model.Job, bool, error) {
	match := hyperVergeLinkedInJobID.FindStringSubmatch(job.ID)
	if match == nil {
		return model.Job{}, false, fmt.Errorf("invalid job id %q", job.ID)
	}
	base := strings.TrimRight(s.linkedinBase, "/")
	if base == "" {
		base = hyperVergeLinkedInGuestBase
	}
	endpoint := base + "/" + match[1]
	body, _, err := fetchHyperVergeBoundaryPage(
		ctx,
		s.client,
		endpoint,
		hyperVergeLinkedInBodyLimit,
		http.StatusOK,
	)
	if err != nil {
		return model.Job{}, false, err
	}
	return parseHyperVergeLinkedInDetail(job, match[1], string(body))
}

func parseHyperVergeLinkedInDetail(
	job model.Job,
	externalID string,
	document string,
) (model.Job, bool, error) {
	document, err := stripHyperVergeComments(document)
	if err != nil {
		return model.Job{}, false, err
	}

	titleLinks := htmlElementsByClass(document, "topcard__link")
	titles := htmlElementsByClass(document, "topcard__title")
	if len(titleLinks) != 1 || titleLinks[0].tag != "a" ||
		len(titles) != 1 || titles[0].tag != "h2" {
		return model.Job{}, false, fmt.Errorf(
			"expected one LinkedIn top-card title link and heading, found %d/%d",
			len(titleLinks),
			len(titles),
		)
	}
	detailTitle := cleanHTMLFragment(titles[0].inner)
	if detailTitle == "" || len(detailTitle) > 300 {
		return model.Job{}, false, fmt.Errorf("invalid LinkedIn title length %d", len(detailTitle))
	}
	if err := validateHyperVergeLinkedInJobURL(titleLinks[0].attrs["href"], externalID); err != nil {
		return model.Job{}, false, err
	}

	organizations := htmlElementsByClass(document, "topcard__org-name-link")
	if len(organizations) != 1 || organizations[0].tag != "a" {
		return model.Job{}, false, fmt.Errorf(
			"expected one LinkedIn organization link, found %d",
			len(organizations),
		)
	}
	if organization := cleanHTMLFragment(organizations[0].inner); organization != "HyperVerge" {
		return model.Job{}, false, fmt.Errorf(
			"LinkedIn organization is %q, want HyperVerge",
			organization,
		)
	}
	if err := validateHyperVergeLinkedInOrganizationURL(organizations[0].attrs["href"]); err != nil {
		return model.Job{}, false, err
	}

	closedMarkers := htmlElementsByClass(document, "closed-job__flavor--closed")
	applyMarkers := htmlElementsByClass(document, "top-card-layout__cta--primary")
	if len(closedMarkers) != 0 {
		if len(closedMarkers) != 1 ||
			cleanHTMLFragment(closedMarkers[0].inner) != hyperVergeClosedApplicationsText {
			return model.Job{}, false, fmt.Errorf("invalid explicit closed-job marker")
		}
		if len(applyMarkers) != 0 {
			return model.Job{}, false, fmt.Errorf(
				"LinkedIn detail mixes a closed-job marker with an active Apply action",
			)
		}
		return model.Job{}, false, nil
	}
	if len(applyMarkers) != 1 || applyMarkers[0].tag != "button" ||
		cleanHTMLFragment(applyMarkers[0].inner) != "Apply" ||
		applyMarkers[0].attrs["data-modal"] != "job-details-topcard-apply-modal" {
		return model.Job{}, false, fmt.Errorf(
			"expected one active LinkedIn Apply action, found %d",
			len(applyMarkers),
		)
	}
	if !hyperVergeTitlesCompatible(job.Title, detailTitle) {
		return model.Job{}, false, fmt.Errorf(
			"LinkedIn title %q is incompatible with first-party title %q",
			detailTitle,
			job.Title,
		)
	}

	descriptions := htmlElementsByClass(document, "description__text")
	if len(descriptions) != 1 || descriptions[0].tag != "div" {
		return model.Job{}, false, fmt.Errorf(
			"expected one LinkedIn description block, found %d",
			len(descriptions),
		)
	}
	markup := htmlElementsByClass(descriptions[0].inner, "show-more-less-html__markup")
	if len(markup) != 1 || markup[0].tag != "div" {
		return model.Job{}, false, fmt.Errorf(
			"expected one full LinkedIn description markup block, found %d",
			len(markup),
		)
	}
	description := cleanHTMLFragment(markup[0].inner)
	if len(description) < 100 || len(description) > hyperVergeMaxDescriptionLength {
		return model.Job{}, false, fmt.Errorf(
			"LinkedIn description length %d is outside expected range",
			len(description),
		)
	}

	job.Title = detailTitle
	job.Description = description
	return job, true, nil
}

func (s *hyperVerge) validateGoogleFormBoundary(
	ctx context.Context,
	formID string,
	canonicalFormURL string,
) error {
	base := strings.TrimRight(s.googleFormsBase, "/")
	if base == "" {
		base = hyperVergeGoogleFormsBase
	}
	endpoint := base + "/" + formID + "/viewform"
	body, status, err := fetchHyperVergeBoundaryPage(
		ctx,
		s.client,
		endpoint,
		hyperVergeGoogleFormBodyLimit,
		http.StatusOK,
		http.StatusUnauthorized,
	)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return fmt.Errorf(
			"form became anonymously accessible; full role identity and description require review",
		)
	}
	document, err := stripHyperVergeComments(string(body))
	if err != nil {
		return err
	}
	roots := htmlElementsByClass(document, "document-root")
	logins := htmlElementsByClass(document, "login")
	if len(roots) != 1 || roots[0].tag != "div" ||
		len(logins) != 1 || logins[0].tag != "div" {
		return fmt.Errorf(
			"401 response omitted the Google Forms login boundary",
		)
	}
	loginText := cleanHTMLFragment(logins[0].inner)
	if !strings.Contains(loginText, "Sign in to your Google Account") ||
		!strings.Contains(loginText, "You must sign in to access this content") {
		return fmt.Errorf("401 response has an unrecognized Google Forms login message")
	}
	buttons := htmlElementsByClass(logins[0].inner, "sign-in-button")
	if len(buttons) != 1 || buttons[0].tag != "button" ||
		cleanHTMLFragment(buttons[0].inner) != "Sign in" {
		return fmt.Errorf("Google Forms login boundary omitted its Sign in action")
	}
	popupURLs := hyperVergeGooglePopupURL.FindAllStringSubmatch(logins[0].inner, -1)
	if len(popupURLs) != 1 {
		return fmt.Errorf("Google Forms login boundary has %d Sign in URLs, want 1", len(popupURLs))
	}
	rawPopupURL := popupURLs[0][1]
	if rawPopupURL == "" {
		rawPopupURL = popupURLs[0][2]
	}
	if rawPopupURL == "" {
		rawPopupURL = popupURLs[0][3]
	}
	if err := validateHyperVergeGoogleLoginURL(
		html.UnescapeString(rawPopupURL),
		canonicalFormURL,
	); err != nil {
		return err
	}
	return nil
}

func hyperVergeGoogleFormID(job model.Job) (string, error) {
	parsed, err := url.Parse(job.URL)
	if err != nil || parsed.User != nil || parsed.Scheme != "https" ||
		parsed.Hostname() != "docs.google.com" || parsed.Port() != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
		parsed.Opaque != "" || parsed.ForceQuery {
		return "", fmt.Errorf("invalid canonical URL %q", job.URL)
	}
	match := hyperVergeFormPath.FindStringSubmatch(parsed.EscapedPath())
	if match == nil {
		return "", fmt.Errorf("invalid canonical URL path %q", parsed.EscapedPath())
	}
	if !strings.HasPrefix(job.ID, "hyperverge/google-form/"+match[1]+"/") {
		return "", fmt.Errorf("id and URL form identifiers differ")
	}
	return match[1], nil
}

func fetchHyperVergePage(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	body, _, err := fetchHyperVergeBoundaryPage(
		ctx,
		client,
		endpoint,
		hyperVergeBodyLimit,
		http.StatusOK,
	)
	return body, err
}

func fetchHyperVergeBoundaryPage(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	limit int64,
	acceptedStatuses ...int,
) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := clientWithoutRedirects(client).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.String() != endpoint {
		finalURL := "<unknown>"
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		return nil, resp.StatusCode, fmt.Errorf(
			"GET %s: redirected to unexpected URL %q",
			endpoint,
			finalURL,
		)
	}
	statusAccepted := false
	for _, accepted := range acceptedStatuses {
		if resp.StatusCode == accepted {
			statusAccepted = true
			break
		}
	}
	if !statusAccepted {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, resp.StatusCode, fmt.Errorf(
			"GET %s: %s: %s",
			endpoint,
			resp.Status,
			bytes.TrimSpace(snippet),
		)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return nil, resp.StatusCode, fmt.Errorf(
			"GET %s: unexpected Content-Type %q",
			endpoint,
			resp.Header.Get("Content-Type"),
		)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if int64(len(body)) > limit {
		return nil, resp.StatusCode, fmt.Errorf(
			"GET %s: response exceeds %d-byte safety limit",
			endpoint,
			limit,
		)
	}
	return body, resp.StatusCode, nil
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

func validateHyperVergeLinkedInJobURL(raw, externalID string) error {
	parsed, err := parseHyperVergeLinkedInURL(raw)
	if err != nil {
		return fmt.Errorf("invalid LinkedIn top-card URL: %w", err)
	}
	path := strings.TrimSuffix(parsed.EscapedPath(), "/")
	const prefix = "/jobs/view/"
	if !strings.HasPrefix(path, prefix) {
		return fmt.Errorf("LinkedIn top-card URL has unexpected path %q", path)
	}
	slug := strings.TrimPrefix(path, prefix)
	if slug != externalID && !strings.HasSuffix(slug, "-"+externalID) {
		return fmt.Errorf(
			"LinkedIn top-card URL does not identify job %s",
			externalID,
		)
	}
	return nil
}

func validateHyperVergeLinkedInOrganizationURL(raw string) error {
	parsed, err := parseHyperVergeLinkedInURL(raw)
	if err != nil {
		return fmt.Errorf("invalid LinkedIn organization URL: %w", err)
	}
	if strings.TrimSuffix(parsed.EscapedPath(), "/") != "/company/hyperverge" {
		return fmt.Errorf(
			"LinkedIn organization URL has unexpected path %q",
			parsed.EscapedPath(),
		)
	}
	return nil
}

func parseHyperVergeLinkedInURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
	if err != nil || parsed.User != nil || parsed.Scheme != "https" ||
		parsed.Port() != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
		parsed.Opaque != "" || parsed.ForceQuery {
		return nil, fmt.Errorf("invalid URL %q", raw)
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "linkedin.com" && !strings.HasSuffix(host, ".linkedin.com") {
		return nil, fmt.Errorf("URL host %q is not LinkedIn", parsed.Hostname())
	}
	return parsed, nil
}

func validateHyperVergeGoogleLoginURL(raw, canonicalFormURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
	if err != nil || parsed.User != nil || parsed.Scheme != "https" ||
		parsed.Hostname() != "accounts.google.com" || parsed.Port() != "" ||
		parsed.EscapedPath() != "/ServiceLogin" || parsed.Fragment != "" ||
		parsed.RawPath != "" || parsed.Opaque != "" || parsed.ForceQuery {
		return fmt.Errorf("invalid Google Forms Sign in URL %q", raw)
	}
	if parsed.Query().Get("continue") != canonicalFormURL {
		return fmt.Errorf("Google Forms Sign in URL does not return to the expected form")
	}
	return nil
}

func hyperVergeTitlesCompatible(firstParty, linkedIn string) bool {
	firstTokens := hyperVergeTitleTokens(firstParty)
	linkedInTokens := hyperVergeTitleTokens(linkedIn)
	if len(firstTokens) == 0 || len(linkedInTokens) == 0 {
		return false
	}
	if strings.Join(firstTokens, " ") == strings.Join(linkedInTokens, " ") {
		return true
	}

	firstSet := make(map[string]struct{}, len(firstTokens))
	linkedInSet := make(map[string]struct{}, len(linkedInTokens))
	for _, token := range firstTokens {
		firstSet[token] = struct{}{}
	}
	for _, token := range linkedInTokens {
		linkedInSet[token] = struct{}{}
	}
	matched := 0
	for token := range firstSet {
		if _, ok := linkedInSet[token]; ok {
			matched++
		}
	}
	if matched < 2 || matched*5 < len(firstSet)*3 {
		return false
	}
	for _, anchor := range []string{
		"administrator", "counsel", "developer", "engineer", "intern",
		"lead", "manager", "strategist", "support",
	} {
		_, firstHas := firstSet[anchor]
		_, linkedInHas := linkedInSet[anchor]
		if firstHas && linkedInHas {
			return true
		}
	}
	return false
}

func hyperVergeTitleTokens(title string) []string {
	raw := hyperVergeTitleTokenRE.FindAllString(strings.ToLower(title), -1)
	seen := make(map[string]struct{}, len(raw))
	tokens := make([]string, 0, len(raw))
	for _, token := range raw {
		switch token {
		case "engineering", "engineers":
			token = "engineer"
		case "developers":
			token = "developer"
		case "interns":
			token = "intern"
		case "managers":
			token = "manager"
		case "reliabilty":
			token = "reliability"
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	return tokens
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
