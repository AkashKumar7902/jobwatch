package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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

func TestHyperVergeFetchesCompleteDepartmentMarkup(t *testing.T) {
	formDuplicate := testHyperVergeFormURL + testHyperVergeFormURL
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
				hyperVergeTestJob(
					"Engineering Manager",
					"Bengaluru",
					"Full-time",
					"https://www.linkedin.com/jobs/view/4412659818/?trk=careers&amp;refId=one",
				)+
				hyperVergeTestJob(
					"Site Reliability Engineer",
					"Bengaluru",
					"Full-time",
					formDuplicate,
				),
		)+
			hyperVergeTestPane(
				"deeplearning",
				hyperVergeTestJob(
					"Site Reliability Engineer",
					"Bengaluru",
					"Full-time",
					testHyperVergeFormURL,
				),
			)+
			hyperVergeTestPane(
				"product",
				`<div class="job-openings-content no-openings text-center">`+
					`<p>Currently no openings under this role</p></div>`,
			),
	)

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/careers/" {
			t.Errorf("request = %s %s, want GET /careers/", r.Method, r.URL.Path)
		}
		if r.Header.Get("Accept") != "text/html,application/xhtml+xml" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "jobwatch") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		fmt.Fprint(w, document)
	}))
	defer server.Close()

	src := &hyperVerge{
		company: "HyperVerge",
		base:    server.URL,
		client:  server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Job{
		{
			ID:             "hyperverge/linkedin/4412659818",
			Company:        "HyperVerge",
			Title:          "Engineering Manager",
			Location:       "Bengaluru",
			URL:            "https://www.linkedin.com/jobs/view/4412659818/",
			EmploymentType: "Full-time",
		},
		{
			ID: "hyperverge/google-form/" + testHyperVergeFormID +
				"/fd6294091b9253773a278b739f3eb4ddc3d6ee5526dd894933902d37dbf03cee",
			Company:        "HyperVerge",
			Title:          "Site Reliability Engineer",
			Location:       "Bengaluru",
			URL:            testHyperVergeFormURL,
			EmploymentType: "Full-time",
		},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs =\n%+v\nwant:\n%+v", jobs, want)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
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
