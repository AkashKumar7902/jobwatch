package source

// HROne career portals expose an anonymous, page-at-a-time JSON list API and
// a JSON detail API. The list endpoint returns the encrypted position token
// needed by both the public application URL and the lazy detail request.
//
// Config:
//
//	- name: Addverb
//	  source: hrone
//	  params:
//	    domain_code: addverb
//	    api_key: ${HRONE_API_KEY}
//	    request_type: ${HRONE_REQUEST_TYPE}
//	    company_code: ${HRONE_COMPANY_CODE}

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	hronePageSize        = 15
	hroneMaximumPages    = 1000
	hroneListBodyLimit   = 8 << 20
	hroneDetailBodyLimit = 6 << 20

	hroneAPIBase    = "https://app.hrone.cloud"
	hronePortalBase = "https://career.hrone.cloud"
)

var (
	hroneDomainCodeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
	hroneTokenRE      = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func init() {
	Register("hrone", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawDomain, err := p.Require("domain_code")
		if err != nil {
			return nil, err
		}
		domainCode, err := canonicalHROneDomainCode(rawDomain)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", "domain_code", err)
		}

		rawAPIKey, err := p.Require("api_key")
		if err != nil {
			return nil, err
		}
		apiKey, err := canonicalHROneToken(rawAPIKey, 32, 512)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", "api_key", err)
		}

		rawRequestType, err := p.Require("request_type")
		if err != nil {
			return nil, err
		}
		requestType, err := canonicalHROneToken(rawRequestType, 8, 256)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", "request_type", err)
		}

		rawCompanyCode, err := p.Require("company_code")
		if err != nil {
			return nil, err
		}
		companyCode, err := canonicalHROneToken(rawCompanyCode, 8, 256)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", "company_code", err)
		}
		if client == nil {
			client = http.DefaultClient
		}

		return &hrone{
			company:      company,
			domainCode:   domainCode,
			apiKey:       apiKey,
			requestType:  requestType,
			companyCode:  companyCode,
			keyPrefix:    fmt.Sprintf("hrone/%s/%s/", domainCode, companyCode),
			apiBase:      hroneAPIBase,
			portalBase:   hronePortalBase,
			client:       client,
			maxPages:     hroneMaximumPages,
			postingsByID: make(map[string]hronePosting),
		}, nil
	})
}

type hrone struct {
	company     string
	domainCode  string
	apiKey      string
	requestType string
	companyCode string
	// keyPrefix excludes request_type: it is an opaque per-portal token HROne
	// regenerates whenever a customer reconfigures their career portal, and
	// it names a request route, not an employer.
	// Must stay equal to source.StatePrefix for these params.
	keyPrefix  string // hrone/{domain_code}/{company_code}/
	apiBase    string
	portalBase string
	client     *http.Client
	maxPages   int

	mu           sync.RWMutex
	postingsByID map[string]hronePosting
}

func (s *hrone) Company() string { return s.company }

type hroneSearchRequest struct {
	DepartmentCode      string          `json:"departmentCode"`
	CompanyCode         string          `json:"companyCode"`
	CareerPortalType    string          `json:"careerPortalType"`
	JobTitle            string          `json:"jobTitle"`
	EmploymentType      string          `json:"employmentType"`
	SeniorityName       string          `json:"seniorityName"`
	JobFunction         string          `json:"jobFunction"`
	Company             string          `json:"company"`
	BusinessUnitCode    string          `json:"businessUnitCode"`
	Department          string          `json:"department"`
	SubDepartment       string          `json:"subDepartment"`
	GradeCode           string          `json:"gradeCode"`
	DesignationCode     string          `json:"designationCode"`
	LevelCode           string          `json:"levelCode"`
	BranchCode          string          `json:"branchCode"`
	SubBranchCode       string          `json:"subBranchCode"`
	RegionCode          string          `json:"regionCode"`
	LocationID          string          `json:"locationId"`
	Experience          string          `json:"experience"`
	Qualification       string          `json:"qualification"`
	SkillsName          string          `json:"skillsName"`
	UrgentOpening       string          `json:"urgentOpening"`
	JobPosted           string          `json:"jobPosted"`
	IsShortURL          bool            `json:"isShortUrl"`
	Pagination          hronePagination `json:"pagination"`
	Nationality         string          `json:"nationality"`
	PreferredLocationID string          `json:"preferredLocationId"`
}

type hronePagination struct {
	PageNumber int `json:"pageNumber"`
	PageSize   int `json:"pageSize"`
}

type hronePosting struct {
	JobTitle            *string  `json:"jobTitle"`
	PositionID          *int64   `json:"positionId"`
	EncryptedPositionID *string  `json:"encryptedPositionId"`
	SourceType          *string  `json:"sourceType"`
	JobCode             *string  `json:"jobCode"`
	DepartmentCode      *string  `json:"departmentCode"`
	PreferredLocation   *string  `json:"preferredLocation"`
	ExperienceFrom      *float64 `json:"experienceFrom"`
	ExperienceTo        *float64 `json:"experienceTo"`
}

func (s *hrone) Fetch(ctx context.Context) ([]model.Job, error) {
	if s.maxPages <= 0 || s.maxPages > hroneMaximumPages {
		return nil, fmt.Errorf(
			"hrone %s: max pages must be between 1 and %d, got %d",
			s.domainCode, hroneMaximumPages, s.maxPages,
		)
	}
	jobs := make([]model.Job, 0)
	postingsByID := make(map[string]hronePosting)
	seenPositionIDs := make(map[int64]struct{})

	for pageNumber := 1; pageNumber <= s.maxPages; pageNumber++ {
		request := hroneSearchRequest{
			CompanyCode:      s.companyCode,
			CareerPortalType: s.requestType,
			JobPosted:        "0",
			Pagination: hronePagination{
				PageNumber: pageNumber,
				PageSize:   hronePageSize,
			},
		}
		var postings *[]hronePosting
		if err := s.doJSON(
			ctx,
			http.MethodPost,
			s.apiBase+"/api/external/referral/CareerPosition/Details",
			request,
			hroneListBodyLimit,
			&postings,
		); err != nil {
			return nil, fmt.Errorf("hrone %s page %d: %w", s.domainCode, pageNumber, err)
		}
		if postings == nil {
			return nil, fmt.Errorf("hrone %s page %d: response was null instead of a postings array", s.domainCode, pageNumber)
		}
		page := *postings
		if len(page) > hronePageSize {
			return nil, fmt.Errorf(
				"hrone %s page %d returned %d postings, requested at most %d",
				s.domainCode, pageNumber, len(page), hronePageSize,
			)
		}

		for index, posting := range page {
			normalized, encryptedID, positionID, err := s.normalizePosting(posting)
			if err != nil {
				return nil, fmt.Errorf("hrone %s page %d row %d: %w", s.domainCode, pageNumber, index, err)
			}
			if _, duplicate := postingsByID[encryptedID]; duplicate {
				return nil, fmt.Errorf("hrone %s: duplicate encryptedPositionId %q", s.domainCode, encryptedID)
			}
			if _, duplicate := seenPositionIDs[positionID]; duplicate {
				return nil, fmt.Errorf("hrone %s: duplicate positionId %d", s.domainCode, positionID)
			}
			postingsByID[encryptedID] = posting
			seenPositionIDs[positionID] = struct{}{}
			jobs = append(jobs, normalized)
		}

		if len(page) < hronePageSize {
			s.mu.Lock()
			s.postingsByID = postingsByID
			s.mu.Unlock()
			return jobs, nil
		}
	}

	return nil, fmt.Errorf(
		"hrone %s: pagination exceeded safety limit of %d pages (%d postings)",
		s.domainCode, s.maxPages, len(jobs),
	)
}

func (s *hrone) normalizePosting(posting hronePosting) (model.Job, string, int64, error) {
	if posting.JobTitle == nil || posting.PositionID == nil || posting.EncryptedPositionID == nil ||
		posting.SourceType == nil || posting.JobCode == nil || posting.DepartmentCode == nil ||
		posting.ExperienceFrom == nil || posting.ExperienceTo == nil {
		return model.Job{}, "", 0, fmt.Errorf("posting omitted required HROne fields")
	}
	title := strings.TrimSpace(*posting.JobTitle)
	if title == "" {
		return model.Job{}, "", 0, fmt.Errorf("positionId %d has an empty jobTitle", *posting.PositionID)
	}
	if *posting.PositionID <= 0 {
		return model.Job{}, "", 0, fmt.Errorf("invalid positionId %d", *posting.PositionID)
	}
	encryptedID, err := canonicalHROneToken(*posting.EncryptedPositionID, 8, 256)
	if err != nil {
		return model.Job{}, "", 0, fmt.Errorf("positionId %d has invalid encryptedPositionId: %w", *posting.PositionID, err)
	}
	sourceType, err := canonicalHROneToken(*posting.SourceType, 8, 256)
	if err != nil {
		return model.Job{}, "", 0, fmt.Errorf("positionId %d has invalid sourceType: %w", *posting.PositionID, err)
	}
	jobCode := strings.TrimSpace(*posting.JobCode)
	if jobCode == "" {
		return model.Job{}, "", 0, fmt.Errorf("positionId %d has an empty jobCode", *posting.PositionID)
	}
	departmentCode := strings.TrimSpace(*posting.DepartmentCode)
	if departmentCode != "" {
		if _, err := canonicalHROneToken(departmentCode, 8, 256); err != nil {
			return model.Job{}, "", 0, fmt.Errorf("positionId %d has invalid departmentCode: %w", *posting.PositionID, err)
		}
	}
	if *posting.ExperienceFrom < 0 || *posting.ExperienceTo < 0 ||
		*posting.ExperienceTo < *posting.ExperienceFrom {
		return model.Job{}, "", 0, fmt.Errorf(
			"positionId %d has invalid experience range %g-%g",
			*posting.PositionID, *posting.ExperienceFrom, *posting.ExperienceTo,
		)
	}

	location := ""
	if posting.PreferredLocation != nil {
		location = strings.TrimSpace(*posting.PreferredLocation)
	}
	return model.Job{
		ID:       s.jobID(encryptedID),
		Company:  s.company,
		Title:    title,
		Location: location,
		URL:      s.applicationURL(encryptedID, departmentCode, sourceType),
	}, encryptedID, *posting.PositionID, nil
}

type hroneDetail struct {
	RequestID                  *int64                    `json:"requestId"`
	JobTitle                   *string                   `json:"jobTitle"`
	JobCode                    *string                   `json:"jobCode"`
	CurrentStatus              *int                      `json:"currentStatus"`
	IsClosedForAll             *int                      `json:"isClosedForAll"`
	CanAddCandidate            *int                      `json:"canAddCandidate"`
	JobDescriptionBodyWithHTML *string                   `json:"jobDescriptionBodyWithHtml"`
	JobDescriptionBody         *string                   `json:"jobDescriptionBody"`
	EmployeeType               *string                   `json:"employeeType"`
	PreferredLocationList      *[]hronePreferredLocation `json:"preferredLocationList"`
}

type hronePreferredLocation struct {
	ID   *string `json:"id"`
	Text *string `json:"text"`
}

func (s *hrone) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("hrone %s: nil job", s.domainCode)
	}
	prefix := s.jobID("")
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("hrone %s: job ID does not belong to this board", s.domainCode)
	}
	encryptedID, err := canonicalHROneToken(strings.TrimPrefix(job.ID, prefix), 8, 256)
	if err != nil {
		return fmt.Errorf("hrone %s: invalid job ID %q: %w", s.domainCode, job.ID, err)
	}

	s.mu.RLock()
	posting, ok := s.postingsByID[encryptedID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("hrone %s: job ID %q was not returned by the latest fetch", s.domainCode, job.ID)
	}

	sourceType, err := canonicalHROneToken(*posting.SourceType, 8, 256)
	if err != nil {
		return fmt.Errorf("hrone %s position %s: invalid cached sourceType: %w", s.domainCode, encryptedID, err)
	}
	detailEndpoint := fmt.Sprintf(
		"%s/api/external/referral/JobOpening/Request/Details/%s/%s/%s",
		strings.TrimRight(s.apiBase, "/"),
		url.PathEscape(encryptedID),
		url.PathEscape(s.companyCode),
		url.PathEscape(sourceType),
	)
	var detail hroneDetail
	if err := s.doJSON(ctx, http.MethodGet, detailEndpoint, nil, hroneDetailBodyLimit, &detail); err != nil {
		return fmt.Errorf("hrone %s position %s detail: %w", s.domainCode, encryptedID, err)
	}
	if detail.RequestID == nil || detail.JobTitle == nil || detail.JobCode == nil ||
		detail.CurrentStatus == nil || detail.IsClosedForAll == nil ||
		detail.CanAddCandidate == nil || detail.EmployeeType == nil ||
		detail.PreferredLocationList == nil {
		return fmt.Errorf("hrone %s position %s detail omitted required HROne fields", s.domainCode, encryptedID)
	}
	if *detail.RequestID != *posting.PositionID {
		return fmt.Errorf(
			"hrone %s position %s detail requestId %d does not match positionId %d",
			s.domainCode, encryptedID, *detail.RequestID, *posting.PositionID,
		)
	}
	title := strings.TrimSpace(*detail.JobTitle)
	if title == "" || title != strings.TrimSpace(*posting.JobTitle) {
		return fmt.Errorf("hrone %s position %s detail jobTitle does not match list", s.domainCode, encryptedID)
	}
	jobCode := strings.TrimSpace(*detail.JobCode)
	if jobCode == "" || jobCode != strings.TrimSpace(*posting.JobCode) {
		return fmt.Errorf("hrone %s position %s detail jobCode does not match list", s.domainCode, encryptedID)
	}
	if *detail.CurrentStatus != 1 || *detail.IsClosedForAll != 0 || *detail.CanAddCandidate != 1 {
		return fmt.Errorf(
			"hrone %s position %s is not open (currentStatus=%d, isClosedForAll=%d, canAddCandidate=%d)",
			s.domainCode, encryptedID, *detail.CurrentStatus, *detail.IsClosedForAll, *detail.CanAddCandidate,
		)
	}

	descriptionHTML := ""
	if detail.JobDescriptionBodyWithHTML != nil {
		descriptionHTML = *detail.JobDescriptionBodyWithHTML
	}
	if strings.TrimSpace(descriptionHTML) == "" && detail.JobDescriptionBody != nil {
		descriptionHTML = *detail.JobDescriptionBody
	}
	description := strings.TrimSpace(htmltext.ToText(descriptionHTML))
	if description == "" {
		return fmt.Errorf("hrone %s position %s detail has an empty job description", s.domainCode, encryptedID)
	}
	employeeType := strings.TrimSpace(*detail.EmployeeType)
	if employeeType == "" {
		return fmt.Errorf("hrone %s position %s detail has an empty employeeType", s.domainCode, encryptedID)
	}

	locations := make([]string, 0, len(*detail.PreferredLocationList))
	for index, location := range *detail.PreferredLocationList {
		if location.ID == nil || location.Text == nil {
			return fmt.Errorf(
				"hrone %s position %s detail preferredLocationList row %d omitted id or text",
				s.domainCode, encryptedID, index,
			)
		}
		if text := strings.TrimSpace(*location.Text); text != "" {
			locations = append(locations, text)
		}
	}

	updated := *job
	updated.Title = title
	updated.Description = description
	updated.EmploymentType = employeeType
	updated.URL = s.applicationURL(encryptedID, strings.TrimSpace(*posting.DepartmentCode), sourceType)
	if location := strings.Join(distinctStrings(locations), "; "); location != "" {
		updated.Location = location
	}
	*job = updated
	return nil
}

func (s *hrone) doJSON(
	ctx context.Context,
	method, endpoint string,
	body any,
	bodyLimit int64,
	out any,
) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("domainCode", s.domainCode)
	req.Header.Set("apiKey", s.apiKey)
	req.Header.Set("AccessMode", "W")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s %s: %s: %s", method, endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		return fmt.Errorf("%s %s: reading response: %w", method, endpoint, err)
	}
	if int64(len(responseBody)) > bodyLimit {
		return fmt.Errorf("%s %s: response exceeds %d-byte safety limit", method, endpoint, bodyLimit)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", method, endpoint, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s %s: response contained trailing JSON", method, endpoint)
		}
		return fmt.Errorf("%s %s: decoding response trailer: %w", method, endpoint, err)
	}
	return nil
}

func canonicalHROneDomainCode(raw string) (string, error) {
	domainCode := strings.ToLower(strings.TrimSpace(raw))
	if !hroneDomainCodeRE.MatchString(domainCode) {
		return "", fmt.Errorf("invalid HROne domain code %q", raw)
	}
	return domainCode, nil
}

func canonicalHROneToken(raw string, minimumLength, maximumLength int) (string, error) {
	token := strings.TrimSpace(raw)
	if len(token) < minimumLength || len(token) > maximumLength || !hroneTokenRE.MatchString(token) {
		return "", fmt.Errorf(
			"expected a URL-safe token between %d and %d characters",
			minimumLength, maximumLength,
		)
	}
	return token, nil
}

func (s *hrone) jobID(encryptedPositionID string) string {
	return s.keyPrefix + encryptedPositionID
}

func (s *hrone) portalURL() string {
	query := url.Values{
		"appId": {s.apiKey},
		"dc":    {s.domainCode},
		"rqt":   {s.requestType},
		"cc":    {s.companyCode},
	}
	return strings.TrimRight(s.portalBase, "/") + "/career-portal?" + query.Encode()
}

func (s *hrone) applicationURL(encryptedPositionID, departmentCode, sourceType string) string {
	query := url.Values{
		"appId": {s.apiKey},
		"dc":    {s.domainCode},
		"rqt":   {s.requestType},
		"cc":    {s.companyCode},
		"pid":   {encryptedPositionID},
		"dptc":  {departmentCode},
		"st":    {sourceType},
		"fm":    {"CR"},
	}
	return strings.TrimRight(s.portalBase, "/") + "/apply-job?" + query.Encode()
}
