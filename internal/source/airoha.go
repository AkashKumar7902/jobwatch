package source

// Airoha renders a public, paginated careers table and full detail pages.
// The site defaults to a locale-dependent UI, so the source first obtains the
// anonymous en-US culture cookie from its public SetLanguage endpoint.
//
//	GET https://careers.airoha.com/Home/SetLanguage?culture=en-US&returnUrl=/Jobs
//	GET https://careers.airoha.com/Jobs?page=N
//	GET https://careers.airoha.com/Jobs/Detail?sn={uuid}

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	airohaPageSize = 10
	airohaMaxPages = 100
)

var (
	airohaRowRE         = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	airohaCellRE        = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	airohaDetailLinkRE  = regexp.MustCompile(`(?is)href=["']/Jobs/Detail\?sn=([0-9a-f-]{36})["']`)
	airohaPageLinkRE    = regexp.MustCompile(`(?i)href=["']/Jobs\?page=([0-9]+)["']`)
	airohaTitleRE       = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	airohaLocationRE    = regexp.MustCompile(`(?is)<td\s+data-label=["']Location["'][^>]*>(.*?)</td>`)
	airohaTypeRE        = regexp.MustCompile(`(?is)<td\s+data-label=["']Type["'][^>]*>(.*?)</td>`)
	airohaDescriptionRE = regexp.MustCompile(`(?is)<h3[^>]*>\s*Description\s*</h3>.*?<p[^>]*>(.*?)</p>`)
	airohaRequirementRE = regexp.MustCompile(`(?is)<h3[^>]*>\s*Requirement\s*</h3>.*?<p[^>]*>(.*?)</p>`)
)

func init() {
	Register("airoha", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &airoha{company: company, base: "https://careers.airoha.com", client: client}, nil
	})
}

type airoha struct {
	company string
	base    string
	client  *http.Client

	cultureMu     sync.Mutex
	cultureCookie *http.Cookie
}

func (s *airoha) Company() string { return s.company }

func (s *airoha) Fetch(ctx context.Context) ([]model.Job, error) {
	if _, err := s.ensureCulture(ctx); err != nil {
		return nil, err
	}
	var (
		jobs          []model.Job
		seen          = make(map[string]struct{})
		expectedPages = -1
	)
	for pageNumber := 1; ; pageNumber++ {
		if pageNumber > airohaMaxPages {
			return nil, fmt.Errorf("airoha: pagination exceeded safety limit %d", airohaMaxPages)
		}
		endpoint := s.base + "/Jobs"
		if pageNumber > 1 {
			endpoint += "?page=" + strconv.Itoa(pageNumber)
		}
		body, err := s.fetchHTML(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		if pageNumber == 1 {
			expectedPages = 1
			for _, match := range airohaPageLinkRE.FindAllSubmatch(body, -1) {
				page, _ := strconv.Atoi(string(match[1]))
				if page > expectedPages {
					expectedPages = page
				}
			}
			if expectedPages > airohaMaxPages {
				return nil, fmt.Errorf("airoha: page count %d exceeds safety limit %d", expectedPages, airohaMaxPages)
			}
		}

		pageJobs := 0
		for _, rowMatch := range airohaRowRE.FindAllSubmatch(body, -1) {
			linkMatch := airohaDetailLinkRE.FindSubmatch(rowMatch[1])
			if linkMatch == nil {
				continue
			}
			postingID := strings.ToLower(string(linkMatch[1]))
			if !customBoardUUIDRE.MatchString(postingID) {
				return nil, fmt.Errorf("airoha: page %d has invalid posting id %q", pageNumber, postingID)
			}
			if _, duplicate := seen[postingID]; duplicate {
				return nil, fmt.Errorf("airoha: duplicate posting id %q", postingID)
			}
			cells := airohaCellRE.FindAllSubmatch(rowMatch[1], -1)
			if len(cells) < 6 {
				return nil, fmt.Errorf("airoha: page %d posting %q has %d cells, want at least 6", pageNumber, postingID, len(cells))
			}
			title := cleanText(string(cells[0][1]))
			location := cleanText(string(cells[2][1]))
			if title == "" || location == "" {
				return nil, fmt.Errorf("airoha: page %d posting %q has an empty title or location", pageNumber, postingID)
			}
			seen[postingID] = struct{}{}
			pageJobs++
			jobs = append(jobs, model.Job{
				ID:       "airoha/" + postingID,
				Company:  s.company,
				Title:    title,
				Location: location,
				URL:      s.base + "/Jobs/Detail?sn=" + url.QueryEscape(postingID),
			})
		}
		if pageJobs == 0 {
			return nil, fmt.Errorf("airoha: page %d returned no job rows", pageNumber)
		}
		if pageNumber < expectedPages && pageJobs != airohaPageSize {
			return nil, fmt.Errorf("airoha: page %d returned %d jobs, want %d", pageNumber, pageJobs, airohaPageSize)
		}
		if pageJobs > airohaPageSize {
			return nil, fmt.Errorf("airoha: page %d returned %d jobs, exceeds page size %d", pageNumber, pageJobs, airohaPageSize)
		}
		if pageNumber >= expectedPages {
			break
		}
	}
	return jobs, nil
}

func (s *airoha) Detail(ctx context.Context, job *model.Job) error {
	const prefix = "airoha/"
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("airoha: job id %q does not have prefix %q", job.ID, prefix)
	}
	postingID := strings.ToLower(strings.TrimPrefix(job.ID, prefix))
	if !customBoardUUIDRE.MatchString(postingID) {
		return fmt.Errorf("airoha: job id %q has an invalid posting id", job.ID)
	}
	if _, err := s.ensureCulture(ctx); err != nil {
		return err
	}
	endpoint := s.base + "/Jobs/Detail?sn=" + url.QueryEscape(postingID)
	body, err := s.fetchHTML(ctx, endpoint)
	if err != nil {
		return err
	}
	titleMatch := airohaTitleRE.FindSubmatch(body)
	locationMatch := airohaLocationRE.FindSubmatch(body)
	typeMatch := airohaTypeRE.FindSubmatch(body)
	descriptionMatch := airohaDescriptionRE.FindSubmatch(body)
	requirementMatch := airohaRequirementRE.FindSubmatch(body)
	if titleMatch == nil || locationMatch == nil || typeMatch == nil ||
		descriptionMatch == nil || requirementMatch == nil {
		return fmt.Errorf("airoha: detail %q omitted required fields", postingID)
	}
	title := cleanText(string(titleMatch[1]))
	location := cleanText(string(locationMatch[1]))
	employmentType := cleanText(string(typeMatch[1]))
	description := htmltext.ToText(string(descriptionMatch[1]))
	requirement := htmltext.ToText(string(requirementMatch[1]))
	if title == "" || location == "" || employmentType == "" || description == "" {
		return fmt.Errorf("airoha: detail %q has an empty required field", postingID)
	}
	if requirement != "" {
		description += "\n\nRequirement\n" + requirement
	}
	job.Title = title
	job.Location = location
	job.EmploymentType = employmentType
	job.Description = description
	job.URL = endpoint
	return nil
}

func (s *airoha) ensureCulture(ctx context.Context) (*http.Cookie, error) {
	s.cultureMu.Lock()
	defer s.cultureMu.Unlock()
	if s.cultureCookie != nil {
		return s.cultureCookie, nil
	}
	query := url.Values{"culture": {"en-US"}, "returnUrl": {"/Jobs"}}
	endpoint := s.base + "/Home/SetLanguage?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	redirectClient := *s.client
	redirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := redirectClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	for _, cookie := range resp.Cookies() {
		value, _ := url.QueryUnescape(cookie.Value)
		if strings.Contains(value, "c=en-US") && strings.Contains(value, "uic=en-US") {
			s.cultureCookie = cookie
			return cookie, nil
		}
	}
	return nil, fmt.Errorf("airoha: SetLanguage did not return the en-US culture cookie")
}

func (s *airoha) fetchHTML(ctx context.Context, endpoint string) ([]byte, error) {
	cookie, err := s.ensureCulture(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.AddCookie(cookie)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	return readCustomBoardHTML(resp.Body, endpoint)
}
