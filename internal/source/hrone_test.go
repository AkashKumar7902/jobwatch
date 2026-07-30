package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	testHROneDomain      = "addverb"
	testHROneRequestType = "request_type_token"
	testHROneCompanyCode = "company_code_token"
	testHROneAPIKey      = "abcdefghijklmnopqrstuvwxyz_123456"
)

func TestHROneNewValidatesAndCanonicalizesCoordinates(t *testing.T) {
	t.Parallel()

	config := params.Map{
		"domain_code":  " AddVerb ",
		"api_key":      " " + testHROneAPIKey + " ",
		"request_type": testHROneRequestType,
		"company_code": testHROneCompanyCode,
	}
	src, err := New("hrone", "Addverb", config, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("source type = %T, want *identifiedSource", src)
	}
	got, ok := wrapped.Source.(*hrone)
	if !ok {
		t.Fatalf("wrapped source type = %T, want *hrone", wrapped.Source)
	}
	if got.domainCode != testHROneDomain {
		t.Errorf("domainCode = %q, want %q", got.domainCode, testHROneDomain)
	}
	if got.apiKey != testHROneAPIKey {
		t.Errorf("apiKey was not trimmed")
	}
	if got.client != http.DefaultClient {
		t.Errorf("nil client did not default to http.DefaultClient")
	}
	if got.apiBase != hroneAPIBase || got.portalBase != hronePortalBase {
		t.Errorf("production bases = %q, %q", got.apiBase, got.portalBase)
	}
	if got.maxPages != hroneMaximumPages {
		t.Errorf("maxPages = %d, want %d", got.maxPages, hroneMaximumPages)
	}
	if got.Company() != "Addverb" {
		t.Errorf("Company = %q", got.Company())
	}
	if got.boardIdentity() != "hrone/addverb/request_type_token/company_code_token" {
		t.Errorf("boardIdentity = %q", got.boardIdentity())
	}

	portal, err := url.Parse(got.portalURL())
	if err != nil {
		t.Fatalf("parse portal URL: %v", err)
	}
	if portal.Scheme != "https" || portal.Host != "career.hrone.cloud" ||
		portal.Path != "/career-portal" {
		t.Errorf("portal URL = %q", got.portalURL())
	}
	wantQuery := url.Values{
		"appId": {testHROneAPIKey},
		"dc":    {testHROneDomain},
		"rqt":   {testHROneRequestType},
		"cc":    {testHROneCompanyCode},
	}
	if !reflect.DeepEqual(portal.Query(), wantQuery) {
		t.Errorf("portal query = %#v, want %#v", portal.Query(), wantQuery)
	}

	tests := []struct {
		name    string
		params  params.Map
		wantErr string
	}{
		{
			name: "missing domain code",
			params: params.Map{
				"api_key": testHROneAPIKey, "request_type": testHROneRequestType,
				"company_code": testHROneCompanyCode,
			},
			wantErr: `missing required param "domain_code"`,
		},
		{
			name: "domain can not alter headers",
			params: params.Map{
				"domain_code": "addverb\r\nX-Evil: yes", "api_key": testHROneAPIKey,
				"request_type": testHROneRequestType, "company_code": testHROneCompanyCode,
			},
			wantErr: "invalid HROne domain code",
		},
		{
			name: "missing api key",
			params: params.Map{
				"domain_code": testHROneDomain, "request_type": testHROneRequestType,
				"company_code": testHROneCompanyCode,
			},
			wantErr: `missing required param "api_key"`,
		},
		{
			name: "short api key",
			params: params.Map{
				"domain_code": testHROneDomain, "api_key": "short",
				"request_type": testHROneRequestType, "company_code": testHROneCompanyCode,
			},
			wantErr: "URL-safe token",
		},
		{
			name: "missing request type",
			params: params.Map{
				"domain_code": testHROneDomain, "api_key": testHROneAPIKey,
				"company_code": testHROneCompanyCode,
			},
			wantErr: `missing required param "request_type"`,
		},
		{
			name: "request type can not alter path",
			params: params.Map{
				"domain_code": testHROneDomain, "api_key": testHROneAPIKey,
				"request_type": "../request", "company_code": testHROneCompanyCode,
			},
			wantErr: "URL-safe token",
		},
		{
			name: "missing company code",
			params: params.Map{
				"domain_code": testHROneDomain, "api_key": testHROneAPIKey,
				"request_type": testHROneRequestType,
			},
			wantErr: `missing required param "company_code"`,
		},
		{
			name: "company code can not alter path",
			params: params.Map{
				"domain_code": testHROneDomain, "api_key": testHROneAPIKey,
				"request_type": testHROneRequestType, "company_code": "code/other",
			},
			wantErr: "URL-safe token",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := New("hrone", "Example", test.params, http.DefaultClient)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestHROneBoardIDsSurviveRenameAndAPIKeyRotation(t *testing.T) {
	t.Parallel()

	firstConfig := params.Map{
		"domain_code": " AddVerb ", "api_key": testHROneAPIKey,
		"request_type": testHROneRequestType, "company_code": testHROneCompanyCode,
	}
	secondConfig := params.Map{
		"domain_code": "addverb", "api_key": strings.Repeat("z", 40),
		"request_type": testHROneRequestType, "company_code": testHROneCompanyCode,
	}
	firstSource, err := New("hrone", "Old display name", firstConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondSource, err := New("hrone", "New display name", secondConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if Identity(firstSource) != Identity(secondSource) {
		t.Fatalf("API-key rotation changed identity: %q != %q", Identity(firstSource), Identity(secondSource))
	}
	if Identity(firstSource) != "hrone/addverb/request_type_token/company_code_token" {
		t.Fatalf("identity = %q", Identity(firstSource))
	}
	if StatePrefix(firstSource) != "hrone/addverb/request_type_token/company_code_token/" {
		t.Fatalf("state prefix = %q", StatePrefix(firstSource))
	}

	first := firstSource.(*identifiedSource).Source.(*hrone)
	second := secondSource.(*identifiedSource).Source.(*hrone)

	want := "hrone/addverb/request_type_token/company_code_token/encrypted_position_01"
	if got := first.jobID("encrypted_position_01"); got != want {
		t.Fatalf("first jobID = %q, want %q", got, want)
	}
	if got := second.jobID("encrypted_position_01"); got != want {
		t.Fatalf("rotated jobID = %q, want %q", got, want)
	}
}

func TestHROneFetchPaginatesWithExactWireModelAndNormalizes(t *testing.T) {
	t.Parallel()

	postings := make([]hronePosting, 16)
	for index := range postings {
		postings[index] = testHROnePosting(index + 1)
	}
	*postings[0].JobTitle = "  Staff Robotics Engineer  "
	*postings[0].PreferredLocation = "  Noida  "
	postings[1].PreferredLocation = nil

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		testHROneHeaders(t, r, http.MethodPost)
		if r.URL.Path != "/api/external/referral/CareerPosition/Details" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		wantBody := testHROneSearchBody(int(call))
		if string(body) != wantBody {
			t.Errorf("request body = %s\nwant = %s", body, wantBody)
		}
		start := (int(call) - 1) * hronePageSize
		end := min(start+hronePageSize, len(postings))
		if start > end {
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}
		writeHROneJSON(t, w, postings[start:end])
	}))
	defer server.Close()

	src := testHROneSource(server)
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if len(jobs) != len(postings) {
		t.Fatalf("jobs = %d, want %d", len(jobs), len(postings))
	}

	first := jobs[0]
	if first.ID != "hrone/addverb/request_type_token/company_code_token/encrypted_position_001" {
		t.Errorf("ID = %q", first.ID)
	}
	if first.Company != "Addverb" || first.Title != "Staff Robotics Engineer" {
		t.Errorf("company/title = %q/%q", first.Company, first.Title)
	}
	if first.Location != "Noida" {
		t.Errorf("Location = %q", first.Location)
	}
	if first.Description != "" || first.EmploymentType != "" || !first.PostedAt.IsZero() {
		t.Errorf("list unexpectedly populated lazy fields: %#v", first)
	}
	if jobs[1].Location != "" {
		t.Errorf("nullable preferredLocation = %q, want empty", jobs[1].Location)
	}
	application, err := url.Parse(first.URL)
	if err != nil {
		t.Fatalf("parse application URL: %v", err)
	}
	if application.Scheme != "https" || application.Host != "career.example.test" ||
		application.Path != "/apply-job" {
		t.Errorf("application URL = %q", first.URL)
	}
	wantQuery := url.Values{
		"appId": {testHROneAPIKey},
		"dc":    {testHROneDomain},
		"rqt":   {testHROneRequestType},
		"cc":    {testHROneCompanyCode},
		"pid":   {"encrypted_position_001"},
		"dptc":  {"department_code_token"},
		"st":    {"source_type_token"},
		"fm":    {"CR"},
	}
	if !reflect.DeepEqual(application.Query(), wantQuery) {
		t.Errorf("application query = %#v, want %#v", application.Query(), wantQuery)
	}

	src.mu.RLock()
	cached := len(src.postingsByID)
	src.mu.RUnlock()
	if cached != len(postings) {
		t.Errorf("cached postings = %d, want %d", cached, len(postings))
	}
}

func TestHROneFetchAcceptsEmptyBoardAndReplacesDetailCache(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeHROneJSON(t, w, []hronePosting{})
	}))
	defer server.Close()
	src := testHROneSource(server)
	src.postingsByID["old_position"] = testHROnePosting(1)

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want empty", jobs)
	}
	src.mu.RLock()
	cached := len(src.postingsByID)
	src.mu.RUnlock()
	if cached != 0 {
		t.Errorf("cached postings = %d, want empty", cached)
	}
}

func TestHROneFetchRejectsDriftDuplicatesAndUnsafePagination(t *testing.T) {
	t.Parallel()

	valid := testHROnePosting(1)
	tests := []struct {
		name     string
		response any
		wantErr  string
	}{
		{
			name:     "object instead of list",
			response: map[string]any{"validationType": "3", "message": "expired"},
			wantErr:  "decoding response",
		},
		{
			name:     "null instead of list",
			response: nil,
			wantErr:  "null instead of a postings array",
		},
		{
			name:     "too many rows",
			response: testHROnePostings(hronePageSize + 1),
			wantErr:  "requested at most",
		},
		{
			name: "required field omitted",
			response: func() []hronePosting {
				bad := valid
				bad.SourceType = nil
				return []hronePosting{bad}
			}(),
			wantErr: "omitted required HROne fields",
		},
		{
			name: "empty title",
			response: func() []hronePosting {
				bad := valid
				bad.JobTitle = testHROnePointer(" ")
				return []hronePosting{bad}
			}(),
			wantErr: "empty jobTitle",
		},
		{
			name: "invalid position ID",
			response: func() []hronePosting {
				bad := valid
				bad.PositionID = testHROnePointer(int64(0))
				return []hronePosting{bad}
			}(),
			wantErr: "invalid positionId",
		},
		{
			name: "unsafe encrypted ID",
			response: func() []hronePosting {
				bad := valid
				bad.EncryptedPositionID = testHROnePointer("../position")
				return []hronePosting{bad}
			}(),
			wantErr: "invalid encryptedPositionId",
		},
		{
			name: "unsafe source type",
			response: func() []hronePosting {
				bad := valid
				bad.SourceType = testHROnePointer("source/type")
				return []hronePosting{bad}
			}(),
			wantErr: "invalid sourceType",
		},
		{
			name: "empty job code",
			response: func() []hronePosting {
				bad := valid
				bad.JobCode = testHROnePointer("")
				return []hronePosting{bad}
			}(),
			wantErr: "empty jobCode",
		},
		{
			name: "invalid department code",
			response: func() []hronePosting {
				bad := valid
				bad.DepartmentCode = testHROnePointer("bad/code")
				return []hronePosting{bad}
			}(),
			wantErr: "invalid departmentCode",
		},
		{
			name: "invalid experience range",
			response: func() []hronePosting {
				bad := valid
				bad.ExperienceFrom = testHROnePointer(7.0)
				bad.ExperienceTo = testHROnePointer(3.0)
				return []hronePosting{bad}
			}(),
			wantErr: "invalid experience range",
		},
		{
			name: "duplicate encrypted ID",
			response: func() []hronePosting {
				duplicate := testHROnePosting(2)
				duplicate.EncryptedPositionID = valid.EncryptedPositionID
				return []hronePosting{valid, duplicate}
			}(),
			wantErr: "duplicate encryptedPositionId",
		},
		{
			name: "duplicate position ID",
			response: func() []hronePosting {
				duplicate := testHROnePosting(2)
				duplicate.PositionID = valid.PositionID
				return []hronePosting{valid, duplicate}
			}(),
			wantErr: "duplicate positionId",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeHROneJSON(t, w, test.response)
			}))
			defer server.Close()
			_, err := testHROneSource(server).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestHROneFetchPreservesPreviousCacheOnFailedRefresh(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writeHROneJSON(t, w, testHROnePostings(hronePageSize))
			return
		}
		http.Error(w, "upstream failed", http.StatusBadGateway)
	}))
	defer server.Close()
	src := testHROneSource(server)
	src.postingsByID["previous_position"] = testHROnePosting(99)

	if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("Fetch error = %v, want 502", err)
	}
	src.mu.RLock()
	_, preserved := src.postingsByID["previous_position"]
	cacheSize := len(src.postingsByID)
	src.mu.RUnlock()
	if !preserved || cacheSize != 1 {
		t.Errorf("failed refresh replaced cache: %#v", src.postingsByID)
	}
}

func TestHROneFetchEnforcesPageSafetyLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request hroneSearchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		start := (request.Pagination.PageNumber - 1) * hronePageSize
		postings := make([]hronePosting, hronePageSize)
		for index := range postings {
			postings[index] = testHROnePosting(start + index + 1)
		}
		writeHROneJSON(t, w, postings)
	}))
	defer server.Close()
	src := testHROneSource(server)
	src.maxPages = 2
	_, err := src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "safety limit of 2 pages") {
		t.Fatalf("Fetch error = %v, want page safety limit", err)
	}

	src.maxPages = 0
	_, err = src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "max pages must be between") {
		t.Fatalf("Fetch with zero maxPages error = %v", err)
	}
}

func TestHROneDetailValidatesAndPopulatesLazyFields(t *testing.T) {
	t.Parallel()

	posting := testHROnePosting(1)
	detail := testHROneDetail(posting)
	detail.JobDescriptionBodyWithHTML = testHROnePointer(
		"<section><h2>What you will do</h2><p>Build &amp; ship robots.</p></section>",
	)
	detail.PreferredLocationList = &[]hronePreferredLocation{
		{ID: testHROnePointer("1047"), Text: testHROnePointer(" Noida ")},
		{ID: testHROnePointer("1048"), Text: testHROnePointer("Bengaluru")},
		{ID: testHROnePointer("1049"), Text: testHROnePointer("Noida")},
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			testHROneHeaders(t, r, http.MethodPost)
			writeHROneJSON(t, w, []hronePosting{posting})
			return
		}
		testHROneHeaders(t, r, http.MethodGet)
		wantPath := "/api/external/referral/JobOpening/Request/Details/" +
			"encrypted_position_001/" + testHROneCompanyCode + "/source_type_token"
		if r.URL.Path != wantPath {
			t.Errorf("detail path = %q, want %q", r.URL.Path, wantPath)
		}
		if r.ContentLength > 0 {
			t.Errorf("GET detail ContentLength = %d, want no body", r.ContentLength)
		}
		writeHROneJSON(t, w, detail)
	}))
	defer server.Close()

	src := testHROneSource(server)
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	got := jobs[0]
	if got.Title != "Robotics Engineer 1" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Description != "What you will do\nBuild & ship robots." {
		t.Errorf("Description = %q", got.Description)
	}
	if got.EmploymentType != "Permanent" {
		t.Errorf("EmploymentType = %q", got.EmploymentType)
	}
	if got.Location != "Noida; Bengaluru" {
		t.Errorf("Location = %q", got.Location)
	}
}

func TestHROneDetailFallsBackToPlainDescriptionAndListLocation(t *testing.T) {
	t.Parallel()

	posting := testHROnePosting(1)
	detail := testHROneDetail(posting)
	detail.JobDescriptionBodyWithHTML = testHROnePointer("")
	detail.JobDescriptionBody = testHROnePointer("<p>Plain fallback.</p>")
	detail.PreferredLocationList = &[]hronePreferredLocation{}
	server := testHROneDetailServer(t, detail, http.StatusOK)
	defer server.Close()
	src := testHROneSource(server)
	src.postingsByID[*posting.EncryptedPositionID] = posting
	job, _, _, err := src.normalizePosting(posting)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if err := src.Detail(context.Background(), &job); err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if job.Description != "Plain fallback." {
		t.Errorf("Description = %q", job.Description)
	}
	if job.Location != "Noida" {
		t.Errorf("Location = %q, want list location fallback", job.Location)
	}
}

func TestHROneDetailRejectsForeignOrUnfetchedJobs(t *testing.T) {
	t.Parallel()

	src := testHROneSource(nil)
	tests := []struct {
		name    string
		job     *model.Job
		wantErr string
	}{
		{name: "nil", job: nil, wantErr: "nil job"},
		{
			name: "foreign board",
			job: &model.Job{
				ID: "hrone/other/request_type_token/company_code_token/encrypted_position_001",
			},
			wantErr: "does not belong",
		},
		{
			name: "invalid token",
			job: &model.Job{
				ID: src.jobID("../bad"),
			},
			wantErr: "invalid job ID",
		},
		{
			name: "not from latest fetch",
			job: &model.Job{
				ID: src.jobID("encrypted_position_001"),
			},
			wantErr: "latest fetch",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := src.Detail(context.Background(), test.job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Detail error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestHROneDetailRejectsClosedMismatchedAndDriftedResponsesAtomically(t *testing.T) {
	t.Parallel()

	posting := testHROnePosting(1)
	base := testHROneDetail(posting)
	tests := []struct {
		name       string
		mutate     func(*hroneDetail)
		statusCode int
		raw        string
		wantErr    string
	}{
		{
			name: "required field omitted",
			mutate: func(detail *hroneDetail) {
				detail.EmployeeType = nil
			},
			wantErr: "omitted required HROne fields",
		},
		{
			name: "request ID mismatch",
			mutate: func(detail *hroneDetail) {
				detail.RequestID = testHROnePointer(int64(9999))
			},
			wantErr: "does not match positionId",
		},
		{
			name: "title mismatch",
			mutate: func(detail *hroneDetail) {
				detail.JobTitle = testHROnePointer("Different title")
			},
			wantErr: "jobTitle does not match",
		},
		{
			name: "job code mismatch",
			mutate: func(detail *hroneDetail) {
				detail.JobCode = testHROnePointer("OTHER")
			},
			wantErr: "jobCode does not match",
		},
		{
			name: "inactive",
			mutate: func(detail *hroneDetail) {
				detail.CurrentStatus = testHROnePointer(0)
			},
			wantErr: "is not open",
		},
		{
			name: "closed for all",
			mutate: func(detail *hroneDetail) {
				detail.IsClosedForAll = testHROnePointer(1)
			},
			wantErr: "is not open",
		},
		{
			name: "can not add candidate",
			mutate: func(detail *hroneDetail) {
				detail.CanAddCandidate = testHROnePointer(0)
			},
			wantErr: "is not open",
		},
		{
			name: "empty descriptions",
			mutate: func(detail *hroneDetail) {
				detail.JobDescriptionBodyWithHTML = testHROnePointer(" ")
				detail.JobDescriptionBody = testHROnePointer("")
			},
			wantErr: "empty job description",
		},
		{
			name: "empty employee type",
			mutate: func(detail *hroneDetail) {
				detail.EmployeeType = testHROnePointer(" ")
			},
			wantErr: "empty employeeType",
		},
		{
			name: "location schema omitted",
			mutate: func(detail *hroneDetail) {
				detail.PreferredLocationList = &[]hronePreferredLocation{
					{ID: testHROnePointer("1047")},
				}
			},
			wantErr: "omitted id or text",
		},
		{
			name:       "HTTP failure",
			statusCode: http.StatusBadGateway,
			wantErr:    "502 Bad Gateway",
		},
		{
			name:    "invalid JSON",
			raw:     `{"requestId":`,
			wantErr: "decoding response",
		},
		{
			name:    "trailing JSON",
			raw:     `{} {}`,
			wantErr: "trailing JSON",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			detail := base
			if test.mutate != nil {
				test.mutate(&detail)
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.statusCode != 0 && test.statusCode != http.StatusOK {
					http.Error(w, "upstream failed", test.statusCode)
					return
				}
				if test.raw != "" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, test.raw)
					return
				}
				writeHROneJSON(t, w, detail)
			})
			server := httptest.NewServer(handler)
			defer server.Close()
			src := testHROneSource(server)
			src.postingsByID[*posting.EncryptedPositionID] = posting
			job, _, _, err := src.normalizePosting(posting)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			before := job

			err = src.Detail(context.Background(), &job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Detail error = %v, want substring %q", err, test.wantErr)
			}
			if !reflect.DeepEqual(job, before) {
				t.Errorf("Detail partially mutated job on error:\n got %#v\nwant %#v", job, before)
			}
		})
	}
}

func TestHROneHTTPResponseBodyLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `["`+strings.Repeat("x", hroneListBodyLimit)+`"]`)
	}))
	defer server.Close()
	_, err := testHROneSource(server).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("Fetch error = %v, want response limit", err)
	}
}

func testHROneSource(server *httptest.Server) *hrone {
	apiBase := "https://app.example.test"
	client := http.DefaultClient
	if server != nil {
		apiBase = server.URL
		client = server.Client()
	}
	return &hrone{
		company:      "Addverb",
		domainCode:   testHROneDomain,
		apiKey:       testHROneAPIKey,
		requestType:  testHROneRequestType,
		companyCode:  testHROneCompanyCode,
		apiBase:      apiBase,
		portalBase:   "https://career.example.test",
		client:       client,
		maxPages:     hroneMaximumPages,
		postingsByID: make(map[string]hronePosting),
	}
}

func testHROnePosting(number int) hronePosting {
	return hronePosting{
		JobTitle:            testHROnePointer(fmt.Sprintf("Robotics Engineer %d", number)),
		PositionID:          testHROnePointer(int64(1000 + number)),
		EncryptedPositionID: testHROnePointer(fmt.Sprintf("encrypted_position_%03d", number)),
		SourceType:          testHROnePointer("source_type_token"),
		JobCode:             testHROnePointer(fmt.Sprintf("ADD-%04d", number)),
		DepartmentCode:      testHROnePointer("department_code_token"),
		PreferredLocation:   testHROnePointer("Noida"),
		ExperienceFrom:      testHROnePointer(2.0),
		ExperienceTo:        testHROnePointer(5.0),
	}
}

func testHROnePostings(count int) []hronePosting {
	postings := make([]hronePosting, count)
	for index := range postings {
		postings[index] = testHROnePosting(index + 1)
	}
	return postings
}

func testHROneDetail(posting hronePosting) hroneDetail {
	locations := []hronePreferredLocation{
		{ID: testHROnePointer("1047"), Text: testHROnePointer("Noida")},
	}
	return hroneDetail{
		RequestID:                  posting.PositionID,
		JobTitle:                   posting.JobTitle,
		JobCode:                    posting.JobCode,
		CurrentStatus:              testHROnePointer(1),
		IsClosedForAll:             testHROnePointer(0),
		CanAddCandidate:            testHROnePointer(1),
		JobDescriptionBodyWithHTML: testHROnePointer("<p>Build robots.</p>"),
		JobDescriptionBody:         testHROnePointer("Build robots."),
		EmployeeType:               testHROnePointer("Permanent"),
		PreferredLocationList:      &locations,
	}
}

func testHROneDetailServer(t *testing.T, detail hroneDetail, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if statusCode != http.StatusOK {
			http.Error(w, "upstream failed", statusCode)
			return
		}
		writeHROneJSON(t, w, detail)
	}))
}

func testHROneHeaders(t *testing.T, r *http.Request, method string) {
	t.Helper()
	if r.Method != method {
		t.Errorf("method = %q, want %q", r.Method, method)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	if got := r.Header.Get("domainCode"); got != testHROneDomain {
		t.Errorf("domainCode = %q", got)
	}
	if got := r.Header.Get("apiKey"); got != testHROneAPIKey {
		t.Errorf("apiKey = %q", got)
	}
	if got := r.Header.Get("AccessMode"); got != "W" {
		t.Errorf("AccessMode = %q", got)
	}
	if got := r.Header.Get("User-Agent"); !strings.Contains(got, "jobwatch") {
		t.Errorf("User-Agent = %q", got)
	}
	if method == http.MethodPost {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
	}
}

func testHROneSearchBody(pageNumber int) string {
	return `{"departmentCode":"","companyCode":"` + testHROneCompanyCode +
		`","careerPortalType":"` + testHROneRequestType +
		`","jobTitle":"","employmentType":"","seniorityName":"","jobFunction":"","company":"",` +
		`"businessUnitCode":"","department":"","subDepartment":"","gradeCode":"","designationCode":"",` +
		`"levelCode":"","branchCode":"","subBranchCode":"","regionCode":"","locationId":"","experience":"",` +
		`"qualification":"","skillsName":"","urgentOpening":"","jobPosted":"0","isShortUrl":false,` +
		`"pagination":{"pageNumber":` + strconv.Itoa(pageNumber) +
		`,"pageSize":15},"nationality":"","preferredLocationId":""}`
}

func writeHROneJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func testHROnePointer[T any](value T) *T {
	return &value
}
