package source

// Wayfair runs its ATS on Avature, but the direct Avature endpoints are
// session-gated. Wayfair's own careers front-end proxies the same data as a
// single unauthenticated JSON POST that returns every active posting with
// full descriptions — so this source is Wayfair-specific, not a reusable
// ATS platform:
//
//	POST https://www.wayfair.com/a/careers/careers/job_search_data
//	     body: {"categoryIds":[],"teamIds":[],...,"keywords":""}  (empty = all)
//
// Config (no params needed):
//
//	- name: Wayfair
//	  source: wayfair

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("wayfair", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &wayfair{company: company, client: client}, nil
	})
}

type wayfair struct {
	company string
	client  *http.Client
}

func (w *wayfair) Company() string { return w.company }

func (w *wayfair) Fetch(ctx context.Context) ([]model.Job, error) {
	var payload struct {
		JobListData []struct {
			ID                 int64  `json:"id"`
			RequisitionID      string `json:"requisitionId"`
			Title              string `json:"title"`
			Description        string `json:"description"` // full HTML
			JobTypeDisplayName string `json:"jobTypeDisplayName"`
			ApplyLink          string `json:"applyLink"`
			IsActive           bool   `json:"isActive"`
			IsHidden           bool   `json:"isHidden"`
			Location           struct {
				Name    string `json:"name"`
				City    string `json:"city"`
				Country string `json:"country"`
			} `json:"location"`
		} `json:"jobListData"`
	}
	// Empty filter arrays return every posting; searchText is unused.
	body := []byte(`{"categoryIds":[],"teamIds":[],"locationIds":[],"countryIds":[],"teamCategoryIds":[],"stateIds":[],"selectedJobTypeIds":[],"keywords":""}`)
	url := "https://www.wayfair.com/a/careers/careers/job_search_data"
	if err := fetchJSON(ctx, w.client, http.MethodPost, url, body, &payload); err != nil {
		return nil, err
	}

	jobs := make([]model.Job, 0, len(payload.JobListData))
	for _, j := range payload.JobListData {
		if !j.IsActive || j.IsHidden {
			continue
		}
		loc := j.Location.Name
		if loc == "" {
			loc = j.Location.City
		}
		url := j.ApplyLink
		if url == "" {
			url = "https://www.wayfair.com/careers/jobs"
		}
		jobs = append(jobs, model.Job{
			ID:             "wayfair/" + strconv.FormatInt(j.ID, 10),
			Company:        w.company,
			Title:          j.Title,
			Location:       loc,
			URL:            url,
			EmploymentType: j.JobTypeDisplayName,
			Description:    htmltext.ToText(j.Description),
		})
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("wayfair returned no active postings (endpoint or schema may have changed)")
	}
	return jobs, nil
}
