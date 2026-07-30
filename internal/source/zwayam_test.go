package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	testZwayamDomain    = "careers.example.test"
	testZwayamCompanyID = "15470"
)

func TestZwayamNewValidatesCanonicalCoordinates(t *testing.T) {
	t.Parallel()

	src, err := New("zwayam", "Cult.fit", params.Map{
		"domain": "Careers.Cult.FIT.", "company_id": testZwayamCompanyID,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("source type = %T, want *identifiedSource", src)
	}
	got, ok := wrapped.Source.(*zwayam)
	if !ok {
		t.Fatalf("wrapped source type = %T, want *zwayam", wrapped.Source)
	}
	if got.domain != "careers.cult.fit" || got.companyNumber != 15470 ||
		got.encodedID != "MTU0NzA=" || got.origin != "https://careers.cult.fit" {
		t.Fatalf("normalized source = %#v", got)
	}
	if got.client != http.DefaultClient {
		t.Errorf("nil client did not use http.DefaultClient")
	}

	tests := []struct {
		name    string
		params  params.Map
		wantErr string
	}{
		{
			name: "missing domain",
			params: params.Map{
				"company_id": testZwayamCompanyID,
			},
			wantErr: `missing required param "domain"`,
		},
		{
			name: "domain contains scheme",
			params: params.Map{
				"domain": "https://careers.cult.fit", "company_id": testZwayamCompanyID,
			},
			wantErr: "invalid board host",
		},
		{
			name: "missing company id",
			params: params.Map{
				"domain": "careers.cult.fit",
			},
			wantErr: `missing required param "company_id"`,
		},
		{
			name: "base64 company id",
			params: params.Map{
				"domain": "careers.cult.fit", "company_id": "MTU0NzA=",
			},
			wantErr: "canonical positive decimal",
		},
		{
			name: "zero company id",
			params: params.Map{
				"domain": "careers.cult.fit", "company_id": "0",
			},
			wantErr: "canonical positive decimal",
		},
		{
			name: "leading zero company id",
			params: params.Map{
				"domain": "careers.cult.fit", "company_id": "015470",
			},
			wantErr: "canonical positive decimal",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := New("zwayam", "Cult.fit", test.params, http.DefaultClient)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestZwayamFetchDiscoversSitePaginatesAndNormalizes(t *testing.T) {
	t.Parallel()

	hits := make([]*zwayamSearchHit, 11)
	for index := range hits {
		hits[index] = testZwayamHit(index + 1)
	}
	hits[0].Source.JobTitle = "  Fitness Manager  "
	hits[0].Source.Locations = []zwayamLocation{
		{FormattedLocation: " Bengaluru, Karnataka, India "},
		{Location: "Chennai", State: "Tamil Nadu", Country: "India"},
	}

	var searchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><base href="/cult/"></head></html>`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/jobs/search" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		assertZwayamBrowserHeaders(t, r, serverURLFromRequest(r))
		values, err := readZwayamMultipart(r)
		if err != nil {
			t.Errorf("read multipart: %v", err)
			http.Error(w, "invalid multipart", http.StatusBadRequest)
			return
		}
		if values["domain"] != testZwayamDomain {
			t.Errorf("domain = %q", values["domain"])
		}
		if values["companyId"] != base64.StdEncoding.EncodeToString([]byte(testZwayamCompanyID)) {
			t.Errorf("companyId = %q", values["companyId"])
		}
		var filter zwayamSearchFilter
		if err := json.Unmarshal([]byte(values["filterCri"]), &filter); err != nil {
			t.Errorf("decode filterCri: %v", err)
			http.Error(w, "invalid filter", http.StatusBadRequest)
			return
		}
		if filter.SelectedCall != "sort" || filter.SortCriteria.Name != "modifiedDate" ||
			filter.SortCriteria.IsAscending || filter.AnyOfTheseWords != "" {
			t.Errorf("filterCri = %#v", filter)
		}
		call := searchCalls.Add(1)
		var results []*zwayamSearchHit
		switch filter.PaginationStartNo {
		case 0:
			results = hits[:10]
		case 10:
			results = hits[10:]
		default:
			t.Errorf("unexpected paginationStartNo %d", filter.PaginationStartNo)
			http.Error(w, "unexpected offset", http.StatusBadRequest)
			return
		}
		if wantCall := int32(filter.PaginationStartNo/zwayamPageSize + 1); call != wantCall {
			t.Errorf("search call = %d, want %d", call, wantCall)
		}
		writeZwayamJSON(t, w, testZwayamPage(results, len(hits), filter.PaginationStartNo == 0))
	}))
	defer server.Close()

	src := testZwayamSource(server)
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := searchCalls.Load(); got != 2 {
		t.Fatalf("search requests = %d, want 2", got)
	}
	if len(jobs) != len(hits) {
		t.Fatalf("jobs = %d, want %d", len(jobs), len(hits))
	}
	first := jobs[0]
	if first.ID != "zwayam/"+testZwayamDomain+"/"+testZwayamCompanyID+"/1" {
		t.Errorf("ID = %q", first.ID)
	}
	if first.Company != "Cult.fit" || first.Title != "Fitness Manager" {
		t.Errorf("job = %#v", first)
	}
	if first.Location != "Bengaluru, Karnataka, India; Chennai, Tamil Nadu, India" {
		t.Errorf("Location = %q", first.Location)
	}
	if first.URL != server.URL+"/cult/jobview/role-1" {
		t.Errorf("URL = %q", first.URL)
	}
	if first.Description != "" || first.EmploymentType != "" {
		t.Errorf("lazy fields were populated: %#v", first)
	}
	wantDate := time.Date(2026, time.February, 6, 0, 0, 0, 0, time.UTC)
	if !first.PostedAt.Equal(wantDate) {
		t.Errorf("PostedAt = %s, want %s", first.PostedAt, wantDate)
	}
	if jobs[10].ID != "zwayam/"+testZwayamDomain+"/"+testZwayamCompanyID+"/11" {
		t.Errorf("last ID = %q", jobs[10].ID)
	}
}

func TestZwayamFetchAcceptsExplicitEmptyBoard(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<base href="/cult/">`)
		case r.Method == http.MethodPost:
			writeZwayamJSON(t, w, testZwayamPage([]*zwayamSearchHit{}, 0, false))
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	jobs, err := testZwayamSource(server).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want empty", jobs)
	}
}

func TestZwayamDetailHydratesFullConfiguration(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/jobs-service/v1/jobs/careersite" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		assertZwayamBrowserHeaders(t, r, serverURLFromRequest(r))
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var request zwayamDetailRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode detail request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		want := zwayamDetailRequest{
			CompanyID: 15470, JobURL: "fitness-manager", ExternalSource: "CareerSite", CampusURL: "empty",
		}
		if request != want {
			t.Errorf("detail request = %#v, want %#v", request, want)
		}
		writeZwayamJSON(t, w, testZwayamDetail(902496, "fitness-manager"))
	}))
	defer server.Close()

	src := testZwayamSource(server)
	job := model.Job{
		ID:  src.jobID("902496"),
		URL: server.URL + "/cult/jobview/fitness-manager",
	}
	if err := src.Detail(context.Background(), &job); err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("detail calls = %d", calls.Load())
	}
	if job.Title != "Fitness Manager" || job.Location != "Bengaluru" {
		t.Errorf("normalized detail = %#v", job)
	}
	for _, text := range []string{
		"Description:", "Build & operate studios.", "Role:", "Ship safely.",
		"Skills Required:", "Operations", "Years Of Exp:", "Up to 4 years",
	} {
		if !strings.Contains(job.Description, text) {
			t.Errorf("Description %q does not contain %q", job.Description, text)
		}
	}
	wantDate := time.Date(2026, time.February, 6, 0, 0, 0, 0, time.UTC)
	if !job.PostedAt.Equal(wantDate) {
		t.Errorf("PostedAt = %s, want %s", job.PostedAt, wantDate)
	}
}

func TestZwayamCapturedWireFixturesUseProductionNames(t *testing.T) {
	t.Parallel()

	const searchPayload = `{
		"code":200,
		"data":{
			"data":[{
				"_id":"902496",
				"_source":{
					"id":902496,
					"companyId":15470,
					"jobTitle":"Fitness Manager",
					"jobUrl":"fitness-manager-bengaluru-2026020614214383",
					"location":"Bengaluru",
					"createDate":"06-Feb-2026",
					"status":1,
					"displayStatus":"Open",
					"requisitionStatus":"A"
				}
			}],
			"totalCount":1,
			"hasMoreData":false,
			"facetedSearchConfig":{"paginationHowMuch":"10"}
		}
	}`
	var search zwayamSearchResponse
	if err := json.Unmarshal([]byte(searchPayload), &search); err != nil {
		t.Fatalf("decode captured search payload: %v", err)
	}
	if search.Code == nil || *search.Code != 200 || search.Data == nil ||
		search.Data.Results == nil || len(*search.Data.Results) != 1 ||
		(*search.Data.Results)[0].Source.JobURL != "fitness-manager-bengaluru-2026020614214383" {
		t.Fatalf("decoded search = %#v", search)
	}

	const detailPayload = `{
		"id":902496,
		"companyId":15470,
		"jobUrl":"fitness-manager-bengaluru-2026020614214383",
		"status":1,
		"jobTitle":"Fitness Manager",
		"location":"Bengaluru",
		"createDate":"06-Feb-2026",
		"jobConfigurationData":{
			"Description":"<p>Full description</p>",
			"Years Of Exp":"Up to 4 years"
		}
	}`
	var detail zwayamDetailResponse
	if err := json.Unmarshal([]byte(detailPayload), &detail); err != nil {
		t.Fatalf("decode captured detail payload: %v", err)
	}
	if detail.ID == nil || *detail.ID != 902496 || detail.JobConfigurationData == nil ||
		len(*detail.JobConfigurationData) != 2 {
		t.Fatalf("decoded detail = %#v", detail)
	}
}

func TestZwayamFetchRejectsPaginationDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		build   func(call int32) zwayamSearchResponse
		wantErr string
	}{
		{
			name: "missing response code",
			build: func(_ int32) zwayamSearchResponse {
				response := testZwayamPage([]*zwayamSearchHit{}, 0, false)
				response.Code = nil
				return response
			},
			wantErr: "response code is not 200",
		},
		{
			name: "non-success response code",
			build: func(_ int32) zwayamSearchResponse {
				response := testZwayamPage([]*zwayamSearchHit{}, 0, false)
				response.Code = zwayamInt(500)
				return response
			},
			wantErr: "response code is not 200",
		},
		{
			name: "missing data",
			build: func(_ int32) zwayamSearchResponse {
				response := testZwayamPage([]*zwayamSearchHit{}, 0, false)
				response.Data = nil
				return response
			},
			wantErr: "response omitted data",
		},
		{
			name: "missing result list",
			build: func(_ int32) zwayamSearchResponse {
				response := testZwayamPage([]*zwayamSearchHit{}, 0, false)
				response.Data.Results = nil
				return response
			},
			wantErr: "omitted pagination fields",
		},
		{
			name: "missing total",
			build: func(_ int32) zwayamSearchResponse {
				response := testZwayamPage([]*zwayamSearchHit{}, 0, false)
				response.Data.TotalCount = nil
				return response
			},
			wantErr: "omitted pagination fields",
		},
		{
			name: "missing has more",
			build: func(_ int32) zwayamSearchResponse {
				response := testZwayamPage([]*zwayamSearchHit{}, 0, false)
				response.Data.HasMoreData = nil
				return response
			},
			wantErr: "omitted pagination fields",
		},
		{
			name: "missing pagination config",
			build: func(_ int32) zwayamSearchResponse {
				response := testZwayamPage([]*zwayamSearchHit{}, 0, false)
				response.Data.FacetedSearchConfig = nil
				return response
			},
			wantErr: "omitted pagination fields",
		},
		{
			name: "changed page size",
			build: func(_ int32) zwayamSearchResponse {
				response := testZwayamPage([]*zwayamSearchHit{}, 0, false)
				value := stringish("20")
				response.Data.FacetedSearchConfig.PaginationHowMuch = &value
				return response
			},
			wantErr: `paginationHowMuch is "20", want 10`,
		},
		{
			name: "negative total",
			build: func(_ int32) zwayamSearchResponse {
				return testZwayamPage([]*zwayamSearchHit{}, -1, false)
			},
			wantErr: "negative totalCount",
		},
		{
			name: "unreasonable total",
			build: func(_ int32) zwayamSearchResponse {
				return testZwayamPage([]*zwayamSearchHit{}, zwayamMaximumPostings+1, false)
			},
			wantErr: "exceeds safety limit",
		},
		{
			name: "empty page before total",
			build: func(_ int32) zwayamSearchResponse {
				return testZwayamPage([]*zwayamSearchHit{}, 1, true)
			},
			wantErr: "empty page",
		},
		{
			name: "page exceeds fixed size",
			build: func(_ int32) zwayamSearchResponse {
				return testZwayamPage(testZwayamHits(11), 11, false)
			},
			wantErr: "returned 11 jobs",
		},
		{
			name: "page exceeds total",
			build: func(_ int32) zwayamSearchResponse {
				return testZwayamPage(testZwayamHits(2), 1, false)
			},
			wantErr: "would exceed reported total",
		},
		{
			name: "short page before total",
			build: func(_ int32) zwayamSearchResponse {
				return testZwayamPage(testZwayamHits(1), 2, true)
			},
			wantErr: "short page",
		},
		{
			name: "incorrect has more",
			build: func(_ int32) zwayamSearchResponse {
				return testZwayamPage(testZwayamHits(1), 1, true)
			},
			wantErr: "hasMoreData is true, want false",
		},
		{
			name: "total changes",
			build: func(call int32) zwayamSearchResponse {
				if call == 1 {
					return testZwayamPage(testZwayamHits(10), 11, true)
				}
				return testZwayamPage([]*zwayamSearchHit{testZwayamHit(11)}, 12, true)
			},
			wantErr: "totalCount changed",
		},
		{
			name: "duplicate stable id across pages",
			build: func(call int32) zwayamSearchResponse {
				if call == 1 {
					return testZwayamPage(testZwayamHits(10), 11, true)
				}
				return testZwayamPage([]*zwayamSearchHit{testZwayamHit(1)}, 11, false)
			},
			wantErr: "duplicate posting id",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, `<base href="/cult/">`)
					return
				}
				writeZwayamJSON(t, w, test.build(calls.Add(1)))
			}))
			defer server.Close()

			_, err := testZwayamSource(server).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestZwayamFetchRejectsRecordDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*zwayamSearchHit)
		wantErr string
	}{
		{name: "null hit", mutate: nil, wantErr: "record omitted _source"},
		{
			name: "missing source",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source = nil
			},
			wantErr: "record omitted _source",
		},
		{
			name: "non-canonical id",
			mutate: func(hit *zwayamSearchHit) {
				hit.ID = "01"
			},
			wantErr: "invalid _id",
		},
		{
			name: "source id changed",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source.ID = zwayamInt64(2)
			},
			wantErr: "omitted or changed source id",
		},
		{
			name: "company changed",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source.CompanyID = zwayamInt64(99)
			},
			wantErr: "omitted or changed companyId",
		},
		{
			name: "numeric status closed",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source.Status = zwayamInt(0)
			},
			wantErr: "not consistently marked open",
		},
		{
			name: "display status closed",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source.DisplayStatus = "Closed"
			},
			wantErr: "not consistently marked open",
		},
		{
			name: "requisition status changed",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source.RequisitionStatus = "C"
			},
			wantErr: "not consistently marked open",
		},
		{
			name: "missing title",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source.JobTitle = " "
			},
			wantErr: "omitted jobTitle",
		},
		{
			name: "unsafe slug",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source.JobURL = "../another/job"
			},
			wantErr: "invalid jobUrl",
		},
		{
			name: "invalid create date",
			mutate: func(hit *zwayamSearchHit) {
				hit.Source.CreateDate = "yesterday"
			},
			wantErr: "unsupported createDate",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, `<base href="/cult/">`)
					return
				}
				var hit *zwayamSearchHit
				if test.mutate != nil {
					hit = testZwayamHit(1)
					test.mutate(hit)
				}
				writeZwayamJSON(t, w, testZwayamPage([]*zwayamSearchHit{hit}, 1, false))
			}))
			defer server.Close()

			_, err := testZwayamSource(server).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestZwayamFetchRejectsTransportDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name: "HTTP failure", status: http.StatusBadGateway,
			contentType: "text/plain", body: "upstream failed", wantErr: "502 Bad Gateway",
		},
		{
			name: "wrong content type", status: http.StatusOK,
			contentType: "text/html", body: `{}`, wantErr: "unexpected Content-Type",
		},
		{
			name: "malformed JSON", status: http.StatusOK,
			contentType: "application/json", body: `{"code":`, wantErr: "decoding response",
		},
		{
			name: "trailing JSON", status: http.StatusOK,
			contentType: "application/json", body: `{} {}`, wantErr: "trailing JSON",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, `<base href="/cult/">`)
					return
				}
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()

			_, err := testZwayamSource(server).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestZwayamSiteDiscoveryRejectsUnsafeOrDriftedPages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "missing base", body: `<html></html>`, wantErr: "0 non-empty base hrefs"},
		{
			name:    "multiple bases",
			body:    `<base href="/cult/"><base href="/other/">`,
			wantErr: "2 non-empty base hrefs",
		},
		{
			name:    "foreign base",
			body:    `<base href="https://evil.example/jobs/">`,
			wantErr: "leaves board host",
		},
		{
			name:    "query in base",
			body:    `<base href="/cult/?tenant=other">`,
			wantErr: "unsafe career-site base href",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()

			_, err := testZwayamSource(server).discoverSiteBase(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("discoverSiteBase error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestZwayamSiteDiscoveryRejectsForeignRedirect(t *testing.T) {
	t.Parallel()

	targetCalls := atomic.Int32{}
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	_, err := testZwayamSource(server).discoverSiteBase(context.Background())
	if err == nil || !strings.Contains(err.Error(), "redirect leaves career-site host") {
		t.Fatalf("discoverSiteBase error = %v", err)
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("foreign redirect was followed")
	}
}

func TestZwayamDetailRejectsForeignJobWithoutRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	src := testZwayamSource(server)

	tests := []struct {
		name    string
		job     *model.Job
		wantErr string
	}{
		{name: "nil job", job: nil, wantErr: "nil job"},
		{
			name: "foreign source",
			job: &model.Job{
				ID: "ukg/example/job", URL: server.URL + "/cult/jobview/role-1",
			},
			wantErr: "does not belong",
		},
		{
			name: "foreign board",
			job: &model.Job{
				ID: "zwayam/other.example/15470/1", URL: server.URL + "/cult/jobview/role-1",
			},
			wantErr: "does not belong",
		},
		{
			name: "invalid posting id",
			job: &model.Job{
				ID: src.jobID("01"), URL: server.URL + "/cult/jobview/role-1",
			},
			wantErr: "invalid job ID",
		},
		{
			name: "foreign URL",
			job: &model.Job{
				ID: src.jobID("1"), URL: "https://evil.example/cult/jobview/role-1",
			},
			wantErr: "not a canonical URL",
		},
		{
			name: "wrong URL route",
			job: &model.Job{
				ID: src.jobID("1"), URL: server.URL + "/cult/jobs/role-1",
			},
			wantErr: "does not contain a jobview path",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			err := src.Detail(context.Background(), test.job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Detail error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("unexpected detail requests = %d", calls.Load())
	}
}

func TestZwayamDetailRejectsIncompleteOrClosedResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*zwayamDetailResponse)
		wantErr string
	}{
		{
			name: "missing id",
			mutate: func(detail *zwayamDetailResponse) {
				detail.ID = nil
			},
			wantErr: "omitted or changed id",
		},
		{
			name: "changed id",
			mutate: func(detail *zwayamDetailResponse) {
				detail.ID = zwayamInt64(2)
			},
			wantErr: "omitted or changed id",
		},
		{
			name: "changed company",
			mutate: func(detail *zwayamDetailResponse) {
				detail.CompanyID = zwayamInt64(99)
			},
			wantErr: "omitted or changed companyId",
		},
		{
			name: "closed posting",
			mutate: func(detail *zwayamDetailResponse) {
				detail.Status = zwayamInt(0)
			},
			wantErr: "response is not open",
		},
		{
			name: "changed slug",
			mutate: func(detail *zwayamDetailResponse) {
				detail.JobURL = "another-role"
			},
			wantErr: "response changed jobUrl",
		},
		{
			name: "missing title",
			mutate: func(detail *zwayamDetailResponse) {
				detail.JobTitle = " "
			},
			wantErr: "omitted jobTitle",
		},
		{
			name: "missing configuration",
			mutate: func(detail *zwayamDetailResponse) {
				detail.JobConfigurationData = nil
			},
			wantErr: "omitted jobConfigurationData",
		},
		{
			name: "empty configuration",
			mutate: func(detail *zwayamDetailResponse) {
				empty := map[string]json.RawMessage{}
				detail.JobConfigurationData = &empty
			},
			wantErr: "jobConfigurationData is empty",
		},
		{
			name: "non-string configuration",
			mutate: func(detail *zwayamDetailResponse) {
				invalid := map[string]json.RawMessage{
					"Description": json.RawMessage(`{"html":"unexpected"}`),
				}
				detail.JobConfigurationData = &invalid
			},
			wantErr: `field "Description" is not a string`,
		},
		{
			name: "blank configuration",
			mutate: func(detail *zwayamDetailResponse) {
				blank := map[string]json.RawMessage{
					"Description": json.RawMessage(`"<p> </p>"`),
					"Role":        json.RawMessage(`null`),
				}
				detail.JobConfigurationData = &blank
			},
			wantErr: "contains no description text",
		},
		{
			name: "invalid create date",
			mutate: func(detail *zwayamDetailResponse) {
				detail.CreateDate = "last Thursday"
			},
			wantErr: "unsupported createDate",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				detail := testZwayamDetail(1, "role-1")
				test.mutate(&detail)
				writeZwayamJSON(t, w, detail)
			}))
			defer server.Close()

			src := testZwayamSource(server)
			job := model.Job{
				ID:          src.jobID("1"),
				Title:       "Original title",
				Description: "Original description",
				URL:         server.URL + "/cult/jobview/role-1",
			}
			err := src.Detail(context.Background(), &job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Detail error = %v, want substring %q", err, test.wantErr)
			}
			if job.Title != "Original title" || job.Description != "Original description" {
				t.Fatalf("Detail mutated job on error: %#v", job)
			}
		})
	}
}

func TestZwayamDetailRejectsTransportDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name: "HTTP failure", status: http.StatusGone,
			contentType: "text/plain", body: "closed", wantErr: "410 Gone",
		},
		{
			name: "wrong content type", status: http.StatusOK,
			contentType: "text/html", body: `{}`, wantErr: "unexpected Content-Type",
		},
		{
			name: "malformed JSON", status: http.StatusOK,
			contentType: "application/json", body: `{"id":`, wantErr: "decoding response",
		},
		{
			name: "trailing JSON", status: http.StatusOK,
			contentType: "application/json", body: `{} {}`, wantErr: "trailing JSON",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			src := testZwayamSource(server)
			job := model.Job{ID: src.jobID("1"), URL: server.URL + "/cult/jobview/role-1"}
			err := src.Detail(context.Background(), &job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Detail error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestZwayamDescriptionOrdersKnownAndDynamicFields(t *testing.T) {
	t.Parallel()

	configuration := map[string]json.RawMessage{
		"Zeta":        json.RawMessage(`"Last"`),
		"Role":        json.RawMessage(`"<p>Second</p>"`),
		"Description": json.RawMessage(`"<p>First</p>"`),
		"Alpha":       json.RawMessage(`"Third"`),
	}
	description, err := zwayamDescription(configuration)
	if err != nil {
		t.Fatalf("zwayamDescription: %v", err)
	}
	wantOrder := []string{"Description:", "Role:", "Alpha:", "Zeta:"}
	last := -1
	for _, marker := range wantOrder {
		index := strings.Index(description, marker)
		if index <= last {
			t.Fatalf("description order = %q", description)
		}
		last = index
	}
}

func testZwayamSource(server *httptest.Server) *zwayam {
	return &zwayam{
		company:       "Cult.fit",
		domain:        testZwayamDomain,
		companyID:     testZwayamCompanyID,
		companyNumber: 15470,
		encodedID:     base64.StdEncoding.EncodeToString([]byte(testZwayamCompanyID)),
		origin:        server.URL,
		apiBase:       server.URL,
		client:        server.Client(),
	}
}

func testZwayamHit(index int) *zwayamSearchHit {
	id := int64(index)
	return &zwayamSearchHit{
		ID: strconv.Itoa(index),
		Source: &zwayamSearchSource{
			ID:                &id,
			CompanyID:         zwayamInt64(15470),
			JobTitle:          fmt.Sprintf("Role %d", index),
			JobURL:            fmt.Sprintf("role-%d", index),
			Location:          fmt.Sprintf("City %d", index),
			CreateDate:        "06-Feb-2026",
			Status:            zwayamInt(1),
			DisplayStatus:     "Open",
			RequisitionStatus: "A",
		},
	}
}

func testZwayamHits(count int) []*zwayamSearchHit {
	hits := make([]*zwayamSearchHit, count)
	for index := range hits {
		hits[index] = testZwayamHit(index + 1)
	}
	return hits
}

func testZwayamPage(
	results []*zwayamSearchHit,
	total int,
	hasMore bool,
) zwayamSearchResponse {
	pageSize := stringish("10")
	return zwayamSearchResponse{
		Code: zwayamInt(http.StatusOK),
		Data: &zwayamSearchData{
			Results:     &results,
			TotalCount:  &total,
			HasMoreData: &hasMore,
			FacetedSearchConfig: &zwayamFacetedSearchConfig{
				PaginationHowMuch: &pageSize,
			},
		},
	}
}

func testZwayamDetail(id int64, slug string) zwayamDetailResponse {
	configuration := map[string]json.RawMessage{
		"Description":     json.RawMessage(`"<p>Build &amp; operate studios.</p>"`),
		"Role":            json.RawMessage(`"<ul><li>Ship safely.</li></ul>"`),
		"Skills Required": json.RawMessage(`"Operations"`),
		"Years Of Exp":    json.RawMessage(`"Up to 4 years"`),
		"Location":        json.RawMessage(`"Bengaluru"`),
	}
	return zwayamDetailResponse{
		ID:                   &id,
		CompanyID:            zwayamInt64(15470),
		JobURL:               slug,
		Status:               zwayamInt(1),
		JobTitle:             "Fitness Manager",
		Location:             "Bengaluru",
		CreateDate:           "06-Feb-2026",
		JobConfigurationData: &configuration,
	}
}

func zwayamInt(value int) *int       { return &value }
func zwayamInt64(value int64) *int64 { return &value }

func writeZwayamJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode JSON response: %v", err)
	}
}

func readZwayamMultipart(r *http.Request) (map[string]string, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return values, nil
		}
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		if name == "" || part.FileName() != "" {
			_ = part.Close()
			return nil, fmt.Errorf("unexpected multipart part %q", name)
		}
		if _, duplicate := values[name]; duplicate {
			_ = part.Close()
			return nil, fmt.Errorf("duplicate multipart field %q", name)
		}
		value, err := io.ReadAll(io.LimitReader(part, 1<<20))
		closeErr := part.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		values[name] = string(value)
	}
}

func assertZwayamBrowserHeaders(t *testing.T, r *http.Request, origin string) {
	t.Helper()
	if got := r.Header.Get("Origin"); got != origin {
		t.Errorf("Origin = %q, want %q", got, origin)
	}
	if got := r.Header.Get("Referer"); got != strings.TrimRight(origin, "/")+"/" {
		t.Errorf("Referer = %q", got)
	}
	if got := r.Header.Get("User-Agent"); !strings.Contains(got, "Mozilla/5.0") ||
		!strings.Contains(got, "jobwatch") {
		t.Errorf("User-Agent = %q", got)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
}

func serverURLFromRequest(r *http.Request) string {
	return "http://" + r.Host
}
