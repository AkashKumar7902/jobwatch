package source

// MakeMyTrip's careers front-end loads its complete public list from one
// anonymous first-party endpoint, then filters it in the browser. The list
// omits descriptions, which are fetched lazily from its detail endpoint.
//
//	GET https://careers.makemytrip.com/api/jobs
//	GET https://careers.makemytrip.com/api/jobDetails?jobId={id}

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	makeMyTripAPIBase    = "https://careers.makemytrip.com"
	makeMyTripSiteBase   = "https://careers.makemytrip.com/prod"
	makeMyTripMaxJobs    = 10000
	makeMyTripCountry    = "India"
	makeMyTripTimeLayout = "02-01-2006 15:04:05"
)

var makeMyTripJobIDRE = regexp.MustCompile(`^[0-9a-f]{14}$`)

// The API omits an offset, but its update timestamps track India Standard
// Time (an update returned at 10:09 UTC was stamped 15:39).
var makeMyTripTimeZone = time.FixedZone("IST", 5*60*60+30*60)

func init() {
	Register("makemytrip", func(company string, p params.Map, client *http.Client) (Source, error) {
		if len(p) != 0 {
			keys := make([]string, 0, len(p))
			for key := range p {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("makemytrip source does not accept params (got %s)", strings.Join(keys, ", "))
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &makeMyTrip{
			company:  company,
			apiBase:  makeMyTripAPIBase,
			siteBase: makeMyTripSiteBase,
			client:   client,
		}, nil
	})
}

type makeMyTrip struct {
	company  string
	apiBase  string
	siteBase string
	client   *http.Client
}

type makeMyTripListResponse struct {
	AllJobs       *[]makeMyTripPosting      `json:"allJobs"`
	BusinessUnits *[]makeMyTripBusinessUnit `json:"businessUnits"`
	Locations     *[]string                 `json:"locations"`
}

type makeMyTripBusinessUnit struct {
	Name  string `json:"name"`
	Count *int   `json:"count"`
}

type makeMyTripPosting struct {
	JobID               string    `json:"job_id"`
	JobCode             string    `json:"job_code"`
	GroupCompany        string    `json:"group_company"`
	BusinessUnit        string    `json:"business_unit"`
	JobTitle            string    `json:"job_title"`
	Location            *[]string `json:"location"`
	LocationCity        *[]string `json:"location_city"`
	LocationCountry     string    `json:"location_country"`
	PostOnCareersPage   *int      `json:"post_on_careers_page"`
	EmployeeType        string    `json:"employee_type"`
	JobCreatedTimestamp string    `json:"job_created_timestamp"`
	JobUpdatedTimestamp string    `json:"job_updated_timestamp"`
}

type makeMyTripDetailResponse struct {
	Status *int              `json:"status"`
	Error  string            `json:"error"`
	Data   *makeMyTripDetail `json:"data"`
}

type makeMyTripDetail struct {
	ApplyURL            string    `json:"applyUrl"`
	JobTitle            string    `json:"job_title"`
	GroupCompany        string    `json:"group_company"`
	EmployeeType        string    `json:"employee_type"`
	Location            *[]string `json:"location"`
	LocationCity        *[]string `json:"location_city"`
	LocationCountry     string    `json:"location_country"`
	JobCreatedTimestamp string    `json:"job_created_timestamp"`
	JobUpdatedTimestamp string    `json:"job_updated_timestamp"`
	JobDescription      string    `json:"job_decription"`
	JobStatus           string    `json:"job_status"`
}

func (s *makeMyTrip) Company() string { return s.company }

func (s *makeMyTrip) Fetch(ctx context.Context) ([]model.Job, error) {
	var response makeMyTripListResponse
	endpoint := strings.TrimRight(s.apiBase, "/") + "/api/jobs"
	if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &response); err != nil {
		return nil, fmt.Errorf("makemytrip: fetching complete list: %w", err)
	}
	if response.AllJobs == nil || response.BusinessUnits == nil || response.Locations == nil {
		return nil, fmt.Errorf("makemytrip: list omitted allJobs, businessUnits, or locations")
	}
	postings := *response.AllJobs
	if len(postings) > makeMyTripMaxJobs {
		return nil, fmt.Errorf("makemytrip: list returned %d jobs, exceeding hard limit %d", len(postings), makeMyTripMaxJobs)
	}

	jobs := make([]model.Job, 0, len(postings))
	seenIDs := make(map[string]struct{}, len(postings))
	actualUnits := make(map[string]int)
	actualCities := make(map[string]struct{})
	for index, posting := range postings {
		job, unit, cities, err := s.normalizePosting(posting)
		if err != nil {
			return nil, fmt.Errorf("makemytrip: item %d: %w", index, err)
		}
		externalID := strings.TrimPrefix(job.ID, "makemytrip/")
		if _, duplicate := seenIDs[externalID]; duplicate {
			return nil, fmt.Errorf("makemytrip: duplicate job_id %q", externalID)
		}
		seenIDs[externalID] = struct{}{}
		actualUnits[unit]++
		for _, city := range cities {
			actualCities[city] = struct{}{}
		}
		jobs = append(jobs, job)
	}
	if err := makeMyTripValidateBusinessUnits(*response.BusinessUnits, actualUnits); err != nil {
		return nil, err
	}
	if err := makeMyTripValidateLocations(*response.Locations, actualCities); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *makeMyTrip) normalizePosting(posting makeMyTripPosting) (model.Job, string, []string, error) {
	id := strings.TrimSpace(posting.JobID)
	if id != posting.JobID || !makeMyTripJobIDRE.MatchString(id) {
		return model.Job{}, "", nil, fmt.Errorf("invalid job_id %q", id)
	}
	if strings.TrimSpace(posting.JobCode) == "" {
		return model.Job{}, "", nil, fmt.Errorf("job %s has empty job_code", id)
	}
	title := strings.TrimSpace(posting.JobTitle)
	if title == "" {
		return model.Job{}, "", nil, fmt.Errorf("job %s has empty job_title", id)
	}
	if posting.PostOnCareersPage == nil || *posting.PostOnCareersPage != 1 {
		return model.Job{}, "", nil, fmt.Errorf("job %s has post_on_careers_page %s, want 1", id, makeMyTripIntValue(posting.PostOnCareersPage))
	}
	if strings.TrimSpace(posting.LocationCountry) != makeMyTripCountry {
		return model.Job{}, "", nil, fmt.Errorf("job %s has location_country %q, want %q", id, posting.LocationCountry, makeMyTripCountry)
	}
	if strings.TrimSpace(posting.GroupCompany) == "" {
		return model.Job{}, "", nil, fmt.Errorf("job %s has empty group_company", id)
	}
	unit := strings.TrimSpace(posting.BusinessUnit)
	if unit == "" {
		return model.Job{}, "", nil, fmt.Errorf("job %s has empty business_unit", id)
	}
	employmentType := strings.TrimSpace(posting.EmployeeType)
	if employmentType == "" {
		return model.Job{}, "", nil, fmt.Errorf("job %s has empty employee_type", id)
	}
	locations, err := makeMyTripRequiredDistinctStrings(posting.Location, "location")
	if err != nil {
		return model.Job{}, "", nil, fmt.Errorf("job %s: %w", id, err)
	}
	cities, err := makeMyTripRequiredDistinctStrings(posting.LocationCity, "location_city")
	if err != nil {
		return model.Job{}, "", nil, fmt.Errorf("job %s: %w", id, err)
	}
	createdAt, err := makeMyTripParseTimestamp(posting.JobCreatedTimestamp)
	if err != nil {
		return model.Job{}, "", nil, fmt.Errorf("job %s has invalid job_created_timestamp %q", id, posting.JobCreatedTimestamp)
	}
	updatedAt, err := makeMyTripParseTimestamp(posting.JobUpdatedTimestamp)
	if err != nil {
		return model.Job{}, "", nil, fmt.Errorf("job %s has invalid job_updated_timestamp %q", id, posting.JobUpdatedTimestamp)
	}
	if updatedAt.Before(createdAt) {
		return model.Job{}, "", nil, fmt.Errorf("job %s was updated before it was created", id)
	}
	publicURL := strings.TrimRight(s.siteBase, "/") + "/opportunity/" + id + "/" +
		url.PathEscape(makeMyTripSlug(title))
	return model.Job{
		ID:             "makemytrip/" + id,
		Company:        s.company,
		Title:          title,
		Location:       strings.Join(locations, "; "),
		URL:            publicURL,
		EmploymentType: employmentType,
		PostedAt:       createdAt,
	}, unit, cities, nil
}

func (s *makeMyTrip) Detail(ctx context.Context, job *model.Job) error {
	const prefix = "makemytrip/"
	if job == nil {
		return fmt.Errorf("makemytrip: nil job")
	}
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("makemytrip: job id %q does not have prefix %q", job.ID, prefix)
	}
	postingID := strings.TrimPrefix(job.ID, prefix)
	if !makeMyTripJobIDRE.MatchString(postingID) {
		return fmt.Errorf("makemytrip: job id has invalid posting id %q", postingID)
	}
	query := url.Values{"jobId": {postingID}}
	endpoint := strings.TrimRight(s.apiBase, "/") + "/api/jobDetails?" + query.Encode()
	var response makeMyTripDetailResponse
	if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &response); err != nil {
		return fmt.Errorf("makemytrip: detail %s: %w", postingID, err)
	}
	if strings.TrimSpace(response.Error) != "" {
		return fmt.Errorf("makemytrip: detail %s returned error %q", postingID, response.Error)
	}
	if response.Status == nil || *response.Status != 1 || response.Data == nil {
		return fmt.Errorf("makemytrip: detail %s omitted successful status or data", postingID)
	}
	detail := response.Data
	if strings.TrimSpace(detail.JobStatus) != "OPEN" {
		return fmt.Errorf("makemytrip: detail %s has job_status %q, want OPEN", postingID, detail.JobStatus)
	}
	if strings.TrimSpace(detail.JobTitle) != job.Title {
		return fmt.Errorf("makemytrip: detail %s title %q does not match list title %q", postingID, detail.JobTitle, job.Title)
	}
	if strings.TrimSpace(detail.LocationCountry) != makeMyTripCountry {
		return fmt.Errorf("makemytrip: detail %s has location_country %q, want %q", postingID, detail.LocationCountry, makeMyTripCountry)
	}
	if strings.TrimSpace(detail.GroupCompany) == "" {
		return fmt.Errorf("makemytrip: detail %s has empty group_company", postingID)
	}
	locations, err := makeMyTripRequiredDistinctStrings(detail.Location, "location")
	if err != nil {
		return fmt.Errorf("makemytrip: detail %s: %w", postingID, err)
	}
	if location := strings.Join(locations, "; "); location != job.Location {
		return fmt.Errorf("makemytrip: detail %s location %q does not match list location %q", postingID, location, job.Location)
	}
	if _, err := makeMyTripRequiredDistinctStrings(detail.LocationCity, "location_city"); err != nil {
		return fmt.Errorf("makemytrip: detail %s: %w", postingID, err)
	}
	employmentType := strings.TrimSpace(detail.EmployeeType)
	if employmentType == "" || employmentType != job.EmploymentType {
		return fmt.Errorf("makemytrip: detail %s employee_type %q does not match list value %q", postingID, detail.EmployeeType, job.EmploymentType)
	}
	createdAt, err := makeMyTripParseTimestamp(detail.JobCreatedTimestamp)
	if err != nil {
		return fmt.Errorf("makemytrip: detail %s has invalid job_created_timestamp %q", postingID, detail.JobCreatedTimestamp)
	}
	updatedAt, err := makeMyTripParseTimestamp(detail.JobUpdatedTimestamp)
	if err != nil {
		return fmt.Errorf("makemytrip: detail %s has invalid job_updated_timestamp %q", postingID, detail.JobUpdatedTimestamp)
	}
	if updatedAt.Before(createdAt) {
		return fmt.Errorf("makemytrip: detail %s was updated before it was created", postingID)
	}
	if !createdAt.Equal(job.PostedAt) {
		return fmt.Errorf("makemytrip: detail %s created timestamp does not match list", postingID)
	}
	expectedApplyURL := "https://gommt.darwinbox.in/ms/candidatev2/main/careers/jobDetails/" + postingID + "?from=all"
	if strings.TrimSpace(detail.ApplyURL) != expectedApplyURL {
		return fmt.Errorf("makemytrip: detail %s has invalid applyUrl %q", postingID, detail.ApplyURL)
	}
	description := htmltext.ToText(detail.JobDescription)
	if description == "" {
		return fmt.Errorf("makemytrip: detail %s has empty job_decription", postingID)
	}

	job.Description = description
	return nil
}

func makeMyTripValidateBusinessUnits(facets []makeMyTripBusinessUnit, actual map[string]int) error {
	declared := make(map[string]int, len(facets))
	for index, facet := range facets {
		name := strings.TrimSpace(facet.Name)
		if name == "" || facet.Count == nil || *facet.Count < 0 {
			return fmt.Errorf("makemytrip: businessUnits item %d has invalid name or count", index)
		}
		if _, duplicate := declared[name]; duplicate {
			return fmt.Errorf("makemytrip: duplicate businessUnits name %q", name)
		}
		declared[name] = *facet.Count
	}
	if len(declared) != len(actual) {
		return fmt.Errorf("makemytrip: businessUnits has %d groups, jobs contain %d", len(declared), len(actual))
	}
	for name, count := range actual {
		if declaredCount, ok := declared[name]; !ok || declaredCount != count {
			return fmt.Errorf("makemytrip: businessUnits count for %q is %d, jobs contain %d", name, declaredCount, count)
		}
	}
	return nil
}

func makeMyTripValidateLocations(locations []string, actual map[string]struct{}) error {
	declared := make(map[string]struct{}, len(locations))
	for index, location := range locations {
		location = strings.TrimSpace(location)
		if location == "" {
			return fmt.Errorf("makemytrip: locations item %d is empty", index)
		}
		if _, duplicate := declared[location]; duplicate {
			return fmt.Errorf("makemytrip: duplicate locations value %q", location)
		}
		declared[location] = struct{}{}
	}
	if len(declared) != len(actual) {
		return fmt.Errorf("makemytrip: locations has %d values, jobs contain %d cities", len(declared), len(actual))
	}
	for city := range actual {
		if _, ok := declared[city]; !ok {
			return fmt.Errorf("makemytrip: locations omitted job city %q", city)
		}
	}
	return nil
}

func makeMyTripRequiredDistinctStrings(values *[]string, field string) ([]string, error) {
	if values == nil || len(*values) == 0 {
		return nil, fmt.Errorf("omitted or empty %s", field)
	}
	result := make([]string, 0, len(*values))
	seen := make(map[string]struct{}, len(*values))
	for index, value := range *values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s item %d is empty", field, index)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("%s contains duplicate %q", field, value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func makeMyTripParseTimestamp(value string) (time.Time, error) {
	parsed, err := time.ParseInLocation(makeMyTripTimeLayout, strings.TrimSpace(value), makeMyTripTimeZone)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func makeMyTripSlug(title string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || unicode.IsSpace(r) {
			return '-'
		}
		return unicode.ToLower(r)
	}, title)
}

func makeMyTripIntValue(value *int) string {
	if value == nil {
		return "<missing>"
	}
	return fmt.Sprint(*value)
}
