package source

import (
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
)

func TestPublicisSapientFetchAndDetail(t *testing.T) {
	var listCalls, detailCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bin/ps-redesign/careersJobsearch":
			listCalls++
			if r.URL.Query().Get("country") != "India" ||
				r.URL.Query().Get("start") != "0" ||
				r.URL.Query().Get("rows") != "100" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			writeJSON(t, w, map[string]any{"response": map[string]any{
				"numFound": 2, "start": 0, "docs": []map[string]any{
					{
						"id": "2026-144904", "jobId": "2026-144904",
						"name":             " Manager Data Engineering ",
						"displayLocation":  "Gurgaon, Haryana, India",
						"countryName":      "India",
						"jobDetailUrl":     "/job-details/2026-144904-manager-data-engineering-gurgaon",
						"typeOfEmployment": "Full-time",
						"releasedDate":     "2026-07-01T10:20:00Z",
					},
					{
						"id": "2026-144904-1064156", "jobId": "2026-144904",
						"name":             "Manager Data Engineering",
						"displayLocation":  "Bengaluru, Karnataka, India",
						"countryName":      "India",
						"jobDetailUrl":     "/job-details/2026-144904-1064156-manager-data-engineering-bengaluru",
						"typeOfEmployment": "Full-time",
						"releasedDate":     "2026-07-01T10:20:00Z",
					},
				},
			}})
		case "/job-details/2026-144904-manager-data-engineering-gurgaon":
			detailCalls++
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, publicisSapientDetailMarkup(
				t,
				"https://sapient-publicisgroupe.icims.com/jobs/144904/job/login",
			))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src := &publicisSapient{
		company: "Publicis Sapient", baseURL: server.URL, country: "India",
		maxPostings: 1000, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 1 || detailCalls != 0 || len(jobs) != 1 {
		t.Fatalf("calls list/detail=%d/%d jobs=%d", listCalls, detailCalls, len(jobs))
	}
	job := &jobs[0]
	if job.ID != "publicissapient/2026-144904" ||
		job.Company != "Publicis Sapient" ||
		job.Title != "Manager Data Engineering" ||
		job.Location != "Bengaluru, Karnataka, India; Gurgaon, Haryana, India" ||
		job.EmploymentType != "Full-time" ||
		job.URL != server.URL+"/job-details/2026-144904-manager-data-engineering-gurgaon" {
		t.Errorf("job = %#v", job)
	}
	wantTime, _ := time.Parse(time.RFC3339, "2026-07-01T10:20:00Z")
	if !job.PostedAt.Equal(wantTime) {
		t.Errorf("PostedAt = %s", job.PostedAt)
	}

	if err := src.Detail(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 {
		t.Fatalf("detail calls = %d", detailCalls)
	}
	if job.Description != "Job Description\nBuild & ship.\n\nQualifications\nOne year." {
		t.Errorf("Description = %q", job.Description)
	}
	if job.URL != "https://sapient-publicisgroupe.icims.com/jobs/144904/job/login" {
		t.Errorf("URL = %q", job.URL)
	}
}

func TestPublicisSapientRejectsMalformedPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"response": map[string]any{
			"numFound": 1, "start": 0, "docs": []map[string]any{{
				"id": "2026-1", "jobId": "2026-1", "name": "Role",
				"displayLocation": "Gurugram, India", "countryName": "India",
				"releasedDate": "2026-07-01T10:20:00Z",
				"jobDetailUrl": "https://evil.example/job",
			}},
		}})
	}))
	defer server.Close()
	src := &publicisSapient{
		company: "Publicis Sapient", baseURL: server.URL, country: "India",
		maxPostings: 100, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "invalid detail path") {
		t.Fatalf("Fetch = %#v, %v", jobs, err)
	}
}

func TestPublicisSapientDetailRequiresStructuredDescription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(
			w,
			`<html><div class="job-details" data-react-props='{&#34;primaryCta&#34;:{&#34;ctaLinkUrl&#34;:&#34;https://sapient-publicisgroupe.icims.com/jobs/1/job/login&#34;}}'></div></html>`,
		)
	}))
	defer server.Close()
	src := &publicisSapient{baseURL: server.URL, client: server.Client()}
	job := model.Job{URL: server.URL + "/job-details/one"}
	if err := src.Detail(context.Background(), &job); err == nil {
		t.Fatal("expected empty structured detail to fail")
	}
}

func TestPublicisSapientRejectsUnscopedOrUndatedDocuments(t *testing.T) {
	tests := []struct {
		name         string
		countryName  string
		releasedDate string
		wantErr      string
	}{
		{name: "missing country", releasedDate: "2026-07-01T10:20:00Z", wantErr: "countryName"},
		{
			name: "foreign country", countryName: "United States",
			releasedDate: "2026-07-01T10:20:00Z", wantErr: "countryName",
		},
		{name: "missing date", countryName: "India", wantErr: "empty releasedDate"},
		{
			name: "malformed date", countryName: "India", releasedDate: "July 1, 2026",
			wantErr: "invalid releasedDate",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"response": map[string]any{
					"numFound": 1, "start": 0, "docs": []map[string]any{{
						"id": "2026-1", "jobId": "2026-1", "name": "Role",
						"displayLocation": "Gurugram, India",
						"countryName":     test.countryName,
						"releasedDate":    test.releasedDate,
						"jobDetailUrl":    "/job-details/2026-1-role",
					}},
				}})
			}))
			defer server.Close()
			src := &publicisSapient{
				baseURL: server.URL, country: "India",
				maxPostings: 100, client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch = %#v, %v; want error containing %q", jobs, err, test.wantErr)
			}
		})
	}
}

func TestPublicisSapientRejectsInconsistentLocationDocuments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"response": map[string]any{
			"numFound": 2, "start": 0, "docs": []map[string]any{
				{
					"id": "2026-1", "jobId": "2026-1", "name": "Role",
					"displayLocation": "Gurugram, India", "countryName": "India",
					"releasedDate": "2026-07-01T10:20:00Z",
					"jobDetailUrl": "/job-details/2026-1-role",
				},
				{
					"id": "2026-1-123", "jobId": "2026-1", "name": "Different role",
					"displayLocation": "Bengaluru, India", "countryName": "India",
					"releasedDate": "2026-07-01T10:20:00Z",
					"jobDetailUrl": "/job-details/2026-1-123-role",
				},
			},
		}})
	}))
	defer server.Close()
	src := &publicisSapient{
		baseURL: server.URL, country: "India",
		maxPostings: 100, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "inconsistent shared fields") {
		t.Fatalf("Fetch = %#v, %v", jobs, err)
	}
}

func TestPublicisSapientDetailRejectsUntrustedInputsBeforeRequest(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()
	src := &publicisSapient{baseURL: server.URL, client: server.Client()}
	tests := []struct {
		name string
		job  *model.Job
	}{
		{name: "nil job"},
		{name: "base root", job: &model.Job{URL: server.URL}},
		{name: "wrong path", job: &model.Job{URL: server.URL + "/other/one"}},
		{name: "query", job: &model.Job{URL: server.URL + "/job-details/one?next=evil"}},
		{name: "foreign", job: &model.Job{URL: "https://example.com/job-details/one"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := src.Detail(context.Background(), test.job); err == nil {
				t.Fatal("Detail unexpectedly succeeded")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("untrusted jobs made %d requests", calls)
	}
}

func TestPublicisSapientDetailRejectsRedirectWithoutFollowingIt(t *testing.T) {
	var redirectedCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/job-details/one" {
			http.Redirect(w, r, "/job-details/two", http.StatusFound)
			return
		}
		redirectedCalls++
		_, _ = fmt.Fprint(w, publicisSapientDetailMarkup(
			t,
			"https://sapient-publicisgroupe.icims.com/jobs/2/job/login",
		))
	}))
	defer server.Close()
	src := &publicisSapient{baseURL: server.URL, client: server.Client()}
	job := model.Job{
		URL: server.URL + "/job-details/one", Description: "unchanged",
		EmploymentType: "unchanged",
	}
	original := job
	err := src.Detail(context.Background(), &job)
	if err == nil || !strings.Contains(err.Error(), "302 Found") {
		t.Fatalf("Detail error = %v", err)
	}
	if redirectedCalls != 0 {
		t.Fatalf("redirect target received %d requests", redirectedCalls)
	}
	if job != original {
		t.Fatalf("job mutated on failure: %#v", job)
	}
}

func TestPublicisSapientDetailValidatesPrimaryCTABeforeMutation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(
			w,
			publicisSapientDetailMarkup(t, "https://evil.example/jobs/1/job/login"),
		)
	}))
	defer server.Close()
	src := &publicisSapient{baseURL: server.URL, client: server.Client()}
	job := model.Job{
		URL: server.URL + "/job-details/one", Description: "unchanged",
		EmploymentType: "unchanged",
	}
	original := job
	err := src.Detail(context.Background(), &job)
	if err == nil || !strings.Contains(err.Error(), "invalid primary CTA") {
		t.Fatalf("Detail error = %v", err)
	}
	if job != original {
		t.Fatalf("job mutated on failure: %#v", job)
	}
}

func TestValidatePublicisSapientApplyURL(t *testing.T) {
	valid := "https://sapient-publicisgroupe.icims.com/jobs/144904/job/login"
	if got, err := validatePublicisSapientApplyURL(valid); err != nil || got != valid {
		t.Fatalf("valid apply URL = %q, %v", got, err)
	}
	for _, raw := range []string{
		"",
		"/jobs/144904/job/login",
		"http://sapient-publicisgroupe.icims.com/jobs/144904/job/login",
		"https://evil.example/jobs/144904/job/login",
		"https://sapient-publicisgroupe.icims.com/jobs/not-numeric/job/login",
		"https://sapient-publicisgroupe.icims.com/jobs/144904/job",
		"https://sapient-publicisgroupe.icims.com/jobs/144904/job/login?next=evil",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, err := validatePublicisSapientApplyURL(raw); err == nil || got != "" {
				t.Fatalf("validate(%q) = %q, %v", raw, got, err)
			}
		})
	}
}

func publicisSapientDetailMarkup(t *testing.T, cta string) string {
	t.Helper()
	payload := map[string]any{
		"typeOfEmployment": "Full-time",
		"jobDescriptionSection": map[string]any{
			"title": "Job Description", "body": "<p>Build &amp; ship.</p>",
		},
		"qualificationDescriptionSection": map[string]any{
			"title": "Qualifications", "body": "<ul><li>One year.</li></ul>",
		},
		"primaryCta": map[string]any{"ctaLinkUrl": cta},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return `<html><div class="job-details" data-cmp-is="x" data-react-props='` +
		stdhtml.EscapeString(string(encoded)) + `'></div></html>`
}
