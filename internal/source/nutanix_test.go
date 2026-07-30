package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestNutanixRegistrationAndParams(t *testing.T) {
	t.Parallel()

	src, err := New("nutanix", "Nutanix", params.Map{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := src.(*identifiedSource)
	if !ok {
		t.Fatalf("source type = %T, want *identifiedSource", src)
	}
	implementation, ok := wrapped.Source.(*nutanix)
	if !ok {
		t.Fatalf("wrapped source type = %T, want *nutanix", wrapped.Source)
	}
	if implementation.base != "https://careers.nutanix.com" {
		t.Errorf("base = %q", implementation.base)
	}
	if implementation.client != http.DefaultClient {
		t.Error("nil client did not select http.DefaultClient")
	}
	if Identity(src) != "nutanix" || StatePrefix(src) != "nutanix/" {
		t.Errorf("identity/prefix = %q/%q", Identity(src), StatePrefix(src))
	}

	_, err = New("nutanix", "Nutanix", params.Map{"z": "1", "a": "2"}, nil)
	if err == nil || !strings.Contains(err.Error(), "got a, z") {
		t.Fatalf("unknown params error = %v, want sorted names", err)
	}
}

func TestNutanixFetchesCompleteXMLFeed(t *testing.T) {
	t.Parallel()

	var requests int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/en/jobs/xml/" ||
			r.URL.Query().Get("rss") != "true" || len(r.URL.Query()) != 1 {
			t.Errorf("request = %s %s, want exact XML feed", r.Method, r.URL.String())
		}
		if r.Header.Get("Accept") != "application/xml,text/xml" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "jobwatch") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		fmt.Fprint(w, nutanixTestFeed(
			nutanixTestJob(
				"Role &amp; One", "2026-07-30T06:20:08.3470000Z",
				"opaque_one", "1001",
				server.URL+"/en/jobs/1001/role-one/",
				"Bengaluru", "", "India",
				"<p>Build &amp; ship.</p><ul><li>Own APIs</li></ul>",
				"Full-Time", "Engineering",
			),
			nutanixTestJob(
				"Role Two", "2026-07-29T04:46:21.3700000Z",
				"opaque_two", "n2387",
				server.URL+"/en/jobs/n2387/%E6%97%A5%E6%9C%AC/",
				"Remote", "Remote", "United States",
				"<p>Lead systems.</p>",
				"New Grad", "Engineering",
			),
		))
	}))
	defer server.Close()

	src := &nutanix{company: "Nutanix Careers", base: server.URL, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Job{
		{
			ID:             "nutanix/opaque_one",
			Company:        "Nutanix Careers",
			Title:          "Role & One",
			Location:       "Bengaluru, India",
			URL:            server.URL + "/en/jobs/1001/role-one/",
			EmploymentType: "Full-Time",
			Description:    "Build & ship.\nOwn APIs",
			PostedAt:       time.Date(2026, 7, 30, 6, 20, 8, 347000000, time.UTC),
		},
		{
			ID:             "nutanix/opaque_two",
			Company:        "Nutanix Careers",
			Title:          "Role Two",
			Location:       "Remote, United States",
			URL:            server.URL + "/en/jobs/n2387/%E6%97%A5%E6%9C%AC/",
			EmploymentType: "New Grad",
			Description:    "Lead systems.",
			PostedAt:       time.Date(2026, 7, 29, 4, 46, 21, 370000000, time.UTC),
		},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs =\n%+v\nwant:\n%+v", jobs, want)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestNutanixRejectsMalformedOrIncompleteJobsAtomically(t *testing.T) {
	t.Parallel()

	valid := nutanixTestJob(
		"Role One", "2026-07-30T06:20:08Z", "opaque_one", "1001",
		"https://careers.nutanix.com/en/jobs/1001/role-one/",
		"Bengaluru", "", "India", "<p>Build.</p>", "Full-Time", "Engineering",
	)
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"bad XML", func(string) string { return `<source><job>` }, "decoding XML"},
		{"second XML document", func(body string) string {
			return body + `<source><publisher>Nutanix</publisher></source>`
		}, "second document"},
		{"publisher", func(body string) string {
			return strings.Replace(body, "<publisher>Nutanix</publisher>", "<publisher>Other</publisher>", 1)
		}, "publisher is"},
		{"publisher URL", func(body string) string {
			return strings.Replace(body, "https://www.nutanix.com/", "https://example.com/", 1)
		}, "publisher URL"},
		{"build date", func(body string) string { return strings.Replace(body, "2026-07-30T08:32:43Z", "yesterday", 1) }, "invalid lastBuildDate"},
		{"requisition", func(body string) string {
			return strings.Replace(body, "<requisitionid>opaque_one</requisitionid>", "<requisitionid>bad/id</requisitionid>", 1)
		}, "invalid requisition"},
		{"reference mismatch", func(body string) string {
			return strings.Replace(body, "<referencenumber>opaque_one</referencenumber>", "<referencenumber>different</referencenumber>", 1)
		}, "does not match"},
		{"API ID", func(body string) string {
			return strings.Replace(body, "<apijobid>1001</apijobid>", "<apijobid>bad/id</apijobid>", 1)
		}, "invalid API job ID"},
		{"title", func(body string) string {
			return strings.Replace(body, "<title>Role One</title>", "<title></title>", 1)
		}, "invalid title"},
		{"description", func(body string) string { return strings.Replace(body, "&lt;p&gt;Build.&lt;/p&gt;", "", 1) }, "empty description"},
		{"company", func(body string) string {
			return strings.Replace(body, "<company>Nutanix</company>", "<company>Other</company>", 1)
		}, "company is"},
		{"external URL", func(body string) string {
			return strings.Replace(body, "https://careers.nutanix.com/en/jobs/1001/role-one/", "https://evil.example/en/jobs/1001/role-one/", 1)
		}, "does not match"},
		{"URL ID mismatch", func(body string) string {
			return strings.Replace(body, "/en/jobs/1001/role-one/", "/en/jobs/9999/role-one/", 1)
		}, "does not contain API job ID"},
		{"URL query", func(body string) string {
			return strings.Replace(body, "/en/jobs/1001/role-one/", "/en/jobs/1001/role-one/?x=1", 1)
		}, "invalid URL"},
		{"posting date", func(body string) string { return strings.Replace(body, "2026-07-30T06:20:08Z", "today", 1) }, "invalid posting date"},
		{"activity date", func(body string) string { return strings.Replace(body, "2026-07-30T08:27:03Z", "today", 1) }, "invalid last activity"},
		{"location", func(body string) string {
			body = strings.Replace(body, "<city>Bengaluru</city>", "<city></city>", 1)
			return strings.Replace(body, "<country>India</country>", "<country></country>", 1)
		}, "has no location"},
		{"job type", func(body string) string {
			return strings.Replace(body, "<jobtype>Full-Time</jobtype>", "<jobtype></jobtype>", 1)
		}, "invalid job type"},
		{"category", func(body string) string {
			return strings.Replace(body, "<category>Engineering</category>", "<category></category>", 1)
		}, "invalid category"},
		{"duplicate", func(body string) string {
			jobStart := strings.Index(body, "<job>")
			jobEnd := strings.Index(body, "</job>") + len("</job>")
			return strings.Replace(body, "</source>", body[jobStart:jobEnd]+"</source>", 1)
		}, "duplicate stable job ID"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			src := &nutanix{company: "Nutanix", base: "https://careers.nutanix.com"}
			jobs, err := src.parseFeed([]byte(test.mutate(nutanixTestFeed(valid))))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
			if jobs != nil {
				t.Fatalf("jobs = %#v, want nil on any malformed record", jobs)
			}
		})
	}
}

func TestNutanixHTTPValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{"status", http.StatusBadGateway, "text/xml", "upstream", "502 Bad Gateway"},
		{"content type", http.StatusOK, "text/html", "<html></html>", "unexpected Content-Type"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			src := &nutanix{company: "Nutanix", base: server.URL, client: server.Client()}
			jobs, err := src.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
			if jobs != nil {
				t.Fatalf("jobs = %#v, want nil", jobs)
			}
		})
	}
}

func nutanixTestFeed(jobs ...string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<source><publisher>Nutanix</publisher>` +
		`<publisherurl>https://www.nutanix.com/</publisherurl>` +
		`<lastBuildDate>2026-07-30T08:32:43Z</lastBuildDate>` +
		strings.Join(jobs, "") + `</source>`
}

func nutanixTestJob(
	title, date, requisitionID, apiJobID, jobURL,
	city, state, country, description, jobType, category string,
) string {
	return `<job>` +
		`<title>` + title + `</title>` +
		`<date>` + date + `</date>` +
		`<requisitionid>` + requisitionID + `</requisitionid>` +
		`<referencenumber>` + requisitionID + `</referencenumber>` +
		`<apijobid>` + apiJobID + `</apijobid>` +
		`<url>` + jobURL + `</url>` +
		`<company>Nutanix</company>` +
		`<city>` + city + `</city><state>` + state + `</state>` +
		`<country>` + country + `</country><postalcode></postalcode>` +
		`<description>` + strings.ReplaceAll(strings.ReplaceAll(description, "<", "&lt;"), ">", "&gt;") + `</description>` +
		`<jobtype>` + jobType + `</jobtype><category>` + category + `</category>` +
		`<lastactivitydate>2026-07-30T08:27:03Z</lastactivitydate>` +
		`</job>`
}
