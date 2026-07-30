package source

// Siemens Digital Industries Software exposes its Jobsyn search index as an
// anonymous paginated API. This source intentionally selects the EDA business
// structure and uses the per-location GUID, not reqid, because one requisition
// can appear as several location rows.
//
//	GET https://prod-search-api.jobsyn.org/api/v1/solr/search
//	    ?page=N&businessStructures=electronic-design-automation-eda&num_items=10
//	    x-origin: jobs.sw.siemens.com

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	siemensJobsPageSize = 10
	siemensJobsMaxPages = 500
)

var (
	siemensJobsGUIDRE    = regexp.MustCompile(`^[0-9A-F]{32}$`)
	siemensJobsJobTypeRE = regexp.MustCompile(`(?mi)\*\*Job Type:\*\*\s*([^\r\n]+)`)
	siemensJobsSlugPart  = regexp.MustCompile(`[^a-z0-9]+`)
	siemensJobsBadSlugRE = regexp.MustCompile(`[\x00-\x20/?#]`)
)

func init() {
	Register("siemensjobs", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &siemensJobs{
			company:  company,
			apiBase:  "https://prod-search-api.jobsyn.org/api/v1/solr/search",
			jobsBase: "https://jobs.sw.siemens.com",
			origin:   "jobs.sw.siemens.com",
			client:   client,
		}, nil
	})
}

type siemensJobs struct {
	company  string
	apiBase  string
	jobsBase string
	origin   string
	client   *http.Client
}

type siemensJobsPosting struct {
	GUID        string `json:"guid"`
	ID          string `json:"id"`
	Title       string `json:"title_exact"`
	TitleSlug   string `json:"title_slug"`
	Location    string `json:"location_exact"`
	Description string `json:"description"`
	DateNew     string `json:"date_new"`
}

type siemensJobsPagination struct {
	HasMorePages bool           `json:"has_more_pages"`
	Offset       siemensJobsInt `json:"offset"`
	Page         siemensJobsInt `json:"page"`
	PageSize     siemensJobsInt `json:"page_size"`
	Total        siemensJobsInt `json:"total"`
	TotalPages   siemensJobsInt `json:"total_pages"`
}

// Jobsyn inconsistently serializes integer pagination values as either 2 or
// 2.0. Accept both representations, but reject genuinely fractional values.
type siemensJobsInt int

func (n *siemensJobsInt) UnmarshalJSON(data []byte) error {
	value, err := strconv.ParseFloat(string(data), 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) ||
		value != math.Trunc(value) || value < math.MinInt || value > math.MaxInt {
		return fmt.Errorf("expected an integer, got %q", data)
	}
	*n = siemensJobsInt(value)
	return nil
}

func (s *siemensJobs) Company() string { return s.company }

func (s *siemensJobs) Fetch(ctx context.Context) ([]model.Job, error) {
	var (
		jobs          []model.Job
		seen          = make(map[string]struct{})
		expectedTotal = -1
		expectedPages = -1
	)
	for pageNumber := 1; ; pageNumber++ {
		if pageNumber > siemensJobsMaxPages {
			return nil, fmt.Errorf("siemensjobs: pagination exceeded safety limit %d", siemensJobsMaxPages)
		}
		page, err := s.fetchPage(ctx, pageNumber)
		if err != nil {
			return nil, err
		}
		if page.Jobs == nil || page.Pagination == nil {
			return nil, fmt.Errorf("siemensjobs: page %d omitted jobs or pagination", pageNumber)
		}
		pagination := *page.Pagination
		reportedPage := int(pagination.Page)
		offset := int(pagination.Offset)
		pageSize := int(pagination.PageSize)
		total := int(pagination.Total)
		totalPages := int(pagination.TotalPages)
		if reportedPage != pageNumber ||
			offset != (pageNumber-1)*siemensJobsPageSize ||
			pageSize != siemensJobsPageSize {
			return nil, fmt.Errorf("siemensjobs: page %d returned inconsistent pagination metadata", pageNumber)
		}
		if total < 0 || totalPages < 0 {
			return nil, fmt.Errorf("siemensjobs: page %d returned negative pagination totals", pageNumber)
		}
		calculatedPages := 0
		if total > 0 {
			calculatedPages = (total + siemensJobsPageSize - 1) / siemensJobsPageSize
		}
		if totalPages != calculatedPages {
			return nil, fmt.Errorf(
				"siemensjobs: total %d is inconsistent with total_pages %d",
				total, totalPages,
			)
		}
		if pagination.HasMorePages != (pageNumber < totalPages) {
			return nil, fmt.Errorf("siemensjobs: page %d has inconsistent has_more_pages", pageNumber)
		}
		if pageNumber == 1 {
			expectedTotal, expectedPages = total, totalPages
			if expectedPages > siemensJobsMaxPages {
				return nil, fmt.Errorf("siemensjobs: total_pages %d exceeds safety limit %d", expectedPages, siemensJobsMaxPages)
			}
		} else if total != expectedTotal || totalPages != expectedPages {
			return nil, fmt.Errorf("siemensjobs: pagination totals changed on page %d", pageNumber)
		}

		wantItems := siemensJobsPageSize
		remaining := expectedTotal - (pageNumber-1)*siemensJobsPageSize
		if remaining < wantItems {
			wantItems = remaining
		}
		if wantItems < 0 || len(*page.Jobs) != wantItems {
			return nil, fmt.Errorf(
				"siemensjobs: page %d returned %d jobs, want %d",
				pageNumber, len(*page.Jobs), wantItems,
			)
		}
		for i, posting := range *page.Jobs {
			guid := strings.ToUpper(strings.TrimSpace(posting.GUID))
			if !siemensJobsGUIDRE.MatchString(guid) {
				return nil, fmt.Errorf("siemensjobs: page %d item %d has invalid guid %q", pageNumber, i, posting.GUID)
			}
			if posting.ID != "seo.joblisting."+guid {
				return nil, fmt.Errorf("siemensjobs: page %d item %d id does not match guid", pageNumber, i)
			}
			if _, duplicate := seen[guid]; duplicate {
				return nil, fmt.Errorf("siemensjobs: duplicate guid %q", guid)
			}
			seen[guid] = struct{}{}
			title := strings.TrimSpace(posting.Title)
			titleSlug := strings.TrimSpace(posting.TitleSlug)
			if title == "" || titleSlug == "" || siemensJobsBadSlugRE.MatchString(titleSlug) {
				return nil, fmt.Errorf("siemensjobs: page %d item %d has invalid title or title_slug", pageNumber, i)
			}
			description := strings.TrimSpace(posting.Description)
			if description == "" {
				return nil, fmt.Errorf("siemensjobs: page %d item %d has an empty description", pageNumber, i)
			}
			locationSlug := siemensJobsSlug(posting.Location)
			if locationSlug == "" {
				return nil, fmt.Errorf("siemensjobs: page %d item %d has an empty location", pageNumber, i)
			}
			var postedAt time.Time
			if strings.TrimSpace(posting.DateNew) != "" {
				postedAt, err = time.Parse(time.RFC3339Nano, posting.DateNew)
				if err != nil {
					return nil, fmt.Errorf("siemensjobs: page %d item %d has invalid date_new: %w", pageNumber, i, err)
				}
			}
			employmentType := ""
			if match := siemensJobsJobTypeRE.FindStringSubmatch(description); match != nil {
				employmentType = strings.TrimSpace(match[1])
			}
			jobs = append(jobs, model.Job{
				ID:             "siemensjobs/" + guid,
				Company:        s.company,
				Title:          title,
				Location:       strings.TrimSpace(posting.Location),
				URL:            fmt.Sprintf("%s/%s/%s/%s/job/", s.jobsBase, locationSlug, url.PathEscape(titleSlug), guid),
				EmploymentType: employmentType,
				Description:    description,
				PostedAt:       postedAt,
			})
		}
		if pageNumber >= expectedPages {
			break
		}
	}
	if len(jobs) != expectedTotal {
		return nil, fmt.Errorf("siemensjobs: collected %d jobs, want %d", len(jobs), expectedTotal)
	}
	return jobs, nil
}

func (s *siemensJobs) fetchPage(ctx context.Context, pageNumber int) (struct {
	Jobs       *[]siemensJobsPosting  `json:"jobs"`
	Pagination *siemensJobsPagination `json:"pagination"`
}, error) {
	var page struct {
		Jobs       *[]siemensJobsPosting  `json:"jobs"`
		Pagination *siemensJobsPagination `json:"pagination"`
	}
	query := url.Values{
		"page":               {fmt.Sprint(pageNumber)},
		"businessStructures": {"electronic-design-automation-eda"},
		"num_items":          {fmt.Sprint(siemensJobsPageSize)},
	}
	endpoint := s.apiBase + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return page, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-origin", s.origin)
	resp, err := s.client.Do(req)
	if err != nil {
		return page, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return page, fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return page, fmt.Errorf("GET %s: decoding response: %w", endpoint, err)
	}
	return page, nil
}

func siemensJobsSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Trim(siemensJobsSlugPart.ReplaceAllString(value, "-"), "-")
}
