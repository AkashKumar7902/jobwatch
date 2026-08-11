package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

type customARoundTripper func(*http.Request) (*http.Response, error)

func (f customARoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func requireSingleCap(t *testing.T, collector *diagnostic.Collector) {
	t.Helper()
	if got := collector.Snapshot().Caps; got != 1 {
		t.Fatalf("cap diagnostics = %d, want 1", got)
	}
}

func TestCustomASourceNamesAndFactories(t *testing.T) {
	tests := []struct {
		name   string
		params params.Map
	}{
		{"eightfold", params.Map{"host": "careers.example.com", "domain": "example.com"}},
		{"kula", params.Map{"account_name": "acme"}},
		{"amazon", params.Map{}},
		{"atlassian", params.Map{}},
		{"deshaw", params.Map{}},
		{"avature", params.Map{"host": "jobs.example.com", "site": "en_US/careers", "search_path": "SearchJobs"}},
		{"medianet", params.Map{}},
	}
	available := strings.Join(Names(), ",")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(","+available+",", ","+test.name+",") {
				t.Fatalf("%q is not registered; Names=%v", test.name, Names())
			}
			if _, err := New(test.name, "Acme", test.params, &http.Client{}); err != nil {
				t.Fatalf("factory failed: %v", err)
			}
		})
	}
}

func TestEightfoldPaginationAndLazyDetail(t *testing.T) {
	const firstID int64 = 9000000000000001
	listCalls, detailCalls := 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pcsx/search":
			listCalls++
			if r.URL.Query().Get("domain") != "acme.com" || r.URL.Query().Get("location") != "India" ||
				r.URL.Query().Get("query") != "Cradlepoint" {
				http.Error(w, "missing board query", http.StatusBadRequest)
				return
			}
			start, _ := strconv.Atoi(r.URL.Query().Get("start"))
			end := min(start+eightfoldPageSize, 12)
			positions := make([]map[string]any, 0, max(0, end-start))
			for i := start; i < end; i++ {
				id := firstID + int64(i)
				positions = append(positions, map[string]any{
					"id": id, "displayJobId": fmt.Sprintf("REQ-%d", i), "atsJobId": fmt.Sprintf("REQ-%d", i),
					"name": fmt.Sprintf("Engineer %d", i), "locations": []string{"India"},
					"postedTs": 1_785_237_164, "positionUrl": fmt.Sprintf("/careers/job/%d", id),
				})
			}
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200, "error": map[string]string{"message": "", "body": ""},
				"data": map[string]any{"count": 12, "positions": positions},
			})
		case "/api/pcsx/position_details":
			detailCalls++
			if r.URL.Query().Get("position_id") != strconv.FormatInt(firstID, 10) {
				http.Error(w, "wrong detail id", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200,
				"data": map[string]any{
					"id": firstID, "name": "Engineer 0", "locations": []string{"Bengaluru, India"},
					"postedTs": 1_785_237_164, "jobDescription": "<p>Build reliable systems. 3+ years.</p>",
					"publicUrl":                  server.URL + "/careers/job/" + strconv.FormatInt(firstID, 10),
					"efcustomTextEmploymentType": []string{"Full-Time"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src := &eightfold{
		company: "Acme", host: "test.eightfold", domain: "acme.com", location: "India", query: "Cradlepoint",
		base: server.URL, keyPrefix: "eightfold/acme.com/", maxPostings: 100, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 12 || listCalls != 2 || detailCalls != 0 {
		t.Fatalf("Fetch jobs=%d listCalls=%d detailCalls=%d", len(jobs), listCalls, detailCalls)
	}
	// The serving host is transport and must not appear in the key.
	if got, want := jobs[0].ID, "eightfold/acme.com/"+strconv.FormatInt(firstID, 10); got != want {
		t.Fatalf("ID=%q want %q", got, want)
	}
	if jobs[0].Description != "" {
		t.Fatal("list fetch eagerly populated description")
	}
	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 || !strings.Contains(jobs[0].Description, "Build reliable systems") ||
		jobs[0].EmploymentType != "Full-Time" || jobs[0].Location != "Bengaluru, India" {
		t.Fatalf("detail did not normalize job: %+v, calls=%d", jobs[0], detailCalls)
	}
	src.maxPostings = 10
	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err = src.Fetch(ctx)
	if err != nil || len(jobs) != 10 {
		t.Fatalf("capped Fetch = %d jobs, %v", len(jobs), err)
	}
	requireSingleCap(t, collector)
}

func TestKulaPaginationAndNormalization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("accountName") != "acme" || r.URL.Query().Get("type") != "ats_job_post.index" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := (page - 1) * kulaPageSize
		end := min(start+kulaPageSize, 100)
		data := make([]map[string]any, 0, max(0, end-start))
		for i := start; i < end; i++ {
			data = append(data, map[string]any{
				"id": i + 1, "title": fmt.Sprintf("Kula Role %d", i+1), "listed": true,
				"kind": "external", "is_confidential": false,
				"ats_job": map[string]any{
					"job_description": "<p>Own the role end to end.</p>", "workplace": "office",
					"employment_type": "full_time",
					"offices": []map[string]any{{
						"id": 1, "location": "Bengaluru, Karnataka, India", "country": "India",
					}},
				},
			})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": data, "errors": []any{},
			"meta": map[string]any{"count": 100, "page": page, "items": kulaPageSize, "pages": 2},
		})
	}))
	defer server.Close()
	src := &kula{
		company: "Acme", account: "acme", apiBase: server.URL,
		boardBase: server.URL + "/acme", maxPostings: 200, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 100 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	if jobs[0].ID != "kula/acme/1" || jobs[0].EmploymentType != "full_time" ||
		!strings.Contains(jobs[0].Description, "Own the role") ||
		jobs[0].URL != server.URL+"/acme/1" {
		t.Fatalf("unexpected first job: %+v", jobs[0])
	}
	src.maxPostings = 99
	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err = src.Fetch(ctx)
	if err != nil || len(jobs) != 99 {
		t.Fatalf("capped Fetch = %d jobs, %v", len(jobs), err)
	}
	requireSingleCap(t, collector)
}

func TestAmazonCountryPaginationAndFullDescription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("normalized_country_code[]") != "IND" {
			http.Error(w, "wrong country", http.StatusBadRequest)
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("result_limit"))
		end := min(offset+limit, 101)
		postings := make([]map[string]any, 0, max(0, end-offset))
		for i := offset; i < end; i++ {
			id := strconv.Itoa(1000 + i)
			postings = append(postings, map[string]any{
				"id": fmt.Sprintf("uuid-%d", i), "id_icims": id, "title": fmt.Sprintf("Amazon Role %d", i),
				"country_code": "IND", "normalized_location": "Bengaluru, Karnataka, IND",
				"job_path": "/en/jobs/" + id + "/amazon-role", "job_schedule_type": "full-time",
				"description": "<p>Build products.</p>", "basic_qualifications": "<p>Three years.</p>",
				"preferred_qualifications": "<p>Distributed systems.</p>", "posted_date": "July 30, 2026",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": 101, "jobs": postings})
	}))
	defer server.Close()
	src := &amazon{
		company: "Amazon", country: "IND", searchURL: server.URL + "/search.json",
		siteBase: server.URL, maxPostings: 500, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 101 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	if jobs[0].ID != "amazon/IND/1000" || !strings.Contains(jobs[0].Description, "Distributed systems") ||
		jobs[0].PostedAt.IsZero() || jobs[0].URL != server.URL+"/en/jobs/1000/amazon-role" {
		t.Fatalf("unexpected first job: %+v", jobs[0])
	}
	src.maxPostings = 100
	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err = src.Fetch(ctx)
	if err != nil || len(jobs) != 100 {
		t.Fatalf("capped Fetch = %d jobs, %v", len(jobs), err)
	}
	requireSingleCap(t, collector)
}

func TestAtlassianBulkNormalization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{
			"id":25020,"portalId":242,"title":"Account Executive","locations":["Bengaluru - India","Remote"],
			"category":"Sales","overview":"<p>Working at Atlassian</p>",
			"responsibilities":"<ul><li>Own accounts</li></ul>",
			"qualifications":"<p>5+ years</p>","compensation":"<p>Equity eligible</p>",
			"applyUrl":"https://apply.example/25020",
			"portalJobPost":{"portalId":242,"portalUrl":"https://portal.example/25020","id":25020,"updatedDate":"2026-06-26"}
		}]`))
	}))
	defer server.Close()
	src := &atlassian{
		company: "Atlassian", endpoint: server.URL, detailBase: server.URL + "/details",
		maxPostings: 100, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "atlassian/25020" ||
		jobs[0].URL != server.URL+"/details/25020" ||
		!strings.Contains(jobs[0].Description, "Own accounts") {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestAtlassianDuplicateReconciliation(t *testing.T) {
	t.Run("identical triple", func(t *testing.T) {
		jobs, err := fetchAtlassianFixture(t, []map[string]any{
			atlassianFixturePosting(1, "One"),
			atlassianFixturePosting(1, "One"),
			atlassianFixturePosting(1, "One"),
		}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].ID != "atlassian/1" {
			t.Fatalf("jobs = %+v", jobs)
		}
	})

	t.Run("unused field conflict", func(t *testing.T) {
		first := atlassianFixturePosting(1, "One")
		second := atlassianFixturePosting(1, "One")
		second["category"] = "Different unused category"
		_, err := fetchAtlassianFixture(t, []map[string]any{first, second}, 10)
		if err == nil || !strings.Contains(err.Error(), "conflicting duplicate posting id 1") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid duplicate is validated", func(t *testing.T) {
		first := atlassianFixturePosting(1, "One")
		second := atlassianFixturePosting(1, "One")
		second["overview"] = ""
		_, err := fetchAtlassianFixture(t, []map[string]any{first, second}, 10)
		if err == nil || !strings.Contains(err.Error(), "has no description fields") {
			t.Fatalf("error = %v", err)
		}
	})

	for _, test := range []struct {
		name string
		cap  int
		want string
	}{
		{name: "duplicate consumes raw cap", cap: 2, want: "atlassian/1"},
		{name: "third raw row admitted", cap: 3, want: "atlassian/1,atlassian/2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			jobs, err := fetchAtlassianFixture(t, []map[string]any{
				atlassianFixturePosting(1, "One"),
				atlassianFixturePosting(1, "One"),
				atlassianFixturePosting(2, "Two"),
			}, test.cap)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, len(jobs))
			for index := range jobs {
				ids[index] = jobs[index].ID
			}
			if got := strings.Join(ids, ","); got != test.want {
				t.Fatalf("IDs = %q, want %q", got, test.want)
			}
		})
	}

	t.Run("cap emits diagnostic", func(t *testing.T) {
		ctx, collector := diagnostic.WithCollector(context.Background())
		jobs, err := fetchAtlassianFixtureContext(t, ctx, []map[string]any{
			atlassianFixturePosting(1, "One"),
			atlassianFixturePosting(2, "Two"),
		}, 1)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("capped Fetch = %d jobs, %v", len(jobs), err)
		}
		requireSingleCap(t, collector)
	})
}

func atlassianFixturePosting(id int64, title string) map[string]any {
	return map[string]any{
		"id": id, "portalId": 242, "title": title, "locations": []string{"Remote"},
		"category": "Engineering", "overview": "<p>Description</p>",
		"responsibilities": "", "qualifications": "", "compensation": "",
		"applyUrl": "https://apply.example/job",
		"portalJobPost": map[string]any{
			"portalId": 242, "portalUrl": "https://portal.example/job", "id": id, "updatedDate": "2026-08-10",
		},
	}
}

func fetchAtlassianFixture(t *testing.T, postings []map[string]any, cap int) ([]model.Job, error) {
	return fetchAtlassianFixtureContext(t, context.Background(), postings, cap)
}

func fetchAtlassianFixtureContext(t *testing.T, ctx context.Context, postings []map[string]any, cap int) ([]model.Job, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(postings); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	src := &atlassian{
		company: "Atlassian", endpoint: server.URL, detailBase: server.URL + "/details",
		maxPostings: cap, client: server.Client(),
	}
	return src.Fetch(ctx)
}

func TestDEShawNextDataRegularAndInternships(t *testing.T) {
	payload := `{
		"props":{"pageProps":{"jobsFetchingError":false,
			"regularJobs":[{
				"id":5874,"displayName":"Administrative Associate","office":[{"name":"New York","abbreviation":"NYC"}],
				"data":{"id":5874,"displayName":"Administrative Associate","validFromDate":"2026-04-15",
					"activeOnJobsListing":true,"jobUrl":"Administrative-Associate-5874",
					"jobDescription":{"websiteDescription":"<p>Support the team.</p>","responsibilitiesHtml":"<ul><li>Plan events.</li></ul>","peopleWeAreLookingFor":["Detail oriented."]},
					"jobMetadata":{"activeOnWebsite":true,"workStatus":"Limited Term"}}}],
			"internships":[{
				"id":5709,"displayName":"Research Analyst Intern","office":[{"name":"New York","abbreviation":"NYC"}],
				"data":{"id":5709,"displayName":"Research Analyst Intern","validFromDate":"2026-01-01",
					"activeOnJobsListing":true,"isActive":true,"jobUrl":"Research-Analyst-Intern-5709",
					"jobDescription":{"websiteDescription":"<p>Research companies.</p>","responsibilitiesHtml":"<p>Analyze data.</p>","peopleWeAreLookingFor":["Critical thinkers."]},
					"jobMetadata":{"activeOnWebsite":true,"workStatus":"Intern"}}}]
		}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `<html><script id="__NEXT_DATA__" type="application/json">%s</script></html>`, payload)
	}))
	defer server.Close()
	src := &deshaw{
		company: "D. E. Shaw", careersURL: server.URL + "/careers",
		siteBase: server.URL, maxPostings: 100, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != "deshaw/5874" || jobs[1].ID != "deshaw/5709" ||
		jobs[1].EmploymentType != "Intern" ||
		jobs[0].URL != server.URL+"/careers/administrative-associate-5874" ||
		jobs[1].URL != server.URL+"/careers/research-analyst-intern-5709" ||
		!strings.Contains(jobs[0].Description, "Detail oriented") {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
	src.maxPostings = 1
	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err = src.Fetch(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("capped Fetch = %d jobs, %v", len(jobs), err)
	}
	requireSingleCap(t, collector)
}

func TestDEShawRequiresCurrentListingActivityAndRejectsLegacyConflict(t *testing.T) {
	postingJSON := func(activity string) string {
		return fmt.Sprintf(`{
			"id":5874,"displayName":"Administrative Associate","office":[{"name":"New York"}],
			"data":{"id":5874,"displayName":"Administrative Associate","validFromDate":"2026-04-15",
				%s,"jobUrl":"Administrative-Associate-5874",
				"jobDescription":{"websiteDescription":"Support the team."},
				"jobMetadata":{"workStatus":"Limited Term"}}}`, activity)
	}
	for _, test := range []struct {
		name     string
		activity string
		wantErr  string
	}{
		{"current field only", `"activeOnJobsListing":true`, ""},
		{"consistent legacy field", `"activeOnJobsListing":true,"isActive":true`, ""},
		{"current field missing", `"isActive":true`, "missing activeOnJobsListing"},
		{"current field false", `"activeOnJobsListing":false`, "missing activeOnJobsListing"},
		{"legacy field conflicts", `"activeOnJobsListing":true,"isActive":false`, "conflicting legacy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var posting deshawPosting
			if err := json.Unmarshal([]byte(postingJSON(test.activity)), &posting); err != nil {
				t.Fatal(err)
			}
			_, err := (&deshaw{company: "D. E. Shaw", siteBase: "https://www.deshaw.com"}).normalize(posting)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("normalize error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestAvatureSSRPaginationAndLazyDetail(t *testing.T) {
	listCalls, detailCalls := 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/SearchJobs") {
			listCalls++
			offset, _ := strconv.Atoi(r.URL.Query().Get("jobOffset"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("jobRecordsPerPage"))
			end := min(offset+limit, 21)
			fmt.Fprint(w, "<html><div>Showing jobs of 21</div>")
			for i := offset; i < end; i++ {
				id := strconv.Itoa(200000 + i)
				fmt.Fprintf(w, `<a class="link link_result" href="/en_US/careers/JobDetail/Role-%[1]s/%[1]s">Role %[1]s</a>
					<span class="list-item-location">Orlando, USA</span>
					<span class="list-item-id">Role ID %[1]s</span>
					<span class="list-item-workerType">Regular Employee</span>`, id)
			}
			fmt.Fprint(w, "</html>")
			return
		}
		if strings.Contains(r.URL.Path, "/JobDetail/") {
			detailCalls++
			id := pathLast(r.URL.Path)
			fmt.Fprintf(w, `<html><head><link rel="canonical" href="%s%s"></head><body>
				<article><div class="article__content__view__field__label">Role ID</div>
				<div class="article__content__view__field__value">%s</div>
				<div><strong>Locations</strong>: Austin, USA <br></div>
				<div class="article__content__view__field__label">Worker Type</div>
				<div class="article__content__view__field__value">Contract</div></article>
				<article><h3>Description &amp; Requirements</h3><div>Build <b>great systems</b>. 4+ years.</div></article>
				</body></html>`, server.URL, r.URL.Path, id)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	src := &avature{
		company: "EA", host: host, site: "en_US/careers",
		searchURL: server.URL + "/en_US/careers/SearchJobs/", maxPostings: 100, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 21 || listCalls != 2 || detailCalls != 0 {
		t.Fatalf("Fetch jobs=%d listCalls=%d detailCalls=%d", len(jobs), listCalls, detailCalls)
	}
	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 || !strings.Contains(jobs[0].Description, "great systems") ||
		jobs[0].Location != "Austin, USA" || jobs[0].EmploymentType != "Contract" {
		t.Fatalf("unexpected detailed job: %+v calls=%d", jobs[0], detailCalls)
	}
	src.maxPostings = 20
	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err = src.Fetch(ctx)
	if err != nil || len(jobs) != 20 {
		t.Fatalf("capped Fetch = %d jobs, %v", len(jobs), err)
	}
	requireSingleCap(t, collector)
}

func TestSmartRecruitersCapEmitsDiagnostic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		postings := make([]map[string]any, 0, 2)
		for i := 1; i <= 2; i++ {
			postings = append(postings, map[string]any{
				"id": strconv.Itoa(i), "name": fmt.Sprintf("Role %d", i),
				"releasedDate": "2026-08-11T00:00:00Z",
				"location":     map[string]any{"city": "Pune", "country": "in", "remote": false},
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"totalFound": 2, "content": postings})
	}))
	defer server.Close()
	client := &http.Client{Transport: customARoundTripper(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return server.Client().Transport.RoundTrip(clone)
	})}
	src := &smartRecruiters{company: "Acme", id: "Acme", maxPostings: 1, client: client}
	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err := src.Fetch(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("capped Fetch = %d jobs, %v", len(jobs), err)
	}
	requireSingleCap(t, collector)
}

func TestParseAvatureTotalMarkers(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int
		wantErr string
	}{
		{
			name: "live shaped aria only",
			body: `<div class="list-controls__text__legend" aria-label="337 results">Jobs</div>`,
			want: 337,
		},
		{
			name: "decoded comma aria",
			body: `<span data-x="1" class="other list-controls__text__legend" aria-label="1&#44;234 result">Jobs</span>`,
			want: 1234,
		},
		{
			name: "legacy and aria agree",
			body: `<div>Showing jobs of 337</div><div class="list-controls__text__legend" aria-label="337 results"></div>`,
			want: 337,
		},
		{
			name: "trusted aria overrides legacy candidate",
			body: `<div>Showing jobs of 336</div><div class="list-controls__text__legend" aria-label="337 results"></div>`,
			want: 337,
		},
		{
			name: "trusted aria ignores unrelated class year",
			body: `<p>Class of 2027</p><div class="list-controls__text__legend" aria-label="337 results"></div>`,
			want: 337,
		},
		{
			name:    "trusted markers conflict",
			body:    `<div class="list-controls__text__legend" aria-label="336 results"></div><span class="list-controls__text__legend" aria-label="337 results"></span>`,
			wantErr: "conflicting total result counts",
		},
		{
			name:    "legacy fallback markers conflict",
			body:    `<div>Showing jobs of 336</div><p>Class of 2027</p>`,
			wantErr: "conflicting total result counts",
		},
		{
			name:    "trusted aria marker is malformed",
			body:    `<div>Showing jobs of 337</div><div class="list-controls__text__legend" aria-label="about 337 jobs"></div>`,
			wantErr: "invalid total result aria-label",
		},
		{
			name:    "untrusted aria marker is ignored",
			body:    `<p class="list-controls__text__legend" aria-label="337 results"></p><div class="not-list-controls__text__legend" aria-label="337 results"></div>`,
			wantErr: "omitted total result count",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseAvatureTotal([]byte(test.body))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("total = %d, error = %v, want %d", got, err, test.want)
			}
		})
	}
}

func pathLast(value string) string {
	value = strings.TrimRight(value, "/")
	return value[strings.LastIndexByte(value, '/')+1:]
}

func TestMediaNetDepartmentsDetailsAndStablePath(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte(`<a class="flex-btn-link" href="/engineering/">1 Position</a>
				<a class="flex-btn-link" href="/design/">1 Position</a>`))
		case "/engineering/":
			fmt.Fprintf(w, `<ul class="openings-list"><li><a href="%s/engineering/shared-role/">Engineer &amp; Builder</a></li></ul>`, server.URL)
		case "/design/":
			fmt.Fprint(w, `<ul class="openings-list"><li><a href="/design/shared-role/">Product Designer</a></li></ul>`)
		case "/engineering/shared-role/":
			w.Write([]byte(`<h2 id="jobProfile">Engineer &amp; Builder</h2>
				<input type="hidden" name="post_id" value="8420">
				<div class="post-body"><p>Build <strong>ad systems</strong>.</p><p>3+ years.</p></div>
				<div class="social-share-wrapper">Apply</div>`))
		case "/design/shared-role/":
			w.Write([]byte(`<h2 id="jobProfile">Product Designer</h2>
				<input type="hidden" name="post_id" value="8420">
				<div class="post-body"><p>Design ad systems.</p></div>
				<div class="social-share-wrapper">Apply</div>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	src := &mediaNet{
		company: "Media.net", baseURL: server.URL + "/",
		maxPostings: 100, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != "medianet/engineering/shared-role" ||
		jobs[1].ID != "medianet/design/shared-role" ||
		jobs[0].Title != "Engineer & Builder" ||
		!strings.Contains(jobs[0].Description, "ad systems") {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestMediaNetRejectsDuplicateCanonicalPathBeforeDetails(t *testing.T) {
	detailCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<a class="flex-btn-link" href="/engineering/">1 Position</a><a class="flex-btn-link" href="/design/">1 Position</a>`)
		case "/engineering/", "/design/":
			fmt.Fprint(w, `<ul class="openings-list"><li><a href="/roles/shared/">Shared</a></li></ul>`)
		case "/roles/shared/":
			detailCalls++
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	src := &mediaNet{company: "Media.net", baseURL: server.URL + "/", maxPostings: 10, client: server.Client()}
	_, err := src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "duplicate detail path") {
		t.Fatalf("error = %v", err)
	}
	if detailCalls != 0 {
		t.Fatalf("made %d detail requests before rejecting duplicate path", detailCalls)
	}
}

func TestMediaNetSameSiteURLSafety(t *testing.T) {
	src := &mediaNet{baseURL: "https://careers.media.net/"}
	for _, valid := range []string{"/engineering/role/", "https://careers.media.net/design/role"} {
		if _, err := src.sameSiteURL(valid); err != nil {
			t.Errorf("valid URL %q: %v", valid, err)
		}
	}
	for _, unsafe := range []string{
		"http://careers.media.net/engineering/role/",
		"https://CAREERS.media.net/engineering/role/",
		"https://user@careers.media.net/engineering/role/",
		"/engineering/role/?query=1",
		"/engineering/role/?",
		"/engineering/role/#fragment",
		"/engineering/%72ole/",
		"/engineering/../role/",
		"/engineering//role/",
	} {
		if _, err := src.sameSiteURL(unsafe); err == nil {
			t.Errorf("unsafe URL %q was accepted", unsafe)
		}
	}
}

func TestMediaNetRejectsMalformedPostIDMarker(t *testing.T) {
	validTail := `<h2 id="jobProfile">Engineer</h2><div class="post-body"><p>Build.</p></div><div class="social-share-wrapper">Apply</div>`
	for _, test := range []struct {
		name   string
		marker string
	}{
		{name: "missing"},
		{name: "zero", marker: `<input name="post_id" value="0">`},
		{name: "not numeric", marker: `<input name="post_id" value="job">`},
		{name: "conflicting", marker: `<input name="post_id" value="8420"><input name="post_id" value="8421">`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, err := parseMediaNetDetail([]byte(test.marker + validTail)); err == nil {
				t.Fatal("expected malformed post_id error")
			}
		})
	}
}

func TestCustomASchemaChecks(t *testing.T) {
	t.Run("eightfold missing count", func(t *testing.T) {
		server := testJSONServer(t, `{"status":200,"data":{"positions":[]}}`)
		defer server.Close()
		src := &eightfold{company: "x", host: "x", domain: "x.com", base: server.URL, keyPrefix: "eightfold/x.com/", maxPostings: 10, client: server.Client()}
		if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "omitted count or positions") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("kula incoherent pages", func(t *testing.T) {
		server := testJSONServer(t, `{"data":[],"errors":[],"meta":{"count":100,"page":1,"items":99,"pages":1}}`)
		defer server.Close()
		src := &kula{company: "x", account: "x", apiBase: server.URL, boardBase: server.URL, maxPostings: 100, client: server.Client()}
		if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "inconsistent") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("amazon missing jobs", func(t *testing.T) {
		server := testJSONServer(t, `{"hits":1}`)
		defer server.Close()
		src := &amazon{company: "x", country: "IND", searchURL: server.URL, siteBase: server.URL, maxPostings: 100, client: server.Client()}
		if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "omitted hits or jobs") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("atlassian mismatched portal id", func(t *testing.T) {
		server := testJSONServer(t, `[{"id":1,"title":"x","overview":"x","portalJobPost":{"id":2}}]`)
		defer server.Close()
		src := &atlassian{company: "x", endpoint: server.URL, detailBase: server.URL, maxPostings: 10, client: server.Client()}
		if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "portalJobPost id") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("deshaw missing next data", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html></html>")) }))
		defer server.Close()
		src := &deshaw{company: "x", careersURL: server.URL, siteBase: server.URL, maxPostings: 10, client: server.Client()}
		if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "omitted __NEXT_DATA__") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("avature missing total", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html></html>")) }))
		defer server.Close()
		host := strings.TrimPrefix(server.URL, "http://")
		src := &avature{company: "x", host: host, site: "careers", searchURL: server.URL, maxPostings: 10, client: server.Client()}
		if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "omitted total") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("medianet no active departments", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`<a class="flex-btn-link" href="/engineering/">0 Position</a>`))
		}))
		defer server.Close()
		src := &mediaNet{company: "x", baseURL: server.URL + "/", maxPostings: 10, client: server.Client()}
		if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "no active departments") {
			t.Fatalf("got %v", err)
		}
	})
}

func testJSONServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}
