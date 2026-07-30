package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"jobwatch/internal/params"
)

func TestGoogleRegistrationIdentityAndParams(t *testing.T) {
	t.Parallel()

	src, err := New("google", "Google India", params.Map{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("source type = %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*googleIndia)
	if !ok {
		t.Fatalf("wrapped source type = %T, want *googleIndia", wrapped.Source)
	}
	if implementation.endpoint != googleRPCURL || implementation.client != http.DefaultClient {
		t.Errorf("implementation = %#v", implementation)
	}
	if Identity(src) != "google/IN" || StatePrefix(src) != "google/" {
		t.Errorf("identity/prefix = %q/%q", Identity(src), StatePrefix(src))
	}

	_, err = New("google", "Google", params.Map{"z": "1", "a": "2"}, nil)
	if err == nil || !strings.Contains(err.Error(), "got a, z") {
		t.Fatalf("unknown params error = %v, want sorted names", err)
	}
}

func TestGoogleFetchesCompleteStableSnapshot(t *testing.T) {
	t.Parallel()

	const total = googleRPCPageSize + 1
	records := make([]json.RawMessage, total)
	for index := range records {
		id := fmt.Sprintf("%017d", 10_000_000_000_000_000+index)
		locations := []any{
			[]any{"Bengaluru, Karnataka, India", []string{"Bengaluru, India"}, "Bengaluru", nil, "KA", "IN"},
		}
		if index == 0 {
			locations = []any{
				[]any{"London, UK", []string{"London, UK"}, "London", nil, "London", "GB"},
				[]any{"Bengaluru, Karnataka, India", []string{"Bengaluru, India"}, "Bengaluru", nil, "KA", "IN"},
			}
		}
		records[index] = mustGoogleJSON(t, googleTestJob(id, fmt.Sprintf("Engineer %d", index), locations))
	}

	var requests []int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := googleTestRequestPage(t, r)
		requests = append(requests, page)
		var pageRecords []json.RawMessage
		switch page {
		case 1:
			pageRecords = records[:googleRPCPageSize]
		case 2:
			pageRecords = records[googleRPCPageSize:]
		default:
			t.Fatalf("unexpected page %d", page)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, googleTestRPCResponse(t, pageRecords, total, googleRPCPageSize))
	}))
	defer server.Close()

	src := &googleIndia{
		company: "Google India", endpoint: server.URL + googleTestRPCQuery(),
		client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(requests, []int{1, 2, 1, 2}) {
		t.Fatalf("requested pages = %v, want [1 2 1 2]", requests)
	}
	if len(jobs) != total {
		t.Fatalf("jobs = %d, want %d", len(jobs), total)
	}
	first := jobs[0]
	if first.ID != "google/10000000000000000" ||
		first.Company != "Google India" ||
		first.Title != "Engineer 0" ||
		first.Location != "London, UK; Bengaluru, Karnataka, India" ||
		first.URL != googlePublicJobBase+"10000000000000000" ||
		!strings.Contains(first.Description, "About the job\nBuild useful products.") ||
		!strings.Contains(first.Description, "Minimum qualifications:") ||
		!strings.Contains(first.Description, "Responsibilities\nShip reliable systems.") ||
		!strings.Contains(first.Description, "India is an eligible location.") {
		t.Errorf("first job = %#v", first)
	}
	if !first.PostedAt.IsZero() {
		t.Errorf("PostedAt = %v, want zero for undocumented timestamps", first.PostedAt)
	}
	if first.EmploymentType != "" {
		t.Errorf("EmploymentType = %q, want empty", first.EmploymentType)
	}
}

func TestGoogleRequestIsAnonymousAndExactlyScoped(t *testing.T) {
	t.Parallel()

	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if page := googleTestRequestPage(t, r); page != 1 {
			t.Errorf("page = %d, want 1", page)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("request unexpectedly carried auth state")
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded;charset=UTF-8" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "jobwatch") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		record := mustGoogleJSON(t, googleTestJob(
			"10000000000000000", "Engineer",
			[]any{[]any{"India", []string{"India"}, "", nil, "", "IN"}},
		))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, googleTestRPCResponse(t, []json.RawMessage{record}, 1, googleRPCPageSize))
	}))
	defer server.Close()

	src := &googleIndia{company: "Google India", endpoint: server.URL + googleTestRPCQuery(), client: server.Client()}
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("server was not called")
	}
}

func TestGoogleRejectsMalformedJobs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func([]any)
		wantErr string
	}{
		{"short row", func([]any) {}, "20 to 64"},
		{"job ID", func(row []any) { row[0] = "abc" }, "invalid job ID"},
		{"title", func(row []any) { row[1] = "" }, "invalid title"},
		{"application host", func(row []any) {
			row[2] = "https://evil.example/about/careers/applications/signin?jobId=" +
				strings.Repeat("A", 80) + "&loc=IN&title=Engineer"
		}, "trusted Google"},
		{"application query", func(row []any) {
			row[2] = "https://www.google.com/about/careers/applications/signin?jobId=" +
				strings.Repeat("A", 80) + "&loc=IN&title=Engineer&next=https://evil.example"
		}, "unexpected query"},
		{"company resource", func(row []any) { row[5] = "projects/other" }, "company resource"},
		{"affiliate", func(row []any) { row[7] = "DeepMind" }, "want Google"},
		{"locale", func(row []any) { row[8] = "de-DE" }, "want en-US"},
		{"no India location", func(row []any) {
			row[9] = []any{[]any{"London, UK", []string{"London"}, "London", nil, "", "GB"}}
		}, "no location is in India"},
		{"responsibilities", func(row []any) { row[3] = []any{nil, ""} }, "responsibilities field is empty"},
		{"qualifications shape", func(row []any) { row[4] = []any{"not-null", "<p>x</p>"} }, "unexpected shape"},
		{"about", func(row []any) { row[10] = []any{nil, "<p></p>"} }, "about field is empty"},
		{"timestamp shape", func(row []any) { row[13] = []any{"today", 0} }, "published timestamp"},
		{"timestamp order", func(row []any) { row[12] = []any{1_800_000_000, 0} }, "out of order"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			row := googleTestJob(
				"10000000000000000", "Engineer",
				[]any{[]any{"Bengaluru, India", []string{"India"}, "Bengaluru", nil, "KA", "IN"}},
			)
			if test.name == "short row" {
				raw := mustGoogleJSON(t, row[:10])
				_, err := (&googleIndia{company: "Google India"}).normalizeJob(raw)
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			test.mutate(row)
			raw := mustGoogleJSON(t, row)
			_, err := (&googleIndia{company: "Google India"}).normalizeJob(raw)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestGoogleRejectsPaginationDriftAndDuplicatesAtomically(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		serve   func(t *testing.T, page, call int) ([]json.RawMessage, int)
		wantErr string
	}{
		{
			name: "short first page",
			serve: func(t *testing.T, page, call int) ([]json.RawMessage, int) {
				return googleTestRecords(t, 0, googleRPCPageSize-1), googleRPCPageSize + 1
			},
			wantErr: "returned 19 jobs, want 20",
		},
		{
			name: "total changes",
			serve: func(t *testing.T, page, call int) ([]json.RawMessage, int) {
				if page == 1 {
					return googleTestRecords(t, 0, googleRPCPageSize), googleRPCPageSize + 1
				}
				return googleTestRecords(t, googleRPCPageSize, 2), googleRPCPageSize + 2
			},
			wantErr: "total changed",
		},
		{
			name: "duplicate across pages",
			serve: func(t *testing.T, page, call int) ([]json.RawMessage, int) {
				if page == 1 {
					return googleTestRecords(t, 0, googleRPCPageSize), googleRPCPageSize + 1
				}
				return googleTestRecords(t, 0, 1), googleRPCPageSize + 1
			},
			wantErr: "duplicate stable job ID",
		},
		{
			name: "page one changes between traversals",
			serve: func(t *testing.T, page, call int) ([]json.RawMessage, int) {
				if page == 1 && call == 3 {
					return googleTestRecords(t, 100, googleRPCPageSize), googleRPCPageSize + 1
				}
				if page == 1 {
					return googleTestRecords(t, 0, googleRPCPageSize), googleRPCPageSize + 1
				}
				return googleTestRecords(t, googleRPCPageSize, 1), googleRPCPageSize + 1
			},
			wantErr: "snapshot changed",
		},
		{
			name: "later pages shift while page one stays fixed",
			serve: func(t *testing.T, page, call int) ([]json.RawMessage, int) {
				const total = googleRPCPageSize*2 + 1
				if call <= 3 {
					switch page {
					case 1:
						return googleTestRecords(t, 0, googleRPCPageSize), total
					case 2:
						return googleTestRecords(t, googleRPCPageSize, googleRPCPageSize), total
					default:
						return googleTestRecords(t, googleRPCPageSize*2, 1), total
					}
				}
				switch page {
				case 1:
					return googleTestRecords(t, 0, googleRPCPageSize), total
				case 2:
					return googleTestRecords(t, googleRPCPageSize+1, googleRPCPageSize), total
				default:
					return googleTestRecords(t, 100, 1), total
				}
			},
			wantErr: "snapshot changed",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			call := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call++
				page := googleTestRequestPage(t, r)
				records, total := test.serve(t, page, call)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, googleTestRPCResponse(t, records, total, googleRPCPageSize))
			}))
			defer server.Close()
			src := &googleIndia{
				company: "Google India", endpoint: server.URL + googleTestRPCQuery(),
				client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch = %d jobs, %v; want %q", len(jobs), err, test.wantErr)
			}
			if jobs != nil {
				t.Fatalf("jobs = %#v, want nil on snapshot failure", jobs)
			}
		})
	}
}

func TestGoogleRPCFramingAndSchemaValidation(t *testing.T) {
	t.Parallel()

	validRecord := mustGoogleJSON(t, googleTestJob(
		"10000000000000000", "Engineer",
		[]any{[]any{"India", []string{"India"}, "", nil, "", "IN"}},
	))
	valid := googleTestRPCResponse(t, []json.RawMessage{validRecord}, 1, googleRPCPageSize)
	inner := googleTestEmbeddedPayload(t, []json.RawMessage{validRecord}, 1, googleRPCPageSize)

	tests := []struct {
		name string
		body string
		want string
	}{
		{"XSSI", strings.TrimPrefix(valid, ")]}'"), "XSSI"},
		{"frame length", googleTestCorruptFirstLength(t, valid), "frame 0 length"},
		{"invalid frame JSON", ")]}'\n\n3\n{\n", "not valid JSON"},
		{"unknown row", googleTestFramed(t, []any{[]any{"mystery", 1}}), "unexpected tag"},
		{"missing RPC", googleTestFramed(t, []any{[]any{"di", 1}}), "omitted"},
		{"wrong RPC", googleTestFramed(t, []any{[]any{"wrb.fr", "other", inner}}), "unexpected ID"},
		{"duplicate RPC", googleTestFramed(t, []any{
			[]any{"wrb.fr", googleRPCID, inner},
			[]any{"wrb.fr", googleRPCID, inner},
		}), "duplicate"},
		{"embedded JSON", googleTestFramed(t, []any{[]any{"wrb.fr", googleRPCID, "{"}}), "embedded payload"},
		{"embedded shape", googleTestFramed(t, []any{[]any{"wrb.fr", googleRPCID, "[]"}}), "want 4"},
		{"page size", googleTestRPCResponse(t, []json.RawMessage{validRecord}, 1, 19), "page size"},
		{"null jobs", googleTestRPCResponseNullJobs(t, 1), "omitted jobs"},
		{"excess total", googleTestRPCResponse(t, []json.RawMessage{validRecord}, googleMaxJobs+1, googleRPCPageSize), "safety limit"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseGoogleRPCPage([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGoogleHTTPBoundaryValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{"status", http.StatusBadGateway, "application/json", "upstream", "502 Bad Gateway"},
		{"content type", http.StatusOK, "text/html", "<html></html>", "unexpected Content-Type"},
		{"body limit", http.StatusOK, "application/json", strings.Repeat("x", googleRPCBodyLimit+1), "safety limit"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			src := &googleIndia{company: "Google India", endpoint: server.URL, client: server.Client()}
			jobs, err := src.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch = %#v, %v; want %q", jobs, err, test.wantErr)
			}
			if jobs != nil {
				t.Fatalf("jobs = %#v, want nil", jobs)
			}
		})
	}
}

func TestGoogleDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls.Add(1)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	src := &googleIndia{company: "Google India", endpoint: redirect.URL, client: redirect.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "302 Found") || jobs != nil {
		t.Fatalf("Fetch = %#v, %v; want redirect rejection", jobs, err)
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetCalls.Load())
	}
}

func TestGoogleRetriesTransientPageFailure(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	record := mustGoogleJSON(t, googleTestJob(
		"10000000000000000", "Engineer",
		[]any{[]any{"India", []string{"India"}, "", nil, "", "IN"}},
	))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		googleTestRequestPage(t, r)
		if calls.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, googleTestRPCResponse(t, []json.RawMessage{record}, 1, googleRPCPageSize))
	}))
	defer server.Close()

	src := &googleIndia{company: "Google India", endpoint: server.URL + googleTestRPCQuery(), client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || calls.Load() != 3 {
		t.Fatalf("jobs/calls = %d/%d, want 1/3", len(jobs), calls.Load())
	}
}

func TestGoogleHonorsRetryAfter429(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	record := mustGoogleJSON(t, googleTestJob(
		"10000000000000000", "Engineer",
		[]any{[]any{"India", []string{"India"}, "", nil, "", "IN"}},
	))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		googleTestRequestPage(t, r)
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, googleTestRPCResponse(t, []json.RawMessage{record}, 1, googleRPCPageSize))
	}))
	defer server.Close()

	src := &googleIndia{company: "Google India", endpoint: server.URL + googleTestRPCQuery(), client: server.Client()}
	started := time.Now()
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("Fetch retried after %s, want Retry-After delay of 1s", elapsed)
	}
	if len(jobs) != 1 || calls.Load() != 3 {
		t.Fatalf("jobs/calls = %d/%d, want 1/3", len(jobs), calls.Load())
	}
}

func TestGoogleRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"60", 60 * time.Second},
		{"600", googleMaxRetryAfter},
		{now.Add(15 * time.Second).Format(http.TimeFormat), 15 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"0", 0},
		{"invalid", 0},
	}
	for _, test := range tests {
		if got := googleRetryAfter(test.value, now); got != test.want {
			t.Errorf("googleRetryAfter(%q) = %s, want %s", test.value, got, test.want)
		}
	}
}

func googleTestRequestPage(t *testing.T, r *http.Request) int {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.Method)
	}
	query := r.URL.Query()
	if query.Get("rpcids") != googleRPCID ||
		query.Get("source-path") != "/about/careers/applications/jobs/results/" ||
		query.Get("hl") != "en-IN" || query.Get("rt") != "c" || len(query) != 4 {
		t.Errorf("endpoint query = %v", query)
	}
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	if len(r.PostForm) != 1 || len(r.PostForm["f.req"]) != 1 {
		t.Fatalf("form = %v", r.PostForm)
	}
	var batch [][][]json.RawMessage
	if err := json.Unmarshal([]byte(r.PostForm.Get("f.req")), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || len(batch[0]) != 1 || len(batch[0][0]) != 4 {
		t.Fatalf("batch shape = %#v", batch)
	}
	var rpcID, encodedQuery, mode string
	if err := json.Unmarshal(batch[0][0][0], &rpcID); err != nil ||
		json.Unmarshal(batch[0][0][1], &encodedQuery) != nil ||
		json.Unmarshal(batch[0][0][3], &mode) != nil ||
		rpcID != googleRPCID || mode != "generic" ||
		!strings.EqualFold(string(bytesTrimSpace(batch[0][0][2])), "null") {
		t.Fatalf("batch call = %s", r.PostForm.Get("f.req"))
	}
	var inputs [][]json.RawMessage
	if err := json.Unmarshal([]byte(encodedQuery), &inputs); err != nil ||
		len(inputs) != 1 || len(inputs[0]) != 8 {
		t.Fatalf("query input = %s", encodedQuery)
	}
	if string(inputs[0][0]) != "null" ||
		string(inputs[0][1]) != `["Google"]` ||
		string(inputs[0][2]) != "null" ||
		string(inputs[0][3]) != "null" ||
		string(inputs[0][4]) != `"en-GB"` ||
		string(inputs[0][5]) != "null" ||
		string(inputs[0][6]) != `[["India"]]` {
		t.Fatalf("query input = %s", encodedQuery)
	}
	var page int
	if err := json.Unmarshal(inputs[0][7], &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func googleTestRPCQuery() string {
	return "?rpcids=r06xKb&source-path=%2Fabout%2Fcareers%2Fapplications%2Fjobs%2Fresults%2F&hl=en-IN&rt=c"
}

func googleTestJob(id, title string, locations []any) []any {
	row := make([]any, 21)
	applyQuery := url.Values{
		"jobId": {strings.Repeat("A", 100) + "==_V2"},
		"loc":   {"IN"},
		"title": {title},
	}
	row[0] = id
	row[1] = title
	row[2] = "https://www.google.com/about/careers/applications/signin?" + applyQuery.Encode()
	row[3] = []any{nil, "<ul><li>Ship reliable systems.</li></ul>"}
	row[4] = []any{nil, "<h3>Minimum qualifications:</h3><ul><li>One year of experience.</li></ul>"}
	row[5] = googleCompanyResource
	row[7] = "Google"
	row[8] = "en-US"
	row[9] = locations
	row[10] = []any{nil, "<p>Build useful products.</p>"}
	row[11] = []any{2}
	row[12] = []any{1_700_000_000, 0}
	row[13] = []any{1_700_000_100, 0}
	row[14] = []any{1_700_000_200, 0}
	row[15] = []any{nil, ""}
	row[18] = []any{nil, "India is an eligible location."}
	row[19] = []any{nil, "<ul><li>One year of experience.</li></ul>"}
	row[20] = 1
	return row
}

func googleTestRecords(t *testing.T, start, count int) []json.RawMessage {
	t.Helper()
	records := make([]json.RawMessage, count)
	for index := range records {
		id := fmt.Sprintf("%017d", 10_000_000_000_000_000+start+index)
		records[index] = mustGoogleJSON(t, googleTestJob(
			id, fmt.Sprintf("Engineer %d", start+index),
			[]any{[]any{"India", []string{"India"}, "", nil, "", "IN"}},
		))
	}
	return records
}

func googleTestRPCResponse(
	t *testing.T, records []json.RawMessage, total, pageSize int,
) string {
	t.Helper()
	inner := googleTestEmbeddedPayload(t, records, total, pageSize)
	return googleTestFramed(t,
		[]any{
			[]any{"wrb.fr", googleRPCID, inner, nil, nil, nil},
			[]any{"di", 177},
			[]any{"af.httprm", 176, "opaque", 40},
		},
		[]any{[]any{"e", 4, nil, nil, 100}},
	)
}

func googleTestRPCResponseNullJobs(t *testing.T, total int) string {
	t.Helper()
	payload, err := json.Marshal([]any{nil, nil, total, googleRPCPageSize})
	if err != nil {
		t.Fatal(err)
	}
	return googleTestFramed(t, []any{[]any{"wrb.fr", googleRPCID, string(payload)}})
}

func googleTestEmbeddedPayload(
	t *testing.T, records []json.RawMessage, total, pageSize int,
) string {
	t.Helper()
	payload, err := json.Marshal([]any{records, nil, total, pageSize})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func googleTestFramed(t *testing.T, frameValues ...any) string {
	t.Helper()
	var builder strings.Builder
	builder.WriteString(")]}'\n\n")
	for _, value := range frameValues {
		frame, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&builder, "%d\n%s\n", utf8.RuneCount(frame)+2, frame)
	}
	return builder.String()
}

func mustGoogleJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func bytesTrimSpace(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

func googleTestCorruptFirstLength(t *testing.T, response string) string {
	t.Helper()
	const prefix = ")]}'\n\n"
	if !strings.HasPrefix(response, prefix) {
		t.Fatal("test response lacks prefix")
	}
	rest := response[len(prefix):]
	newline := strings.IndexByte(rest, '\n')
	if newline < 0 {
		t.Fatal("test response lacks frame length")
	}
	return prefix + "1" + rest[newline:]
}
