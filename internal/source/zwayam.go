package source

// Zwayam career sites expose an anonymous multipart search API and a separate
// JSON detail API. Both endpoints expect the first-party site's Origin and
// Referer even though requests are sent to public.zwayam.com.
//
// Config:
//
//	- name: Cult.fit
//	  source: zwayam
//	  params:
//	    domain: careers.cult.fit
//	    company_id: "15470"

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	zwayamAPIBase         = "https://public.zwayam.com"
	zwayamPageSize        = 10
	zwayamMaximumPageSize = 1000
	zwayamMaximumPostings = 100_000
	zwayamBodyLimit       = 8 << 20
)

func init() {
	Register("zwayam", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawDomain, err := p.Require("domain")
		if err != nil {
			return nil, err
		}
		domain, err := normalizeBoardHost(rawDomain)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", "domain", err)
		}
		rawCompanyID, err := p.Require("company_id")
		if err != nil {
			return nil, err
		}
		companyID, companyNumber, err := canonicalZwayamDecimal(rawCompanyID)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", "company_id", err)
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &zwayam{
			company:       company,
			domain:        domain,
			companyID:     companyID,
			companyNumber: companyNumber,
			encodedID:     base64.StdEncoding.EncodeToString([]byte(companyID)),
			keyPrefix:     "zwayam/" + companyID + "/",
			origin:        "https://" + domain,
			apiBase:       zwayamAPIBase,
			client:        client,
		}, nil
	})
}

type zwayam struct {
	company   string
	domain    string
	companyID string
	// keyPrefix excludes the career-site domain: that is the employer's
	// marketing DNS and gets re-pointed on a rebrand, while company_id is
	// Zwayam's own immutable account number.
	// Must stay equal to source.StatePrefix for these params.
	keyPrefix     string // zwayam/{company_id}/
	companyNumber int64
	encodedID     string
	origin        string
	apiBase       string
	client        *http.Client
}

func (s *zwayam) Company() string { return s.company }

type zwayamSearchFilter struct {
	PaginationStartNo int                `json:"paginationStartNo"`
	SelectedCall      string             `json:"selectedCall"`
	SortCriteria      zwayamSortCriteria `json:"sortCriteria"`
	AnyOfTheseWords   string             `json:"anyOfTheseWords"`
}

type zwayamSortCriteria struct {
	Name        string `json:"name"`
	IsAscending bool   `json:"isAscending"`
}

type zwayamSearchResponse struct {
	Code *int              `json:"code"`
	Data *zwayamSearchData `json:"data"`
}

type zwayamSearchData struct {
	Results             *[]*zwayamSearchHit        `json:"data"`
	TotalCount          *int                       `json:"totalCount"`
	HasMoreData         *bool                      `json:"hasMoreData"`
	FacetedSearchConfig *zwayamFacetedSearchConfig `json:"facetedSearchConfig"`
}

type zwayamFacetedSearchConfig struct {
	PaginationHowMuch *stringish `json:"paginationHowMuch"`
}

type zwayamSearchHit struct {
	ID     string              `json:"_id"`
	Source *zwayamSearchSource `json:"_source"`
}

type zwayamSearchSource struct {
	ID                *int64           `json:"id"`
	CompanyID         *int64           `json:"companyId"`
	JobTitle          string           `json:"jobTitle"`
	JobURL            string           `json:"jobUrl"`
	Location          string           `json:"location"`
	LocationSeparated string           `json:"locationSeparatedbySlash"`
	Locations         []zwayamLocation `json:"jobLocationRecord"`
	CreateDate        string           `json:"createDate"`
	Status            *int             `json:"status"`
	DisplayStatus     string           `json:"displayStatus"`
	RequisitionStatus string           `json:"requisitionStatus"`
}

type zwayamLocation struct {
	FormattedLocation string `json:"formattedLocation"`
	Location          string `json:"location"`
	City              string `json:"city"`
	State             string `json:"state"`
	Country           string `json:"country"`
}

type zwayamDetailRequest struct {
	CompanyID      int64  `json:"companyId"`
	JobURL         string `json:"jobUrl"`
	ExternalSource string `json:"externalSource"`
	CampusURL      string `json:"campusUrl"`
}

type zwayamDetailResponse struct {
	ID                   *int64                      `json:"id"`
	CompanyID            *int64                      `json:"companyId"`
	JobURL               string                      `json:"jobUrl"`
	Status               *int                        `json:"status"`
	JobTitle             string                      `json:"jobTitle"`
	Location             string                      `json:"location"`
	CreateDate           string                      `json:"createDate"`
	JobConfigurationData *map[string]json.RawMessage `json:"jobConfigurationData"`
}

func (s *zwayam) Fetch(ctx context.Context) ([]model.Job, error) {
	siteBase, err := s.discoverSiteBase(ctx)
	if err != nil {
		return nil, err
	}

	var jobs []model.Job
	seen := make(map[string]struct{})
	expectedTotal := -1
	expectedPageSize := -1
	for offset := 0; ; {
		response, err := s.search(ctx, offset)
		if err != nil {
			return nil, fmt.Errorf("zwayam %s page at offset %d: %w", s.domain, offset, err)
		}
		if response.Code == nil || *response.Code != http.StatusOK {
			return nil, fmt.Errorf("zwayam %s page at offset %d: response code is not 200", s.domain, offset)
		}
		if response.Data == nil {
			return nil, fmt.Errorf("zwayam %s page at offset %d: response omitted data", s.domain, offset)
		}
		page := response.Data
		if page.Results == nil || page.TotalCount == nil || page.HasMoreData == nil ||
			page.FacetedSearchConfig == nil || page.FacetedSearchConfig.PaginationHowMuch == nil {
			return nil, fmt.Errorf("zwayam %s page at offset %d: response omitted pagination fields", s.domain, offset)
		}
		pageSize, err := strconv.Atoi(string(*page.FacetedSearchConfig.PaginationHowMuch))
		if err != nil || pageSize <= 0 || pageSize > zwayamMaximumPageSize {
			return nil, fmt.Errorf(
				"zwayam %s page at offset %d: paginationHowMuch %q is not from 1 to %d",
				s.domain, offset, *page.FacetedSearchConfig.PaginationHowMuch, zwayamMaximumPageSize,
			)
		}
		if expectedPageSize < 0 {
			expectedPageSize = pageSize
		} else if pageSize != expectedPageSize {
			return nil, fmt.Errorf(
				"zwayam %s page at offset %d: paginationHowMuch changed from %d to %d",
				s.domain, offset, expectedPageSize, pageSize,
			)
		}
		total := *page.TotalCount
		if total < 0 {
			return nil, fmt.Errorf("zwayam %s page at offset %d: negative totalCount %d", s.domain, offset, total)
		}
		if total > zwayamMaximumPostings {
			return nil, fmt.Errorf(
				"zwayam %s: totalCount %d exceeds safety limit %d",
				s.domain, total, zwayamMaximumPostings,
			)
		}
		if expectedTotal < 0 {
			expectedTotal = total
			jobs = make([]model.Job, 0, total)
		} else if total != expectedTotal {
			return nil, fmt.Errorf(
				"zwayam %s page at offset %d: totalCount changed from %d to %d",
				s.domain, offset, expectedTotal, total,
			)
		}

		results := *page.Results
		if len(results) > expectedPageSize {
			return nil, fmt.Errorf(
				"zwayam %s page at offset %d: returned %d jobs, page size is %d",
				s.domain, offset, len(results), expectedPageSize,
			)
		}
		if len(results) == 0 && offset != expectedTotal {
			return nil, fmt.Errorf(
				"zwayam %s: empty page at offset %d before reported total %d",
				s.domain, offset, expectedTotal,
			)
		}
		if offset+len(results) > expectedTotal {
			return nil, fmt.Errorf(
				"zwayam %s: page at offset %d would exceed reported total %d",
				s.domain, offset, expectedTotal,
			)
		}
		for index, result := range results {
			job, postingID, err := s.normalizeSearchHit(result, siteBase)
			if err != nil {
				return nil, fmt.Errorf(
					"zwayam %s page at offset %d row %d: %w",
					s.domain, offset, index, err,
				)
			}
			if _, duplicate := seen[postingID]; duplicate {
				return nil, fmt.Errorf("zwayam %s: duplicate posting id %q", s.domain, postingID)
			}
			seen[postingID] = struct{}{}
			jobs = append(jobs, job)
		}

		nextOffset := offset + len(results)
		wantMore := nextOffset < expectedTotal
		if *page.HasMoreData != wantMore {
			return nil, fmt.Errorf(
				"zwayam %s page at offset %d: hasMoreData is %t, want %t",
				s.domain, offset, *page.HasMoreData, wantMore,
			)
		}
		if !wantMore {
			return jobs, nil
		}
		if len(results) < expectedPageSize {
			return nil, fmt.Errorf(
				"zwayam %s: short page of %d at offset %d before reported total %d",
				s.domain, len(results), offset, expectedTotal,
			)
		}
		offset = nextOffset
	}
}

func (s *zwayam) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("zwayam %s: nil job", s.domain)
	}
	prefix := s.jobID("")
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("zwayam %s: job ID does not belong to this board", s.domain)
	}
	postingID, postingNumber, err := canonicalZwayamDecimal(strings.TrimPrefix(job.ID, prefix))
	if err != nil {
		return fmt.Errorf("zwayam %s: invalid job ID %q: %w", s.domain, job.ID, err)
	}
	slug, err := s.slugFromJobURL(job.URL)
	if err != nil {
		return fmt.Errorf("zwayam %s posting %s: %w", s.domain, postingID, err)
	}

	payload, err := json.Marshal(zwayamDetailRequest{
		CompanyID: s.companyNumber, JobURL: slug, ExternalSource: "CareerSite", CampusURL: "empty",
	})
	if err != nil {
		return fmt.Errorf("zwayam %s posting %s: encode detail request: %w", s.domain, postingID, err)
	}
	var detail zwayamDetailResponse
	if err := s.postJSON(
		ctx,
		strings.TrimRight(s.apiBase, "/")+"/jobs-service/v1/jobs/careersite",
		"application/json",
		payload,
		&detail,
	); err != nil {
		return fmt.Errorf("zwayam %s posting %s detail: %w", s.domain, postingID, err)
	}
	if detail.ID == nil || *detail.ID != postingNumber {
		return fmt.Errorf("zwayam %s posting %s detail: response omitted or changed id", s.domain, postingID)
	}
	if detail.CompanyID == nil || *detail.CompanyID != s.companyNumber {
		return fmt.Errorf("zwayam %s posting %s detail: response omitted or changed companyId", s.domain, postingID)
	}
	if detail.Status == nil || *detail.Status != 1 {
		return fmt.Errorf("zwayam %s posting %s detail: response is not open", s.domain, postingID)
	}
	if strings.TrimSpace(detail.JobURL) != slug {
		return fmt.Errorf("zwayam %s posting %s detail: response changed jobUrl", s.domain, postingID)
	}
	title := strings.TrimSpace(detail.JobTitle)
	if title == "" {
		return fmt.Errorf("zwayam %s posting %s detail: response omitted jobTitle", s.domain, postingID)
	}
	if detail.JobConfigurationData == nil {
		return fmt.Errorf("zwayam %s posting %s detail: response omitted jobConfigurationData", s.domain, postingID)
	}
	description, err := zwayamDescription(*detail.JobConfigurationData)
	if err != nil {
		return fmt.Errorf("zwayam %s posting %s detail: %w", s.domain, postingID, err)
	}
	postedAt, err := parseZwayamDate(detail.CreateDate)
	if err != nil {
		return fmt.Errorf("zwayam %s posting %s detail: %w", s.domain, postingID, err)
	}

	job.Title = title
	job.Description = description
	if location := strings.TrimSpace(detail.Location); location != "" {
		job.Location = location
	}
	if job.PostedAt.IsZero() {
		job.PostedAt = postedAt
	}
	return nil
}

func (s *zwayam) search(ctx context.Context, offset int) (zwayamSearchResponse, error) {
	filter, err := json.Marshal(zwayamSearchFilter{
		PaginationStartNo: offset,
		SelectedCall:      "sort",
		SortCriteria: zwayamSortCriteria{
			Name: "modifiedDate", IsAscending: false,
		},
		AnyOfTheseWords: "",
	})
	if err != nil {
		return zwayamSearchResponse{}, fmt.Errorf("encode filterCri: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "filterCri", value: string(filter)},
		{name: "domain", value: s.domain},
		{name: "companyId", value: s.encodedID},
	} {
		if err := writer.WriteField(field.name, field.value); err != nil {
			return zwayamSearchResponse{}, fmt.Errorf("encode multipart field %s: %w", field.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return zwayamSearchResponse{}, fmt.Errorf("close multipart request: %w", err)
	}

	var response zwayamSearchResponse
	if err := s.postJSON(
		ctx,
		strings.TrimRight(s.apiBase, "/")+"/jobs/search",
		writer.FormDataContentType(),
		body.Bytes(),
		&response,
	); err != nil {
		return zwayamSearchResponse{}, err
	}
	return response, nil
}

func (s *zwayam) postJSON(
	ctx context.Context,
	endpoint string,
	contentType string,
	body []byte,
	out any,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", s.origin)
	req.Header.Set("Referer", strings.TrimRight(s.origin, "/")+"/")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("POST %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf("POST %s: unexpected Content-Type %q", endpoint, resp.Header.Get("Content-Type"))
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, zwayamBodyLimit+1))
	if err != nil {
		return fmt.Errorf("POST %s: reading response: %w", endpoint, err)
	}
	if len(responseBody) > zwayamBodyLimit {
		return fmt.Errorf("POST %s: response exceeds %d bytes", endpoint, zwayamBodyLimit)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("POST %s: decoding response: %w", endpoint, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("POST %s: response contains trailing JSON", endpoint)
		}
		return fmt.Errorf("POST %s: decoding trailing response: %w", endpoint, err)
	}
	return nil
}

func (s *zwayam) discoverSiteBase(ctx context.Context) (string, error) {
	client := *s.client
	originalCheckRedirect := client.CheckRedirect
	originURL, err := url.Parse(s.origin)
	if err != nil {
		return "", fmt.Errorf("zwayam %s: invalid configured origin: %w", s.domain, err)
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if !strings.EqualFold(req.URL.Host, originURL.Host) ||
			(req.URL.Scheme != "http" && req.URL.Scheme != "https") {
			return fmt.Errorf("redirect leaves career-site host %q", originURL.Host)
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		return nil
	}
	body, err := fetchHTMLPage(ctx, &client, strings.TrimRight(s.origin, "/")+"/", nil)
	if err != nil {
		return "", fmt.Errorf("zwayam %s: discover career-site base: %w", s.domain, err)
	}

	var baseHrefs []string
	document := string(body)
	for _, match := range htmlOpenTagRe.FindAllStringSubmatchIndex(document, -1) {
		if !strings.EqualFold(document[match[2]:match[3]], "base") {
			continue
		}
		if href := strings.TrimSpace(parseHTMLAttrs(document[match[4]:match[5]])["href"]); href != "" {
			baseHrefs = append(baseHrefs, href)
		}
	}
	if len(baseHrefs) != 1 {
		return "", fmt.Errorf(
			"zwayam %s: career-site page has %d non-empty base hrefs, want 1",
			s.domain, len(baseHrefs),
		)
	}
	resolved, err := resolveBoardURL(strings.TrimRight(s.origin, "/")+"/", baseHrefs[0])
	if err != nil {
		return "", fmt.Errorf("zwayam %s: invalid career-site base href: %w", s.domain, err)
	}
	resolvedURL, err := url.Parse(resolved)
	if err != nil {
		return "", fmt.Errorf("zwayam %s: parse career-site base href: %w", s.domain, err)
	}
	if resolvedURL.Scheme != originURL.Scheme || resolvedURL.RawQuery != "" ||
		resolvedURL.ForceQuery || resolvedURL.User != nil {
		return "", fmt.Errorf("zwayam %s: unsafe career-site base href %q", s.domain, baseHrefs[0])
	}
	resolvedURL.Path = strings.TrimRight(resolvedURL.Path, "/") + "/"
	return resolvedURL.String(), nil
}

func (s *zwayam) normalizeSearchHit(hit *zwayamSearchHit, siteBase string) (model.Job, string, error) {
	if hit == nil || hit.Source == nil {
		return model.Job{}, "", fmt.Errorf("record omitted _source")
	}
	postingID, postingNumber, err := canonicalZwayamDecimal(hit.ID)
	if err != nil {
		return model.Job{}, "", fmt.Errorf("invalid _id: %w", err)
	}
	record := hit.Source
	if record.ID == nil || *record.ID != postingNumber {
		return model.Job{}, "", fmt.Errorf("posting %s omitted or changed source id", postingID)
	}
	if record.CompanyID == nil || *record.CompanyID != s.companyNumber {
		return model.Job{}, "", fmt.Errorf("posting %s omitted or changed companyId", postingID)
	}
	if record.Status == nil || *record.Status != 1 ||
		!strings.EqualFold(strings.TrimSpace(record.DisplayStatus), "Open") ||
		strings.TrimSpace(record.RequisitionStatus) != "A" {
		return model.Job{}, "", fmt.Errorf("posting %s is not consistently marked open", postingID)
	}
	title := strings.TrimSpace(record.JobTitle)
	if title == "" {
		return model.Job{}, "", fmt.Errorf("posting %s omitted jobTitle", postingID)
	}
	slug, err := canonicalZwayamSlug(record.JobURL)
	if err != nil {
		return model.Job{}, "", fmt.Errorf("posting %s has invalid jobUrl: %w", postingID, err)
	}
	postedAt, err := parseZwayamDate(record.CreateDate)
	if err != nil {
		return model.Job{}, "", fmt.Errorf("posting %s: %w", postingID, err)
	}
	return model.Job{
		ID:       s.jobID(postingID),
		Company:  s.company,
		Title:    title,
		Location: zwayamLocationText(record),
		URL:      siteBase + "jobview/" + url.PathEscape(slug),
		PostedAt: postedAt,
	}, postingID, nil
}

func zwayamLocationText(record *zwayamSearchSource) string {
	var locations []string
	for _, location := range record.Locations {
		name := strings.TrimSpace(location.FormattedLocation)
		if name == "" {
			name = strings.Join(distinctStrings([]string{
				location.Location, location.City, location.State, location.Country,
			}), ", ")
		}
		locations = append(locations, name)
	}
	if locations = distinctStrings(locations); len(locations) > 0 {
		return strings.Join(locations, "; ")
	}
	return firstNonemptyZwayam(record.LocationSeparated, record.Location)
}

func zwayamDescription(configuration map[string]json.RawMessage) (string, error) {
	if len(configuration) == 0 {
		return "", fmt.Errorf("jobConfigurationData is empty")
	}
	preferred := []string{
		"Description", "Role", "Responsibilities", "Skills Required", "Years Of Exp", "Location",
	}
	ordered := make([]string, 0, len(configuration))
	used := make(map[string]struct{}, len(configuration))
	for _, key := range preferred {
		if _, ok := configuration[key]; ok {
			ordered = append(ordered, key)
			used[key] = struct{}{}
		}
	}
	var remaining []string
	for key := range configuration {
		if _, ok := used[key]; !ok {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	ordered = append(ordered, remaining...)

	var sections []string
	for _, key := range ordered {
		raw := configuration[key]
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("jobConfigurationData field %q is not a string", key)
		}
		text := strings.TrimSpace(htmltext.ToText(value))
		if text != "" {
			sections = append(sections, strings.TrimSpace(key)+":\n"+text)
		}
	}
	if len(sections) == 0 {
		return "", fmt.Errorf("jobConfigurationData contains no description text")
	}
	return strings.Join(sections, "\n\n"), nil
}

func (s *zwayam) slugFromJobURL(raw string) (string, error) {
	jobURL, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid first-party job URL: %w", err)
	}
	originURL, err := url.Parse(s.origin)
	if err != nil {
		return "", fmt.Errorf("invalid configured origin: %w", err)
	}
	if jobURL.Scheme != originURL.Scheme || !strings.EqualFold(jobURL.Host, originURL.Host) ||
		jobURL.RawQuery != "" || jobURL.ForceQuery || jobURL.User != nil || jobURL.Fragment != "" {
		return "", fmt.Errorf("job URL is not a canonical URL on %s", s.domain)
	}
	segments := strings.Split(strings.Trim(jobURL.Path, "/"), "/")
	if len(segments) < 2 || segments[len(segments)-2] != "jobview" {
		return "", fmt.Errorf("job URL does not contain a jobview path")
	}
	return canonicalZwayamSlug(segments[len(segments)-1])
}

func canonicalZwayamSlug(raw string) (string, error) {
	slug := strings.TrimSpace(raw)
	if slug == "" || slug != raw || strings.ContainsAny(slug, `/\?#`) {
		return "", fmt.Errorf("expected one non-empty URL path segment, got %q", raw)
	}
	for _, character := range slug {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("URL path segment contains a control character")
		}
	}
	return slug, nil
}

func canonicalZwayamDecimal(raw string) (string, int64, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", 0, fmt.Errorf("expected a canonical positive decimal integer, got %q", raw)
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return "", 0, fmt.Errorf("expected a canonical positive decimal integer, got %q", raw)
		}
	}
	number, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || number <= 0 || strconv.FormatInt(number, 10) != raw {
		return "", 0, fmt.Errorf("expected a canonical positive decimal integer, got %q", raw)
	}
	return raw, number, nil
}

func parseZwayamDate(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("response omitted createDate")
	}
	parsed, err := time.Parse("02-Jan-2006", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("unsupported createDate %q", raw)
	}
	return parsed, nil
}

func firstNonemptyZwayam(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (s *zwayam) jobID(postingID string) string { return s.keyPrefix + postingID }
