package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestDarwinboxRegistrationPaginationDetailAndNormalization(t *testing.T) {
	const total = 12
	postings := darwinboxFixtureListJobs(total)
	postings[0].OfficeLocationDisplay = "Multiple locations"
	postings[0].TooltipLocations = darwinboxStrings(
		"Bengaluru, Karnataka, India ",
		" Mumbai, Maharashtra, India",
		"Bengaluru, Karnataka, India ",
	)
	postings[1].DesignationDisplayName = ""

	var (
		mu       sync.Mutex
		requests []string
		server   *httptest.Server
	)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.RequestURI())
		mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Accept"); got != "application/json, text/plain, */*" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("Accept-Language"); got != "en-US,en;q=0.9" {
			t.Errorf("Accept-Language = %q", got)
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "Chrome/138") {
			t.Errorf("User-Agent = %q", got)
		}
		wantReferer := server.URL + "/ms/candidatev2/main/careers/allJobs"
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Path {
		case "/ms/candidateapi/getCompanyConfig":
			if r.URL.RawQuery != "" {
				t.Errorf("config query = %q", r.URL.RawQuery)
			}
			if got := r.Header.Get("Referer"); got != wantReferer {
				t.Errorf("config Referer = %q, want %q", got, wantReferer)
			}
			darwinboxEncode(t, w, darwinboxFixtureConfig(total))
		case "/ms/candidateapi/job":
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if err != nil || len(r.URL.Query()) != 1 {
				t.Errorf("list query = %v", r.URL.Query())
			}
			if got := r.Header.Get("Referer"); got != wantReferer {
				t.Errorf("list Referer = %q, want %q", got, wantReferer)
			}
			start := (page - 1) * darwinboxPageSize
			end := start + darwinboxPageSize
			if end > total {
				end = total
			}
			darwinboxEncode(t, w, darwinboxFixtureListResponse(total, postings[start:end]))
		case "/ms/candidateapi/job/" + postings[0].ID:
			wantDetailReferer := server.URL +
				"/ms/candidatev2/main/careers/jobDetails/" + postings[0].ID
			if got := r.Header.Get("Referer"); got != wantDetailReferer {
				t.Errorf("detail Referer = %q, want %q", got, wantDetailReferer)
			}
			darwinboxEncode(t, w, darwinboxFixtureDetailResponse(postings[0]))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src, err := New(
		"darwinbox",
		"Moneyview",
		params.Map{"subdomain": " MoneyView ", "max_postings": "20"},
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("New returned %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*darwinbox)
	if !ok {
		t.Fatalf("wrapped source = %T, want *darwinbox", wrapped.Source)
	}
	implementation.apiBase = server.URL
	implementation.careersBase = server.URL + "/ms/candidatev2/main/careers"

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != total {
		t.Fatalf("len(jobs) = %d, want %d", len(jobs), total)
	}
	wantFirst := model.Job{
		ID:             "darwinbox/moneyview/a0000000000001",
		Company:        "Moneyview",
		Title:          "Engineer 1",
		Location:       "Bengaluru, Karnataka, India; Mumbai, Maharashtra, India",
		URL:            server.URL + "/ms/candidatev2/main/careers/jobDetails/a0000000000001",
		EmploymentType: "Full Time",
		PostedAt:       time.Unix(1783967401, 0).UTC(),
	}
	if !reflect.DeepEqual(jobs[0], wantFirst) {
		t.Fatalf("first job =\n%+v\nwant\n%+v", jobs[0], wantFirst)
	}
	if jobs[1].Title != "Engineer 2" {
		t.Fatalf("second title = %q", jobs[1].Title)
	}
	if _, ok := any(implementation).(Detailer); !ok {
		t.Fatal("*darwinbox does not implement Detailer")
	}
	if err := wrapped.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if got, want := jobs[0].Description, "Build & ship.\nUse Go."; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}

	mu.Lock()
	gotRequests := append([]string(nil), requests...)
	mu.Unlock()
	wantRequests := []string{
		"/ms/candidateapi/getCompanyConfig",
		"/ms/candidateapi/job?page=1",
		"/ms/candidateapi/job?page=2",
		"/ms/candidateapi/job/a0000000000001",
	}
	if !reflect.DeepEqual(gotRequests, wantRequests) {
		t.Fatalf("requests = %v, want %v", gotRequests, wantRequests)
	}
}

func TestDarwinboxFactoryValidation(t *testing.T) {
	tests := []struct {
		name    string
		params  params.Map
		wantErr string
	}{
		{name: "missing subdomain", params: params.Map{}, wantErr: `missing required param "subdomain"`},
		{name: "empty subdomain", params: params.Map{"subdomain": ""}, wantErr: `missing required param "subdomain"`},
		{name: "scheme", params: params.Map{"subdomain": "https://moneyview"}, wantErr: "invalid Darwinbox subdomain"},
		{name: "dot", params: params.Map{"subdomain": "moneyview.darwinbox.in"}, wantErr: "invalid Darwinbox subdomain"},
		{name: "underscore", params: params.Map{"subdomain": "money_view"}, wantErr: "invalid Darwinbox subdomain"},
		{name: "leading hyphen", params: params.Map{"subdomain": "-moneyview"}, wantErr: "invalid Darwinbox subdomain"},
		{name: "trailing hyphen", params: params.Map{"subdomain": "moneyview-"}, wantErr: "invalid Darwinbox subdomain"},
		{name: "too long", params: params.Map{"subdomain": strings.Repeat("a", 64)}, wantErr: "invalid Darwinbox subdomain"},
		{name: "zero cap", params: params.Map{"subdomain": "moneyview", "max_postings": "0"}, wantErr: "1 to 10000"},
		{name: "negative cap", params: params.Map{"subdomain": "moneyview", "max_postings": "-1"}, wantErr: "1 to 10000"},
		{name: "large cap", params: params.Map{"subdomain": "moneyview", "max_postings": "10001"}, wantErr: "1 to 10000"},
		{name: "invalid cap", params: params.Map{"subdomain": "moneyview", "max_postings": "many"}, wantErr: "expected integer"},
		{
			name: "unknown params",
			params: params.Map{
				"subdomain": "moneyview",
				"z":         "1",
				"a":         "2",
			},
			wantErr: "got a, z",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New("darwinbox", "Moneyview", test.params, nil); err == nil ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}

	src, err := New("darwinbox", "Moneyview", params.Map{"subdomain": " MONEYVIEW "}, nil)
	if err != nil {
		t.Fatal(err)
	}
	implementation := src.(*identifiedSource).Source.(*darwinbox)
	if implementation.subdomain != "moneyview" ||
		implementation.maxPostings != darwinboxDefaultMaxJobs ||
		implementation.client == nil {
		t.Fatalf("implementation = %+v", implementation)
	}
	if Identity(src) != "darwinbox/moneyview" ||
		StatePrefix(src) != "darwinbox/moneyview/" {
		t.Fatalf("identity/prefix = %q/%q", Identity(src), StatePrefix(src))
	}
}

func TestDarwinboxAllowsCompleteEmptyBoard(t *testing.T) {
	config := darwinboxFixtureConfig(0)
	pages := map[int]darwinboxListResponse{
		1: darwinboxFixtureListResponse(0, []darwinboxListJob{}),
	}
	jobs, requests, err := darwinboxFetchFixtures(t, config, pages, 100)
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
	}
	if !reflect.DeepEqual(requests, []string{
		"/ms/candidateapi/getCompanyConfig",
		"/ms/candidateapi/job?page=1",
	}) {
		t.Fatalf("requests = %v", requests)
	}
}

func TestDarwinboxRejectsInvalidCompanyConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*darwinboxConfigResponse)
		wantErr string
	}{
		{name: "status", mutate: func(r *darwinboxConfigResponse) { r.Status = "error" }, wantErr: "omitted successful status"},
		{name: "message", mutate: func(r *darwinboxConfigResponse) { r.Message = nil }, wantErr: "omitted successful status"},
		{name: "company", mutate: func(r *darwinboxConfigResponse) { r.Message.Company = nil }, wantErr: "omitted successful status"},
		{name: "company name", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.CompanyName = " " }, wantErr: "empty company_name"},
		{name: "subdomain", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.Subdomain = "other" }, wantErr: `identifies subdomain "other"`},
		{name: "tenant id empty", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.TenantID = "" }, wantErr: "invalid tenant_id"},
		{name: "tenant id unsafe", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.TenantID = "../135" }, wantErr: "invalid tenant_id"},
		{name: "recruitment omitted", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.RecruitmentEnabled = nil }, wantErr: "recruitment is not enabled"},
		{name: "recruitment disabled", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.RecruitmentEnabled = darwinboxBool(false) }, wantErr: "recruitment is not enabled"},
		{name: "v2 omitted", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.NewCareers = nil }, wantErr: "Candidate v2 careers are not enabled"},
		{name: "v2 disabled", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.NewCareers = darwinboxBool(false) }, wantErr: "Candidate v2 careers are not enabled"},
		{name: "preview omitted", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.IsPreview = nil }, wantErr: "missing a non-preview flag"},
		{name: "preview true", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.IsPreview = darwinboxBool(true) }, wantErr: "missing a non-preview flag"},
		{name: "count omitted", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.AllJobsCount = nil }, wantErr: "invalid allJobsCount"},
		{name: "count negative", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.AllJobsCount = darwinboxInt(-1) }, wantErr: "invalid allJobsCount"},
		{name: "hard cap", mutate: func(r *darwinboxConfigResponse) {
			r.Message.Company.AllJobsCount = darwinboxInt(darwinboxHardMaxJobs + 1)
		}, wantErr: "hard safety limit"},
		{name: "date format omitted", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.DateTimeFormat = nil }, wantErr: "omitted its timezone"},
		{name: "timezone empty", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.DateTimeFormat.Timezone = " " }, wantErr: "omitted its timezone"},
		{name: "timezone invalid", mutate: func(r *darwinboxConfigResponse) { r.Message.Company.DateTimeFormat.Timezone = "Mars/Olympus" }, wantErr: "invalid timezone"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := darwinboxFixtureConfig(0)
			test.mutate(&response)
			_, requests, err := darwinboxFetchFixtures(t, response, nil, 100)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
			if len(requests) != 1 || requests[0] != "/ms/candidateapi/getCompanyConfig" {
				t.Fatalf("requests = %v, want config only", requests)
			}
		})
	}
}

func TestDarwinboxRejectsDisabledUdaanBeforeListing(t *testing.T) {
	response := darwinboxFixtureConfig(21)
	response.Message.Company.CompanyName = "udaan"
	response.Message.Company.Subdomain = "udaan"
	response.Message.Company.RecruitmentEnabled = darwinboxBool(false)

	var listRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/ms/candidateapi/getCompanyConfig" {
			darwinboxEncode(t, w, response)
			return
		}
		listRequests.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	s := darwinboxFixtureSource("udaan", server)
	_, err := s.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "recruitment is not enabled") {
		t.Fatalf("error = %v", err)
	}
	if got := listRequests.Load(); got != 0 {
		t.Fatalf("list requests = %d, want 0", got)
	}
}

func TestDarwinboxEnforcesOperationalMaxBeforeListing(t *testing.T) {
	response := darwinboxFixtureConfig(11)
	var listRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/ms/candidateapi/getCompanyConfig" {
			darwinboxEncode(t, w, response)
			return
		}
		listRequests.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	s := darwinboxFixtureSource("moneyview", server)
	s.maxPostings = 10
	_, err := s.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeding max_postings=10") {
		t.Fatalf("error = %v", err)
	}
	if got := listRequests.Load(); got != 0 {
		t.Fatalf("list requests = %d, want 0", got)
	}
}

func TestDarwinboxRejectsIncompleteOrDriftedPagination(t *testing.T) {
	tests := []struct {
		name    string
		config  darwinboxConfigResponse
		pages   map[int]darwinboxListResponse
		wantErr string
	}{
		{
			name:   "status",
			config: darwinboxFixtureConfig(1),
			pages: map[int]darwinboxListResponse{
				1: func() darwinboxListResponse {
					r := darwinboxFixtureListResponse(1, darwinboxFixtureListJobs(1))
					r.Status = "error"
					return r
				}(),
			},
			wantErr: "omitted successful status",
		},
		{
			name:   "message omitted",
			config: darwinboxFixtureConfig(1),
			pages: map[int]darwinboxListResponse{
				1: {Status: "success"},
			},
			wantErr: "omitted successful status",
		},
		{
			name:   "count omitted",
			config: darwinboxFixtureConfig(1),
			pages: map[int]darwinboxListResponse{
				1: {Status: "success", Message: &darwinboxListMessage{
					Jobs: darwinboxListJobs(darwinboxFixtureListJobs(1)),
				}},
			},
			wantErr: "omitted successful status",
		},
		{
			name:   "jobs omitted",
			config: darwinboxFixtureConfig(1),
			pages: map[int]darwinboxListResponse{
				1: {Status: "success", Message: &darwinboxListMessage{
					JobsCount: darwinboxInt(1),
				}},
			},
			wantErr: "omitted successful status",
		},
		{
			name:   "count mismatch",
			config: darwinboxFixtureConfig(1),
			pages: map[int]darwinboxListResponse{
				1: darwinboxFixtureListResponse(2, darwinboxFixtureListJobs(1)),
			},
			wantErr: "company config reported 1",
		},
		{
			name:   "short first page",
			config: darwinboxFixtureConfig(11),
			pages: map[int]darwinboxListResponse{
				1: darwinboxFixtureListResponse(11, darwinboxFixtureListJobs(9)),
			},
			wantErr: "returned 9 jobs, want 10",
		},
		{
			name:   "short final page",
			config: darwinboxFixtureConfig(11),
			pages: map[int]darwinboxListResponse{
				1: darwinboxFixtureListResponse(11, darwinboxFixtureListJobs(10)),
				2: darwinboxFixtureListResponse(11, []darwinboxListJob{}),
			},
			wantErr: "returned 0 jobs, want 1",
		},
		{
			name:   "extra final item",
			config: darwinboxFixtureConfig(11),
			pages: map[int]darwinboxListResponse{
				1: darwinboxFixtureListResponse(11, darwinboxFixtureListJobs(10)),
				2: darwinboxFixtureListResponse(11, darwinboxFixtureListJobs(2)),
			},
			wantErr: "returned 2 jobs, want 1",
		},
		{
			name:   "count changes",
			config: darwinboxFixtureConfig(11),
			pages: map[int]darwinboxListResponse{
				1: darwinboxFixtureListResponse(11, darwinboxFixtureListJobs(10)),
				2: darwinboxFixtureListResponse(12, darwinboxFixtureListJobs(1)),
			},
			wantErr: "page 2 reported jobscount=12",
		},
		{
			name:   "duplicate across pages",
			config: darwinboxFixtureConfig(11),
			pages: map[int]darwinboxListResponse{
				1: darwinboxFixtureListResponse(11, darwinboxFixtureListJobs(10)),
				2: darwinboxFixtureListResponse(11, darwinboxFixtureListJobs(1)),
			},
			wantErr: `duplicate job id "a0000000000001"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobs, _, err := darwinboxFetchFixtures(t, test.config, test.pages, 100)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
			if jobs != nil {
				t.Fatalf("jobs = %#v, want nil on error", jobs)
			}
		})
	}
}

func TestDarwinboxRejectsInvalidListPosting(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*darwinboxListJob)
		wantErr string
	}{
		{name: "id empty", mutate: func(j *darwinboxListJob) { j.ID = "" }, wantErr: "invalid opaque job id"},
		{name: "id whitespace", mutate: func(j *darwinboxListJob) { j.ID = " " + j.ID }, wantErr: "invalid opaque job id"},
		{name: "id path", mutate: func(j *darwinboxListJob) { j.ID = "../job" }, wantErr: "invalid opaque job id"},
		{name: "id too long", mutate: func(j *darwinboxListJob) { j.ID = strings.Repeat("a", 129) }, wantErr: "invalid opaque job id"},
		{name: "title empty", mutate: func(j *darwinboxListJob) { j.Title = " " }, wantErr: "empty title"},
		{name: "display mismatch", mutate: func(j *darwinboxListJob) { j.DesignationDisplayName = "Other" }, wantErr: "does not match title"},
		{name: "employment empty", mutate: func(j *darwinboxListJob) { j.EmploymentType = " " }, wantErr: "empty emp_type"},
		{name: "locations omitted", mutate: func(j *darwinboxListJob) { j.TooltipLocations = nil }, wantErr: "omitted tool_tip_locations"},
		{name: "empty location item", mutate: func(j *darwinboxListJob) { j.TooltipLocations = darwinboxStrings("") }, wantErr: "item 0 is empty"},
		{
			name: "single display mismatch",
			mutate: func(j *darwinboxListJob) {
				j.OfficeLocationDisplay = "Mumbai"
				j.TooltipLocations = darwinboxStrings("Bengaluru")
			},
			wantErr: "does not match tool_tip_locations",
		},
		{
			name: "multi display mismatch",
			mutate: func(j *darwinboxListJob) {
				j.OfficeLocationDisplay = "Bengaluru"
				j.TooltipLocations = darwinboxStrings("Bengaluru", "Mumbai")
			},
			wantErr: "does not mark multiple locations",
		},
		{
			name: "empty location",
			mutate: func(j *darwinboxListJob) {
				j.OfficeLocationDisplay = ""
				j.TooltipLocations = darwinboxStrings()
			},
			wantErr: "location is empty",
		},
		{name: "created empty", mutate: func(j *darwinboxListJob) { j.CreatedOn = " " }, wantErr: "created_on is empty"},
		{name: "created invalid", mutate: func(j *darwinboxListJob) { j.CreatedOn = "2026-07-14" }, wantErr: "invalid created_on"},
		{name: "posting omitted", mutate: func(j *darwinboxListJob) { j.JobPostingOn = nil }, wantErr: "invalid job_posting_on"},
		{name: "posting zero", mutate: func(j *darwinboxListJob) { j.JobPostingOn = darwinboxInt64(0) }, wantErr: "invalid job_posting_on"},
		{name: "posting huge", mutate: func(j *darwinboxListJob) { j.JobPostingOn = darwinboxInt64(darwinboxMaxUnix + 1) }, wantErr: "invalid job_posting_on"},
		{name: "timezone empty", mutate: func(j *darwinboxListJob) { j.Timezone = "" }, wantErr: "does not match tenant timezone"},
		{name: "timezone mismatch", mutate: func(j *darwinboxListJob) { j.Timezone = "UTC" }, wantErr: "does not match tenant timezone"},
	}
	s := &darwinbox{
		company: "Moneyview", subdomain: "moneyview",
		careersBase: "https://moneyview.darwinbox.in/ms/candidatev2/main/careers",
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			posting := darwinboxFixtureListJobs(1)[0]
			test.mutate(&posting)
			if _, _, err := s.normalizeListJob(posting, "Asia/Kolkata"); err == nil ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestDarwinboxNormalizesMultipleLocationsAndTrimsDisplayFields(t *testing.T) {
	posting := darwinboxFixtureListJobs(1)[0]
	posting.Title = " Engineer 1 "
	posting.DesignationDisplayName = "Engineer 1 "
	posting.EmploymentType = " Full Time "
	posting.OfficeLocationDisplay = " Multiple locations "
	posting.TooltipLocations = darwinboxStrings(" Bengaluru ", "Mumbai", " Bengaluru ")
	s := &darwinbox{
		company: "Renamed Moneyview", subdomain: "moneyview",
		careersBase: "https://moneyview.darwinbox.in/ms/candidatev2/main/careers",
	}
	job, id, err := s.normalizeListJob(posting, "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	if id != posting.ID || job.Title != "Engineer 1" ||
		job.EmploymentType != "Full Time" ||
		job.Location != "Bengaluru; Mumbai" {
		t.Fatalf("job = %+v id=%q", job, id)
	}
}

func TestDarwinboxDetailRejectsInvalidJobWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	s := darwinboxFixtureSource("moneyview", server)
	valid := darwinboxFixtureModelJob(s, darwinboxFixtureListJobs(1)[0])
	tests := []struct {
		name    string
		job     *model.Job
		wantErr string
	}{
		{name: "nil", job: nil, wantErr: "nil job"},
		{name: "prefix", job: darwinboxJobCopy(valid, func(j *model.Job) { j.ID = "darwinbox/other/a0000000000001" }), wantErr: "does not have prefix"},
		{name: "opaque id", job: darwinboxJobCopy(valid, func(j *model.Job) { j.ID = "darwinbox/moneyview/../job" }), wantErr: "invalid opaque job id"},
		{name: "URL", job: darwinboxJobCopy(valid, func(j *model.Job) { j.URL += "?from=all" }), wantErr: "does not match canonical URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := s.Detail(context.Background(), test.job); err == nil ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestDarwinboxDetailValidationAndAtomicity(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*darwinboxDetailResponse)
		wantErr string
	}{
		{name: "status", mutate: func(r *darwinboxDetailResponse) { r.Status = "error" }, wantErr: "omitted successful status"},
		{name: "message", mutate: func(r *darwinboxDetailResponse) { r.Message = nil }, wantErr: "omitted successful status"},
		{name: "job omitted", mutate: func(r *darwinboxDetailResponse) { r.Message.Job = nil }, wantErr: "omitted successful status"},
		{
			name: "job empty",
			mutate: func(r *darwinboxDetailResponse) {
				empty := []darwinboxDetailJob{}
				r.Message.Job = &empty
			},
			wantErr: "returned 0 jobs",
		},
		{
			name: "job multiple",
			mutate: func(r *darwinboxDetailResponse) {
				*r.Message.Job = append(*r.Message.Job, (*r.Message.Job)[0])
			},
			wantErr: "returned 2 jobs",
		},
		{name: "id invalid", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].ID = "../job" }, wantErr: "invalid opaque job id"},
		{name: "id mismatch", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].ID = "a0000000000002" }, wantErr: `returned job id "a0000000000002"`},
		{name: "title empty", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].Title = " " }, wantErr: "does not match list title"},
		{name: "title mismatch", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].Title = "Other" }, wantErr: "does not match list title"},
		{name: "display mismatch", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].DesignationDisplayName = "Other" }, wantErr: "does not match title"},
		{name: "type empty", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].EmploymentType = " " }, wantErr: "does not match list value"},
		{name: "type mismatch", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].EmploymentType = "Contract" }, wantErr: "does not match list value"},
		{name: "location omitted", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].TooltipLocations = nil }, wantErr: "omitted tool_tip_locations"},
		{name: "location mismatch", mutate: func(r *darwinboxDetailResponse) {
			d := &(*r.Message.Job)[0]
			d.OfficeLocationDisplay = "Mumbai"
			d.TooltipLocations = darwinboxStrings("Mumbai")
		}, wantErr: "does not match list value"},
		{name: "created invalid", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].CreatedOn = "yesterday" }, wantErr: "invalid created_on"},
		{name: "posting omitted", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].JobPostingOn = nil }, wantErr: "invalid job_posting_on"},
		{name: "posted omitted", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].PostedOn = nil }, wantErr: "invalid posted_on"},
		{name: "detail timestamps differ", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].PostedOn = darwinboxInt64(1783967402) }, wantErr: "posting timestamps do not match"},
		{name: "remote omitted", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].IsRemote = nil }, wantErr: "invalid is_remote"},
		{name: "remote invalid", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].IsRemote = darwinboxInt(2) }, wantErr: "invalid is_remote"},
		{name: "timezone empty", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].Timezone = " " }, wantErr: "empty timezone"},
		{name: "timezone invalid", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].Timezone = "Mars/Olympus" }, wantErr: "invalid timezone"},
		{name: "description empty", mutate: func(r *darwinboxDetailResponse) { (*r.Message.Job)[0].Description = "<p> </p>" }, wantErr: "empty jd"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			posting := darwinboxFixtureListJobs(1)[0]
			response := darwinboxFixtureDetailResponse(posting)
			test.mutate(&response)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				darwinboxEncode(t, w, response)
			}))
			defer server.Close()
			s := darwinboxFixtureSource("moneyview", server)
			job := darwinboxFixtureModelJob(s, posting)
			job.Description = "existing description"
			before := job
			err := s.Detail(context.Background(), &job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
			if !reflect.DeepEqual(job, before) {
				t.Fatalf("job changed on error:\n got %+v\nwant %+v", job, before)
			}
		})
	}
}

func TestDarwinboxDetailRejectsListTimestampMismatchAtomically(t *testing.T) {
	posting := darwinboxFixtureListJobs(1)[0]
	response := darwinboxFixtureDetailResponse(posting)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		darwinboxEncode(t, w, response)
	}))
	defer server.Close()
	s := darwinboxFixtureSource("moneyview", server)
	job := darwinboxFixtureModelJob(s, posting)
	job.PostedAt = job.PostedAt.Add(time.Second)
	before := job
	err := s.Detail(context.Background(), &job)
	if err == nil || !strings.Contains(err.Error(), "posting timestamps do not match") {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(job, before) {
		t.Fatalf("job changed on error: got %+v want %+v", job, before)
	}
}

func TestDarwinboxHTTPBoundaryValidation(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		bodyLimit int64
		wantErr   string
	}{
		{
			name: "status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				http.Error(w, `{"status":"error"}`, http.StatusForbidden)
			},
			wantErr: "403 Forbidden",
		},
		{
			name: "content type missing",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{}`)
			},
			wantErr: "unexpected Content-Type",
		},
		{
			name: "content type HTML",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, `{}`)
			},
			wantErr: "unexpected Content-Type",
		},
		{
			name: "invalid JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{`)
			},
			wantErr: "decoding response",
		},
		{
			name: "trailing JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{} {}`)
			},
			wantErr: "trailing JSON",
		},
		{
			name: "body limit",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"long":"value"}`)
			},
			bodyLimit: 3,
			wantErr:   "response exceeds 3-byte",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			s := darwinboxFixtureSource("moneyview", server)
			limit := test.bodyLimit
			if limit == 0 {
				limit = 1024
			}
			var out any
			err := s.getJSON(
				context.Background(),
				server.URL+"/ms/candidateapi/getCompanyConfig",
				server.URL+"/ms/candidatev2/main/careers/allJobs",
				limit,
				&out,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestDarwinboxRejectsRedirectsAndUnexpectedFinalURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	s := darwinboxFixtureSource("moneyview", server)
	var out any
	err := s.getJSON(
		context.Background(),
		server.URL+"/redirect",
		server.URL+"/ms/candidatev2/main/careers/allJobs",
		1024,
		&out,
	)
	if err == nil || !strings.Contains(err.Error(), "302 Found") {
		t.Fatalf("redirect error = %v", err)
	}

	s.client = &http.Client{Transport: darwinboxRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})}
	err = s.getJSON(
		context.Background(),
		server.URL+"/ms/candidateapi/getCompanyConfig",
		server.URL+"/ms/candidatev2/main/careers/allJobs",
		1024,
		&out,
	)
	if err == nil || !strings.Contains(err.Error(), "unexpected final URL") {
		t.Fatalf("final URL error = %v", err)
	}
}

func TestDarwinboxRejectsUntrustedRequestInputs(t *testing.T) {
	s := &darwinbox{
		subdomain:   "moneyview",
		apiBase:     "https://moneyview.darwinbox.in",
		careersBase: "https://moneyview.darwinbox.in/ms/candidatev2/main/careers",
		client: &http.Client{Transport: darwinboxRoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("must not request")
		})},
	}
	tests := []struct {
		name     string
		endpoint string
		referer  string
		wantErr  string
	}{
		{
			name:     "endpoint host",
			endpoint: "https://evil.example/ms/candidateapi/job?page=1",
			referer:  s.careersBase + "/allJobs",
			wantErr:  "untrusted Darwinbox endpoint",
		},
		{
			name:     "endpoint userinfo",
			endpoint: "https://user@moneyview.darwinbox.in/ms/candidateapi/job?page=1",
			referer:  s.careersBase + "/allJobs",
			wantErr:  "untrusted Darwinbox endpoint",
		},
		{
			name:     "endpoint fragment",
			endpoint: s.apiBase + "/ms/candidateapi/job?page=1#fragment",
			referer:  s.careersBase + "/allJobs",
			wantErr:  "untrusted Darwinbox endpoint",
		},
		{
			name:     "referer host",
			endpoint: s.apiBase + "/ms/candidateapi/job?page=1",
			referer:  "https://evil.example/allJobs",
			wantErr:  "untrusted Darwinbox Referer",
		},
		{
			name:     "referer query",
			endpoint: s.apiBase + "/ms/candidateapi/job?page=1",
			referer:  s.careersBase + "/allJobs?",
			wantErr:  "untrusted Darwinbox Referer",
		},
		{
			name:     "referer path",
			endpoint: s.apiBase + "/ms/candidateapi/job?page=1",
			referer:  s.careersBase + "/other",
			wantErr:  "untrusted Darwinbox Referer",
		},
		{
			name:     "referer encoded id",
			endpoint: s.apiBase + "/ms/candidateapi/job?page=1",
			referer:  s.careersBase + "/jobDetails/%61000000000001",
			wantErr:  "untrusted Darwinbox Referer",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out any
			err := s.getJSON(
				context.Background(), test.endpoint, test.referer, 1024, &out,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestDarwinboxPropagatesContextAndTransportErrors(t *testing.T) {
	s := &darwinbox{
		subdomain:   "moneyview",
		apiBase:     "https://moneyview.darwinbox.in",
		careersBase: "https://moneyview.darwinbox.in/ms/candidatev2/main/careers",
		client: &http.Client{Transport: darwinboxRoundTripper(func(req *http.Request) (*http.Response, error) {
			return nil, req.Context().Err()
		})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out any
	err := s.getJSON(
		ctx,
		s.apiBase+"/ms/candidateapi/getCompanyConfig",
		s.careersBase+"/allJobs",
		1024,
		&out,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestDarwinboxConcurrentFetchAndDetail(t *testing.T) {
	const workers = 8
	posting := darwinboxFixtureListJobs(1)[0]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/ms/candidateapi/getCompanyConfig":
			darwinboxEncode(t, w, darwinboxFixtureConfig(1))
		case r.URL.Path == "/ms/candidateapi/job":
			darwinboxEncode(t, w, darwinboxFixtureListResponse(1, []darwinboxListJob{posting}))
		case r.URL.Path == "/ms/candidateapi/job/"+posting.ID:
			darwinboxEncode(t, w, darwinboxFixtureDetailResponse(posting))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s := darwinboxFixtureSource("moneyview", server)

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobs, err := s.Fetch(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if len(jobs) != 1 {
				errs <- fmt.Errorf("got %d jobs", len(jobs))
				return
			}
			if err := s.Detail(context.Background(), &jobs[0]); err != nil {
				errs <- err
				return
			}
			if jobs[0].Description != "Build & ship.\nUse Go." {
				errs <- fmt.Errorf("description = %q", jobs[0].Description)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestDarwinboxStableIDsIgnoreDisplayNameAndOperationalLimit(t *testing.T) {
	posting := darwinboxFixtureListJobs(1)[0]
	first := &darwinbox{
		company: "Moneyview", subdomain: "moneyview", maxPostings: 10,
		careersBase: "https://moneyview.darwinbox.in/ms/candidatev2/main/careers",
	}
	second := &darwinbox{
		company: "Renamed Moneyview", subdomain: "moneyview", maxPostings: 100,
		careersBase: "https://moneyview.darwinbox.in/ms/candidatev2/main/careers",
	}
	a, _, err := first.normalizeListJob(posting, "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := second.normalizeListJob(posting, "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.URL != b.URL {
		t.Fatalf("stable identity changed: %+v vs %+v", a, b)
	}
	if a.Company == b.Company {
		t.Fatalf("display names unexpectedly match: %q", a.Company)
	}
}

func darwinboxFetchFixtures(
	t *testing.T,
	config darwinboxConfigResponse,
	pages map[int]darwinboxListResponse,
	maxPostings int,
) ([]model.Job, []string, error) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ms/candidateapi/getCompanyConfig":
			darwinboxEncode(t, w, config)
		case "/ms/candidateapi/job":
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if err != nil {
				http.Error(w, "bad page", http.StatusBadRequest)
				return
			}
			response, ok := pages[page]
			if !ok {
				http.Error(w, "missing fixture page", http.StatusInternalServerError)
				return
			}
			darwinboxEncode(t, w, response)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s := darwinboxFixtureSource("moneyview", server)
	s.maxPostings = maxPostings
	jobs, err := s.Fetch(context.Background())
	mu.Lock()
	gotRequests := append([]string(nil), requests...)
	mu.Unlock()
	return jobs, gotRequests, err
}

func darwinboxFixtureSource(subdomain string, server *httptest.Server) *darwinbox {
	return &darwinbox{
		company:     strings.ToUpper(subdomain),
		subdomain:   subdomain,
		maxPostings: darwinboxDefaultMaxJobs,
		apiBase:     server.URL,
		careersBase: server.URL + "/ms/candidatev2/main/careers",
		client:      server.Client(),
	}
}

func darwinboxFixtureConfig(total int) darwinboxConfigResponse {
	return darwinboxConfigResponse{
		Status: "success",
		Message: &darwinboxConfigMessage{Company: &darwinboxCompanyConfig{
			CompanyName:        "Moneyview",
			Subdomain:          "moneyview",
			TenantID:           "135",
			RecruitmentEnabled: darwinboxBool(true),
			NewCareers:         darwinboxBool(true),
			IsPreview:          darwinboxBool(false),
			AllJobsCount:       darwinboxInt(total),
			DateTimeFormat:     &darwinboxDateTimeFormat{Timezone: "Asia/Kolkata"},
		}},
	}
}

func darwinboxFixtureListResponse(total int, jobs []darwinboxListJob) darwinboxListResponse {
	return darwinboxListResponse{
		Status: "success",
		Message: &darwinboxListMessage{
			JobsCount: darwinboxInt(total),
			Jobs:      darwinboxListJobs(jobs),
		},
	}
}

func darwinboxFixtureListJobs(count int) []darwinboxListJob {
	jobs := make([]darwinboxListJob, count)
	for index := range jobs {
		id := fmt.Sprintf("a%013x", index+1)
		jobs[index] = darwinboxListJob{
			ID:                     id,
			DesignationDisplayName: fmt.Sprintf("Engineer %d", index+1),
			CreatedOn:              "2026-07-14T06:55:28.000Z",
			OfficeLocationDisplay:  "Bengaluru, Karnataka, India",
			JobPostingOn:           darwinboxInt64(1783967400 + int64(index+1)),
			Department:             "Engineering",
			EmploymentType:         "Full Time",
			Title:                  fmt.Sprintf("Engineer %d", index+1),
			TooltipLocations:       darwinboxStrings("Bengaluru, Karnataka, India"),
			Timezone:               "Asia/Kolkata",
		}
	}
	return jobs
}

func darwinboxFixtureDetailResponse(posting darwinboxListJob) darwinboxDetailResponse {
	return darwinboxDetailResponse{
		Status: "success",
		Message: &darwinboxDetailMessage{Job: darwinboxDetailJobs(darwinboxDetailJob{
			ID:                     posting.ID,
			DesignationDisplayName: posting.DesignationDisplayName,
			CreatedOn:              posting.CreatedOn,
			Description:            "&lt;p&gt;Build &amp;amp; ship.&lt;/p&gt;&lt;ul&gt;&lt;li&gt;Use Go.&lt;/li&gt;&lt;/ul&gt;",
			IsRemote:               darwinboxInt(0),
			OfficeLocationDisplay:  posting.OfficeLocationDisplay,
			JobPostingOn:           darwinboxInt64(*posting.JobPostingOn),
			PostedOn:               darwinboxInt64(*posting.JobPostingOn),
			EmploymentType:         posting.EmploymentType,
			Title:                  posting.Title,
			TooltipLocations:       darwinboxStrings((*posting.TooltipLocations)...),
			Timezone:               posting.Timezone,
		})},
	}
}

func darwinboxFixtureModelJob(s *darwinbox, posting darwinboxListJob) model.Job {
	job, _, err := s.normalizeListJob(posting, "Asia/Kolkata")
	if err != nil {
		panic(err)
	}
	return job
}

func darwinboxJobCopy(job model.Job, mutate func(*model.Job)) *model.Job {
	copy := job
	mutate(&copy)
	return &copy
}

func darwinboxEncode(t *testing.T, w io.Writer, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func darwinboxBool(value bool) *bool    { return &value }
func darwinboxInt(value int) *int       { return &value }
func darwinboxInt64(value int64) *int64 { return &value }
func darwinboxStrings(values ...string) *[]string {
	return &values
}
func darwinboxListJobs(values []darwinboxListJob) *[]darwinboxListJob {
	return &values
}
func darwinboxDetailJobs(values ...darwinboxDetailJob) *[]darwinboxDetailJob {
	return &values
}

type darwinboxRoundTripper func(*http.Request) (*http.Response, error)

func (f darwinboxRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
