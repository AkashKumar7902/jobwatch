package source

// Walmart's first-party careers search returns complete job postings from a
// single anonymous JSON endpoint. The query and the response's server-applied
// filters are both fixed here: changing either could silently broaden the
// source from Walmart Global Tech India to Walmart's worldwide job board.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	walmartSearchURL = "https://careers.walmart.com/api/ai/search-ai/api/v1/combined/hybrid-search?page=0&size=1000&locale=en_US"
	walmartPublicURL = "https://careers.walmart.com/in/en/jobs/"
	walmartQuery     = "Walmart Global Tech India"
	walmartLocale    = "en_US"
	walmartMaxJobs   = 1000
	walmartBodyLimit = 64 << 20
)

var (
	walmartFilterRE = regexp.MustCompile(
		`^\s*([A-Za-z][A-Za-z0-9]*)\s*==\s*'([^']*)'\s+AND\s+([A-Za-z][A-Za-z0-9]*)\s*==\s*'([^']*)'\s*$`,
	)
	walmartJobIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func init() {
	Register("walmart", func(company string, _ params.Map, client *http.Client) (Source, error) {
		if client == nil {
			client = http.DefaultClient
		}
		return &walmart{
			company:  company,
			endpoint: walmartSearchURL,
			client:   client,
		}, nil
	})
}

type walmart struct {
	company  string
	endpoint string
	client   *http.Client
}

type walmartSearchRequest struct {
	Query       string `json:"query"`
	BasicSearch bool   `json:"basicSearch"`
	Filter      string `json:"filter"`
	Locale      string `json:"locale"`
}

type walmartSearchResponse struct {
	Query              *string                `json:"query"`
	JobFilters         *string                `json:"jobFilters"`
	JobSearchSucceeded *bool                  `json:"jobSearchSucceeded"`
	TotalJobs          *int                   `json:"totalJobs"`
	Jobs               *[]walmartSearchResult `json:"jobs"`
}

type walmartSearchResult struct {
	ID       string           `json:"id"`
	Text     string           `json:"text"`
	Metadata *walmartMetadata `json:"metadata"`
}

type walmartMetadata struct {
	JobID                  string           `json:"jobId"`
	Title                  string           `json:"title"`
	JobPostingTitle        string           `json:"jobPostingTitle"`
	Location               *walmartLocation `json:"location"`
	PrimaryLocationCity    string           `json:"primaryLocationCity"`
	PrimaryLocationState   string           `json:"primaryLocationState"`
	PrimaryLocationCountry string           `json:"primaryLocationCountry"`
	TimeType               *string          `json:"timeType"`
	PositionWorkerType     string           `json:"positionWorkerType"`
	JobPostingStartDate    *int64           `json:"jobPostingStartDate"`
	Population             *string          `json:"population"`
	Brand                  string           `json:"brand"`
	ReqType                string           `json:"reqType"`
}

type walmartLocation struct {
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
}

type walmartExternalSignature struct {
	Job                model.Job
	SearchTitle        string
	TimeType           string
	TimeTypePresent    bool
	PositionWorkerType string
	Population         string
	PopulationPresent  bool
	Latitude           float64
	Longitude          float64
}

func (s *walmart) Company() string { return s.company }

func (s *walmart) Fetch(ctx context.Context) ([]model.Job, error) {
	response, err := s.search(ctx)
	if err != nil {
		return nil, err
	}
	if response.Query == nil || strings.TrimSpace(*response.Query) != walmartQuery {
		return nil, fmt.Errorf("walmart: response omitted or changed query")
	}
	if response.JobSearchSucceeded == nil || !*response.JobSearchSucceeded {
		return nil, fmt.Errorf("walmart: response did not report a successful job search")
	}
	if response.JobFilters == nil || !walmartFilterMatches(*response.JobFilters) {
		return nil, fmt.Errorf("walmart: unsafe or missing jobFilters %q", walmartStringValue(response.JobFilters))
	}
	if response.TotalJobs == nil || response.Jobs == nil {
		return nil, fmt.Errorf("walmart: response omitted totalJobs or jobs")
	}
	total := *response.TotalJobs
	if total < 0 {
		return nil, fmt.Errorf("walmart: response reported negative totalJobs %d", total)
	}
	if total > walmartMaxJobs {
		return nil, fmt.Errorf("walmart: totalJobs %d exceeds hard limit %d", total, walmartMaxJobs)
	}
	results := *response.Jobs
	if len(results) > walmartMaxJobs {
		return nil, fmt.Errorf("walmart: response returned %d jobs, exceeding hard limit %d", len(results), walmartMaxJobs)
	}
	if len(results) != total {
		return nil, fmt.Errorf("walmart: incomplete response returned %d jobs, totalJobs is %d", len(results), total)
	}

	jobs := make([]model.Job, 0, len(results))
	seenExternal := make(map[string]walmartExternalSignature)
	for index, result := range results {
		kind, jobID, err := walmartResultKind(result)
		if err != nil {
			return nil, fmt.Errorf("walmart: result %d: %w", index, err)
		}
		if kind == "Internal" {
			continue
		}
		job, signature, err := s.normalizeExternal(result, jobID)
		if err != nil {
			return nil, fmt.Errorf("walmart: result %d: %w", index, err)
		}
		if previous, duplicate := seenExternal[jobID]; duplicate {
			if previous != signature {
				return nil, fmt.Errorf("walmart: conflicting external records for jobId %q", jobID)
			}
			continue
		}
		seenExternal[jobID] = signature
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *walmart) search(ctx context.Context) (walmartSearchResponse, error) {
	payload, err := json.Marshal(walmartSearchRequest{
		Query: walmartQuery, BasicSearch: false, Filter: "", Locale: walmartLocale,
	})
	if err != nil {
		return walmartSearchResponse{}, fmt.Errorf("walmart: encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return walmartSearchResponse{}, fmt.Errorf("walmart: creating request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return walmartSearchResponse{}, fmt.Errorf("walmart: POST search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return walmartSearchResponse{}, fmt.Errorf("walmart: POST search: %s: %s", resp.Status, bytes.TrimSpace(snippet))
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return walmartSearchResponse{}, fmt.Errorf("walmart: POST search: unexpected Content-Type %q", resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, walmartBodyLimit+1))
	if err != nil {
		return walmartSearchResponse{}, fmt.Errorf("walmart: POST search: reading response: %w", err)
	}
	if len(body) > walmartBodyLimit {
		return walmartSearchResponse{}, fmt.Errorf("walmart: POST search: response exceeds %d bytes", walmartBodyLimit)
	}
	var response walmartSearchResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&response); err != nil {
		return walmartSearchResponse{}, fmt.Errorf("walmart: POST search: decoding response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return walmartSearchResponse{}, fmt.Errorf("walmart: POST search: response contains trailing JSON")
		}
		return walmartSearchResponse{}, fmt.Errorf("walmart: POST search: decoding trailing response: %w", err)
	}
	return response, nil
}

func walmartResultKind(result walmartSearchResult) (kind, jobID string, err error) {
	if result.Metadata == nil {
		return "", "", fmt.Errorf("record %q omitted metadata", strings.TrimSpace(result.ID))
	}
	jobID = strings.TrimSpace(result.Metadata.JobID)
	if !walmartJobIDRE.MatchString(jobID) {
		return "", "", fmt.Errorf("record %q has invalid jobId %q", strings.TrimSpace(result.ID), jobID)
	}
	outerID := strings.TrimSpace(result.ID)
	switch outerID {
	case jobID + "-External":
		kind = "External"
	case jobID + "-Internal":
		kind = "Internal"
	default:
		return "", "", fmt.Errorf("record id %q does not match jobId %q with an External/Internal suffix", outerID, jobID)
	}
	if strings.TrimSpace(result.Metadata.ReqType) != kind {
		return "", "", fmt.Errorf("record %q reqType %q does not match %s suffix", outerID, result.Metadata.ReqType, kind)
	}
	return kind, jobID, nil
}

func (s *walmart) normalizeExternal(
	result walmartSearchResult,
	jobID string,
) (model.Job, walmartExternalSignature, error) {
	metadata := result.Metadata
	if metadata == nil {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q omitted metadata", jobID)
	}
	searchTitle := strings.TrimSpace(metadata.Title)
	title := strings.TrimSpace(metadata.JobPostingTitle)
	if searchTitle == "" || title == "" {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q omitted title or jobPostingTitle", jobID)
	}
	description := strings.TrimSpace(result.Text)
	if description == "" {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q has an empty description", jobID)
	}
	if strings.TrimSpace(metadata.Brand) != "Walmart" {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q has unexpected brand %q", jobID, metadata.Brand)
	}
	country := strings.TrimSpace(metadata.PrimaryLocationCountry)
	if country != "IN" {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q has country %q, want IN", jobID, country)
	}
	city := strings.TrimSpace(metadata.PrimaryLocationCity)
	state := strings.TrimSpace(metadata.PrimaryLocationState)
	if city == "" || state == "" {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q omitted primary location city or state", jobID)
	}
	if metadata.Location == nil || metadata.Location.Lat == nil || metadata.Location.Lon == nil {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q omitted location coordinates", jobID)
	}
	latitude, longitude := *metadata.Location.Lat, *metadata.Location.Lon
	if math.IsNaN(latitude) || math.IsInf(latitude, 0) || latitude < -90 || latitude > 90 ||
		math.IsNaN(longitude) || math.IsInf(longitude, 0) || longitude < -180 || longitude > 180 {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q has invalid location coordinates", jobID)
	}
	positionWorkerType := strings.TrimSpace(metadata.PositionWorkerType)
	if positionWorkerType == "" {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q omitted positionWorkerType", jobID)
	}
	if metadata.JobPostingStartDate == nil || *metadata.JobPostingStartDate < 946684800000 ||
		*metadata.JobPostingStartDate >= 7258118400000 {
		return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q has invalid jobPostingStartDate", jobID)
	}
	employmentType := ""
	timeType := ""
	timeTypePresent := metadata.TimeType != nil
	if metadata.TimeType != nil {
		timeType = strings.TrimSpace(*metadata.TimeType)
		if timeType == "" {
			return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q has an empty timeType", jobID)
		}
		employmentType = timeType
	}
	if employmentType == "" {
		employmentType = positionWorkerType
	}
	population := ""
	populationPresent := metadata.Population != nil
	if populationPresent {
		population = strings.TrimSpace(*metadata.Population)
		if population == "" {
			return model.Job{}, walmartExternalSignature{}, fmt.Errorf("jobId %q has an empty population", jobID)
		}
	}
	job := model.Job{
		ID:             "walmart/" + jobID,
		Company:        s.company,
		Title:          title,
		Location:       strings.Join([]string{city, state, country}, ", "),
		URL:            walmartPublicURL + jobID,
		EmploymentType: employmentType,
		Description:    description,
		PostedAt:       time.UnixMilli(*metadata.JobPostingStartDate).UTC(),
	}
	signature := walmartExternalSignature{
		Job: job, SearchTitle: searchTitle,
		TimeType: timeType, TimeTypePresent: timeTypePresent,
		PositionWorkerType: positionWorkerType,
		Population:         population, PopulationPresent: populationPresent,
		Latitude: latitude, Longitude: longitude,
	}
	return job, signature, nil
}

func walmartFilterMatches(raw string) bool {
	match := walmartFilterRE.FindStringSubmatch(raw)
	if match == nil {
		return false
	}
	clauses := map[string]string{}
	for i := 1; i < len(match); i += 2 {
		if _, duplicate := clauses[match[i]]; duplicate {
			return false
		}
		clauses[match[i]] = match[i+1]
	}
	return len(clauses) == 2 &&
		clauses["brand"] == "Walmart" &&
		clauses["primaryLocationCountry"] == "IN"
}

func walmartStringValue(value *string) string {
	if value == nil {
		return "<missing>"
	}
	return *value
}
