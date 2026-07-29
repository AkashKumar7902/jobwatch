package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestRipplingFetchesAllPagesAndDetailsLazily(t *testing.T) {
	postings := make([]map[string]any, 1001)
	for i := range postings {
		id := ripplingTestUUID(i)
		postings[i] = map[string]any{
			"id":   id,
			"name": fmt.Sprintf("Job %04d", i),
			"url":  ripplingTestURL(id),
		}
	}
	firstID := ripplingTestUUID(0)
	postings[0]["name"] = "  Software Engineer I  "
	postings[0]["locations"] = []map[string]string{
		{"name": "San Francisco, CA"},
		{"name": " Remote "},
		{"name": "San Francisco, CA"},
		{"name": ""},
	}

	var listCalls, detailCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		switch r.URL.Path {
		case "/api/v2/board/acme/jobs":
			wantQuery := fmt.Sprintf("groupJobsByLocation=true&page=%d&pageSize=1000", listCalls)
			if r.URL.RawQuery != wantQuery {
				t.Errorf("query = %q, want %q", r.URL.RawQuery, wantQuery)
			}
			start := listCalls * ripplingPageSize
			end := start + ripplingPageSize
			if end > len(postings) {
				end = len(postings)
			}
			writeJSON(t, w, map[string]any{
				"items": postings[start:end], "page": listCalls, "pageSize": 1000,
				"totalItems": len(postings), "totalPages": 2,
			})
			listCalls++
		case "/api/v2/board/acme/jobs/" + firstID:
			detailCalls++
			writeJSON(t, w, map[string]any{
				"uuid": firstID, "createdOn": "2026-03-31T17:37:26.093000-07:00",
				"unlistedFromSearch": false,
				"description": map[string]string{
					"company": "<p>About &amp; values.</p>",
					"role":    "<p>Build things.</p><p>1+ years.</p>",
				},
				"employmentType": map[string]string{"id": "Salaried, full-time", "label": "SALARIED_FT"},
				"workLocations":  []string{"New York, NY", " Remote ", "New York, NY"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := &rippling{
		company: "Acme", slug: "acme",
		apiBase: srv.URL + "/api/v2/board/acme", jobsBase: "https://ats.rippling.com/acme/jobs",
		client: srv.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != len(postings) {
		t.Fatalf("Fetch returned %d jobs, want %d", len(jobs), len(postings))
	}
	if listCalls != 2 || detailCalls != 0 {
		t.Fatalf("calls after Fetch: list=%d detail=%d, want list=2 detail=0", listCalls, detailCalls)
	}
	first := &jobs[0]
	if first.ID != "rippling/acme/"+firstID {
		t.Errorf("ID = %q", first.ID)
	}
	if first.Title != "Software Engineer I" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.Company != "Acme" {
		t.Errorf("Company = %q", first.Company)
	}
	if first.Location != "San Francisco, CA; Remote" {
		t.Errorf("Location = %q", first.Location)
	}
	if first.URL != ripplingTestURL(firstID) {
		t.Errorf("URL = %q", first.URL)
	}
	if first.Description != "" {
		t.Errorf("Fetch populated lazy description %q", first.Description)
	}

	if err := src.Detail(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 {
		t.Fatalf("detail calls = %d, want 1", detailCalls)
	}
	if first.Description != "About & values.\n\nBuild things.\n1+ years." {
		t.Errorf("Description = %q", first.Description)
	}
	if first.EmploymentType != "Salaried, full-time" {
		t.Errorf("EmploymentType = %q", first.EmploymentType)
	}
	if first.Location != "New York, NY; Remote" {
		t.Errorf("detailed Location = %q", first.Location)
	}
	if first.URL != ripplingTestURL(firstID) {
		t.Errorf("detailed URL = %q", first.URL)
	}
	wantTime, _ := time.Parse(time.RFC3339Nano, "2026-03-31T17:37:26.093000-07:00")
	if !first.PostedAt.Equal(wantTime) {
		t.Errorf("PostedAt = %s, want %s", first.PostedAt, wantTime)
	}
}

func TestRipplingEmptyBoardRequiresArrayAndMetadata(t *testing.T) {
	src, closeServer := ripplingSourceForResponse(t, map[string]any{
		"items": []any{}, "page": 0, "pageSize": 1000, "totalItems": 0, "totalPages": 0,
	})
	defer closeServer()
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("Fetch = %#v, want a non-nil empty slice", jobs)
	}

	for name, payload := range map[string]map[string]any{
		"nonzero total pages": {
			"items": []any{}, "page": 0, "pageSize": 1000, "totalItems": 0, "totalPages": 1,
		},
		"null items": {
			"items": nil, "page": 0, "pageSize": 1000, "totalItems": 0, "totalPages": 0,
		},
		"missing items": {
			"page": 0, "pageSize": 1000, "totalItems": 0, "totalPages": 0,
		},
		"null page": {
			"items": []any{}, "page": nil, "pageSize": 1000, "totalItems": 0, "totalPages": 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, closeServer := ripplingSourceForResponse(t, payload)
			defer closeServer()
			assertNilFetchError(t, src)
		})
	}
}

func TestRipplingRejectsInconsistentPagesAndPostings(t *testing.T) {
	oneID := ripplingTestUUID(1)
	oneValid := []map[string]any{ripplingTestPosting(oneID, "One")}
	for name, payload := range map[string]map[string]any{
		"incoherent total pages": {
			"items": oneValid, "page": 0, "pageSize": 1000, "totalItems": 1, "totalPages": 2,
		},
		"wrong page": {
			"items": oneValid, "page": 1, "pageSize": 1000, "totalItems": 1, "totalPages": 1,
		},
		"wrong page size": {
			"items": oneValid, "page": 0, "pageSize": 1, "totalItems": 1, "totalPages": 1,
		},
		"count mismatch": {
			"items": []any{}, "page": 0, "pageSize": 1000, "totalItems": 1, "totalPages": 1,
		},
		"too many pages": {
			"items": []any{}, "page": 0, "pageSize": 1000,
			"totalItems": ripplingPageSize * (ripplingMaxPages + 1), "totalPages": ripplingMaxPages + 1,
		},
		"invalid id": {
			"items": []map[string]any{{"id": "not-a-uuid", "name": "One", "url": ripplingTestURL("not-a-uuid")}},
			"page":  0, "pageSize": 1000, "totalItems": 1, "totalPages": 1,
		},
		"empty name": {
			"items": []map[string]any{{"id": oneID, "name": " ", "url": ripplingTestURL(oneID)}},
			"page":  0, "pageSize": 1000, "totalItems": 1, "totalPages": 1,
		},
		"missing url": {
			"items": []map[string]any{{"id": oneID, "name": "One"}},
			"page":  0, "pageSize": 1000, "totalItems": 1, "totalPages": 1,
		},
		"noncanonical url": {
			"items": []map[string]any{{"id": oneID, "name": "One", "url": "https://example.com/job"}},
			"page":  0, "pageSize": 1000, "totalItems": 1, "totalPages": 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, closeServer := ripplingSourceForResponse(t, payload)
			defer closeServer()
			assertNilFetchError(t, src)
		})
	}
}

func TestRipplingRejectsPaginationDriftAndDuplicateIDs(t *testing.T) {
	lastID := ripplingTestUUID(1000)
	firstID := ripplingTestUUID(0)
	for _, test := range []struct {
		name       string
		secondPage map[string]any
	}{
		{
			name: "total drift",
			secondPage: map[string]any{
				"items": []map[string]any{ripplingTestPosting(lastID, "Last")},
				"page":  1, "pageSize": 1000, "totalItems": 1002, "totalPages": 2,
			},
		},
		{
			name: "duplicate id",
			secondPage: map[string]any{
				"items": []map[string]any{ripplingTestPosting(firstID, "Duplicate")},
				"page":  1, "pageSize": 1000, "totalItems": 1001, "totalPages": 2,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstItems := make([]map[string]any, 1000)
			for i := range firstItems {
				firstItems[i] = ripplingTestPosting(ripplingTestUUID(i), "Job")
			}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls == 0 {
					writeJSON(t, w, map[string]any{
						"items": firstItems, "page": 0, "pageSize": 1000,
						"totalItems": 1001, "totalPages": 2,
					})
				} else {
					writeJSON(t, w, test.secondPage)
				}
				calls++
			}))
			defer srv.Close()
			src := &rippling{
				company: "Acme", slug: "acme", apiBase: srv.URL,
				jobsBase: "https://ats.rippling.com/acme/jobs", client: srv.Client(),
			}
			assertNilFetchError(t, src)
		})
	}
}

func TestRipplingDetailRejectsInvalidResponsesWithoutMutation(t *testing.T) {
	postingID := ripplingTestUUID(1)
	base := map[string]any{
		"uuid": postingID, "createdOn": "2026-01-02T03:04:05.123456789Z",
		"unlistedFromSearch": false,
		"description":        map[string]string{"company": "<p>Company</p>", "role": "<p>Role</p>"},
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"uuid mismatch", func(p map[string]any) { p["uuid"] = ripplingTestUUID(2) }},
		{"unlisted", func(p map[string]any) { p["unlistedFromSearch"] = true }},
		{"missing unlisted", func(p map[string]any) { delete(p, "unlistedFromSearch") }},
		{"null unlisted", func(p map[string]any) { p["unlistedFromSearch"] = nil }},
		{"empty role description", func(p map[string]any) {
			p["description"] = map[string]string{"company": "<p>Company</p>", "role": ""}
		}},
		{"invalid date", func(p map[string]any) { p["createdOn"] = "not-a-date" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := cloneMap(base)
			test.mutate(payload)
			src, closeServer := ripplingSourceForResponse(t, payload)
			defer closeServer()
			job := model.Job{
				ID: "rippling/acme/" + postingID, Description: "before", EmploymentType: "before",
				Location: "before", URL: "before", PostedAt: time.Unix(1, 0),
			}
			before := job
			if err := src.Detail(context.Background(), &job); err == nil {
				t.Fatal("Detail succeeded, want error")
			}
			if job != before {
				t.Fatalf("Detail mutated job on error:\n got %+v\nwant %+v", job, before)
			}
		})
	}
}

func TestRipplingDetailUsesLabelAndKeepsListLocation(t *testing.T) {
	postingID := ripplingTestUUID(1)
	src, closeServer := ripplingSourceForResponse(t, map[string]any{
		"uuid": postingID, "createdOn": "2026-01-02T03:04:05Z",
		"unlistedFromSearch": false,
		"description":        map[string]string{"company": "", "role": "<p>Role text</p>"},
		"employmentType":     map[string]string{"id": " ", "label": "SALARIED_FT"},
		"workLocations":      []string{"", " "},
	})
	defer closeServer()
	job := model.Job{ID: "rippling/acme/" + postingID, Location: "List location"}
	if err := src.Detail(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.EmploymentType != "SALARIED_FT" {
		t.Errorf("EmploymentType = %q", job.EmploymentType)
	}
	if job.Location != "List location" {
		t.Errorf("Location = %q, want list fallback", job.Location)
	}
	if job.Description != "Role text" {
		t.Errorf("Description = %q", job.Description)
	}
}

func TestRipplingDetailAllowsMissingOrEmptyCreatedOn(t *testing.T) {
	postingID := ripplingTestUUID(1)
	for name, createdOn := range map[string]any{
		"missing": nil,
		"empty":   "",
	} {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{
				"uuid": postingID, "unlistedFromSearch": false,
				"description": map[string]string{"role": "<p>Role text</p>"},
			}
			if name != "missing" {
				payload["createdOn"] = createdOn
			}
			src, closeServer := ripplingSourceForResponse(t, payload)
			defer closeServer()
			job := model.Job{ID: "rippling/acme/" + postingID}
			if err := src.Detail(context.Background(), &job); err != nil {
				t.Fatal(err)
			}
			if !job.PostedAt.IsZero() {
				t.Errorf("PostedAt = %s, want zero", job.PostedAt)
			}
		})
	}
}

func TestRipplingDetailRejectsInvalidPostingIDBeforeRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer srv.Close()
	src := &rippling{
		company: "Acme", slug: "acme", apiBase: srv.URL,
		jobsBase: "https://ats.rippling.com/acme/jobs", client: srv.Client(),
	}
	job := model.Job{ID: "rippling/acme/not-a-uuid"}
	if err := src.Detail(context.Background(), &job); err == nil {
		t.Fatal("Detail accepted an invalid posting id")
	}
	if requests != 0 {
		t.Fatalf("Detail made %d requests for invalid posting id", requests)
	}
}

func TestRipplingReportsHTTPAndJSONErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"non-200", http.StatusBadGateway, "upstream failed"},
		{"malformed JSON", http.StatusOK, "{"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer srv.Close()
			src := &rippling{
				company: "Acme", slug: "acme", apiBase: srv.URL,
				jobsBase: "https://ats.rippling.com/acme/jobs", client: srv.Client(),
			}
			assertNilFetchError(t, src)
		})
	}
}

func TestRipplingValidatesSlugWithoutNormalizing(t *testing.T) {
	for _, slug := range []string{"rippling", "acme-1", "a"} {
		t.Run("valid_"+slug, func(t *testing.T) {
			if _, err := New("rippling", "Acme", params.Map{"board_slug": slug}, &http.Client{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, slug := range []string{"", "Rippling", " acme", "acme ", "-acme", "acme-", "acme--jobs", "acme_jobs", "acme/jobs"} {
		t.Run("invalid_"+strings.ReplaceAll(slug, "/", "_"), func(t *testing.T) {
			if _, err := New("rippling", "Acme", params.Map{"board_slug": slug}, &http.Client{}); err == nil {
				t.Fatalf("New accepted invalid slug %q", slug)
			}
		})
	}
}

func TestRipplingIdentityAndStatePrefix(t *testing.T) {
	a, err := New("rippling", "Acme", params.Map{"board_slug": "acme"}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("rippling", "Renamed Acme", params.Map{"board_slug": "acme"}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if Identity(a) != "rippling/acme" || Identity(a) != Identity(b) {
		t.Errorf("identities = %q, %q", Identity(a), Identity(b))
	}
	if StatePrefix(a) != "rippling/acme/" {
		t.Errorf("StatePrefix = %q", StatePrefix(a))
	}
	if _, ok := a.(Detailer); !ok {
		t.Error("wrapped Rippling source does not expose Detailer")
	}
}

func ripplingSourceForResponse(t *testing.T, payload any) (*rippling, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, payload)
	}))
	return &rippling{
		company: "Acme", slug: "acme", apiBase: srv.URL,
		jobsBase: "https://ats.rippling.com/acme/jobs", client: srv.Client(),
	}, srv.Close
}

func assertNilFetchError(t *testing.T, src *rippling) {
	t.Helper()
	jobs, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatalf("Fetch returned %d jobs without error", len(jobs))
	}
	if jobs != nil {
		t.Fatalf("Fetch returned partial jobs on error: %#v", jobs)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func ripplingTestUUID(n int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", n, n)
}

func ripplingTestURL(id string) string {
	return "https://ats.rippling.com/acme/jobs/" + id
}

func ripplingTestPosting(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "url": ripplingTestURL(id)}
}
