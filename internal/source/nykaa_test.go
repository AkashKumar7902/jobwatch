package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNykaaFetchesSSRPagesAndDetailsLazily(t *testing.T) {
	ids := make([]string, 11)
	for i := range ids {
		ids[i] = customTestUUID(i + 1)
	}
	listCalls := 0
	detailCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			listCalls++
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page == 0 {
				page = 1
			}
			start := (page - 1) * nykaaPageSize
			end := min(start+nykaaPageSize, len(ids))
			fmt.Fprintf(w, "Showing %d of %d - Jobs", end-start, len(ids))
			for i := start; i < end; i++ {
				fmt.Fprint(w, nykaaTestCard(ids[i], fmt.Sprintf("Engineer &amp; Lead %d", i)))
			}
			fmt.Fprintf(w, `<div data-last-page="2" data-current-page="%d" data-pagination-container></div>`, page)
			return
		}
		if r.URL.Path == "/"+ids[0] {
			detailCalls++
			fmt.Fprint(w, `<h1 class="text-2xl font-semibold text-primary"> Engineer &amp; Lead 0 </h1>`+
				`<span>Posted on </span><span> 20 May 2026 </span>`+
				`<div class="job-description-panel w-full"><p>Build systems.</p>`+
				`<ul><li>Own quality</li></ul></div></div></div>`+
				`<div class="col-span-12 mt-6"></div>`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := &nykaa{company: "Nykaa", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 11 || listCalls != 2 || detailCalls != 0 {
		t.Fatalf("Fetch returned %d jobs, list calls=%d detail calls=%d", len(jobs), listCalls, detailCalls)
	}
	first := &jobs[0]
	if first.ID != "nykaa/"+ids[0] || first.Title != "Engineer & Lead 0" ||
		first.Location != "Mumbai" || first.EmploymentType != "Full Time" {
		t.Errorf("first job = %+v", *first)
	}
	if err := src.Detail(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if first.Description != "Build systems.\nOwn quality" {
		t.Errorf("Description = %q", first.Description)
	}
	wantDate, _ := time.Parse("02 January 2006", "20 May 2026")
	if !first.PostedAt.Equal(wantDate) {
		t.Errorf("PostedAt = %s, want %s", first.PostedAt, wantDate)
	}
}

func TestNykaaRejectsUpstreamErrorShell(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body>Unable to fetch job listings</body></html>`)
	}))
	defer srv.Close()
	src := &nykaa{company: "Nykaa", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}

func TestNykaaRejectsInconsistentPagination(t *testing.T) {
	id := customTestUUID(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "Showing 1 of 11 - Jobs")
		fmt.Fprint(w, nykaaTestCard(id, "Engineer"))
		fmt.Fprint(w, `<div data-last-page="1" data-current-page="1"></div>`)
	}))
	defer srv.Close()
	src := &nykaa{company: "Nykaa", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}

func nykaaTestCard(id, title string) string {
	return strings.Join([]string{
		`<div class="flex flex-col space-y-3 border-offset-background p-5 md:flex-row">`,
		`<div class="w-full">`,
		`<a href="/` + id + `" class="text-lg font-semibold text-primary">` + title + `</a>`,
		`<p class="text-sm">Minimum experience</p>`,
		`<div class="flex flex-wrap items-center space-x-3">`,
		`<span class="break-all text-sm"> Mumbai </span><div class="dot"></div>`,
		`<span class="break-all text-sm"> In Office </span><div class="dot"></div>`,
		`<span class="break-all text-sm"> Full Time </span>`,
		`</div></div><div class="flex justify-end">`,
		`<a href="/` + id + `">Apply</a></div></div>`,
	}, "")
}
