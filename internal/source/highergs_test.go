package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"jobwatch/internal/model"
)

func TestHigherGSFetchPaginatesAndNormalizes(t *testing.T) {
	const (
		total       = higherGSPageSize * 2
		maxPostings = higherGSPageSize + 1
	)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			OperationName string `json:"operationName"`
			Variables     struct {
				Input struct {
					Page struct {
						PageSize   int `json:"pageSize"`
						PageNumber int `json:"pageNumber"`
					} `json:"page"`
				} `json:"searchQueryInput"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.OperationName != "GetRoles" {
			t.Errorf("operation = %q", request.OperationName)
		}
		page := request.Variables.Input.Page
		if page.PageNumber != calls {
			t.Errorf("page = %d, want %d", page.PageNumber, calls)
		}
		if page.PageSize != higherGSPageSize {
			t.Errorf("page size = %d, want fixed size %d", page.PageSize, higherGSPageSize)
		}
		start := page.PageNumber * page.PageSize
		end := min(start+page.PageSize, total)
		items := make([]map[string]any, 0, end-start)
		for i := start; i < end; i++ {
			sourceID := i + 1
			items = append(items, map[string]any{
				"roleId":   fmt.Sprintf("%d_GS_MID_CAREER", sourceID),
				"jobTitle": fmt.Sprintf("Role %d", i), "status": "POSTED",
				"locations": []map[string]any{
					{"city": "Bengaluru", "state": "Karnataka", "country": "India", "primary": true},
				},
				"externalSource": map[string]string{"sourceId": fmt.Sprint(sourceID)},
			})
		}
		writeJSON(t, w, map[string]any{
			"data": map[string]any{"roleSearch": map[string]any{
				"totalCount": total, "items": items,
			}},
		})
		calls++
	}))
	defer server.Close()

	src := &higherGS{
		company: "Goldman Sachs", apiURL: server.URL, publicBase: "https://higher.gs.com",
		maxPostings: maxPostings, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(jobs) != maxPostings {
		t.Fatalf("calls=%d jobs=%d, want 2/%d", calls, len(jobs), maxPostings)
	}
	first, last := jobs[0], jobs[len(jobs)-1]
	if first.ID != "highergs/1" || first.Title != "Role 0" ||
		first.Location != "Bengaluru, Karnataka, India" ||
		first.URL != "https://higher.gs.com/roles/1" {
		t.Errorf("first = %#v", first)
	}
	if last.ID != "highergs/101" {
		t.Errorf("last ID = %q", last.ID)
	}
}

func TestHigherGSDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["operationName"] != "GetRoleById" {
			t.Errorf("operation = %v", request["operationName"])
		}
		writeJSON(t, w, map[string]any{
			"data": map[string]any{"role": map[string]any{
				"roleId": "179116_GS_MID_CAREER", "jobTitle": "Engineer",
				"descriptionHtml": "<p>Build &amp; ship.</p><p>One year.</p>",
				"locations": []map[string]any{
					{"city": "Bengaluru", "state": "Karnataka", "country": "India"},
				},
				"jobType": map[string]string{"code": "FT", "description": "Full-time"},
				"status":  "POSTED", "applyActive": true,
				"externalSource": map[string]any{
					"sourceId":               "179116",
					"externalApplicationUrl": higherGSTestApplicationURL("179116"),
				},
			}},
		})
	}))
	defer server.Close()
	src := &higherGS{
		company: "Goldman Sachs", apiURL: server.URL, publicBase: "https://higher.gs.com",
		client: server.Client(),
	}
	job := model.Job{ID: "highergs/179116"}
	if err := src.Detail(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Description != "Build & ship.\nOne year." ||
		job.Location != "Bengaluru, Karnataka, India" ||
		job.EmploymentType != "Full-time" ||
		job.URL != higherGSTestApplicationURL("179116") {
		t.Errorf("detail = %#v", job)
	}
}

func TestHigherGSFetchRequiresPostedStatus(t *testing.T) {
	for _, status := range []string{"", "CLOSED"} {
		t.Run(fmt.Sprintf("status_%q", status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{
					"data": map[string]any{"roleSearch": map[string]any{
						"totalCount": 1,
						"items": []map[string]any{{
							"jobTitle": "Role", "status": status,
							"externalSource": map[string]string{"sourceId": "1"},
						}},
					}},
				})
			}))
			defer server.Close()

			src := &higherGS{
				company: "Goldman Sachs", apiURL: server.URL,
				publicBase: "https://higher.gs.com", maxPostings: 10,
				client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), "want POSTED") {
				t.Fatalf("Fetch = %#v, %v; want nil jobs and a status error", jobs, err)
			}
		})
	}
}

func TestHigherGSFetchRejectsInvalidSourceIDs(t *testing.T) {
	for _, sourceID := range []string{"", "0", "0123", "abc", "123/extra"} {
		t.Run(fmt.Sprintf("source_%q", sourceID), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{
					"data": map[string]any{"roleSearch": map[string]any{
						"totalCount": 1,
						"items": []map[string]any{{
							"jobTitle": "Role", "status": "POSTED",
							"externalSource": map[string]string{"sourceId": sourceID},
						}},
					}},
				})
			}))
			defer server.Close()

			src := &higherGS{
				company: "Goldman Sachs", apiURL: server.URL,
				publicBase: "https://higher.gs.com", maxPostings: 10,
				client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil || !strings.Contains(err.Error(), "invalid external source ID") {
				t.Fatalf("Fetch = %#v, %v; want nil jobs and an ID error", jobs, err)
			}
		})
	}
}

func TestHigherGSFetchRejectsPaginationOvershoot(t *testing.T) {
	tests := []struct {
		name  string
		total int
		count int
	}{
		{"page size", higherGSPageSize + 1, higherGSPageSize + 1},
		{"total count", 1, 2},
		{"short non-terminal page", higherGSPageSize * 2, higherGSPageSize / 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				items := make([]map[string]any, 0, test.count)
				for i := 1; i <= test.count; i++ {
					items = append(items, map[string]any{
						"jobTitle": "Role", "status": "POSTED",
						"externalSource": map[string]string{"sourceId": fmt.Sprint(i)},
					})
				}
				writeJSON(t, w, map[string]any{
					"data": map[string]any{"roleSearch": map[string]any{
						"totalCount": test.total, "items": items,
					}},
				})
			}))
			defer server.Close()

			src := &higherGS{
				company: "Goldman Sachs", apiURL: server.URL,
				publicBase: "https://higher.gs.com", maxPostings: 200,
				client: server.Client(),
			}
			jobs, err := src.Fetch(context.Background())
			if err == nil || jobs != nil {
				t.Fatalf("Fetch = %#v, %v; want nil jobs and a pagination error", jobs, err)
			}
		})
	}
}

func TestHigherGSDetailRejectsInvalidJobBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	src := &higherGS{apiURL: server.URL, client: server.Client()}
	if err := src.Detail(context.Background(), nil); err == nil {
		t.Fatal("Detail(nil) succeeded")
	}
	for _, id := range []string{
		"", "highergs/", "highergs/0", "highergs/0123", "highergs/abc",
		"highergs/123/extra", "other/123",
	} {
		t.Run(id, func(t *testing.T) {
			job := model.Job{ID: id}
			if err := src.Detail(context.Background(), &job); err == nil {
				t.Fatalf("Detail(%q) succeeded", id)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid jobs made %d requests", calls.Load())
	}
}

func TestHigherGSDetailRejectsInactiveOrUnsafeResponseWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing status", func(role map[string]any) { delete(role, "status") }},
		{"closed status", func(role map[string]any) { role["status"] = "CLOSED" }},
		{"inactive application", func(role map[string]any) { role["applyActive"] = false }},
		{"missing application URL", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] = ""
		}},
		{"relative application URL", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] = "/apply/179116"
		}},
		{"HTTP application URL", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] =
				"http://hdpc.fa.us2.oraclecloud.com/hcmUI/CandidateExperience/en/sites/LateralHiring/job/179116/apply/email"
		}},
		{"userinfo application URL", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] =
				"https://user@hdpc.fa.us2.oraclecloud.com/hcmUI/CandidateExperience/en/sites/LateralHiring/job/179116/apply/email"
		}},
		{"fragment application URL", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] =
				higherGSTestApplicationURL("179116") + "#apply"
		}},
		{"empty fragment application URL", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] =
				higherGSTestApplicationURL("179116") + "#"
		}},
		{"query application URL", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] =
				higherGSTestApplicationURL("179116") + "?redirect=https%3A%2F%2Fevil.example"
		}},
		{"wrong application host", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] =
				"https://example.oraclecloud.com/hcmUI/CandidateExperience/en/sites/LateralHiring/job/179116/apply/email"
		}},
		{"wrong application path", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] =
				"https://hdpc.fa.us2.oraclecloud.com/apply/179116"
		}},
		{"wrong application job", func(role map[string]any) {
			role["externalSource"].(map[string]any)["externalApplicationUrl"] =
				higherGSTestApplicationURL("999999")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			role := higherGSTestDetailRole("179116")
			test.mutate(role)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{
					"data": map[string]any{"role": role},
				})
			}))
			defer server.Close()

			src := &higherGS{apiURL: server.URL, client: server.Client()}
			job := model.Job{
				ID: "highergs/179116", Company: "Goldman Sachs", Title: "Before",
				Location: "Before", URL: "https://higher.gs.com/roles/179116",
				EmploymentType: "Before", Description: "Before",
			}
			before := job
			if err := src.Detail(context.Background(), &job); err == nil {
				t.Fatal("Detail succeeded")
			}
			if job != before {
				t.Fatalf("Detail mutated job on error:\ngot  %#v\nwant %#v", job, before)
			}
		})
	}
}

func TestHigherGSRejectsGraphQLErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"errors": []map[string]string{{"message": "upstream failed"}},
		})
	}))
	defer server.Close()
	src := &higherGS{
		company: "Goldman Sachs", apiURL: server.URL, publicBase: "https://higher.gs.com",
		maxPostings: 10, client: server.Client(),
	}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil || !strings.Contains(err.Error(), "upstream failed") {
		t.Fatalf("Fetch = %#v, %v", jobs, err)
	}
}

func higherGSTestDetailRole(sourceID string) map[string]any {
	return map[string]any{
		"roleId": sourceID + "_GS_MID_CAREER", "jobTitle": "Engineer",
		"descriptionHtml": "<p>Build &amp; ship.</p>",
		"locations": []map[string]any{
			{"city": "Bengaluru", "state": "Karnataka", "country": "India"},
		},
		"jobType": map[string]string{"code": "FT", "description": "Full-time"},
		"status":  "POSTED", "applyActive": true,
		"externalSource": map[string]any{
			"sourceId": sourceID, "externalApplicationUrl": higherGSTestApplicationURL(sourceID),
		},
	}
}

func higherGSTestApplicationURL(sourceID string) string {
	return "https://hdpc.fa.us2.oraclecloud.com/hcmUI/CandidateExperience/en/sites/" +
		"LateralHiring/job/" + sourceID + "/apply/email"
}
