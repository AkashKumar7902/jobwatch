package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestMakeMyTripRegistrationListDetailAndNormalization(t *testing.T) {
	const firstID = "a6a69f6956f482"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent is empty")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/jobs":
			if r.URL.RawQuery != "" {
				t.Errorf("list query = %q, want empty", r.URL.RawQuery)
			}
			if err := json.NewEncoder(w).Encode(makeMyTripFixtureList()); err != nil {
				t.Fatal(err)
			}
		case "/api/jobDetails":
			query := r.URL.Query()
			if len(query) != 1 || query.Get("jobId") != firstID {
				t.Errorf("detail query = %v", query)
			}
			if err := json.NewEncoder(w).Encode(makeMyTripFixtureDetail(firstID)); err != nil {
				t.Fatal(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src, err := New("makemytrip", "MakeMyTrip", params.Map{}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("New returned %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*makeMyTrip)
	if !ok {
		t.Fatalf("wrapped source = %T, want *makeMyTrip", wrapped.Source)
	}
	implementation.apiBase = server.URL
	implementation.siteBase = server.URL + "/prod"

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Job{
		{
			ID:             "makemytrip/" + firstID,
			Company:        "MakeMyTrip",
			Title:          "Inside Sales Lead",
			Location:       "Bangalore, Karnataka, India (Bangalore_MMT)",
			URL:            server.URL + "/prod/opportunity/" + firstID + "/inside-sales-lead",
			EmploymentType: "Employee",
			PostedAt:       time.Date(2026, 7, 29, 12, 48, 21, 0, time.UTC),
		},
		{
			ID:             "makemytrip/a65d2ee365f4ff",
			Company:        "MakeMyTrip",
			Title:          "Project Based Internship (Technology)",
			Location:       "Gurgaon, Haryana, India (Gurgaon_MMT)",
			URL:            server.URL + "/prod/opportunity/a65d2ee365f4ff/project-based-internship-%28technology%29",
			EmploymentType: "Intern",
			PostedAt:       time.Date(2026, 7, 28, 4, 30, 0, 0, time.UTC),
		},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs =\n%+v\nwant\n%+v", jobs, want)
	}
	if _, ok := any(implementation).(Detailer); !ok {
		t.Fatal("*makeMyTrip does not implement Detailer")
	}
	if err := wrapped.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if jobs[0].Description != "Build & ship.\nUse Go." {
		t.Fatalf("description = %q", jobs[0].Description)
	}
	if !reflect.DeepEqual(paths, []string{"/api/jobs", "/api/jobDetails?jobId=" + firstID}) {
		t.Fatalf("paths = %v", paths)
	}
}

func TestMakeMyTripAllowsAnEmptyCompleteBoard(t *testing.T) {
	response := makeMyTripListResponse{
		AllJobs:       makeMyTripPostings(),
		BusinessUnits: makeMyTripBusinessUnits(),
		Locations:     makeMyTripStrings(),
	}
	jobs, err := makeMyTripFetchFixture(t, response)
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
	}
}

func TestMakeMyTripRejectsParams(t *testing.T) {
	_, err := New("makemytrip", "MakeMyTrip", params.Map{"z": "1", "a": "2"}, nil)
	if err == nil || !strings.Contains(err.Error(), "got a, z") {
		t.Fatalf("error = %v", err)
	}
}

func TestMakeMyTripRejectsIncompleteOrDriftedList(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*makeMyTripListResponse)
		wantErr string
	}{
		{name: "missing allJobs", mutate: func(response *makeMyTripListResponse) { response.AllJobs = nil }, wantErr: "list omitted allJobs"},
		{name: "missing businessUnits", mutate: func(response *makeMyTripListResponse) { response.BusinessUnits = nil }, wantErr: "list omitted allJobs"},
		{name: "missing locations", mutate: func(response *makeMyTripListResponse) { response.Locations = nil }, wantErr: "list omitted allJobs"},
		{
			name: "hard limit",
			mutate: func(response *makeMyTripListResponse) {
				postings := make([]makeMyTripPosting, makeMyTripMaxJobs+1)
				response.AllJobs = &postings
			},
			wantErr: "exceeding hard limit",
		},
		{
			name: "duplicate id",
			mutate: func(response *makeMyTripListResponse) {
				*response.AllJobs = append(*response.AllJobs, (*response.AllJobs)[0])
			},
			wantErr: `duplicate job_id "a6a69f6956f482"`,
		},
		{
			name: "business unit count mismatch",
			mutate: func(response *makeMyTripListResponse) {
				(*response.BusinessUnits)[0].Count = makeMyTripInt(2)
			},
			wantErr: `businessUnits count for "Supply" is 2, jobs contain 1`,
		},
		{
			name: "missing business unit group",
			mutate: func(response *makeMyTripListResponse) {
				*response.BusinessUnits = (*response.BusinessUnits)[:1]
			},
			wantErr: "businessUnits has 1 groups, jobs contain 2",
		},
		{
			name: "invalid business unit",
			mutate: func(response *makeMyTripListResponse) {
				(*response.BusinessUnits)[0].Count = nil
			},
			wantErr: "invalid name or count",
		},
		{
			name: "duplicate business unit",
			mutate: func(response *makeMyTripListResponse) {
				*response.BusinessUnits = append(*response.BusinessUnits, (*response.BusinessUnits)[0])
			},
			wantErr: "duplicate businessUnits",
		},
		{
			name: "location set mismatch",
			mutate: func(response *makeMyTripListResponse) {
				(*response.Locations)[0] = "Mumbai"
			},
			wantErr: `locations omitted job city "Bangalore"`,
		},
		{
			name: "missing location facet",
			mutate: func(response *makeMyTripListResponse) {
				*response.Locations = (*response.Locations)[:1]
			},
			wantErr: "locations has 1 values, jobs contain 2 cities",
		},
		{
			name: "duplicate location facet",
			mutate: func(response *makeMyTripListResponse) {
				*response.Locations = append(*response.Locations, (*response.Locations)[0])
			},
			wantErr: "duplicate locations",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := makeMyTripFixtureList()
			test.mutate(&response)
			_, err := makeMyTripFetchFixture(t, response)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestMakeMyTripRejectsInvalidListPosting(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*makeMyTripPosting)
		wantErr string
	}{
		{name: "invalid id", mutate: func(posting *makeMyTripPosting) { posting.JobID = "../a6a69f6956f482" }, wantErr: "invalid job_id"},
		{name: "noncanonical id", mutate: func(posting *makeMyTripPosting) { posting.JobID = " a6a69f6956f482 " }, wantErr: "invalid job_id"},
		{name: "empty code", mutate: func(posting *makeMyTripPosting) { posting.JobCode = " " }, wantErr: "empty job_code"},
		{name: "empty title", mutate: func(posting *makeMyTripPosting) { posting.JobTitle = " " }, wantErr: "empty job_title"},
		{name: "missing public flag", mutate: func(posting *makeMyTripPosting) { posting.PostOnCareersPage = nil }, wantErr: "post_on_careers_page <missing>"},
		{name: "not public", mutate: func(posting *makeMyTripPosting) { posting.PostOnCareersPage = makeMyTripInt(0) }, wantErr: "post_on_careers_page 0"},
		{name: "wrong country", mutate: func(posting *makeMyTripPosting) { posting.LocationCountry = "US" }, wantErr: `location_country "US"`},
		{name: "empty company", mutate: func(posting *makeMyTripPosting) { posting.GroupCompany = " " }, wantErr: "empty group_company"},
		{name: "empty unit", mutate: func(posting *makeMyTripPosting) { posting.BusinessUnit = " " }, wantErr: "empty business_unit"},
		{name: "empty type", mutate: func(posting *makeMyTripPosting) { posting.EmployeeType = " " }, wantErr: "empty employee_type"},
		{name: "missing location", mutate: func(posting *makeMyTripPosting) { posting.Location = nil }, wantErr: "omitted or empty location"},
		{name: "empty location item", mutate: func(posting *makeMyTripPosting) { posting.Location = makeMyTripStrings("") }, wantErr: "location item 0 is empty"},
		{name: "duplicate location", mutate: func(posting *makeMyTripPosting) { posting.Location = makeMyTripStrings("Bangalore", "Bangalore") }, wantErr: `location contains duplicate "Bangalore"`},
		{name: "missing city", mutate: func(posting *makeMyTripPosting) { posting.LocationCity = nil }, wantErr: "omitted or empty location_city"},
		{name: "duplicate city", mutate: func(posting *makeMyTripPosting) { posting.LocationCity = makeMyTripStrings("Bangalore", "Bangalore") }, wantErr: `location_city contains duplicate "Bangalore"`},
		{name: "invalid created", mutate: func(posting *makeMyTripPosting) { posting.JobCreatedTimestamp = "2026-07-29" }, wantErr: "invalid job_created_timestamp"},
		{name: "invalid updated", mutate: func(posting *makeMyTripPosting) { posting.JobUpdatedTimestamp = "2026-07-30" }, wantErr: "invalid job_updated_timestamp"},
		{name: "updated before created", mutate: func(posting *makeMyTripPosting) { posting.JobUpdatedTimestamp = "28-07-2026 18:18:20" }, wantErr: "updated before it was created"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := makeMyTripFixtureList()
			test.mutate(&(*response.AllJobs)[0])
			_, err := makeMyTripFetchFixture(t, response)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestMakeMyTripDetailRejectsDriftAndDoesNotMutateJob(t *testing.T) {
	const id = "a6a69f6956f482"
	tests := []struct {
		name    string
		mutate  func(*makeMyTripDetailResponse)
		wantErr string
	}{
		{name: "error response", mutate: func(response *makeMyTripDetailResponse) { response.Error = "not found" }, wantErr: `returned error "not found"`},
		{name: "missing status", mutate: func(response *makeMyTripDetailResponse) { response.Status = nil }, wantErr: "omitted successful status"},
		{name: "failed status", mutate: func(response *makeMyTripDetailResponse) { response.Status = makeMyTripInt(0) }, wantErr: "omitted successful status"},
		{name: "missing data", mutate: func(response *makeMyTripDetailResponse) { response.Data = nil }, wantErr: "omitted successful status"},
		{name: "closed", mutate: func(response *makeMyTripDetailResponse) { response.Data.JobStatus = "CLOSED" }, wantErr: `job_status "CLOSED"`},
		{name: "title drift", mutate: func(response *makeMyTripDetailResponse) { response.Data.JobTitle = "Other" }, wantErr: "does not match list title"},
		{name: "wrong country", mutate: func(response *makeMyTripDetailResponse) { response.Data.LocationCountry = "US" }, wantErr: `location_country "US"`},
		{name: "empty company", mutate: func(response *makeMyTripDetailResponse) { response.Data.GroupCompany = " " }, wantErr: "empty group_company"},
		{name: "location drift", mutate: func(response *makeMyTripDetailResponse) { response.Data.Location = makeMyTripStrings("Gurgaon") }, wantErr: "does not match list location"},
		{name: "missing city", mutate: func(response *makeMyTripDetailResponse) { response.Data.LocationCity = nil }, wantErr: "omitted or empty location_city"},
		{name: "type drift", mutate: func(response *makeMyTripDetailResponse) { response.Data.EmployeeType = "Intern" }, wantErr: "does not match list value"},
		{name: "invalid created", mutate: func(response *makeMyTripDetailResponse) { response.Data.JobCreatedTimestamp = "2026-07-29" }, wantErr: "invalid job_created_timestamp"},
		{name: "invalid updated", mutate: func(response *makeMyTripDetailResponse) { response.Data.JobUpdatedTimestamp = "2026-07-30" }, wantErr: "invalid job_updated_timestamp"},
		{name: "updated before created", mutate: func(response *makeMyTripDetailResponse) { response.Data.JobUpdatedTimestamp = "28-07-2026 10:00:00" }, wantErr: "updated before it was created"},
		{name: "created drift", mutate: func(response *makeMyTripDetailResponse) { response.Data.JobCreatedTimestamp = "28-07-2026 18:18:21" }, wantErr: "created timestamp does not match list"},
		{name: "apply URL mismatch", mutate: func(response *makeMyTripDetailResponse) { response.Data.ApplyURL = "https://gommt.darwinbox.in/other" }, wantErr: "invalid applyUrl"},
		{name: "empty description", mutate: func(response *makeMyTripDetailResponse) { response.Data.JobDescription = "&lt;p&gt; &lt;/p&gt;" }, wantErr: "empty job_decription"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := makeMyTripFixtureDetail(id)
			test.mutate(&response)
			src, closeServer := makeMyTripDetailFixtureSource(t, response)
			defer closeServer()
			job := makeMyTripFixtureJob(id)
			before := job
			err := src.Detail(context.Background(), &job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
			if !reflect.DeepEqual(job, before) {
				t.Fatalf("failed Detail mutated job:\n%+v\nbefore:\n%+v", job, before)
			}
		})
	}
}

func TestMakeMyTripDetailRejectsInvalidJobIdentity(t *testing.T) {
	src := &makeMyTrip{company: "MakeMyTrip", apiBase: "https://unused.invalid", client: http.DefaultClient}
	if err := src.Detail(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "nil job") {
		t.Fatalf("nil job error = %v", err)
	}
	for _, id := range []string{"other/a6a69f6956f482", "makemytrip/../bad", "makemytrip/"} {
		job := model.Job{ID: id}
		if err := src.Detail(context.Background(), &job); err == nil {
			t.Fatalf("Detail accepted job ID %q", id)
		}
	}
}

func makeMyTripFetchFixture(t *testing.T, response makeMyTripListResponse) ([]model.Job, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()
	src := &makeMyTrip{
		company:  "MakeMyTrip",
		apiBase:  server.URL,
		siteBase: server.URL + "/prod",
		client:   server.Client(),
	}
	return src.Fetch(context.Background())
}

func makeMyTripDetailFixtureSource(t *testing.T, response makeMyTripDetailResponse) (*makeMyTrip, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	return &makeMyTrip{
		company:  "MakeMyTrip",
		apiBase:  server.URL,
		siteBase: server.URL + "/prod",
		client:   server.Client(),
	}, server.Close
}

func makeMyTripFixtureList() makeMyTripListResponse {
	postings := []makeMyTripPosting{
		{
			JobID:               "a6a69f6956f482",
			JobCode:             "JOB_2114",
			GroupCompany:        "MakeMyTrip (India) Limited",
			BusinessUnit:        "Supply",
			JobTitle:            "Inside Sales Lead",
			Location:            makeMyTripStrings("Bangalore, Karnataka, India (Bangalore_MMT)"),
			LocationCity:        makeMyTripStrings("Bangalore"),
			LocationCountry:     makeMyTripCountry,
			PostOnCareersPage:   makeMyTripInt(1),
			EmployeeType:        "Employee",
			JobCreatedTimestamp: "29-07-2026 18:18:21",
			JobUpdatedTimestamp: "30-07-2026 15:39:17",
		},
		{
			JobID:               "a65d2ee365f4ff",
			JobCode:             "JOB_2113",
			GroupCompany:        "MakeMyTrip (India) Limited",
			BusinessUnit:        "Internship",
			JobTitle:            "Project Based Internship (Technology)",
			Location:            makeMyTripStrings("Gurgaon, Haryana, India (Gurgaon_MMT)"),
			LocationCity:        makeMyTripStrings("Gurgaon"),
			LocationCountry:     makeMyTripCountry,
			PostOnCareersPage:   makeMyTripInt(1),
			EmployeeType:        "Intern",
			JobCreatedTimestamp: "28-07-2026 10:00:00",
			JobUpdatedTimestamp: "28-07-2026 12:00:00",
		},
	}
	units := []makeMyTripBusinessUnit{
		{Name: "Supply", Count: makeMyTripInt(1)},
		{Name: "Internship", Count: makeMyTripInt(1)},
	}
	locations := []string{"Bangalore", "Gurgaon"}
	return makeMyTripListResponse{
		AllJobs:       &postings,
		BusinessUnits: &units,
		Locations:     &locations,
	}
}

func makeMyTripFixtureDetail(id string) makeMyTripDetailResponse {
	return makeMyTripDetailResponse{
		Status: makeMyTripInt(1),
		Data: &makeMyTripDetail{
			ApplyURL:            "https://gommt.darwinbox.in/ms/candidatev2/main/careers/jobDetails/" + id + "?from=all",
			JobTitle:            "Inside Sales Lead",
			GroupCompany:        "MakeMyTrip (India) Limited",
			EmployeeType:        "Employee",
			Location:            makeMyTripStrings("Bangalore, Karnataka, India (Bangalore_MMT)"),
			LocationCity:        makeMyTripStrings("Bangalore"),
			LocationCountry:     makeMyTripCountry,
			JobCreatedTimestamp: "29-07-2026 18:18:21",
			JobUpdatedTimestamp: "30-07-2026 15:39:17",
			JobDescription:      "&lt;p&gt;Build &amp;amp; ship.&lt;/p&gt;&lt;ul&gt;&lt;li&gt;Use Go.&lt;/li&gt;&lt;/ul&gt;",
			JobStatus:           "OPEN",
		},
	}
}

func makeMyTripFixtureJob(id string) model.Job {
	return model.Job{
		ID:             "makemytrip/" + id,
		Company:        "MakeMyTrip",
		Title:          "Inside Sales Lead",
		Location:       "Bangalore, Karnataka, India (Bangalore_MMT)",
		URL:            "https://careers.makemytrip.com/prod/opportunity/" + id + "/inside-sales-lead",
		EmploymentType: "Employee",
		PostedAt:       time.Date(2026, 7, 29, 12, 48, 21, 0, time.UTC),
	}
}

func makeMyTripPostings(values ...makeMyTripPosting) *[]makeMyTripPosting {
	if values == nil {
		values = make([]makeMyTripPosting, 0)
	}
	return &values
}
func makeMyTripBusinessUnits(values ...makeMyTripBusinessUnit) *[]makeMyTripBusinessUnit {
	if values == nil {
		values = make([]makeMyTripBusinessUnit, 0)
	}
	return &values
}
func makeMyTripStrings(values ...string) *[]string {
	if values == nil {
		values = make([]string, 0)
	}
	return &values
}
func makeMyTripInt(value int) *int { return &value }
