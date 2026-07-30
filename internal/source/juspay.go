package source

// Juspay's careers UI revalidates from this public JSON endpoint. It returns
// every global opening with the full description in one request.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const juspayMaxJobIDLength = 64

// Juspay currently emits opaque identifiers made from uppercase alphanumeric
// segments separated by single hyphens (for example DEV-BE01 and SBDM01).
var juspayJobIDRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$`)

func init() {
	Register("juspay", func(company string, p params.Map, client *http.Client) (Source, error) {
		if len(p) != 0 {
			keys := make([]string, 0, len(p))
			for key := range p {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("juspay source does not accept params (got %s)", strings.Join(keys, ", "))
		}
		if client == nil {
			client = http.DefaultClient
		}
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
		id := posting.JobID
		if len(id) > juspayMaxJobIDLength || !juspayJobIDRE.MatchString(id) {
			return nil, fmt.Errorf("juspay: item %d has invalid job_id %q", i, id)
		}
		title := strings.TrimSpace(posting.Title)
		description := strings.TrimSpace(posting.Description)
		if title == "" || description == "" {
			return nil, fmt.Errorf("juspay: item %d omitted title or description", i)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("juspay: duplicate job ID %q", id)
		}
		seen[id] = struct{}{}
		publicURL, err := url.JoinPath(s.publicBase, id)
		if err != nil {
			return nil, fmt.Errorf("juspay: item %d: building public URL: %w", i, err)
		}
		jobs = append(jobs, model.Job{
			ID:             "juspay/" + id,
			Company:        s.company,
			Title:          title,
			Location:       strings.TrimSpace(posting.Location),
			URL:            publicURL,
			EmploymentType: strings.TrimSpace(posting.EmploymentType),
			Description:    description,
		})
	}
	return jobs, nil
}
