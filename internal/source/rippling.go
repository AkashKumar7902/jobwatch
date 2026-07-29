package source

// Rippling Recruiting public board API (no auth):
//
//	GET https://ats.rippling.com/api/v2/board/{slug}/jobs?groupJobsByLocation=true&page=N&pageSize=1000
//	GET https://ats.rippling.com/api/v2/board/{slug}/jobs/{id}
//
// The list endpoint omits the full description, so details are fetched lazily
// only for postings the runner evaluates.
//
// Config:
//
//	- name: Rippling
//	  source: rippling
//	  params:
//	    board_slug: rippling

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	ripplingPageSize = 1000
	ripplingMaxPages = 100
)

var ripplingSlugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var ripplingPostingIDRE = regexp.MustCompile(`^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$`)

func init() {
	Register("rippling", func(company string, p params.Map, client *http.Client) (Source, error) {
		slug, err := p.Require("board_slug")
		if err != nil {
			return nil, err
		}
		if !ripplingSlugRE.MatchString(slug) {
			return nil, fmt.Errorf("param %q: invalid Rippling board slug %q", "board_slug", slug)
		}
		return &rippling{
			company:  company,
			slug:     slug,
			apiBase:  "https://ats.rippling.com/api/v2/board/" + slug,
			jobsBase: "https://ats.rippling.com/" + slug + "/jobs",
			client:   client,
		}, nil
	})
}

type rippling struct {
	company  string
	slug     string
	apiBase  string
	jobsBase string
	client   *http.Client
}

type ripplingListPage struct {
	Items      *[]ripplingPosting `json:"items"`
	Page       *int               `json:"page"`
	PageSize   *int               `json:"pageSize"`
	TotalItems *int               `json:"totalItems"`
	TotalPages *int               `json:"totalPages"`
}

type ripplingPosting struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Locations []struct {
		Name string `json:"name"`
	} `json:"locations"`
}

type ripplingDetail struct {
	UUID               string  `json:"uuid"`
	CreatedOn          *string `json:"createdOn"`
	UnlistedFromSearch *bool   `json:"unlistedFromSearch"`
	Description        *struct {
		Company string `json:"company"`
		Role    string `json:"role"`
	} `json:"description"`
	EmploymentType struct {
		Label string `json:"label"`
		ID    string `json:"id"`
	} `json:"employmentType"`
	WorkLocations []string `json:"workLocations"`
}

func (s *rippling) Company() string { return s.company }

func (s *rippling) Fetch(ctx context.Context) ([]model.Job, error) {
	var (
		expectedTotal int
		expectedPages int
		postings      []ripplingPosting
	)

	for pageNumber := 0; ; pageNumber++ {
		query := url.Values{
			"groupJobsByLocation": {"true"},
			"page":                {fmt.Sprint(pageNumber)},
			"pageSize":            {fmt.Sprint(ripplingPageSize)},
		}
		var page ripplingListPage
		endpoint := s.apiBase + "/jobs?" + query.Encode()
		if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &page); err != nil {
			return nil, err
		}
		if page.Items == nil || page.Page == nil || page.PageSize == nil ||
			page.TotalItems == nil || page.TotalPages == nil {
			return nil, fmt.Errorf("rippling %s: page %d omitted required pagination metadata", s.slug, pageNumber)
		}
		if *page.Page != pageNumber {
			return nil, fmt.Errorf("rippling %s: requested page %d, response reported page %d", s.slug, pageNumber, *page.Page)
		}
		if *page.PageSize != ripplingPageSize {
			return nil, fmt.Errorf("rippling %s: page %d reported pageSize %d, want %d", s.slug, pageNumber, *page.PageSize, ripplingPageSize)
		}
		if *page.TotalItems < 0 || *page.TotalPages < 0 {
			return nil, fmt.Errorf("rippling %s: page %d reported negative totals", s.slug, pageNumber)
		}

		if pageNumber == 0 {
			expectedTotal = *page.TotalItems
			expectedPages = *page.TotalPages
			if expectedPages > ripplingMaxPages {
				return nil, fmt.Errorf("rippling %s: totalPages %d exceeds safety limit %d", s.slug, expectedPages, ripplingMaxPages)
			}

			calculatedPages := 0
			if expectedTotal > 0 {
				calculatedPages = (expectedTotal + ripplingPageSize - 1) / ripplingPageSize
			}
			coherentEmpty := expectedTotal == 0 && expectedPages == 0
			if !coherentEmpty && expectedPages != calculatedPages {
				return nil, fmt.Errorf("rippling %s: totalItems %d is inconsistent with totalPages %d", s.slug, expectedTotal, expectedPages)
			}
		} else if *page.TotalItems != expectedTotal || *page.TotalPages != expectedPages {
			return nil, fmt.Errorf("rippling %s: pagination totals changed on page %d", s.slug, pageNumber)
		}

		postings = append(postings, (*page.Items)...)
		if expectedTotal == 0 {
			if len(*page.Items) != 0 {
				return nil, fmt.Errorf("rippling %s: empty board returned %d items", s.slug, len(*page.Items))
			}
			return []model.Job{}, nil
		}

		remaining := expectedTotal - pageNumber*ripplingPageSize
		wantItems := ripplingPageSize
		if remaining < wantItems {
			wantItems = remaining
		}
		if wantItems <= 0 || len(*page.Items) != wantItems {
			return nil, fmt.Errorf("rippling %s: page %d returned %d items, want %d", s.slug, pageNumber, len(*page.Items), wantItems)
		}
		if pageNumber+1 >= expectedPages {
			break
		}
	}

	if len(postings) != expectedTotal {
		return nil, fmt.Errorf("rippling %s: collected %d items, want %d", s.slug, len(postings), expectedTotal)
	}

	jobs := make([]model.Job, 0, len(postings))
	seen := make(map[string]struct{}, len(postings))
	for i, posting := range postings {
		if !ripplingPostingIDRE.MatchString(posting.ID) {
			return nil, fmt.Errorf("rippling %s: item %d has invalid posting id %q", s.slug, i, posting.ID)
		}
		title := strings.TrimSpace(posting.Name)
		if title == "" {
			return nil, fmt.Errorf("rippling %s: item %d has an empty name", s.slug, i)
		}
		canonicalURL := s.jobsBase + "/" + posting.ID
		if posting.URL != canonicalURL {
			return nil, fmt.Errorf("rippling %s: item %d URL %q does not match canonical URL %q", s.slug, i, posting.URL, canonicalURL)
		}
		if _, duplicate := seen[posting.ID]; duplicate {
			return nil, fmt.Errorf("rippling %s: duplicate posting id %q", s.slug, posting.ID)
		}
		seen[posting.ID] = struct{}{}

		jobs = append(jobs, model.Job{
			ID:       fmt.Sprintf("rippling/%s/%s", s.slug, posting.ID),
			Company:  s.company,
			Title:    title,
			Location: joinDistinctLocations(posting.Locations),
			URL:      canonicalURL,
		})
	}
	return jobs, nil
}

func (s *rippling) Detail(ctx context.Context, job *model.Job) error {
	prefix := fmt.Sprintf("rippling/%s/", s.slug)
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("rippling %s: job id %q does not have prefix %q", s.slug, job.ID, prefix)
	}
	postingID := strings.TrimPrefix(job.ID, prefix)
	if postingID == "" {
		return fmt.Errorf("rippling %s: job id has an empty posting id", s.slug)
	}
	if !ripplingPostingIDRE.MatchString(postingID) {
		return fmt.Errorf("rippling %s: job id has invalid posting id %q", s.slug, postingID)
	}

	var detail ripplingDetail
	endpoint := s.apiBase + "/jobs/" + url.PathEscape(postingID)
	if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &detail); err != nil {
		return err
	}
	if detail.UUID != postingID {
		return fmt.Errorf("rippling %s: detail UUID %q does not match posting id %q", s.slug, detail.UUID, postingID)
	}
	if detail.UnlistedFromSearch == nil {
		return fmt.Errorf("rippling %s: detail %q omitted unlistedFromSearch", s.slug, postingID)
	}
	if *detail.UnlistedFromSearch {
		return fmt.Errorf("rippling %s: detail %q is unlisted from search", s.slug, postingID)
	}
	if detail.Description == nil {
		return fmt.Errorf("rippling %s: detail %q omitted description", s.slug, postingID)
	}

	companyDescription := htmltext.ToText(detail.Description.Company)
	roleDescription := htmltext.ToText(detail.Description.Role)
	if roleDescription == "" {
		return fmt.Errorf("rippling %s: detail %q has an empty role description", s.slug, postingID)
	}
	description := roleDescription
	if companyDescription != "" {
		description = companyDescription + "\n\n" + roleDescription
	}
	var postedAt time.Time
	if detail.CreatedOn != nil && strings.TrimSpace(*detail.CreatedOn) != "" {
		var err error
		postedAt, err = time.Parse(time.RFC3339Nano, *detail.CreatedOn)
		if err != nil {
			return fmt.Errorf("rippling %s: detail %q has invalid createdOn: %w", s.slug, postingID, err)
		}
	}

	employmentType := strings.TrimSpace(detail.EmploymentType.ID)
	if employmentType == "" {
		employmentType = strings.TrimSpace(detail.EmploymentType.Label)
	}
	detailLocations := distinctStrings(detail.WorkLocations)

	job.Description = description
	job.EmploymentType = employmentType
	job.PostedAt = postedAt
	job.URL = s.jobsBase + "/" + postingID
	if len(detailLocations) > 0 {
		job.Location = strings.Join(detailLocations, "; ")
	}
	return nil
}

func joinDistinctLocations(locations []struct {
	Name string `json:"name"`
}) string {
	names := make([]string, 0, len(locations))
	for _, location := range locations {
		names = append(names, location.Name)
	}
	return strings.Join(distinctStrings(names), "; ")
}

func distinctStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
