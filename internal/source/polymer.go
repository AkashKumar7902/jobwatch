package source

// Polymer public job-board API (no auth):
//
//	GET https://api.polymer.co/v1/hire/organizations/{organization_slug}/jobs?page=N
//	GET https://api.polymer.co/v1/hire/organizations/{organization_slug}/jobs/{job_id}
//
// The list endpoint omits the full description, so details are fetched lazily
// only for postings the runner evaluates.
//
// Docs: https://developer.polymer.co/
//
// Config:
//
//	- name: Swym
//	  source: polymer
//	  params:
//	    organization_slug: swym

import (
	"bytes"
	"context"
	"encoding/json"
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
	polymerMaxPages         = 1000
	polymerMaxPostings      = 100000
	polymerMaxResponseBytes = 16 << 20
)

var (
	polymerOrganizationSlugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	polymerHashIDRE           = regexp.MustCompile(`^[A-Za-z0-9]{12}$`)
	polymerJobIDRE            = regexp.MustCompile(`^[1-9][0-9]*$`)
)

func init() {
	Register("polymer", func(company string, p params.Map, client *http.Client) (Source, error) {
		slug, err := p.Require("organization_slug")
		if err != nil {
			return nil, err
		}
		if len(slug) > 100 || !polymerOrganizationSlugRE.MatchString(slug) {
			return nil, fmt.Errorf(
				`param "organization_slug": invalid Polymer organization slug %q`,
				slug,
			)
		}
		if len(p) != 1 {
			keys := make([]string, 0, len(p))
			for key := range p {
				if key != "organization_slug" {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			return nil, fmt.Errorf(
				"polymer source accepts only organization_slug (unexpected: %s)",
				strings.Join(keys, ", "),
			)
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &polymer{
			company: company,
			slug:    slug,
			apiBase: "https://api.polymer.co/v1/hire/organizations/" + slug + "/jobs",
			client:  client,
		}, nil
	})
}

type polymer struct {
	company string
	slug    string
	apiBase string
	client  *http.Client
}

type polymerListPage struct {
	Items *[]polymerListPosting `json:"items"`
	Meta  *polymerListMeta      `json:"meta"`
}

type polymerListMeta struct {
	Total            *int               `json:"total"`
	IsLast           *bool              `json:"is_last"`
	IsFirst          *bool              `json:"is_first"`
	Page             *int               `json:"page"`
	NextPage         polymerNullableInt `json:"next_page"`
	Count            *int               `json:"count"`
	OrganizationName *string            `json:"organization_name"`
}

type polymerNullableInt struct {
	present bool
	valid   bool
	value   int
}

func (n *polymerNullableInt) UnmarshalJSON(data []byte) error {
	n.present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(data, &n.value); err != nil {
		return err
	}
	n.valid = true
	return nil
}

type polymerNullableString struct {
	present bool
	valid   bool
	value   string
}

func (s *polymerNullableString) UnmarshalJSON(data []byte) error {
	s.present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(data, &s.value); err != nil {
		return err
	}
	s.valid = true
	return nil
}

type polymerListPosting struct {
	ID                   *int64 `json:"id"`
	JobID                *int64 `json:"job_id"`
	HashID               string `json:"hash_id"`
	Title                string `json:"title"`
	OrganizationName     string `json:"organization_name"`
	KindPretty           string `json:"kind_pretty"`
	CreatedAt            string `json:"created_at"`
	PublishedAt          string `json:"published_at"`
	CreatedAtTimestamp   *int64 `json:"created_at_timestamp"`
	PublishedAtTimestamp *int64 `json:"published_at_timestamp"`
	DisplayLocation      string `json:"display_location"`
	RemotenessPretty     string `json:"remoteness_pretty"`
	JobPostURL           string `json:"job_post_url"`
}

type polymerDetailPosting struct {
	ID                        *int64                `json:"id"`
	JobID                     *int64                `json:"job_id"`
	HashID                    string                `json:"hash_id"`
	Title                     string                `json:"title"`
	Description               *string               `json:"description"`
	OrganizationName          string                `json:"organization_name"`
	KindPretty                string                `json:"kind_pretty"`
	CreatedAt                 string                `json:"created_at"`
	PublishedAt               string                `json:"published_at"`
	CreatedAtTimestamp        *int64                `json:"created_at_timestamp"`
	PublishedAtTimestamp      *int64                `json:"published_at_timestamp"`
	DisplayLocation           string                `json:"display_location"`
	RemotenessPretty          string                `json:"remoteness_pretty"`
	JobPostURL                string                `json:"job_post_url"`
	JobApplicationDescription *string               `json:"job_application_description_url"`
	ArchivedAt                polymerNullableString `json:"archived_at"`
}

func (s *polymer) Company() string { return s.company }

func (s *polymer) Fetch(ctx context.Context) ([]model.Job, error) {
	var (
		allPostings    []polymerListPosting
		expectedCount  = -1
		expectedPages  = -1
		expectedOrg    string
		expectedOrigin string
	)

	for pageNumber := 1; ; pageNumber++ {
		if pageNumber > polymerMaxPages {
			return nil, fmt.Errorf(
				"polymer %s: pagination exceeded safety limit %d",
				s.slug, polymerMaxPages,
			)
		}

		endpoint := s.apiBase + "?page=" + strconv.Itoa(pageNumber)
		var page polymerListPage
		if err := s.getJSON(ctx, endpoint, &page); err != nil {
			return nil, fmt.Errorf("polymer %s jobs page %d: %w", s.slug, pageNumber, err)
		}
		if page.Items == nil || page.Meta == nil {
			return nil, fmt.Errorf("polymer %s jobs page %d: omitted items or meta", s.slug, pageNumber)
		}
		if err := validatePolymerMeta(page.Meta, pageNumber); err != nil {
			return nil, fmt.Errorf("polymer %s jobs page %d: %w", s.slug, pageNumber, err)
		}

		meta := page.Meta
		organizationName := normalizePolymerText(*meta.OrganizationName)
		if pageNumber == 1 {
			expectedCount = *meta.Count
			expectedPages = *meta.Total
			expectedOrg = organizationName
			if expectedCount > polymerMaxPostings {
				return nil, fmt.Errorf(
					"polymer %s: job count %d exceeds safety limit %d",
					s.slug, expectedCount, polymerMaxPostings,
				)
			}
			if expectedPages > polymerMaxPages {
				return nil, fmt.Errorf(
					"polymer %s: page count %d exceeds safety limit %d",
					s.slug, expectedPages, polymerMaxPages,
				)
			}
			if expectedCount == 0 {
				if expectedPages != 0 && expectedPages != 1 {
					return nil, fmt.Errorf(
						"polymer %s: empty board reported %d pages",
						s.slug, expectedPages,
					)
				}
			} else {
				if expectedPages < 1 || expectedPages > expectedCount {
					return nil, fmt.Errorf(
						"polymer %s: %d jobs is inconsistent with %d pages",
						s.slug, expectedCount, expectedPages,
					)
				}
			}
		} else if *meta.Count != expectedCount ||
			*meta.Total != expectedPages ||
			organizationName != expectedOrg {
			return nil, fmt.Errorf("polymer %s: pagination metadata changed on page %d", s.slug, pageNumber)
		}

		if expectedCount == 0 {
			if len(*page.Items) != 0 || !*meta.IsLast || meta.NextPage.valid {
				return nil, fmt.Errorf("polymer %s: empty-board pagination is inconsistent", s.slug)
			}
			return []model.Job{}, nil
		}
		if len(*page.Items) == 0 {
			return nil, fmt.Errorf("polymer %s: page %d returned no jobs", s.slug, pageNumber)
		}
		if len(allPostings)+len(*page.Items) > expectedCount {
			return nil, fmt.Errorf(
				"polymer %s: collected more than reported job count %d",
				s.slug, expectedCount,
			)
		}
		allPostings = append(allPostings, (*page.Items)...)

		if *meta.IsLast {
			if pageNumber != expectedPages || meta.NextPage.valid {
				return nil, fmt.Errorf("polymer %s: last-page metadata is inconsistent", s.slug)
			}
			break
		}
		if pageNumber >= expectedPages ||
			!meta.NextPage.valid ||
			meta.NextPage.value != pageNumber+1 {
			return nil, fmt.Errorf("polymer %s: next-page metadata is inconsistent on page %d", s.slug, pageNumber)
		}
	}

	if len(allPostings) != expectedCount {
		return nil, fmt.Errorf(
			"polymer %s: collected %d jobs, API reported %d",
			s.slug, len(allPostings), expectedCount,
		)
	}

	jobs := make([]model.Job, 0, len(allPostings))
	seen := make(map[int64]struct{}, len(allPostings))
	for index, posting := range allPostings {
		normalized, origin, err := s.normalizeListPosting(posting, expectedOrg)
		if err != nil {
			return nil, fmt.Errorf("polymer %s job %d: %w", s.slug, index, err)
		}
		if _, duplicate := seen[*posting.ID]; duplicate {
			return nil, fmt.Errorf("polymer %s: duplicate job ID %d", s.slug, *posting.ID)
		}
		seen[*posting.ID] = struct{}{}
		if expectedOrigin == "" {
			expectedOrigin = origin
		} else if origin != expectedOrigin {
			return nil, fmt.Errorf(
				"polymer %s job %d: posting URL origin %q differs from %q",
				s.slug, index, origin, expectedOrigin,
			)
		}
		jobs = append(jobs, normalized)
	}
	return jobs, nil
}

func validatePolymerMeta(meta *polymerListMeta, pageNumber int) error {
	if meta.Total == nil || meta.IsLast == nil || meta.IsFirst == nil ||
		meta.Page == nil || !meta.NextPage.present || meta.Count == nil ||
		meta.OrganizationName == nil {
		return fmt.Errorf("omitted required pagination metadata")
	}
	if *meta.Total < 0 || *meta.Count < 0 {
		return fmt.Errorf("reported negative page or job totals")
	}
	if *meta.Page != pageNumber {
		return fmt.Errorf("reported page %d, want %d", *meta.Page, pageNumber)
	}
	if *meta.IsFirst != (pageNumber == 1) {
		return fmt.Errorf("is_first is inconsistent with page %d", pageNumber)
	}
	if normalizePolymerText(*meta.OrganizationName) == "" {
		return fmt.Errorf("reported an empty organization name")
	}
	if meta.NextPage.valid && meta.NextPage.value <= pageNumber {
		return fmt.Errorf("reported non-forward next page %d", meta.NextPage.value)
	}
	return nil
}

func (s *polymer) normalizeListPosting(
	posting polymerListPosting,
	expectedOrganization string,
) (model.Job, string, error) {
	id, err := validatePolymerPostingIdentity(posting.ID, posting.JobID, posting.HashID)
	if err != nil {
		return model.Job{}, "", err
	}
	title := normalizePolymerText(posting.Title)
	if title == "" {
		return model.Job{}, "", fmt.Errorf("has an empty title")
	}
	organization := normalizePolymerText(posting.OrganizationName)
	if organization == "" || organization != expectedOrganization {
		return model.Job{}, "", fmt.Errorf(
			"organization name %q does not match board organization %q",
			organization, expectedOrganization,
		)
	}
	employmentType := normalizePolymerText(posting.KindPretty)
	if employmentType == "" {
		return model.Job{}, "", fmt.Errorf("has an empty employment type")
	}
	location := polymerLocation(posting.DisplayLocation, posting.RemotenessPretty)
	if location == "" {
		return model.Job{}, "", fmt.Errorf("has neither a display location nor remote status")
	}
	if _, err := validatePolymerTimestamp(
		"created_at", posting.CreatedAt, posting.CreatedAtTimestamp,
	); err != nil {
		return model.Job{}, "", err
	}
	postedAt, err := validatePolymerTimestamp(
		"published_at", posting.PublishedAt, posting.PublishedAtTimestamp,
	)
	if err != nil {
		return model.Job{}, "", err
	}
	jobURL, origin, err := validatePolymerJobURL(posting.JobPostURL, s.slug, id)
	if err != nil {
		return model.Job{}, "", err
	}

	return model.Job{
		ID:             fmt.Sprintf("polymer/%s/%d", s.slug, id),
		Company:        s.company,
		Title:          title,
		Location:       location,
		URL:            jobURL,
		EmploymentType: employmentType,
		PostedAt:       postedAt,
	}, origin, nil
}

func (s *polymer) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("polymer %s: nil job", s.slug)
	}
	prefix := "polymer/" + s.slug + "/"
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("polymer %s: job ID %q does not belong to this board", s.slug, job.ID)
	}
	rawID := strings.TrimPrefix(job.ID, prefix)
	if !polymerJobIDRE.MatchString(rawID) {
		return fmt.Errorf("polymer %s: invalid job ID %q", s.slug, job.ID)
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return fmt.Errorf("polymer %s: invalid job ID %q: %w", s.slug, job.ID, err)
	}
	if job.Company != s.company {
		return fmt.Errorf("polymer %s job %d: company does not match list record", s.slug, id)
	}
	listURL, _, err := validatePolymerJobURL(job.URL, s.slug, id)
	if err != nil {
		return fmt.Errorf("polymer %s job %d: invalid list URL: %w", s.slug, id, err)
	}

	var detail polymerDetailPosting
	endpoint := s.apiBase + "/" + rawID
	if err := s.getJSON(ctx, endpoint, &detail); err != nil {
		return fmt.Errorf("polymer %s job %d detail: %w", s.slug, id, err)
	}
	detailID, err := validatePolymerPostingIdentity(detail.ID, detail.JobID, detail.HashID)
	if err != nil {
		return fmt.Errorf("polymer %s job %d detail: %w", s.slug, id, err)
	}
	if detailID != id {
		return fmt.Errorf(
			"polymer %s job %d detail: returned job ID %d",
			s.slug, id, detailID,
		)
	}
	if !detail.ArchivedAt.present {
		return fmt.Errorf("polymer %s job %d detail: omitted archived_at", s.slug, id)
	}
	if detail.ArchivedAt.valid {
		if strings.TrimSpace(detail.ArchivedAt.value) == "" {
			return fmt.Errorf("polymer %s job %d detail: archived_at is an empty string", s.slug, id)
		}
		return fmt.Errorf("polymer %s job %d detail: posting is archived", s.slug, id)
	}

	title := normalizePolymerText(detail.Title)
	if title == "" {
		return fmt.Errorf("polymer %s job %d detail: empty title", s.slug, id)
	}
	if normalizePolymerText(detail.OrganizationName) == "" {
		return fmt.Errorf("polymer %s job %d detail: empty organization name", s.slug, id)
	}
	employmentType := normalizePolymerText(detail.KindPretty)
	if employmentType == "" {
		return fmt.Errorf("polymer %s job %d detail: empty employment type", s.slug, id)
	}
	location := polymerLocation(detail.DisplayLocation, detail.RemotenessPretty)
	if location == "" {
		return fmt.Errorf("polymer %s job %d detail: empty location and remote status", s.slug, id)
	}
	if detail.Description == nil {
		return fmt.Errorf("polymer %s job %d detail: omitted description", s.slug, id)
	}
	description := htmltext.ToText(*detail.Description)
	if description == "" {
		return fmt.Errorf("polymer %s job %d detail: empty description", s.slug, id)
	}
	if _, err := validatePolymerTimestamp(
		"created_at", detail.CreatedAt, detail.CreatedAtTimestamp,
	); err != nil {
		return fmt.Errorf("polymer %s job %d detail: %w", s.slug, id, err)
	}
	postedAt, err := validatePolymerTimestamp(
		"published_at", detail.PublishedAt, detail.PublishedAtTimestamp,
	)
	if err != nil {
		return fmt.Errorf("polymer %s job %d detail: %w", s.slug, id, err)
	}
	detailURL, _, err := validatePolymerJobURL(detail.JobPostURL, s.slug, id)
	if err != nil {
		return fmt.Errorf("polymer %s job %d detail: %w", s.slug, id, err)
	}
	if detailURL != listURL {
		return fmt.Errorf(
			"polymer %s job %d detail: posting URL %q differs from list URL %q",
			s.slug, id, detailURL, listURL,
		)
	}
	if detail.JobApplicationDescription == nil ||
		strings.TrimSpace(*detail.JobApplicationDescription) == "" {
		return fmt.Errorf(
			"polymer %s job %d detail: omitted job application description URL",
			s.slug, id,
		)
	}
	applicationURL, _, err := validatePolymerJobURL(
		*detail.JobApplicationDescription, s.slug, id,
	)
	if err != nil {
		return fmt.Errorf(
			"polymer %s job %d detail: invalid job application description URL: %w",
			s.slug, id, err,
		)
	}
	if applicationURL != detailURL {
		return fmt.Errorf(
			"polymer %s job %d detail: job application description URL differs from posting URL",
			s.slug, id,
		)
	}

	updated := *job
	updated.Title = title
	updated.Location = location
	updated.URL = detailURL
	updated.EmploymentType = employmentType
	updated.Description = description
	updated.PostedAt = postedAt
	*job = updated
	return nil
}

func validatePolymerPostingIdentity(id, jobID *int64, hashID string) (int64, error) {
	if id == nil || jobID == nil {
		return 0, fmt.Errorf("omitted id or job_id")
	}
	if *id <= 0 || *jobID != *id {
		return 0, fmt.Errorf("id %d and job_id %d are invalid or inconsistent", *id, *jobID)
	}
	if !polymerHashIDRE.MatchString(hashID) {
		return 0, fmt.Errorf("job %d has invalid hash_id %q", *id, hashID)
	}
	return *id, nil
}

func validatePolymerTimestamp(name, raw string, unixSeconds *int64) (time.Time, error) {
	if strings.TrimSpace(raw) == "" || unixSeconds == nil {
		return time.Time{}, fmt.Errorf("omitted %s or %s_timestamp", name, name)
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	if parsed.Unix() != *unixSeconds {
		return time.Time{}, fmt.Errorf(
			"%s %q does not match timestamp %d",
			name, raw, *unixSeconds,
		)
	}
	return parsed, nil
}

func validatePolymerJobURL(raw, slug string, id int64) (string, string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return "", "", fmt.Errorf("job %d has an empty or non-canonical posting URL", id)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("job %d has invalid posting URL %q: %w", id, raw, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Port() != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawPath != "" {
		return "", "", fmt.Errorf("job %d has untrusted posting URL %q", id, raw)
	}
	host := parsed.Hostname()
	if host == "" || parsed.Host != strings.ToLower(host) || !strings.Contains(host, ".") {
		return "", "", fmt.Errorf("job %d has invalid posting URL host %q", id, parsed.Host)
	}
	idPath := "/" + strconv.FormatInt(id, 10)
	wantPath := idPath
	if host == "jobs.polymer.co" {
		wantPath = "/" + slug + idPath
	}
	if parsed.EscapedPath() != wantPath {
		return "", "", fmt.Errorf(
			"job %d posting URL path %q, want %q",
			id, parsed.EscapedPath(), wantPath,
		)
	}
	return parsed.String(), parsed.Scheme + "://" + parsed.Host, nil
}

func polymerLocation(displayLocation, remoteness string) string {
	location := normalizePolymerText(displayLocation)
	if location != "" {
		return location
	}
	return normalizePolymerText(remoteness)
}

func normalizePolymerText(value string) string {
	return compactSpaces(html.UnescapeString(value))
}

func (s *polymer) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := clientWithoutRedirects(s.client).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.Request == nil || resp.Request.URL == nil ||
		resp.Request.URL.String() != req.URL.String() ||
		resp.Request.URL.ForceQuery != req.URL.ForceQuery {
		finalURL := "<unknown>"
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		return fmt.Errorf("unexpected redirect from %q to %q", req.URL.String(), finalURL)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf(
			"GET %s: %s: %s",
			req.URL, resp.Status, bytes.TrimSpace(snippet),
		)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf(
			"GET %s: unexpected Content-Type %q",
			req.URL, resp.Header.Get("Content-Type"),
		)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, polymerMaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("GET %s: reading response: %w", req.URL, err)
	}
	if len(body) > polymerMaxResponseBytes {
		return fmt.Errorf(
			"GET %s: response exceeds %d-byte limit",
			req.URL, polymerMaxResponseBytes,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("GET %s: decoding response: %w", req.URL, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("GET %s: response contains multiple JSON values", req.URL)
		}
		return fmt.Errorf("GET %s: decoding trailing response data: %w", req.URL, err)
	}
	return nil
}
