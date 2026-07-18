package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fetch lists the whole board without any detail request; Detail fills the
// posting on demand. Identities are the externalPath (stable req ID), so
// they don't change between runs.
func TestWorkdayListsLazilyThenDetails(t *testing.T) {
	var listCalls, detailCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") {
			listCalls++
			w.Write([]byte(`{"total":1,"jobPostings":[{"title":"Software Engineer I","externalPath":"/job/Pune/SWE_R-123","locationsText":"Pune"}]}`))
			return
		}
		detailCalls++
		w.Write([]byte(`{"jobPostingInfo":{"jobDescription":"<p>Build things. 1+ years.</p>","location":"Pune, India","timeType":"Full time","externalUrl":"https://x/job/R-123"}}`))
	}))
	defer srv.Close()

	// Build the source directly so base points at the test server
	// (New would construct an https:// base URL).
	wd := &workday{company: "Acme", base: srv.URL + "/wday/cxs/acme/jobs", maxPostings: 500, client: srv.Client()}

	jobs, err := wd.Fetch(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Fetch = %d jobs, %v", len(jobs), err)
	}
	if detailCalls != 0 {
		t.Errorf("Fetch made %d detail calls, want 0 (lazy)", detailCalls)
	}
	if jobs[0].Description != "" {
		t.Error("Fetch should not populate description")
	}
	if !strings.HasSuffix(jobs[0].ID, "/job/Pune/SWE_R-123") {
		t.Errorf("id should embed externalPath, got %q", jobs[0].ID)
	}

	if err := wd.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 || !strings.Contains(jobs[0].Description, "Build things") || jobs[0].EmploymentType != "Full time" {
		t.Errorf("Detail did not fill posting: calls=%d job=%+v", detailCalls, jobs[0])
	}
}
