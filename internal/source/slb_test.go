package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
)

func TestSLBFactoryDefaultsNilClient(t *testing.T) {
	src, err := New("slb", "SLB", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	implementation := src.(*identifiedSource).Source.(*slb)
	if implementation.client != http.DefaultClient {
		t.Fatalf("nil client = %p, want http.DefaultClient", implementation.client)
	}
}

func TestSLBFetchesCoveoConfigSearchAndDetail(t *testing.T) {
	var listingCalls, searchCalls, detailCalls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/job-listing":
			listingCalls++
			_, _ = fmt.Fprint(w, `<html>
<input type="hidden" id="organizationId" value="slb-org" />
<input type="hidden" id="accessToken" value="public-token" />
<input type="hidden" id="searchHub" value="CoveoJobsHub" />
<input type="hidden" id="searchsource" value="ATS_Jobs_Source - Prod" />
</html>`)
		case "/rest/search/v2":
			searchCalls++
			if r.URL.Query().Get("organizationId") != "slb-org" {
				t.Errorf("organizationId = %q", r.URL.Query().Get("organizationId"))
			}
			if r.Header.Get("Authorization") != "Bearer public-token" {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request["aq"] != `@country=="India"` ||
				request["cq"] != `@source==$"ATS_Jobs_Source - Prod"` {
				t.Errorf("search request = %#v", request)
			}
			writeJSON(t, w, map[string]any{
				"totalCount": 1, "totalCountFiltered": 1,
				"results": []map[string]any{
					{
						"title":    " Software Engineer ",
						"clickUri": server.URL + "/jobdescription.aspx?id=WDR001%20one",
						"raw": map[string]any{
							"permanentid": "stable-hash", "country": []string{"India"},
							"city": "Mysuru", "date": int64(1785384562000),
						},
					},
				},
			})
		case "/jobdescription.aspx":
			detailCalls++
			if r.URL.Query().Get("id") != "WDR001 one" {
				t.Errorf("detail id = %q", r.URL.Query().Get("id"))
			}
			_, _ = fmt.Fprint(w, `<html><section class="job-description-redesign gap-x-4">
<p><strong>Job Characteristics</strong></p><p>Build &amp; ship.</p>
<p><strong>Requirements</strong></p><p>0-2 years.</p>
</section></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src := &slb{
		company: "SLB", baseURL: server.URL, searchURL: server.URL + "/rest/search/v2",
		country: "India", maxPostings: 1000, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listingCalls != 1 || searchCalls != 1 || detailCalls != 0 || len(jobs) != 1 {
		t.Fatalf("calls listing/search/detail=%d/%d/%d jobs=%d",
			listingCalls, searchCalls, detailCalls, len(jobs))
	}
	job := &jobs[0]
	if job.ID != "slb/stable-hash" || job.Title != "Software Engineer" ||
		job.Location != "India, Mysuru" ||
		job.URL != server.URL+"/jobdescription.aspx?id=WDR001%20one" {
		t.Errorf("job = %#v", job)
	}
	if !job.PostedAt.Equal(time.UnixMilli(1785384562000)) {
		t.Errorf("PostedAt = %s", job.PostedAt)
	}
	if err := src.Detail(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 {
		t.Fatalf("detail calls = %d", detailCalls)
	}
	if job.Description != "Job Characteristics\nBuild & ship.\n\nRequirements\n0-2 years." {
		t.Errorf("Description = %q", job.Description)
	}
}

func TestSLBRejectsMissingSearchConfiguration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>no token</html>`)
	}))
	defer server.Close()
	src := &slb{
		baseURL: server.URL, searchURL: server.URL + "/search",
		country: "India", maxPostings: 10, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "omitted Coveo") {
		t.Fatalf("Fetch = %#v, %v", jobs, err)
	}
}

func TestSLBRejectsIncoherentPagination(t *testing.T) {
	tests := []struct {
		name        string
		maxPostings int
		total       int
		results     []map[string]any
		wantErr     string
	}{
		{
			name:        "result beyond zero total",
			maxPostings: 10,
			total:       0,
			results: []map[string]any{
				{
					"title": "Unexpected", "clickUri": "https://careers.slb.com/jobdescription.aspx?id=one",
					"raw": map[string]any{"permanentid": "one"},
				},
			},
			wantErr: "exceeding declared target 0",
		},
		{
			name:        "page exceeds requested limit",
			maxPostings: 1,
			total:       2,
			results: []map[string]any{
				{
					"title": "One", "clickUri": "https://careers.slb.com/jobdescription.aspx?id=one",
					"raw": map[string]any{"permanentid": "one"},
				},
				{
					"title": "Two", "clickUri": "https://careers.slb.com/jobdescription.aspx?id=two",
					"raw": map[string]any{"permanentid": "two"},
				},
			},
			wantErr: "requested at most 1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/job-listing" {
					fmt.Fprint(w, `<input id="organizationId" value="org"><input id="accessToken" value="token">`+
						`<input id="searchHub" value="hub"><input id="searchsource" value="source">`)
					return
				}
				writeJSON(t, w, map[string]any{
					"totalCount": test.total, "totalCountFiltered": test.total,
					"results": test.results,
				})
			}))
			defer server.Close()
			src := &slb{
				baseURL: server.URL, searchURL: server.URL + "/search",
				country: "India", maxPostings: test.maxPostings, client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch = %#v, %v; want error containing %q", jobs, err, test.wantErr)
			}
		})
	}
}

func TestSLBRejectsResultsWithoutExplicitIndiaCountry(t *testing.T) {
	tests := []struct {
		name      string
		countries []string
	}{
		{name: "missing"},
		{name: "foreign", countries: []string{"United States"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/job-listing" {
					_, _ = fmt.Fprint(
						w,
						`<input id="organizationId" value="org">`+
							`<input id="accessToken" value="token">`+
							`<input id="searchHub" value="hub">`+
							`<input id="searchsource" value="source">`,
					)
					return
				}
				writeJSON(t, w, map[string]any{
					"totalCount": 1, "totalCountFiltered": 1,
					"results": []map[string]any{{
						"title":    "Role",
						"clickUri": server.URL + "/jobdescription.aspx?id=one",
						"raw": map[string]any{
							"permanentid": "one", "country": test.countries,
						},
					}},
				})
			}))
			defer server.Close()
			src := &slb{
				baseURL: server.URL, searchURL: server.URL + "/search",
				country: "India", maxPostings: 10, client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), `does not include country "India"`) {
				t.Fatalf("Fetch = %#v, %v", jobs, err)
			}
		})
	}
}

func TestSLBDetailRejectsUntrustedJobsBeforeRequest(t *testing.T) {
	src := &slb{baseURL: "https://careers.slb.com", client: http.DefaultClient}
	tests := []struct {
		name string
		job  *model.Job
	}{
		{name: "nil job"},
		{
			name: "foreign id",
			job: &model.Job{
				ID: "other/id", URL: "https://careers.slb.com/jobdescription.aspx?id=one",
			},
		},
		{
			name: "foreign URL",
			job:  &model.Job{ID: "slb/one", URL: "https://example.com/jobdescription.aspx?id=one"},
		},
		{
			name: "wrong path",
			job:  &model.Job{ID: "slb/one", URL: "https://careers.slb.com/jobdescription-evil.aspx?id=one"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := src.Detail(context.Background(), test.job); err == nil {
				t.Fatal("Detail unexpectedly succeeded")
			}
		})
	}
}

func TestSLBDetailRejectsRedirectBeforeDestinationRequest(t *testing.T) {
	var destinationRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jobdescription.aspx" {
			http.Redirect(w, r, "/other", http.StatusFound)
			return
		}
		destinationRequests++
		http.Error(w, "unexpected redirect destination", http.StatusInternalServerError)
	}))
	defer server.Close()

	src := &slb{baseURL: server.URL, client: server.Client()}
	job := model.Job{ID: "slb/stable-id", URL: server.URL + "/jobdescription.aspx?id=one"}
	before := job
	err := src.Detail(context.Background(), &job)
	if err == nil || !strings.Contains(err.Error(), "302 Found") {
		t.Fatalf("Detail error = %v, want redirect status", err)
	}
	if destinationRequests != 0 {
		t.Fatalf("redirect destination requests = %d, want 0", destinationRequests)
	}
	if job != before {
		t.Fatalf("job mutated on redirect: %#v", job)
	}
}
