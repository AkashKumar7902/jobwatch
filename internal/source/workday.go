package source

// Workday public job-board API (no auth). Unlike the other sources this one
// is POST-based and paged (20 per page), and descriptions require one extra
// GET per posting:
//
//	POST {host}/wday/cxs/{tenant}/{site}/jobs
//	     body: {"appliedFacets":{},"limit":20,"offset":N,"searchText":""}
//	GET  {host}/wday/cxs/{tenant}/{site}{externalPath}
//
// Config (all three parts appear in the board URL,
// e.g. https://redhat.wd5.myworkdayjobs.com/jobs):
//
//	- name: RedHat
//	  source: workday
//	  params:
//	    host: redhat.wd5.myworkdayjobs.com
//	    tenant: redhat
//	    site: jobs
//	    max_postings: 200    # optional cap on detail requests

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const workdayPageSize = 20

func init() {
	Register("workday", func(company string, p params.Map, client *http.Client) (Source, error) {
		host, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		tenant, err := p.Require("tenant")
		if err != nil {
			return nil, err
		}
		site, err := p.Require("site")
		if err != nil {
			return nil, err
		}
		maxPostings, err := p.Int("max_postings", 500)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &workday{
			company: company, maxPostings: maxPostings, client: client,
			base: fmt.Sprintf("https://%s/wday/cxs/%s/%s", host, tenant, site),
		}, nil
	})
}

type workday struct {
	company     string
	base        string // https://{host}/wday/cxs/{tenant}/{site}
	maxPostings int
	client      *http.Client
}

func (w *workday) Company() string { return w.company }

func (w *workday) Fetch(ctx context.Context) ([]model.Job, error) {
	type posting struct {
		Title        string `json:"title"`
		ExternalPath string `json:"externalPath"`
	}

	// Page through the list.
	var postings []posting
	total := 0
	for offset := 0; ; offset += workdayPageSize {
		var page struct {
			Total       int       `json:"total"`
			JobPostings []posting `json:"jobPostings"`
		}
		body := fmt.Appendf(nil, `{"appliedFacets":{},"limit":%d,"offset":%d,"searchText":""}`, workdayPageSize, offset)
		if err := fetchJSON(ctx, w.client, http.MethodPost, w.base+"/jobs", body, &page); err != nil {
			return nil, err
		}
		total = page.Total
		postings = append(postings, page.JobPostings...)
		if len(page.JobPostings) == 0 || offset+workdayPageSize >= page.Total || len(postings) >= w.maxPostings {
			break
		}
	}
	if len(postings) > w.maxPostings {
		postings = postings[:w.maxPostings]
	}
	if total > len(postings) {
		log.Printf("workday %s: evaluating %d of %d postings (max_postings cap)", w.company, len(postings), total)
	}

	// Descriptions live on the detail endpoint. Workday stays eager: the
	// stored job IDs are detail-endpoint GUIDs that the list doesn't
	// carry, so listing lazily would change identities and re-alert
	// every existing posting.
	jobs := make([]model.Job, 0, len(postings))
	failed := 0
	var firstErr error
	for _, p := range postings {
		var detail struct {
			JobPostingInfo struct {
				ID             string `json:"id"`
				JobDescription string `json:"jobDescription"`
				Location       string `json:"location"`
				TimeType       string `json:"timeType"` // e.g. "Full time"
				ExternalURL    string `json:"externalUrl"`
			} `json:"jobPostingInfo"`
		}
		if err := fetchJSON(ctx, w.client, http.MethodGet, w.base+p.ExternalPath, nil, &detail); err != nil {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("posting %s: %w", p.ExternalPath, err)
			}
			continue
		}
		info := detail.JobPostingInfo

		id := info.ID
		if id == "" {
			id = p.ExternalPath
		}
		jobs = append(jobs, model.Job{
			ID:             fmt.Sprintf("workday/%s/%s", w.base, id),
			Company:        w.company,
			Title:          p.Title,
			Location:       info.Location,
			URL:            info.ExternalURL,
			EmploymentType: info.TimeType,
			Description:    htmltext.ToText(info.JobDescription),
		})
	}
	return detailResult(jobs, failed, len(postings), firstErr)
}
