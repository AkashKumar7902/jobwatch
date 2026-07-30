package source

// Goldman Sachs' Higher careers site exposes an anonymous GraphQL API. The
// list is paged and descriptions are fetched lazily for newly evaluated jobs.
//
// Config:
//
//	- name: Goldman Sachs
//	  source: highergs
//	  params:
//	    max_postings: 2000

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	higherGSPageSize              = 100
	higherGSAPIURL                = "https://api-higher.gs.com/gateway/api/v1/graphql"
	higherGSApplicationHost       = "hdpc.fa.us2.oraclecloud.com"
	higherGSApplicationPathPrefix = "/hcmUI/CandidateExperience/en/sites/LateralHiring/job/"
)

var (
	higherGSSourceIDRE = regexp.MustCompile(`^[1-9][0-9]*$`)
	higherGSJobIDRE    = regexp.MustCompile(`^highergs/([1-9][0-9]*)$`)
)

func init() {
	Register("highergs", func(company string, p params.Map, client *http.Client) (Source, error) {
		maxPostings, err := p.Int("max_postings", 2000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &higherGS{
			company: company, apiURL: higherGSAPIURL,
			publicBase: "https://higher.gs.com", maxPostings: maxPostings, client: client,
		}, nil
	})
}

type higherGS struct {
	company     string
	apiURL      string
	publicBase  string
	maxPostings int
	client      *http.Client
}

func (s *higherGS) Company() string { return s.company }

type higherGSLocation struct {
	Primary bool   `json:"primary"`
	State   string `json:"state"`
	Country string `json:"country"`
	City    string `json:"city"`
}

type higherGSExternalSource struct {
	ExternalApplicationURL string `json:"externalApplicationUrl"`
	ApplyInExternalSource  bool   `json:"applyInExternalSource"`
	SourceID               string `json:"sourceId"`
	SecondarySourceID      string `json:"secondarySourceId"`
}

type higherGSRole struct {
	RoleID         string                 `json:"roleId"`
	CorporateTitle string                 `json:"corporateTitle"`
	JobTitle       string                 `json:"jobTitle"`
	JobFunction    string                 `json:"jobFunction"`
	Locations      []higherGSLocation     `json:"locations"`
	Division       string                 `json:"division"`
	Description    string                 `json:"descriptionHtml"`
	ApplyActive    bool                   `json:"applyActive"`
	Status         string                 `json:"status"`
	ExternalSource higherGSExternalSource `json:"externalSource"`
	JobType        *struct {
		Code        string `json:"code"`
		Description string `json:"description"`
	} `json:"jobType"`
}

type higherGSEnvelope struct {
	Data struct {
		RoleSearch *struct {
			TotalCount int            `json:"totalCount"`
			Items      []higherGSRole `json:"items"`
		} `json:"roleSearch"`
		Role *higherGSRole `json:"role"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

const higherGSListQuery = `query GetRoles($searchQueryInput: RoleSearchQueryInput!) {
  roleSearch(searchQueryInput: $searchQueryInput) {
    totalCount
    items {
      roleId corporateTitle jobTitle jobFunction
      locations { primary state country city }
      status division
      jobType { code description }
      externalSource { sourceId }
    }
  }
}`

const higherGSDetailQuery = `query GetRoleById($externalSourceId: String!, $externalSourceFetch: Boolean) {
  role(externalSourceId: $externalSourceId, externalSourceFetch: $externalSourceFetch) {
    roleId corporateTitle jobTitle jobFunction
    locations { primary state country city }
    division descriptionHtml
    jobType { code description }
    applyActive status
    externalSource {
      externalApplicationUrl applyInExternalSource sourceId secondarySourceId
    }
  }
}`

func (s *higherGS) Fetch(ctx context.Context) ([]model.Job, error) {
	var roles []higherGSRole
	seen := make(map[string]struct{})
	total := -1
	for pageNumber := 0; len(roles) < s.maxPostings; pageNumber++ {
		request := map[string]any{
			"operationName": "GetRoles",
			"query":         higherGSListQuery,
			"variables": map[string]any{
				"searchQueryInput": map[string]any{
					"page":        map[string]int{"pageSize": higherGSPageSize, "pageNumber": pageNumber},
					"sort":        map[string]string{"sortStrategy": "RELEVANCE", "sortOrder": "DESC"},
					"filters":     []any{},
					"experiences": []string{"EARLY_CAREER", "PROFESSIONAL"},
					"searchTerm":  "",
				},
			},
		}
		var envelope higherGSEnvelope
		if err := postJSONValue(ctx, s.client, s.apiURL, request, &envelope); err != nil {
			return nil, err
		}
		if err := higherGSErrors(envelope.Errors); err != nil {
			return nil, err
		}
		if envelope.Data.RoleSearch == nil {
			return nil, fmt.Errorf("highergs: response omitted roleSearch")
		}
		page := envelope.Data.RoleSearch
		if page.TotalCount < 0 {
			return nil, fmt.Errorf("highergs: response reported negative totalCount %d", page.TotalCount)
		}
		if total < 0 {
			total = page.TotalCount
		} else if page.TotalCount != total {
			return nil, fmt.Errorf("highergs: totalCount changed from %d to %d on page %d", total, page.TotalCount, pageNumber)
		}
		if len(page.Items) > higherGSPageSize {
			return nil, fmt.Errorf(
				"highergs: page %d returned %d roles, exceeding page size %d",
				pageNumber, len(page.Items), higherGSPageSize,
			)
		}
		pageOffset := pageNumber * higherGSPageSize
		if pageOffset+len(page.Items) > total {
			return nil, fmt.Errorf(
				"highergs: page %d extends through role %d, exceeding totalCount %d",
				pageNumber, pageOffset+len(page.Items), total,
			)
		}
		expectedPageItems := min(higherGSPageSize, total-pageOffset)
		if len(page.Items) != expectedPageItems {
			return nil, fmt.Errorf(
				"highergs: page %d returned %d roles, want %d for totalCount %d",
				pageNumber, len(page.Items), expectedPageItems, total,
			)
		}
		if len(page.Items) == 0 {
			if len(roles) < min(total, s.maxPostings) {
				return nil, fmt.Errorf("highergs: pagination ended after %d of %d roles", len(roles), total)
			}
			break
		}
		for i, role := range page.Items {
			sourceID := strings.TrimSpace(role.ExternalSource.SourceID)
			if !higherGSSourceIDRE.MatchString(sourceID) {
				return nil, fmt.Errorf(
					"highergs: page %d item %d has invalid external source ID %q",
					pageNumber, i, role.ExternalSource.SourceID,
				)
			}
			if _, duplicate := seen[sourceID]; duplicate {
				return nil, fmt.Errorf("highergs: duplicate external source ID %q", sourceID)
			}
			seen[sourceID] = struct{}{}
			roles = append(roles, role)
			if len(roles) >= s.maxPostings {
				break
			}
		}
		if len(roles) >= total {
			break
		}
	}

	jobs := make([]model.Job, 0, len(roles))
	for i, role := range roles {
		title := strings.TrimSpace(role.JobTitle)
		if title == "" {
			return nil, fmt.Errorf("highergs: role %d has an empty job title", i)
		}
		if role.Status != "POSTED" {
			return nil, fmt.Errorf(
				"highergs: role %d has status %q, want POSTED",
				i, role.Status,
			)
		}
		sourceID := strings.TrimSpace(role.ExternalSource.SourceID)
		jobs = append(jobs, model.Job{
			ID:       "highergs/" + sourceID,
			Company:  s.company,
			Title:    title,
			Location: higherGSLocations(role.Locations),
			URL:      s.publicBase + "/roles/" + sourceID,
		})
	}
	return jobs, nil
}

func (s *higherGS) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("highergs: nil job")
	}
	match := higherGSJobIDRE.FindStringSubmatch(job.ID)
	if match == nil {
		return fmt.Errorf("highergs: invalid job ID %q", job.ID)
	}
	sourceID := match[1]
	request := map[string]any{
		"operationName": "GetRoleById",
		"query":         higherGSDetailQuery,
		"variables": map[string]any{
			"externalSourceId": sourceID, "externalSourceFetch": true,
		},
	}
	var envelope higherGSEnvelope
	if err := postJSONValue(ctx, s.client, s.apiURL, request, &envelope); err != nil {
		return err
	}
	if err := higherGSErrors(envelope.Errors); err != nil {
		return err
	}
	role := envelope.Data.Role
	if role == nil {
		return fmt.Errorf("highergs: detail %q omitted role", sourceID)
	}
	if role.ExternalSource.SourceID != sourceID {
		return fmt.Errorf("highergs: detail source ID %q does not match %q", role.ExternalSource.SourceID, sourceID)
	}
	if role.Status != "POSTED" {
		return fmt.Errorf(
			"highergs: detail %q has status %q, want POSTED",
			sourceID, role.Status,
		)
	}
	if !role.ApplyActive {
		return fmt.Errorf("highergs: detail %q is not accepting applications", sourceID)
	}
	description := htmltext.ToText(role.Description)
	if description == "" {
		return fmt.Errorf("highergs: detail %q has an empty description", sourceID)
	}
	applicationURL, err := validateHigherGSApplicationURL(
		role.ExternalSource.ExternalApplicationURL,
		sourceID,
	)
	if err != nil {
		return fmt.Errorf("highergs: detail %q: %w", sourceID, err)
	}
	employmentType := ""
	if role.JobType != nil {
		employmentType = strings.TrimSpace(role.JobType.Description)
	}
	location := higherGSLocations(role.Locations)

	job.Description = description
	if role.JobType != nil {
		job.EmploymentType = employmentType
	}
	if location != "" {
		job.Location = location
	}
	job.URL = applicationURL
	return nil
}

func validateHigherGSApplicationURL(raw, sourceID string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.User != nil || parsed.Opaque != "" || parsed.Host == "" ||
		!strings.EqualFold(parsed.Hostname(), higherGSApplicationHost) ||
		parsed.Port() != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || strings.Contains(raw, "#") || parsed.RawPath != "" {
		return "", fmt.Errorf("invalid external application URL %q", raw)
	}
	expectedPath := higherGSApplicationPathPrefix + sourceID + "/apply/email"
	if parsed.Path != expectedPath {
		return "", fmt.Errorf(
			"external application URL path is %q, want %q",
			parsed.Path,
			expectedPath,
		)
	}
	return raw, nil
}

func higherGSLocations(locations []higherGSLocation) string {
	var values []string
	for _, location := range locations {
		values = append(values, strings.Join(distinctStrings([]string{
			location.City, location.State, location.Country,
		}), ", "))
	}
	return strings.Join(distinctStrings(values), "; ")
}

func higherGSErrors(errors []struct {
	Message string `json:"message"`
}) error {
	if len(errors) == 0 {
		return nil
	}
	var messages []string
	for _, item := range errors {
		if message := strings.TrimSpace(item.Message); message != "" {
			messages = append(messages, message)
		}
	}
	if len(messages) == 0 {
		return fmt.Errorf("highergs GraphQL returned %d unnamed errors", len(errors))
	}
	return fmt.Errorf("highergs GraphQL: %s", strings.Join(messages, "; "))
}
