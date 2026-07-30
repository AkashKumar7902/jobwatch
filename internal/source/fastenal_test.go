package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestFastenalUsesAnonymousCSRFSessionAndDetailsLazily(t *testing.T) {
	postings := make([]map[string]any, 26)
	for i := range postings {
		postings[i] = map[string]any{
			"jobId": 631000 + i, "title": fmt.Sprintf("Job %d", i),
			"type": "Full-time", "city": "Winona", "state": "MN",
			"department": "Engineering", "approvedDate": int64(1785283200000),
		}
	}
	listCalls := 0
	detailCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jobs":
			if r.Method != http.MethodGet {
				t.Errorf("/jobs method = %s", r.Method)
			}
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "anonymous-session", Path: "/"})
			fmt.Fprint(w, `<meta name="_csrf_header" content="X-CSRF-TOKEN">`+
				`<meta name="_csrf" content="csrf-value">`)
		case "/load-jobs":
			listCalls++
			if r.Method != http.MethodPost {
				t.Errorf("/load-jobs method = %s", r.Method)
			}
			if r.Header.Get("X-CSRF-TOKEN") != "csrf-value" {
				t.Errorf("CSRF header = %q", r.Header.Get("X-CSRF-TOKEN"))
			}
			cookie, err := r.Cookie("JSESSIONID")
			if err != nil || cookie.Value != "anonymous-session" {
				t.Errorf("session cookie = %#v, %v", cookie, err)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("length") != "25" || r.Form.Get("order[0][column]") != "1" ||
				r.Form.Get("order[0][dir]") != "asc" || r.Form.Get("sortColumn") != "1" ||
				r.Form.Get("sortDir") != "asc" {
				t.Errorf("form = %v", r.Form)
			}
			draw, _ := strconv.Atoi(r.Form.Get("draw"))
			start, _ := strconv.Atoi(r.Form.Get("start"))
			end := min(start+fastenalPageSize, len(postings))
			writeJSON(t, w, map[string]any{
				"data": postings[start:end], "draw": draw,
				"recordsTotal": len(postings), "recordsFiltered": len(postings),
			})
		case "/details/631000":
			detailCalls++
			fmt.Fprint(w, `<table><tr><td>Job ID</td><td>631000</td></tr></table>`+
				`<div class="cms-job-description"><p><label>Job Description</label></p>`+
				`<p>Build &amp; ship.</p><ul><li>Own quality</li></ul></div>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := &fastenal{company: "Fastenal", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != len(postings) || listCalls != 2 || detailCalls != 0 {
		t.Fatalf("Fetch returned %d jobs, list calls=%d detail calls=%d", len(jobs), listCalls, detailCalls)
	}
	first := &jobs[0]
	if first.ID != "fastenal/631000" || first.Location != "Winona, MN" ||
		first.EmploymentType != "Full-time" || first.Description != "" {
		t.Errorf("first job = %+v", *first)
	}
	if err := src.Detail(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 || first.Description != "Build & ship.\nOwn quality" {
		t.Errorf("detail calls=%d description=%q", detailCalls, first.Description)
	}
}

func TestFastenalRejectsMissingDataTablesSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jobs" {
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "session"})
			fmt.Fprint(w, `<meta name="_csrf_header" content="X-CSRF-TOKEN"><meta name="_csrf" content="token">`)
			return
		}
		writeJSON(t, w, map[string]any{
			"data": nil, "draw": 1, "recordsTotal": 0, "recordsFiltered": 0,
		})
	}))
	defer srv.Close()
	src := &fastenal{company: "Fastenal", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}
