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
// This source implements Detailer: Fetch lists the whole board cheaply and
// the runner requests details only for postings it actually evaluates.
//
//	- name: RedHat
//	  source: workday
//	  params:
//	    host: redhat.wd5.myworkdayjobs.com
//	    tenant: redhat
//	    site: jobs
//	    max_postings: 500    # optional cap on list paging

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

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
		Title         string `json:"title"`
		ExternalPath  string `json:"externalPath"`
		LocationsText string `json:"locationsText"`
	}

	// Page through the list.
	var postings []posting
	seenPaths := make(map[string]struct{})
	total := 0
	var partialErr error
	for offset := 0; len(postings) < w.maxPostings; {
		limit := min(workdayPageSize, w.maxPostings-len(postings))
		var page struct {
			Total       int       `json:"total"`
			JobPostings []posting `json:"jobPostings"`
		}
		body := fmt.Appendf(nil, `{"appliedFacets":{},"limit":%d,"offset":%d,"searchText":""}`, limit, offset)
		if err := fetchJSON(ctx, w.client, http.MethodPost, w.base+"/jobs", body, &page); err != nil {
			err = fmt.Errorf("workday %s: fetch page at offset %d after %d postings: %w", w.company, offset, len(postings), err)
			if len(postings) == 0 {
				return nil, err
			}
			partialErr = err
			break
		}

		// Some live Workday boards report the total only on the first page
		// and return zero on every later, non-empty page. Keep the first
		// meaningful value instead of treating those zeroes as an empty board.
		if total == 0 && page.Total > 0 {
			total = page.Total
		}
		if len(page.JobPostings) == 0 {
			if total > len(postings) {
				partialErr = fmt.Errorf(
					"workday %s: pagination ended with an empty page at offset %d after %d of %d postings",
					w.company, offset, len(postings), total,
				)
			}
			break
		}

		// Do not let a server that ignores the requested limit make Fetch
		// exceed max_postings.
		pagePostings := page.JobPostings
		if len(pagePostings) > limit {
			pagePostings = pagePostings[:limit]
		}

		pagePaths := make(map[string]struct{}, len(pagePostings))
		newPaths := 0
		var duplicatePath string
		for i, p := range pagePostings {
			if p.ExternalPath == "" {
				partialErr = fmt.Errorf(
					"workday %s: posting at offset %d is missing externalPath",
					w.company, offset+i,
				)
				break
			}
			if _, duplicate := pagePaths[p.ExternalPath]; duplicate {
				partialErr = fmt.Errorf(
					"workday %s: duplicate externalPath %q on page at offset %d",
					w.company, p.ExternalPath, offset,
				)
				break
			}
			pagePaths[p.ExternalPath] = struct{}{}
			if _, duplicate := seenPaths[p.ExternalPath]; duplicate {
				duplicatePath = p.ExternalPath
			} else {
				newPaths++
			}
		}
		if partialErr != nil {
			break
		}
		if newPaths == 0 {
			partialErr = fmt.Errorf(
				"workday %s: repeated page at offset %d made no unique externalPath progress",
				w.company, offset,
			)
			break
		}
		if duplicatePath != "" {
			partialErr = fmt.Errorf(
				"workday %s: duplicate externalPath %q across pages at offset %d",
				w.company, duplicatePath, offset,
			)
			break
		}
		for _, p := range pagePostings {
			seenPaths[p.ExternalPath] = struct{}{}
		}
		postings = append(postings, pagePostings...)

		if len(postings) >= w.maxPostings || (total > 0 && len(postings) >= total) {
			break
		}
		// A short page is terminal. If a known total says more jobs remain,
		// surface the jobs already collected as a partial result.
		if len(page.JobPostings) < limit {
			if total > len(postings) {
				partialErr = fmt.Errorf(
					"workday %s: pagination ended with a short page (%d of %d requested) at offset %d after %d of %d postings",
					w.company, len(page.JobPostings), limit, offset, len(postings), total,
				)
			}
			break
		}
		offset += limit
	}
	if len(postings) == w.maxPostings {
		switch {
		case total > len(postings):
			log.Printf("workday %s: listing %d of %d postings (max_postings cap)", w.company, len(postings), total)
		case total == 0:
			log.Printf("workday %s: listing %d postings (max_postings cap; total unknown)", w.company, len(postings))
		}
	}

	if partialErr != nil && len(postings) == 0 {
		return nil, partialErr
	}
	jobs := make([]model.Job, 0, len(postings))
	for _, p := range postings {
		jobs = append(jobs, model.Job{
			// externalPath (starts with "/job/...") carries the stable req
			// ID, so it works as the identity without a detail request.
			ID:       "workday/" + w.base + p.ExternalPath,
			Company:  w.company,
			Title:    p.Title,
			Location: p.LocationsText,
			URL:      w.base + p.ExternalPath,
			// Description and EmploymentType arrive via Detail on demand.
		})
	}
	return jobs, partialErr
}

// Detail fills the description, employment type, and canonical URL for one
// posting. Fetching details lazily matters enormously at fleet scale:
// eager mode meant ~500 GETs per board per run, which Workday's WAF
// answered with 429s and HTML challenges across ~50 boards.
func (w *workday) Detail(ctx context.Context, job *model.Job) error {
	externalPath := strings.TrimPrefix(job.ID, "workday/"+w.base)
	var detail struct {
		JobPostingInfo struct {
			JobDescription string `json:"jobDescription"`
			Location       string `json:"location"`
			TimeType       string `json:"timeType"` // e.g. "Full time"
			ExternalURL    string `json:"externalUrl"`
		} `json:"jobPostingInfo"`
	}
	if err := fetchJSON(ctx, w.client, http.MethodGet, w.base+externalPath, nil, &detail); err != nil {
		return err
	}
	info := detail.JobPostingInfo
	job.Description = htmltext.ToText(info.JobDescription)
	job.EmploymentType = info.TimeType
	if info.Location != "" {
		job.Location = info.Location
	}
	if info.ExternalURL != "" {
		job.URL = info.ExternalURL
	}
	return nil
}
