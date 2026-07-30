package source

// SAP SuccessFactors career sites render their paged result set server-side.
// Detail pages expose JobPosting metadata as JSON-LD on some versions and as
// schema.org microdata on others.

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
	successFactorsPageRe = regexp.MustCompile(`(?i)Page\s+([0-9]+)\s+of\s+([0-9]+)\s*,\s*Results\s+([0-9]+)\s+to\s+([0-9]+)\s+of\s+([0-9]+)`)
	successFactorsIDRe   = regexp.MustCompile(`/([0-9]+)/?$`)
)

func init() {
	Register("successfactors", func(company string, p params.Map, client *http.Client) (Source, error) {
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
		return &successFactors{
			company: company, host: host, base: "https://" + host,
			maxPages: maxPages, client: client,
		}, nil
	})
}

type successFactors struct {
	company  string
	host     string
	base     string
	maxPages int
	client   *http.Client
}

func (s *successFactors) Company() string { return s.company }

func (s *successFactors) Fetch(ctx context.Context) ([]model.Job, error) {
	if s.maxPages <= 0 {
		return nil, fmt.Errorf("successfactors %s: max_pages must be positive", s.host)
	}
	var jobs []model.Job
	seen := make(map[string]struct{})
	startRow := 0
	expectedPage := 1
	for pageAttempt := 1; pageAttempt <= s.maxPages; pageAttempt++ {
		query := url.Values{
			"q":             {""},
			"sortColumn":    {"referencedate"},
			"sortDirection": {"desc"},
			"startrow":      {strconv.Itoa(startRow)},
		}
		endpoint := s.base + "/search/?" + query.Encode()
		body, err := fetchHTMLPage(ctx, s.client, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("successfactors %s page %d: %w", s.host, expectedPage, err)
		}
		doc := string(body)
		pageMatch := successFactorsPageRe.FindStringSubmatch(doc)
		if pageMatch == nil {
			if startRow == 0 && len(htmlElementsByClass(doc, "data-row")) == 0 &&
				(strings.Contains(strings.ToLower(doc), "no jobs") || strings.Contains(strings.ToLower(doc), "0 results")) {
				return []model.Job{}, nil
			}
			return nil, fmt.Errorf("successfactors %s page %d: missing pagination metadata", s.host, expectedPage)
		}
		pageNumber, _ := strconv.Atoi(pageMatch[1])
		pageCount, _ := strconv.Atoi(pageMatch[2])
		firstResult, _ := strconv.Atoi(pageMatch[3])
		lastResult, _ := strconv.Atoi(pageMatch[4])
		totalResults, _ := strconv.Atoi(pageMatch[5])
		if totalResults == 0 {
			if startRow != 0 || len(htmlElementsByClass(doc, "data-row")) != 0 {
				return nil, fmt.Errorf("successfactors %s: zero-result pagination contains job rows", s.host)
			}
			return []model.Job{}, nil
		}
		if pageNumber != expectedPage || pageCount < pageNumber || firstResult != startRow+1 ||
			lastResult < firstResult || lastResult > totalResults {
			return nil, fmt.Errorf(
				"successfactors %s: inconsistent pagination page=%d/%d results=%d-%d/%d for startrow=%d",
				s.host, pageNumber, pageCount, firstResult, lastResult, totalResults, startRow,
			)
		}
		rows := htmlElementsByClass(doc, "data-row")
		if len(rows) == 0 {
			return nil, fmt.Errorf("successfactors %s page %d: pagination reports jobs but no data rows were found", s.host, pageNumber)
		}
		if len(rows) > lastResult-firstResult+1 {
			return nil, fmt.Errorf("successfactors %s page %d: found %d rows for result range %d-%d", s.host, pageNumber, len(rows), firstResult, lastResult)
		}
		for rowIndex, row := range rows {
			var jobAnchor *htmlElement
			for _, anchor := range htmlAnchors(row.inner) {
				if hasClass(anchor.attrs, "jobTitle-link") {
					copy := anchor
					jobAnchor = &copy
					break
				}
			}
			if jobAnchor == nil {
				return nil, fmt.Errorf("successfactors %s page %d row %d: missing job link", s.host, pageNumber, rowIndex)
			}
			jobURL, err := resolveBoardURL(s.base, jobAnchor.attrs["href"])
			if err != nil {
				return nil, fmt.Errorf("successfactors %s page %d row %d: %w", s.host, pageNumber, rowIndex, err)
			}
			parsedURL, _ := url.Parse(jobURL)
			parsedURL.RawQuery = ""
			jobURL = parsedURL.String()
			idMatch := successFactorsIDRe.FindStringSubmatch(parsedURL.Path)
			title := cleanHTMLFragment(jobAnchor.inner)
			if idMatch == nil || title == "" {
				return nil, fmt.Errorf("successfactors %s page %d row %d: missing numeric job ID or title", s.host, pageNumber, rowIndex)
			}
			id := idMatch[1]
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("successfactors %s page %d: duplicate job ID %q", s.host, pageNumber, id)
			}
			seen[id] = struct{}{}
			location := ""
			if element, ok := firstHTMLClass(row.inner, "jobLocation"); ok {
				location = cleanHTMLFragment(element.inner)
			}
			var postedAtRaw string
			if element, ok := firstHTMLClass(row.inner, "jobDate"); ok {
				postedAtRaw = cleanHTMLFragment(element.inner)
			}
			postedAt, err := parsePostingDate(postedAtRaw)
			if err != nil {
				return nil, fmt.Errorf("successfactors %s job %s: %w", s.host, id, err)
			}
			jobs = append(jobs, model.Job{
				ID:       fmt.Sprintf("successfactors/%s/%s", s.host, id),
				Company:  s.company,
				Title:    title,
				Location: location,
				URL:      jobURL,
				PostedAt: postedAt,
			})
		}
		if lastResult == totalResults {
			if len(jobs) != totalResults {
				return nil, fmt.Errorf("successfactors %s: parsed %d unique jobs, pagination reported %d", s.host, len(jobs), totalResults)
			}
			return jobs, nil
		}
		if pageNumber == pageCount {
			return nil, fmt.Errorf("successfactors %s page %d: last page ended at %d of %d", s.host, pageNumber, lastResult, totalResults)
		}
		startRow = lastResult
		expectedPage++
	}
	return nil, fmt.Errorf("successfactors %s: pagination exceeded max_pages=%d", s.host, s.maxPages)
}

func (s *successFactors) Detail(ctx context.Context, job *model.Job) error {
	prefix := "successfactors/" + s.host + "/"
	if job == nil || !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("successfactors %s: job ID does not belong to this board", s.host)
	}
	id := strings.TrimPrefix(job.ID, prefix)
	if id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("successfactors %s: invalid job ID %q", s.host, job.ID)
	}
	detailURL, err := resolveBoardURL(s.base, job.URL)
	if err != nil {
		return fmt.Errorf("successfactors %s job %s: %w", s.host, id, err)
	}
	parsedURL, _ := url.Parse(detailURL)
	if match := successFactorsIDRe.FindStringSubmatch(parsedURL.Path); match == nil || match[1] != id {
		return fmt.Errorf("successfactors %s job %s: detail URL does not contain the job ID", s.host, id)
	}
	body, err := fetchHTMLPage(ctx, s.client, detailURL, nil)
	if err != nil {
		return fmt.Errorf("successfactors %s job %s detail: %w", s.host, id, err)
	}
	doc := string(body)
	posting, structuredErr := extractStructuredJobPosting(doc)
	if structuredErr != nil || strings.TrimSpace(posting.Description) == "" {
		posting = structuredJobPosting{
			Title:          microdataValue(doc, "title"),
			EmploymentType: microdataValue(doc, "employmentType"),
			DatePosted:     microdataValue(doc, "datePosted"),
		}
		if description, ok := firstHTMLClass(doc, "jobdescription"); ok {
			posting.Description = description.inner
		}
		var locationParts []string
		for _, property := range []string{"streetAddress", "addressLocality", "addressRegion", "addressCountry"} {
			value := compactSpaces(microdataValue(doc, property))
			if value != "" && !containsText(locationParts, value) {
				locationParts = append(locationParts, value)
			}
		}
		posting.Location = strings.Join(locationParts, ", ")
	}
	description := cleanHTMLFragment(posting.Description)
	if description == "" {
		return fmt.Errorf("successfactors %s job %s detail: missing description", s.host, id)
	}
	postedAt, err := parsePostingDate(posting.DatePosted)
	if err != nil {
		return fmt.Errorf("successfactors %s job %s detail: %w", s.host, id, err)
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

func containsText(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted || strings.Contains(value, wanted) {
			return true
		}
	}
	return false
}
