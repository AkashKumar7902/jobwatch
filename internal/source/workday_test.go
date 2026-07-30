package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type workdayTestRequest struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

type workdayTestPosting struct {
	Title        string `json:"title"`
	ExternalPath string `json:"externalPath"`
}

type workdayTestResponse struct {
	Total       int                  `json:"total"`
	JobPostings []workdayTestPosting `json:"jobPostings"`
}

type workdayTestCall struct {
	limit  int
	offset int
}

func workdayTestPostings(start, count int) []workdayTestPosting {
	postings := make([]workdayTestPosting, 0, count)
	for i := start; i < start+count; i++ {
		postings = append(postings, workdayTestPosting{
			Title:        fmt.Sprintf("Job %d", i),
			ExternalPath: fmt.Sprintf("/job/J-%d", i),
		})
	}
	return postings
}

func writeWorkdayTestPage(t *testing.T, w http.ResponseWriter, total int, postings []workdayTestPosting) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(workdayTestResponse{Total: total, JobPostings: postings}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

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
		alwaysTotalZero bool
		ignoreLimit     bool
		wantLog         string
		wantCalls       []workdayTestCall
	}{
		{
			name:            "preserves first total when later pages report zero",
			jobCount:        45,
			maxPostings:     500,
			laterTotalsZero: true,
			wantCalls:       []workdayTestCall{{20, 0}, {20, 20}, {20, 40}},
		},
		{
			name:            "max postings remains exact when the server ignores limit",
			jobCount:        45,
			maxPostings:     25,
			laterTotalsZero: true,
			ignoreLimit:     true,
			wantCalls:       []workdayTestCall{{20, 0}, {5, 20}},
		},
		{
			name:            "unknown total logs max postings cap",
			jobCount:        45,
			maxPostings:     25,
			alwaysTotalZero: true,
			wantLog:         "max_postings cap; total unknown",
			wantCalls:       []workdayTestCall{{20, 0}, {5, 20}},
		},
		{
			name:        "exact full terminal page does not request an empty page",
			jobCount:    40,
			maxPostings: 500,
			wantCalls:   []workdayTestCall{{20, 0}, {20, 20}},
		},
		{
			name:        "truly empty board stops after the first page",
			jobCount:    0,
			maxPostings: 500,
			wantCalls:   []workdayTestCall{{20, 0}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []workdayTestCall
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/jobs") {
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				var req workdayTestRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				calls = append(calls, workdayTestCall{req.Limit, req.Offset})

				responseLimit := req.Limit
				if tt.ignoreLimit {
					responseLimit = workdayPageSize
				}
				end := min(req.Offset+responseLimit, tt.jobCount)
				page := workdayTestPostings(req.Offset, max(0, end-req.Offset))
				total := tt.jobCount
				if tt.alwaysTotalZero || tt.laterTotalsZero && req.Offset > 0 {
					total = 0
				}
				writeWorkdayTestPage(t, w, total, page)
			}))
			defer srv.Close()

			wd := &workday{
				company:     "Acme",
				base:        srv.URL + "/wday/cxs/acme/jobs",
				maxPostings: tt.maxPostings,
				client:      srv.Client(),
			}
			var logs bytes.Buffer
			oldLogWriter := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(oldLogWriter)

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
			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log = %q, want substring %q", logs.String(), tt.wantLog)
			}
		})
	}
}

func TestWorkdayIncompletePaginationReturnsPartialJobs(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		firstTotal int
		wantJobs   int
		wantErr    string
		wantCalls  int
		wantNil    bool
	}{
		{
			name:       "later empty page",
			mode:       "empty",
			firstTotal: 25,
			wantJobs:   20,
			wantErr:    "empty page at offset 20 after scanning 20 of 25 raw postings (20 usable)",
			wantCalls:  2,
		},
		{
			name:       "later short page",
			mode:       "short",
			firstTotal: 30,
			wantJobs:   25,
			wantErr:    "short page (5 of 20 requested) at offset 20 after scanning 25 of 30 raw postings (25 usable)",
			wantCalls:  2,
		},
		{
			name:       "later request failure",
			mode:       "error",
			firstTotal: 40,
			wantJobs:   20,
			wantErr:    "fetch page at offset 20 after scanning 20 raw postings (20 usable)",
			wantCalls:  2,
		},
		{
			name:      "first request failure",
			mode:      "first error",
			wantErr:   "fetch page at offset 0 after scanning 0 raw postings (0 usable)",
			wantCalls: 1,
			wantNil:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req workdayTestRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if tt.mode == "first error" {
					http.Error(w, "upstream unavailable", http.StatusBadGateway)
					return
				}
				if req.Offset == 0 {
					writeWorkdayTestPage(t, w, tt.firstTotal, workdayTestPostings(0, workdayPageSize))
					return
				}
				switch tt.mode {
				case "empty":
					writeWorkdayTestPage(t, w, 0, nil)
				case "short":
					writeWorkdayTestPage(t, w, 0, workdayTestPostings(workdayPageSize, 5))
				case "error":
					http.Error(w, "upstream unavailable", http.StatusBadGateway)
				default:
					http.Error(w, "unexpected test mode", http.StatusInternalServerError)
				}
			}))
			defer srv.Close()

			wd := &workday{
				company:     "Acme",
				base:        srv.URL + "/wday/cxs/acme/jobs",
				maxPostings: 500,
				client:      srv.Client(),
			}
			jobs, err := wd.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Fetch error = %v, want substring %q", err, tt.wantErr)
			}
			if len(jobs) != tt.wantJobs {
				t.Fatalf("Fetch returned %d partial jobs, want %d", len(jobs), tt.wantJobs)
			}
			if tt.wantNil && jobs != nil {
				t.Fatalf("first-page failure jobs = %#v, want nil", jobs)
			}
			if calls != tt.wantCalls {
				t.Errorf("request count = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func TestWorkdaySkipsInvalidRowsAndCompletesKnownTotal(t *testing.T) {
	var calls []workdayTestCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req workdayTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		calls = append(calls, workdayTestCall{req.Limit, req.Offset})

		total := 0
		var page []workdayTestPosting
		switch req.Offset {
		case 0:
			total = 45
			page = workdayTestPostings(0, workdayPageSize)
			page[5].ExternalPath = ""
		case 20:
			page = workdayTestPostings(20, workdayPageSize)
			page[0].ExternalPath = "/job/J-0" // Cross-page drift duplicate.
			page[2].ExternalPath = page[1].ExternalPath
		case 40:
			page = workdayTestPostings(40, 5)
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
			return
		}
		writeWorkdayTestPage(t, w, total, page)
	}))
	defer srv.Close()

	wd := &workday{
		company:     "Acme",
		base:        srv.URL + "/wday/cxs/acme/jobs",
		maxPostings: 500,
		client:      srv.Client(),
	}
	jobs, err := wd.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch error = nil, want skipped-row warning")
	}
	for _, want := range []string{
		"skipped 1 malformed postings with missing externalPath (raw offsets [5])",
		"skipped 2 duplicate externalPath postings",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Fetch error = %q, want substring %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "pagination ended") {
		t.Errorf("known raw total was reached but Fetch reported incomplete pagination: %v", err)
	}
	if len(jobs) != 42 {
		t.Fatalf("Fetch returned %d usable jobs, want 42", len(jobs))
	}
	if !strings.HasSuffix(jobs[len(jobs)-1].ID, "/job/J-44") {
		t.Errorf("last job = %q, want later page J-44", jobs[len(jobs)-1].ID)
	}
	if want := []workdayTestCall{{20, 0}, {20, 20}, {20, 40}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("requests = %#v, want %#v", calls, want)
	}
}

func TestWorkdayRawPositionCapWithSkippedRows(t *testing.T) {
	var calls []workdayTestCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req workdayTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		calls = append(calls, workdayTestCall{req.Limit, req.Offset})

		// Return a full 20 rows even when Fetch requests only the five raw
		// positions remaining under max_postings.
		page := workdayTestPostings(req.Offset, workdayPageSize)
		if req.Offset == 0 {
			page[5].ExternalPath = ""
		} else {
			page[0].ExternalPath = "/job/J-0"
		}
		writeWorkdayTestPage(t, w, 45, page)
	}))
	defer srv.Close()

	wd := &workday{
		company:     "Acme",
		base:        srv.URL + "/wday/cxs/acme/jobs",
		maxPostings: 25,
		client:      srv.Client(),
	}
	var logs bytes.Buffer
	oldLogWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldLogWriter)

	jobs, err := wd.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "skipped 1 malformed") ||
		!strings.Contains(err.Error(), "skipped 1 duplicate") {
		t.Fatalf("Fetch error = %v, want malformed and duplicate warnings", err)
	}
	if len(jobs) != 23 {
		t.Fatalf("Fetch returned %d usable jobs, want 23 from 25 raw positions", len(jobs))
	}
	if want := []workdayTestCall{{20, 0}, {5, 20}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("requests = %#v, want %#v", calls, want)
	}
	wantLog := "scanned 25 of 45 raw postings, listing 23 usable postings (max_postings cap)"
	if !strings.Contains(logs.String(), wantLog) {
		t.Errorf("log = %q, want substring %q", logs.String(), wantLog)
	}
}

func TestWorkdayRepeatedPageReturnsPartialJobs(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req workdayTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Offset == 0 {
			writeWorkdayTestPage(t, w, 60, workdayTestPostings(0, workdayPageSize))
			return
		}
		writeWorkdayTestPage(t, w, 0, workdayTestPostings(0, workdayPageSize))
	}))
	defer srv.Close()

	wd := &workday{
		company:     "Acme",
		base:        srv.URL + "/wday/cxs/acme/jobs",
		maxPostings: 500,
		client:      srv.Client(),
	}
	jobs, err := wd.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "repeated page at offset 20 made no unique externalPath progress") ||
		!strings.Contains(err.Error(), "skipped 20 duplicate externalPath postings") {
		t.Fatalf("Fetch error = %v, want repeated-page and duplicate warnings", err)
	}
	if len(jobs) != workdayPageSize {
		t.Fatalf("Fetch returned %d partial jobs, want %d", len(jobs), workdayPageSize)
	}
	if calls != 2 {
		t.Errorf("request count = %d, want 2", calls)
	}
}
