package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestForty2GearsReadsCompleteFlightPayloadAndDetails(t *testing.T) {
	postings := []forty2GearsPosting{
		{
			Title: "Lead- Product Security", Excerpt: "Truncated",
			Href:     "/careers/lead-product-security/",
			JobTypes: []string{"Full Time"}, JobLocations: []string{"Bengaluru", "India"},
		},
		{
			Title: "Cloud Engineer", Excerpt: "Truncated",
			Href:     "/careers/cloud-engineer/",
			JobTypes: []string{"Full Time"}, JobLocations: []string{"Remote", "India"},
		},
	}
	itemsJSON, err := json.Marshal(postings)
	if err != nil {
		t.Fatal(err)
	}
	listFlight := `1e:{"model":{"heading":"Current Openings","items":[[]]},"items":` +
		string(itemsJSON) + `}`
	detailCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/careers/":
			fmt.Fprint(w, customTestFlight(t, listFlight))
		case "/careers/lead-product-security/":
			detailCalls++
			fmt.Fprint(w, `<h1 class="text-4xl font-bold">Lead- Product Security</h1>`)
			fmt.Fprint(w, customTestFlight(
				t,
				`<p class="wp-block-paragraph">Build <strong>securely.</strong></p>`+
					`<ul class="wp-block-list"><li>Own reviews</li></ul>`,
			))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := &forty2Gears{company: "42Gears", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || detailCalls != 0 {
		t.Fatalf("Fetch returned %d jobs, detail calls=%d", len(jobs), detailCalls)
	}
	first := &jobs[0]
	if first.ID != "forty2gears/lead-product-security" ||
		first.Location != "Bengaluru, India" || first.EmploymentType != "Full Time" {
		t.Errorf("first job = %+v", *first)
	}
	if err := src.Detail(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 || first.Description != "Build securely.\nOwn reviews" {
		t.Errorf("detail calls=%d description=%q", detailCalls, first.Description)
	}
}

func TestForty2GearsRejectsInitialPageOnlyFlightModel(t *testing.T) {
	payload := `1e:{"model":{"heading":"Current Openings","items":[[]]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, customTestFlight(t, payload))
	}))
	defer srv.Close()
	src := &forty2Gears{company: "42Gears", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}
