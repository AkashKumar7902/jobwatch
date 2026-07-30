package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestWalmartRegistrationRequestAndNormalization(t *testing.T) {
	response := walmartFixtureResponse(
		walmartFixtureResult("R-1001", "External"),
		walmartFixtureResult("R-1001", "Internal"),
		walmartFixtureResult("R-1002", "External"),
	)
	(*response.Jobs)[1] = walmartSearchResult{
		ID: "R-1001-Internal",
		Metadata: &walmartMetadata{
			JobID: "R-1001", ReqType: "Internal",
		},
	}
	second := &(*response.Jobs)[2]
	second.Metadata.JobPostingTitle = "Principal Engineer"
	second.Metadata.Title = "Principal Engineer"
	second.Metadata.PrimaryLocationCity = "CHENNAI"
	second.Metadata.PrimaryLocationState = "TN"
	second.Metadata.TimeType = nil
	second.Metadata.PositionWorkerType = "Regular/Permanent"
	second.Metadata.Population = nil
	second.Metadata.JobPostingStartDate = walmartInt64(1785283200000)
	second.Text = "  Lead a platform team.  "

	var requestCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCalls++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/ai/search-ai/api/v1/combined/hybrid-search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		query := r.URL.Query()
		if len(query) != 3 || query.Get("page") != "0" || query.Get("size") != "1000" || query.Get("locale") != walmartLocale {
			t.Errorf("query = %v", query)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent is empty")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		const expectedBody = `{"query":"Walmart Global Tech India","basicSearch":false,"filter":"","locale":"en_US"}`
		if string(body) != expectedBody {
			t.Errorf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	src, err := New("walmart", "Walmart Global Tech India", params.Map{}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("New returned %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*walmart)
	if !ok {
		t.Fatalf("wrapped source = %T, want *walmart", wrapped.Source)
	}
	implementation.endpoint = server.URL + "/api/ai/search-ai/api/v1/combined/hybrid-search?page=0&size=1000&locale=en_US"

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requestCalls != 1 {
		t.Fatalf("request calls = %d, want 1", requestCalls)
	}
	want := []model.Job{
		{
			ID:             "walmart/R-1001",
			Company:        "Walmart Global Tech India",
			Title:          "Senior Software Engineer",
			Location:       "BENGALURU, KA, IN",
			URL:            "https://careers.walmart.com/in/en/jobs/R-1001",
			EmploymentType: "Full time",
			Description:    "Build reliable distributed systems.",
			PostedAt:       time.UnixMilli(1782691200000).UTC(),
		},
		{
			ID:             "walmart/R-1002",
			Company:        "Walmart Global Tech India",
			Title:          "Principal Engineer",
			Location:       "CHENNAI, TN, IN",
			URL:            "https://careers.walmart.com/in/en/jobs/R-1002",
			EmploymentType: "Regular/Permanent",
			Description:    "Lead a platform team.",
			PostedAt:       time.UnixMilli(1785283200000).UTC(),
		},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs =\n%+v\nwant\n%+v", jobs, want)
	}
}

func TestWalmartDeduplicatesIdenticalExternalRecords(t *testing.T) {
	result := walmartFixtureResult("R-2001", "External")
	response := walmartFixtureResponse(result, result)
	jobs, err := walmartFetchFixture(t, response)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "walmart/R-2001" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestWalmartAllowsAnEmptyCompleteBoard(t *testing.T) {
	response := walmartFixtureResponse()
	jobs, err := walmartFetchFixture(t, response)
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
	}
}

func TestWalmartRejectsUnsafeIncompleteOrDriftedResponses(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*walmartSearchResponse)
		wantErr string
	}{
		{
			name: "missing query",
			mutate: func(response *walmartSearchResponse) {
				response.Query = nil
			},
			wantErr: "changed query",
		},
		{
			name: "changed query",
			mutate: func(response *walmartSearchResponse) {
				response.Query = walmartString("Walmart")
			},
			wantErr: "changed query",
		},
		{
			name: "missing success flag",
			mutate: func(response *walmartSearchResponse) {
				response.JobSearchSucceeded = nil
			},
			wantErr: "successful job search",
		},
		{
			name: "failed job search",
			mutate: func(response *walmartSearchResponse) {
				response.JobSearchSucceeded = walmartBool(false)
			},
			wantErr: "successful job search",
		},
		{
			name: "missing filters",
			mutate: func(response *walmartSearchResponse) {
				response.JobFilters = nil
			},
			wantErr: "unsafe or missing jobFilters",
		},
		{
			name: "widened filters",
			mutate: func(response *walmartSearchResponse) {
				response.JobFilters = walmartString("brand == 'Walmart'")
			},
			wantErr: "unsafe or missing jobFilters",
		},
		{
			name: "extra filter",
			mutate: func(response *walmartSearchResponse) {
				response.JobFilters = walmartString("brand == 'Walmart' AND primaryLocationCountry == 'IN' AND reqType == 'External'")
			},
			wantErr: "unsafe or missing jobFilters",
		},
		{
			name: "missing total",
			mutate: func(response *walmartSearchResponse) {
				response.TotalJobs = nil
			},
			wantErr: "omitted totalJobs or jobs",
		},
		{
			name: "missing jobs",
			mutate: func(response *walmartSearchResponse) {
				response.Jobs = nil
			},
			wantErr: "omitted totalJobs or jobs",
		},
		{
			name: "negative total",
			mutate: func(response *walmartSearchResponse) {
				response.TotalJobs = walmartInt(-1)
			},
			wantErr: "negative totalJobs",
		},
		{
			name: "total exceeds hard cap",
			mutate: func(response *walmartSearchResponse) {
				response.TotalJobs = walmartInt(walmartMaxJobs + 1)
			},
			wantErr: "exceeds hard limit",
		},
		{
			name: "truncated response",
			mutate: func(response *walmartSearchResponse) {
				response.TotalJobs = walmartInt(2)
			},
			wantErr: "incomplete response",
		},
		{
			name: "metadata missing",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata = nil
			},
			wantErr: "omitted metadata",
		},
		{
			name: "invalid job id",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.JobID = "../R-1001"
			},
			wantErr: "invalid jobId",
		},
		{
			name: "outer id mismatch",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].ID = "R-9999-External"
			},
			wantErr: "does not match jobId",
		},
		{
			name: "unknown outer id suffix",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].ID = "R-1001-Public"
			},
			wantErr: "External/Internal suffix",
		},
		{
			name: "request type mismatch",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.ReqType = "Internal"
			},
			wantErr: "does not match External suffix",
		},
		{
			name: "missing search title",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.Title = ""
			},
			wantErr: "omitted title or jobPostingTitle",
		},
		{
			name: "missing posting title",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.JobPostingTitle = ""
			},
			wantErr: "omitted title or jobPostingTitle",
		},
		{
			name: "missing description",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Text = " "
			},
			wantErr: "empty description",
		},
		{
			name: "wrong brand",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.Brand = "Sam's Club"
			},
			wantErr: "unexpected brand",
		},
		{
			name: "wrong country",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.PrimaryLocationCountry = "US"
			},
			wantErr: "want IN",
		},
		{
			name: "missing city",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.PrimaryLocationCity = ""
			},
			wantErr: "omitted primary location",
		},
		{
			name: "missing state",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.PrimaryLocationState = ""
			},
			wantErr: "omitted primary location",
		},
		{
			name: "missing coordinates",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.Location = nil
			},
			wantErr: "omitted location coordinates",
		},
		{
			name: "invalid latitude",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.Location.Lat = walmartFloat(91)
			},
			wantErr: "invalid location coordinates",
		},
		{
			name: "invalid longitude",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.Location.Lon = walmartFloat(181)
			},
			wantErr: "invalid location coordinates",
		},
		{
			name: "missing worker type",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.PositionWorkerType = ""
			},
			wantErr: "omitted positionWorkerType",
		},
		{
			name: "empty time type",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.TimeType = walmartString(" ")
			},
			wantErr: "empty timeType",
		},
		{
			name: "missing start date",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.JobPostingStartDate = nil
			},
			wantErr: "invalid jobPostingStartDate",
		},
		{
			name: "start date in seconds",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.JobPostingStartDate = walmartInt64(1782691200)
			},
			wantErr: "invalid jobPostingStartDate",
		},
		{
			name: "empty population",
			mutate: func(response *walmartSearchResponse) {
				(*response.Jobs)[0].Metadata.Population = walmartString(" ")
			},
			wantErr: "empty population",
		},
		{
			name: "conflicting duplicate external",
			mutate: func(response *walmartSearchResponse) {
				duplicate := walmartFixtureResult("R-1001", "External")
				duplicate.Metadata.PositionWorkerType = "Intern (Fixed Term)"
				results := append(*response.Jobs, duplicate)
				response.Jobs = &results
				response.TotalJobs = walmartInt(len(results))
			},
			wantErr: "conflicting external records",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := walmartFixtureResponse(walmartFixtureResult("R-1001", "External"))
			test.mutate(&response)
			jobs, err := walmartFetchFixture(t, response)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch = (%+v, %v), want error containing %q", jobs, err, test.wantErr)
			}
			if jobs != nil {
				t.Fatalf("jobs = %+v, want nil on error", jobs)
			}
		})
	}
}

func TestWalmartRequiresJSONAndOneValidDocument(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name: "http failure", status: http.StatusBadGateway,
			contentType: "text/plain", body: "upstream unavailable", wantErr: "502 Bad Gateway",
		},
		{
			name: "wrong content type", status: http.StatusOK,
			contentType: "text/html", body: `{}`, wantErr: "unexpected Content-Type",
		},
		{
			name: "malformed json", status: http.StatusOK,
			contentType: "application/json", body: `{`, wantErr: "decoding response",
		},
		{
			name: "trailing json", status: http.StatusOK,
			contentType: "application/json", body: `{}` + "\n" + `{}`, wantErr: "trailing JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			src := &walmart{company: "Walmart", endpoint: server.URL, client: server.Client()}
			jobs, err := src.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch = (%+v, %v), want error containing %q", jobs, err, test.wantErr)
			}
		})
	}
}

func TestWalmartFilterMatchesOnlyExactScope(t *testing.T) {
	valid := []string{
		"brand == 'Walmart' AND primaryLocationCountry == 'IN'",
		" primaryLocationCountry == 'IN' AND brand == 'Walmart' ",
		"brand=='Walmart'   AND   primaryLocationCountry=='IN'",
	}
	for _, filter := range valid {
		if !walmartFilterMatches(filter) {
			t.Errorf("walmartFilterMatches(%q) = false", filter)
		}
	}
	invalid := []string{
		"",
		"brand == 'Walmart'",
		"brand == 'Walmart' OR primaryLocationCountry == 'IN'",
		"brand == 'Walmart' AND primaryLocationCountry == 'US'",
		"brand == 'Walmart' AND primaryLocationCountry == 'IN' AND reqType == 'External'",
		"brand == 'Walmart' AND brand == 'Walmart'",
		"Brand == 'Walmart' AND primaryLocationCountry == 'IN'",
		`brand == "Walmart" AND primaryLocationCountry == "IN"`,
	}
	for _, filter := range invalid {
		if walmartFilterMatches(filter) {
			t.Errorf("walmartFilterMatches(%q) = true", filter)
		}
	}
}

func walmartFetchFixture(t *testing.T, response walmartSearchResponse) ([]model.Job, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()
	src := &walmart{company: "Walmart", endpoint: server.URL, client: server.Client()}
	return src.Fetch(context.Background())
}

func walmartFixtureResponse(results ...walmartSearchResult) walmartSearchResponse {
	filter := "brand == 'Walmart' AND primaryLocationCountry == 'IN'"
	jobs := make([]walmartSearchResult, len(results))
	copy(jobs, results)
	return walmartSearchResponse{
		Query:              walmartString(walmartQuery),
		JobFilters:         &filter,
		JobSearchSucceeded: walmartBool(true),
		TotalJobs:          walmartInt(len(jobs)),
		Jobs:               &jobs,
	}
}

func walmartFixtureResult(jobID, kind string) walmartSearchResult {
	latitude, longitude := 12.936511, 77.693299
	timeType, population := "Full time", "WALMART_EXT_IN"
	return walmartSearchResult{
		ID:   jobID + "-" + kind,
		Text: "  Build reliable distributed systems.  ",
		Metadata: &walmartMetadata{
			JobID: jobID, Title: "Senior Software Engineer",
			JobPostingTitle: "Senior Software Engineer",
			Location: &walmartLocation{
				Lat: &latitude, Lon: &longitude,
			},
			PrimaryLocationCity: "BENGALURU", PrimaryLocationState: "KA",
			PrimaryLocationCountry: "IN", TimeType: &timeType,
			PositionWorkerType:  "Regular/Permanent",
			JobPostingStartDate: walmartInt64(1782691200000),
			Population:          &population,
			Brand:               "Walmart",
			ReqType:             kind,
		},
	}
}

func walmartString(value string) *string  { return &value }
func walmartBool(value bool) *bool        { return &value }
func walmartInt(value int) *int           { return &value }
func walmartInt64(value int64) *int64     { return &value }
func walmartFloat(value float64) *float64 { return &value }
