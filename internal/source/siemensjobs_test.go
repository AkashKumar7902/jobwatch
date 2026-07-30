package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestSiemensJobsFetchesAllPagesWithPerLocationGUIDs(t *testing.T) {
	postings := make([]map[string]any, 11)
	for i := range postings {
		guid := fmt.Sprintf("%032X", i+1)
		postings[i] = map[string]any{
			"guid": guid, "id": "seo.joblisting." + guid,
			"title_exact":    fmt.Sprintf("EDA Engineer %d", i),
			"title_slug":     fmt.Sprintf("eda-engineer-%d", i),
			"location_exact": "New Cairo, EGY",
			"description":    "**Job Type:** Full-time\n\nBuild EDA tools.",
			"date_new":       "2026-07-30T03:47:24Z",
		}
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-origin") != "jobs.sw.siemens.com" {
			t.Errorf("x-origin = %q", r.Header.Get("x-origin"))
		}
		if r.URL.Query().Get("businessStructures") != "electronic-design-automation-eda" ||
			r.URL.Query().Get("num_items") != "10" {
			t.Errorf("query = %v", r.URL.Query())
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := (page - 1) * siemensJobsPageSize
		end := min(start+siemensJobsPageSize, len(postings))
		reportedPage, reportedOffset, reportedPages := any(page), any(start), any(2)
		if page == 2 {
			reportedPage = json.RawMessage("2.0")
			reportedOffset = json.RawMessage("10.0")
			reportedPages = json.RawMessage("2.0")
		}
		writeJSON(t, w, map[string]any{
			"jobs": postings[start:end],
			"pagination": map[string]any{
				"has_more_pages": page == 1, "offset": reportedOffset, "page": reportedPage,
				"page_size": 10, "total": len(postings), "total_pages": reportedPages,
			},
		})
		calls++
	}))
	defer srv.Close()

	src := &siemensJobs{
		company: "Siemens EDA", apiBase: srv.URL, jobsBase: "https://jobs.sw.siemens.com",
		origin: "jobs.sw.siemens.com", client: srv.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 11 || calls != 2 {
		t.Fatalf("Fetch returned %d jobs in %d calls, want 11 in 2", len(jobs), calls)
	}
	first := jobs[0]
	if first.ID != "siemensjobs/00000000000000000000000000000001" {
		t.Errorf("ID = %q", first.ID)
	}
	if first.URL != "https://jobs.sw.siemens.com/new-cairo-egy/eda-engineer-0/00000000000000000000000000000001/job/" {
		t.Errorf("URL = %q", first.URL)
	}
	if first.EmploymentType != "Full-time" || first.Description != "**Job Type:** Full-time\n\nBuild EDA tools." {
		t.Errorf("normalized fields = %+v", first)
	}
}

func TestSiemensJobsPaginationAcceptsDecimalIntegersOnly(t *testing.T) {
	var value siemensJobsInt
	if err := json.Unmarshal([]byte("2.0"), &value); err != nil || value != 2 {
		t.Fatalf("unmarshal 2.0 = (%d, %v), want (2, nil)", value, err)
	}
	for _, input := range []string{"2.5", `"2"`, "null"} {
		if err := json.Unmarshal([]byte(input), &value); err == nil {
			t.Errorf("unmarshal %s succeeded, want error", input)
		}
	}
}

func TestSiemensJobsRejectsDuplicateGUIDs(t *testing.T) {
	guid := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	posting := map[string]any{
		"guid": guid, "id": "seo.joblisting." + guid, "title_exact": "One",
		"title_slug": "one", "location_exact": "Austin, TX", "description": "Full description",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"jobs": []any{posting, posting},
			"pagination": map[string]any{
				"has_more_pages": false, "offset": 0, "page": 1,
				"page_size": 10, "total": 2, "total_pages": 1,
			},
		})
	}))
	defer srv.Close()
	src := &siemensJobs{
		company: "Siemens", apiBase: srv.URL, jobsBase: srv.URL,
		origin: "jobs.sw.siemens.com", client: srv.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}

func TestSiemensJobsRejectsMissingPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"jobs": []any{}})
	}))
	defer srv.Close()
	src := &siemensJobs{
		company: "Siemens", apiBase: srv.URL, jobsBase: srv.URL,
		origin: "jobs.sw.siemens.com", client: srv.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}
