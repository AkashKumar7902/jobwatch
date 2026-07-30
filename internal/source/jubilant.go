package source

// Jubilant Bhartia Group's careers site exposes its complete public job list
// and individual descriptions through anonymous first-party JSON endpoints.
// The supplied company catalog is India-focused, so this source requires an
// explicit ISO-3 country scope rather than silently importing the whole
// Canada/India/United States board.
//
//	GET https://jubilantcareer.jubl.com/JubilantCareersPortal/rest/Portal/getAllJobs
//	GET https://jubilantcareer.jubl.com/JubilantCareersPortal/rest/Portal/getJobDetails/{id}

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	jubilantAPIBase         = "https://jubilantcareer.jubl.com/JubilantCareersPortal/rest/Portal"
	jubilantSiteBase        = "https://jubilantcareer.jubl.com"
	jubilantMaxJobs         = 10000
	jubilantListBodyLimit   = 8 << 20
	jubilantDetailBodyLimit = 8 << 20
	jubilantActiveStatus    = "010"
)

var (
	jubilantCountryRE = regexp.MustCompile(`^[A-Z]{3}$`)
	jubilantJobIDRE   = regexp.MustCompile(`^[1-9][0-9]*$`)
)

func init() {
	Register("jubilant", func(company string, p params.Map, client *http.Client) (Source, error) {
		for key := range p {
			if key != "country_code" {
				return nil, fmt.Errorf("jubilant source: unsupported param %q", key)
			}
		}
		country := strings.ToUpper(strings.TrimSpace(p.GetDefault("country_code", "IND")))
		if !jubilantCountryRE.MatchString(country) {
			return nil, fmt.Errorf("param %q: expected ISO-3 country code, got %q", "country_code", country)
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &jubilant{
			company:  company,
			country:  country,
			apiBase:  jubilantAPIBase,
			siteBase: jubilantSiteBase,
			client:   client,
		}, nil
	})
}

type jubilant struct {
	company  string
	country  string
	apiBase  string
	siteBase string
	client   *http.Client
}

type jubilantListResponse struct {
	JobList      *[]jubilantListJob  `json:"jobList"`
	CompanyList  *[]jubilantCompany  `json:"companyList"`
	CountryList  *[]jubilantCountry  `json:"countryList"`
	FunctionList *[]jubilantFunction `json:"functionList"`
	LocationList *[]jubilantLocation `json:"locationList"`
}

type jubilantListJob struct {
	JobID               string `json:"jobId"`
	JobCountry          string `json:"jobCountry"`
	CountryName         string `json:"CountryName"`
	FunctionalArea      string `json:"functionalArea"`
	JobTitle            string `json:"jobTitle"`
	LocationDescription string `json:"locationDescription"`
	Company             string `json:"company"`
}

type jubilantCompany struct {
	Code string `json:"companyCode"`
	Name string `json:"company"`
}

type jubilantCountry struct {
	Name string `json:"country"`
	Code string `json:"countryCode"`
}

type jubilantFunction struct {
	Description string `json:"funtionDesc"`
	Name        string `json:"funtion"`
}

type jubilantLocation struct {
	Description string `json:"locationDesc"`
	Name        string `json:"location"`
}

type jubilantDetailResponse struct {
	Country         string `json:"country"`
	JobPostingDate  string `json:"jobpostingdate"`
	JobTitle        string `json:"jobtitle"`
	Function        string `json:"funct"`
	Location        string `json:"locationdescr"`
	Company         string `json:"companydescr"`
	DescriptionHTML string `json:"jobdescr"`
	JobOpeningID    *int   `json:"jobOpeningId"`
	Status          string `json:"status"`
}

func (s *jubilant) Company() string { return s.company }

func (s *jubilant) Fetch(ctx context.Context) ([]model.Job, error) {
	var response jubilantListResponse
	if err := s.getJSON(ctx, "/getAllJobs", jubilantListBodyLimit, &response); err != nil {
		return nil, fmt.Errorf("jubilant %s: complete list: %w", s.country, err)
	}
	if response.JobList == nil || response.CompanyList == nil || response.CountryList == nil ||
		response.FunctionList == nil || response.LocationList == nil {
		return nil, fmt.Errorf("jubilant %s: list omitted jobs or a required facet", s.country)
	}
	postings := *response.JobList
	if len(postings) > jubilantMaxJobs {
		return nil, fmt.Errorf("jubilant %s: list returned %d jobs, exceeding hard limit %d", s.country, len(postings), jubilantMaxJobs)
	}

	companies, err := jubilantCompanyFacet(*response.CompanyList)
	if err != nil {
		return nil, fmt.Errorf("jubilant %s: %w", s.country, err)
	}
	countries, err := jubilantCountryFacet(*response.CountryList)
	if err != nil {
		return nil, fmt.Errorf("jubilant %s: %w", s.country, err)
	}
	functions, err := jubilantFunctionFacet(*response.FunctionList)
	if err != nil {
		return nil, fmt.Errorf("jubilant %s: %w", s.country, err)
	}
	locations, err := jubilantLocationFacet(*response.LocationList)
	if err != nil {
		return nil, fmt.Errorf("jubilant %s: %w", s.country, err)
	}
	if _, ok := countries[s.country]; !ok {
		return nil, fmt.Errorf("jubilant %s: country is absent from countryList", s.country)
	}

	jobs := make([]model.Job, 0)
	seen := make(map[string]struct{}, len(postings))
	for index, posting := range postings {
		id := strings.TrimSpace(posting.JobID)
		if !jubilantJobIDRE.MatchString(id) || id != posting.JobID {
			return nil, fmt.Errorf("jubilant %s: job %d has invalid jobId %q", s.country, index, posting.JobID)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("jubilant %s: duplicate jobId %q", s.country, id)
		}
		seen[id] = struct{}{}

		country := jubilantCleanScalar(posting.JobCountry)
		countryName := jubilantCleanScalar(posting.CountryName)
		expectedCountryName, knownCountry := countries[country]
		if !knownCountry || countryName != expectedCountryName {
			return nil, fmt.Errorf("jubilant %s: job %s has unknown or inconsistent country %q/%q", s.country, id, country, countryName)
		}
		title := jubilantCleanScalar(posting.JobTitle)
		company := jubilantCleanScalar(posting.Company)
		function := jubilantCleanScalar(posting.FunctionalArea)
		location := jubilantCleanScalar(posting.LocationDescription)
		if title == "" || company == "" || function == "" || location == "" {
			return nil, fmt.Errorf("jubilant %s: job %s has an empty title, company, function, or location", s.country, id)
		}
		if _, ok := companies[company]; !ok {
			return nil, fmt.Errorf("jubilant %s: job %s has company %q absent from companyList", s.country, id, company)
		}
		if _, ok := functions[function]; !ok {
			return nil, fmt.Errorf("jubilant %s: job %s has function %q absent from functionList", s.country, id, function)
		}
		if _, ok := locations[location]; !ok {
			return nil, fmt.Errorf("jubilant %s: job %s has location %q absent from locationList", s.country, id, location)
		}
		if country != s.country {
			continue
		}

		jobs = append(jobs, model.Job{
			ID:       "jubilant/" + s.country + "/" + id,
			Company:  s.company,
			Title:    title,
			Location: location,
			URL:      strings.TrimRight(s.siteBase, "/") + "/jobprofile/" + id + "/home",
		})
	}
	return jobs, nil
}

func (s *jubilant) Detail(ctx context.Context, job *model.Job) error {
	prefix := "jubilant/" + s.country + "/"
	if job == nil {
		return fmt.Errorf("jubilant %s: nil job", s.country)
	}
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("jubilant %s: job id %q does not have prefix %q", s.country, job.ID, prefix)
	}
	id := strings.TrimPrefix(job.ID, prefix)
	if !jubilantJobIDRE.MatchString(id) {
		return fmt.Errorf("jubilant %s: job id has invalid external id %q", s.country, id)
	}

	var detail jubilantDetailResponse
	if err := s.getJSON(ctx, "/getJobDetails/"+id, jubilantDetailBodyLimit, &detail); err != nil {
		return fmt.Errorf("jubilant %s: detail %s: %w", s.country, id, err)
	}
	numericID, _ := strconv.Atoi(id)
	if detail.JobOpeningID == nil || *detail.JobOpeningID != numericID {
		return fmt.Errorf("jubilant %s: detail %s omitted or changed jobOpeningId", s.country, id)
	}
	if strings.TrimSpace(detail.Status) != jubilantActiveStatus {
		return fmt.Errorf("jubilant %s: detail %s has status %q, want %q", s.country, id, detail.Status, jubilantActiveStatus)
	}
	if jubilantCleanScalar(detail.Country) != s.country {
		return fmt.Errorf("jubilant %s: detail %s has country %q", s.country, id, detail.Country)
	}
	if title := jubilantCleanScalar(detail.JobTitle); title == "" || title != job.Title {
		return fmt.Errorf("jubilant %s: detail %s title %q does not match list title %q", s.country, id, title, job.Title)
	}
	if location := jubilantCleanScalar(detail.Location); location == "" || location != job.Location {
		return fmt.Errorf("jubilant %s: detail %s location %q does not match list location %q", s.country, id, location, job.Location)
	}
	if jubilantCleanScalar(detail.Company) == "" || jubilantCleanScalar(detail.Function) == "" {
		return fmt.Errorf("jubilant %s: detail %s omitted company or function", s.country, id)
	}
	description := htmltext.ToText(detail.DescriptionHTML)
	if description == "" {
		return fmt.Errorf("jubilant %s: detail %s has empty jobdescr", s.country, id)
	}
	postedAt, err := time.Parse("02/01/06", strings.TrimSpace(detail.JobPostingDate))
	if err != nil {
		return fmt.Errorf("jubilant %s: detail %s has invalid jobpostingdate %q", s.country, id, detail.JobPostingDate)
	}

	job.Description = description
	job.PostedAt = postedAt
	return nil
}

func (s *jubilant) getJSON(ctx context.Context, path string, limit int64, out any) error {
	endpoint := strings.TrimRight(s.apiBase, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating GET %s: %w", endpoint, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf("GET %s: unexpected Content-Type %q", endpoint, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
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

func jubilantCompanyFacet(values []jubilantCompany) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(values))
	for index, value := range values {
		code := strings.TrimSpace(value.Code)
		name := jubilantCleanScalar(value.Name)
		if code == "" || name == "" {
			return nil, fmt.Errorf("companyList item %d has an empty code or name", index)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("companyList contains duplicate company %q", name)
		}
		result[name] = struct{}{}
	}
	return result, nil
}

func jubilantCountryFacet(values []jubilantCountry) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for index, value := range values {
		code := strings.TrimSpace(value.Code)
		name := jubilantCleanScalar(value.Name)
		if !jubilantCountryRE.MatchString(code) || name == "" {
			return nil, fmt.Errorf("countryList item %d has an invalid code or name", index)
		}
		if _, duplicate := result[code]; duplicate {
			return nil, fmt.Errorf("countryList contains duplicate code %q", code)
		}
		result[code] = name
	}
	return result, nil
}

func jubilantFunctionFacet(values []jubilantFunction) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(values))
	for index, value := range values {
		name := jubilantCleanScalar(value.Name)
		description := jubilantCleanScalar(value.Description)
		if name == "" || description != name {
			return nil, fmt.Errorf("functionList item %d has an empty or inconsistent name", index)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("functionList contains duplicate function %q", name)
		}
		result[name] = struct{}{}
	}
	return result, nil
}

func jubilantLocationFacet(values []jubilantLocation) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(values))
	for index, value := range values {
		name := jubilantCleanScalar(value.Name)
		description := jubilantCleanScalar(value.Description)
		if name == "" || description != name {
			return nil, fmt.Errorf("locationList item %d has an empty or inconsistent name", index)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("locationList contains duplicate location %q", name)
		}
		result[name] = struct{}{}
	}
	return result, nil
}

func jubilantCleanScalar(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
