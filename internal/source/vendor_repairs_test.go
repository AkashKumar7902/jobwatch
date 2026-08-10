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
)

func TestOracleCESparseCursorWindows(t *testing.T) {
	var offsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finder := r.URL.Query().Get("finder")
		switch {
		case strings.Contains(finder, "offset=0"):
			offsets = append(offsets, "0")
			fmt.Fprint(w, `{"items":[{"TotalJobsCount":201,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"1","Title":"One"},{"Id":"2","Title":"Two"}]}]}`)
		case strings.Contains(finder, "offset=100"):
			offsets = append(offsets, "100")
			fmt.Fprint(w, `{"items":[{"TotalJobsCount":201,"Offset":100,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"3","Title":"Three"}]}]}`)
		case strings.Contains(finder, "offset=200"):
			offsets = append(offsets, "200")
			fmt.Fprint(w, `{"items":[{"TotalJobsCount":201,"Offset":200,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"4","Title":"Four"}]}]}`)
		default:
			t.Errorf("unexpected finder %q", finder)
		}
	}))
	defer server.Close()

	src := oracleCETestSource(server, 3)
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(offsets, ","); got != "0,100,200" {
		t.Fatalf("requested offsets %s", got)
	}
	if len(jobs) != 4 || jobs[3].ID != "oraclece/acmepod/CX/4" {
		t.Fatalf("unexpected sparse-window jobs: %+v", jobs)
	}
}

func TestOracleCECursorValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "zero board", body: `{"items":[{"TotalJobsCount":0,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[]}]}`},
		{name: "positive total without visible jobs", body: `{"items":[{"TotalJobsCount":1,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[]}]}`, wantErr: "positive total 1 but returned no visible postings"},
		{name: "zero limit", body: `{"items":[{"TotalJobsCount":1,"Offset":0,"Limit":0,"SiteNumber":"CX","requisitionList":[]}]}`, wantErr: "invalid response limit"},
		{name: "oversized limit", body: `{"items":[{"TotalJobsCount":1,"Offset":0,"Limit":101,"SiteNumber":"CX","requisitionList":[]}]}`, wantErr: "invalid response limit"},
		{name: "final window overflow", body: `{"items":[{"TotalJobsCount":2,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"1","Title":"One"},{"Id":"2","Title":"Two"},{"Id":"3","Title":"Three"}]}]}`, wantErr: "cursor window 2"},
		{name: "empty nonfinal", body: `{"items":[{"TotalJobsCount":101,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[]}]}`, wantErr: "empty page before"},
		{name: "malformed row", body: `{"items":[{"TotalJobsCount":1,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"1"}]}]}`, wantErr: "missing Id or Title"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, test.body) }))
			defer server.Close()
			jobs, err := oracleCETestSource(server, 1).Fetch(context.Background())
			if test.wantErr == "" {
				if err != nil || len(jobs) != 0 {
					t.Fatalf("jobs=%v err=%v", jobs, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("got %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestOracleCEAllowsSparseEmptyFinalWindowAfterVisibleRows(t *testing.T) {
	var offsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finder := r.URL.Query().Get("finder")
		if strings.Contains(finder, "offset=100") {
			offsets = append(offsets, "100")
			fmt.Fprint(w, `{"items":[{"TotalJobsCount":101,"Offset":100,"Limit":100,"SiteNumber":"CX","requisitionList":[]}]}`)
			return
		}
		offsets = append(offsets, "0")
		fmt.Fprint(w, `{"items":[{"TotalJobsCount":101,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"1","Title":"One"}]}]}`)
	}))
	defer server.Close()

	jobs, err := oracleCETestSource(server, 2).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(offsets, ","); got != "0,100" {
		t.Fatalf("requested offsets %s", got)
	}
	if len(jobs) != 1 || jobs[0].ID != "oraclece/acmepod/CX/1" {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestOracleCEDuplicateAcrossCorrectWindows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("finder"), "offset=100") {
			fmt.Fprint(w, `{"items":[{"TotalJobsCount":101,"Offset":100,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"1","Title":"One"}]}]}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"TotalJobsCount":101,"Offset":0,"Limit":100,"SiteNumber":"CX","requisitionList":[{"Id":"1","Title":"One"}]}]}`)
	}))
	defer server.Close()
	if _, err := oracleCETestSource(server, 2).Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "duplicate requisition") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func oracleCETestSource(server *httptest.Server, maxPages int) *oracleCE {
	return &oracleCE{
		company: "Acme", host: "acmepod.fa.us2.oraclecloud.com", site: "CX", base: server.URL,
		keyPrefix: "oraclece/acmepod/CX/", maxPages: maxPages, client: server.Client(),
	}
}

func TestEnphaseCanonicalAliasesConvergeAndAggregate(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeEnphasePage(t, w, 2, 1, 2,
			enphaseTestRow("oOne-zurich", "oOne", "Zurich"),
			enphaseTestRow("oOne-austin", "oOne", "Austin"),
		)
	}))
	defer server.Close()
	jobs, err := (&enphase{company: "Enphase", base: server.URL, maxPages: 3, client: server.Client()}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || len(jobs) != 1 || jobs[0].ID != "enphase/oOne" || jobs[0].Location != "Austin; Zurich" {
		t.Fatalf("requests=%d jobs=%+v", requests.Load(), jobs)
	}
}

func TestEnphasePagerDriftThenTwoMatchingSnapshots(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := requests.Add(1)
		if call == 1 {
			writeEnphasePage(t, w, 2, 1, 2, enphaseTestRow("oOne", "oOne", "Austin"))
			return
		}
		writeEnphasePage(t, w, 2, 1, 2,
			enphaseTestRow("oOne-austin", "oOne", "Austin"),
			enphaseTestRow("oOne-zurich", "oOne", "Zurich"),
		)
	}))
	defer server.Close()
	jobs, err := (&enphase{company: "Enphase", base: server.URL, maxPages: 3, client: server.Client()}).Fetch(context.Background())
	if err != nil || requests.Load() != 3 || len(jobs) != 1 {
		t.Fatalf("requests=%d jobs=%v err=%v", requests.Load(), jobs, err)
	}
}

func TestEnphaseAliasesThenStableSingletonConverges(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := requests.Add(1)
		if call == 1 {
			writeEnphasePage(t, w, 2, 1, 2,
				enphaseTestRow("oOne-austin", "oOne", "Austin"),
				enphaseTestRow("oOne-zurich", "oOne", "Zurich"),
			)
			return
		}
		writeEnphasePage(t, w, 1, 1, 1, enphaseTestRow("oOne", "oOne", "Austin"))
	}))
	defer server.Close()

	jobs, err := (&enphase{company: "Enphase", base: server.URL, maxPages: 3, client: server.Client()}).Fetch(context.Background())
	if err != nil || requests.Load() != 3 || len(jobs) != 1 || jobs[0].Location != "Austin" {
		t.Fatalf("requests=%d jobs=%+v err=%v", requests.Load(), jobs, err)
	}
}

func TestEnphaseRejectsThreeDifferentDuplicateSnapshots(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := requests.Add(1)
		location := "Location " + strconv.Itoa(int(call))
		writeEnphasePage(t, w, 2, 1, 2,
			enphaseTestRow("oOne-a", "oOne", "Austin"),
			enphaseTestRow("oOne-b", "oOne", location),
		)
	}))
	defer server.Close()
	_, err := (&enphase{company: "Enphase", base: server.URL, maxPages: 3, client: server.Client()}).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not stabilize after 3 attempts") || requests.Load() != 3 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
}

func TestEnphaseConflictingCanonicalRowsFailImmediately(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		second := enphaseTestRow("oOne-b", "oOne", "Zurich")
		second.Name = "Conflicting title"
		writeEnphasePage(t, w, 2, 1, 2, enphaseTestRow("oOne-a", "oOne", "Austin"), second)
	}))
	defer server.Close()
	_, err := (&enphase{company: "Enphase", base: server.URL, maxPages: 3, client: server.Client()}).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "conflicting rows") || requests.Load() != 1 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
}

func TestEnphaseSchemaAndCapErrorsAreImmediate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*enphaseRow)
		pages   int
		size    int
		total   int
		max     int
		wantErr string
	}{
		{name: "missing requisition", mutate: func(r *enphaseRow) { r.Requisition = "" }, total: 1, pages: 1, size: 1, max: 3, wantErr: "invalid requisitionid"},
		{name: "nonnumeric requisition", mutate: func(r *enphaseRow) { r.Requisition = "R-1" }, total: 1, pages: 1, size: 1, max: 3, wantErr: "invalid requisitionid"},
		{name: "unsafe j", mutate: func(r *enphaseRow) { r.ApplyURL = "https://app.jobvite.com/x?j=bad%2Fid" }, total: 1, pages: 1, size: 1, max: 3, wantErr: "safe nonempty j"},
		{name: "page cap", total: 2, pages: 2, size: 1, max: 1, wantErr: "exceeds max_pages"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				row := enphaseTestRow("oOne", "oOne", "Austin")
				if test.mutate != nil {
					test.mutate(&row)
				}
				writeEnphasePage(t, w, test.total, test.pages, test.size, row)
			}))
			defer server.Close()
			_, err := (&enphase{company: "Enphase", base: server.URL, maxPages: test.max, client: server.Client()}).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) || requests.Load() != 1 {
				t.Fatalf("requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestNormalizeEnphaseApplyURLAliasesAndSafety(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		jid     string
		wantJ   string
		wantErr bool
	}{
		{name: "canonical", raw: "http://app.jobvite.com/x?k=Apply&j=oOne#fragment", jid: "oOne", wantJ: "oOne"},
		{name: "safe suffix", raw: "https://app.jobvite.com/x?j=oOne", jid: "oOne-location_2", wantJ: "oOne"},
		{name: "empty suffix", raw: "https://app.jobvite.com/x?j=oOne", jid: "oOne-", wantErr: true},
		{name: "unsafe suffix", raw: "https://app.jobvite.com/x?j=oOne", jid: "oOne-place/2", wantErr: true},
		{name: "duplicate j", raw: "https://app.jobvite.com/x?j=oOne&j=oTwo", jid: "oOne", wantErr: true},
		{name: "foreign host", raw: "https://example.com/x?j=oOne", jid: "oOne", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotURL, gotJ, err := normalizeEnphaseApplyURL(test.raw, test.jid)
			if (err != nil) != test.wantErr {
				t.Fatalf("url=%q j=%q err=%v", gotURL, gotJ, err)
			}
			if !test.wantErr && (gotJ != test.wantJ || !strings.HasPrefix(gotURL, "https://app.jobvite.com/") || strings.Contains(gotURL, "#")) {
				t.Fatalf("url=%q j=%q", gotURL, gotJ)
			}
		})
	}
}

func TestEnphaseContextErrorIsImmediate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&enphase{company: "Enphase", base: "https://enphase.invalid", maxPages: 3, client: http.DefaultClient}).Fetch(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("got %v", err)
	}
}

func TestEnphaseHTTPErrorIsImmediate(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, err := (&enphase{company: "Enphase", base: server.URL, maxPages: 3, client: server.Client()}).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") || requests.Load() != 1 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
}

func enphaseTestRow(jid, jobviteID, location string) enphaseRow {
	return enphaseRow{
		JID: jid, Name: "Power Engineer", Category: "Engineering",
		ApplyURL:    "http://app.jobvite.com/CompanyJobs/Careers.aspx?c=abc&j=" + jobviteID + "&k=Apply",
		Description: "<p>Build clean energy.</p>", Location: location, Requisition: "9001",
	}
}

func writeEnphasePage(t *testing.T, w http.ResponseWriter, total, pages, size int, rows ...enphaseRow) {
	t.Helper()
	response := map[string]any{
		"rows": rows,
		"pager": map[string]any{
			"current_page": 0, "total_items": total, "total_pages": pages, "items_per_page": size,
		},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Error(err)
	}
}
