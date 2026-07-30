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
//	    max_postings: 500    # optional cap on raw list positions paged

import (
	"context"
	"errors"
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
	rawScanned := 0
	missingCount := 0
	var missingOffsets []int
	duplicateCount := 0
	var duplicateExamples []string
	var partialErrs []error
	for offset := 0; offset < w.maxPostings; {
		limit := min(workdayPageSize, w.maxPostings-offset)
		var page struct {
			Total       int       `json:"total"`
			JobPostings []posting `json:"jobPostings"`
		}
		body := fmt.Appendf(nil, `{"appliedFacets":{},"limit":%d,"offset":%d,"searchText":""}`, limit, offset)
		if err := fetchJSON(ctx, w.client, http.MethodPost, w.base+"/jobs", body, &page); err != nil {
			err = fmt.Errorf(
				"workday %s: fetch page at offset %d after scanning %d raw postings (%d usable): %w",
				w.company, offset, rawScanned, len(postings), err,
			)
			if rawScanned == 0 {
				return nil, err
			}
			partialErrs = append(partialErrs, err)
			break
		}

		// Some live Workday boards report the total only on the first page
		// and return zero on every later, non-empty page. Keep the first
		// meaningful value instead of treating those zeroes as an empty board.
		if total == 0 && page.Total > 0 {
			total = page.Total
		}
		if len(page.JobPostings) == 0 {
			if total > rawScanned {
				partialErrs = append(partialErrs, fmt.Errorf(
					"workday %s: pagination ended with an empty page at offset %d after scanning %d of %d raw postings (%d usable)",
					w.company, offset, rawScanned, total, len(postings),
				))
			}
			break
		}

		// max_postings is a cap on raw list positions, not on valid output.
		// Workday occasionally emits null placeholder rows, and some servers
		// ignore a smaller final-page limit.
		pagePostings := page.JobPostings
		if len(pagePostings) > limit {
			pagePostings = pagePostings[:limit]
		}
		rawScanned = offset + len(pagePostings)

		pagePaths := make(map[string]struct{}, len(pagePostings))
		crossPageDuplicates := 0
		for i, p := range pagePostings {
			rawOffset := offset + i
			if p.ExternalPath == "" {
				missingCount++
				if len(missingOffsets) < 5 {
					missingOffsets = append(missingOffsets, rawOffset)
				}
				continue
			}
			if _, duplicate := seenPaths[p.ExternalPath]; duplicate {
				crossPageDuplicates++
				duplicateCount++
				if len(duplicateExamples) < 5 {
					duplicateExamples = append(duplicateExamples, fmt.Sprintf("%q at raw offset %d", p.ExternalPath, rawOffset))
				}
				continue
			}
			if _, duplicate := pagePaths[p.ExternalPath]; duplicate {
				duplicateCount++
				if len(duplicateExamples) < 5 {
					duplicateExamples = append(duplicateExamples, fmt.Sprintf("%q at raw offset %d", p.ExternalPath, rawOffset))
				}
				continue
			}
			pagePaths[p.ExternalPath] = struct{}{}
			postings = append(postings, p)
		}
		for path := range pagePaths {
			seenPaths[path] = struct{}{}
		}

		// A page with only already-seen paths (and possibly placeholders)
		// usually means the upstream ignored offset. Stop rather than walking
		// the whole cap while returning no unique progress.
		if len(pagePaths) == 0 && crossPageDuplicates > 0 {
			partialErrs = append(partialErrs, fmt.Errorf(
				"workday %s: repeated page at offset %d made no unique externalPath progress",
				w.company, offset,
			))
			break
		}

		// The reported total counts raw list positions, including placeholders.
		if total > 0 && rawScanned >= total {
			break
		}
		if rawScanned >= w.maxPostings {
			break
		}

		// A short page is terminal. If a known total says more raw positions
		// remain, surface the usable jobs already collected as a partial result.
		if len(pagePostings) < limit {
			if total > rawScanned {
				partialErrs = append(partialErrs, fmt.Errorf(
					"workday %s: pagination ended with a short page (%d of %d requested) at offset %d after scanning %d of %d raw postings (%d usable)",
					w.company, len(pagePostings), limit, offset, rawScanned, total, len(postings),
				))
			}
			break
		}
		offset += limit
	}
	if rawScanned >= w.maxPostings {
		switch {
		case total > rawScanned:
			log.Printf(
				"workday %s: scanned %d of %d raw postings, listing %d usable postings (max_postings cap)",
				w.company, rawScanned, total, len(postings),
			)
		case total == 0:
			log.Printf(
				"workday %s: scanned %d raw postings, listing %d usable postings (max_postings cap; total unknown)",
				w.company, rawScanned, len(postings),
			)
		}
	}

	if missingCount > 0 {
		extra := ""
		if missingCount > len(missingOffsets) {
			extra = fmt.Sprintf("; %d more", missingCount-len(missingOffsets))
		}
		partialErrs = append(partialErrs, fmt.Errorf(
			"workday %s: skipped %d malformed postings with missing externalPath (raw offsets %v%s)",
			w.company, missingCount, missingOffsets, extra,
		))
	}
	if duplicateCount > 0 {
		extra := ""
		if duplicateCount > len(duplicateExamples) {
			extra = fmt.Sprintf("; %d more", duplicateCount-len(duplicateExamples))
		}
		partialErrs = append(partialErrs, fmt.Errorf(
			"workday %s: skipped %d duplicate externalPath postings (%s%s)",
			w.company, duplicateCount, strings.Join(duplicateExamples, ", "), extra,
		))
	}
	partialErr := errors.Join(partialErrs...)
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
