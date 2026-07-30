package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	testHyperVergeFormID  = "1FAIpQLSfbwbuossfACp1p7IHpXegk5k0E5yo0AFcXat4MTsxQAyEcog"
	testHyperVergeFormURL = "https://docs.google.com/forms/d/e/" + testHyperVergeFormID + "/viewform"
)

func TestHyperVergeRegistrationAndParams(t *testing.T) {
	t.Parallel()

	src, err := New("hyperverge", "HyperVerge", params.Map{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("source type = %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*hyperVerge)
	if !ok {
		t.Fatalf("wrapped source type = %T, want *hyperVerge", wrapped.Source)
	}
	if implementation.base != "https://hyperverge.co" {
		t.Errorf("base = %q", implementation.base)
	}
	if implementation.linkedinBase != hyperVergeLinkedInGuestBase {
		t.Errorf("LinkedIn base = %q", implementation.linkedinBase)
	}
	if implementation.googleFormsBase != hyperVergeGoogleFormsBase {
		t.Errorf("Google Forms base = %q", implementation.googleFormsBase)
	}
	if implementation.client != http.DefaultClient {
		t.Error("nil client did not select http.DefaultClient")
	}

	_, err = New(
		"hyperverge",
		"HyperVerge",
		params.Map{"z": "1", "a": "2"},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "got a, z") {
		t.Fatalf("unknown params error = %v, want sorted parameter names", err)
	}
}

func TestHyperVergeFetchEmitsOnlyAnonymousActionableRoles(t *testing.T) {
	activeJob := hyperVergeTestJob(
		"Deep Learning / Machine Learning Intern",
		"Bengaluru",
		"Full-time",
		"https://www.linkedin.com/jobs/view/4442287503/",
	)
	closedJob := hyperVergeTestJob(
		"Engineering Manager",
		"Bengaluru",
		"Full-time",
		"https://www.linkedin.com/jobs/view/4412659818/",
	)
	document := hyperVergeTestDocument(
		[]string{"engineering", "deeplearning", "product"},
		hyperVergeTestPane(
			"engineering",
			`<!--<div class="job-col">`+
				hyperVergeTestJob(
					"Archived Role",
					"Bengaluru",
					"Full-time",
					"https://www.linkedin.com/jobs/view/9999999999/",
				)+`</div>-->`+
				activeJob+
				closedJob,
		)+
			hyperVergeTestPane(
				"deeplearning",
				activeJob,
			)+
			hyperVergeTestPane(
				"product",
				`<div class="job-openings-content no-openings text-center">`+
					`<p>Currently no openings under this role</p></div>`,
			),
	)

	description := strings.TrimSpace(strings.Repeat(
		"Build and evaluate production machine-learning systems with the research team. ",
		3,
	))
	var (
		requestsMu sync.Mutex
		requests   = make(map[string]int)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsMu.Lock()
		requests[r.URL.Path]++
		requestsMu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Accept") != "text/html,application/xhtml+xml" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "jobwatch") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		switch r.URL.Path {
		case "/careers/":
			fmt.Fprint(w, document)
		case "/linkedin/4442287503":
			fmt.Fprint(
				w,
				hyperVergeTestLinkedInDetail(
					"4442287503",
					"Machine Learning Research Intern (LLMs & Computer Vision)",
					"HyperVerge",
					description,
					false,
				),
			)
		case "/linkedin/4412659818":
			fmt.Fprint(
				w,
				hyperVergeTestLinkedInDetail(
					"4412659818",
					"Full Stack Engineer",
					"HyperVerge",
					description,
					true,
				),
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src := &hyperVerge{
		company:         "HyperVerge",
		base:            server.URL,
		linkedinBase:    server.URL + "/linkedin",
		googleFormsBase: server.URL + "/forms",
		client:          server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Job{
		{
			ID:             "hyperverge/linkedin/4442287503",
			Company:        "HyperVerge",
			Title:          "Machine Learning Research Intern (LLMs & Computer Vision)",
			Location:       "Bengaluru",
			URL:            "https://www.linkedin.com/jobs/view/4442287503/",
			EmploymentType: "Full-time",
			Description:    description,
		},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs =\n%+v\nwant:\n%+v", jobs, want)
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	wantRequests := map[string]int{
		"/careers/":            1,
		"/linkedin/4442287503": 1,
		"/linkedin/4412659818": 1,
	}
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
}

func TestHyperVergeAllowsExplicitlyEmptyBoard(t *testing.T) {
	t.Parallel()

	src := &hyperVerge{company: "HyperVerge"}
	jobs, err := src.parseCareersPage(hyperVergeTestDocument(
		[]string{"engineering"},
		hyperVergeTestPane(
			"engineering",
			`<div class="job-openings-content no-openings">`+
				`<p>Currently no openings under this role</p></div>`,
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("jobs = %#v, want non-nil empty slice", jobs)
	}
}

func TestHyperVergeRejectsIncompleteOrDriftedMarkup(t *testing.T) {
	t.Parallel()

	validJob := hyperVergeTestJob(
		"Engineering Manager",
		"Bengaluru",
		"Full-time",
		"https://www.linkedin.com/jobs/view/4412659818/",
	)
	valid := hyperVergeTestDocument(
		[]string{"engineering"},
		hyperVergeTestPane("engineering", validJob),
	)
	tests := []struct {
		name     string
		document string
		wantErr  string
	}{
		{
			name:     "missing section",
			document: `<html><body></body></html>`,
			wantErr:  "expected one section",
		},
		{
			name:     "duplicate section",
			document: valid + valid,
			wantErr:  "expected one section",
		},
		{
			name: "missing selector",
			document: `<section class="section-openings" id="jobOpenings">` +
				`<div class="tab-content">` +
				hyperVergeTestPane("engineering", validJob) +
				`</div></section>`,
			wantErr: "selector #selectBox is missing",
		},
		{
			name: "duplicate option",
			document: `<section class="section-openings" id="jobOpenings">` +
				`<select id="selectBox"><option value="engineering">Engineering</option>` +
				`<option value="engineering">Engineering</option></select>` +
				`<div class="tab-content">` +
				hyperVergeTestPane("engineering", validJob) +
				`</div></section>`,
			wantErr: "duplicate department option",
		},
		{
			name: "pane count mismatch",
			document: hyperVergeTestDocument(
				[]string{"engineering", "product"},
				hyperVergeTestPane("engineering", validJob),
			),
			wantErr: "declares 2 panes",
		},
		{
			name: "pane missing role",
			document: hyperVergeTestDocument(
				[]string{"engineering"},
				`<div class="tab-pane" id="engineering">`+validJob+`</div>`,
			),
			wantErr: "invalid tag, role, or id",
		},
		{
			name: "pane without jobs or empty state",
			document: hyperVergeTestDocument(
				[]string{"engineering"},
				hyperVergeTestPane("engineering", `<p>Nothing here</p>`),
			),
			wantErr: "neither jobs nor an explicit empty state",
		},
		{
			name: "pane mixes jobs and empty state",
			document: hyperVergeTestDocument(
				[]string{"engineering"},
				hyperVergeTestPane(
					"engineering",
					validJob+`<div class="no-openings">`+
						`Currently no openings under this role</div>`,
				),
			),
			wantErr: "mixes jobs with an empty state",
		},
		{
			name: "missing title",
			document: hyperVergeTestDocument(
				[]string{"engineering"},
				hyperVergeTestPane(
					"engineering",
					strings.Replace(validJob, `class="job-title"`, `class="other-title"`, 1),
				),
			),
			wantErr: "expected one job title",
		},
		{
			name: "missing employment type",
			document: hyperVergeTestDocument(
				[]string{"engineering"},
				hyperVergeTestPane(
					"engineering",
					strings.Replace(validJob, `<span>Full-time</span>`, "", 1),
				),
			),
			wantErr: "employment type spans",
		},
		{
			name: "unsupported apply host",
			document: hyperVergeTestDocument(
				[]string{"engineering"},
				hyperVergeTestPane(
					"engineering",
					strings.Replace(
						validJob,
						"https://www.linkedin.com/jobs/view/4412659818/",
						"https://example.com/jobs/4412659818",
						1,
					),
				),
			),
			wantErr: "unsupported apply URL host",
		},
		{
			name: "conflicting duplicate id",
			document: hyperVergeTestDocument(
				[]string{"engineering"},
				hyperVergeTestPane(
					"engineering",
					validJob+strings.Replace(validJob, "Engineering Manager", "Other Role", 1),
				),
			),
			wantErr: "reused with conflicting fields",
		},
		{
			name:     "unterminated comment",
			document: valid + "<!-- archived",
			wantErr:  "unterminated HTML comment",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			src := &hyperVerge{company: "HyperVerge"}
			jobs, err := src.parseCareersPage(test.document)
			if err == nil || jobs != nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf(
					"parseCareersPage = (%#v, %v), want nil jobs and error containing %q",
					jobs,
					err,
					test.wantErr,
				)
			}
		})
	}
}

func TestHyperVergeLinkedInDetailStates(t *testing.T) {
	t.Parallel()

	description := strings.TrimSpace(strings.Repeat(
		"Research, build, evaluate, and deploy machine-learning models for identity products. ",
		3,
	))
	listJob := model.Job{
		ID:             "hyperverge/linkedin/4442287503",
		Company:        "HyperVerge",
		Title:          "Deep Learning / Machine Learning Intern",
		Location:       "Bengaluru",
		URL:            "https://www.linkedin.com/jobs/view/4442287503/",
		EmploymentType: "Full-time",
	}
	active := hyperVergeTestLinkedInDetail(
		"4442287503",
		"Machine Learning Research Intern (LLMs & Computer Vision)",
		"HyperVerge",
		description,
		false,
	)
	job, actionable, err := parseHyperVergeLinkedInDetail(listJob, "4442287503", active)
	if err != nil {
		t.Fatal(err)
	}
	if !actionable || job.Title != "Machine Learning Research Intern (LLMs & Computer Vision)" ||
		job.Description != description {
		t.Fatalf("active detail = (%+v, %t), want hydrated actionable job", job, actionable)
	}

	closed := hyperVergeTestLinkedInDetail(
		"4442287503",
		"Unrelated stale title",
		"HyperVerge",
		description,
		true,
	)
	job, actionable, err = parseHyperVergeLinkedInDetail(listJob, "4442287503", closed)
	if err != nil || actionable || job != (model.Job{}) {
		t.Fatalf("closed detail = (%+v, %t, %v), want zero job, false, nil", job, actionable, err)
	}

	tests := []struct {
		name     string
		document string
		wantErr  string
	}{
		{
			name:     "unterminated comment",
			document: active + "<!--",
			wantErr:  "unterminated HTML comment",
		},
		{
			name:     "missing title",
			document: strings.Replace(active, "topcard__title", "other-title", 1),
			wantErr:  "top-card title link and heading",
		},
		{
			name:     "wrong job id",
			document: strings.Replace(active, "-4442287503?trk", "-9999999999?trk", 1),
			wantErr:  "does not identify job",
		},
		{
			name:     "wrong organization",
			document: strings.Replace(active, ">HyperVerge</a>", ">Other Company</a>", 1),
			wantErr:  "organization is",
		},
		{
			name: "lookalike organization host",
			document: strings.Replace(
				active,
				"https://www.linkedin.com/company/hyperverge",
				"https://www.linkedin.com.example/company/hyperverge",
				1,
			),
			wantErr: "is not LinkedIn",
		},
		{
			name:     "incompatible active title",
			document: strings.Replace(active, "Machine Learning Research Intern (LLMs & Computer Vision)", "Finance Director", 1),
			wantErr:  "incompatible with first-party title",
		},
		{
			name:     "missing apply marker",
			document: strings.Replace(active, "top-card-layout__cta--primary", "other-cta", 1),
			wantErr:  "active LinkedIn Apply action",
		},
		{
			name:     "wrong apply modal",
			document: strings.Replace(active, "job-details-topcard-apply-modal", "other-modal", 1),
			wantErr:  "active LinkedIn Apply action",
		},
		{
			name:     "missing description",
			document: strings.Replace(active, "description__text", "other-description", 1),
			wantErr:  "description block",
		},
		{
			name: "short description",
			document: hyperVergeTestLinkedInDetail(
				"4442287503",
				"Machine Learning Research Intern",
				"HyperVerge",
				"Too short",
				false,
			),
			wantErr: "description length",
		},
		{
			name: "closed and active",
			document: strings.Replace(
				closed,
				`</section>`,
				`<button class="top-card-layout__cta--primary" data-modal="job-details-topcard-apply-modal">Apply</button></section>`,
				1,
			),
			wantErr: "mixes a closed-job marker",
		},
		{
			name:     "changed closed marker",
			document: strings.Replace(closed, hyperVergeClosedApplicationsText, "Applications unavailable", 1),
			wantErr:  "invalid explicit closed-job marker",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, actionable, err := parseHyperVergeLinkedInDetail(
				listJob,
				"4442287503",
				test.document,
			)
			if err == nil || actionable || got != (model.Job{}) ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf(
					"parse detail = (%+v, %t, %v), want error containing %q",
					got,
					actionable,
					err,
					test.wantErr,
				)
			}
		})
	}
}

func TestHyperVergeGoogleFormBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        func() string
		wantErr     string
	}{
		{
			name:        "sign in required",
			status:      http.StatusUnauthorized,
			contentType: "text/html; charset=utf-8",
			body: func() string {
				return hyperVergeTestGoogleSignInBoundary(testHyperVergeFormURL)
			},
		},
		{
			name:        "live unquoted sign in URL",
			status:      http.StatusUnauthorized,
			contentType: "text/html; charset=utf-8",
			body: func() string {
				document := hyperVergeTestGoogleSignInBoundary(testHyperVergeFormURL)
				document = strings.Replace(document, `data-popup-url="`, `data-popup-url=`, 1)
				return strings.Replace(document, `">Sign in`, `>Sign in`, 1)
			},
		},
		{
			name:        "public form needs review",
			status:      http.StatusOK,
			contentType: "text/html",
			body:        func() string { return `<form>Public form</form>` },
			wantErr:     "became anonymously accessible",
		},
		{
			name:        "unrecognized unauthorized page",
			status:      http.StatusUnauthorized,
			contentType: "text/html",
			body:        func() string { return `<p>Unauthorized</p>` },
			wantErr:     "omitted the Google Forms login boundary",
		},
		{
			name:        "wrong return form",
			status:      http.StatusUnauthorized,
			contentType: "text/html",
			body: func() string {
				return hyperVergeTestGoogleSignInBoundary("https://docs.google.com/forms/d/e/other/viewform")
			},
			wantErr: "does not return to the expected form",
		},
		{
			name:        "transient status",
			status:      http.StatusBadGateway,
			contentType: "text/html",
			body:        func() string { return "upstream down" },
			wantErr:     "502 Bad Gateway",
		},
		{
			name:        "wrong content type",
			status:      http.StatusUnauthorized,
			contentType: "application/json",
			body:        func() string { return `{}` },
			wantErr:     "unexpected Content-Type",
		},
		{
			name:        "body limit",
			status:      http.StatusUnauthorized,
			contentType: "text/html",
			body: func() string {
				return strings.Repeat("x", hyperVergeGoogleFormBodyLimit+1)
			},
			wantErr: "response exceeds",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body())
			}))
			defer server.Close()
			src := &hyperVerge{
				googleFormsBase: server.URL + "/forms",
				client:          server.Client(),
			}
			err := src.validateGoogleFormBoundary(
				context.Background(),
				testHyperVergeFormID,
				testHyperVergeFormURL,
			)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestHyperVergeLinkedInHTTPBoundary(t *testing.T) {
	t.Parallel()

	job := model.Job{
		ID:       "hyperverge/linkedin/4442287503",
		Company:  "HyperVerge",
		Title:    "Machine Learning Intern",
		Location: "Bengaluru",
		URL:      "https://www.linkedin.com/jobs/view/4442287503/",
	}
	validBody := hyperVergeTestLinkedInDetail(
		"4442287503",
		"Machine Learning Intern",
		"HyperVerge",
		strings.Repeat("Build and validate machine-learning research systems for production use. ", 3),
		false,
	)
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name:        "transient status",
			status:      http.StatusTooManyRequests,
			contentType: "text/html",
			body:        "rate limited",
			wantErr:     "429 Too Many Requests",
		},
		{
			name:        "wrong content type",
			status:      http.StatusOK,
			contentType: "application/json",
			body:        `{}`,
			wantErr:     "unexpected Content-Type",
		},
		{
			name:        "body limit",
			status:      http.StatusOK,
			contentType: "text/html",
			body:        strings.Repeat("x", hyperVergeLinkedInBodyLimit+1),
			wantErr:     "response exceeds",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Accept-Language"); got != "en-US,en;q=0.9" {
					t.Errorf("Accept-Language = %q", got)
				}
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			src := &hyperVerge{
				linkedinBase: server.URL + "/linkedin",
				client:       server.Client(),
			}
			got, actionable, err := src.fetchLinkedInDetail(context.Background(), job)
			if err == nil || actionable || got != (model.Job{}) ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf(
					"fetch detail = (%+v, %t, %v), want error containing %q",
					got,
					actionable,
					err,
					test.wantErr,
				)
			}
		})
	}

	var redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/linkedin/4442287503" {
			http.Redirect(w, r, "/other", http.StatusFound)
			return
		}
		redirected.Store(true)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, validBody)
	}))
	defer server.Close()
	src := &hyperVerge{
		linkedinBase: server.URL + "/linkedin",
		client:       server.Client(),
	}
	got, actionable, err := src.fetchLinkedInDetail(context.Background(), job)
	if err == nil || actionable || got != (model.Job{}) ||
		!strings.Contains(err.Error(), "302 Found") {
		t.Fatalf("redirect fetch = (%+v, %t, %v), want strict redirect error", got, actionable, err)
	}
	if redirected.Load() {
		t.Fatal("redirect destination was requested")
	}
}

func TestHyperVergeGoogleFormOnlyBoardFailsExplicitly(t *testing.T) {
	t.Parallel()

	document := hyperVergeTestDocument(
		[]string{"engineering"},
		hyperVergeTestPane(
			"engineering",
			hyperVergeTestJob(
				"Site Reliability Engineer",
				"Bengaluru",
				"Full-time",
				testHyperVergeFormURL,
			),
		),
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/careers/":
			fmt.Fprint(w, document)
		case "/forms/" + testHyperVergeFormID + "/viewform":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, hyperVergeTestGoogleSignInBoundary(testHyperVergeFormURL))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	src := &hyperVerge{
		company:         "HyperVerge",
		base:            server.URL,
		googleFormsBase: server.URL + "/forms",
		client:          server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "requires sign-in") {
		t.Fatalf("Fetch = %#v, %v; want explicit incomplete-form error", jobs, err)
	}
}

func TestHyperVergeGoogleFormBoundaryRejectsRedirect(t *testing.T) {
	t.Parallel()

	var redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/forms/") {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		redirected.Store(true)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, hyperVergeTestGoogleSignInBoundary(testHyperVergeFormURL))
	}))
	defer server.Close()
	src := &hyperVerge{
		googleFormsBase: server.URL + "/forms",
		client:          server.Client(),
	}
	err := src.validateGoogleFormBoundary(
		context.Background(),
		testHyperVergeFormID,
		testHyperVergeFormURL,
	)
	if err == nil || !strings.Contains(err.Error(), "302 Found") {
		t.Fatalf("error = %v, want strict redirect error", err)
	}
	if redirected.Load() {
		t.Fatal("redirect destination was requested")
	}
}

func TestHyperVergeFetchIsAtomicOnLinkedInFailure(t *testing.T) {
	t.Parallel()

	document := hyperVergeTestDocument(
		[]string{"engineering"},
		hyperVergeTestPane(
			"engineering",
			hyperVergeTestJob(
				"Machine Learning Intern",
				"Bengaluru",
				"Intern",
				"https://www.linkedin.com/jobs/view/4442287503/",
			)+
				hyperVergeTestJob(
					"Business Development Manager",
					"Bengaluru",
					"Full-time",
					"https://www.linkedin.com/jobs/view/4432195329/",
				),
		),
	)
	description := strings.Repeat("Own a complete production responsibility for this role. ", 3)
	var validDetailFetched atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/careers/":
			fmt.Fprint(w, document)
		case "/linkedin/4442287503":
			validDetailFetched.Store(true)
			fmt.Fprint(
				w,
				hyperVergeTestLinkedInDetail(
					"4442287503",
					"Machine Learning Intern",
					"HyperVerge",
					description,
					false,
				),
			)
		case "/linkedin/4432195329":
			http.Error(w, "temporary failure", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	src := &hyperVerge{
		company:      "HyperVerge",
		base:         server.URL,
		linkedinBase: server.URL + "/linkedin",
		client:       server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and transient detail error", jobs, err)
	}
	if !validDetailFetched.Load() {
		t.Fatal("concurrent valid detail boundary was not exercised")
	}
}

func TestHyperVergeFetchesLinkedInDetailsConcurrentlyAndPreservesOrder(t *testing.T) {
	t.Parallel()

	document := hyperVergeTestDocument(
		[]string{"engineering"},
		hyperVergeTestPane(
			"engineering",
			hyperVergeTestJob(
				"Engineering Manager",
				"Bengaluru",
				"Full-time",
				"https://www.linkedin.com/jobs/view/4412659818/",
			)+
				hyperVergeTestJob(
					"Content Strategist",
					"Bengaluru",
					"Full-time",
					"https://www.linkedin.com/jobs/view/4415077121/",
				),
		),
	)
	description := strings.Repeat("Own strategy, execution, measurement, and collaboration for this opening. ", 3)
	var (
		activeRequests atomic.Int32
		maxRequests    atomic.Int32
		arrived        atomic.Int32
		arrivedBoth    = make(chan struct{})
		arrivedOnce    sync.Once
		release        = make(chan struct{})
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/careers/" {
			fmt.Fprint(w, document)
			return
		}
		current := activeRequests.Add(1)
		for {
			prior := maxRequests.Load()
			if current <= prior || maxRequests.CompareAndSwap(prior, current) {
				break
			}
		}
		if arrived.Add(1) == 2 {
			arrivedOnce.Do(func() { close(arrivedBoth) })
		}
		<-release
		activeRequests.Add(-1)
		switch r.URL.Path {
		case "/linkedin/4412659818":
			fmt.Fprint(
				w,
				hyperVergeTestLinkedInDetail(
					"4412659818",
					"Engineering Manager",
					"HyperVerge",
					description,
					false,
				),
			)
		case "/linkedin/4415077121":
			fmt.Fprint(
				w,
				hyperVergeTestLinkedInDetail(
					"4415077121",
					"Content Strategist",
					"HyperVerge",
					description,
					false,
				),
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	src := &hyperVerge{
		company:      "HyperVerge",
		base:         server.URL,
		linkedinBase: server.URL + "/linkedin",
		client:       server.Client(),
	}
	type fetchResult struct {
		jobs []model.Job
		err  error
	}
	done := make(chan fetchResult, 1)
	go func() {
		jobs, err := src.Fetch(context.Background())
		done <- fetchResult{jobs: jobs, err: err}
	}()
	select {
	case <-arrivedBoth:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("two LinkedIn detail requests did not overlap")
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if maxRequests.Load() < 2 {
		t.Fatalf("maximum concurrent detail requests = %d, want at least 2", maxRequests.Load())
	}
	if len(result.jobs) != 2 ||
		result.jobs[0].ID != "hyperverge/linkedin/4412659818" ||
		result.jobs[1].ID != "hyperverge/linkedin/4415077121" {
		t.Fatalf("jobs = %+v, want deterministic first-party order", result.jobs)
	}
}

func TestHyperVergeRejectsBadHTTPResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name:        "status",
			status:      http.StatusBadGateway,
			contentType: "text/html",
			body:        "upstream down",
			wantErr:     "502 Bad Gateway: upstream down",
		},
		{
			name:        "content type",
			status:      http.StatusOK,
			contentType: "application/json",
			body:        `{}`,
			wantErr:     `unexpected Content-Type "application/json"`,
		},
		{
			name:        "body limit",
			status:      http.StatusOK,
			contentType: "text/html",
			body:        strings.Repeat("x", hyperVergeBodyLimit+1),
			wantErr:     "response exceeds",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			src := &hyperVerge{
				company: "HyperVerge",
				base:    server.URL,
				client:  server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf(
					"Fetch = (%#v, %v), want nil jobs and error containing %q",
					jobs,
					err,
					test.wantErr,
				)
			}
		})
	}
}

func TestCanonicalHyperVergeApplyURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantURL string
		wantID  string
		wantErr string
	}{
		{
			name:    "LinkedIn tracking stripped",
			raw:     "https://www.linkedin.com/jobs/view/4412659818/?trk=careers",
			wantURL: "https://www.linkedin.com/jobs/view/4412659818/",
			wantID:  "hyperverge/linkedin/4412659818",
		},
		{
			name:    "duplicated Google form repaired",
			raw:     testHyperVergeFormURL + testHyperVergeFormURL,
			wantURL: testHyperVergeFormURL,
			wantID: "hyperverge/google-form/" + testHyperVergeFormID +
				"/fd6294091b9253773a278b739f3eb4ddc3d6ee5526dd894933902d37dbf03cee",
		},
		{
			name:    "wrong scheme",
			raw:     "http://www.linkedin.com/jobs/view/4412659818/",
			wantErr: "invalid apply URL",
		},
		{
			name:    "userinfo",
			raw:     "https://user@www.linkedin.com/jobs/view/4412659818/",
			wantErr: "invalid apply URL",
		},
		{
			name:    "lookalike host",
			raw:     "https://www.linkedin.com.example/jobs/view/4412659818/",
			wantErr: "unsupported apply URL host",
		},
		{
			name:    "encoded path",
			raw:     "https://www.linkedin.com/jobs/view/%34%34%31%32%36%35%39%38%31%38/",
			wantErr: "invalid apply URL",
		},
		{
			name:    "conflicting concatenated forms",
			raw:     testHyperVergeFormURL + testHyperVergeFormURL + "x",
			wantErr: "conflicting concatenated URLs",
		},
		{
			name:    "unsupported host",
			raw:     "https://jobs.example.com/opening/123",
			wantErr: "unsupported apply URL host",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotURL, gotID, err := canonicalHyperVergeApplyURL(
				test.raw,
				"Site Reliability Engineer",
				"Bengaluru",
			)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotURL != test.wantURL || gotID != test.wantID {
				t.Fatalf("canonical = (%q, %q), want (%q, %q)", gotURL, gotID, test.wantURL, test.wantID)
			}
		})
	}
}

func hyperVergeTestDocument(options []string, panes string) string {
	var selector strings.Builder
	for _, option := range options {
		fmt.Fprintf(
			&selector,
			`<option value="%s">%s</option>`,
			option,
			strings.ToUpper(option[:1])+option[1:],
		)
	}
	return `<html><body><section class="section section-openings" id="jobOpenings">` +
		`<select id="selectBox">` + selector.String() + `</select>` +
		`<div class="tab-content">` + panes + `</div>` +
		`</section></body></html>`
}

func hyperVergeTestPane(id, body string) string {
	return `<div role="tabpanel" class="tab-pane" id="` + id + `">` + body + `</div>`
}

func hyperVergeTestJob(title, location, employmentType, applyURL string) string {
	return `<div class="job-col">` +
		`<p class="job-title">` + title + `</p>` +
		`<div class="job-meta d-flex"><div class="d-flex">` +
		`<p><img alt="location"><span>` + location + `</span></p>` +
		`<p><img alt="type"><span>` + employmentType + `</span></p>` +
		`</div><a class="btn" href="` + applyURL + `">` +
		`Apply Now <i class="fa-solid fa-arrow-right"></i></a></div>` +
		`</div>`
}

func hyperVergeTestLinkedInDetail(
	id, title, organization, description string,
	closed bool,
) string {
	state := `<button class="sign-up-modal__outlet top-card-layout__cta--primary" ` +
		`data-modal="job-details-topcard-apply-modal">Apply</button>`
	if closed {
		state = `<figure class="closed-job"><figcaption class="closed-job__flavor--closed">` +
			hyperVergeClosedApplicationsText + `</figcaption></figure>`
	}
	return `<section class="top-card-layout">` +
		`<a class="topcard__link" href="https://in.linkedin.com/jobs/view/test-role-` + id + `?trk=public">` +
		`<h2 class="topcard__title">` + title + `</h2></a>` +
		`<a class="topcard__org-name-link" href="https://www.linkedin.com/company/hyperverge?trk=public">` +
		organization + `</a>` +
		state +
		`</section>` +
		`<div class="description__text description__text--rich">` +
		`<div class="show-more-less-html__markup"><p>` + description + `</p></div>` +
		`</div>`
}

func hyperVergeTestGoogleSignInBoundary(formURL string) string {
	loginURL := "https://accounts.google.com/ServiceLogin?continue=" +
		url.QueryEscape(formURL) + "&btmpl=popup"
	return `<div class="document-root loading">` +
		`<div class="login"><div class="title-text">Sign in to your Google Account</div>` +
		`<div class="subtitle-text">You must sign in to access this content</div>` +
		`<button class="sign-in-button" data-popup-url="` + loginURL + `">Sign in</button>` +
		`</div></div>`
}
