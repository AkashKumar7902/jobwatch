package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestJioFetchesEveryStatefulPageAndHydratesDetail(t *testing.T) {
	const (
		categoryCode  = "0206"
		categoryLabel = "Engineering & Technology"
	)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			http.SetCookie(w, &http.Cookie{Name: "jio-session", Value: "ok", Path: "/"})
			fmt.Fprint(w, "<html>Jio Careers</html>")
		case "/frmJobCategories.aspx":
			if cookie, err := r.Cookie("jio-session"); err != nil || cookie.Value != "ok" {
				t.Errorf("category directory did not receive anonymous session")
			}
			fmt.Fprint(w, jioCategoryFixture(categoryCode, categoryLabel, 11))
		case "/frmfuncwisejob.aspx":
			if cookie, err := r.Cookie("jio-session"); err != nil || cookie.Value != "ok" {
				t.Errorf("function page did not receive anonymous session")
			}
			if r.Method == http.MethodGet {
				fmt.Fprint(w, jioPageFixture(categoryLabel, 1, 2, jioTestJobs(0, 10)))
				return
			}
			if r.Method != http.MethodPost {
				t.Errorf("function method = %s", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("__EVENTTARGET") != "ctl00$MainContent$lstJoblist$DataPager1$ctl00$PageDropDownList" ||
				r.Form.Get("ctl00$MainContent$lstJoblist$DataPager1$ctl00$PageDropDownList") != "2" ||
				r.Form.Get("__VIEWSTATE") != "signed-viewstate-1" ||
				r.Form.Get("ctl00$hdnToken") != "token-1" {
				t.Errorf("unexpected page form: %v", r.Form)
			}
			fmt.Fprint(w, jioPageFixture(categoryLabel, 2, 2, jioTestJobs(10, 1)))
		case "/frmjobdescription.aspx":
			fmt.Fprint(w, jioDetailFixture(t, "Software Engineer ( 86610001 )", "Bengaluru", "30 Jul 2026"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src, err := New(
		"jio", "Reliance Jio",
		params.Map{"functions": categoryCode, "max_postings": "20"},
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if Identity(src) != "jio/0206" || StatePrefix(src) != "jio/" {
		t.Fatalf("identity=%q prefix=%q", Identity(src), StatePrefix(src))
	}
	src.(*identifiedSource).Source.(*jio).base = server.URL

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 11 {
		t.Fatalf("jobs = %d, want 11", len(jobs))
	}
	for index, job := range jobs {
		wantExternalID := strconv.Itoa(86610001 + index)
		if job.ID != "jio/"+wantExternalID || job.Company != "Reliance Jio" ||
			job.Location != "Bengaluru" || !job.PostedAt.Equal(time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("job %d = %+v", index, job)
		}
	}
	if err := src.(Detailer).Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jobs[0].Description, "Experience Requirement:\n0 - 3 years") ||
		!strings.Contains(jobs[0].Description, "Build & ship.\nOwn tests.") {
		t.Fatalf("description = %q", jobs[0].Description)
	}
	if calls.Load() != 5 {
		t.Fatalf("requests = %d, want home + directory + two pages + detail", calls.Load())
	}
}

func TestJioRejectsInvalidParamsAndCanonicalizesScope(t *testing.T) {
	source, err := New(
		"jio", "Jio",
		params.Map{"functions": "0216, 0206,0210"},
		&http.Client{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if Identity(source) != "jio/0206+0210+0216" {
		t.Fatalf("identity = %q", Identity(source))
	}
	for _, test := range []struct {
		name    string
		params  params.Map
		wantErr string
	}{
		{"blank", params.Map{"functions": " "}, "function code"},
		{"bad code", params.Map{"functions": "engineering"}, "four-digit"},
		{"duplicate", params.Map{"functions": "0206,0206"}, "duplicate"},
		{"limit zero", params.Map{"functions": "0206", "max_postings": "0"}, "1 to 20000"},
		{"unknown", params.Map{"functions": "0206", "host": "evil.example"}, "unsupported param"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := New("jio", "Jio", test.params, nil)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestJioFailsClosedOnDirectoryAndPageDrift(t *testing.T) {
	source := &jio{base: "https://careers.jio.com"}
	valid := jioCategoryFixture("0206", "Engineering & Technology", 1)
	for _, test := range []struct {
		name    string
		page    string
		wantErr string
	}{
		{"missing records", "<html></html>", "omitted category records"},
		{"cross host", strings.Replace(valid, `/frmfuncwisejob.aspx?`, `https://evil.example/frmfuncwisejob.aspx?`, 1), "trusted Jio"},
		{"missing query", strings.Replace(valid, `&amp;flag=encrypted`, "", 1), "query parameter"},
		{"description drift", strings.Replace(valid, `lblfunctional_0">Engineering & Technology</span>`, `lblfunctional_0">Different</span>`, 1), "disagrees"},
		{"count drift", strings.Replace(valid, ">1</span>", ">many</span>", 1), "invalid job count"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := source.parseCategories(test.page)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}

	category := jioCategory{code: "0206", label: "Engineering & Technology", count: 11}
	for _, test := range []struct {
		name    string
		page    string
		wantErr string
	}{
		{"short first page", jioPageFixture(category.label, 1, 2, jioTestJobs(0, 9)), "incomplete job fields"},
		{"wrong page total", jioPageFixture(category.label, 1, 3, jioTestJobs(0, 10)), "page metadata"},
		{"wrong current page", jioPageFixture(category.label, 2, 2, jioTestJobs(0, 10)), "page metadata"},
		{"missing signed state", strings.Replace(jioPageFixture(category.label, 1, 2, jioTestJobs(0, 10)), `name="__VIEWSTATE"`, `name="removed"`, 1), "missing signed form field"},
		{"unsafe detail URL", strings.Replace(jioPageFixture(category.label, 1, 2, jioTestJobs(0, 10)), `/frmjobdescription.aspx?`, `https://evil.example/frmjobdescription.aspx?`, 1), "trusted Jio"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := source.parsePage(test.page, category, 1)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestJioDetailRejectsUntrustedOrDriftedJobsAtomically(t *testing.T) {
	valid := model.Job{
		ID:       "jio/86610001",
		Company:  "Jio",
		Title:    "Software Engineer ( 86610001 )",
		Location: "Bengaluru",
		URL: "https://careers.jio.com/frmjobdescription.aspx?" +
			"JBTITLE=title&jbID=id&funcCode=function",
		PostedAt: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
	}
	var requests atomic.Int32
	source := &jio{
		company: "Jio", base: "https://careers.jio.com",
		client: &http.Client{Transport: freshteamRoundTripper(func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			response := freshteamResponse(
				req, http.StatusOK,
				jioDetailFixture(t, "Different ( 86610001 )", "Bengaluru", "30 Jul 2026"),
			)
			response.Header.Set("Content-Type", "text/html")
			return response, nil
		})},
	}
	for _, test := range []struct {
		name   string
		mutate func(*model.Job)
		noCall bool
	}{
		{"wrong prefix", func(job *model.Job) { job.ID = "other/86610001" }, true},
		{"bad id", func(job *model.Job) { job.ID = "jio/../1" }, true},
		{"cross host", func(job *model.Job) {
			job.URL = "https://evil.example/frmjobdescription.aspx?JBTITLE=a&jbID=b&funcCode=c"
		}, true},
		{"bare query", func(job *model.Job) { job.URL = "https://careers.jio.com/frmjobdescription.aspx?" }, true},
		{"title drift", func(_ *model.Job) {}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := valid
			test.mutate(&before)
			job := before
			prior := requests.Load()
			err := source.Detail(context.Background(), &job)
			if err == nil {
				t.Fatal("expected detail error")
			}
			if job != before {
				t.Fatalf("job mutated on failure: got %+v want %+v", job, before)
			}
			if test.noCall && requests.Load() != prior {
				t.Fatal("invalid job caused an HTTP request")
			}
		})
	}
}

func TestJioDetailComparesStructuredDescriptionSemantically(t *testing.T) {
	const (
		title    = "Software Engineer ( 86610001 )"
		location = "Bengaluru"
		date     = "30 Jul 2026"
		visible  = "Build &amp; ship.<br/>Own tests."
	)
	for _, test := range []struct {
		name                  string
		structuredDescription string
		wantErr               bool
	}{
		{
			name:                  "equivalent HTML and punctuation spacing",
			structuredDescription: "<p>Build &amp; ship .</p><p>Own tests .</p>",
		},
		{
			name:                  "empty structured description remains invalid",
			structuredDescription: "<p> </p>",
			wantErr:               true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, jioDetailFixtureWithDescriptions(
					t, title, location, date, visible, test.structuredDescription,
				))
			}))
			defer server.Close()
			job := model.Job{
				ID: "jio/86610001", Company: "Jio", Title: title, Location: location,
				URL:      server.URL + "/frmjobdescription.aspx?JBTITLE=title&jbID=id&funcCode=function",
				PostedAt: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
			}
			before := job
			source := &jio{company: "Jio", base: server.URL, client: server.Client()}
			err := source.Detail(context.Background(), &job)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "JobPosting data disagrees") {
					t.Fatalf("Detail error = %v, want structured-data disagreement", err)
				}
				if job != before {
					t.Fatalf("job mutated on failure: got %+v want %+v", job, before)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(job.Description, "Build & ship.\nOwn tests.") {
				t.Fatalf("description = %q", job.Description)
			}
		})
	}
}

func jioCategoryFixture(code, label string, count int) string {
	return fmt.Sprintf(`<!doctype html><html><body>
<div><ul><li class="list-cont">
<input type="hidden" id="MainContent_rptJoblist_hdfunctioncode_0" value="%s">
<input type="hidden" id="MainContent_rptJoblist_hdDescription_0" value="%s">
<a id="MainContent_rptJoblist_HRFLink_0" href="/frmfuncwisejob.aspx?func=encrypted&amp;desc=encrypted&amp;flag=encrypted">
<span id="MainContent_rptJoblist_lblfunctional_0">%s</span>
<span id="MainContent_rptJoblist_lblfunctionjobCount_0">%d</span>
</a></li></ul></div></body></html>`, code, label, label, count)
}

func jioTestJobs(start, count int) []model.Job {
	jobs := make([]model.Job, 0, count)
	for index := 0; index < count; index++ {
		externalID := strconv.Itoa(86610001 + start + index)
		jobs = append(jobs, model.Job{
			ID:       externalID,
			Title:    "Software Engineer ( " + externalID + " )",
			Location: "Bengaluru",
			URL: "/frmjobdescription.aspx?" +
				"JBTITLE=title" + externalID + "&jbID=id" + externalID + "&funcCode=function",
		})
	}
	return jobs
}

func jioPageFixture(label string, page, totalPages int, jobs []model.Job) string {
	var cards strings.Builder
	for index, job := range jobs {
		fmt.Fprintf(&cards, `<a id="aJobDesc"><h2>
<a id="MainContent_lstJoblist_hylUser_%d" href="%s">%s</a>
</h2></a><p>
<span id="MainContent_lstJoblist_Label2_%d">%s</span>
<span id="MainContent_lstJoblist_lblfunctional_%d">%s</span>
<span id="MainContent_lstJoblist_Label1_%d">30 Jul 2026</span>
</p>`, index, job.URL, job.Title, index, job.Location, index, label, index)
	}
	var options strings.Builder
	for number := 1; number <= totalPages; number++ {
		selected := ""
		if number == page {
			selected = ` selected="selected"`
		}
		fmt.Fprintf(&options, `<option%s value="%d">%02d</option>`, selected, number, number)
	}
	return fmt.Sprintf(`<!doctype html><html><body><form>
<input type="hidden" name="ToolkitScriptManager1_HiddenField" value="">
<input type="hidden" name="__EVENTTARGET" value="">
<input type="hidden" name="__EVENTARGUMENT" value="">
<input type="hidden" name="__LASTFOCUS" value="">
<input type="hidden" name="__VIEWSTATE" value="signed-viewstate-%d">
<input type="hidden" name="__VIEWSTATEGENERATOR" value="generator">
<input type="hidden" name="__EVENTVALIDATION" value="signed-validation-%d">
<input type="hidden" name="ctl00$hdnToken" value="token-%d">
<input type="hidden" name="ctl00$hdnmaibaap" value="parent-%d">
<select id="MainContent_ddlentries"><option value="5">05</option><option selected="selected" value="10">10</option></select>
<article>%s
<span id="MainContent_lstJoblist_DataPager1">
<span id="MainContent_lstJoblist_DataPager1_ctl00_CurrentPageLabel">Showing %d of %d Pages</span>
<select id="MainContent_lstJoblist_DataPager1_ctl00_PageDropDownList">%s</select>
</span></article></form></body></html>`,
		page, page, page, page, cards.String(), page, totalPages, options.String())
}

func jioDetailFixture(t *testing.T, title, location, date string) string {
	const responsibilities = "Build &amp; ship.<br/>Own tests."
	return jioDetailFixtureWithDescriptions(t, title, location, date, responsibilities, responsibilities)
}

func jioDetailFixtureWithDescriptions(t *testing.T, title, location, date, visible, structuredDescription string) string {
	t.Helper()
	parsed, err := time.Parse("02 Jan 2006", date)
	if err != nil {
		t.Fatal(err)
	}
	posting, err := json.Marshal(map[string]any{
		"@context":    "https://schema.org",
		"@type":       "JobPosting",
		"title":       title,
		"description": structuredDescription,
		"datePosted":  parsed.Format("2006-01-02"),
		"jobLocation": map[string]any{"address": map[string]any{"addressLocality": location, "addressCountry": "IN"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`<!doctype html><html><head>
<script type="application/ld+json">%s</script></head><body>
<span id="MainContent_lblJobTitle">%s</span>
<span id="MainContent_lblPostedDate">%s</span>
<span id="MainContent_lblLoc">%s</span>
<span id="MainContent_lblSummRole">%s</span>
<span id="MainContent_lblEduReq">B.Tech</span>
<span id="MainContent_lblExpReq">0 - 3 years</span>
<span id="MainContent_lblSkill">Go &amp; testing</span>
</body></html>`, posting, title, date, location, visible)
}

func TestJioURLValidationPreservesEncryptedQuery(t *testing.T) {
	source := &jio{base: "https://careers.jio.com"}
	raw := `/frmjobdescription.aspx?JBTITLE=ab+c/==&amp;jbID=de+f/==&amp;funcCode=gh+i/==`
	resolved, err := source.validateURL(raw, "/frmjobdescription.aspx", []string{"JBTITLE", "funcCode", "jbID"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(resolved)
	if parsed.RawQuery == "" || !strings.Contains(resolved, "JBTITLE=") {
		t.Fatalf("resolved URL = %q", resolved)
	}
}

func TestJioStopsHTTPSDowngradeBeforeFollowingRedirect(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downgraded := "http://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, downgraded, http.StatusFound)
	}))
	defer server.Close()

	src := &jio{client: server.Client()}
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, server.URL+"/frmJobCategories.aspx?n=1", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.doHTML(req, map[string]*http.Cookie{
		"session": {Name: "session", Value: "secret", Secure: true},
	})
	if err == nil || !strings.Contains(err.Error(), "untrusted Jio page") {
		t.Fatalf("doHTML error = %v, want HTTPS downgrade rejection", err)
	}
}
