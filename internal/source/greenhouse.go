package source

// Greenhouse public job-board API (no auth):
//
//	GET https://boards-api.greenhouse.io/v1/boards/{board_token}/jobs?content=true
//
// ?content=true includes each job's description (HTML-escaped HTML).
// Docs: https://developers.greenhouse.io/job-board.html
//
// EU-hosted boards live under boards.eu.greenhouse.io; set `region: eu`.
//
// Config:
//
//	- name: GitLab
//	  source: greenhouse
//	  params:
//	    board_token: gitlab
//	    region: eu           # optional

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
	Register("greenhouse", func(company string, p params.Map, client *http.Client) (Source, error) {
		token, err := p.Require("board_token")
		if err != nil {
			return nil, err
		}
		host := "boards-api.greenhouse.io"
		if p.Get("region") == "eu" {
			host = "boards.eu.greenhouse.io"
		}
		return &greenhouse{company: company, token: token, host: host, client: client}, nil
	})
}

type greenhouse struct {
	company string
	token   string
	host    string
	client  *http.Client
}

func (g *greenhouse) Company() string { return g.company }

func (g *greenhouse) Fetch(ctx context.Context) ([]model.Job, error) {
	var payload struct {
		Jobs []struct {
			ID          int64     `json:"id"`
			Title       string    `json:"title"`
			AbsoluteURL string    `json:"absolute_url"`
			UpdatedAt   time.Time `json:"updated_at"`
			Location    struct {
				Name string `json:"name"`
			} `json:"location"`
			Content string `json:"content"`
		} `json:"jobs"`
	}
	url := fmt.Sprintf("https://%s/v1/boards/%s/jobs?content=true", g.host, g.token)
	if err := fetchJSON(ctx, g.client, http.MethodGet, url, nil, &payload); err != nil {
		return nil, err
	}

	jobs := make([]model.Job, 0, len(payload.Jobs))
	for _, j := range payload.Jobs {
		jobs = append(jobs, model.Job{
			ID:          fmt.Sprintf("greenhouse/%s/%d", g.token, j.ID),
			Company:     g.company,
			Title:       j.Title,
			Location:    j.Location.Name,
			URL:         j.AbsoluteURL,
			Description: htmltext.ToText(j.Content),
			PostedAt:    j.UpdatedAt,
		})
	}
	return jobs, nil
}
