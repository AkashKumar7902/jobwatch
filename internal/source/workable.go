package source

// Workable public widget API (no auth):
//
//	GET https://apply.workable.com/api/v1/widget/accounts/{account}?details=true
//
// ?details=true includes each job's HTML description.
//
// Config:
//
//	- name: Doist
//	  source: workable
//	  params:
//	    account: doist       # from apply.workable.com/{account}

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("workable", func(company string, p params.Map, client *http.Client) (Source, error) {
		account, err := p.Require("account")
		if err != nil {
			return nil, err
		}
		return &workable{company: company, account: account, client: client}, nil
	})
}

type workable struct {
	company string
	account string
	client  *http.Client
}

func (w *workable) Company() string { return w.company }

func (w *workable) Fetch(ctx context.Context) ([]model.Job, error) {
	var payload struct {
		Jobs []struct {
			Title       string `json:"title"`
			Shortcode   string `json:"shortcode"`
			URL         string `json:"url"`
			Country     string `json:"country"`
			City        string `json:"city"`
			Telecommute bool   `json:"telecommuting"`
			Employment  string `json:"employment_type"` // often empty
			PublishedOn string `json:"published_on"`    // "2006-01-02"
			Description string `json:"description"`     // HTML, present with details=true
		} `json:"jobs"`
	}
	url := fmt.Sprintf("https://apply.workable.com/api/v1/widget/accounts/%s?details=true", w.account)
	if err := fetchJSON(ctx, w.client, http.MethodGet, url, nil, &payload); err != nil {
		return nil, err
	}

	jobs := make([]model.Job, 0, len(payload.Jobs))
	for _, j := range payload.Jobs {
		loc := strings.TrimPrefix(strings.Join(trimEmpty(j.City, j.Country), ", "), ", ")
		if j.Telecommute {
			loc = strings.TrimSuffix("Remote "+loc, " ")
		}
		postedAt, _ := time.Parse("2006-01-02", j.PublishedOn)
		jobs = append(jobs, model.Job{
			ID:             fmt.Sprintf("workable/%s/%s", w.account, j.Shortcode),
			Company:        w.company,
			Title:          j.Title,
			Location:       loc,
			URL:            j.URL,
			EmploymentType: j.Employment,
			Description:    htmltext.ToText(j.Description),
			PostedAt:       postedAt,
		})
	}
	return jobs, nil
}

// trimEmpty drops empty strings so joins don't produce stray separators.
func trimEmpty(parts ...string) []string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
