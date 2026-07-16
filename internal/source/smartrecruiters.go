package source

// SmartRecruiters public postings API (no auth):
//
//	GET https://api.smartrecruiters.com/v1/companies/{company}/postings?limit=100&offset=N
//	GET https://api.smartrecruiters.com/v1/companies/{company}/postings/{id}   (description)
//
// The list endpoint has no description, so this source makes one extra
// request per posting — max_postings (default 200) caps that.
//
// Config:
//
//	- name: Percona
//	  source: smartrecruiters
//	  params:
//	    company_id: Percona
//	    max_postings: 200    # optional

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
	Register("smartrecruiters", func(company string, p params.Map, client *http.Client) (Source, error) {
		id, err := p.Require("company_id")
		if err != nil {
			return nil, err
		}
		maxPostings, err := p.Int("max_postings", 200)
		if err != nil {
			return nil, err
		}
		return &smartRecruiters{company: company, id: id, maxPostings: maxPostings, client: client}, nil
	})
}

type smartRecruiters struct {
	company     string
	id          string
	maxPostings int
	client      *http.Client
}

type srPosting struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ReleasedDate string `json:"releasedDate"`
	Location     struct {
		City    string `json:"city"`
		Country string `json:"country"`
		Remote  bool   `json:"remote"`
	} `json:"location"`
}

func (s *smartRecruiters) Company() string { return s.company }

func (s *smartRecruiters) Fetch(ctx context.Context) ([]model.Job, error) {
	// Page through the posting list.
	var postings []srPosting
	for offset := 0; ; {
		var page struct {
			TotalFound int         `json:"totalFound"`
			Content    []srPosting `json:"content"`
		}
		url := fmt.Sprintf("https://api.smartrecruiters.com/v1/companies/%s/postings?limit=100&offset=%d", s.id, offset)
		if err := fetchJSON(ctx, s.client, http.MethodGet, url, nil, &page); err != nil {
			return nil, err
		}
		postings = append(postings, page.Content...)
		offset += len(page.Content)
		if len(page.Content) == 0 || offset >= page.TotalFound || offset >= s.maxPostings {
			break
		}
	}
	if len(postings) > s.maxPostings {
		postings = postings[:s.maxPostings]
	}

	// The description lives on the detail endpoint.
	jobs := make([]model.Job, 0, len(postings))
	for _, p := range postings {
		var detail struct {
			ApplyURL         string `json:"applyUrl"`
			TypeOfEmployment struct {
				Label string `json:"label"` // e.g. "Full-time"
			} `json:"typeOfEmployment"`
			JobAd struct {
				Sections map[string]struct {
					Title string `json:"title"`
					Text  string `json:"text"`
				} `json:"sections"`
			} `json:"jobAd"`
		}
		url := fmt.Sprintf("https://api.smartrecruiters.com/v1/companies/%s/postings/%s", s.id, p.ID)
		if err := fetchJSON(ctx, s.client, http.MethodGet, url, nil, &detail); err != nil {
			return nil, fmt.Errorf("posting %s (%s): %w", p.ID, p.Name, err)
		}

		var desc strings.Builder
		// Fixed order keeps output deterministic (map iteration isn't).
		for _, key := range []string{"companyDescription", "jobDescription", "qualifications", "additionalInformation"} {
			if sec, ok := detail.JobAd.Sections[key]; ok && sec.Text != "" {
				desc.WriteString(sec.Title + "\n" + htmltext.ToText(sec.Text) + "\n\n")
			}
		}

		jobURL := detail.ApplyURL
		if jobURL == "" {
			jobURL = fmt.Sprintf("https://jobs.smartrecruiters.com/%s/%s", s.id, p.ID)
		}
		loc := p.Location.City
		if p.Location.Remote {
			loc = strings.TrimSpace("Remote " + loc)
		}
		postedAt, _ := time.Parse(time.RFC3339, p.ReleasedDate)
		jobs = append(jobs, model.Job{
			ID:             fmt.Sprintf("smartrecruiters/%s/%s", s.id, p.ID),
			Company:        s.company,
			Title:          p.Name,
			Location:       loc,
			URL:            jobURL,
			EmploymentType: detail.TypeOfEmployment.Label,
			Description:    strings.TrimSpace(desc.String()),
			PostedAt:       postedAt,
		})
	}
	return jobs, nil
}
