package source

// Darwinbox Candidate v2 career portals expose anonymous company, list, and
// detail JSON endpoints on each tenant's darwinbox.in subdomain. The company
// response is checked before pagination so disabled or mismatched tenants
// cannot silently look like an empty board.
//
// Config:
//
//	- name: Moneyview
//	  source: darwinbox
//	  params:
//	    subdomain: moneyview
//	    max_postings: 2000

import (
	"bytes"
	"context"
	"encoding/json"
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

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	darwinboxPageSize        = 10
	darwinboxDefaultMaxJobs  = 2_000
	darwinboxHardMaxJobs     = 10_000
	darwinboxConfigBodyLimit = 1 << 20
	darwinboxListBodyLimit   = 8 << 20
	darwinboxDetailBodyLimit = 16 << 20
	darwinboxMaxUnix         = int64(253402300799)

	darwinboxBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
)

var (
	darwinboxSubdomainRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	darwinboxOpaqueIDRE  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,127})$`)
	darwinboxTenantIDRE  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,127})$`)
)

func init() {
	Register("darwinbox", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawSubdomain, err := p.Require("subdomain")
		if err != nil {
			return nil, err
		}
		subdomain := strings.ToLower(strings.TrimSpace(rawSubdomain))
		if !darwinboxSubdomainRE.MatchString(subdomain) {
			return nil, fmt.Errorf("param %q: invalid Darwinbox subdomain %q", "subdomain", rawSubdomain)
		}
		maxPostings, err := positiveCappedParam(
			p, "max_postings", darwinboxDefaultMaxJobs, darwinboxHardMaxJobs,
		)
		if err != nil {
			return nil, err
		}
		var unknown []string
		for key := range p {
			if key != "subdomain" && key != "max_postings" {
				unknown = append(unknown, key)
			}
		}
		if len(unknown) != 0 {
			sort.Strings(unknown)
			return nil, fmt.Errorf(
				"darwinbox source accepts only params %q and %q (got %s)",
				"subdomain", "max_postings", strings.Join(unknown, ", "),
			)
		}
		if client == nil {
			client = http.DefaultClient
		}
		base := "https://" + subdomain + ".darwinbox.in"
		return &darwinbox{
			company:     company,
			subdomain:   subdomain,
			maxPostings: maxPostings,
			apiBase:     base,
			careersBase: base + "/ms/candidatev2/main/careers",
			client:      client,
		}, nil
	})
}

type darwinbox struct {
	company     string
	subdomain   string
	maxPostings int
	apiBase     string
	careersBase string
	client      *http.Client
}

type darwinboxConfigResponse struct {
	Status  string                  `json:"status"`
	Message *darwinboxConfigMessage `json:"message"`
}

type darwinboxConfigMessage struct {
	Company *darwinboxCompanyConfig `json:"company"`
}

type darwinboxCompanyConfig struct {
	CompanyName        string                   `json:"company_name"`
	Subdomain          string                   `json:"subdomain"`
	TenantID           string                   `json:"tenant_id"`
	RecruitmentEnabled *bool                    `json:"recruitment_enabled"`
	NewCareers         *bool                    `json:"new_careers"`
	IsPreview          *bool                    `json:"is_preview"`
	AllJobsCount       *int                     `json:"allJobsCount"`
	DateTimeFormat     *darwinboxDateTimeFormat `json:"date_time_format"`
}

type darwinboxDateTimeFormat struct {
	Timezone string `json:"timezone"`
}

type darwinboxListResponse struct {
	Status  string                `json:"status"`
	Message *darwinboxListMessage `json:"message"`
}

type darwinboxListMessage struct {
	JobsCount *int                `json:"jobscount"`
	Jobs      *[]darwinboxListJob `json:"jobs"`
}

type darwinboxListJob struct {
	ID                     string    `json:"id"`
	DesignationDisplayName string    `json:"designation_display_name"`
	CreatedOn              string    `json:"created_on"`
	OfficeLocationDisplay  string    `json:"officelocation_show_arr"`
	JobPostingOn           *int64    `json:"job_posting_on"`
	Department             string    `json:"department"`
	EmploymentType         string    `json:"emp_type"`
	Title                  string    `json:"title"`
	TooltipLocations       *[]string `json:"tool_tip_locations"`
	Timezone               string    `json:"timezone"`
}

type darwinboxDetailResponse struct {
	Status  string                  `json:"status"`
	Message *darwinboxDetailMessage `json:"message"`
}

type darwinboxDetailMessage struct {
	Job *[]darwinboxDetailJob `json:"job"`
}

type darwinboxDetailJob struct {
	ID                     string    `json:"id"`
	DesignationDisplayName string    `json:"designation_display_name"`
	CreatedOn              string    `json:"created_on"`
	Description            string    `json:"jd"`
	IsRemote               *int      `json:"is_remote"`
	OfficeLocationDisplay  string    `json:"officelocation_show_arr"`
	JobPostingOn           *int64    `json:"job_posting_on"`
	PostedOn               *int64    `json:"posted_on"`
	EmploymentType         string    `json:"emp_type"`
	Title                  string    `json:"title"`
	TooltipLocations       *[]string `json:"tool_tip_locations"`
	Timezone               string    `json:"timezone"`
}

func (s *darwinbox) Company() string { return s.company }

func (s *darwinbox) Fetch(ctx context.Context) ([]model.Job, error) {
	config, err := s.fetchConfig(ctx)
	if err != nil {
		return nil, err
	}
	total := *config.AllJobsCount
	if total > s.maxPostings {
		return nil, fmt.Errorf(
			"darwinbox %s: company reports %d jobs, exceeding max_postings=%d",
			s.subdomain, total, s.maxPostings,
		)
	}

	totalPages := 1
	if total > 0 {
		totalPages = (total + darwinboxPageSize - 1) / darwinboxPageSize
	}
	jobs := make([]model.Job, 0, total)
	seen := make(map[string]struct{}, total)
	for pageNumber := 1; pageNumber <= totalPages; pageNumber++ {
		endpoint := s.apiBase + "/ms/candidateapi/job?page=" + strconv.Itoa(pageNumber)
		var response darwinboxListResponse
		if err := s.getJSON(
			ctx, endpoint, s.careersBase+"/allJobs", darwinboxListBodyLimit, &response,
		); err != nil {
			return nil, fmt.Errorf("darwinbox %s: page %d: %w", s.subdomain, pageNumber, err)
		}
		if response.Status != "success" || response.Message == nil ||
			response.Message.JobsCount == nil || response.Message.Jobs == nil {
			return nil, fmt.Errorf(
				"darwinbox %s: page %d omitted successful status, jobscount, or jobs",
				s.subdomain, pageNumber,
			)
		}
		if *response.Message.JobsCount != total {
			return nil, fmt.Errorf(
				"darwinbox %s: page %d reported jobscount=%d, company config reported %d",
				s.subdomain, pageNumber, *response.Message.JobsCount, total,
			)
		}
		postings := *response.Message.Jobs
		want := 0
		if total > 0 {
			remaining := total - (pageNumber-1)*darwinboxPageSize
			want = darwinboxPageSize
			if remaining < want {
				want = remaining
			}
		}
		if len(postings) != want {
			return nil, fmt.Errorf(
				"darwinbox %s: page %d returned %d jobs, want %d",
				s.subdomain, pageNumber, len(postings), want,
			)
		}
		for index, posting := range postings {
			job, externalID, err := s.normalizeListJob(posting, config.DateTimeFormat.Timezone)
			if err != nil {
				return nil, fmt.Errorf(
					"darwinbox %s: page %d item %d: %w",
					s.subdomain, pageNumber, index, err,
				)
			}
			if _, duplicate := seen[externalID]; duplicate {
				return nil, fmt.Errorf(
					"darwinbox %s: duplicate job id %q",
					s.subdomain, externalID,
				)
			}
			seen[externalID] = struct{}{}
			jobs = append(jobs, job)
		}
	}
	if len(jobs) != total {
		return nil, fmt.Errorf(
			"darwinbox %s: collected %d unique jobs, company config reported %d",
			s.subdomain, len(jobs), total,
		)
	}
	return jobs, nil
}

func (s *darwinbox) fetchConfig(ctx context.Context) (*darwinboxCompanyConfig, error) {
	endpoint := s.apiBase + "/ms/candidateapi/getCompanyConfig"
	var response darwinboxConfigResponse
	if err := s.getJSON(
		ctx, endpoint, s.careersBase+"/allJobs", darwinboxConfigBodyLimit, &response,
	); err != nil {
		return nil, fmt.Errorf("darwinbox %s: company config: %w", s.subdomain, err)
	}
	if response.Status != "success" || response.Message == nil || response.Message.Company == nil {
		return nil, fmt.Errorf(
			"darwinbox %s: company config omitted successful status or company",
			s.subdomain,
		)
	}
	config := response.Message.Company
	if strings.TrimSpace(config.CompanyName) == "" {
		return nil, fmt.Errorf("darwinbox %s: company config has an empty company_name", s.subdomain)
	}
	if config.Subdomain != s.subdomain {
		return nil, fmt.Errorf(
			"darwinbox %s: company config identifies subdomain %q",
			s.subdomain, config.Subdomain,
		)
	}
	if !darwinboxTenantIDRE.MatchString(config.TenantID) {
		return nil, fmt.Errorf(
			"darwinbox %s: company config has invalid tenant_id %q",
			s.subdomain, config.TenantID,
		)
	}
	if config.RecruitmentEnabled == nil || !*config.RecruitmentEnabled {
		return nil, fmt.Errorf("darwinbox %s: recruitment is not enabled", s.subdomain)
	}
	if config.NewCareers == nil || !*config.NewCareers {
		return nil, fmt.Errorf("darwinbox %s: Candidate v2 careers are not enabled", s.subdomain)
	}
	if config.IsPreview == nil || *config.IsPreview {
		return nil, fmt.Errorf("darwinbox %s: company config is missing a non-preview flag", s.subdomain)
	}
	if config.AllJobsCount == nil || *config.AllJobsCount < 0 {
		return nil, fmt.Errorf("darwinbox %s: company config has an invalid allJobsCount", s.subdomain)
	}
	if *config.AllJobsCount > darwinboxHardMaxJobs {
		return nil, fmt.Errorf(
			"darwinbox %s: allJobsCount=%d exceeds hard safety limit %d",
			s.subdomain, *config.AllJobsCount, darwinboxHardMaxJobs,
		)
	}
	if config.DateTimeFormat == nil || strings.TrimSpace(config.DateTimeFormat.Timezone) == "" {
		return nil, fmt.Errorf("darwinbox %s: company config omitted its timezone", s.subdomain)
	}
	if _, err := time.LoadLocation(config.DateTimeFormat.Timezone); err != nil {
		return nil, fmt.Errorf(
			"darwinbox %s: company config has invalid timezone %q",
			s.subdomain, config.DateTimeFormat.Timezone,
		)
	}
	return config, nil
}

func (s *darwinbox) normalizeListJob(
	posting darwinboxListJob,
	tenantTimezone string,
) (model.Job, string, error) {
	id, err := canonicalDarwinboxID(posting.ID)
	if err != nil {
		return model.Job{}, "", err
	}
	title := strings.TrimSpace(posting.Title)
	if title == "" {
		return model.Job{}, "", fmt.Errorf("job %s has an empty title", id)
	}
	displayName := strings.TrimSpace(posting.DesignationDisplayName)
	if displayName != "" && displayName != title {
		return model.Job{}, "", fmt.Errorf(
			"job %s designation_display_name %q does not match title %q",
			id, displayName, title,
		)
	}
	employmentType := strings.TrimSpace(posting.EmploymentType)
	if employmentType == "" {
		return model.Job{}, "", fmt.Errorf("job %s has an empty emp_type", id)
	}
	location, err := normalizeDarwinboxLocation(
		posting.OfficeLocationDisplay, posting.TooltipLocations,
	)
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", id, err)
	}
	if _, err := parseDarwinboxCreatedOn(posting.CreatedOn); err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", id, err)
	}
	postedAt, err := parseDarwinboxUnix(posting.JobPostingOn, "job_posting_on")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", id, err)
	}
	timezone := strings.TrimSpace(posting.Timezone)
	if timezone == "" || timezone != tenantTimezone {
		return model.Job{}, "", fmt.Errorf(
			"job %s timezone %q does not match tenant timezone %q",
			id, posting.Timezone, tenantTimezone,
		)
	}
	return model.Job{
		ID:             s.jobID(id),
		Company:        s.company,
		Title:          title,
		Location:       location,
		URL:            s.publicURL(id),
		EmploymentType: employmentType,
		PostedAt:       postedAt,
	}, id, nil
}

func (s *darwinbox) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("darwinbox %s: nil job", s.subdomain)
	}
	prefix := "darwinbox/" + s.subdomain + "/"
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf(
			"darwinbox %s: job id %q does not have prefix %q",
			s.subdomain, job.ID, prefix,
		)
	}
	id, err := canonicalDarwinboxID(strings.TrimPrefix(job.ID, prefix))
	if err != nil {
		return fmt.Errorf("darwinbox %s: job id: %w", s.subdomain, err)
	}
	publicURL := s.publicURL(id)
	if job.URL != publicURL {
		return fmt.Errorf(
			"darwinbox %s: job %s URL %q does not match canonical URL %q",
			s.subdomain, id, job.URL, publicURL,
		)
	}

	endpoint := s.apiBase + "/ms/candidateapi/job/" + url.PathEscape(id)
	var response darwinboxDetailResponse
	if err := s.getJSON(ctx, endpoint, publicURL, darwinboxDetailBodyLimit, &response); err != nil {
		return fmt.Errorf("darwinbox %s: detail %s: %w", s.subdomain, id, err)
	}
	if response.Status != "success" || response.Message == nil || response.Message.Job == nil {
		return fmt.Errorf(
			"darwinbox %s: detail %s omitted successful status or job",
			s.subdomain, id,
		)
	}
	postings := *response.Message.Job
	if len(postings) != 1 {
		return fmt.Errorf(
			"darwinbox %s: detail %s returned %d jobs, want 1",
			s.subdomain, id, len(postings),
		)
	}
	detail := postings[0]
	detailID, err := canonicalDarwinboxID(detail.ID)
	if err != nil {
		return fmt.Errorf("darwinbox %s: detail %s: %w", s.subdomain, id, err)
	}
	if detailID != id {
		return fmt.Errorf(
			"darwinbox %s: detail %s returned job id %q",
			s.subdomain, id, detailID,
		)
	}
	title := strings.TrimSpace(detail.Title)
	if title == "" || title != job.Title {
		return fmt.Errorf(
			"darwinbox %s: detail %s title %q does not match list title %q",
			s.subdomain, id, detail.Title, job.Title,
		)
	}
	displayName := strings.TrimSpace(detail.DesignationDisplayName)
	if displayName != "" && displayName != title {
		return fmt.Errorf(
			"darwinbox %s: detail %s designation_display_name %q does not match title %q",
			s.subdomain, id, displayName, title,
		)
	}
	employmentType := strings.TrimSpace(detail.EmploymentType)
	if employmentType == "" || employmentType != job.EmploymentType {
		return fmt.Errorf(
			"darwinbox %s: detail %s emp_type %q does not match list value %q",
			s.subdomain, id, detail.EmploymentType, job.EmploymentType,
		)
	}
	location, err := normalizeDarwinboxLocation(
		detail.OfficeLocationDisplay, detail.TooltipLocations,
	)
	if err != nil {
		return fmt.Errorf("darwinbox %s: detail %s: %w", s.subdomain, id, err)
	}
	if location != job.Location {
		return fmt.Errorf(
			"darwinbox %s: detail %s location %q does not match list value %q",
			s.subdomain, id, location, job.Location,
		)
	}
	if _, err := parseDarwinboxCreatedOn(detail.CreatedOn); err != nil {
		return fmt.Errorf("darwinbox %s: detail %s: %w", s.subdomain, id, err)
	}
	jobPostingOn, err := parseDarwinboxUnix(detail.JobPostingOn, "job_posting_on")
	if err != nil {
		return fmt.Errorf("darwinbox %s: detail %s: %w", s.subdomain, id, err)
	}
	postedOn, err := parseDarwinboxUnix(detail.PostedOn, "posted_on")
	if err != nil {
		return fmt.Errorf("darwinbox %s: detail %s: %w", s.subdomain, id, err)
	}
	if !jobPostingOn.Equal(postedOn) || !jobPostingOn.Equal(job.PostedAt) {
		return fmt.Errorf(
			"darwinbox %s: detail %s posting timestamps do not match the list",
			s.subdomain, id,
		)
	}
	if detail.IsRemote == nil || (*detail.IsRemote != 0 && *detail.IsRemote != 1) {
		return fmt.Errorf("darwinbox %s: detail %s has invalid is_remote", s.subdomain, id)
	}
	detailTimezone := strings.TrimSpace(detail.Timezone)
	if detailTimezone == "" {
		return fmt.Errorf("darwinbox %s: detail %s has an empty timezone", s.subdomain, id)
	}
	if _, err := time.LoadLocation(detailTimezone); err != nil {
		return fmt.Errorf(
			"darwinbox %s: detail %s has invalid timezone %q",
			s.subdomain, id, detail.Timezone,
		)
	}
	description := htmltext.ToText(detail.Description)
	if description == "" {
		return fmt.Errorf("darwinbox %s: detail %s has an empty jd", s.subdomain, id)
	}

	updated := *job
	updated.Description = description
	*job = updated
	return nil
}

func normalizeDarwinboxLocation(display string, values *[]string) (string, error) {
	if values == nil {
		return "", fmt.Errorf("omitted tool_tip_locations")
	}
	display = strings.TrimSpace(display)
	locations := make([]string, 0, len(*values))
	seen := make(map[string]struct{}, len(*values))
	for index, raw := range *values {
		location := strings.TrimSpace(raw)
		if location == "" {
			return "", fmt.Errorf("tool_tip_locations item %d is empty", index)
		}
		if _, duplicate := seen[location]; duplicate {
			continue
		}
		seen[location] = struct{}{}
		locations = append(locations, location)
	}
	switch len(*values) {
	case 0:
		if display == "" {
			return "", fmt.Errorf("location is empty")
		}
		return display, nil
	case 1:
		if display != locations[0] {
			return "", fmt.Errorf(
				"officelocation_show_arr %q does not match tool_tip_locations %q",
				display, locations[0],
			)
		}
	default:
		if display != "Multiple locations" {
			return "", fmt.Errorf(
				"officelocation_show_arr %q does not mark multiple locations",
				display,
			)
		}
	}
	return strings.Join(locations, "; "), nil
}

func parseDarwinboxCreatedOn(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("created_on is empty")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid created_on %q", value)
	}
	return parsed, nil
}

func parseDarwinboxUnix(value *int64, field string) (time.Time, error) {
	if value == nil || *value <= 0 || *value > darwinboxMaxUnix {
		return time.Time{}, fmt.Errorf("invalid %s", field)
	}
	return time.Unix(*value, 0).UTC(), nil
}

func canonicalDarwinboxID(value string) (string, error) {
	if !darwinboxOpaqueIDRE.MatchString(value) {
		return "", fmt.Errorf("invalid opaque job id %q", value)
	}
	return value, nil
}

func (s *darwinbox) jobID(id string) string {
	return "darwinbox/" + s.subdomain + "/" + id
}

func (s *darwinbox) publicURL(id string) string {
	return s.careersBase + "/jobDetails/" + url.PathEscape(id)
}

func (s *darwinbox) getJSON(
	ctx context.Context,
	endpoint string,
	referer string,
	bodyLimit int64,
	out any,
) error {
	requestURL, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	baseURL, err := url.Parse(s.apiBase)
	if err != nil {
		return err
	}
	if requestURL.Scheme != baseURL.Scheme || requestURL.Host != baseURL.Host ||
		requestURL.User != nil || requestURL.Fragment != "" {
		return fmt.Errorf("untrusted Darwinbox endpoint %q", endpoint)
	}
	refererURL, err := url.Parse(referer)
	if err != nil {
		return err
	}
	careersURL, err := url.Parse(s.careersBase)
	if err != nil {
		return err
	}
	if refererURL.Scheme != careersURL.Scheme || refererURL.Host != careersURL.Host ||
		refererURL.User != nil || refererURL.RawQuery != "" || refererURL.ForceQuery ||
		refererURL.Fragment != "" {
		return fmt.Errorf("untrusted Darwinbox Referer %q", referer)
	}
	careersPath := strings.TrimRight(careersURL.EscapedPath(), "/")
	refererPath := refererURL.EscapedPath()
	trustedRefererPath := refererPath == careersPath+"/allJobs"
	if rawID, ok := strings.CutPrefix(refererPath, careersPath+"/jobDetails/"); ok {
		id, unescapeErr := url.PathUnescape(rawID)
		trustedRefererPath = unescapeErr == nil && rawID == url.PathEscape(id)
		if trustedRefererPath {
			if _, err := canonicalDarwinboxID(id); err != nil {
				trustedRefererPath = false
			}
		}
	}
	if !trustedRefererPath {
		return fmt.Errorf("untrusted Darwinbox Referer %q", referer)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", darwinboxBrowserUserAgent)
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Referer", refererURL.String())

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	guarded := &http.Client{
		Transport: client.Transport,
		Jar:       client.Jar,
		Timeout:   client.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := guarded.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.Request == nil || response.Request.URL == nil ||
		response.Request.URL.String() != requestURL.String() {
		return fmt.Errorf("GET %s: response has an unexpected final URL", endpoint)
	}
	if response.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 200))
		return fmt.Errorf(
			"GET %s: %s: %s",
			endpoint, response.Status, bytes.TrimSpace(snippet),
		)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf(
			"GET %s: unexpected Content-Type %q",
			endpoint, response.Header.Get("Content-Type"),
		)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, bodyLimit+1))
	if err != nil {
		return fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if int64(len(body)) > bodyLimit {
		return fmt.Errorf("GET %s: response exceeds %d-byte safety limit", endpoint, bodyLimit)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("GET %s: decoding response: %w", endpoint, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("GET %s: response contains trailing JSON", endpoint)
		}
		return fmt.Errorf("GET %s: decoding trailing response: %w", endpoint, err)
	}
	return nil
}
