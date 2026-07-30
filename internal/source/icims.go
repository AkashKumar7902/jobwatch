package source

// iCIMS career portals expose their real server-rendered result list when
// requested with in_iframe=1. Full descriptions live in schema.org JSON-LD
// on each posting page.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var (
	icimsPageRe     = regexp.MustCompile(`(?i)Search\s+Results\s+Page\s+([0-9]+)\s+of\s+([0-9]+)`)
	icimsIDRe       = regexp.MustCompile(`/jobs/([0-9]+)(?:/|$)`)
	icimsLocationRe = regexp.MustCompile(`(?is)<span\b[^>]*class\s*=\s*(?:"[^"]*\bfield-label\b[^"]*"|'[^']*\bfield-label\b[^']*')[^>]*>\s*(?:Job\s+Locations?|Location(?:\s*:\s*Location)?)\s*</span>\s*<span\b[^>]*>(.*?)</span>`)
)

func init() {
	Register("icims", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawHost, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		host, err := normalizeBoardHost(rawHost)
		if err != nil {
			return nil, err
		}
		maxPages, err := positiveCappedParam(p, "max_pages", 100, 500)
		if err != nil {
			return nil, err
		}
		return &icims{
			company: company, host: host, base: "https://" + host,
			maxPages: maxPages, client: client,
		}, nil
	})
}

type icims struct {
	company  string
	host     string
	base     string
	maxPages int
	client   *http.Client
}

func (i *icims) Company() string { return i.company }

func (i *icims) Fetch(ctx context.Context) ([]model.Job, error) {
	if i.maxPages <= 0 {
		return nil, fmt.Errorf("icims %s: max_pages must be positive", i.host)
	}
	var jobs []model.Job
	seen := make(map[string]struct{})
	for pageIndex := 0; pageIndex < i.maxPages; pageIndex++ {
		query := url.Values{
			"pr":        {strconv.Itoa(pageIndex)},
			"in_iframe": {"1"},
		}
		endpoint := i.base + "/jobs/search?" + query.Encode()
		body, err := fetchHTMLPage(ctx, i.client, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("icims %s page %d: %w", i.host, pageIndex+1, err)
		}
		doc := string(body)
		pageMatch := icimsPageRe.FindStringSubmatch(doc)
		if pageMatch == nil {
			if pageIndex == 0 && len(htmlElementsByClass(doc, "iCIMS_JobCardItem")) == 0 &&
				(strings.Contains(strings.ToLower(doc), "no jobs") || strings.Contains(strings.ToLower(doc), "0 results")) {
				return []model.Job{}, nil
			}
			return nil, fmt.Errorf("icims %s page %d: missing pagination heading", i.host, pageIndex+1)
		}
		pageNumber, _ := strconv.Atoi(pageMatch[1])
		pageCount, _ := strconv.Atoi(pageMatch[2])
		if pageNumber != pageIndex+1 || pageCount < pageNumber {
			return nil, fmt.Errorf("icims %s: inconsistent pagination page=%d/%d for pr=%d", i.host, pageNumber, pageCount, pageIndex)
		}
		cards := htmlElementsByClass(doc, "iCIMS_JobCardItem")
		if len(cards) == 0 {
			lowerText := strings.ToLower(cleanHTMLFragment(doc))
			if pageIndex == 0 && pageCount == 1 &&
				(strings.Contains(lowerText, "no jobs") || strings.Contains(lowerText, "no results")) {
				return []model.Job{}, nil
			}
			return nil, fmt.Errorf("icims %s page %d: pagination reports jobs but no job cards were found", i.host, pageNumber)
		}
		for cardIndex, card := range cards {
			var jobAnchor *htmlElement
			for _, anchor := range htmlAnchors(card.inner) {
				if hasClass(anchor.attrs, "iCIMS_Anchor") {
					copy := anchor
					jobAnchor = &copy
					break
				}
			}
			if jobAnchor == nil {
				return nil, fmt.Errorf("icims %s page %d card %d: missing job link", i.host, pageNumber, cardIndex)
			}
			jobURL, err := resolveBoardURL(i.base, jobAnchor.attrs["href"])
			if err != nil {
				return nil, fmt.Errorf("icims %s page %d card %d: %w", i.host, pageNumber, cardIndex, err)
			}
			parsedURL, _ := url.Parse(jobURL)
			idMatch := icimsIDRe.FindStringSubmatch(parsedURL.Path)
			if idMatch == nil {
				return nil, fmt.Errorf("icims %s page %d card %d: job URL has no numeric ID", i.host, pageNumber, cardIndex)
			}
			id := idMatch[1]
			parsedURL.RawQuery = ""
			parsedURL.Fragment = ""
			jobURL = parsedURL.String()
			title := ""
			if heading, ok := firstHTMLClass(jobAnchor.inner, "iCIMS_Header"); ok {
				title = cleanHTMLFragment(heading.inner)
			}
			if title == "" {
				headingRe := regexp.MustCompile(`(?is)<h[1-6]\b[^>]*>(.*?)</h[1-6]>`)
				if heading := headingRe.FindStringSubmatch(jobAnchor.inner); heading != nil {
					title = cleanHTMLFragment(heading[1])
				}
			}
			if title == "" {
				title = compactSpaces(jobAnchor.attrs["title"])
				title = strings.TrimSpace(strings.TrimPrefix(title, id+" - "))
			}
			if title == "" {
				return nil, fmt.Errorf("icims %s page %d card %d: missing title", i.host, pageNumber, cardIndex)
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("icims %s page %d: duplicate job ID %q", i.host, pageNumber, id)
			}
			seen[id] = struct{}{}
			location := ""
			if match := icimsLocationRe.FindStringSubmatch(card.inner); match != nil {
				location = cleanHTMLFragment(match[1])
			}
			jobs = append(jobs, model.Job{
				ID:       fmt.Sprintf("icims/%s/%s", i.host, id),
				Company:  i.company,
				Title:    title,
				Location: location,
				URL:      jobURL,
			})
		}
		if pageNumber == pageCount {
			return jobs, nil
		}
	}
	return nil, fmt.Errorf("icims %s: pagination exceeded max_pages=%d", i.host, i.maxPages)
}

func (i *icims) Detail(ctx context.Context, job *model.Job) error {
	prefix := "icims/" + i.host + "/"
	if job == nil || !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("icims %s: job ID does not belong to this board", i.host)
	}
	id := strings.TrimPrefix(job.ID, prefix)
	if id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("icims %s: invalid job ID %q", i.host, job.ID)
	}
	detailURL, err := resolveBoardURL(i.base, job.URL)
	if err != nil {
		return fmt.Errorf("icims %s job %s: %w", i.host, id, err)
	}
	parsedURL, _ := url.Parse(detailURL)
	if match := icimsIDRe.FindStringSubmatch(parsedURL.Path); match == nil || match[1] != id {
		return fmt.Errorf("icims %s job %s: detail URL does not contain the job ID", i.host, id)
	}
	query := parsedURL.Query()
	query.Set("in_iframe", "1")
	parsedURL.RawQuery = query.Encode()
	body, err := fetchHTMLPage(ctx, i.client, parsedURL.String(), nil)
	if err != nil {
		return fmt.Errorf("icims %s job %s detail: %w", i.host, id, err)
	}
	posting, err := extractStructuredJobPosting(string(body))
	if err != nil {
		return fmt.Errorf("icims %s job %s detail: %w", i.host, id, err)
	}
	description := cleanHTMLFragment(posting.Description)
	if description == "" {
		return fmt.Errorf("icims %s job %s detail: missing description", i.host, id)
	}
	postedAt, err := parsePostingDate(posting.DatePosted)
	if err != nil {
		return fmt.Errorf("icims %s job %s detail: %w", i.host, id, err)
	}
	updated := *job
	if title := compactSpaces(posting.Title); title != "" {
		updated.Title = title
	}
	updated.Description = description
	if location := compactSpaces(posting.Location); location != "" {
		updated.Location = location
	}
	if employmentType := compactSpaces(posting.EmploymentType); employmentType != "" {
		updated.EmploymentType = employmentType
	}
	if !postedAt.IsZero() {
		updated.PostedAt = postedAt
	}
	*job = updated
	return nil
}
