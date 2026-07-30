package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestWorkdayPagination(t *testing.T) {
	tests := []struct {
		name            string
		jobCount        int
		maxPostings     int
		laterTotalsZero bool
		ignoreLimit     bool
		wantCalls       []struct {
			limit  int
			offset int
		}
	}{
		{
			name:            "preserves first total when later pages report zero",
			jobCount:        45,
			maxPostings:     500,
			laterTotalsZero: true,
			wantCalls: []struct {
				limit  int
				offset int
			}{{20, 0}, {20, 20}, {20, 40}},
		},
		{
			name:            "max postings remains exact when the server ignores limit",
			jobCount:        45,
			maxPostings:     25,
			laterTotalsZero: true,
			ignoreLimit:     true,
			wantCalls: []struct {
				limit  int
				offset int
			}{{20, 0}, {5, 20}},
		},
		{
			name:        "exact full terminal page does not request an empty page",
			jobCount:    40,
			maxPostings: 500,
			wantCalls: []struct {
				limit  int
				offset int
			}{{20, 0}, {20, 20}},
		},
		{
			name:        "truly empty board stops after the first page",
			jobCount:    0,
			maxPostings: 500,
			wantCalls: []struct {
				limit  int
				offset int
			}{{20, 0}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type request struct {
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
			}
			type posting struct {
				Title        string `json:"title"`
				ExternalPath string `json:"externalPath"`
			}

			var calls []struct {
				limit  int
				offset int
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/jobs") {
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				var req request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				calls = append(calls, struct {
					limit  int
					offset int
				}{req.Limit, req.Offset})

				responseLimit := req.Limit
				if tt.ignoreLimit {
					responseLimit = workdayPageSize
				}
				end := min(req.Offset+responseLimit, tt.jobCount)
				page := make([]posting, 0, max(0, end-req.Offset))
				for i := req.Offset; i < end; i++ {
					page = append(page, posting{
						Title:        fmt.Sprintf("Job %d", i),
						ExternalPath: fmt.Sprintf("/job/J-%d", i),
					})
				}
				total := tt.jobCount
				if tt.laterTotalsZero && req.Offset > 0 {
					total = 0
				}
				if err := json.NewEncoder(w).Encode(struct {
					Total       int       `json:"total"`
					JobPostings []posting `json:"jobPostings"`
				}{total, page}); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer srv.Close()

			wd := &workday{
				company:     "Acme",
				base:        srv.URL + "/wday/cxs/acme/jobs",
				maxPostings: tt.maxPostings,
				client:      srv.Client(),
			}
			jobs, err := wd.Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wantJobs := min(tt.jobCount, tt.maxPostings)
			if len(jobs) != wantJobs {
				t.Fatalf("Fetch returned %d jobs, want %d", len(jobs), wantJobs)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Errorf("requests = %#v, want %#v", calls, tt.wantCalls)
			}
		})
	}
}
