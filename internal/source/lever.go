package source

// Lever public postings API (no auth):
//
//	GET https://api.lever.co/v0/postings/{site}?mode=json
//
// EU-hosted boards live under api.eu.lever.co; set `region: eu`.
// The description intro is in descriptionPlain, but requirement bullets
// ("2+ years of ...") usually live in lists[].content, so both are joined.
//
// Config:
//
//	- name: Kraken
//	  source: lever
//	  params:
//	    site: kraken123      # from jobs.lever.co/{site}
//	    region: eu           # optional

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
	Register("lever", func(company string, p params.Map, client *http.Client) (Source, error) {
		site, err := p.Require("site")
		if err != nil {
			return nil, err
		}
		host := "api.lever.co"
		if p.Get("region") == "eu" {
			host = "api.eu.lever.co"
		}
		return &lever{company: company, site: site, host: host, client: client}, nil
	})
}

type lever struct {
	company string
	site    string
	host    string
	client  *http.Client
}

func (l *lever) Company() string { return l.company }

func (l *lever) Fetch(ctx context.Context) ([]model.Job, error) {
	var postings []struct {
		ID         string `json:"id"`
		Text       string `json:"text"`
		HostedURL  string `json:"hostedUrl"`
		CreatedAt  int64  `json:"createdAt"` // unix millis
		Categories struct {
			Location   string `json:"location"`
			Commitment string `json:"commitment"` // e.g. "Full-Time"
		} `json:"categories"`
		DescriptionPlain string `json:"descriptionPlain"`
		AdditionalPlain  string `json:"additionalPlain"`
		Lists            []struct {
			Text    string `json:"text"`    // section heading, e.g. "Requirements"
			Content string `json:"content"` // HTML bullet list
		} `json:"lists"`
	}
	url := fmt.Sprintf("https://%s/v0/postings/%s?mode=json", l.host, l.site)
	if err := fetchJSON(ctx, l.client, http.MethodGet, url, nil, &postings); err != nil {
		return nil, err
	}

	jobs := make([]model.Job, 0, len(postings))
	for _, p := range postings {
		var desc strings.Builder
		desc.WriteString(p.DescriptionPlain)
		for _, list := range p.Lists {
			desc.WriteString("\n\n" + list.Text + "\n")
			desc.WriteString(htmltext.ToText(list.Content))
		}
		if p.AdditionalPlain != "" {
			desc.WriteString("\n\n" + p.AdditionalPlain)
		}
		jobs = append(jobs, model.Job{
			ID:             fmt.Sprintf("lever/%s/%s", l.site, p.ID),
			Company:        l.company,
			Title:          p.Text,
			Location:       p.Categories.Location,
			URL:            p.HostedURL,
			EmploymentType: p.Categories.Commitment,
			Description:    desc.String(),
			PostedAt:       time.UnixMilli(p.CreatedAt),
		})
	}
	return jobs, nil
}
