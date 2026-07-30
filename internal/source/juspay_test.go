package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jobwatch/internal/params"
)

func TestJuspayRegistrationAndParams(t *testing.T) {
	src, err := New("juspay", "JUSPAY", params.Map{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	implementation := src.(*identifiedSource).Source.(*juspay)
	if implementation.client != http.DefaultClient {
		t.Fatalf("nil client = %p, want http.DefaultClient", implementation.client)
	}

	_, err = New("juspay", "JUSPAY", params.Map{"z": "1", "a": "2"}, nil)
	if err == nil || !strings.Contains(err.Error(), "got a, z") {
		t.Fatalf("unknown params error = %v, want sorted names", err)
	}
}

func TestJuspayFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"allJobs": []map[string]any{
			{
				"category": "Engineering", "is_global": true,
				"job_description_career": "**Build systems.**\n\n0-2 years.",
				"job_id":                 "DEV-BE01", "job_location": "Bangalore",
				"job_title": " Software Development Engineer Backend ",
				"job_type":  "Full-Time", "opening_status": true,
			},
			{
				"is_global": true, "opening_status": false,
				"job_id": "CLOSED", "job_title": "Closed", "job_description_career": "Closed",
			},
			{
				"is_global": false, "opening_status": true,
				"job_id": "LOCAL", "job_title": "Local", "job_description_career": "Local",
			},
		}})
	}))
	defer server.Close()
	src := &juspay{
		company: "JUSPAY", apiURL: server.URL,
		publicBase: "https://juspay.io/careers/", client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %#v", jobs)
	}
	job := jobs[0]
	if job.ID != "juspay/DEV-BE01" || job.Company != "JUSPAY" ||
		job.Title != "Software Development Engineer Backend" ||
		job.Location != "Bangalore" ||
		job.URL != "https://juspay.io/careers/DEV-BE01" ||
		job.EmploymentType != "Full-Time" ||
		job.Description != "**Build systems.**\n\n0-2 years." {
		t.Errorf("job = %#v", job)
	}
}

func TestJuspayRejectsMalformedJobIDsAtomically(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "empty", id: ""},
		{name: "leading whitespace", id: " DEV-BE01"},
		{name: "trailing whitespace", id: "DEV-BE01 "},
		{name: "slash", id: "DEV/BE01"},
		{name: "backslash", id: `DEV\BE01`},
		{name: "dot traversal", id: "../DEV-BE01"},
		{name: "dot segment", id: "DEV-..-BE01"},
		{name: "query", id: "DEV-BE01?next=evil"},
		{name: "fragment", id: "DEV-BE01#fragment"},
		{name: "percent encoded slash", id: "DEV%2FBE01"},
		{name: "lowercase", id: "dev-BE01"},
		{name: "underscore", id: "DEV_BE01"},
		{name: "leading hyphen", id: "-DEV-BE01"},
		{name: "trailing hyphen", id: "DEV-BE01-"},
		{name: "empty segment", id: "DEV--BE01"},
		{name: "leading digit", id: "1DEV-BE01"},
		{name: "non ascii", id: "DEV-BÉ01"},
		{name: "overlong", id: strings.Repeat("A", juspayMaxJobIDLength+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"allJobs": []map[string]any{
					{
						"is_global": true, "opening_status": true,
						"job_id": "SBDM01", "job_title": "Valid first",
						"job_description_career": "Description",
					},
					{
						"is_global": true, "opening_status": true,
						"job_id": test.id, "job_title": "Invalid second",
						"job_description_career": "Description",
					},
				}})
			}))
			defer server.Close()

			src := &juspay{
				company: "JUSPAY", apiURL: server.URL,
				publicBase: "https://juspay.io/careers", client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), "item 1") ||
				!strings.Contains(err.Error(), "invalid job_id") {
				t.Fatalf("Fetch = %#v, %v, want atomic invalid job_id failure", jobs, err)
			}
		})
	}
}

func TestJuspayRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"allJobs": []map[string]any{{
			"is_global": true, "opening_status": true, "job_id": "ID", "job_title": "Role",
		}}})
	}))
	defer server.Close()
	src := &juspay{apiURL: server.URL, client: server.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "omitted") {
		t.Fatalf("Fetch = %#v, %v", jobs, err)
	}
}
