package source

// BambooHR public careers API (no auth):
//
//	GET https://{company}.bamboohr.com/careers/list
//	GET https://{company}.bamboohr.com/careers/{id}/detail   (description)
//
// The list has no descriptions, so one extra request is made per posting —
// fine for the small boards BambooHR typically hosts.
//
// Config:
//
//	- name: Lullabot
//	  source: bamboohr
//	  params:
//	    company_slug: lullabot    # from {company_slug}.bamboohr.com

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("bamboohr", func(company string, p params.Map, client *http.Client) (Source, error) {
		slug, err := p.Require("company_slug")
		if err != nil {
			return nil, err
		}
		return &bambooHR{company: company, slug: slug, client: client}, nil
	})
}

type bambooHR struct {
	company string
	slug    string
	client  *http.Client
}

func (b *bambooHR) Company() string { return b.company }

func (b *bambooHR) Fetch(ctx context.Context) ([]model.Job, error) {
	var list struct {
		Result []struct {
			ID             string `json:"id"`
			JobOpeningName string `json:"jobOpeningName"`
			Employment     string `json:"employmentStatusLabel"` // e.g. "Full Time"
			ATSLocation    struct {
				City    string `json:"city"`
				State   string `json:"state"`
				Country string `json:"country"`
			} `json:"atsLocation"`
			IsRemote *bool `json:"isRemote"`
		} `json:"result"`
	}
	listURL := fmt.Sprintf("https://%s.bamboohr.com/careers/list", b.slug)
	if err := fetchJSON(ctx, b.client, http.MethodGet, listURL, nil, &list); err != nil {
		return nil, err
	}

	jobs := make([]model.Job, 0, len(list.Result))
	for _, item := range list.Result {
		var detail struct {
			Result struct {
				JobOpening struct {
					Description string `json:"description"`
					ShareURL    string `json:"jobOpeningShareUrl"`
				} `json:"jobOpening"`
			} `json:"result"`
		}
		detailURL := fmt.Sprintf("https://%s.bamboohr.com/careers/%s/detail", b.slug, item.ID)
		if err := fetchJSON(ctx, b.client, http.MethodGet, detailURL, nil, &detail); err != nil {
			return nil, fmt.Errorf("posting %s (%s): %w", item.ID, item.JobOpeningName, err)
		}

		loc := strings.Join(trimEmpty(item.ATSLocation.City, item.ATSLocation.State, item.ATSLocation.Country), ", ")
		if item.IsRemote != nil && *item.IsRemote {
			loc = strings.TrimSpace("Remote " + loc)
		}
		url := detail.Result.JobOpening.ShareURL
		if url == "" {
			url = fmt.Sprintf("https://%s.bamboohr.com/careers/%s", b.slug, item.ID)
		}
		jobs = append(jobs, model.Job{
			ID:             fmt.Sprintf("bamboohr/%s/%s", b.slug, item.ID),
			Company:        b.company,
			Title:          item.JobOpeningName,
			Location:       loc,
			URL:            url,
			EmploymentType: item.Employment,
			Description:    htmltext.ToText(detail.Result.JobOpening.Description),
		})
	}
	return jobs, nil
}
