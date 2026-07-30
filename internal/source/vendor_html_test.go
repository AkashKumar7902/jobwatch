package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestOracleCEFetchAndLazyDetail(t *testing.T) {
	var detailRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hcmRestApi/resources/latest/recruitingCEJobRequisitions":
			if r.URL.Query().Get("onlyData") != "true" || r.URL.Query().Get("expand") != "requisitionList" {
				t.Errorf("unexpected list query: %s", r.URL.RawQuery)
			}
			finder := r.URL.Query().Get("finder")
			offset := 0
			postings := `[{"Id":101,"Title":"Platform Engineer","PostedDate":"2026-07-30","PrimaryLocation":"Bengaluru, IN","WorkplaceType":"Hybrid"},{"Id":"102","Title":"Data Engineer","PostedDate":"2026-07-29","PrimaryLocationCountry":"India"}]`
			if strings.Contains(finder, "offset=2") {
				offset = 2
				postings = `[{"Id":"103","Title":"Security Engineer","PostedDate":"2026-07-28","PrimaryLocation":"Pune, IN"}]`
			} else if !strings.Contains(finder, "siteNumber=CX,limit=100,offset=0") {
				t.Errorf("unexpected finder: %q", finder)
			}
			fmt.Fprintf(w, `{"items":[{"TotalJobsCount":3,"Offset":%d,"Limit":100,"SiteNumber":"CX","requisitionList":%s}],"count":1,"hasMore":%t,"limit":25,"offset":0}`,
				offset, postings, offset == 0)
		case "/hcmRestApi/resources/latest/recruitingCEJobRequisitionDetails":
			detailRequests.Add(1)
			if got := r.URL.Query().Get("finder"); got != "ById;Id=101,siteNumber=CX" {
				t.Errorf("unexpected detail finder: %q", got)
			}
			fmt.Fprint(w, `{"items":[{"Id":"101","Title":"Platform Engineer","ExternalPostedStartDate":"2026-07-30T09:30:00Z","ExternalDescriptionStr":"<p>Build reliable systems.</p>","ExternalResponsibilitiesStr":"<ul><li>Own services</li></ul>","PrimaryLocation":"Bengaluru, IN","JobType":"Regular"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src := &oracleCE{
		company: "Acme", host: "jobs.example.com", site: "CX", base: server.URL,
		maxPages: 2, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if detailRequests.Load() != 0 {
		t.Fatal("Fetch eagerly requested Oracle details")
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs, want 3", len(jobs))
	}
	if jobs[0].ID != "oraclece/jobs.example.com/CX/101" ||
		jobs[0].URL != server.URL+"/hcmUI/CandidateExperience/en/sites/CX/job/101" ||
		jobs[0].PostedAt.Format("2006-01-02") != "2026-07-30" {
		t.Fatalf("unexpected first job: %+v", jobs[0])
	}
	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if detailRequests.Load() != 1 || jobs[0].Description != "Build reliable systems.\n\nOwn services" ||
		jobs[0].EmploymentType != "Regular" {
		t.Fatalf("unexpected detailed job: %+v", jobs[0])
	}
}

func TestOracleCEPaginationCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"items":[{"TotalJobsCount":2,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"1","Title":"One"}]}]}`)
	}))
	defer server.Close()
	src := &oracleCE{company: "Acme", host: "jobs.example.com", site: "CX", base: server.URL, maxPages: 1, client: server.Client()}
	if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "max_pages") {
		t.Fatalf("expected pagination-cap error, got %v", err)
	}
}

func TestSuccessFactorsFetchAndMicrodataDetail(t *testing.T) {
	var detailRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/" {
			if r.URL.Query().Get("sortColumn") != "referencedate" || r.URL.Query().Get("sortDirection") != "desc" {
				t.Errorf("unexpected sort query: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("startrow") == "0" {
				fmt.Fprint(w, `<table aria-label="Search results for . Page 1 of 2, Results 1 to 2 of 3">
<tr class="data-row"><td><a class="jobTitle-link" href="/job/Platform-Engineer/101/">Platform Engineer</a><span class="jobLocation">Pune, IN</span><span class="jobDate">Jul 30, 2026</span></td></tr>
<tr class="data-row"><td><a class="jobTitle-link" href="/job/Data-Engineer/102/">Data Engineer</a><span class="jobLocation">Remote</span></td></tr></table>`)
			} else if r.URL.Query().Get("startrow") == "2" {
				fmt.Fprint(w, `<table aria-label="Search results for . Page 2 of 2, Results 3 to 3 of 3">
<tr class="data-row"><td><a class="jobTitle-link" href="/job/Security-Engineer/103/">Security Engineer</a><span class="jobLocation">Delhi, IN</span></td></tr></table>`)
			} else {
				t.Errorf("unexpected startrow: %q", r.URL.Query().Get("startrow"))
			}
			return
		}
		if r.URL.Path == "/job/Platform-Engineer/101/" {
			detailRequests.Add(1)
			fmt.Fprint(w, `<div itemscope itemtype="https://schema.org/JobPosting">
<span itemprop="title">Platform Engineer</span>
<meta itemprop="streetAddress" content="Pune, IN">
<meta itemprop="addressLocality" content="Pune">
<meta itemprop="datePosted" content="Thu Jul 30 02:00:00 UTC 2026">
<meta itemprop="employmentType" content="Full-time">
<div class="jobdescription"><p>Build <strong>distributed systems</strong>.</p></div></div>`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	src := &successFactors{
		company: "Acme", host: "jobs.example.com", base: server.URL,
		maxPages: 2, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || jobs[0].ID != "successfactors/jobs.example.com/101" ||
		jobs[0].PostedAt.Format("2006-01-02") != "2026-07-30" {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
	if detailRequests.Load() != 0 {
		t.Fatal("Fetch eagerly requested SuccessFactors details")
	}
	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if jobs[0].Description != "Build distributed systems." || jobs[0].Location != "Pune, IN" ||
		jobs[0].EmploymentType != "Full-time" || detailRequests.Load() != 1 {
		t.Fatalf("unexpected detailed job: %+v", jobs[0])
	}
}

func TestICIMSFetchAndJSONLDDetail(t *testing.T) {
	var detailRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jobs/search" {
			if r.URL.Query().Get("in_iframe") != "1" {
				t.Errorf("missing in_iframe=1: %s", r.URL.RawQuery)
			}
			page := r.URL.Query().Get("pr")
			if page == "0" {
				fmt.Fprint(w, `<h1>Search Results Page 1 of 2</h1><ul>
<li class="iCIMS_JobCardItem"><a href="/jobs/3067/principal-engineer/job?in_iframe=1" class="iCIMS_Anchor" title="3067 - Principal Engineer"><h3>Principal Engineer</h3></a><span class="sr-only field-label">Job Locations</span><span>US-CA-Carlsbad</span></li></ul>`)
			} else if page == "1" {
				fmt.Fprint(w, `<h1>Search Results Page 2 of 2</h1><ul>
<li class="iCIMS_JobCardItem"><a href="/jobs/3068/data-engineer/job?in_iframe=1" class="iCIMS_Anchor"><h3>Data Engineer</h3></a><span class="sr-only field-label">Location : Location</span><span>IN-KA-Bengaluru</span></li></ul>`)
			} else {
				t.Errorf("unexpected pr: %q", page)
			}
			return
		}
		if r.URL.Path == "/jobs/3067/principal-engineer/job" {
			detailRequests.Add(1)
			if r.URL.Query().Get("in_iframe") != "1" {
				t.Errorf("detail missing in_iframe=1")
			}
			fmt.Fprint(w, `<script type="application/ld+json">{"@context":"https://schema.org","@graph":[{"@type":["Thing","JobPosting"],"title":"Principal Engineer","description":"<p>Design &amp; build products.</p>","employmentType":["FULL_TIME","Regular"],"datePosted":"2026-07-30T08:00:00Z","jobLocation":{"address":{"addressLocality":"Carlsbad","addressRegion":"CA","addressCountry":"US"}}}]}</script>`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	src := &icims{
		company: "Acme", host: "careers.example.com", base: server.URL,
		maxPages: 2, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != "icims/careers.example.com/3067" ||
		jobs[0].URL != server.URL+"/jobs/3067/principal-engineer/job" ||
		jobs[0].Location != "US-CA-Carlsbad" {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
	if detailRequests.Load() != 0 {
		t.Fatal("Fetch eagerly requested iCIMS details")
	}
	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if jobs[0].Description != "Design & build products." ||
		jobs[0].EmploymentType != "FULL_TIME, Regular" ||
		jobs[0].Location != "Carlsbad, CA, US" ||
		!jobs[0].PostedAt.Equal(time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected detailed job: %+v", jobs[0])
	}
}

func TestEnphaseFetchHeadersAndNormalization(t *testing.T) {
	var requests atomic.Int32
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Origin") != serverURL ||
			r.Header.Get("Referer") != serverURL+"/careers" ||
			!strings.Contains(r.Header.Get("User-Agent"), "Chrome/138") {
			t.Errorf("unexpected Enphase headers: origin=%q referer=%q user-agent=%q", r.Header.Get("Origin"), r.Header.Get("Referer"), r.Header.Get("User-Agent"))
		}
		page := r.URL.Query().Get("page")
		jid, name := "oOne", "Power Engineer"
		if page == "1" {
			jid, name = "oTwo", "Firmware Engineer"
		}
		response := map[string]any{
			"rows": []map[string]string{{
				"jid": jid, "name": name, "category": "Engineering",
				"applyUrl":           "http://app.jobvite.com/CompanyJobs/Careers.aspx?c=abc&amp;j=" + jid + "&amp;k=Apply",
				"description__value": "&amp;lt;p&amp;gt;Build clean energy.&amp;lt;/p&amp;gt;",
				"location":           "Bengaluru, IN",
			}},
			"pager": map[string]any{
				"current_page": page, "total_items": "2", "total_pages": 2, "items_per_page": "1",
			},
		}
		if page == "0" {
			response["pager"].(map[string]any)["current_page"] = 0
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	src := &enphase{company: "Enphase", base: server.URL, maxPages: 2, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || len(jobs) != 2 {
		t.Fatalf("got %d requests and %d jobs", requests.Load(), len(jobs))
	}
	if jobs[0].ID != "enphase/oOne" || jobs[0].Description != "Build clean energy." ||
		!strings.HasPrefix(jobs[0].URL, "https://app.jobvite.com/") ||
		jobs[0].EmploymentType != "" {
		t.Fatalf("unexpected first job: %+v", jobs[0])
	}
}

func TestEnphaseRejectsInconsistentTotal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"rows":[{"jid":"only","name":"Only","category":"Engineering","applyUrl":"https://app.jobvite.com/x?j=only","description__value":"<p>Text</p>"}],"pager":{"current_page":0,"total_items":"2","total_pages":1,"items_per_page":2}}`)
	}))
	defer server.Close()
	src := &enphase{company: "Enphase", base: server.URL, maxPages: 2, client: server.Client()}
	if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "reported 2") {
		t.Fatalf("expected inconsistent-total error, got %v", err)
	}
}

func TestRichpanelFetchAndVisibleDetail(t *testing.T) {
	var detailRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/careers":
			fmt.Fprint(w, `<h2>Current Openings</h2>
<a href="/careers/lead-backend-engineer" class="job-wrap w-inline-block">
<div class="job-txt-title">Lead Backend Engineer</div>
<div class="job-txt-depart">Location</div><div class="job-txt-depart-head">Bangalore, IN</div>
<div class="job-txt-depart">Department</div><div class="job-txt-depart-head">Engineering</div>
<div class="job-txt-depart">Type</div><div class="job-txt-depart-head">Full-time</div></a>`)
		case "/careers/lead-backend-engineer":
			detailRequests.Add(1)
			fmt.Fprint(w, `<div class="job-txt-title">Lead Backend Engineer</div>
<div class="job-txt-depart">Location</div><div class="job-txt-depart-head">Bangalore, IN</div>
<div class="job-rte w-richtext"><p>Build the platform.</p><div><p>Coach engineers.</p></div></div>
<div class="job-rte hide-static w-richtext"><p>Lorem ipsum dummy copy.</p></div>
<div class="job-rte benefits w-richtext"><p>Benefits copy.</p></div>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src := &richpanel{company: "Richpanel", base: server.URL, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "richpanel/lead-backend-engineer" ||
		jobs[0].Location != "Bangalore, IN" || jobs[0].EmploymentType != "Full-time" ||
		detailRequests.Load() != 0 {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if jobs[0].Description != "Build the platform.\nCoach engineers." ||
		strings.Contains(jobs[0].Description, "Lorem") || detailRequests.Load() != 1 {
		t.Fatalf("unexpected detailed job: %+v", jobs[0])
	}
}

func TestRichpanelEmptyBoardRequiresMarker(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "valid empty", body: `<h2>Current Openings</h2>`},
		{name: "changed markup", body: `<h2>Careers</h2>`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			src := &richpanel{company: "Richpanel", base: server.URL, client: server.Client()}
			jobs, err := src.Fetch(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("got jobs=%v err=%v", jobs, err)
			}
		})
	}
}

func TestVendorFactoriesValidateConnectionParams(t *testing.T) {
	client := &http.Client{}
	valid := []struct {
		name   string
		params params.Map
	}{
		{name: "oraclece", params: params.Map{"host": "eeho.fa.us2.oraclecloud.com", "site": "CX_45001"}},
		{name: "successfactors", params: params.Map{"host": "jobs.sap.com"}},
		{name: "icims", params: params.Map{"host": "careers-example.icims.com"}},
		{name: "enphase", params: params.Map{}},
		{name: "richpanel", params: params.Map{}},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			source, err := New(test.name, "Acme", test.params, client)
			if err != nil {
				t.Fatal(err)
			}
			if source.Company() != "Acme" {
				t.Fatalf("company = %q", source.Company())
			}
		})
	}
	for _, name := range []string{"oraclece", "successfactors", "icims"} {
		t.Run(name+" invalid host", func(t *testing.T) {
			p := params.Map{"host": "https://example.com/path"}
			if name == "oraclece" {
				p["site"] = "CX"
			}
			if _, err := New(name, "Acme", p, client); err == nil {
				t.Fatal("expected invalid-host error")
			}
		})
	}
	if _, err := New("icims", "Acme", params.Map{"host": "jobs.example.com", "max_pages": "501"}, client); err == nil {
		t.Fatal("expected max_pages cap error")
	}
}

func TestDetailValidationIsAtomic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<script type="application/ld+json">{"@type":"JobPosting","title":"Changed","description":""}</script>`)
	}))
	defer server.Close()
	src := &icims{company: "Acme", host: "jobs.example.com", base: server.URL, maxPages: 1, client: server.Client()}
	original := model.Job{
		ID: "icims/jobs.example.com/42", Company: "Acme", Title: "Original",
		Location: "Original Location", Description: "Original Description",
		URL: server.URL + "/jobs/42/example/job",
	}
	job := original
	if err := src.Detail(context.Background(), &job); err == nil {
		t.Fatal("expected missing-description error")
	}
	if job != original {
		t.Fatalf("detail mutated job on failure:\n got %+v\nwant %+v", job, original)
	}
}

func TestResolveBoardURLRejectsCrossHost(t *testing.T) {
	if _, err := resolveBoardURL("https://jobs.example.com", "https://evil.example/job/1"); err == nil {
		t.Fatal("expected cross-host URL error")
	}
}
