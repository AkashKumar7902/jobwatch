package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	polymerTestSlug      = "acme"
	polymerTestCompany   = "Acme Careers"
	polymerTestCreated   = "2026-01-02T03:04:05Z"
	polymerTestPublished = "2026-01-03T04:05:06.123Z"
)

var (
	polymerTestCreatedTime, _   = time.Parse(time.RFC3339Nano, polymerTestCreated)
	polymerTestPublishedTime, _ = time.Parse(time.RFC3339Nano, polymerTestPublished)
)

func TestPolymerFactoryValidatesOrganizationSlug(t *testing.T) {
	src, err := New(
		"polymer", polymerTestCompany,
		params.Map{"organization_slug": "swym"},
		&http.Client{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if src.Company() != polymerTestCompany {
		t.Fatalf("Company() = %q", src.Company())
	}
	if _, ok := src.(Detailer); !ok {
		t.Fatal("wrapped Polymer source does not expose Detailer")
	}
	if Identity(src) != "polymer/swym" || StatePrefix(src) != "polymer/swym/" {
		t.Fatalf("identity/prefix = %q/%q", Identity(src), StatePrefix(src))
	}
	defaultClientSource, err := New(
		"polymer", polymerTestCompany,
		params.Map{"organization_slug": "swym"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if implementation := defaultClientSource.(*identifiedSource).Source.(*polymer); implementation.client != http.DefaultClient {
		t.Fatal("nil client did not select http.DefaultClient")
	}
	if _, err := New(
		"polymer", "Acme",
		params.Map{"organization_slug": "swym", "ignored": "value"},
		&http.Client{},
	); err == nil || !strings.Contains(err.Error(), "unexpected: ignored") {
		t.Fatalf("unexpected-param error = %v", err)
	}

	valid := []string{"a", "swym", "company-2", strings.Repeat("a", 100)}
	for _, slug := range valid {
		t.Run("valid_"+slug[:min(len(slug), 20)], func(t *testing.T) {
			if _, err := New(
				"polymer", "Acme",
				params.Map{"organization_slug": slug},
				&http.Client{},
			); err != nil {
				t.Fatalf("organization_slug %q: %v", slug, err)
			}
		})
	}

	invalid := []string{
		"", " Swym", "swym ", "Swym", "-swym", "swym-", "swym--jobs",
		"swym_jobs", "swym.jobs", "https://swym", "swym/jobs",
		strings.Repeat("a", 101),
	}
	for index, slug := range invalid {
		t.Run(fmt.Sprintf("invalid_%d", index), func(t *testing.T) {
			if _, err := New(
				"polymer", "Acme",
				params.Map{"organization_slug": slug},
				&http.Client{},
			); err == nil {
				t.Fatalf("organization_slug %q succeeded", slug)
			}
		})
	}
}

func TestPolymerFetchesEveryPageAndHydratesDetailLazily(t *testing.T) {
	first := polymerTestListPosting(101, "R&amp;D Engineer")
	first["display_location"] = ""
	first["remoteness_pretty"] = "Remote"
	second := polymerTestListPosting(102, "Platform Engineer")
	third := polymerTestListPosting(103, "Product Engineer")

	var listCalls, detailCalls atomic.Int32
	src, closeServer := polymerTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") == "" {
			t.Errorf("missing request headers: %v", r.Header)
		}
		switch {
		case r.URL.Path == "/v1/hire/organizations/acme/jobs" &&
			r.URL.Query().Get("page") == "1":
			if r.URL.RawQuery != "page=1" {
				t.Errorf("page 1 query = %q", r.URL.RawQuery)
			}
			listCalls.Add(1)
			polymerWriteJSON(t, w, polymerTestListPage(
				[]map[string]any{first, second}, 1, 2, 3, "Acme & Co",
			))
		case r.URL.Path == "/v1/hire/organizations/acme/jobs" &&
			r.URL.Query().Get("page") == "2":
			if r.URL.RawQuery != "page=2" {
				t.Errorf("page 2 query = %q", r.URL.RawQuery)
			}
			listCalls.Add(1)
			polymerWriteJSON(t, w, polymerTestListPage(
				[]map[string]any{third}, 2, 2, 3, "Acme & Co",
			))
		case r.URL.Path == "/v1/hire/organizations/acme/jobs/101":
			if r.URL.RawQuery != "" || r.URL.ForceQuery {
				t.Errorf("detail query = %q force=%v", r.URL.RawQuery, r.URL.ForceQuery)
			}
			detailCalls.Add(1)
			detail := polymerTestDetailPosting(101, "R&amp;D Engineer II")
			detail["description"] = "<p>Build &amp; ship.</p><ul><li>Own reliability.</li></ul>"
			detail["display_location"] = "Bengaluru, IN"
			detail["kind_pretty"] = "Permanent"
			polymerWriteJSON(t, w, detail)
		default:
			t.Errorf("unexpected request URL %q", r.URL)
			http.NotFound(w, r)
		}
	})
	defer closeServer()

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listCalls.Load() != 2 || detailCalls.Load() != 0 {
		t.Fatalf("calls after Fetch: list=%d detail=%d", listCalls.Load(), detailCalls.Load())
	}
	if len(jobs) != 3 {
		t.Fatalf("Fetch returned %d jobs, want 3", len(jobs))
	}
	if jobs[0].ID != "polymer/acme/101" ||
		jobs[0].Company != polymerTestCompany ||
		jobs[0].Title != "R&D Engineer" ||
		jobs[0].Location != "Remote" ||
		jobs[0].URL != "https://jobs.polymer.co/acme/101" ||
		jobs[0].EmploymentType != "Full-time" ||
		!jobs[0].PostedAt.Equal(polymerTestPublishedTime) ||
		jobs[0].Description != "" {
		t.Fatalf("unexpected first list job: %+v", jobs[0])
	}
	if jobs[2].ID != "polymer/acme/103" {
		t.Fatalf("pagination order changed: %+v", jobs)
	}

	if err := src.Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if detailCalls.Load() != 1 {
		t.Fatalf("detail calls = %d, want 1", detailCalls.Load())
	}
	if jobs[0].Title != "R&D Engineer II" ||
		jobs[0].Location != "Bengaluru, IN" ||
		jobs[0].EmploymentType != "Permanent" ||
		jobs[0].Description != "Build & ship.\nOwn reliability." ||
		!jobs[0].PostedAt.Equal(polymerTestPublishedTime) {
		t.Fatalf("unexpected hydrated job: %+v", jobs[0])
	}
}

func TestPolymerFetchSupportsCustomJobBoardDomains(t *testing.T) {
	posting := polymerTestListPosting(101, "Engineer")
	posting["job_post_url"] = "https://careers.acme.test/101"
	src, closeServer := polymerSourceForPayload(t, polymerTestListPage(
		[]map[string]any{posting}, 1, 1, 1, "Acme & Co",
	))
	defer closeServer()

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].URL != "https://careers.acme.test/101" {
		t.Fatalf("Fetch = %+v", jobs)
	}
}

func TestPolymerEmptyBoardRequiresCoherentMetadata(t *testing.T) {
	for _, total := range []int{0, 1} {
		t.Run(fmt.Sprintf("total_%d", total), func(t *testing.T) {
			src, closeServer := polymerSourceForPayload(t, polymerTestListPage(
				[]map[string]any{}, 1, total, 0, "Acme & Co",
			))
			defer closeServer()
			jobs, err := src.Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if jobs == nil || len(jobs) != 0 {
				t.Fatalf("Fetch = %#v, want non-nil empty slice", jobs)
			}
		})
	}
}

func TestPolymerFetchRejectsSchemaAndCompletenessDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing items", func(p map[string]any) { delete(p, "items") }},
		{"null items", func(p map[string]any) { p["items"] = nil }},
		{"missing meta", func(p map[string]any) { delete(p, "meta") }},
		{"null meta", func(p map[string]any) { p["meta"] = nil }},
		{"missing total", polymerMutateMeta(func(m map[string]any) { delete(m, "total") })},
		{"missing is last", polymerMutateMeta(func(m map[string]any) { delete(m, "is_last") })},
		{"missing is first", polymerMutateMeta(func(m map[string]any) { delete(m, "is_first") })},
		{"missing page", polymerMutateMeta(func(m map[string]any) { delete(m, "page") })},
		{"missing next page", polymerMutateMeta(func(m map[string]any) { delete(m, "next_page") })},
		{"missing count", polymerMutateMeta(func(m map[string]any) { delete(m, "count") })},
		{"missing organization", polymerMutateMeta(func(m map[string]any) { delete(m, "organization_name") })},
		{"negative total", polymerMutateMeta(func(m map[string]any) { m["total"] = -1 })},
		{"negative count", polymerMutateMeta(func(m map[string]any) { m["count"] = -1 })},
		{"wrong page", polymerMutateMeta(func(m map[string]any) { m["page"] = 2 })},
		{"wrong first flag", polymerMutateMeta(func(m map[string]any) { m["is_first"] = false })},
		{"wrong last flag", polymerMutateMeta(func(m map[string]any) { m["is_last"] = false })},
		{"last has next", polymerMutateMeta(func(m map[string]any) { m["next_page"] = 2 })},
		{"empty organization", polymerMutateMeta(func(m map[string]any) { m["organization_name"] = " " })},
		{"page count over limit", polymerMutateMeta(func(m map[string]any) { m["total"] = polymerMaxPages + 1 })},
		{"job count over limit", polymerMutateMeta(func(m map[string]any) { m["count"] = polymerMaxPostings + 1 })},
		{"more pages than jobs", polymerMutateMeta(func(m map[string]any) { m["total"] = 2 })},
		{"reported count mismatch", polymerMutateMeta(func(m map[string]any) { m["count"] = 2 })},
		{"missing id", polymerMutatePosting(func(p map[string]any) { delete(p, "id") })},
		{"missing job id", polymerMutatePosting(func(p map[string]any) { delete(p, "job_id") })},
		{"zero id", polymerMutatePosting(func(p map[string]any) { p["id"], p["job_id"] = 0, 0 })},
		{"mismatched job id", polymerMutatePosting(func(p map[string]any) { p["job_id"] = 102 })},
		{"invalid hash", polymerMutatePosting(func(p map[string]any) { p["hash_id"] = "short" })},
		{"empty title", polymerMutatePosting(func(p map[string]any) { p["title"] = " " })},
		{"empty posting organization", polymerMutatePosting(func(p map[string]any) { p["organization_name"] = "" })},
		{"wrong posting organization", polymerMutatePosting(func(p map[string]any) { p["organization_name"] = "Other" })},
		{"empty employment type", polymerMutatePosting(func(p map[string]any) { p["kind_pretty"] = "" })},
		{"empty location", polymerMutatePosting(func(p map[string]any) {
			p["display_location"], p["remoteness_pretty"] = "", ""
		})},
		{"missing created at", polymerMutatePosting(func(p map[string]any) { delete(p, "created_at") })},
		{"missing created timestamp", polymerMutatePosting(func(p map[string]any) { delete(p, "created_at_timestamp") })},
		{"invalid created at", polymerMutatePosting(func(p map[string]any) { p["created_at"] = "yesterday" })},
		{"created timestamp mismatch", polymerMutatePosting(func(p map[string]any) { p["created_at_timestamp"] = 1 })},
		{"missing published at", polymerMutatePosting(func(p map[string]any) { delete(p, "published_at") })},
		{"missing published timestamp", polymerMutatePosting(func(p map[string]any) {
			delete(p, "published_at_timestamp")
		})},
		{"published timestamp mismatch", polymerMutatePosting(func(p map[string]any) {
			p["published_at_timestamp"] = 1
		})},
		{"missing URL", polymerMutatePosting(func(p map[string]any) { delete(p, "job_post_url") })},
		{"HTTP URL", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "http://jobs.polymer.co/acme/101"
		})},
		{"userinfo URL", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "https://user@jobs.polymer.co/acme/101"
		})},
		{"port URL", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "https://jobs.polymer.co:443/acme/101"
		})},
		{"query URL", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "https://jobs.polymer.co/acme/101?ref=jobs"
		})},
		{"bare query URL", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "https://jobs.polymer.co/acme/101?"
		})},
		{"fragment URL", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "https://jobs.polymer.co/acme/101#apply"
		})},
		{"wrong default path", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "https://jobs.polymer.co/other/101"
		})},
		{"wrong custom path", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "https://careers.acme.test/jobs/101"
		})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := polymerTestListPage(
				[]map[string]any{polymerTestListPosting(101, "Engineer")},
				1, 1, 1, "Acme & Co",
			)
			test.mutate(payload)
			src, closeServer := polymerSourceForPayload(t, payload)
			defer closeServer()
			polymerAssertNilFetchError(t, src)
		})
	}
}

func TestPolymerFetchRejectsCrossPageDriftAndDuplicates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"total drift", polymerMutateMeta(func(m map[string]any) { m["total"] = 3 })},
		{"count drift", polymerMutateMeta(func(m map[string]any) { m["count"] = 3 })},
		{"organization drift", polymerMutateMeta(func(m map[string]any) { m["organization_name"] = "Other" })},
		{"wrong second page", polymerMutateMeta(func(m map[string]any) { m["page"] = 1 })},
		{"wrong first flag", polymerMutateMeta(func(m map[string]any) { m["is_first"] = true })},
		{"not last", polymerMutateMeta(func(m map[string]any) { m["is_last"] = false })},
		{"next page on last", polymerMutateMeta(func(m map[string]any) { m["next_page"] = 3 })},
		{"duplicate ID", func(p map[string]any) {
			p["items"] = []map[string]any{polymerTestListPosting(101, "Duplicate")}
		}},
		{"different URL origin", polymerMutatePosting(func(p map[string]any) {
			p["job_post_url"] = "https://careers.acme.test/102"
		})},
		{"empty page", func(p map[string]any) { p["items"] = []map[string]any{} }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			firstPage := polymerTestListPage(
				[]map[string]any{polymerTestListPosting(101, "One")},
				1, 2, 2, "Acme & Co",
			)
			secondPage := polymerTestListPage(
				[]map[string]any{polymerTestListPosting(102, "Two")},
				2, 2, 2, "Acme & Co",
			)
			test.mutate(secondPage)
			src, closeServer := polymerTestSource(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "1" {
					polymerWriteJSON(t, w, firstPage)
				} else {
					polymerWriteJSON(t, w, secondPage)
				}
			})
			defer closeServer()
			polymerAssertNilFetchError(t, src)
		})
	}
}

func TestPolymerFetchRejectsHTTPProtocolViolations(t *testing.T) {
	valid := polymerTestListPage(
		[]map[string]any{polymerTestListPosting(101, "Engineer")},
		1, 1, 1, "Acme & Co",
	)
	validJSON, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"non-OK status", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upstream failed", http.StatusBadGateway)
		}},
		{"missing content type", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(validJSON)
		}},
		{"wrong content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(validJSON)
		}},
		{"malformed JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":`))
		}},
		{"trailing JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(append(validJSON, []byte(` {}`)...))
		}},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/moved" {
				polymerWriteJSON(t, w, valid)
				return
			}
			http.Redirect(w, r, "/moved?page=1", http.StatusFound)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src, closeServer := polymerTestSource(t, test.handler)
			defer closeServer()
			polymerAssertNilFetchError(t, src)
		})
	}

	t.Run("response size limit", func(t *testing.T) {
		src, closeServer := polymerTestSource(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"padding":"`))
			_, _ = w.Write([]byte(strings.Repeat("x", polymerMaxResponseBytes)))
			_, _ = w.Write([]byte(`"}`))
		})
		defer closeServer()
		polymerAssertNilFetchError(t, src)
	})

	t.Run("transport error", func(t *testing.T) {
		src := &polymer{
			company: polymerTestCompany,
			slug:    polymerTestSlug,
			apiBase: "https://api.polymer.co/v1/hire/organizations/acme/jobs",
			client: &http.Client{Transport: polymerRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("network unavailable")
			})},
		}
		polymerAssertNilFetchError(t, src)
	})
}

func TestPolymerDetailRejectsInvalidInputBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	src, closeServer := polymerTestSource(t, func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	})
	defer closeServer()

	if err := src.Detail(context.Background(), nil); err == nil {
		t.Fatal("nil job succeeded")
	}

	base := polymerTestJob(101)
	tests := []struct {
		name   string
		mutate func(*model.Job)
	}{
		{"wrong source prefix", func(j *model.Job) { j.ID = "other/acme/101" }},
		{"wrong board prefix", func(j *model.Job) { j.ID = "polymer/other/101" }},
		{"empty posting ID", func(j *model.Job) { j.ID = "polymer/acme/" }},
		{"zero posting ID", func(j *model.Job) { j.ID = "polymer/acme/0" }},
		{"leading-zero posting ID", func(j *model.Job) { j.ID = "polymer/acme/0101" }},
		{"non-numeric posting ID", func(j *model.Job) { j.ID = "polymer/acme/abc" }},
		{"overflow posting ID", func(j *model.Job) { j.ID = "polymer/acme/999999999999999999999" }},
		{"wrong company", func(j *model.Job) { j.Company = "Other" }},
		{"empty URL", func(j *model.Job) { j.URL = "" }},
		{"HTTP URL", func(j *model.Job) { j.URL = "http://jobs.polymer.co/acme/101" }},
		{"wrong URL ID", func(j *model.Job) { j.URL = "https://jobs.polymer.co/acme/102" }},
		{"query URL", func(j *model.Job) { j.URL += "?ref=mail" }},
		{"bare query URL", func(j *model.Job) { j.URL += "?" }},
		{"fragment URL", func(j *model.Job) { j.URL += "#apply" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := base
			test.mutate(&job)
			if err := src.Detail(context.Background(), &job); err == nil {
				t.Fatal("Detail succeeded")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid input made %d requests", requests.Load())
	}
}

func TestPolymerDetailRejectsInvalidResponseWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing id", func(p map[string]any) { delete(p, "id") }},
		{"mismatched ids", func(p map[string]any) { p["job_id"] = 102 }},
		{"wrong returned job", func(p map[string]any) { p["id"], p["job_id"] = 102, 102 }},
		{"invalid hash", func(p map[string]any) { p["hash_id"] = "short" }},
		{"empty title", func(p map[string]any) { p["title"] = " " }},
		{"empty organization", func(p map[string]any) { p["organization_name"] = "" }},
		{"empty employment type", func(p map[string]any) { p["kind_pretty"] = "" }},
		{"empty location", func(p map[string]any) {
			p["display_location"], p["remoteness_pretty"] = "", ""
		}},
		{"missing description", func(p map[string]any) { delete(p, "description") }},
		{"null description", func(p map[string]any) { p["description"] = nil }},
		{"empty description", func(p map[string]any) { p["description"] = "<p> </p>" }},
		{"missing archived marker", func(p map[string]any) { delete(p, "archived_at") }},
		{"empty archived marker", func(p map[string]any) { p["archived_at"] = "" }},
		{"archived", func(p map[string]any) { p["archived_at"] = "2026-01-04T00:00:00Z" }},
		{"invalid created date", func(p map[string]any) { p["created_at"] = "today" }},
		{"missing created timestamp", func(p map[string]any) { delete(p, "created_at_timestamp") }},
		{"published timestamp mismatch", func(p map[string]any) { p["published_at_timestamp"] = 1 }},
		{"HTTP posting URL", func(p map[string]any) {
			p["job_post_url"] = "http://jobs.polymer.co/acme/101"
		}},
		{"changed posting URL", func(p map[string]any) {
			p["job_post_url"] = "https://careers.acme.test/101"
		}},
		{"missing application URL", func(p map[string]any) {
			delete(p, "job_application_description_url")
		}},
		{"null application URL", func(p map[string]any) {
			p["job_application_description_url"] = nil
		}},
		{"different application URL", func(p map[string]any) {
			p["job_application_description_url"] = "https://careers.acme.test/101"
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := polymerTestDetailPosting(101, "Engineer")
			test.mutate(payload)
			src, closeServer := polymerSourceForPayload(t, payload)
			defer closeServer()

			job := polymerTestJob(101)
			job.Title = "before title"
			job.Location = "before location"
			job.EmploymentType = "before type"
			job.Description = "before description"
			job.PostedAt = time.Unix(1, 0)
			before := job
			if err := src.Detail(context.Background(), &job); err == nil {
				t.Fatal("Detail succeeded")
			}
			if job != before {
				t.Fatalf("Detail mutated job on error:\n got %+v\nwant %+v", job, before)
			}
		})
	}
}

func TestPolymerDetailRejectsHTTPProtocolViolationsWithoutMutation(t *testing.T) {
	valid := polymerTestDetailPosting(101, "Engineer")
	validJSON, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"not found", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "closed", http.StatusNotFound)
		}},
		{"wrong content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(validJSON)
		}},
		{"trailing JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(append(validJSON, []byte(` null`)...))
		}},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/moved") {
				polymerWriteJSON(t, w, valid)
				return
			}
			http.Redirect(w, r, r.URL.Path+"/moved", http.StatusTemporaryRedirect)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src, closeServer := polymerTestSource(t, test.handler)
			defer closeServer()
			job := polymerTestJob(101)
			before := job
			if err := src.Detail(context.Background(), &job); err == nil {
				t.Fatal("Detail succeeded")
			}
			if job != before {
				t.Fatal("Detail mutated job on HTTP error")
			}
		})
	}
}

func TestPolymerFetchAndDetailAreConcurrentSafe(t *testing.T) {
	list := polymerTestListPage(
		[]map[string]any{polymerTestListPosting(101, "Engineer")},
		1, 1, 1, "Acme & Co",
	)
	detail := polymerTestDetailPosting(101, "Engineer")
	src, closeServer := polymerTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/101") {
			polymerWriteJSON(t, w, detail)
			return
		}
		polymerWriteJSON(t, w, list)
	})
	defer closeServer()

	const workers = 12
	errs := make(chan error, workers*2)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(2)
		go func() {
			defer wait.Done()
			jobs, err := src.Fetch(context.Background())
			if err == nil && (len(jobs) != 1 || jobs[0].ID != "polymer/acme/101") {
				err = fmt.Errorf("unexpected jobs: %+v", jobs)
			}
			errs <- err
		}()
		go func() {
			defer wait.Done()
			job := polymerTestJob(101)
			err := src.Detail(context.Background(), &job)
			if err == nil && job.Description != "Build reliable systems.\nShip safely." {
				err = fmt.Errorf("unexpected detail: %+v", job)
			}
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func polymerTestListPosting(id int64, title string) map[string]any {
	return map[string]any{
		"id":                     id,
		"job_id":                 id,
		"hash_id":                fmt.Sprintf("%012d", id),
		"title":                  title,
		"organization_name":      "Acme &amp; Co",
		"kind_pretty":            "Full-time",
		"created_at":             polymerTestCreated,
		"published_at":           polymerTestPublished,
		"created_at_timestamp":   polymerTestCreatedTime.Unix(),
		"published_at_timestamp": polymerTestPublishedTime.Unix(),
		"display_location":       "Bengaluru, IN",
		"remoteness_pretty":      "Remote friendly",
		"job_post_url":           fmt.Sprintf("https://jobs.polymer.co/acme/%d", id),
	}
}

func polymerTestDetailPosting(id int64, title string) map[string]any {
	posting := polymerTestListPosting(id, title)
	posting["description"] = "<p>Build reliable systems.</p><ul><li>Ship safely.</li></ul>"
	posting["job_application_description_url"] = posting["job_post_url"]
	posting["archived_at"] = nil
	return posting
}

func polymerTestListPage(
	items []map[string]any,
	page, total, count int,
	organization string,
) map[string]any {
	isLast := page == total
	if count == 0 {
		isLast = true
	}
	var next any
	if !isLast {
		next = page + 1
	}
	return map[string]any{
		"items": items,
		"meta": map[string]any{
			"total":             total,
			"is_last":           isLast,
			"is_first":          page == 1,
			"page":              page,
			"next_page":         next,
			"count":             count,
			"organization_name": organization,
		},
	}
}

func polymerMutateMeta(mutate func(map[string]any)) func(map[string]any) {
	return func(payload map[string]any) {
		mutate(payload["meta"].(map[string]any))
	}
}

func polymerMutatePosting(mutate func(map[string]any)) func(map[string]any) {
	return func(payload map[string]any) {
		mutate(payload["items"].([]map[string]any)[0])
	}
}

func polymerTestJob(id int64) model.Job {
	return model.Job{
		ID:             fmt.Sprintf("polymer/acme/%d", id),
		Company:        polymerTestCompany,
		Title:          "Engineer",
		Location:       "Bengaluru, IN",
		URL:            fmt.Sprintf("https://jobs.polymer.co/acme/%d", id),
		EmploymentType: "Full-time",
		PostedAt:       polymerTestPublishedTime,
	}
}

func polymerTestSource(
	t *testing.T,
	handler http.HandlerFunc,
) (*polymer, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	return &polymer{
		company: polymerTestCompany,
		slug:    polymerTestSlug,
		apiBase: server.URL + "/v1/hire/organizations/acme/jobs",
		client:  server.Client(),
	}, server.Close
}

func polymerSourceForPayload(t *testing.T, payload any) (*polymer, func()) {
	t.Helper()
	return polymerTestSource(t, func(w http.ResponseWriter, _ *http.Request) {
		polymerWriteJSON(t, w, payload)
	})
}

func polymerWriteJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func polymerAssertNilFetchError(t *testing.T, src *polymer) {
	t.Helper()
	jobs, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatalf("Fetch succeeded: %+v", jobs)
	}
	if jobs != nil {
		t.Fatalf("Fetch returned partial jobs on error: %+v", jobs)
	}
}

type polymerRoundTripper func(*http.Request) (*http.Response, error)

func (f polymerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
