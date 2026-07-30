package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"jobwatch/internal/params"
)

func TestKekaFetchNormalizesCompletePostingArray(t *testing.T) {
	identifier := customTestUUID(99)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/careers/api/embedjobs/default/active/"+identifier {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(t, w, []map[string]any{
			{
				"id": 82288, "title": "  Associate Director  ",
				"description": "<p>Own growth &amp; launches.</p><ul><li>Lead</li></ul>",
				"jobType":     2, "publishedOn": "2026-07-30T05:34:23.67Z",
				"jobLocations": []map[string]string{
					{"name": "Noida", "city": "Noida", "countryName": "India"},
					{"name": " Remote "},
				},
			},
		})
	}))
	defer srv.Close()

	src := &keka{
		company: "SquadStack", host: "squadrun.keka.com", portal: "default",
		identifier: identifier, base: srv.URL, client: srv.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("Fetch returned %d jobs, want 1", len(jobs))
	}
	job := jobs[0]
	if job.ID != "keka/squadrun.keka.com/default/82288" {
		t.Errorf("ID = %q", job.ID)
	}
	if job.Title != "Associate Director" || job.Company != "SquadStack" {
		t.Errorf("job = %+v", job)
	}
	if job.Location != "Noida; Remote" {
		t.Errorf("Location = %q", job.Location)
	}
	if job.EmploymentType != "Full Time" {
		t.Errorf("EmploymentType = %q", job.EmploymentType)
	}
	if job.Description != "Own growth & launches.\nLead" {
		t.Errorf("Description = %q", job.Description)
	}
	wantTime, _ := time.Parse(time.RFC3339Nano, "2026-07-30T05:34:23.67Z")
	if !job.PostedAt.Equal(wantTime) {
		t.Errorf("PostedAt = %s, want %s", job.PostedAt, wantTime)
	}
	if job.URL != srv.URL+"/careers/jobdetails/82288" {
		t.Errorf("URL = %q", job.URL)
	}
}

func TestKekaRejectsNullAndDuplicatePostingArrays(t *testing.T) {
	for name, payload := range map[string]any{
		"null": nil,
		"duplicate": []map[string]any{
			{"id": 1, "title": "One", "description": "<p>Full</p>"},
			{"id": 1, "title": "Two", "description": "<p>Full</p>"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, payload)
			}))
			defer srv.Close()
			src := &keka{
				company: "Acme", host: "acme.keka.com", portal: "default",
				identifier: customTestUUID(1), base: srv.URL, client: srv.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil {
				t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
			}
		})
	}
}

func TestKekaFactoryValidatesConnectionParams(t *testing.T) {
	valid := params.Map{
		"host": "squadrun.keka.com", "portal": "default",
		"identifier": "c750f148-70b8-4a21-868e-f891a1b2d818",
	}
	for name, mutate := range map[string]func(params.Map){
		"host with scheme": func(p params.Map) { p["host"] = "https://squadrun.keka.com" },
		"invalid portal":   func(p params.Map) { p["portal"] = "not/a/portal" },
		"invalid identifier": func(p params.Map) {
			p["identifier"] = "not-a-uuid"
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := params.Map{}
			for key, value := range valid {
				p[key] = value
			}
			mutate(p)
			if _, err := New("keka", "Acme", p, &http.Client{}); err == nil {
				t.Fatal("New succeeded, want parameter error")
			}
		})
	}
}
