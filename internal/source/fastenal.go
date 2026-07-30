package source

// Fastenal's public careers search is a server-side DataTable protected by an
// anonymous session cookie and CSRF token. Fetch first opens /jobs, then pages
// through /load-jobs. Full descriptions live on public detail pages and are
// loaded lazily.
//
//	GET  https://jobs.fastenal.com/jobs
//	POST https://jobs.fastenal.com/load-jobs
//	GET  https://jobs.fastenal.com/details/{jobId}

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	fastenalPageSize = 25
	fastenalMaxJobs  = 20000
)

var (
	fastenalCSRFHeaderRE  = regexp.MustCompile(`(?i)<meta\s+[^>]*name=["']_csrf_header["'][^>]*content=["']([^"']+)["']`)
	fastenalCSRFTokenRE   = regexp.MustCompile(`(?i)<meta\s+[^>]*name=["']_csrf["'][^>]*content=["']([^"']+)["']`)
	fastenalHeaderNameRE  = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	fastenalDetailIDRE    = regexp.MustCompile(`(?is)<td>\s*Job ID\s*</td>\s*<td>\s*([0-9]+)\s*</td>`)
	fastenalDescriptionRE = regexp.MustCompile(`(?is)<div\s+class=["'][^"']*\bcms-job-description\b[^"']*["'][^>]*>(.*?)</div>`)
)

func init() {
	Register("fastenal", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &fastenal{company: company, base: "https://jobs.fastenal.com", client: client}, nil
	})
}

type fastenal struct {
	company string
	base    string
	client  *http.Client
}

type fastenalPosting struct {
	JobID        int64  `json:"jobId"`
	Title        string `json:"title"`
	Type         string `json:"type"`
	City         string `json:"city"`
	State        string `json:"state"`
	Department   string `json:"department"`
	ApprovedDate int64  `json:"approvedDate"`
}

type fastenalPage struct {
	Data            *[]fastenalPosting `json:"data"`
	Draw            *int               `json:"draw"`
	RecordsFiltered *int               `json:"recordsFiltered"`
	RecordsTotal    *int               `json:"recordsTotal"`
}

func (s *fastenal) Company() string { return s.company }

func (s *fastenal) Fetch(ctx context.Context) ([]model.Job, error) {
	csrfHeader, csrfToken, cookies, err := s.openSession(ctx)
	if err != nil {
		return nil, err
	}

	var (
		jobs          []model.Job
		seen          = make(map[int64]struct{})
		expectedTotal = -1
	)
	for draw, start := 1, 0; ; draw, start = draw+1, start+fastenalPageSize {
		page, err := s.fetchPage(ctx, draw, start, csrfHeader, csrfToken, cookies)
		if err != nil {
			return nil, err
		}
		if page.Data == nil || page.Draw == nil || page.RecordsTotal == nil || page.RecordsFiltered == nil {
			return nil, fmt.Errorf("fastenal: page at offset %d omitted required DataTables metadata", start)
		}
		if *page.Draw != draw {
			return nil, fmt.Errorf("fastenal: page at offset %d echoed draw %d, want %d", start, *page.Draw, draw)
		}
		if *page.RecordsTotal < 0 || *page.RecordsFiltered != *page.RecordsTotal {
			return nil, fmt.Errorf("fastenal: page at offset %d returned inconsistent record totals", start)
		}
		if expectedTotal < 0 {
			expectedTotal = *page.RecordsTotal
			if expectedTotal > fastenalMaxJobs {
				return nil, fmt.Errorf("fastenal: recordsTotal %d exceeds safety limit %d", expectedTotal, fastenalMaxJobs)
			}
		} else if *page.RecordsTotal != expectedTotal {
			return nil, fmt.Errorf("fastenal: recordsTotal changed at offset %d", start)
		}

		wantItems := fastenalPageSize
		if remaining := expectedTotal - start; remaining < wantItems {
			wantItems = remaining
		}
		if wantItems < 0 || len(*page.Data) != wantItems {
			return nil, fmt.Errorf(
				"fastenal: page at offset %d returned %d jobs, want %d",
				start, len(*page.Data), wantItems,
			)
		}
		for i, posting := range *page.Data {
			if posting.JobID <= 0 {
				return nil, fmt.Errorf("fastenal: item %d at offset %d has invalid jobId %d", i, start, posting.JobID)
			}
			if _, duplicate := seen[posting.JobID]; duplicate {
				return nil, fmt.Errorf("fastenal: duplicate jobId %d", posting.JobID)
			}
			seen[posting.JobID] = struct{}{}
			title := strings.TrimSpace(posting.Title)
			if title == "" {
				return nil, fmt.Errorf("fastenal: item %d at offset %d has an empty title", i, start)
			}
			location := strings.Trim(strings.TrimSpace(posting.City)+", "+strings.TrimSpace(posting.State), ", ")
			var postedAt time.Time
			if posting.ApprovedDate > 0 {
				postedAt = time.UnixMilli(posting.ApprovedDate)
			}
			jobs = append(jobs, model.Job{
				ID:             fmt.Sprintf("fastenal/%d", posting.JobID),
				Company:        s.company,
				Title:          title,
				Location:       location,
				URL:            fmt.Sprintf("%s/details/%d", s.base, posting.JobID),
				EmploymentType: strings.TrimSpace(posting.Type),
				PostedAt:       postedAt,
			})
		}
		if start+len(*page.Data) >= expectedTotal {
			break
		}
	}
	if len(jobs) != expectedTotal {
		return nil, fmt.Errorf("fastenal: collected %d jobs, want %d", len(jobs), expectedTotal)
	}
	return jobs, nil
}

func (s *fastenal) openSession(ctx context.Context) (string, string, []*http.Cookie, error) {
	endpoint := s.base + "/jobs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", nil, fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	body, err := readCustomBoardHTML(resp.Body, endpoint)
	if err != nil {
		return "", "", nil, err
	}
	headerMatch := fastenalCSRFHeaderRE.FindSubmatch(body)
	tokenMatch := fastenalCSRFTokenRE.FindSubmatch(body)
	if headerMatch == nil || tokenMatch == nil {
		return "", "", nil, fmt.Errorf("fastenal: /jobs omitted CSRF metadata")
	}
	header, token := string(headerMatch[1]), string(tokenMatch[1])
	if !fastenalHeaderNameRE.MatchString(header) || strings.TrimSpace(token) == "" {
		return "", "", nil, fmt.Errorf("fastenal: /jobs returned invalid CSRF metadata")
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return "", "", nil, fmt.Errorf("fastenal: /jobs did not establish an anonymous session")
	}
	return header, token, cookies, nil
}

func (s *fastenal) fetchPage(
	ctx context.Context,
	draw, start int,
	csrfHeader, csrfToken string,
	cookies []*http.Cookie,
) (fastenalPage, error) {
	values := url.Values{
		"draw":             {strconv.Itoa(draw)},
		"start":            {strconv.Itoa(start)},
		"length":           {strconv.Itoa(fastenalPageSize)},
		"order[0][column]": {"1"},
		"order[0][dir]":    {"asc"},
		"sortColumn":       {"1"},
		"sortDir":          {"asc"},
		"query":            {""},
	}
	endpoint := s.base + "/load-jobs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return fastenalPage{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrfHeader, csrfToken)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fastenalPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fastenalPage{}, fmt.Errorf("POST %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	var page fastenalPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return fastenalPage{}, fmt.Errorf("POST %s: decoding response: %w", endpoint, err)
	}
	return page, nil
}

func (s *fastenal) Detail(ctx context.Context, job *model.Job) error {
	const prefix = "fastenal/"
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("fastenal: job id %q does not have prefix %q", job.ID, prefix)
	}
	jobID, err := strconv.ParseInt(strings.TrimPrefix(job.ID, prefix), 10, 64)
	if err != nil || jobID <= 0 {
		return fmt.Errorf("fastenal: job id %q has an invalid posting id", job.ID)
	}
	endpoint := fmt.Sprintf("%s/details/%d", s.base, jobID)
	body, err := fetchCustomBoardHTML(ctx, s.client, endpoint)
	if err != nil {
		return err
	}
	idMatch := fastenalDetailIDRE.FindSubmatch(body)
	if idMatch == nil || string(idMatch[1]) != strconv.FormatInt(jobID, 10) {
		return fmt.Errorf("fastenal: detail %d omitted or mismatched Job ID", jobID)
	}
	descriptionMatch := fastenalDescriptionRE.FindSubmatch(body)
	if descriptionMatch == nil {
		return fmt.Errorf("fastenal: detail %d omitted job description", jobID)
	}
	description := htmltext.ToText(string(descriptionMatch[1]))
	description = strings.TrimSpace(strings.TrimPrefix(description, "Job Description"))
	if description == "" {
		return fmt.Errorf("fastenal: detail %d has an empty job description", jobID)
	}
	job.Description = description
	job.URL = endpoint
	return nil
}
