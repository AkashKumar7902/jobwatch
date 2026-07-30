package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestNationWithNamoRegistrationAndParams(t *testing.T) {
	t.Parallel()

	src, err := New("nationwithnamo", "Nation with Namo", params.Map{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("source type = %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*nationWithNamo)
	if !ok {
		t.Fatalf("wrapped source type = %T, want *nationWithNamo", wrapped.Source)
	}
	if implementation.base != nationWithNamoBaseURL {
		t.Errorf("base = %q, want %q", implementation.base, nationWithNamoBaseURL)
	}
	if implementation.client != http.DefaultClient {
		t.Error("nil client did not select http.DefaultClient")
	}
	if Identity(src) != "nationwithnamo" ||
		StatePrefix(src) != "nationwithnamo/gilp-impact-fellowship/" {
		t.Fatalf("identity/prefix = %q/%q", Identity(src), StatePrefix(src))
	}
	if implementation.now == nil {
		t.Error("clock is nil")
	}

	_, err = New(
		"nationwithnamo",
		"Nation with Namo",
		params.Map{"z": "1", "a": "2"},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "got a, z") {
		t.Fatalf("unknown params error = %v, want sorted parameter names", err)
	}
}

func TestNationWithNamoFetchesCurrentCohortWithFullDetails(t *testing.T) {
	landing := nationWithNamoTestLanding("Open for all")
	detail := nationWithNamoTestDetail()
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Accept"); got != "text/html,application/xhtml+xml" {
			t.Errorf("Accept = %q", got)
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "jobwatch") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, landing)
		case "/apply.html":
			fmt.Fprint(w, nationWithNamoTestApplication())
		case "/fellowship/":
			fmt.Fprint(w, detail)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src := &nationWithNamo{
		company: "Nation with Namo",
		base:    server.URL,
		client:  server.Client(),
		now: func() time.Time {
			return time.Date(2026, time.July, 30, 12, 0, 0, 0, nationWithNamoIST)
		},
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opensAt := time.Date(2026, time.July, 17, 0, 0, 0, 0, nationWithNamoIST)
	want := []model.Job{{
		ID:             "nationwithnamo/gilp-impact-fellowship/2027",
		Company:        "Nation with Namo",
		Title:          nationWithNamoTitle,
		Location:       "India (Hybrid)",
		URL:            server.URL + "/apply.html",
		EmploymentType: "Fellowship",
		Description: joinDescriptionParts(
			"India’s first structured, campus-based fellowship enabling students to deliver meaningful impact through high-priority nation-building projects",
			nationWithNamoTestWorkText(),
			strings.Join(nationWithNamoTestFAQParts(), "\n\n"),
			"Application window: 17 July 2026 through 4 October 2026 (12:00 PM IST deadline). Cohort: 2027.",
		),
		PostedAt: opensAt,
	}}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs =\n%+v\nwant:\n%+v", jobs, want)
	}
	if !reflect.DeepEqual(requests, []string{"/", "/apply.html", "/fellowship/"}) {
		t.Fatalf("requests = %v, want landing, application, then detail", requests)
	}
}

func TestNationWithNamoReturnsExplicitlyEmptyOutsideApplicationWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		now    time.Time
	}{
		{
			name:   "before opening date",
			status: "Open for all",
			now:    time.Date(2026, time.July, 16, 23, 59, 59, 0, nationWithNamoIST),
		},
		{
			name:   "after deadline",
			status: "Open for all",
			now:    time.Date(2026, time.October, 4, 12, 0, 1, 0, nationWithNamoIST),
		},
		{
			name:   "explicit closed state",
			status: "Applications are closed",
			now:    time.Date(2026, time.July, 30, 12, 0, 0, 0, nationWithNamoIST),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/" {
					t.Errorf("unexpected detail request %q for inactive cohort", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, nationWithNamoTestLanding(test.status))
			}))
			defer server.Close()
			src := &nationWithNamo{
				company: "Nation with Namo",
				base:    server.URL,
				client:  server.Client(),
				now:     func() time.Time { return test.now },
			}
			jobs, err := src.Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if jobs == nil || len(jobs) != 0 {
				t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want landing only", requests)
			}
		})
	}
}

func TestNationWithNamoIncludesDeadlineInstant(t *testing.T) {
	t.Parallel()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, nationWithNamoTestLanding("Open for all"))
		case "/apply.html":
			fmt.Fprint(w, nationWithNamoTestApplication())
		case "/fellowship/":
			fmt.Fprint(w, nationWithNamoTestDetail())
		}
	}))
	defer server.Close()
	src := &nationWithNamo{
		company: "Nation with Namo",
		base:    server.URL,
		client:  server.Client(),
		now: func() time.Time {
			return time.Date(2026, time.October, 4, 12, 0, 0, 0, nationWithNamoIST)
		},
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || requests != 3 {
		t.Fatalf("jobs=%d requests=%d, want one current job and three requests", len(jobs), requests)
	}
}

func TestNationWithNamoExplicitApplicationClosureWins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		landing string
		closed  string
	}{
		{
			name:    "active landing action",
			landing: nationWithNamoTestLanding("Open for all"),
			closed:  "All Aplications are Closed",
		},
		{
			name: "commented or missing landing action",
			landing: strings.Replace(
				nationWithNamoTestLanding("Open for all"),
				nationWithNamoTestApplyAnchor(),
				`<!--`+nationWithNamoTestApplyAnchor()+`-->`,
				1,
			),
			closed: "All Applications are Closed",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.Path)
				w.Header().Set("Content-Type", "text/html")
				switch r.URL.Path {
				case "/":
					fmt.Fprint(w, test.landing)
				case "/apply.html":
					fmt.Fprintf(w, `<div class="apply_c">%s</div>`, test.closed)
				default:
					t.Errorf("unexpected request to %q after explicit closure", r.URL.Path)
				}
			}))
			defer server.Close()
			src := &nationWithNamo{
				company: "Nation with Namo",
				base:    server.URL,
				client:  server.Client(),
				now: func() time.Time {
					return time.Date(2026, time.July, 30, 12, 0, 0, 0, nationWithNamoIST)
				},
			}
			jobs, err := src.Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if jobs == nil || len(jobs) != 0 {
				t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
			}
			if !reflect.DeepEqual(requests, []string{"/", "/apply.html"}) {
				t.Fatalf("requests = %v, want landing and authoritative closed page", requests)
			}
		})
	}
}

func TestNationWithNamoRejectsOpenFormWithoutLandingAction(t *testing.T) {
	t.Parallel()

	landing := strings.Replace(
		nationWithNamoTestLanding("Open for all"),
		nationWithNamoTestApplyAnchor(),
		"",
		1,
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			fmt.Fprint(w, landing)
			return
		}
		fmt.Fprint(w, nationWithNamoTestApplication())
	}))
	defer server.Close()
	src := &nationWithNamo{
		company: "Nation with Namo",
		base:    server.URL,
		client:  server.Client(),
		now: func() time.Time {
			return time.Date(2026, time.July, 30, 12, 0, 0, 0, nationWithNamoIST)
		},
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil ||
		!strings.Contains(err.Error(), "open application has no active Apply Now action") {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and missing action error", jobs, err)
	}
}

func TestNationWithNamoAcceptsFirstPartyJavaScriptApplyAction(t *testing.T) {
	t.Parallel()

	document := strings.Replace(
		nationWithNamoTestLanding("Open for all"),
		nationWithNamoTestApplyAnchor(),
		`<a href="javascript:void(0)" `+
			`onclick="localStorage.clear();localStorage.setItem('position','GILP: Impact Fellowship');`+
			`window.location.href='./apply.html'">Apply Now</a>`,
		1,
	)
	schedule, err := parseNationWithNamoSchedule(document)
	if err != nil {
		t.Fatal(err)
	}
	if !schedule.hasApplyAction {
		t.Fatal("JavaScript Apply Now action was not recognized")
	}
}

func TestNationWithNamoRejectsLandingPageDrift(t *testing.T) {
	t.Parallel()

	valid := nationWithNamoTestLanding("Open for all")
	canonicalCalendar := nationWithNamoTestCalendar("Open for all")
	tests := []struct {
		name     string
		document string
		wantErr  string
	}{
		{
			name:     "unterminated comment",
			document: valid + "<!--",
			wantErr:  "unterminated HTML comment",
		},
		{
			name: "duplicate visible hero",
			document: valid +
				`<h1 class="hero_section_heading">GILP: Impact Fellowship</h1>`,
			wantErr: "expected one fellowship hero heading",
		},
		{
			name:     "changed hero",
			document: strings.Replace(valid, nationWithNamoTitle, "Other Program", 1),
			wantErr:  "hero heading is",
		},
		{
			name:     "missing deadline section",
			document: strings.Replace(valid, `id="app-deadlines"`, `id="other"`, 1),
			wantErr:  "div#app-deadlines",
		},
		{
			name:     "missing noon contract",
			document: strings.Replace(valid, "12:00 PM IST", "the stated time", 1),
			wantErr:  "deadline contract is missing",
		},
		{
			name:     "nonconsecutive cycle",
			document: strings.Replace(valid, "2026-2027 Deadlines", "2026-2028 Deadlines", 1),
			wantErr:  "not consecutive",
		},
		{
			name:     "cohort mismatch",
			document: strings.Replace(valid, "batch of 2027", "batch of 2028", 1),
			wantErr:  "does not match application cycle",
		},
		{
			name: "duplicate canonical calendar",
			document: strings.Replace(
				valid,
				canonicalCalendar,
				canonicalCalendar+canonicalCalendar,
				1,
			),
			wantErr: "more than one canonical application calendar",
		},
		{
			name:     "changed label",
			document: strings.Replace(valid, "Deadline for application", "Apply by", 1),
			wantErr:  "calendar label",
		},
		{
			name:     "missing value",
			document: strings.Replace(valid, "<div class=\"day\">9th Oct 2026 Friday</div>", "<div class=\"day\"></div>", 1),
			wantErr:  "calendar value 3 is empty",
		},
		{
			name:     "unsupported status",
			document: nationWithNamoTestLanding("Round one"),
			wantErr:  "unsupported application status",
		},
		{
			name:     "invalid ordinal",
			document: strings.Replace(valid, "17th July 2026", "17st July 2026", 1),
			wantErr:  "invalid ordinal suffix",
		},
		{
			name:     "date outside cycle",
			document: strings.Replace(valid, "17th July 2026", "17th July 2025", 1),
			wantErr:  "dates do not match cycle",
		},
		{
			name: "duplicate apply action",
			document: strings.Replace(
				valid,
				nationWithNamoTestApplyAnchor(),
				nationWithNamoTestApplyAnchor()+nationWithNamoTestApplyAnchor(),
				1,
			),
			wantErr: "at most one active Apply Now action",
		},
		{
			name: "off-origin apply action",
			document: strings.Replace(
				valid,
				`href="./apply.html"`,
				`href="https://example.com/apply.html"`,
				1,
			),
			wantErr: "leaves first-party origin",
		},
		{
			name: "JavaScript action without apply target",
			document: strings.Replace(
				valid,
				nationWithNamoTestApplyAnchor(),
				`<a href="javascript:void(0)" onclick="window.location.href='./other.html'">Apply Now</a>`,
				1,
			),
			wantErr: "does not target /apply.html",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schedule, err := parseNationWithNamoSchedule(test.document)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parse schedule = (%+v, %v), want error containing %q", schedule, err, test.wantErr)
			}
		})
	}
}

func TestNationWithNamoRejectsDetailPageDrift(t *testing.T) {
	t.Parallel()

	valid := nationWithNamoTestDetail()
	schedule, err := parseNationWithNamoSchedule(nationWithNamoTestLanding("Open for all"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		document string
		wantErr  string
	}{
		{
			name:     "missing hero",
			document: strings.ReplaceAll(valid, "hero_section_heading", "other-heading"),
			wantErr:  "fellowship hero heading",
		},
		{
			name:     "missing summary",
			document: strings.Replace(valid, "hero_section_p", "other-summary", 1),
			wantErr:  "hero summary",
		},
		{
			name:     "changed summary",
			document: strings.Replace(valid, "nation-building projects", "other work", 1),
			wantErr:  "hero summary changed",
		},
		{
			name:     "missing work section",
			document: strings.Replace(valid, `id="what_you_work"`, `id="other-work"`, 1),
			wantErr:  "section#what_you_work",
		},
		{
			name:     "missing work marker",
			document: strings.Replace(valid, "consulting-style bootcamp", "training", 1),
			wantErr:  "work-details section omitted",
		},
		{
			name: "too few FAQs",
			document: strings.Replace(
				valid,
				nationWithNamoTestFAQCard(
					"Will I receive any stipend or financial aid?",
					"Fellows receive a competitive stipend.",
				)+nationWithNamoTestFAQCard(
					"What happens after the fellowship?",
					"Top fellows may receive a role.",
				),
				"",
				1,
			),
			wantErr: "FAQ entry count 7",
		},
		{
			name: "missing required FAQ",
			document: strings.Replace(
				valid,
				"Who can apply for the fellowship?",
				"Who is this program for?",
				1,
			),
			wantErr: "required FAQ question",
		},
		{
			name: "duplicate question",
			document: strings.Replace(
				valid,
				"Do I need prior experience in consulting or policy?",
				"Who can apply for the fellowship?",
				1,
			),
			wantErr: "duplicate FAQ question",
		},
		{
			name:     "missing hybrid evidence",
			document: strings.Replace(valid, "hybrid model", "flexible model", 1),
			wantErr:  "hybrid-work statement",
		},
		{
			name: "malformed FAQ card",
			document: strings.Replace(
				valid,
				`class="card-body"`,
				`class="other-body"`,
				1,
			),
			wantErr: "exactly one question and answer",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			description, err := parseNationWithNamoDetail(test.document, schedule)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parse detail = (%q, %v), want error containing %q", description, err, test.wantErr)
			}
		})
	}
}

func TestNationWithNamoFetchIsAtomicWhenDetailFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			fmt.Fprint(w, nationWithNamoTestLanding("Open for all"))
			return
		}
		if r.URL.Path == "/apply.html" {
			fmt.Fprint(w, nationWithNamoTestApplication())
			return
		}
		http.Error(w, "temporary failure", http.StatusBadGateway)
	}))
	defer server.Close()
	src := &nationWithNamo{
		company: "Nation with Namo",
		base:    server.URL,
		client:  server.Client(),
		now: func() time.Time {
			return time.Date(2026, time.July, 30, 12, 0, 0, 0, nationWithNamoIST)
		},
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and detail HTTP error", jobs, err)
	}
}

func TestNationWithNamoRejectsInvalidHTTPResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name: "status", status: http.StatusBadGateway,
			contentType: "text/html", body: "upstream down", wantErr: "502 Bad Gateway",
		},
		{
			name: "content type", status: http.StatusOK,
			contentType: "application/json", body: `{}`, wantErr: "unexpected Content-Type",
		},
		{
			name: "body limit", status: http.StatusOK,
			contentType: "text/html", body: strings.Repeat("x", nationWithNamoBodyLimit+1),
			wantErr: "response exceeds",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.contentType != "" {
					w.Header().Set("Content-Type", test.contentType)
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			src := &nationWithNamo{
				company: "Nation with Namo",
				base:    server.URL,
				client:  server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Fetch = (%#v, %v), want error containing %q", jobs, err, test.wantErr)
			}
		})
	}
}

func TestParseNationWithNamoApplicationStates(t *testing.T) {
	t.Parallel()

	const applicationURL = "https://gilp.nationwithnamo.com/apply.html"
	for _, closed := range []string{"All Aplications are Closed", "All Applications are Closed"} {
		open, err := parseNationWithNamoApplication(
			`<div class="apply_c">`+closed+`</div>`,
			applicationURL,
		)
		if err != nil || open {
			t.Errorf("closed state %q = (%t, %v), want false, nil", closed, open, err)
		}
	}
	open, err := parseNationWithNamoApplication(nationWithNamoTestApplication(), applicationURL)
	if err != nil || !open {
		t.Fatalf("open application = (%t, %v), want true, nil", open, err)
	}

	valid := nationWithNamoTestApplication()
	tests := []struct {
		name     string
		document string
		wantErr  string
	}{
		{
			name: "closed mixed with form",
			document: strings.Replace(
				valid,
				`<div class="apply_c">`,
				`<div class="apply_c">All Applications are Closed`,
				1,
			),
			wantErr: "mixes an explicit closed state",
		},
		{
			name:     "missing container",
			document: strings.Replace(valid, "apply_c", "other", 1),
			wantErr:  "div.apply_c",
		},
		{
			name:     "missing form",
			document: `<div class="apply_c">Applications pending</div>`,
			wantErr:  "has 0 forms",
		},
		{
			name:     "GET form",
			document: strings.Replace(valid, `method="post"`, `method="get"`, 1),
			wantErr:  "want POST",
		},
		{
			name: "off-origin form",
			document: strings.Replace(
				valid,
				`action="/applications"`,
				`action="https://example.com/applications"`,
				1,
			),
			wantErr: "leaves first-party origin",
		},
		{
			name:     "wrong role",
			document: strings.Replace(valid, nationWithNamoTitle, "Other Program", 1),
			wantErr:  "does not identify",
		},
		{
			name:     "missing submit",
			document: strings.Replace(valid, `type="submit"`, `type="button"`, 1),
			wantErr:  "0 submit buttons",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			open, err := parseNationWithNamoApplication(test.document, applicationURL)
			if err == nil || open || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parse application = (%t, %v), want error containing %q", open, err, test.wantErr)
			}
		})
	}
}

func TestNationWithNamoRejectsRedirectedResponse(t *testing.T) {
	t.Parallel()

	var redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/other", http.StatusFound)
			return
		}
		redirected.Store(true)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, nationWithNamoTestLanding("Open for all"))
	}))
	defer server.Close()
	src := &nationWithNamo{
		company: "Nation with Namo",
		base:    server.URL,
		client:  server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "302 Found") {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and redirect error", jobs, err)
	}
	if redirected.Load() {
		t.Fatal("redirect destination was requested")
	}
}

func TestFetchNationWithNamoPageRejectsUnsafeURLs(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"ftp://gilp.nationwithnamo.com/",
		"https://user@gilp.nationwithnamo.com/",
		"https://gilp.nationwithnamo.com/?page=1",
		"https://gilp.nationwithnamo.com/#openings",
		"https://gilp.nationwithnamo.com/?",
	} {
		endpoint := endpoint
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			body, err := fetchNationWithNamoPage(context.Background(), http.DefaultClient, endpoint)
			if err == nil || body != nil || !strings.Contains(err.Error(), "invalid first-party URL") {
				t.Fatalf("fetch = (%q, %v), want URL validation error", body, err)
			}
		})
	}
}

func TestParseNationWithNamoDateValidation(t *testing.T) {
	t.Parallel()

	valid := []string{
		"1st January 2026",
		"2nd February 2026 Monday",
		"3rd March 2026",
		"4th April 2026",
		"11th May 2026",
		"12th June 2026",
		"13th July 2026",
		"21st August 2026",
		"22nd September 2026",
		"23rd October 2026",
		"31st December 2026",
		"29th February 2028",
	}
	for _, raw := range valid {
		if _, err := parseNationWithNamoDate(raw, 0); err != nil {
			t.Errorf("parse %q: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"1th January 2026",
		"31st June 2026",
		"29th February 2027",
		"4th Smarch 2026",
		"2026-07-17",
	} {
		if parsed, err := parseNationWithNamoDate(raw, 0); err == nil {
			t.Errorf("parse %q = %v, want error", raw, parsed)
		}
	}
}

func nationWithNamoTestLanding(status string) string {
	return `<!doctype html><html><body>` +
		`<!--<h1 class="hero_section_heading">Archived Program</h1>-->` +
		`<h1 class="hero_section_heading"><b>GILP: Impact Fellowship</b></h1>` +
		nationWithNamoTestApplyAnchor() +
		`<div id="app-deadlines">` +
		`<h2>Application deadlines</h2>` +
		`<p>Candidates must submit their completed application and latest resume by 12:00 PM IST on the deadline date of their chosen application round.</p>` +
		`<p>Application timelines for the GILP Impact Fellowship Recruitment<br><b>2026-2027 Deadlines</b></p>` +
		nationWithNamoTestCalendar(status) +
		`<ul><li>There are limited openings available for the GILP: Impact Fellowship batch of 2027. The openings will be filled on a first come, first served basis.</li></ul>` +
		`</div></body></html>`
}

func nationWithNamoTestApplyAnchor() string {
	return `<a class="btn btn-plain" href="./apply.html">Apply Now</a>`
}

func nationWithNamoTestApplication() string {
	return `<!doctype html><html><body><div class="apply_c">` +
		`<form method="post" action="/applications">` +
		`<h1>GILP: Impact Fellowship</h1>` +
		`<button type="submit">Submit Application</button>` +
		`</form></div></body></html>`
}

func nationWithNamoTestCalendar(status string) string {
	return `<div class="calendar">` +
		`<div class="days">Status</div>` +
		`<div class="days">Applications open</div>` +
		`<div class="days">Deadline for application</div>` +
		`<div class="days">Shortlist announcement</div>` +
		`<div class="days">Interview process</div>` +
		`<div class="days">Offer notification</div>` +
		`<div class="day">` + status + `</div>` +
		`<div class="day">17th July 2026 Friday</div>` +
		`<div class="day">4th Oct 2026 Sunday</div>` +
		`<div class="day">9th Oct 2026 Friday</div>` +
		`<div class="day">Start Date: 12th Oct 2026 End Date: 16th Oct 2026</div>` +
		`<div class="day">19th Oct 2026 Monday</div>` +
		`</div>`
}

func nationWithNamoTestDetail() string {
	var faq strings.Builder
	for _, part := range nationWithNamoTestFAQParts() {
		question, answer, _ := strings.Cut(part, "\n")
		faq.WriteString(nationWithNamoTestFAQCard(question, answer))
	}
	return `<!doctype html><html><body>` +
		`<!--<h1 class="hero_section_heading">Archived Fellowship</h1>-->` +
		`<h1 class="hero_section_heading"><b>GILP: Impact Fellowship</b></h1>` +
		`<p class="hero_section_p">India’s first structured, campus-based fellowship enabling students to deliver meaningful impact through high-priority nation-building projects</p>` +
		`<section id="what_you_work">` + nationWithNamoTestWorkHTML() + `</section>` +
		`<section><h2>Frequently asked questions</h2>` + faq.String() + `</section>` +
		`</body></html>`
}

func nationWithNamoTestWorkHTML() string {
	return `<h1>Focus on creating meaningful impact</h1>` +
		`<p>The GILP: Impact Fellowship is a first-of-its-kind program focused on youth leadership.</p>` +
		`<p>The program combines a consulting-style bootcamp, in-office immersions, hands-on fieldwork, and structured mentorship.</p>` +
		`<h4>Understand India at its core</h4><p>Build a foundational understanding.</p>` +
		`<h4>Consulting residential bootcamp and immersion experiences</h4><p>Apply core consulting skills.</p>` +
		`<h4>Insights built straight from ground up</h4><p>Work on real issues with real people.</p>` +
		`<h4>Mentorship and career acceleration</h4><p>Top-performing fellows gain pre-placement opportunities (PPOs) at Nation with NaMo.</p>`
}

func nationWithNamoTestWorkText() string {
	return cleanHTMLFragment(nationWithNamoTestWorkHTML())
}

func nationWithNamoTestFAQParts() []string {
	return []string{
		"Who can apply for the fellowship?\nPre-final year undergraduate students from any discipline may apply.",
		"Do I need prior experience in consulting or policy?\nNo. Applicants are trained from the ground up.",
		"Can I pursue this with my academics?\nYes. The fellowship uses a hybrid model alongside the academic schedule.",
		"What is the weekly time commitment required for the fellowship?\nFellows should dedicate 6-10 hours per week.",
		"What is the planned duration for the fellowship?\nThe fellowship lasts nine months.",
		"When will the applications close for the Fellowship?\nApplications close by mid October.",
		"What are the eligibility criteria for applying to the fellowship?\nApplicants must have no active backlogs.",
		"Will I receive any stipend or financial aid?\nFellows receive a competitive stipend.",
		"What happens after the fellowship?\nTop fellows may receive a role.",
	}
}

func nationWithNamoTestFAQCard(question, answer string) string {
	return `<div class="card"><button class="btn btn-collapse">` + question +
		`</button><div class="collapse"><div class="card-body">` + answer +
		`</div></div></div>`
}
