package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
)

func TestKompriseFetch(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/job_listing-sitemap.xml":
			w.Header().Set("Content-Type", "text/xml")
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>%s/job/software-engineer/</loc></url></urlset>`, server.URL)
		case "/job/software-engineer/":
			_, _ = fmt.Fprint(w, `<html>
<h1 class="entry-title">Software Engineer &#8211; Productivity</h1>
<ul><li class="location"><a href="#">Bengaluru</a></li>
<li class="date-posted"><time datetime="2026-07-10">Posted</time></li></ul>
<div class="job_description"><p>Build &amp; ship.</p><p>1+ years.</p></div>
<div class="job_application application"></div></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	src := &komprise{company: "Komprise", baseURL: server.URL, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %#v", jobs)
	}
	job := jobs[0]
	if job.ID != "komprise/software-engineer" ||
		job.Title != "Software Engineer – Productivity" ||
		job.Location != "Bengaluru" ||
		job.URL != server.URL+"/job/software-engineer/" ||
		job.Description != "Build & ship.\n1+ years." {
		t.Errorf("job = %#v", job)
	}
	wantTime, _ := time.Parse("2006-01-02", "2026-07-10")
	if !job.PostedAt.Equal(wantTime) {
		t.Errorf("PostedAt = %s", job.PostedAt)
	}
}

func TestKompriseRejectsEmptySitemap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?><urlset></urlset>`)
	}))
	defer server.Close()
	src := &komprise{baseURL: server.URL, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "no postings") {
		t.Fatalf("Fetch = %#v, %v", jobs, err)
	}
}

func TestKompriseRejectsMalformedPresentDate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>
<h1 class="entry-title">Software Engineer</h1>
<li class="date-posted"><time datetime="not-a-date">Posted</time></li>
<div class="job_description"><p>Build reliable storage software.</p></div>
<div class="job_application application"></div></html>`)
	}))
	defer server.Close()

	src := &komprise{company: "Komprise", baseURL: server.URL, client: server.Client()}
	job, err := src.fetchJob(
		context.Background(),
		server.URL+"/job/software-engineer/",
		"software-engineer",
	)
	if err == nil || job != (model.Job{}) || !strings.Contains(err.Error(), "invalid posted date") {
		t.Fatalf("fetchJob = (%#v, %v), want atomic date error", job, err)
	}
}
