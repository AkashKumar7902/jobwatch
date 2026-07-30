package source

// Nykaa's Skima-backed careers site renders both its paginated list and job
// details into first-party HTML. The list page contains ten jobs per page;
// details are fetched lazily so normal polls do not request every posting.
//
//	GET https://careers.nykaa.com/?page=N
//	GET https://careers.nykaa.com/{uuid}

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	nykaaPageSize = 10
	nykaaMaxPages = 100
)

var (
	nykaaShowingRE = regexp.MustCompile(`(?is)Showing\s*(?:<span[^>]*>\s*)?([0-9]+)(?:\s*</span>)?\s*of\s*(?:<span[^>]*>\s*)?([0-9]+)(?:\s*</span>)?\s*-\s*Jobs`)
	nykaaPageRE    = regexp.MustCompile(`data-last-page=["']([0-9]+)["'][^>]*data-current-page=["']([0-9]+)["']`)
	nykaaItemRE    = regexp.MustCompile(`(?is)<div class=["']flex flex-col space-y-3[^"']*["'][^>]*>.*?<a href=["']/([0-9a-f-]{36})["'] class=["']text-lg[^"']*["'][^>]*>(.*?)</a>.*?<div class=["']flex flex-wrap[^"']*["'][^>]*>(.*?)</div>\s*</div>\s*<div class=["']flex justify-end["']>`)
	nykaaMetaRE    = regexp.MustCompile(`(?is)<span class=["']break-all text-sm["'][^>]*>(.*?)</span>`)
	nykaaTitleRE   = regexp.MustCompile(`(?is)<h1 class=["']text-2xl[^"']*["'][^>]*>(.*?)</h1>`)
	nykaaPostedRE  = regexp.MustCompile(`(?is)<span>\s*Posted on\s*</span>\s*<span>\s*([^<]+?)\s*</span>`)
	nykaaDetailRE  = regexp.MustCompile(`(?is)<div class=["']job-description-panel[^"']*["'][^>]*>(.*?)</div>\s*</div>\s*</div>\s*<div class=["']col-span-12 mt-6`)
)

func init() {
	Register("nykaa", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &nykaa{company: company, base: "https://careers.nykaa.com", client: client}, nil
	})
}

type nykaa struct {
	company string
	base    string
	client  *http.Client
}

func (s *nykaa) Company() string { return s.company }

func (s *nykaa) Fetch(ctx context.Context) ([]model.Job, error) {
	var (
		jobs          []model.Job
		seen          = make(map[string]struct{})
		expectedTotal = -1
		expectedPages = -1
	)
	for pageNumber := 1; ; pageNumber++ {
		if pageNumber > nykaaMaxPages {
			return nil, fmt.Errorf("nykaa: pagination exceeded safety limit %d", nykaaMaxPages)
		}
		endpoint := s.base + "/"
		if pageNumber > 1 {
			endpoint += "?page=" + strconv.Itoa(pageNumber)
		}
		body, err := fetchCustomBoardHTML(ctx, s.client, endpoint)
		if err != nil {
			return nil, err
		}
		if strings.Contains(strings.ToLower(string(body)), "unable to fetch job listings") {
			return nil, fmt.Errorf("nykaa: page %d returned an upstream error shell", pageNumber)
		}
		showingMatch := nykaaShowingRE.FindSubmatch(body)
		pageMatch := nykaaPageRE.FindSubmatch(body)
		if showingMatch == nil || pageMatch == nil {
			return nil, fmt.Errorf("nykaa: page %d omitted result or pagination metadata", pageNumber)
		}
		shown, _ := strconv.Atoi(string(showingMatch[1]))
		total, _ := strconv.Atoi(string(showingMatch[2]))
		lastPage, _ := strconv.Atoi(string(pageMatch[1]))
		currentPage, _ := strconv.Atoi(string(pageMatch[2]))
		if currentPage != pageNumber || total < 0 || lastPage < 1 {
			return nil, fmt.Errorf("nykaa: page %d returned inconsistent pagination metadata", pageNumber)
		}
		calculatedPages := 1
		if total > 0 {
			calculatedPages = (total + nykaaPageSize - 1) / nykaaPageSize
		}
		if lastPage != calculatedPages || lastPage > nykaaMaxPages {
			return nil, fmt.Errorf("nykaa: total %d is inconsistent with last page %d", total, lastPage)
		}
		if pageNumber == 1 {
			expectedTotal, expectedPages = total, lastPage
		} else if total != expectedTotal || lastPage != expectedPages {
			return nil, fmt.Errorf("nykaa: pagination totals changed on page %d", pageNumber)
		}

		itemMatches := nykaaItemRE.FindAllSubmatch(body, -1)
		if shown != len(itemMatches) {
			return nil, fmt.Errorf("nykaa: page %d says it shows %d jobs but contains %d", pageNumber, shown, len(itemMatches))
		}
		wantItems := nykaaPageSize
		if remaining := expectedTotal - (pageNumber-1)*nykaaPageSize; remaining < wantItems {
			wantItems = remaining
		}
		if len(itemMatches) != wantItems {
			return nil, fmt.Errorf("nykaa: page %d contains %d jobs, want %d", pageNumber, len(itemMatches), wantItems)
		}
		for i, match := range itemMatches {
			postingID := strings.ToLower(string(match[1]))
			if !customBoardUUIDRE.MatchString(postingID) {
				return nil, fmt.Errorf("nykaa: page %d item %d has invalid id %q", pageNumber, i, postingID)
			}
			if _, duplicate := seen[postingID]; duplicate {
				return nil, fmt.Errorf("nykaa: duplicate posting id %q", postingID)
			}
			seen[postingID] = struct{}{}
			title := cleanText(string(match[2]))
			if title == "" {
				return nil, fmt.Errorf("nykaa: page %d item %d has an empty title", pageNumber, i)
			}
			meta := nykaaMetaRE.FindAllSubmatch(match[3], -1)
			if len(meta) < 3 {
				return nil, fmt.Errorf("nykaa: page %d item %d omitted location or employment type", pageNumber, i)
			}
			jobs = append(jobs, model.Job{
				ID:             "nykaa/" + postingID,
				Company:        s.company,
				Title:          title,
				Location:       cleanText(string(meta[0][1])),
				URL:            s.base + "/" + postingID,
				EmploymentType: cleanText(string(meta[2][1])),
			})
		}
		if pageNumber >= expectedPages {
			break
		}
	}
	if len(jobs) != expectedTotal {
		return nil, fmt.Errorf("nykaa: collected %d jobs, want %d", len(jobs), expectedTotal)
	}
	return jobs, nil
}

func (s *nykaa) Detail(ctx context.Context, job *model.Job) error {
	const prefix = "nykaa/"
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("nykaa: job id %q does not have prefix %q", job.ID, prefix)
	}
	postingID := strings.ToLower(strings.TrimPrefix(job.ID, prefix))
	if !customBoardUUIDRE.MatchString(postingID) {
		return fmt.Errorf("nykaa: job id %q has an invalid posting id", job.ID)
	}
	endpoint := s.base + "/" + postingID
	body, err := fetchCustomBoardHTML(ctx, s.client, endpoint)
	if err != nil {
		return err
	}
	titleMatch := nykaaTitleRE.FindSubmatch(body)
	descriptionMatch := nykaaDetailRE.FindSubmatch(body)
	if titleMatch == nil || descriptionMatch == nil {
		return fmt.Errorf("nykaa: detail %q omitted title or job description", postingID)
	}
	title := cleanText(string(titleMatch[1]))
	description := htmltext.ToText(string(descriptionMatch[1]))
	if title == "" || description == "" {
		return fmt.Errorf("nykaa: detail %q has an empty title or job description", postingID)
	}
	var postedAt time.Time
	if postedMatch := nykaaPostedRE.FindSubmatch(body); postedMatch != nil {
		postedAt, err = time.Parse("02 January 2006", strings.TrimSpace(string(postedMatch[1])))
		if err != nil {
			return fmt.Errorf("nykaa: detail %q has invalid posted date: %w", postingID, err)
		}
	}
	job.Title = title
	job.Description = description
	job.PostedAt = postedAt
	job.URL = endpoint
	return nil
}
