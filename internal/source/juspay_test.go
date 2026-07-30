package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
		publicBase: "https://juspay.io/careers", client: server.Client(),
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
