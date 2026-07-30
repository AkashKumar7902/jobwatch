package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestIBMRegistrationIdentityRequestPaginationAndDetailInList(t *testing.T) {
	var offsets []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/search/api/v1/ibmcom/appid/careers/responseFormat/json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent is empty")
		}
		query := r.URL.Query()
		if len(query) != 7 ||
			query.Get("appid") != "careers" ||
			query.Get("scope") != "careers2" ||
			query.Get("rc") != "in" ||
			query.Get("rmdt") != "ALL" ||
			query.Get("query") != "" ||
			query.Get("nr") != strconv.Itoa(ibmPageSize) {
			t.Errorf("query = %v", query)
		}
		offset, err := strconv.Atoi(query.Get("fr"))
		if err != nil {
			t.Fatal(err)
		}
		offsets = append(offsets, offset)
		var results []ibmSearchResult
		pageCount := min(ibmPageSize, 32-offset)
		for index := 0; index < pageCount; index++ {
			results = append(results, ibmFixtureResult(strconv.Itoa(120000+offset+index)))
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(ibmFixturePage(32, offset, results...)); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	settings := params.Map{"appid": "careers", "scope": "careers2", "rc": "in"}
	src, err := New("ibm", "IBM India", settings, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	other, err := New("ibm", "IBM India", params.Map{"appid": "careers", "scope": "careers2", "rc": "us"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if Identity(src) == Identity(other) {
		t.Fatalf("rc did not contribute to identity %q", Identity(src))
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("New returned %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*ibm)
	if !ok {
		t.Fatalf("wrapped source = %T, want *ibm", wrapped.Source)
	}
	implementation.apiBase = server.URL

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(offsets, []int{0, 30}) {
		t.Fatalf("offsets = %v, want [0 30]", offsets)
	}
	if len(jobs) != 32 {
		t.Fatalf("len(jobs) = %d, want 32", len(jobs))
	}
	wantFirst := model.Job{
		ID:          "ibm/careers/careers2/in/120000",
		Company:     "IBM India",
		Title:       "Software Engineer 120000",
		Location:    "Bangalore, IN",
		URL:         "https://careers.ibm.com/careers/JobDetail?jobId=120000",
		Description: "Build & ship.\nUse Go.",
		PostedAt:    time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(jobs[0], wantFirst) {
		t.Fatalf("first job =\n%+v\nwant\n%+v", jobs[0], wantFirst)
	}
	if jobs[31].ID != "ibm/careers/careers2/in/120031" {
		t.Fatalf("last job ID = %q", jobs[31].ID)
	}
}

func TestIBMAllowsAnEmptyCompleteBoard(t *testing.T) {
	jobs, err := ibmFetchFixture(t, func(_ int) ibmSearchResponse {
		return ibmFixturePage(0, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
	}
}

func TestIBMRejectsInvalidParams(t *testing.T) {
	tests := []struct {
		name    string
		params  params.Map
		wantErr string
	}{
		{name: "missing appid", params: params.Map{"scope": "careers2", "rc": "in"}, wantErr: `missing required param "appid"`},
		{name: "invalid appid", params: params.Map{"appid": "careers/us", "scope": "careers2", "rc": "in"}, wantErr: "IBM collection identifier"},
		{name: "missing scope", params: params.Map{"appid": "careers", "rc": "in"}, wantErr: `missing required param "scope"`},
		{name: "invalid scope", params: params.Map{"appid": "careers", "scope": " careers2", "rc": "in"}, wantErr: "IBM collection identifier"},
		{name: "missing rc", params: params.Map{"appid": "careers", "scope": "careers2"}, wantErr: `missing required param "rc"`},
		{name: "uppercase rc", params: params.Map{"appid": "careers", "scope": "careers2", "rc": "IN"}, wantErr: "lowercase two-letter"},
		{name: "long rc", params: params.Map{"appid": "careers", "scope": "careers2", "rc": "ind"}, wantErr: "lowercase two-letter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New("ibm", "IBM", test.params, nil)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestIBMRejectsIncompleteOrDriftedPages(t *testing.T) {
	fullFirstPage := make([]ibmSearchResult, ibmPageSize)
	for index := range fullFirstPage {
		fullFirstPage[index] = ibmFixtureResult(strconv.Itoa(200000 + index))
	}
	tests := []struct {
		name    string
		page    func(int) ibmSearchResponse
		wantErr string
	}{
		{
			name: "missing resultset",
			page: func(_ int) ibmSearchResponse {
				return ibmSearchResponse{}
			},
			wantErr: "omitted resultset",
		},
		{
			name: "missing searchquery",
			page: func(_ int) ibmSearchResponse {
				response := ibmFixturePage(0, 0)
				response.ResultSet.SearchQuery = nil
				return response
			},
			wantErr: "omitted resultset",
		},
		{
			name: "changed appid",
			page: func(_ int) ibmSearchResponse {
				response := ibmFixturePage(0, 0)
				response.ResultSet.SearchQuery.AppID = ibmString("other")
				return response
			},
			wantErr: `reported appid "other"`,
		},
		{
			name: "missing total",
			page: func(_ int) ibmSearchResponse {
				response := ibmFixturePage(0, 0)
				response.ResultSet.SearchResults.TotalResults = nil
				return response
			},
			wantErr: "omitted totalresults",
		},
		{
			name: "negative total",
			page: func(_ int) ibmSearchResponse {
				response := ibmFixturePage(0, 0)
				response.ResultSet.SearchResults.TotalResults = ibmInt(-1)
				return response
			},
			wantErr: "negative totalresults",
		},
		{
			name: "wrong start index",
			page: func(_ int) ibmSearchResponse {
				response := ibmFixturePage(0, 0)
				response.ResultSet.SearchResults.StartIndex = ibmInt(1)
				return response
			},
			wantErr: "reported startindex 1",
		},
		{
			name: "numresults mismatch",
			page: func(_ int) ibmSearchResponse {
				response := ibmFixturePage(1, 0, ibmFixtureResult("200001"))
				response.ResultSet.SearchResults.NumResults = ibmInt(0)
				return response
			},
			wantErr: "reported numresults 0 but returned 1",
		},
		{
			name: "short page",
			page: func(_ int) ibmSearchResponse {
				return ibmFixturePage(2, 0, ibmFixtureResult("200001"))
			},
			wantErr: "returned 1 results, want 2",
		},
		{
			name: "page too large",
			page: func(_ int) ibmSearchResponse {
				results := append([]ibmSearchResult(nil), fullFirstPage...)
				results = append(results, ibmFixtureResult("200030"))
				return ibmFixturePage(31, 0, results...)
			},
			wantErr: "page size is 30",
		},
		{
			name: "wrong resultnum",
			page: func(_ int) ibmSearchResponse {
				response := ibmFixturePage(1, 0, ibmFixtureResult("200001"))
				(*response.ResultSet.SearchResults.SearchResultList)[0].ResultNum = ibmInt(7)
				return response
			},
			wantErr: "reported resultnum 7",
		},
		{
			name: "total drift",
			page: func(offset int) ibmSearchResponse {
				if offset == 0 {
					return ibmFixturePage(31, 0, fullFirstPage...)
				}
				return ibmFixturePage(32, offset, ibmFixtureResult("200030"), ibmFixtureResult("200031"))
			},
			wantErr: "totalresults changed from 31 to 32",
		},
		{
			name: "duplicate across pages",
			page: func(offset int) ibmSearchResponse {
				if offset == 0 {
					return ibmFixturePage(31, 0, fullFirstPage...)
				}
				return ibmFixturePage(31, offset, ibmFixtureResult("200000"))
			},
			wantErr: `duplicate job id "200000"`,
		},
		{
			name: "total exceeds hard limit",
			page: func(_ int) ibmSearchResponse {
				return ibmFixturePage(ibmMaxJobs+1, 0, fullFirstPage...)
			},
			wantErr: "exceeds hard limit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ibmFetchFixture(t, test.page)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestIBMRejectsInvalidPostingSchema(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ibmSearchResult)
		wantErr string
	}{
		{name: "missing docattributes", mutate: func(result *ibmSearchResult) { result.DocAttributes = nil }, wantErr: "omitted docattributes"},
		{name: "missing job id", mutate: func(result *ibmSearchResult) { ibmDeleteAttribute(result, "field_text_01") }, wantErr: `omitted "field_text_01"`},
		{name: "duplicate job id", mutate: func(result *ibmSearchResult) { ibmAppendAttribute(result, "field_text_01", "300001") }, wantErr: `duplicate "field_text_01"`},
		{name: "non-string job id", mutate: func(result *ibmSearchResult) { ibmSetRawAttribute(result, "field_text_01", `123`) }, wantErr: "is not a string"},
		{name: "unsafe job id", mutate: func(result *ibmSearchResult) { ibmSetAttribute(result, "field_text_01", "../300001") }, wantErr: "invalid field_text_01"},
		{name: "empty title", mutate: func(result *ibmSearchResult) { result.Title = " " }, wantErr: "empty title"},
		{name: "wrong country", mutate: func(result *ibmSearchResult) { ibmSetAttribute(result, "country", "us") }, wantErr: `country "us", want "in"`},
		{name: "wrong scope", mutate: func(result *ibmSearchResult) { ibmSetAttribute(result, "scope", "other") }, wantErr: `scope "other", want "careers2"`},
		{name: "empty location", mutate: func(result *ibmSearchResult) { ibmSetAttribute(result, "field_keyword_19", " ") }, wantErr: "empty field_keyword_19"},
		{name: "empty description", mutate: func(result *ibmSearchResult) { ibmSetAttribute(result, "raw_body", "<p> </p>") }, wantErr: "empty raw_body"},
		{name: "invalid date", mutate: func(result *ibmSearchResult) { ibmSetAttribute(result, "dcdate", "29-07-2026") }, wantErr: "invalid dcdate"},
		{name: "wrong URL host", mutate: func(result *ibmSearchResult) { result.URL = "https://example.com/careers/JobDetail?jobId=300001" }, wantErr: "invalid URL"},
		{name: "URL job id mismatch", mutate: func(result *ibmSearchResult) { result.URL = "https://careers.ibm.com/careers/JobDetail?jobId=999999" }, wantErr: "invalid URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := ibmFixtureResult("300001")
			test.mutate(&result)
			_, err := ibmFetchFixture(t, func(_ int) ibmSearchResponse {
				return ibmFixturePage(1, 0, result)
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func ibmFetchFixture(t *testing.T, page func(int) ibmSearchResponse) ([]model.Job, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, err := strconv.Atoi(r.URL.Query().Get("fr"))
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(page(offset)); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()
	src := &ibm{
		company: "IBM India",
		appID:   "careers",
		scope:   "careers2",
		rc:      "in",
		apiBase: server.URL,
		client:  server.Client(),
	}
	return src.Fetch(context.Background())
}

func ibmFixturePage(total, start int, results ...ibmSearchResult) ibmSearchResponse {
	if results == nil {
		results = make([]ibmSearchResult, 0)
	}
	for index := range results {
		results[index].ResultNum = ibmInt(index)
	}
	return ibmSearchResponse{
		ResultSet: &ibmResultSet{
			SearchQuery: &ibmSearchQuery{AppID: ibmString("careers")},
			SearchResults: &ibmSearchResults{
				TotalResults:     ibmInt(total),
				StartIndex:       ibmInt(start),
				NumResults:       ibmInt(len(results)),
				SearchResultList: &results,
			},
		},
	}
}

func ibmFixtureResult(id string) ibmSearchResult {
	attributes := []map[string]json.RawMessage{
		ibmRawAttribute("field_text_01", strconv.Quote(id)),
		ibmRawAttribute("country", `"in"`),
		ibmRawAttribute("scope", `"careers2"`),
		ibmRawAttribute("field_keyword_19", `"Bangalore, IN"`),
		ibmRawAttribute("raw_body", `"<p>Build &amp; ship.</p><p>Use Go.</p>"`),
		ibmRawAttribute("dcdate", `"2026-07-29"`),
	}
	return ibmSearchResult{
		ResultNum:     ibmInt(0),
		ID:            fmt.Sprintf("search-hash-%s", id),
		Title:         "Software Engineer " + id,
		URL:           "https://careers.ibm.com/careers/JobDetail?jobId=" + id,
		DocAttributes: &attributes,
	}
}

func ibmRawAttribute(key, raw string) map[string]json.RawMessage {
	return map[string]json.RawMessage{key: json.RawMessage(raw)}
}

func ibmSetAttribute(result *ibmSearchResult, key, value string) {
	ibmSetRawAttribute(result, key, strconv.Quote(value))
}

func ibmSetRawAttribute(result *ibmSearchResult, key, raw string) {
	if result.DocAttributes == nil {
		return
	}
	for _, attribute := range *result.DocAttributes {
		if _, ok := attribute[key]; ok {
			attribute[key] = json.RawMessage(raw)
			return
		}
	}
}

func ibmDeleteAttribute(result *ibmSearchResult, key string) {
	if result.DocAttributes == nil {
		return
	}
	filtered := make([]map[string]json.RawMessage, 0, len(*result.DocAttributes))
	for _, attribute := range *result.DocAttributes {
		if _, ok := attribute[key]; !ok {
			filtered = append(filtered, attribute)
		}
	}
	result.DocAttributes = &filtered
}

func ibmAppendAttribute(result *ibmSearchResult, key, value string) {
	*result.DocAttributes = append(*result.DocAttributes, ibmRawAttribute(key, strconv.Quote(value)))
}

func ibmString(value string) *string { return &value }
func ibmInt(value int) *int          { return &value }
