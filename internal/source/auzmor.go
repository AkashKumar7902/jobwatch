package source

// Auzmor Hire exposes anonymous JSON list and detail endpoints for public
// career portals. The list response always contains every department; limit
// caps the jobs returned within each department, while offset is only echoed.
// Completeness therefore comes from requesting the hard safety limit and
// checking every department's declared job count.
//
// Config:
//
//	- name: LetsTransport
//	  source: auzmor
//	  params:
//	    domain: letstransport

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
	auzmorAPIBase       = "https://hire.api.auzmor.com/api/v1"
	auzmorSiteBase      = "https://hire.auzmor.com"
	auzmorJobLimit      = 10_000
	auzmorGroupLimit    = 10_000
	auzmorListBodyLimit = 32 << 20
)

var (
	auzmorDomainRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,126}[a-z0-9])?$`)
	auzmorUUIDRE   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func init() {
	Register("auzmor", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawDomain, err := p.Require("domain")
		if err != nil {
			return nil, err
		}
		domain := strings.ToLower(strings.TrimSpace(rawDomain))
		if !auzmorDomainRE.MatchString(domain) {
			return nil, fmt.Errorf("param %q: invalid Auzmor domain %q", "domain", rawDomain)
		}
		for key := range p {
			if key != "domain" {
				keys := make([]string, 0, len(p)-1)
				for candidate := range p {
					if candidate != "domain" {
						keys = append(keys, candidate)
					}
				}
				sort.Strings(keys)
				return nil, fmt.Errorf("auzmor source accepts only param %q (got %s)", "domain", strings.Join(keys, ", "))
			}
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &auzmor{
			company:  company,
			domain:   domain,
			apiBase:  auzmorAPIBase,
			siteBase: auzmorSiteBase,
			client:   client,
		}, nil
	})
}

type auzmor struct {
	company  string
	domain   string
	apiBase  string
	siteBase string
	client   *http.Client
}

type auzmorListResponse struct {
	Data       *[]auzmorDepartmentGroup `json:"data"`
	Pagination *auzmorPagination        `json:"pagination"`
}

type auzmorPagination struct {
	TotalItems  *int `json:"totalItems"`
	FilterItems *int `json:"filterItems"`
	Limit       *int `json:"limit"`
	Offset      *int `json:"offset"`
}

type auzmorDepartmentGroup struct {
	Department *auzmorDepartment `json:"department"`
	Count      *int              `json:"count"`
	Jobs       *[]auzmorPosting  `json:"jobs"`
}

type auzmorDepartment struct {
	ID   *int64 `json:"id"`
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type auzmorReference struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type auzmorPosting struct {
	UUID            string           `json:"uuid"`
	Title           string           `json:"title"`
	State           string           `json:"state"`
	Status          string           `json:"status"`
	PublishType     string           `json:"publishType"`
	PublishedDate   string           `json:"publishedDate"`
	CreatedAt       string           `json:"createdAt"`
	IsRemote        *bool            `json:"isRemote"`
	EmploymentTypes *[]string        `json:"employmentTypes"`
	Department      *auzmorReference `json:"department"`
	Location        *auzmorReference `json:"location"`
	Description     string           `json:"description"`
}

type auzmorDetailResponse struct {
	Job        *auzmorPosting   `json:"job"`
	Department *auzmorReference `json:"department"`
	Location   *auzmorReference `json:"location"`
}

func (s *auzmor) Company() string { return s.company }

func (s *auzmor) Fetch(ctx context.Context) ([]model.Job, error) {
	endpoint, err := s.listURL()
	if err != nil {
		return nil, fmt.Errorf("auzmor %s: constructing list URL: %w", s.domain, err)
	}
	var response auzmorListResponse
	if err := s.getJSON(ctx, endpoint, auzmorListBodyLimit, &response); err != nil {
		return nil, fmt.Errorf("auzmor %s: fetching complete list: %w", s.domain, err)
	}
	if response.Data == nil || response.Pagination == nil {
		return nil, fmt.Errorf("auzmor %s: list omitted data or pagination", s.domain)
	}
	groups := *response.Data
	pagination := response.Pagination
	if pagination.TotalItems == nil || pagination.FilterItems == nil ||
		pagination.Limit == nil || pagination.Offset == nil {
		return nil, fmt.Errorf("auzmor %s: pagination omitted totalItems, filterItems, limit, or offset", s.domain)
	}
	if *pagination.TotalItems < 0 || *pagination.FilterItems < 0 {
		return nil, fmt.Errorf(
			"auzmor %s: pagination reported negative totals totalItems=%d filterItems=%d",
			s.domain, *pagination.TotalItems, *pagination.FilterItems,
		)
	}
	if *pagination.TotalItems > auzmorGroupLimit {
		return nil, fmt.Errorf(
			"auzmor %s: totalItems %d exceeds department safety limit %d",
			s.domain, *pagination.TotalItems, auzmorGroupLimit,
		)
	}
	if *pagination.TotalItems != len(groups) || *pagination.FilterItems != len(groups) {
		return nil, fmt.Errorf(
			"auzmor %s: incomplete department list returned %d groups, totalItems=%d filterItems=%d",
			s.domain, len(groups), *pagination.TotalItems, *pagination.FilterItems,
		)
	}
	if *pagination.Limit != auzmorJobLimit || *pagination.Offset != 0 {
		return nil, fmt.Errorf(
			"auzmor %s: pagination changed requested limit/offset to %d/%d",
			s.domain, *pagination.Limit, *pagination.Offset,
		)
	}

	jobs := make([]model.Job, 0)
	seenDepartments := make(map[string]struct{}, len(groups))
	seenDepartmentIDs := make(map[int64]struct{}, len(groups))
	seenJobs := make(map[string]struct{})
	for groupIndex, group := range groups {
		if group.Department == nil || group.Count == nil || group.Jobs == nil {
			return nil, fmt.Errorf(
				"auzmor %s: department group %d omitted department, count, or jobs",
				s.domain, groupIndex,
			)
		}
		department, err := validateAuzmorDepartment(group.Department)
		if err != nil {
			return nil, fmt.Errorf("auzmor %s: department group %d: %w", s.domain, groupIndex, err)
		}
		if _, duplicate := seenDepartments[department.UUID]; duplicate {
			return nil, fmt.Errorf(
				"auzmor %s: duplicate department UUID %q",
				s.domain, department.UUID,
			)
		}
		seenDepartments[department.UUID] = struct{}{}
		if _, duplicate := seenDepartmentIDs[*group.Department.ID]; duplicate {
			return nil, fmt.Errorf(
				"auzmor %s: duplicate department id %d",
				s.domain, *group.Department.ID,
			)
		}
		seenDepartmentIDs[*group.Department.ID] = struct{}{}

		if *group.Count < 0 {
			return nil, fmt.Errorf(
				"auzmor %s: department %s reported negative count %d",
				s.domain, department.UUID, *group.Count,
			)
		}
		postings := *group.Jobs
		if *group.Count != len(postings) {
			return nil, fmt.Errorf(
				"auzmor %s: incomplete department %s returned %d jobs, count is %d",
				s.domain, department.UUID, len(postings), *group.Count,
			)
		}
		if len(postings) > auzmorJobLimit || len(jobs)+len(postings) > auzmorJobLimit {
			return nil, fmt.Errorf(
				"auzmor %s: complete list exceeds job safety limit %d",
				s.domain, auzmorJobLimit,
			)
		}

		for jobIndex, posting := range postings {
			job, externalID, err := s.normalizePosting(posting, department)
			if err != nil {
				return nil, fmt.Errorf(
					"auzmor %s: department %s job %d: %w",
					s.domain, department.UUID, jobIndex, err,
				)
			}
			if _, duplicate := seenJobs[externalID]; duplicate {
				return nil, fmt.Errorf("auzmor %s: duplicate job UUID %q", s.domain, externalID)
			}
			seenJobs[externalID] = struct{}{}
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

func (s *auzmor) normalizePosting(
	posting auzmorPosting,
	group auzmorReference,
) (model.Job, string, error) {
	id, err := canonicalAuzmorUUID(posting.UUID)
	if err != nil {
		return model.Job{}, "", fmt.Errorf("invalid uuid: %w", err)
	}
	title := strings.TrimSpace(posting.Title)
	if title == "" {
		return model.Job{}, "", fmt.Errorf("job %s has empty title", id)
	}
	if posting.State != "PUBLISHED" {
		return model.Job{}, "", fmt.Errorf("job %s has state %q, want PUBLISHED", id, posting.State)
	}
	if posting.Status != "OPEN" {
		return model.Job{}, "", fmt.Errorf("job %s has status %q, want OPEN", id, posting.Status)
	}
	if posting.PublishType != "External" {
		return model.Job{}, "", fmt.Errorf("job %s has publishType %q, want External", id, posting.PublishType)
	}
	if posting.IsRemote == nil {
		return model.Job{}, "", fmt.Errorf("job %s omitted isRemote", id)
	}
	department, err := validateAuzmorReference(posting.Department, "department")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", id, err)
	}
	if department != group {
		return model.Job{}, "", fmt.Errorf(
			"job %s department %q/%q does not match group %q/%q",
			id, department.UUID, department.Name, group.UUID, group.Name,
		)
	}
	location, err := auzmorLocationText(posting.Location, *posting.IsRemote)
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", id, err)
	}
	employmentType, err := auzmorEmploymentType(posting.EmploymentTypes)
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", id, err)
	}
	publishedAt, err := auzmorTimestamp(posting.PublishedDate, "publishedDate")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", id, err)
	}
	createdAt, err := auzmorTimestamp(posting.CreatedAt, "createdAt")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", id, err)
	}
	if publishedAt.Before(createdAt) {
		return model.Job{}, "", fmt.Errorf("job %s was published before it was created", id)
	}
	return model.Job{
		ID:             s.jobID(id),
		Company:        s.company,
		Title:          title,
		Location:       location,
		URL:            s.publicURL(id),
		EmploymentType: employmentType,
		PostedAt:       publishedAt,
	}, id, nil
}

func (s *auzmor) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("auzmor %s: nil job", s.domain)
	}
	prefix := s.jobID("")
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("auzmor %s: job ID %q does not belong to this board", s.domain, job.ID)
	}
	id, err := canonicalAuzmorUUID(strings.TrimPrefix(job.ID, prefix))
	if err != nil {
		return fmt.Errorf("auzmor %s: invalid job ID %q: %w", s.domain, job.ID, err)
	}
	endpoint := strings.TrimRight(s.apiBase, "/") + "/getJob/" +
		url.PathEscape(s.domain) + "/" + url.PathEscape(id)
	var response auzmorDetailResponse
	if err := s.getJSON(ctx, endpoint, customDetailBodyLimit, &response); err != nil {
		return fmt.Errorf("auzmor %s job %s detail: %w", s.domain, id, err)
	}
	if response.Job == nil || response.Department == nil {
		return fmt.Errorf("auzmor %s job %s detail omitted job or department", s.domain, id)
	}
	detail := *response.Job
	detailID, err := canonicalAuzmorUUID(detail.UUID)
	if err != nil {
		return fmt.Errorf("auzmor %s job %s detail has invalid uuid: %w", s.domain, id, err)
	}
	if detailID != id {
		return fmt.Errorf("auzmor %s job %s detail returned uuid %q", s.domain, id, detailID)
	}
	department, err := validateAuzmorReference(detail.Department, "job department")
	if err != nil {
		return fmt.Errorf("auzmor %s job %s detail: %w", s.domain, id, err)
	}
	outerDepartment, err := validateAuzmorReference(response.Department, "department")
	if err != nil {
		return fmt.Errorf("auzmor %s job %s detail: %w", s.domain, id, err)
	}
	if department != outerDepartment {
		return fmt.Errorf("auzmor %s job %s detail department fields disagree", s.domain, id)
	}
	if detail.State != "PUBLISHED" || detail.Status != "OPEN" || detail.PublishType != "External" {
		return fmt.Errorf(
			"auzmor %s job %s detail is not an open external publication (state=%q status=%q publishType=%q)",
			s.domain, id, detail.State, detail.Status, detail.PublishType,
		)
	}
	if detail.IsRemote == nil {
		return fmt.Errorf("auzmor %s job %s detail omitted isRemote", s.domain, id)
	}
	if !sameAuzmorReference(detail.Location, response.Location) {
		return fmt.Errorf("auzmor %s job %s detail location fields disagree", s.domain, id)
	}
	location, err := auzmorLocationText(detail.Location, *detail.IsRemote)
	if err != nil {
		return fmt.Errorf("auzmor %s job %s detail: %w", s.domain, id, err)
	}
	employmentType, err := auzmorEmploymentType(detail.EmploymentTypes)
	if err != nil {
		return fmt.Errorf("auzmor %s job %s detail: %w", s.domain, id, err)
	}
	publishedAt, err := auzmorTimestamp(detail.PublishedDate, "publishedDate")
	if err != nil {
		return fmt.Errorf("auzmor %s job %s detail: %w", s.domain, id, err)
	}
	createdAt, err := auzmorTimestamp(detail.CreatedAt, "createdAt")
	if err != nil {
		return fmt.Errorf("auzmor %s job %s detail: %w", s.domain, id, err)
	}
	if publishedAt.Before(createdAt) {
		return fmt.Errorf("auzmor %s job %s detail was published before it was created", s.domain, id)
	}
	title := strings.TrimSpace(detail.Title)
	if title == "" {
		return fmt.Errorf("auzmor %s job %s detail has empty title", s.domain, id)
	}
	if title != job.Title || location != job.Location ||
		employmentType != job.EmploymentType || !publishedAt.Equal(job.PostedAt) {
		return fmt.Errorf("auzmor %s job %s detail does not match list fields", s.domain, id)
	}
	description := strings.TrimSpace(htmltext.ToText(detail.Description))
	if description == "" {
		return fmt.Errorf("auzmor %s job %s detail has empty description", s.domain, id)
	}

	job.Title = title
	job.Location = location
	job.URL = s.publicURL(id)
	job.EmploymentType = employmentType
	job.Description = description
	job.PostedAt = publishedAt
	return nil
}

func validateAuzmorDepartment(department *auzmorDepartment) (auzmorReference, error) {
	if department == nil {
		return auzmorReference{}, fmt.Errorf("missing department")
	}
	if department.ID == nil || *department.ID <= 0 {
		return auzmorReference{}, fmt.Errorf("department omitted a positive id")
	}
	return validateAuzmorReference(
		&auzmorReference{UUID: department.UUID, Name: department.Name},
		"department",
	)
}

func validateAuzmorReference(reference *auzmorReference, name string) (auzmorReference, error) {
	if reference == nil {
		return auzmorReference{}, fmt.Errorf("missing %s", name)
	}
	id, err := canonicalAuzmorUUID(reference.UUID)
	if err != nil {
		return auzmorReference{}, fmt.Errorf("%s has invalid uuid: %w", name, err)
	}
	value := strings.TrimSpace(reference.Name)
	if value == "" {
		return auzmorReference{}, fmt.Errorf("%s has empty name", name)
	}
	return auzmorReference{UUID: id, Name: value}, nil
}

func sameAuzmorReference(left, right *auzmorReference) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue, leftErr := validateAuzmorReference(left, "location")
	rightValue, rightErr := validateAuzmorReference(right, "location")
	return leftErr == nil && rightErr == nil && leftValue == rightValue
}

func auzmorLocationText(location *auzmorReference, remote bool) (string, error) {
	if location == nil {
		if remote {
			return "Remote", nil
		}
		return "", fmt.Errorf("missing location for non-remote job")
	}
	value, err := validateAuzmorReference(location, "location")
	if err != nil {
		return "", err
	}
	if remote && !strings.Contains(strings.ToLower(value.Name), "remote") {
		return "Remote " + value.Name, nil
	}
	return value.Name, nil
}

func auzmorEmploymentType(values *[]string) (string, error) {
	if values == nil || len(*values) == 0 {
		return "", fmt.Errorf("omitted or empty employmentTypes")
	}
	seen := make(map[string]struct{}, len(*values))
	cleaned := make([]string, 0, len(*values))
	for index, raw := range *values {
		value := strings.TrimSpace(raw)
		if value == "" {
			return "", fmt.Errorf("employmentTypes item %d is empty", index)
		}
		if _, duplicate := seen[value]; duplicate {
			return "", fmt.Errorf("employmentTypes contains duplicate %q", value)
		}
		seen[value] = struct{}{}
		cleaned = append(cleaned, value)
	}
	return strings.Join(cleaned, "; "), nil
}

func auzmorTimestamp(value, field string) (time.Time, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return time.Time{}, fmt.Errorf("invalid %s %q", field, value)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s %q", field, value)
	}
	return parsed, nil
}

func canonicalAuzmorUUID(value string) (string, error) {
	if !auzmorUUIDRE.MatchString(value) {
		return "", fmt.Errorf("expected 32 lowercase hexadecimal characters, got %q", value)
	}
	return value, nil
}

func (s *auzmor) jobID(id string) string {
	return "auzmor/" + s.domain + "/" + id
}

func (s *auzmor) publicURL(id string) string {
	return strings.TrimRight(s.siteBase, "/") + "/" + url.PathEscape(s.domain) +
		"/careers/" + url.PathEscape(id)
}

func (s *auzmor) listURL() (string, error) {
	endpoint, err := url.Parse(
		strings.TrimRight(s.apiBase, "/") + "/careers/" +
			url.PathEscape(s.domain) + "/groupByDepartment",
	)
	if err != nil {
		return "", err
	}
	query := endpoint.Query()
	query.Set("limit", strconv.Itoa(auzmorJobLimit))
	query.Set("offset", "0")
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func (s *auzmor) getJSON(ctx context.Context, endpoint string, limit int64, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 200))
		return fmt.Errorf("GET %s: %s: %s", endpoint, response.Status, bytes.TrimSpace(snippet))
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf("GET %s: unexpected Content-Type %q", endpoint, response.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if int64(len(body)) > limit {
		return fmt.Errorf("GET %s: response exceeds %d-byte safety limit", endpoint, limit)
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
