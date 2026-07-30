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

const (
	testAuzmorDomain       = "letstransport"
	testAuzmorJobID        = "ef6b57aa3e4f4e0ea10b778b28c6e134"
	testAuzmorRemoteJobID  = "1b5084aead0b4526acda72cc2ad83eda"
	testAuzmorDepartmentID = "7c5a3843893244ce909f4ef10195f66b"
)

func TestAuzmorRegistrationValidationAndNormalization(t *testing.T) {
	t.Parallel()

	src, err := New("auzmor", "LetsTransport", params.Map{"domain": " LetsTransport "}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("source type = %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*auzmor)
	if !ok {
		t.Fatalf("wrapped source type = %T, want *auzmor", wrapped.Source)
	}
	if implementation.domain != testAuzmorDomain {
		t.Errorf("domain = %q, want %q", implementation.domain, testAuzmorDomain)
	}
	if implementation.client != http.DefaultClient {
		t.Error("nil client did not select http.DefaultClient")
	}

	tests := []struct {
		name    string
		params  params.Map
		wantErr string
	}{
		{name: "missing domain", params: params.Map{}, wantErr: `missing required param "domain"`},
		{name: "path separator", params: params.Map{"domain": "bad/domain"}, wantErr: "invalid Auzmor domain"},
		{name: "dot", params: params.Map{"domain": "bad.domain"}, wantErr: "invalid Auzmor domain"},
		{name: "leading hyphen", params: params.Map{"domain": "-bad"}, wantErr: "invalid Auzmor domain"},
		{name: "trailing hyphen", params: params.Map{"domain": "bad-"}, wantErr: "invalid Auzmor domain"},
		{
			name: "unknown params",
			params: params.Map{
				"domain": testAuzmorDomain,
				"z":      "1",
				"a":      "2",
			},
			wantErr: "got a, z",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := New("auzmor", "Example", test.params, nil)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestAuzmorFetchAndDetailNormalizeLiveShape(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q, want application/json", r.Header.Get("Accept"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "jobwatch") {
			t.Errorf("User-Agent = %q, want jobwatch", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Path {
		case "/careers/" + testAuzmorDomain + "/groupByDepartment":
			if got := r.URL.Query().Get("limit"); got != "10000" {
				t.Errorf("limit = %q, want 10000", got)
			}
			if got := r.URL.Query().Get("offset"); got != "0" {
				t.Errorf("offset = %q, want 0", got)
			}
			if len(r.URL.Query()) != 2 {
				t.Errorf("query = %v, want only limit and offset", r.URL.Query())
			}
			if err := json.NewEncoder(w).Encode(auzmorListFixture()); err != nil {
				t.Fatal(err)
			}
		case "/getJob/" + testAuzmorDomain + "/" + testAuzmorJobID:
			if r.URL.RawQuery != "" {
				t.Errorf("detail query = %q, want empty", r.URL.RawQuery)
			}
			if err := json.NewEncoder(w).Encode(auzmorDetailFixture()); err != nil {
				t.Fatal(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src, err := New(
		"auzmor",
		"LetsTransport",
		params.Map{"domain": testAuzmorDomain},
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := src.(*identifiedSource)
	implementation := wrapped.Source.(*auzmor)
	implementation.apiBase = server.URL
	implementation.siteBase = server.URL + "/portal"

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Job{
		{
			ID:             "auzmor/letstransport/" + testAuzmorJobID,
			Company:        "LetsTransport",
			Title:          "Product Manager",
			Location:       "Bengaluru, Karnataka, India",
			URL:            server.URL + "/portal/letstransport/careers/" + testAuzmorJobID,
			EmploymentType: "Full Time",
			PostedAt:       time.Date(2021, 6, 3, 13, 49, 54, 0, time.UTC),
		},
		{
			ID:             "auzmor/letstransport/" + testAuzmorRemoteJobID,
			Company:        "LetsTransport",
			Title:          "Senior Product Manager",
			Location:       "Remote",
			URL:            server.URL + "/portal/letstransport/careers/" + testAuzmorRemoteJobID,
			EmploymentType: "Full Time; Permanent",
			PostedAt:       time.Date(2021, 7, 15, 2, 32, 30, 0, time.UTC),
		},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs =\n%+v\nwant:\n%+v", jobs, want)
	}
	if _, ok := any(implementation).(Detailer); !ok {
		t.Fatal("*auzmor does not implement Detailer")
	}
	if err := wrapped.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if jobs[0].Description != "Build products.\nShip them." {
		t.Fatalf("description = %q", jobs[0].Description)
	}
	if !reflect.DeepEqual(requests, []string{
		"/careers/letstransport/groupByDepartment?limit=10000&offset=0",
		"/getJob/letstransport/" + testAuzmorJobID,
	}) {
		t.Fatalf("requests = %v", requests)
	}
}

func TestAuzmorAllowsCompleteEmptyBoard(t *testing.T) {
	response := auzmorListResponse{
		Data: auzmorGroups(),
		Pagination: &auzmorPagination{
			TotalItems:  auzmorInt(0),
			FilterItems: auzmorInt(0),
			Limit:       auzmorInt(auzmorJobLimit),
			Offset:      auzmorInt(0),
		},
	}
	jobs, err := fetchAuzmorFixture(t, response)
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
	}
}

func TestAuzmorRejectsIncompleteOrDriftedList(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*auzmorListResponse)
		wantErr string
	}{
		{name: "missing data", mutate: func(r *auzmorListResponse) { r.Data = nil }, wantErr: "omitted data or pagination"},
		{name: "missing pagination", mutate: func(r *auzmorListResponse) { r.Pagination = nil }, wantErr: "omitted data or pagination"},
		{name: "missing total", mutate: func(r *auzmorListResponse) { r.Pagination.TotalItems = nil }, wantErr: "pagination omitted"},
		{name: "negative total", mutate: func(r *auzmorListResponse) { r.Pagination.TotalItems = auzmorInt(-1) }, wantErr: "negative totals"},
		{name: "too many groups", mutate: func(r *auzmorListResponse) { r.Pagination.TotalItems = auzmorInt(auzmorGroupLimit + 1) }, wantErr: "department safety limit"},
		{name: "total mismatch", mutate: func(r *auzmorListResponse) { r.Pagination.TotalItems = auzmorInt(1) }, wantErr: "incomplete department list"},
		{name: "filter mismatch", mutate: func(r *auzmorListResponse) { r.Pagination.FilterItems = auzmorInt(1) }, wantErr: "incomplete department list"},
		{name: "limit drift", mutate: func(r *auzmorListResponse) { r.Pagination.Limit = auzmorInt(100) }, wantErr: "changed requested limit/offset"},
		{name: "offset drift", mutate: func(r *auzmorListResponse) { r.Pagination.Offset = auzmorInt(1) }, wantErr: "changed requested limit/offset"},
		{name: "missing department", mutate: func(r *auzmorListResponse) { (*r.Data)[0].Department = nil }, wantErr: "omitted department, count, or jobs"},
		{name: "missing count", mutate: func(r *auzmorListResponse) { (*r.Data)[0].Count = nil }, wantErr: "omitted department, count, or jobs"},
		{name: "missing jobs", mutate: func(r *auzmorListResponse) { (*r.Data)[0].Jobs = nil }, wantErr: "omitted department, count, or jobs"},
		{name: "missing numeric department id", mutate: func(r *auzmorListResponse) { (*r.Data)[0].Department.ID = nil }, wantErr: "positive id"},
		{name: "invalid department uuid", mutate: func(r *auzmorListResponse) { (*r.Data)[0].Department.UUID = "bad" }, wantErr: "invalid uuid"},
		{name: "empty department name", mutate: func(r *auzmorListResponse) { (*r.Data)[0].Department.Name = " " }, wantErr: "empty name"},
		{
			name: "duplicate department",
			mutate: func(r *auzmorListResponse) {
				second := (*r.Data)[1].Department
				second.UUID = (*r.Data)[0].Department.UUID
			},
			wantErr: "duplicate department UUID",
		},
		{
			name: "duplicate numeric department id",
			mutate: func(r *auzmorListResponse) {
				second := (*r.Data)[1].Department
				second.ID = auzmorInt64(*(*r.Data)[0].Department.ID)
			},
			wantErr: "duplicate department id",
		},
		{name: "negative job count", mutate: func(r *auzmorListResponse) { (*r.Data)[0].Count = auzmorInt(-1) }, wantErr: "negative count"},
		{name: "truncated group", mutate: func(r *auzmorListResponse) { (*r.Data)[0].Count = auzmorInt(2) }, wantErr: "incomplete department"},
		{
			name: "duplicate job",
			mutate: func(r *auzmorListResponse) {
				second := &(*(*r.Data)[1].Jobs)[0]
				second.UUID = (*(*r.Data)[0].Jobs)[0].UUID
			},
			wantErr: "duplicate job UUID",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := auzmorListFixture()
			test.mutate(&response)
			_, err := fetchAuzmorFixture(t, response)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestAuzmorRejectsInvalidListPosting(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*auzmorPosting)
		wantErr string
	}{
		{name: "invalid uuid", mutate: func(p *auzmorPosting) { p.UUID = "BAD" }, wantErr: "invalid uuid"},
		{name: "empty title", mutate: func(p *auzmorPosting) { p.Title = " " }, wantErr: "empty title"},
		{name: "wrong state", mutate: func(p *auzmorPosting) { p.State = "DRAFT" }, wantErr: `state "DRAFT"`},
		{name: "wrong status", mutate: func(p *auzmorPosting) { p.Status = "CLOSED" }, wantErr: `status "CLOSED"`},
		{name: "wrong publish type", mutate: func(p *auzmorPosting) { p.PublishType = "Internal" }, wantErr: `publishType "Internal"`},
		{name: "missing remote flag", mutate: func(p *auzmorPosting) { p.IsRemote = nil }, wantErr: "omitted isRemote"},
		{name: "missing department", mutate: func(p *auzmorPosting) { p.Department = nil }, wantErr: "missing department"},
		{name: "department drift", mutate: func(p *auzmorPosting) { p.Department.Name = "Other" }, wantErr: "does not match group"},
		{name: "missing location", mutate: func(p *auzmorPosting) { p.Location = nil }, wantErr: "missing location for non-remote"},
		{name: "invalid location", mutate: func(p *auzmorPosting) { p.Location.UUID = "bad" }, wantErr: "location has invalid uuid"},
		{name: "empty employment types", mutate: func(p *auzmorPosting) { p.EmploymentTypes = auzmorStrings() }, wantErr: "omitted or empty employmentTypes"},
		{name: "empty employment type", mutate: func(p *auzmorPosting) { p.EmploymentTypes = auzmorStrings("") }, wantErr: "item 0 is empty"},
		{name: "duplicate employment type", mutate: func(p *auzmorPosting) { p.EmploymentTypes = auzmorStrings("Full Time", "Full Time") }, wantErr: "contains duplicate"},
		{name: "invalid published date", mutate: func(p *auzmorPosting) { p.PublishedDate = "2021-06-03" }, wantErr: "invalid publishedDate"},
		{name: "invalid created date", mutate: func(p *auzmorPosting) { p.CreatedAt = "2021-06-03" }, wantErr: "invalid createdAt"},
		{name: "published before created", mutate: func(p *auzmorPosting) { p.PublishedDate = "2021-06-03T12:00:00Z" }, wantErr: "published before it was created"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := auzmorListFixture()
			test.mutate(&(*(*response.Data)[0].Jobs)[0])
			_, err := fetchAuzmorFixture(t, response)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestAuzmorDetailRejectsDriftWithoutMutation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*auzmorDetailResponse)
		wantErr string
	}{
		{name: "missing job", mutate: func(r *auzmorDetailResponse) { r.Job = nil }, wantErr: "omitted job or department"},
		{name: "missing outer department", mutate: func(r *auzmorDetailResponse) { r.Department = nil }, wantErr: "omitted job or department"},
		{name: "id drift", mutate: func(r *auzmorDetailResponse) { r.Job.UUID = testAuzmorRemoteJobID }, wantErr: "returned uuid"},
		{name: "department disagreement", mutate: func(r *auzmorDetailResponse) { r.Department.Name = "Other" }, wantErr: "department fields disagree"},
		{name: "closed", mutate: func(r *auzmorDetailResponse) { r.Job.Status = "CLOSED" }, wantErr: "not an open external publication"},
		{name: "missing remote flag", mutate: func(r *auzmorDetailResponse) { r.Job.IsRemote = nil }, wantErr: "omitted isRemote"},
		{name: "location disagreement", mutate: func(r *auzmorDetailResponse) { r.Location.Name = "Pune, India" }, wantErr: "location fields disagree"},
		{name: "title drift", mutate: func(r *auzmorDetailResponse) { r.Job.Title = "Other" }, wantErr: "does not match list fields"},
		{name: "type drift", mutate: func(r *auzmorDetailResponse) { r.Job.EmploymentTypes = auzmorStrings("Contract") }, wantErr: "does not match list fields"},
		{name: "date drift", mutate: func(r *auzmorDetailResponse) { r.Job.PublishedDate = "2021-06-04T13:49:54Z" }, wantErr: "does not match list fields"},
		{name: "empty description", mutate: func(r *auzmorDetailResponse) { r.Job.Description = "<p> </p>" }, wantErr: "empty description"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := auzmorDetailFixture()
			test.mutate(&response)
			src, closeServer := auzmorDetailFixtureSource(t, response)
			defer closeServer()
			job := auzmorFixtureJob()
			before := job
			err := src.Detail(context.Background(), &job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Detail error = %v, want substring %q", err, test.wantErr)
			}
			if !reflect.DeepEqual(job, before) {
				t.Fatalf("failed Detail mutated job:\n%+v\nbefore:\n%+v", job, before)
			}
		})
	}
}

func TestAuzmorDetailRejectsInvalidJobIdentity(t *testing.T) {
	t.Parallel()

	src := &auzmor{
		company: "LetsTransport",
		domain:  testAuzmorDomain,
		apiBase: "https://unused.invalid",
		client:  http.DefaultClient,
	}
	if err := src.Detail(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "nil job") {
		t.Fatalf("nil Detail error = %v", err)
	}
	for _, id := range []string{
		"other/letstransport/" + testAuzmorJobID,
		"auzmor/other/" + testAuzmorJobID,
		"auzmor/letstransport/../bad",
		"auzmor/letstransport/",
	} {
		job := model.Job{ID: id}
		if err := src.Detail(context.Background(), &job); err == nil {
			t.Fatalf("Detail accepted invalid job ID %q", id)
		}
	}
}

func TestAuzmorRejectsInvalidHTTPResponses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{name: "status", status: http.StatusBadGateway, contentType: "application/json", body: `{"error":"upstream"}`, wantErr: "502 Bad Gateway"},
		{name: "content type", status: http.StatusOK, contentType: "text/html", body: `{}`, wantErr: "unexpected Content-Type"},
		{name: "invalid json", status: http.StatusOK, contentType: "application/json", body: `{`, wantErr: "decoding response"},
		{name: "trailing json", status: http.StatusOK, contentType: "application/json", body: `{} {}`, wantErr: "trailing JSON"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			src := &auzmor{client: server.Client()}
			var response auzmorListResponse
			err := src.getJSON(context.Background(), server.URL, 1024, &response)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("getJSON error = %v, want substring %q", err, test.wantErr)
			}
		})
	}

	t.Run("body limit", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"padding":"long"}`))
		}))
		defer server.Close()
		src := &auzmor{client: server.Client()}
		var response any
		err := src.getJSON(context.Background(), server.URL, 4, &response)
		if err == nil || !strings.Contains(err.Error(), "response exceeds 4-byte safety limit") {
			t.Fatalf("getJSON error = %v", err)
		}
	})
}

func fetchAuzmorFixture(t *testing.T, response auzmorListResponse) ([]model.Job, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()
	src := &auzmor{
		company:  "LetsTransport",
		domain:   testAuzmorDomain,
		apiBase:  server.URL,
		siteBase: server.URL,
		client:   server.Client(),
	}
	return src.Fetch(context.Background())
}

func auzmorDetailFixtureSource(
	t *testing.T,
	response auzmorDetailResponse,
) (*auzmor, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	return &auzmor{
		company:  "LetsTransport",
		domain:   testAuzmorDomain,
		apiBase:  server.URL,
		siteBase: server.URL,
		client:   server.Client(),
	}, server.Close
}

func auzmorListFixture() auzmorListResponse {
	firstDepartment := auzmorDepartment{
		ID: auzmorInt64(29397), UUID: testAuzmorDepartmentID, Name: "Product",
	}
	secondDepartment := auzmorDepartment{
		ID: auzmorInt64(29398), UUID: "d925cdb3eb2246bfba48e44a46403cac", Name: "Engineering",
	}
	firstJob := auzmorPostingFixture()
	secondJob := auzmorPosting{
		UUID:            testAuzmorRemoteJobID,
		Title:           " Senior Product Manager ",
		State:           "PUBLISHED",
		Status:          "OPEN",
		PublishType:     "External",
		PublishedDate:   "2021-07-15T02:32:30Z",
		CreatedAt:       "2021-07-15T02:26:28Z",
		IsRemote:        auzmorBool(true),
		EmploymentTypes: auzmorStrings("Full Time", "Permanent"),
		Department: &auzmorReference{
			UUID: secondDepartment.UUID,
			Name: secondDepartment.Name,
		},
	}
	groups := []auzmorDepartmentGroup{
		{
			Department: &firstDepartment,
			Count:      auzmorInt(1),
			Jobs:       &[]auzmorPosting{firstJob},
		},
		{
			Department: &secondDepartment,
			Count:      auzmorInt(1),
			Jobs:       &[]auzmorPosting{secondJob},
		},
	}
	return auzmorListResponse{
		Data: &groups,
		Pagination: &auzmorPagination{
			TotalItems:  auzmorInt(2),
			FilterItems: auzmorInt(2),
			Limit:       auzmorInt(auzmorJobLimit),
			Offset:      auzmorInt(0),
		},
	}
}

func auzmorPostingFixture() auzmorPosting {
	return auzmorPosting{
		UUID:          testAuzmorJobID,
		Title:         " Product Manager ",
		State:         "PUBLISHED",
		Status:        "OPEN",
		PublishType:   "External",
		PublishedDate: "2021-06-03T13:49:54Z",
		CreatedAt:     "2021-06-03T13:46:55Z",
		IsRemote:      auzmorBool(false),
		EmploymentTypes: auzmorStrings(
			"Full Time",
		),
		Department: &auzmorReference{
			UUID: testAuzmorDepartmentID,
			Name: " Product ",
		},
		Location: &auzmorReference{
			UUID: "9ce8ffe6f220441a935d38ff524e5b79",
			Name: " Bengaluru, Karnataka, India ",
		},
	}
}

func auzmorDetailFixture() auzmorDetailResponse {
	job := auzmorPostingFixture()
	job.Description = "<p>Build products.</p><p>Ship them.</p>"
	department := auzmorReference{UUID: testAuzmorDepartmentID, Name: "Product"}
	location := auzmorReference{
		UUID: "9ce8ffe6f220441a935d38ff524e5b79",
		Name: "Bengaluru, Karnataka, India",
	}
	return auzmorDetailResponse{
		Job:        &job,
		Department: &department,
		Location:   &location,
	}
}

func auzmorFixtureJob() model.Job {
	return model.Job{
		ID:             "auzmor/letstransport/" + testAuzmorJobID,
		Company:        "LetsTransport",
		Title:          "Product Manager",
		Location:       "Bengaluru, Karnataka, India",
		URL:            "https://hire.auzmor.com/letstransport/careers/" + testAuzmorJobID,
		EmploymentType: "Full Time",
		PostedAt:       time.Date(2021, 6, 3, 13, 49, 54, 0, time.UTC),
	}
}

func auzmorInt(value int) *int                 { return &value }
func auzmorInt64(value int64) *int64           { return &value }
func auzmorBool(value bool) *bool              { return &value }
func auzmorStrings(values ...string) *[]string { return &values }
func auzmorGroups(values ...auzmorDepartmentGroup) *[]auzmorDepartmentGroup {
	if values == nil {
		values = make([]auzmorDepartmentGroup, 0)
	}
	return &values
}
