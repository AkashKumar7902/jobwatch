package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	freshteamTestSlug = "acme"
	freshteamTestHost = freshteamTestSlug + ".freshteam.com"
	freshteamTestBase = "https://" + freshteamTestHost
	freshteamTestID1  = "AbCdEf0123_-"
	freshteamTestID2  = "ZyXwVu9876_-"
)

type freshteamRoundTripper func(*http.Request) (*http.Response, error)

func (f freshteamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func freshteamResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func freshteamTestSource(handler freshteamRoundTripper) *freshteam {
	return &freshteam{
		company: "Acme & Co",
		slug:    freshteamTestSlug,
		host:    freshteamTestHost,
		base:    freshteamTestBase,
		client:  &http.Client{Transport: handler},
	}
}

func freshteamListPage(cards string, count int) string {
	return fmt.Sprintf(`<!doctype html><html><head>
<meta property="og:description" content="#%d Jobs - India">
</head><body><h3>Open Positions</h3>
<div class="job-role-list"><ul>%s</ul></div></body></html>`, count, cards)
}

func freshteamModernCard(id, slug, title, location, employmentType string) string {
	return fmt.Sprintf(`<li><div class="job-list">
<a class="heading" href="/jobs/%s/%s">
<div class="job-title">%s</div>
<div class="job-location"><div class="location-info">%s<br>%s</div></div>
</a></div></li>`, id, slug, title, location, employmentType)
}

func freshteamLegacyCard(id, slug, title, location, employmentType string) string {
	return fmt.Sprintf(`<li class="heading"><a href="/jobs/%s/%s">
<div class="job-list-info"><h6 class="job-title">%s</h6></div>
<div class="job-location"><p class="paragraph">%s</p>
<p class="paragraph location-info">%s<br></p></div>
</a></li>`, id, slug, title, employmentType, location)
}

func freshteamValidPosting() map[string]any {
	return map[string]any{
		"@context":       "https://schema.org",
		"@type":          []any{"Thing", "JobPosting"},
		"url":            freshteamTestBase + "/jobs/" + freshteamTestID1 + "/Platform%20%26%20Reliability",
		"title":          "Platform &amp; Reliability",
		"description":    "&lt;p&gt;Build &amp;amp; operate systems.&lt;/p&gt;&lt;ul&gt;&lt;li&gt;Ship safely.&lt;/li&gt;&lt;/ul&gt;",
		"datePosted":     "2026-07-30 05:25:11 UTC",
		"employmentType": "FULL_TIME",
		"hiringOrganization": map[string]any{
			"@type": "Organization",
			"name":  "Acme &amp; Co",
		},
		"jobLocation": map[string]any{
			"@type": "Place",
			"address": map[string]any{
				"@type":           "PostalAddress",
				"addressLocality": "Bengaluru",
				"addressCountry":  "India",
			},
		},
	}
}

func freshteamDetailPage(t *testing.T, posting map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"@context": "https://schema.org",
		"@graph": []any{
			map[string]any{"@type": "WebPage", "name": "Careers"},
			posting,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return `<script type="application/ld+json">{broken</script>` +
		`<script data-source="freshteam" TYPE="application/ld+json; charset=utf-8">` +
		string(payload) + `</script>`
}

func TestFreshteamFetchCompleteAndLazyDetail(t *testing.T) {
	var listCalls, detailCalls atomic.Int32
	list := freshteamListPage(
		freshteamModernCard(
			freshteamTestID1, "platform-reliability",
			"Platform &amp; Reliability", "Bengaluru", "Full Time",
		)+freshteamLegacyCard(
			freshteamTestID2, "security-engineer",
			"Security Engineer", "Pune", "Contract",
		),
		2,
	)
	src := freshteamTestSource(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", req.Method)
		}
		if req.URL.Scheme != "https" || req.URL.Host != freshteamTestHost {
			t.Errorf("untrusted request URL %q", req.URL)
		}
		if req.Header.Get("User-Agent") == "" ||
			req.Header.Get("Accept") != "text/html,application/xhtml+xml" {
			t.Errorf("missing request headers: %v", req.Header)
		}
		switch req.URL.EscapedPath() {
		case "/jobs":
			listCalls.Add(1)
			return freshteamResponse(req, http.StatusOK, list), nil
		case "/jobs/" + freshteamTestID1 + "/platform-reliability":
			detailCalls.Add(1)
			return freshteamResponse(
				req, http.StatusOK,
				freshteamDetailPage(t, freshteamValidPosting()),
			), nil
		default:
			t.Fatalf("unexpected request path %q", req.URL.EscapedPath())
			return nil, nil
		}
	})

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listCalls.Load() != 1 || detailCalls.Load() != 0 || len(jobs) != 2 {
		t.Fatalf(
			"Fetch calls list=%d detail=%d jobs=%d",
			listCalls.Load(), detailCalls.Load(), len(jobs),
		)
	}
	if jobs[0].ID != "freshteam/acme/"+freshteamTestID1 ||
		jobs[0].Title != "Platform & Reliability" ||
		jobs[0].Location != "Bengaluru" ||
		jobs[0].EmploymentType != "Full Time" ||
		jobs[0].URL != freshteamTestBase+"/jobs/"+freshteamTestID1+"/platform-reliability" {
		t.Fatalf("unexpected first list job: %+v", jobs[0])
	}
	if jobs[1].ID != "freshteam/acme/"+freshteamTestID2 ||
		jobs[1].Location != "Pune" || jobs[1].EmploymentType != "Contract" {
		t.Fatalf("unexpected second list job: %+v", jobs[1])
	}

	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if detailCalls.Load() != 1 {
		t.Fatalf("detail calls = %d, want 1", detailCalls.Load())
	}
	if jobs[0].Title != "Platform & Reliability" ||
		jobs[0].Description != "Build & operate systems.\nShip safely." ||
		jobs[0].EmploymentType != "FULL_TIME" ||
		jobs[0].Location != "Bengaluru, India" ||
		jobs[0].URL != freshteamTestBase+"/jobs/"+freshteamTestID1+"/Platform%20%26%20Reliability" ||
		!jobs[0].PostedAt.Equal(time.Date(2026, 7, 30, 5, 25, 11, 0, time.UTC)) {
		t.Fatalf("unexpected detailed job: %+v", jobs[0])
	}
}

func TestFreshteamFactoryValidatesCompanySlug(t *testing.T) {
	source, err := New(
		"freshteam", "Acme",
		params.Map{"company_slug": "kaleyra-talent"},
		&http.Client{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if source.Company() != "Acme" {
		t.Fatalf("company = %q", source.Company())
	}
	if _, ok := source.(Detailer); !ok {
		t.Fatal("wrapped Freshteam source does not expose Detailer")
	}

	for _, slug := range []string{
		"", " Kaleyra", "Kaleyra", "kaleyra ", "-kaleyra", "kaleyra-",
		"kaleyra_talent", "kaleyra.talent", "https://kaleyra", "a/b",
		strings.Repeat("a", 64),
	} {
		t.Run(fmt.Sprintf("slug_%q", slug), func(t *testing.T) {
			_, err := New(
				"freshteam", "Acme",
				params.Map{"company_slug": slug},
				&http.Client{},
			)
			if err == nil {
				t.Fatalf("expected company_slug %q to fail", slug)
			}
		})
	}
}

func TestFreshteamFetchRejectsDriftDuplicatesAndTrustViolations(t *testing.T) {
	validCard := freshteamModernCard(
		freshteamTestID1, "platform-engineer",
		"Platform Engineer", "Bengaluru", "Full Time",
	)
	tests := []struct {
		name    string
		page    string
		wantErr string
	}{
		{
			name:    "missing list",
			page:    `<h3>Open Positions</h3>`,
			wantErr: "expected one job-role-list",
		},
		{
			name:    "multiple lists",
			page:    `<h3>Open Positions</h3><div class="job-role-list"></div><div class="job-role-list"></div>`,
			wantErr: "expected one job-role-list",
		},
		{
			name:    "missing open positions",
			page:    `<div class="job-role-list"></div><div>No jobs found</div>`,
			wantErr: "missing Open Positions",
		},
		{
			name: "duplicate ID",
			page: freshteamListPage(
				validCard+freshteamModernCard(
					freshteamTestID1, "renamed-role",
					"Renamed Role", "Pune", "Full Time",
				),
				2,
			),
			wantErr: "duplicate job ID",
		},
		{
			name: "changed path",
			page: freshteamListPage(
				`<a href="/positions/`+freshteamTestID1+`/platform"><div class="job-title">Platform</div></a>`,
				1,
			),
			wantErr: "invalid Freshteam posting path",
		},
		{
			name: "cross host",
			page: freshteamListPage(
				`<a href="https://evil.example/jobs/`+freshteamTestID1+`/platform"><div class="job-title">Platform</div></a>`,
				1,
			),
			wantErr: "must use HTTPS on",
		},
		{
			name: "plain HTTP",
			page: freshteamListPage(
				`<a href="http://`+freshteamTestHost+`/jobs/`+freshteamTestID1+`/platform"><div class="job-title">Platform</div></a>`,
				1,
			),
			wantErr: "must use HTTPS on",
		},
		{
			name: "invalid opaque ID",
			page: freshteamListPage(
				freshteamModernCard("short", "platform", "Platform", "Pune", "Full Time"),
				1,
			),
			wantErr: "invalid Freshteam posting path",
		},
		{
			name: "invalid slug",
			page: freshteamListPage(
				freshteamModernCard(freshteamTestID1, "Platform_Engineer", "Platform", "Pune", "Full Time"),
				1,
			),
			wantErr: "invalid Freshteam posting slug",
		},
		{
			name: "missing title",
			page: freshteamListPage(
				`<a href="/jobs/`+freshteamTestID1+`/platform"><div>Platform</div></a>`,
				1,
			),
			wantErr: "missing job-title",
		},
		{
			name: "orphan title",
			page: freshteamListPage(
				validCard+`<div class="job-title">Orphan</div>`,
				2,
			),
			wantErr: "parsed 1 cards but found 2 job titles",
		},
		{
			name:    "reported count mismatch",
			page:    freshteamListPage(validCard, 2),
			wantErr: "page reported 2",
		},
		{
			name: "pagination class",
			page: freshteamListPage(validCard, 1) +
				`<nav class="pagination"><a href="/jobs?page=2">Next</a></nav>`,
			wantErr: "unexpected pagination marker",
		},
		{
			name: "next relation",
			page: freshteamListPage(validCard, 1) +
				`<link href="/jobs?after=opaque" rel="next">`,
			wantErr: "unexpected pagination marker",
		},
		{
			name: "paging query",
			page: freshteamListPage(validCard, 1) +
				`<a href="/jobs?cursor=opaque">More</a>`,
			wantErr: "unexpected pagination marker",
		},
		{
			name:    "zero without marker",
			page:    freshteamListPage("", 0),
			wantErr: "no job cards and no empty-board marker",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src := freshteamTestSource(func(req *http.Request) (*http.Response, error) {
				return freshteamResponse(req, http.StatusOK, test.page), nil
			})
			_, err := src.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestFreshteamFetchAcceptsExplicitEmptyBoard(t *testing.T) {
	page := `<h3>Open Positions</h3><div class="job-role-list"><ul></ul></div>
<div class="no-jobs-found"><div class="not-found-title">No jobs found</div></div>`
	src := freshteamTestSource(func(req *http.Request) (*http.Response, error) {
		return freshteamResponse(req, http.StatusOK, page), nil
	})
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
	}
}

func TestFreshteamFetchStatusBodyLimitAndFinalURL(t *testing.T) {
	tests := []struct {
		name     string
		response func(*http.Request) *http.Response
		wantErr  string
	}{
		{
			name: "HTTP status",
			response: func(req *http.Request) *http.Response {
				return freshteamResponse(req, http.StatusServiceUnavailable, "maintenance")
			},
			wantErr: "Service Unavailable",
		},
		{
			name: "body limit",
			response: func(req *http.Request) *http.Response {
				return freshteamResponse(req, http.StatusOK, strings.Repeat("x", htmlBodyLimit+1))
			},
			wantErr: "response exceeds",
		},
		{
			name: "cross-host final URL",
			response: func(req *http.Request) *http.Response {
				resp := freshteamResponse(req, http.StatusOK, freshteamListPage("", 0))
				evil, _ := url.Parse("https://evil.example/jobs")
				resp.Request = &http.Request{URL: evil}
				return resp
			},
			wantErr: "untrusted final URL",
		},
		{
			name: "unexpected final path",
			response: func(req *http.Request) *http.Response {
				resp := freshteamResponse(req, http.StatusOK, freshteamListPage("", 0))
				other, _ := url.Parse(freshteamTestBase + "/jobs/search")
				resp.Request = &http.Request{URL: other}
				return resp
			},
			wantErr: "unexpected final URL",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src := freshteamTestSource(func(req *http.Request) (*http.Response, error) {
				return test.response(req), nil
			})
			_, err := src.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestFreshteamDetailRejectsInvalidJobsWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	src := freshteamTestSource(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return freshteamResponse(req, http.StatusOK, ""), nil
	})
	valid := model.Job{
		ID:             "freshteam/acme/" + freshteamTestID1,
		Company:        "Acme & Co",
		Title:          "Platform & Reliability",
		Location:       "Bengaluru",
		URL:            freshteamTestBase + "/jobs/" + freshteamTestID1 + "/platform-reliability",
		EmploymentType: "Full Time",
	}
	tests := []struct {
		name string
		job  *model.Job
	}{
		{name: "nil job", job: nil},
		{name: "wrong board", job: freshteamJobPointer(valid, func(j *model.Job) { j.ID = "freshteam/other/" + freshteamTestID1 })},
		{name: "invalid ID", job: freshteamJobPointer(valid, func(j *model.Job) { j.ID = "freshteam/acme/short" })},
		{name: "company mismatch", job: freshteamJobPointer(valid, func(j *model.Job) { j.Company = "Other" })},
		{name: "HTTP URL", job: freshteamJobPointer(valid, func(j *model.Job) { j.URL = "http://" + freshteamTestHost + "/jobs/" + freshteamTestID1 + "/platform" })},
		{name: "cross-host URL", job: freshteamJobPointer(valid, func(j *model.Job) { j.URL = "https://evil.example/jobs/" + freshteamTestID1 + "/platform" })},
		{name: "URL ID mismatch", job: freshteamJobPointer(valid, func(j *model.Job) { j.URL = freshteamTestBase + "/jobs/" + freshteamTestID2 + "/platform" })},
		{name: "URL query", job: freshteamJobPointer(valid, func(j *model.Job) { j.URL += "?from=list" })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := src.Detail(context.Background(), test.job); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid jobs caused %d requests", requests.Load())
	}
}

func freshteamJobPointer(job model.Job, mutate func(*model.Job)) *model.Job {
	mutate(&job)
	return &job
}

func TestFreshteamDetailHandlesClosedAndStatusFailuresAtomically(t *testing.T) {
	original := model.Job{
		ID:             "freshteam/acme/" + freshteamTestID1,
		Company:        "Acme & Co",
		Title:          "Platform & Reliability",
		Location:       "Bengaluru",
		URL:            freshteamTestBase + "/jobs/" + freshteamTestID1 + "/platform-reliability",
		EmploymentType: "Full Time",
		Description:    "list summary",
	}
	for _, status := range []int{
		http.StatusNotFound,
		http.StatusGone,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			src := freshteamTestSource(func(req *http.Request) (*http.Response, error) {
				return freshteamResponse(req, status, "status body"), nil
			})
			job := original
			err := src.Detail(context.Background(), &job)
			if err == nil {
				t.Fatal("expected detail status error")
			}
			if (status == http.StatusNotFound || status == http.StatusGone) &&
				!strings.Contains(err.Error(), "closed before detail fetch") {
				t.Fatalf("closed error = %v", err)
			}
			if job != original {
				t.Fatalf("detail mutated job on failure:\n got %+v\nwant %+v", job, original)
			}
		})
	}
}

func TestFreshteamDetailRejectsSchemaFailuresAtomically(t *testing.T) {
	original := model.Job{
		ID:             "freshteam/acme/" + freshteamTestID1,
		Company:        "Acme & Co",
		Title:          "Platform & Reliability",
		Location:       "Bengaluru",
		URL:            freshteamTestBase + "/jobs/" + freshteamTestID1 + "/platform-reliability",
		EmploymentType: "Full Time",
		Description:    "list summary",
	}
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		page    string
		wantErr string
	}{
		{
			name:    "no structured data",
			page:    `<html><h1>Platform</h1></html>`,
			wantErr: "no JobPosting JSON-LD",
		},
		{
			name:    "malformed structured data",
			page:    `<script type="application/ld+json">{"@type":</script>`,
			wantErr: "no JobPosting JSON-LD",
		},
		{
			name: "wrong schema type",
			mutate: func(posting map[string]any) {
				posting["@type"] = "WebPage"
			},
			wantErr: "no JobPosting JSON-LD",
		},
		{
			name: "missing canonical URL",
			mutate: func(posting map[string]any) {
				posting["url"] = ""
			},
			wantErr: "invalid canonical URL",
		},
		{
			name: "canonical cross host",
			mutate: func(posting map[string]any) {
				posting["url"] = "https://evil.example/jobs/" + freshteamTestID1 + "/platform"
			},
			wantErr: "invalid canonical URL",
		},
		{
			name: "canonical HTTP",
			mutate: func(posting map[string]any) {
				posting["url"] = "http://" + freshteamTestHost + "/jobs/" + freshteamTestID1 + "/platform"
			},
			wantErr: "invalid canonical URL",
		},
		{
			name: "canonical ID mismatch",
			mutate: func(posting map[string]any) {
				posting["url"] = freshteamTestBase + "/jobs/" + freshteamTestID2 + "/platform"
			},
			wantErr: "canonical URL contains job ID",
		},
		{
			name: "canonical query",
			mutate: func(posting map[string]any) {
				posting["url"] = posting["url"].(string) + "?tracking=true"
			},
			wantErr: "posting URL must not contain",
		},
		{
			name: "title mismatch",
			mutate: func(posting map[string]any) {
				posting["title"] = "Different title"
			},
			wantErr: "does not match list title",
		},
		{
			name: "missing description",
			mutate: func(posting map[string]any) {
				posting["description"] = " &nbsp; "
			},
			wantErr: "omitted description",
		},
		{
			name: "missing hiring organization",
			mutate: func(posting map[string]any) {
				delete(posting, "hiringOrganization")
			},
			wantErr: "omitted hiringOrganization",
		},
		{
			name: "hiring organization mismatch",
			mutate: func(posting map[string]any) {
				posting["hiringOrganization"] = map[string]any{"name": "Other Co"}
			},
			wantErr: "does not match company",
		},
		{
			name: "missing employment type",
			mutate: func(posting map[string]any) {
				posting["employmentType"] = ""
			},
			wantErr: "omitted employmentType",
		},
		{
			name: "employment mismatch",
			mutate: func(posting map[string]any) {
				posting["employmentType"] = "CONTRACT"
			},
			wantErr: "does not match list value",
		},
		{
			name: "missing location",
			mutate: func(posting map[string]any) {
				delete(posting, "jobLocation")
			},
			wantErr: "omitted jobLocation",
		},
		{
			name: "location mismatch",
			mutate: func(posting map[string]any) {
				posting["jobLocation"] = map[string]any{
					"address": map[string]any{"addressLocality": "Delhi"},
				}
			},
			wantErr: "does not match list value",
		},
		{
			name: "missing date",
			mutate: func(posting map[string]any) {
				posting["datePosted"] = ""
			},
			wantErr: "omitted datePosted",
		},
		{
			name: "invalid date",
			mutate: func(posting map[string]any) {
				posting["datePosted"] = "yesterday"
			},
			wantErr: "unsupported posting date",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page := test.page
			if page == "" {
				posting := freshteamValidPosting()
				if test.mutate != nil {
					test.mutate(posting)
				}
				page = freshteamDetailPage(t, posting)
			}
			src := freshteamTestSource(func(req *http.Request) (*http.Response, error) {
				return freshteamResponse(req, http.StatusOK, page), nil
			})
			job := original
			err := src.Detail(context.Background(), &job)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
			if job != original {
				t.Fatalf("detail mutated job on failure:\n got %+v\nwant %+v", job, original)
			}
		})
	}
}

func TestFreshteamDetailRejectsRedirectOffBoard(t *testing.T) {
	var calls atomic.Int32
	src := freshteamTestSource(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := freshteamResponse(req, http.StatusFound, "")
		resp.Header.Set("Location", "https://evil.example/jobs/"+freshteamTestID1+"/platform")
		return resp, nil
	})
	job := model.Job{
		ID:             "freshteam/acme/" + freshteamTestID1,
		Company:        "Acme & Co",
		Title:          "Platform & Reliability",
		Location:       "Bengaluru",
		URL:            freshteamTestBase + "/jobs/" + freshteamTestID1 + "/platform-reliability",
		EmploymentType: "Full Time",
	}
	err := src.Detail(context.Background(), &job)
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS on") {
		t.Fatalf("error = %v, want redirect trust error", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("redirect made %d transport calls, want 1", calls.Load())
	}
}

func TestExtractFreshteamPostingSupportsArraysAndOrganizationString(t *testing.T) {
	doc := `<script TYPE='application/ld+json'>[
{"@type":"WebPage"},
{"@type":"https://schema.org/JobPosting",
"url":"https://acme.freshteam.com/jobs/AbCdEf0123_-/platform",
"title":"Platform","description":"&lt;p&gt;Build.&lt;/p&gt;",
"datePosted":"2026-07-30","employmentType":["FULL_TIME","PERMANENT"],
"hiringOrganization":["", "Acme"],
"jobLocation":[{"address":{"addressLocality":"Pune","addressCountry":{"name":"India"}}}]}
]</script>`
	posting, err := extractFreshteamPosting(doc)
	if err != nil {
		t.Fatal(err)
	}
	if posting.Title != "Platform" || posting.Description != "<p>Build.</p>" ||
		posting.EmploymentType != "FULL_TIME, PERMANENT" ||
		posting.HiringOrganization != "Acme" ||
		posting.Location != "Pune, India" {
		t.Fatalf("posting = %+v", posting)
	}
}
