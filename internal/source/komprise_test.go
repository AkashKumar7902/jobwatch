package source

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
)

func TestKompriseFetch(t *testing.T) {
	const baseURL = "http://komprise.test"
	client := newKompriseTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/job_listing-sitemap.xml":
			w.Header().Set("Content-Type", "text/xml")
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>%s/job/software-engineer/</loc></url></urlset>`, baseURL)
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
	src := &komprise{company: "Komprise", baseURL: baseURL, client: client}
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
		job.URL != baseURL+"/job/software-engineer/" ||
		job.Description != "Build & ship.\n1+ years." {
		t.Errorf("job = %#v", job)
	}
	wantTime, _ := time.Parse("2006-01-02", "2026-07-10")
	if !job.PostedAt.Equal(wantTime) {
		t.Errorf("PostedAt = %s", job.PostedAt)
	}
}

func TestKompriseRejectsEmptySitemap(t *testing.T) {
	client := newKompriseTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?><urlset></urlset>`)
	}))
	src := &komprise{baseURL: "http://komprise.test", client: client}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "no postings") {
		t.Fatalf("Fetch = %#v, %v", jobs, err)
	}
}

func TestKompriseRejectsMalformedPresentDate(t *testing.T) {
	client := newKompriseTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>
<h1 class="entry-title">Software Engineer</h1>
<li class="date-posted"><time datetime="not-a-date">Posted</time></li>
<div class="job_description"><p>Build reliable storage software.</p></div>
<div class="job_application application"></div></html>`)
	}))

	const baseURL = "http://komprise.test"
	src := &komprise{company: "Komprise", baseURL: baseURL, client: client}
	job, err := src.fetchJob(
		context.Background(),
		baseURL+"/job/software-engineer/",
		"software-engineer",
	)
	if err == nil || job != (model.Job{}) || !strings.Contains(err.Error(), "invalid posted date") {
		t.Fatalf("fetchJob = (%#v, %v), want atomic date error", job, err)
	}
}

func TestKompriseRejectsUnsafeSitemapJobURLs(t *testing.T) {
	tests := []struct {
		name   string
		jobURL string
	}{
		{name: "userinfo", jobURL: "http://user@komprise.test/job/software-engineer/"},
		{name: "port", jobURL: "http://komprise.test:80/job/software-engineer/"},
		{name: "different scheme", jobURL: "https://komprise.test/job/software-engineer/"},
		{name: "different host", jobURL: "http://evil.test/job/software-engineer/"},
		{name: "host suffix", jobURL: "http://komprise.test.evil/job/software-engineer/"},
		{name: "query", jobURL: "http://komprise.test/job/software-engineer/?draft=1"},
		{name: "force query", jobURL: "http://komprise.test/job/software-engineer/?"},
		{name: "fragment", jobURL: "http://komprise.test/job/software-engineer/#apply"},
		{name: "raw path", jobURL: "http://komprise.test/job/%73oftware-engineer/"},
		{name: "dot traversal", jobURL: "http://komprise.test/job/../software-engineer/"},
		{name: "nested segments", jobURL: "http://komprise.test/job/software-engineer/apply/"},
		{name: "missing trailing slash", jobURL: "http://komprise.test/job/software-engineer"},
		{name: "uppercase slug", jobURL: "http://komprise.test/job/Software-Engineer/"},
		{name: "underscore slug", jobURL: "http://komprise.test/job/software_engineer/"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var detailRequests atomic.Int32
			client := newKompriseTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/job_listing-sitemap.xml" {
					_, _ = fmt.Fprintf(
						w,
						`<?xml version="1.0"?><urlset><url><loc>%s</loc></url></urlset>`,
						test.jobURL,
					)
					return
				}
				detailRequests.Add(1)
				http.Error(w, "unexpected detail request", http.StatusInternalServerError)
			}))
			src := &komprise{
				company: "Komprise",
				baseURL: "http://komprise.test",
				client:  client,
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), "invalid URL") {
				t.Fatalf("Fetch = %#v, %v; want invalid URL error", jobs, err)
			}
			if got := detailRequests.Load(); got != 0 {
				t.Fatalf("unsafe URL caused %d detail requests", got)
			}
		})
	}
}

func TestKompriseRejectsRedirectsWithoutContactingTargets(t *testing.T) {
	t.Run("sitemap", func(t *testing.T) {
		var targetRequests atomic.Int32
		client := newKompriseTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/job_listing-sitemap.xml":
				http.Redirect(w, r, "/redirected-sitemap", http.StatusFound)
			case "/redirected-sitemap":
				targetRequests.Add(1)
				http.Error(w, "redirect target contacted", http.StatusInternalServerError)
			default:
				http.NotFound(w, r)
			}
		}))
		src := &komprise{baseURL: "http://komprise.test", client: client}
		jobs, err := src.Fetch(context.Background())
		if err == nil || jobs != nil || !strings.Contains(err.Error(), "302 Found") {
			t.Fatalf("Fetch = %#v, %v; want redirect status error", jobs, err)
		}
		if got := targetRequests.Load(); got != 0 {
			t.Fatalf("redirect target contacted %d times", got)
		}
	})

	t.Run("detail", func(t *testing.T) {
		var targetRequests atomic.Int32
		client := newKompriseTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/job_listing-sitemap.xml":
				_, _ = fmt.Fprint(
					w,
					`<?xml version="1.0"?><urlset><url><loc>http://komprise.test/job/software-engineer/</loc></url></urlset>`,
				)
			case "/job/software-engineer/":
				http.Redirect(w, r, "/redirected-detail", http.StatusFound)
			case "/redirected-detail":
				targetRequests.Add(1)
				http.Error(w, "redirect target contacted", http.StatusInternalServerError)
			default:
				http.NotFound(w, r)
			}
		}))
		src := &komprise{baseURL: "http://komprise.test", client: client}
		jobs, err := src.Fetch(context.Background())
		if err == nil || jobs != nil || !strings.Contains(err.Error(), "302 Found") {
			t.Fatalf("Fetch = %#v, %v; want redirect status error", jobs, err)
		}
		if got := targetRequests.Load(); got != 0 {
			t.Fatalf("redirect target contacted %d times", got)
		}
	})
}

func TestKompriseRejectsMismatchedFinalResponseURL(t *testing.T) {
	t.Run("sitemap", func(t *testing.T) {
		client := newKompriseTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<?xml version="1.0"?><urlset></urlset>`)
		}))
		client.Transport = kompriseResponseURLTransport{
			base: client.Transport,
			url:  "http://komprise.test/not-the-sitemap",
		}
		src := &komprise{baseURL: "http://komprise.test", client: client}
		jobs, err := src.Fetch(context.Background())
		if err == nil || jobs != nil || !strings.Contains(err.Error(), "does not match requested URL") {
			t.Fatalf("Fetch = %#v, %v; want final URL boundary error", jobs, err)
		}
	})

	t.Run("detail", func(t *testing.T) {
		client := newKompriseTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<html>
<h1 class="entry-title">Software Engineer</h1>
<div class="job_description"><p>Build reliable storage software.</p></div>
<div class="job_application application"></div></html>`)
		}))
		client.Transport = kompriseResponseURLTransport{
			base: client.Transport,
			url:  "http://komprise.test/job/different-role/",
		}
		src := &komprise{baseURL: "http://komprise.test", client: client}
		job, err := src.fetchJob(
			context.Background(),
			"http://komprise.test/job/software-engineer/",
			"software-engineer",
		)
		if err == nil || job != (model.Job{}) || !strings.Contains(err.Error(), "does not match requested URL") {
			t.Fatalf("fetchJob = %#v, %v; want final URL boundary error", job, err)
		}
	})
}

func TestKompriseNilClientIsSafe(t *testing.T) {
	src := &komprise{baseURL: "ftp://komprise.test"}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("Fetch = %#v, %v; want validation error without nil-client panic", jobs, err)
	}
}

type kompriseResponseURLTransport struct {
	base http.RoundTripper
	url  string
}

func (t kompriseResponseURLTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	changed := req.Clone(req.Context())
	changed.URL, err = url.Parse(t.url)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	resp.Request = changed
	return resp, nil
}

func newKompriseTestClient(t *testing.T, handler http.Handler) *http.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		server.Close()
	})
	return &http.Client{Transport: transport}
}
