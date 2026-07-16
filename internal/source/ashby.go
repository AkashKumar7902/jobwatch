package source

// Ashby public posting API (no auth):
//
//	GET https://api.ashbyhq.com/posting-api/job-board/{name}
//
// Docs: https://developers.ashbyhq.com/reference/jobpostingapi
//
// Config:
//
//	- name: Deel
//	  source: ashby
//	  params:
//	    board_name: deel     # from jobs.ashbyhq.com/{board_name}

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("ashby", func(company string, p params.Map, client *http.Client) (Source, error) {
		board, err := p.Require("board_name")
		if err != nil {
			return nil, err
		}
		return &ashby{company: company, board: board, client: client}, nil
	})
}

type ashby struct {
	company string
	board   string
	client  *http.Client
}

func (a *ashby) Company() string { return a.company }

func (a *ashby) Fetch(ctx context.Context) ([]model.Job, error) {
	var payload struct {
		Jobs []struct {
			ID              string `json:"id"`
			Title           string `json:"title"`
			Location        string `json:"location"`
			JobURL          string `json:"jobUrl"`
			PublishedAt     string `json:"publishedAt"`
			IsListed        bool   `json:"isListed"`
			DescriptionHTML string `json:"descriptionHtml"`
		} `json:"jobs"`
	}
	url := fmt.Sprintf("https://api.ashbyhq.com/posting-api/job-board/%s", a.board)
	if err := fetchJSON(ctx, a.client, http.MethodGet, url, nil, &payload); err != nil {
		return nil, err
	}

	jobs := make([]model.Job, 0, len(payload.Jobs))
	for _, j := range payload.Jobs {
		if !j.IsListed {
			continue
		}
		postedAt, _ := time.Parse(time.RFC3339, j.PublishedAt) // zero on parse failure
		jobs = append(jobs, model.Job{
			ID:          fmt.Sprintf("ashby/%s/%s", a.board, j.ID),
			Company:     a.company,
			Title:       j.Title,
			Location:    j.Location,
			URL:         j.JobURL,
			Description: htmltext.ToText(j.DescriptionHTML),
			PostedAt:    postedAt,
		})
	}
	return jobs, nil
}
