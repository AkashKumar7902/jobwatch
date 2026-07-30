package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPayUFetchesCompleteSSRBoardAndDetailsLazily(t *testing.T) {
	ids := []string{customTestUUID(1), customTestUUID(2)}
	detailCalls := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/job-board/":
			fmt.Fprint(w, `Showing <span>2</span> of <span>2</span> positions`)
			for i, id := range ids {
				fmt.Fprintf(w, `<li class="job-entry"><a href="%s/job/%s/?gh_jid=%s" class="title">`+
					`<h3>Role &amp; Lead %d</h3><span class="tag" data-type="title">Engineering</span>`+
					`<span class="tag" data-type="location">Prague, Czech Republic</span></a></li>`,
					srv.URL, id, id, i,
				)
			}
		case "/job/" + ids[0] + "/":
			detailCalls++
			fmt.Fprintf(w, `<section class="hero hero-secondary"><h1>Role &amp; Lead 0</h1>`+
				`<div class="job-location"><p class="hero-subtitle">Prague, Czech Republic</p></div></section>`+
				`<div id="main"><p>Build payments.</p><ul><li>Own growth</li></ul>`+
				`<a href="https://jobs.lever.co/payugpo/%s/apply" class="btn">Apply for this job</a></div>`,
				ids[0],
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := &payu{company: "PayU", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || detailCalls != 0 {
		t.Fatalf("Fetch returned %d jobs, detail calls=%d", len(jobs), detailCalls)
	}
	first := &jobs[0]
	if first.ID != "payu/"+ids[0] || first.Title != "Role & Lead 0" ||
		first.Location != "Prague, Czech Republic" {
		t.Errorf("first job = %+v", *first)
	}
	if err := src.Detail(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 || first.Description != "Build payments.\nOwn growth" {
		t.Errorf("detail calls=%d description=%q", detailCalls, first.Description)
	}
}

func TestPayURejectsTruncatedBoard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `Showing <span>1</span> of <span>2</span> positions`)
	}))
	defer srv.Close()
	src := &payu{company: "PayU", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}
