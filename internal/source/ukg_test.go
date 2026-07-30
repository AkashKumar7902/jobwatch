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

const testUKGBoard = "d6eed263-4950-420d-b9f8-5b1a441c931e"

func TestUKGNewValidatesAndNormalizesBoardCoordinates(t *testing.T) {
	t.Parallel()

	src, err := New("ukg", "AppLogic Networks", params.Map{
		"host":   "Recruiting2.Ultipro.COM.",
		"tenant": "PRO1053PROC",
		"board":  strings.ToUpper(testUKGBoard),
	}, http.DefaultClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("source type = %T, want *identifiedSource", src)
	}
	got, ok := wrapped.Source.(*ukg)
	if !ok {
		t.Fatalf("wrapped source type = %T, want *ukg", wrapped.Source)
	}
	if got.host != "recruiting2.ultipro.com" {
		t.Errorf("host = %q", got.host)
	}
	if got.board != testUKGBoard {
		t.Errorf("board = %q", got.board)
	}
	wantURL := "https://recruiting2.ultipro.com/PRO1053PROC/JobBoard/" + testUKGBoard +
		"/OpportunityDetail?opportunityId=11111111-2222-3333-4444-555555555555"
	if actual := got.detailURL("11111111-2222-3333-4444-555555555555"); actual != wantURL {
		t.Errorf("detailURL = %q, want %q", actual, wantURL)
	}

	tests := []struct {
		name    string
		params  params.Map
		wantErr string
	}{
		{
			name: "missing host",
			params: params.Map{
				"tenant": "PRO1053PROC", "board": testUKGBoard,
			},
			wantErr: `missing required param "host"`,
		},
		{
			name: "host contains scheme",
			params: params.Map{
				"host": "https://recruiting2.ultipro.com", "tenant": "PRO1053PROC", "board": testUKGBoard,
			},
			wantErr: "invalid board host",
		},
		{
			name: "missing tenant",
			params: params.Map{
				"host": "recruiting2.ultipro.com", "board": testUKGBoard,
			},
			wantErr: `missing required param "tenant"`,
		},
		{
			name: "tenant can not alter path",
			params: params.Map{
				"host": "recruiting2.ultipro.com", "tenant": "../other", "board": testUKGBoard,
			},
			wantErr: "invalid UKG tenant",
		},
		{
			name: "missing board",
			params: params.Map{
				"host": "recruiting2.ultipro.com", "tenant": "PRO1053PROC",
			},
			wantErr: `missing required param "board"`,
		},
		{
			name: "board must be UUID",
			params: params.Map{
				"host": "recruiting2.ultipro.com", "tenant": "PRO1053PROC", "board": "not-a-board",
			},
			wantErr: "expected a canonical UUID",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := New("ukg", "Example", test.params, http.DefaultClient)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestUKGFetchPaginatesAndNormalizes(t *testing.T) {
	t.Parallel()

	postings := make([]ukgOpportunity, 51)
	for index := range postings {
		postings[index] = testUKGOpportunity(index + 1)
	}
	postings[0].Title = "  Senior Test Engineer  "
	postings[0].Locations = []ukgLocation{
		{LocalizedName: " Bangalore Office "},
		{
			Address: ukgAddress{
				City: "Pune",
				State: &ukgRegion{
					Name: "Maharashtra",
				},
				Country: &ukgRegion{
					Name: "India",
				},
			},
		},
	}
	postings[1].FullTime = ukgBool(false)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		wantPath := "/PRO1053PROC/JobBoard/" + testUKGBoard + "/JobBoardView/LoadSearchResults"
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "jobwatch") {
			t.Errorf("User-Agent = %q", got)
		}

		var request ukgSearchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		search := request.OpportunitySearch
		if search.Top != ukgPageSize {
			t.Errorf("Top = %d, want %d", search.Top, ukgPageSize)
		}
		wantSkip := 0
		if call == 2 {
			wantSkip = 50
		}
		if search.Skip != wantSkip {
			t.Errorf("Skip = %d, want %d", search.Skip, wantSkip)
		}
		if search.QueryString != "" {
			t.Errorf("QueryString = %q", search.QueryString)
		}
		if search.Filters == nil || len(search.Filters) != 0 {
			t.Errorf("Filters = %#v, want a non-nil empty array", search.Filters)
		}

		end := min(search.Skip+search.Top, len(postings))
		if search.Skip > end {
			t.Errorf("invalid requested skip %d", search.Skip)
			http.Error(w, "invalid page", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"opportunities": postings[search.Skip:end],
			"totalCount":    len(postings),
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	src := testUKGSource(server)
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	if len(jobs) != len(postings) {
		t.Fatalf("jobs = %d, want %d", len(jobs), len(postings))
	}

	firstID := testUKGUUID(1)
	first := jobs[0]
	if first.ID != "ukg/recruiting.example.test/PRO1053PROC/"+testUKGBoard+"/"+firstID {
		t.Errorf("ID = %q", first.ID)
	}
	if first.Company != "AppLogic Networks" {
		t.Errorf("Company = %q", first.Company)
	}
	if first.Title != "Senior Test Engineer" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.Location != "Bangalore Office; Pune, Maharashtra, India" {
		t.Errorf("Location = %q", first.Location)
	}
	if first.EmploymentType != "Full Time" {
		t.Errorf("EmploymentType = %q", first.EmploymentType)
	}
	if first.Description != "" {
		t.Errorf("Description = %q, want lazy empty description", first.Description)
	}
	wantURL := server.URL + "/PRO1053PROC/JobBoard/" + testUKGBoard +
		"/OpportunityDetail?opportunityId=" + firstID
	if first.URL != wantURL {
		t.Errorf("URL = %q, want %q", first.URL, wantURL)
	}
	wantDate := time.Date(2026, time.July, 30, 9, 10, 11, 123000000, time.UTC)
	if !first.PostedAt.Equal(wantDate) {
		t.Errorf("PostedAt = %s, want %s", first.PostedAt, wantDate)
	}
	if jobs[1].EmploymentType != "Part Time" {
		t.Errorf("second EmploymentType = %q", jobs[1].EmploymentType)
	}
	if jobs[50].ID != "ukg/recruiting.example.test/PRO1053PROC/"+testUKGBoard+"/"+testUKGUUID(51) {
		t.Errorf("last ID = %q", jobs[50].ID)
	}
}

func TestUKGFetchAcceptsExplicitEmptyBoard(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"opportunities":[],"totalCount":0}`)
	}))
	defer server.Close()

	jobs, err := testUKGSource(server).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want empty", jobs)
	}
}

func TestUKGWireFixturesUseProductionFieldNames(t *testing.T) {
	t.Parallel()

	const searchPayload = `{
		"opportunities":[{
			"Id":"00000000-0000-4000-8000-000000000001",
			"Title":"Wire Role",
			"FullTime":true,
			"PostedDate":"2026-07-30T09:10:11.123Z",
			"Locations":[{"LocalizedName":"Bengaluru"}]
		}],
		"totalCount":1
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, searchPayload)
	}))
	defer server.Close()

	jobs, err := testUKGSource(server).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch captured search payload: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Title != "Wire Role" || jobs[0].Location != "Bengaluru" {
		t.Fatalf("decoded jobs = %#v", jobs)
	}

	const detailPayload = `<script>new US.Opportunity.CandidateOpportunityDetail({
		"Id":"00000000-0000-4000-8000-000000000001",
		"Title":"Wire Role",
		"FullTime":false,
		"OpportunityIsClosed":false,
		"PostedDate":"2026-07-30T09:10:11.123Z",
		"Locations":[{"LocalizedDescription":"India"}],
		"Description":"<p>Captured description</p>"
	});</script>`
	detail, err := parseUKGDetail([]byte(detailPayload))
	if err != nil {
		t.Fatalf("parse captured detail payload: %v", err)
	}
	if detail.ID != testUKGUUID(1) || detail.FullTime == nil || *detail.FullTime ||
		detail.OpportunityClosed == nil || *detail.OpportunityClosed ||
		detail.Description != "<p>Captured description</p>" {
		t.Fatalf("decoded detail = %#v", detail)
	}
}

func TestUKGFetchRejectsIncompleteOrDriftedResponses(t *testing.T) {
	t.Parallel()

	fifty := make([]ukgOpportunity, 50)
	for index := range fifty {
		fifty[index] = testUKGOpportunity(index + 1)
	}
	one := []ukgOpportunity{testUKGOpportunity(1)}
	two := []ukgOpportunity{testUKGOpportunity(1), testUKGOpportunity(2)}

	tests := []struct {
		name    string
		respond func(t *testing.T, call int32, w http.ResponseWriter)
		wantErr string
	}{
		{
			name: "missing total count",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				writeUKGJSON(t, w, map[string]any{"opportunities": []ukgOpportunity{}})
			},
			wantErr: "omitted totalCount",
		},
		{
			name: "missing opportunities",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				writeUKGJSON(t, w, map[string]any{"totalCount": 0})
			},
			wantErr: "omitted opportunities",
		},
		{
			name: "negative total",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				writeUKGJSON(t, w, map[string]any{"opportunities": []ukgOpportunity{}, "totalCount": -1})
			},
			wantErr: "negative totalCount",
		},
		{
			name: "unreasonable total",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				writeUKGJSON(t, w, map[string]any{"opportunities": []ukgOpportunity{}, "totalCount": ukgMaximumPostings + 1})
			},
			wantErr: "exceeds safety limit",
		},
		{
			name: "empty page before total",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				writeUKGJSON(t, w, map[string]any{"opportunities": []ukgOpportunity{}, "totalCount": 1})
			},
			wantErr: "empty page",
		},
		{
			name: "short page before total",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				writeUKGJSON(t, w, map[string]any{"opportunities": one, "totalCount": 2})
			},
			wantErr: "short page",
		},
		{
			name: "page exceeds total",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				writeUKGJSON(t, w, map[string]any{"opportunities": two, "totalCount": 1})
			},
			wantErr: "would exceed reported total",
		},
		{
			name: "page exceeds requested size",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				tooMany := append(append([]ukgOpportunity{}, fifty...), testUKGOpportunity(51))
				writeUKGJSON(t, w, map[string]any{"opportunities": tooMany, "totalCount": 51})
			},
			wantErr: "requested at most 50",
		},
		{
			name: "total changes",
			respond: func(t *testing.T, call int32, w http.ResponseWriter) {
				if call == 1 {
					writeUKGJSON(t, w, map[string]any{"opportunities": fifty, "totalCount": 51})
					return
				}
				writeUKGJSON(t, w, map[string]any{
					"opportunities": []ukgOpportunity{testUKGOpportunity(51)}, "totalCount": 52,
				})
			},
			wantErr: "totalCount changed",
		},
		{
			name: "duplicate stable UUID",
			respond: func(t *testing.T, call int32, w http.ResponseWriter) {
				if call == 1 {
					writeUKGJSON(t, w, map[string]any{"opportunities": fifty, "totalCount": 51})
					return
				}
				writeUKGJSON(t, w, map[string]any{"opportunities": one, "totalCount": 51})
			},
			wantErr: "duplicate opportunity Id",
		},
		{
			name: "invalid stable UUID",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				bad := testUKGOpportunity(1)
				bad.ID = "not-a-uuid"
				writeUKGJSON(t, w, map[string]any{"opportunities": []ukgOpportunity{bad}, "totalCount": 1})
			},
			wantErr: "invalid Id",
		},
		{
			name: "missing title",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				bad := testUKGOpportunity(1)
				bad.Title = " "
				writeUKGJSON(t, w, map[string]any{"opportunities": []ukgOpportunity{bad}, "totalCount": 1})
			},
			wantErr: "missing Title",
		},
		{
			name: "missing employment schema",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				bad := testUKGOpportunity(1)
				bad.FullTime = nil
				writeUKGJSON(t, w, map[string]any{"opportunities": []ukgOpportunity{bad}, "totalCount": 1})
			},
			wantErr: "omitted FullTime",
		},
		{
			name: "invalid posted date",
			respond: func(t *testing.T, _ int32, w http.ResponseWriter) {
				bad := testUKGOpportunity(1)
				bad.PostedDate = "last Thursday"
				writeUKGJSON(t, w, map[string]any{"opportunities": []ukgOpportunity{bad}, "totalCount": 1})
			},
			wantErr: "unsupported posting date",
		},
		{
			name: "HTTP failure",
			respond: func(_ *testing.T, _ int32, w http.ResponseWriter) {
				http.Error(w, "upstream failed", http.StatusBadGateway)
			},
			wantErr: "502 Bad Gateway",
		},
		{
			name: "invalid JSON",
			respond: func(_ *testing.T, _ int32, w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"opportunities":`)
			},
			wantErr: "decoding response",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				test.respond(t, calls.Add(1), w)
			}))
			defer server.Close()

			_, err := testUKGSource(server).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestUKGDetailParsesEmbeddedCandidateOpportunity(t *testing.T) {
	t.Parallel()

	detail := testUKGOpportunity(7)
	detail.Title = " Principal Engineer "
	detail.FullTime = ukgBool(false)
	detail.Description = `<p>Build &amp; test resilient systems.</p><ul><li>Ship safely.</li></ul>`
	detail.Locations = []ukgLocation{{
		Address: ukgAddress{
			City: "Bengaluru",
			State: &ukgRegion{
				Code: "KA",
			},
			Country: &ukgRegion{
				Name: "India",
			},
		},
	}}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		wantPath := "/PRO1053PROC/JobBoard/" + testUKGBoard + "/OpportunityDetail"
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		if got := r.URL.Query().Get("opportunityId"); got != detail.ID {
			t.Errorf("opportunityId = %q, want %q", got, detail.ID)
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><script>
			var opportunity =
				new   US.Opportunity.CandidateOpportunityDetail  ( %s );
		</script></html>`, encoded)
	}))
	defer server.Close()

	src := testUKGSource(server)
	job := model.Job{ID: src.jobID(detail.ID)}
	if err := src.Detail(context.Background(), &job); err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if job.Title != "Principal Engineer" {
		t.Errorf("Title = %q", job.Title)
	}
	if !strings.Contains(job.Description, "Build & test resilient systems.") ||
		!strings.Contains(job.Description, "Ship safely.") {
		t.Errorf("Description = %q", job.Description)
	}
	if job.Location != "Bengaluru, KA, India" {
		t.Errorf("Location = %q", job.Location)
	}
	if job.EmploymentType != "Part Time" {
		t.Errorf("EmploymentType = %q", job.EmploymentType)
	}
	if job.URL != src.detailURL(detail.ID) {
		t.Errorf("URL = %q", job.URL)
	}
	wantPostedAt, err := time.Parse(time.RFC3339Nano, detail.PostedDate)
	if err != nil {
		t.Fatal(err)
	}
	if !job.PostedAt.Equal(wantPostedAt) {
		t.Errorf("PostedAt = %s, want %s", job.PostedAt, wantPostedAt)
	}
}

func TestUKGDetailRejectsForeignJobWithoutRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	src := testUKGSource(server)

	tests := []struct {
		name    string
		job     *model.Job
		wantErr string
	}{
		{name: "nil", job: nil, wantErr: "nil job"},
		{name: "another source", job: &model.Job{ID: "workday/example/job/1"}, wantErr: "does not belong"},
		{name: "another board", job: &model.Job{
			ID: "ukg/recruiting.example.test/PRO1053PROC/11111111-1111-1111-1111-111111111111/" + testUKGUUID(1),
		}, wantErr: "does not belong"},
		{name: "invalid posting UUID", job: &model.Job{ID: src.jobID("not-a-uuid")}, wantErr: "invalid job ID"},
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
	if got := calls.Load(); got != 0 {
		t.Fatalf("unexpected HTTP request count = %d", got)
	}
}

func TestUKGDetailRejectsIncompleteOrDriftedPage(t *testing.T) {
	t.Parallel()

	requestedID := testUKGUUID(1)
	tests := []struct {
		name       string
		statusCode int
		body       func(t *testing.T) string
		wantErr    string
	}{
		{
			name: "missing embedded model",
			body: func(_ *testing.T) string {
				return `<html><script>var opportunity = {};</script></html>`
			},
			wantErr: "page omitted",
		},
		{
			name: "malformed embedded JSON",
			body: func(_ *testing.T) string {
				return `<script>new US.Opportunity.CandidateOpportunityDetail({"Id":);</script>`
			},
			wantErr: "decode new US.Opportunity.CandidateOpportunityDetail JSON",
		},
		{
			name: "response ID changed",
			body: func(t *testing.T) string {
				detail := testUKGOpportunity(2)
				detail.Description = "Full description"
				return ukgDetailPage(t, detail)
			},
			wantErr: "response Id is",
		},
		{
			name: "response ID malformed",
			body: func(t *testing.T) string {
				detail := testUKGOpportunity(1)
				detail.ID = "broken"
				detail.Description = "Full description"
				return ukgDetailPage(t, detail)
			},
			wantErr: "invalid response Id",
		},
		{
			name: "title omitted",
			body: func(t *testing.T) string {
				detail := testUKGOpportunity(1)
				detail.Title = " "
				detail.Description = "Full description"
				return ukgDetailPage(t, detail)
			},
			wantErr: "missing Title",
		},
		{
			name: "employment schema omitted",
			body: func(t *testing.T) string {
				detail := testUKGOpportunity(1)
				detail.FullTime = nil
				detail.Description = "Full description"
				return ukgDetailPage(t, detail)
			},
			wantErr: "omitted FullTime",
		},
		{
			name: "closed state omitted",
			body: func(t *testing.T) string {
				detail := testUKGOpportunity(1)
				detail.OpportunityClosed = nil
				detail.Description = "Full description"
				return ukgDetailPage(t, detail)
			},
			wantErr: "omitted OpportunityIsClosed",
		},
		{
			name: "closed after listing",
			body: func(t *testing.T) string {
				detail := testUKGOpportunity(1)
				detail.OpportunityClosed = ukgBool(true)
				detail.Description = "Full description"
				return ukgDetailPage(t, detail)
			},
			wantErr: "closed before detail fetch",
		},
		{
			name: "description omitted",
			body: func(t *testing.T) string {
				return ukgDetailPage(t, testUKGOpportunity(1))
			},
			wantErr: "missing Description",
		},
		{
			name:       "HTTP failure",
			statusCode: http.StatusServiceUnavailable,
			body: func(_ *testing.T) string {
				return "maintenance"
			},
			wantErr: "503 Service Unavailable",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			responseBody := test.body(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				status := test.statusCode
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				fmt.Fprint(w, responseBody)
			}))
			defer server.Close()

			src := testUKGSource(server)
			job := model.Job{ID: src.jobID(requestedID)}
			err := src.Detail(context.Background(), &job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Detail error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestParseUKGDetailUsesCompleteJSONValue(t *testing.T) {
	t.Parallel()

	detail := testUKGOpportunity(9)
	detail.Description = `Text containing }); and {"nested":"looking"}`
	page := []byte("prefix new US.Opportunity.CandidateOpportunityDetail(" +
		mustUKGJSON(t, detail) + "); trailing JavaScript")
	got, err := parseUKGDetail(page)
	if err != nil {
		t.Fatalf("parseUKGDetail: %v", err)
	}
	if got.ID != detail.ID || got.Description != detail.Description {
		t.Fatalf("parsed detail = %#v", got)
	}
}

func testUKGSource(server *httptest.Server) *ukg {
	return &ukg{
		company: "AppLogic Networks",
		host:    "recruiting.example.test",
		tenant:  "PRO1053PROC",
		board:   testUKGBoard,
		baseURL: server.URL,
		client:  server.Client(),
	}
}

func testUKGOpportunity(index int) ukgOpportunity {
	return ukgOpportunity{
		ID:                testUKGUUID(index),
		Title:             fmt.Sprintf("Role %d", index),
		RequisitionNumber: fmt.Sprintf("ROLE%06d", index),
		FullTime:          ukgBool(true),
		OpportunityClosed: ukgBool(false),
		PostedDate:        "2026-07-30T09:10:11.123Z",
		Locations: []ukgLocation{{
			LocalizedDescription: fmt.Sprintf("Office %d", index),
		}},
	}
}

func testUKGUUID(index int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", index)
}

func ukgBool(value bool) *bool { return &value }

func writeUKGJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode JSON response: %v", err)
	}
}

func ukgDetailPage(t *testing.T, detail ukgOpportunity) string {
	t.Helper()
	return "<script>new US.Opportunity.CandidateOpportunityDetail(" + mustUKGJSON(t, detail) + ");</script>"
}

func mustUKGJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}
