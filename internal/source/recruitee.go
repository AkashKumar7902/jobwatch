package source

// Recruitee public offers API (no auth):
//
//	GET https://{company}.recruitee.com/api/offers/
//
// Config:
//
//	- name: MarsBased
//	  source: recruitee
//	  params:
//	    company_slug: marsbased    # from {company_slug}.recruitee.com

import (
	"context"
	"fmt"
	"net/http"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("recruitee", func(company string, p params.Map, client *http.Client) (Source, error) {
		slug, err := p.Require("company_slug")
		if err != nil {
			return nil, err
		}
		return &recruitee{company: company, slug: slug, client: client}, nil
	})
}

type recruitee struct {
	company string
	slug    string
	client  *http.Client
}

func (r *recruitee) Company() string { return r.company }

func (r *recruitee) Fetch(ctx context.Context) ([]model.Job, error) {
	var payload struct {
		Offers []struct {
			ID           int64  `json:"id"`
			Title        string `json:"title"`
			CareersURL   string `json:"careers_url"`
			Location     string `json:"location"`
			Description  string `json:"description"`  // HTML
			Requirements string `json:"requirements"` // HTML
		} `json:"offers"`
	}
	url := fmt.Sprintf("https://%s.recruitee.com/api/offers/", r.slug)
	if err := fetchJSON(ctx, r.client, http.MethodGet, url, nil, &payload); err != nil {
		return nil, err
	}

	jobs := make([]model.Job, 0, len(payload.Offers))
	for _, o := range payload.Offers {
		desc := htmltext.ToText(o.Description)
		if o.Requirements != "" {
			desc += "\n\nRequirements\n" + htmltext.ToText(o.Requirements)
		}
		jobs = append(jobs, model.Job{
			ID:          fmt.Sprintf("recruitee/%s/%d", r.slug, o.ID),
			Company:     r.company,
			Title:       o.Title,
			Location:    o.Location,
			URL:         o.CareersURL,
			Description: desc,
		})
	}
	return jobs, nil
}
