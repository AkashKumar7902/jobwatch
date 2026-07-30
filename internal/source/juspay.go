package source

// Juspay's careers UI revalidates from this public JSON endpoint. It returns
// every global opening with the full description in one request.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("juspay", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &juspay{
			company:    company,
			apiURL:     "https://joinus.juspay.in/api/careerJobOpening?limit=1000&isGlobal=true",
			publicBase: "https://juspay.io/careers",
			client:     client,
		}, nil
	})
}

type juspay struct {
	company    string
	apiURL     string
	publicBase string
	client     *http.Client
}

func (s *juspay) Company() string { return s.company }

func (s *juspay) Fetch(ctx context.Context) ([]model.Job, error) {
	var response struct {
		AllJobs *[]struct {
			Category       string `json:"category"`
			IsGlobal       bool   `json:"is_global"`
			Description    string `json:"job_description_career"`
			JobID          string `json:"job_id"`
			Location       string `json:"job_location"`
			Title          string `json:"job_title"`
			EmploymentType string `json:"job_type"`
			Opening        bool   `json:"opening_status"`
		} `json:"allJobs"`
	}
	if err := fetchJSON(ctx, s.client, http.MethodGet, s.apiURL, nil, &response); err != nil {
		return nil, err
	}
	if response.AllJobs == nil {
		return nil, fmt.Errorf("juspay: response omitted allJobs")
	}
	jobs := make([]model.Job, 0, len(*response.AllJobs))
	seen := make(map[string]struct{}, len(*response.AllJobs))
	for i, posting := range *response.AllJobs {
		if !posting.IsGlobal || !posting.Opening {
			continue
		}
		id := strings.TrimSpace(posting.JobID)
		title := strings.TrimSpace(posting.Title)
		description := strings.TrimSpace(posting.Description)
		if id == "" || title == "" || description == "" {
			return nil, fmt.Errorf("juspay: item %d omitted job ID, title, or description", i)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("juspay: duplicate job ID %q", id)
		}
		seen[id] = struct{}{}
		jobs = append(jobs, model.Job{
			ID:             "juspay/" + id,
			Company:        s.company,
			Title:          title,
			Location:       strings.TrimSpace(posting.Location),
			URL:            s.publicBase + "/" + id,
			EmploymentType: strings.TrimSpace(posting.EmploymentType),
			Description:    description,
		})
	}
	return jobs, nil
}
