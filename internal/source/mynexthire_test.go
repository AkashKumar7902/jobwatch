package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMyNextHireFetch(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/employer/careers/reqlist/get" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["source"] != "careers" || body["filterByBuId"] != float64(-1) {
			t.Errorf("body = %#v", body)
		}
		writeJSON(t, w, map[string]any{
			"reqDetailsBOList": []any{
				map[string]any{
					"reqId": 2416, "statusId": 3, "reqTitle": " AI Animator ",
					"jdDisplay": "<not html> Complete description ", "employmentType": "contract",
					"approvedOn": "2026-07-20T10:17:57.424+0000",
					"locationList": []map[string]string{
						{"office": "India", "address": "India"},
						{"office": " Remote "},
						{"office": "India"},
					},
				},
			},
		})
	}))
	defer server.Close()

	src := &myNextHire{
		company: "ShareChat", host: "sharechat.mynexthire.com",
		baseURL: server.URL, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(jobs) != 1 {
		t.Fatalf("calls=%d jobs=%d, want 1/1", calls, len(jobs))
	}
	job := jobs[0]
	if job.ID != "mynexthire/sharechat.mynexthire.com/2416" {
		t.Errorf("ID = %q", job.ID)
	}
	if job.Company != "ShareChat" || job.Title != "AI Animator" {
		t.Errorf("company/title = %q/%q", job.Company, job.Title)
	}
	if job.Location != "India; Remote" {
		t.Errorf("Location = %q", job.Location)
	}
	if job.Description != "<not html> Complete description" {
		t.Errorf("Description = %q", job.Description)
	}
	if job.EmploymentType != "contract" {
		t.Errorf("EmploymentType = %q", job.EmploymentType)
	}
	wantTime, _ := time.Parse("2006-01-02T15:04:05.999-0700", "2026-07-20T10:17:57.424+0000")
	if !job.PostedAt.Equal(wantTime) {
		t.Errorf("PostedAt = %s, want %s", job.PostedAt, wantTime)
	}

	parsed, err := url.Parse(job.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "sharechat.mynexthire.com" || parsed.Path != "/employer/jobs" ||
		parsed.Query().Get("src") != "careers" {
		t.Errorf("URL = %s", job.URL)
	}
	payload, err := base64.StdEncoding.DecodeString(parsed.Query().Get("p"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"reqId":2416`) {
		t.Errorf("URL payload = %s", payload)
	}
}

func TestMyNextHireRejectsMalformedBoards(t *testing.T) {
	for name, payload := range map[string]any{
		"missing list": map[string]any{},
		"bad id": map[string]any{"reqDetailsBOList": []any{
			map[string]any{"reqId": 0, "statusId": 3, "reqTitle": "Role", "jdDisplay": "Description"},
		}},
		"missing title": map[string]any{"reqDetailsBOList": []any{
			map[string]any{"reqId": 1, "statusId": 3, "reqTitle": " ", "jdDisplay": "Description"},
		}},
		"missing description": map[string]any{"reqDetailsBOList": []any{
			map[string]any{"reqId": 1, "statusId": 3, "reqTitle": "Role", "jdDisplay": " "},
		}},
		"duplicate id": map[string]any{"reqDetailsBOList": []any{
			map[string]any{"reqId": 1, "statusId": 3, "reqTitle": "One", "jdDisplay": "Description"},
			map[string]any{"reqId": 1, "statusId": 3, "reqTitle": "Two", "jdDisplay": "Description"},
		}},
		"missing active status": map[string]any{"reqDetailsBOList": []any{
			map[string]any{"reqId": 1, "reqTitle": "Role", "jdDisplay": "Description"},
		}},
		"inactive status": map[string]any{"reqDetailsBOList": []any{
			map[string]any{"reqId": 1, "statusId": 4, "reqTitle": "Role", "jdDisplay": "Description"},
		}},
		"malformed approval date": map[string]any{"reqDetailsBOList": []any{
			map[string]any{
				"reqId": 1, "statusId": 3, "reqTitle": "Role",
				"jdDisplay": "Description", "approvedOn": "not-a-date",
			},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, payload)
			}))
			defer server.Close()
			src := &myNextHire{
				company: "Acme", host: "acme.mynexthire.com",
				baseURL: server.URL, client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil {
				t.Fatalf("Fetch = %#v, %v; want nil error result", jobs, err)
			}
		})
	}
}

func TestMyNextHireAllowsEmptyApprovedOn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"reqDetailsBOList": []any{
				map[string]any{
					"reqId": 1, "statusId": 3, "reqTitle": "Role",
					"jdDisplay": "Description", "approvedOn": " ",
					"location": "Pune",
					"locationList": []map[string]string{
						{"office": " ", "address": " "},
					},
				},
			},
		})
	}))
	defer server.Close()

	src := &myNextHire{
		company: "Acme", host: "acme.mynexthire.com",
		baseURL: server.URL, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || !jobs[0].PostedAt.IsZero() || jobs[0].Location != "Pune" {
		t.Fatalf("Fetch = %#v, want one Pune job with a zero PostedAt", jobs)
	}
}
