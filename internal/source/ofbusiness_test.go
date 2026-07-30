package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/params"
)

func TestOfBusinessFetchesEveryDeclaredPage(t *testing.T) {
	postings := []map[string]any{
		ofBusinessFixturePosting(1, "Platform Engineer", "Bengaluru", "/jobs/platform-engineer"),
		ofBusinessFixturePosting(2, "Platform Engineer", "Pune", "/jobs/platform-engineer"),
		ofBusinessFixturePosting(3, "Data Engineer", "Gurugram", "/jobs/data-engineer"),
		ofBusinessFixturePosting(4, "Cloud Engineer", "Chennai", "/jobs/cloud-engineer"),
		ofBusinessFixturePosting(5, "Backend Engineer", "Hyderabad", "/jobs/backend-engineer"),
	}
	var requests atomic.Int32
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/categories" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Accept") != "text/html,application/xhtml+xml" ||
			!strings.Contains(r.Header.Get("User-Agent"), "Mozilla") {
			t.Errorf("unexpected headers: Accept=%q User-Agent=%q", r.Header.Get("Accept"), r.Header.Get("User-Agent"))
		}
		page := 1
		if value := r.URL.Query().Get("jobsDataset_page"); value != "" {
			var err error
			page, err = strconv.Atoi(value)
			if err != nil {
				t.Errorf("invalid page query %q", value)
			}
		}
		if page == 1 && r.URL.RawQuery != "" {
			t.Errorf("first page unexpectedly had query %q", r.URL.RawQuery)
		}
		if page < 1 || page > 3 {
			http.NotFound(w, r)
			return
		}
		start := (page - 1) * 2
		end := min(start+2, len(postings))
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		fmt.Fprint(w, ofBusinessFixtureDocument(
			t, serverURL, page, len(postings), 2, postings[start:end], postings[:2],
		))
	}))
	serverURL = server.URL
	defer server.Close()

	src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatalf("got %d requests, want 3", requests.Load())
	}
	if len(jobs) != 5 {
		t.Fatalf("got %d jobs, want 5", len(jobs))
	}
	if jobs[0].ID != "ofbusiness/"+ofBusinessFixtureUUID(1) ||
		jobs[0].URL != server.URL+"/jobs/platform-engineer" ||
		jobs[0].Company != "OfBusiness" ||
		jobs[0].Title != "Platform Engineer" ||
		jobs[0].Location != "Bengaluru" ||
		jobs[0].EmploymentType != "Full-Time" {
		t.Fatalf("unexpected first job: %+v", jobs[0])
	}
	if jobs[0].URL != jobs[1].URL || jobs[0].ID == jobs[1].ID {
		t.Fatalf("site-provided duplicate slugs did not retain distinct record IDs: %+v %+v", jobs[0], jobs[1])
	}
	for _, want := range []string{
		"Department: Engineering",
		"Experience Required: 0-2 years",
		"About the Business\nBuild useful products .",
		"What You Will Do\nOwn services",
		"What We Are Looking For\nStrong Go skills.",
		"What We Are Offering\nLearning budget",
	} {
		if !strings.Contains(jobs[0].Description, want) {
			t.Fatalf("description omitted %q:\n%s", want, jobs[0].Description)
		}
	}
	wantTime := time.Date(2026, 7, 1, 10, 0, 1, 0, time.UTC)
	if !jobs[0].PostedAt.Equal(wantTime) {
		t.Fatalf("PostedAt = %s, want %s", jobs[0].PostedAt, wantTime)
	}
}

func TestOfBusinessHandlesEmptyCollection(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, ofBusinessFixtureDocument(t, serverURL, 1, 0, 0, nil, nil))
	}))
	serverURL = server.URL
	defer server.Close()

	src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("got %d jobs, want an empty collection", len(jobs))
	}
}

func TestOfBusinessSkipsOnlyFullyBlankCMSPlaceholder(t *testing.T) {
	job := ofBusinessFixturePosting(1, "Backend Engineer", "Bengaluru", "/jobs/backend-engineer")
	placeholder := map[string]any{
		"_id":          ofBusinessFixtureUUID(2),
		"_createdDate": map[string]any{"$date": "2026-07-01T10:00:02Z"},
		"_updatedDate": map[string]any{"$date": "2026-07-01T10:00:02Z"},
		"jobCode":      202401,
	}
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, ofBusinessFixtureDocument(
			t, serverURL, 1, 2, 2, []map[string]any{job, placeholder}, []map[string]any{job, placeholder},
		))
	}))
	serverURL = server.URL
	defer server.Close()

	src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "ofbusiness/"+ofBusinessFixtureUUID(1) {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestOfBusinessRejectsMalformedPostingFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name: "partial placeholder",
			mutate: func(posting map[string]any) {
				for _, key := range []string{
					"location", "jobType", "experienceRequiredRangeyrs", "jobDescription",
					"whatYouWillDo", "whatWeAreLookingFor", "whatWeAreOffering",
					"link-jobs-jobTitle", "category",
				} {
					delete(posting, key)
				}
			},
			wantErr: "empty location",
		},
		{
			name: "missing rich text",
			mutate: func(posting map[string]any) {
				posting["whatYouWillDo"] = "<p> </p>"
			},
			wantErr: "empty What You Will Do",
		},
		{
			name: "external detail URL",
			mutate: func(posting map[string]any) {
				posting["link-jobs-jobTitle"] = "https://attacker.example/jobs/one"
			},
			wantErr: "invalid detail path",
		},
		{
			name: "detail query",
			mutate: func(posting map[string]any) {
				posting["link-jobs-jobTitle"] = "/jobs/one?redirect=https://attacker.example"
			},
			wantErr: "invalid detail path",
		},
		{
			name: "invalid category",
			mutate: func(posting map[string]any) {
				posting["category"] = map[string]any{"_id": "bad", "category": "Engineering"}
			},
			wantErr: "missing or invalid category",
		},
		{
			name: "invalid timestamp",
			mutate: func(posting map[string]any) {
				posting["_createdDate"] = map[string]any{"$date": "yesterday"}
			},
			wantErr: "invalid _createdDate",
		},
		{
			name: "update before creation",
			mutate: func(posting map[string]any) {
				posting["_updatedDate"] = map[string]any{"$date": "2026-06-30T10:00:00Z"}
			},
			wantErr: "_updatedDate is before _createdDate",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			posting := ofBusinessFixturePosting(1, "Backend Engineer", "Bengaluru", "/jobs/backend-engineer")
			test.mutate(posting)
			err := ofBusinessFetchFixture(t, []map[string]any{posting})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestOfBusinessRejectsIncompletePagination(t *testing.T) {
	t.Run("missing next link", func(t *testing.T) {
		postings := []map[string]any{
			ofBusinessFixturePosting(1, "One", "A", "/jobs/one"),
			ofBusinessFixturePosting(2, "Two", "B", "/jobs/two"),
			ofBusinessFixturePosting(3, "Three", "C", "/jobs/three"),
		}
		var serverURL string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			document := ofBusinessFixtureDocument(t, serverURL, 1, 3, 2, postings[:2], postings[:2])
			document = strings.Replace(
				document,
				`<link rel="next" href="`+serverURL+`/categories?jobsDataset_page=2"/>`,
				"",
				1,
			)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, document)
		}))
		serverURL = server.URL
		defer server.Close()

		src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
		_, err := src.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "omitted its next link") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("off-origin next link", func(t *testing.T) {
		postings := []map[string]any{
			ofBusinessFixturePosting(1, "One", "A", "/jobs/one"),
			ofBusinessFixturePosting(2, "Two", "B", "/jobs/two"),
			ofBusinessFixturePosting(3, "Three", "C", "/jobs/three"),
		}
		var serverURL string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			document := ofBusinessFixtureDocument(t, serverURL, 1, 3, 2, postings[:2], postings[:2])
			document = strings.Replace(document, serverURL+"/categories?jobsDataset_page=2", "https://attacker.example/categories?jobsDataset_page=2", 1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, document)
		}))
		serverURL = server.URL
		defer server.Close()

		src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
		_, err := src.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "does not stay on") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("collection total changes", func(t *testing.T) {
		postings := []map[string]any{
			ofBusinessFixturePosting(1, "One", "A", "/jobs/one"),
			ofBusinessFixturePosting(2, "Two", "B", "/jobs/two"),
			ofBusinessFixturePosting(3, "Three", "C", "/jobs/three"),
		}
		var serverURL string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page := 1
			if r.URL.Query().Get("jobsDataset_page") == "2" {
				page = 2
			}
			total := 3
			if page == 2 {
				total = 4
			}
			start := (page - 1) * 2
			end := min(start+2, len(postings))
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, ofBusinessFixtureDocument(
				t, serverURL, page, total, 2, postings[start:end], postings[:2],
			))
		}))
		serverURL = server.URL
		defer server.Close()

		src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
		_, err := src.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "collection total changed") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("duplicate record across pages", func(t *testing.T) {
		first := []map[string]any{
			ofBusinessFixturePosting(1, "One", "A", "/jobs/one"),
			ofBusinessFixturePosting(2, "Two", "B", "/jobs/two"),
		}
		var serverURL string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page := 1
			current := first
			if r.URL.Query().Get("jobsDataset_page") == "2" {
				page = 2
				current = []map[string]any{first[1]}
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, ofBusinessFixtureDocument(t, serverURL, page, 3, 2, current, first))
		}))
		serverURL = server.URL
		defer server.Close()

		src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
		_, err := src.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "duplicate record ID") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestOfBusinessRejectsMalformedWarmupState(t *testing.T) {
	posting := ofBusinessFixturePosting(1, "Backend Engineer", "Bengaluru", "/jobs/backend-engineer")
	valid := func(t *testing.T) map[string]any {
		t.Helper()
		return ofBusinessFixtureWarmup(1, 1, 1, []map[string]any{posting}, []map[string]any{posting})
	}
	tests := []struct {
		name     string
		document func(*testing.T) string
		wantErr  string
	}{
		{
			name: "missing script",
			document: func(*testing.T) string {
				return "<html></html>"
			},
			wantErr: "omitted wix-warmup-data",
		},
		{
			name: "duplicate script",
			document: func(t *testing.T) string {
				payload := ofBusinessFixtureJSON(t, valid(t))
				return ofBusinessFixtureScript(payload) + ofBusinessFixtureScript(payload)
			},
			wantErr: "duplicate wix-warmup-data",
		},
		{
			name: "wrong script type",
			document: func(t *testing.T) string {
				return `<script type="text/plain" id="wix-warmup-data">` +
					ofBusinessFixtureJSON(t, valid(t)) + `</script>`
			},
			wantErr: "unexpected type",
		},
		{
			name: "invalid JSON",
			document: func(*testing.T) string {
				return ofBusinessFixtureScript(`{"platform":`)
			},
			wantErr: "decoding wix-warmup-data",
		},
		{
			name: "missing Jobs schema",
			document: func(t *testing.T) string {
				root := valid(t)
				dataBinding := root["appsWarmupData"].(map[string]any)["dataBinding"].(map[string]any)
				dataBinding["schemas"] = map[string]any{}
				return ofBusinessFixtureScript(ofBusinessFixtureJSON(t, root))
			},
			wantErr: "omitted the Jobs schema",
		},
		{
			name: "missing record",
			document: func(t *testing.T) string {
				root := valid(t)
				store := root["appsWarmupData"].(map[string]any)["dataBinding"].(map[string]any)["dataStore"].(map[string]any)
				store["recordsByCollectionId"] = map[string]any{"Jobs": map[string]any{}}
				return ofBusinessFixtureScript(ofBusinessFixtureJSON(t, root))
			},
			wantErr: "displayed Jobs item lists",
		},
		{
			name: "conflicting dataset sizes",
			document: func(t *testing.T) string {
				root := valid(t)
				store := root["appsWarmupData"].(map[string]any)["dataBinding"].(map[string]any)["dataStore"].(map[string]any)
				infos := store["recordInfosByDatasetId"].(map[string]any)
				infos["conflict"] = map[string]any{
					"itemIds":     []string{ofBusinessFixtureUUID(1)},
					"datasetSize": map[string]any{"total": 2, "loaded": 1},
				}
				return ofBusinessFixtureScript(ofBusinessFixtureJSON(t, root))
			},
			wantErr: "conflicting dataset sizes",
		},
		{
			name: "conflicting pagination",
			document: func(t *testing.T) string {
				root := valid(t)
				platform := root["platform"].(map[string]any)
				updates := platform["ssrPropsUpdates"].([]any)
				platform["ssrPropsUpdates"] = append(
					updates,
					map[string]any{"otherPagination": map[string]any{"currentPage": 2, "totalPages": 2}},
				)
				return ofBusinessFixtureScript(ofBusinessFixtureJSON(t, root))
			},
			wantErr: "distinct pagination states",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseOfBusinessPage([]byte(test.document(t)))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestOfBusinessHTTPGuards(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name:   "status",
			status: http.StatusBadGateway, contentType: "text/html",
			body: "upstream unavailable", wantErr: "502 Bad Gateway",
		},
		{
			name:   "content type",
			status: http.StatusOK, contentType: "application/json",
			body: `{}`, wantErr: "unexpected Content-Type",
		},
		{
			name:   "body limit",
			status: http.StatusOK, contentType: "text/html",
			body: strings.Repeat("x", ofBusinessBodyLimit+1), wantErr: "exceeds",
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
			src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
			_, err := src.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, test.wantErr)
			}
		})
	}

	t.Run("redirect origin", func(t *testing.T) {
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html></html>")
		}))
		defer destination.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL+"/categories", http.StatusFound)
		}))
		defer server.Close()
		src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
		_, err := src.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "redirected away") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestOfBusinessFactoryRejectsParams(t *testing.T) {
	if _, err := New("ofbusiness", "OfBusiness", params.Map{"host": "attacker.example"}, http.DefaultClient); err == nil ||
		!strings.Contains(err.Error(), "does not accept params") {
		t.Fatalf("unexpected error: %v", err)
	}
	if src, err := New("ofbusiness", "OfBusiness", nil, nil); err != nil || src.Company() != "OfBusiness" {
		t.Fatalf("unexpected source/error: %v %v", src, err)
	}
}

func ofBusinessFetchFixture(t *testing.T, postings []map[string]any) error {
	t.Helper()
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, ofBusinessFixtureDocument(
			t, serverURL, 1, len(postings), len(postings), postings, postings,
		))
	}))
	serverURL = server.URL
	defer server.Close()
	src := &ofBusiness{company: "OfBusiness", base: server.URL, client: server.Client()}
	_, err := src.Fetch(context.Background())
	return err
}

func ofBusinessFixturePosting(
	number int,
	title, location, detailPath string,
) map[string]any {
	id := ofBusinessFixtureUUID(number)
	return map[string]any{
		"_id":                        id,
		"_createdDate":               map[string]any{"$date": fmt.Sprintf("2026-07-01T10:00:%02dZ", number)},
		"_updatedDate":               map[string]any{"$date": fmt.Sprintf("2026-07-02T10:00:%02dZ", number)},
		"jobTitle":                   title,
		"location":                   location,
		"jobType":                    "Full-Time",
		"experienceRequiredRangeyrs": "0-2 years",
		"jobDescription":             "<p>Build <strong>useful products</strong>.</p>",
		"whatYouWillDo":              "<ul><li>Own services</li></ul>",
		"whatWeAreLookingFor":        "<p>Strong Go skills.</p>",
		"whatWeAreOffering":          "<p>Learning budget</p>",
		"link-jobs-jobTitle":         detailPath,
		"category": map[string]any{
			"_id":      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"category": "Engineering",
		},
	}
}

func ofBusinessFixtureUUID(number int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", number)
}

func ofBusinessFixtureDocument(
	t *testing.T,
	base string,
	currentPage, total, pageSize int,
	currentRecords, firstRecords []map[string]any,
) string {
	t.Helper()
	totalPages := 1
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	var links strings.Builder
	if currentPage > 1 {
		href := base + "/categories"
		if currentPage-1 > 1 {
			href += "?jobsDataset_page=" + strconv.Itoa(currentPage-1)
		}
		fmt.Fprintf(&links, `<link rel="prev" href="%s"/>`, href)
	}
	if currentPage < totalPages {
		fmt.Fprintf(
			&links,
			`<link rel="next" href="%s/categories?jobsDataset_page=%d"/>`,
			base, currentPage+1,
		)
	}
	warmup := ofBusinessFixtureWarmup(
		currentPage, totalPages, total, currentRecords, firstRecords,
	)
	return "<html><head>" + links.String() + "</head><body>" +
		ofBusinessFixtureScript(ofBusinessFixtureJSON(t, warmup)) +
		"</body></html>"
}

func ofBusinessFixtureWarmup(
	currentPage, totalPages, total int,
	currentRecords, firstRecords []map[string]any,
) map[string]any {
	currentIDs := ofBusinessFixtureIDs(currentRecords)
	firstIDs := ofBusinessFixtureIDs(firstRecords)
	records := make(map[string]any)
	for _, posting := range firstRecords {
		records[posting["_id"].(string)] = posting
	}
	for _, posting := range currentRecords {
		records[posting["_id"].(string)] = posting
	}
	return map[string]any{
		"platform": map[string]any{
			"ssrPropsUpdates": []any{
				map[string]any{"jobsRepeater": map[string]any{"items": currentIDs}},
				map[string]any{"jobsPagination": map[string]any{
					"currentPage": currentPage,
					"totalPages":  totalPages,
				}},
			},
		},
		"appsWarmupData": map[string]any{
			"dataBinding": map[string]any{
				"schemas": map[string]any{"Jobs": map[string]any{"id": "Jobs"}},
				"dataStore": map[string]any{
					"recordInfosByDatasetId": map[string]any{
						"currentJobs": map[string]any{
							"itemIds": currentIDs,
							"datasetSize": map[string]any{
								"total": total, "loaded": len(currentIDs),
							},
						},
						"hiddenFirstPage": map[string]any{
							"itemIds": firstIDs,
							"datasetSize": map[string]any{
								"total": total, "loaded": len(firstIDs),
							},
						},
					},
					"recordsByCollectionId": map[string]any{"Jobs": records},
				},
			},
		},
	}
}

func ofBusinessFixtureIDs(records []map[string]any) []string {
	ids := make([]string, 0, len(records))
	for _, posting := range records {
		ids = append(ids, posting["_id"].(string))
	}
	return ids
}

func ofBusinessFixtureScript(payload string) string {
	return `<script type="application/json" id="wix-warmup-data">` + payload + `</script>`
}

func ofBusinessFixtureJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
