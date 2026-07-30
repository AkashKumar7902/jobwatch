package source

// UKG Recruiting (formerly UltiPro) exposes an anonymous, offset-paged JSON
// search endpoint. The public detail page embeds the complete posting as the
// argument to US.Opportunity.CandidateOpportunityDetail, so details can be
// fetched lazily without signing in.
//
// Config:
//
//	- name: AppLogic Networks
//	  source: ukg
//	  params:
//	    host: recruiting2.ultipro.com
//	    tenant: PRO1053PROC
//	    board: d6eed263-4950-420d-b9f8-5b1a441c931e

import (
	"bytes"
	"context"
	"encoding/json"
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
	ukgPageSize         = 50
	ukgMaximumPostings  = 100_000
	ukgDetailJSONMarker = `new US.Opportunity.CandidateOpportunityDetail`
)

var (
	ukgTenantRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	ukgUUIDRE   = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	ukgDetailRE = regexp.MustCompile(`new\s+US\.Opportunity\.CandidateOpportunityDetail\s*\(`)
)

func init() {
	Register("ukg", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawHost, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		host, err := normalizeBoardHost(rawHost)
		if err != nil {
			return nil, err
		}
		tenant, err := p.Require("tenant")
		if err != nil {
			return nil, err
		}
		if !ukgTenantRE.MatchString(tenant) {
			return nil, fmt.Errorf("param %q: invalid UKG tenant %q", "tenant", tenant)
		}
		rawBoard, err := p.Require("board")
		if err != nil {
			return nil, err
		}
		board, err := canonicalUKGUUID(rawBoard)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", "board", err)
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &ukg{
			company: company,
			host:    host,
			tenant:  tenant,
			board:   board,
			baseURL: "https://" + host,
			client:  client,
		}, nil
	})
}

type ukg struct {
	company string
	host    string
	tenant  string
	board   string
	baseURL string
	client  *http.Client
}

func (s *ukg) Company() string { return s.company }

type ukgSearchRequest struct {
	OpportunitySearch struct {
		Top         int    `json:"Top"`
		Skip        int    `json:"Skip"`
		QueryString string `json:"QueryString"`
		Filters     []any  `json:"Filters"`
	} `json:"opportunitySearch"`
}

type ukgSearchResponse struct {
	Opportunities *[]ukgOpportunity `json:"opportunities"`
	TotalCount    *int              `json:"totalCount"`
}

type ukgOpportunity struct {
	ID                string        `json:"Id"`
	Title             string        `json:"Title"`
	RequisitionNumber string        `json:"RequisitionNumber"`
	FullTime          *bool         `json:"FullTime"`
	JobCategoryName   string        `json:"JobCategoryName"`
	Locations         []ukgLocation `json:"Locations"`
	PostedDate        string        `json:"PostedDate"`
	Description       string        `json:"Description"`
	OpportunityClosed *bool         `json:"OpportunityIsClosed"`
}

type ukgLocation struct {
	LocalizedName        string     `json:"LocalizedName"`
	LocalizedLocationID  string     `json:"LocalizedLocationId"`
	LocalizedDescription string     `json:"LocalizedDescription"`
	Address              ukgAddress `json:"Address"`
}

type ukgAddress struct {
	Line1      string     `json:"Line1"`
	Line2      string     `json:"Line2"`
	City       string     `json:"City"`
	PostalCode string     `json:"PostalCode"`
	State      *ukgRegion `json:"State"`
	Country    *ukgRegion `json:"Country"`
}

type ukgRegion struct {
	Code string `json:"Code"`
	Name string `json:"Name"`
}

func (s *ukg) Fetch(ctx context.Context) ([]model.Job, error) {
	jobs := make([]model.Job, 0)
	seen := make(map[string]struct{})
	expectedTotal := -1

	for skip := 0; ; {
		var request ukgSearchRequest
		request.OpportunitySearch.Top = ukgPageSize
		request.OpportunitySearch.Skip = skip
		request.OpportunitySearch.Filters = []any{}
		body, err := json.Marshal(request)
		if err != nil {
			return nil, fmt.Errorf("ukg %s: encode page at offset %d: %w", s.host, skip, err)
		}

		var response ukgSearchResponse
		if err := fetchJSON(ctx, s.client, http.MethodPost, s.searchURL(), body, &response); err != nil {
			return nil, fmt.Errorf("ukg %s page at offset %d: %w", s.host, skip, err)
		}
		if response.TotalCount == nil {
			return nil, fmt.Errorf("ukg %s page at offset %d: response omitted totalCount", s.host, skip)
		}
		if response.Opportunities == nil {
			return nil, fmt.Errorf("ukg %s page at offset %d: response omitted opportunities", s.host, skip)
		}
		total := *response.TotalCount
		if total < 0 {
			return nil, fmt.Errorf("ukg %s page at offset %d: negative totalCount %d", s.host, skip, total)
		}
		if total > ukgMaximumPostings {
			return nil, fmt.Errorf("ukg %s: reported totalCount %d exceeds safety limit %d", s.host, total, ukgMaximumPostings)
		}
		if expectedTotal < 0 {
			expectedTotal = total
			jobs = make([]model.Job, 0, total)
		} else if total != expectedTotal {
			return nil, fmt.Errorf("ukg %s page at offset %d: totalCount changed from %d to %d", s.host, skip, expectedTotal, total)
		}

		postings := *response.Opportunities
		if len(postings) > ukgPageSize {
			return nil, fmt.Errorf("ukg %s page at offset %d: returned %d opportunities, requested at most %d", s.host, skip, len(postings), ukgPageSize)
		}
		if len(postings) == 0 {
			if skip == expectedTotal {
				return jobs, nil
			}
			return nil, fmt.Errorf("ukg %s: empty page at offset %d before reported total %d", s.host, skip, expectedTotal)
		}
		if skip+len(postings) > expectedTotal {
			return nil, fmt.Errorf("ukg %s: page at offset %d would exceed reported total %d", s.host, skip, expectedTotal)
		}

		for index, posting := range postings {
			id, err := canonicalUKGUUID(posting.ID)
			if err != nil {
				return nil, fmt.Errorf("ukg %s page at offset %d row %d: invalid Id: %w", s.host, skip, index, err)
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("ukg %s page at offset %d: duplicate opportunity Id %q", s.host, skip, id)
			}
			seen[id] = struct{}{}

			title := strings.TrimSpace(posting.Title)
			if title == "" {
				return nil, fmt.Errorf("ukg %s opportunity %s: missing Title", s.host, id)
			}
			if posting.FullTime == nil {
				return nil, fmt.Errorf("ukg %s opportunity %s: response omitted FullTime", s.host, id)
			}
			postedAt, err := parsePostingDate(posting.PostedDate)
			if err != nil {
				return nil, fmt.Errorf("ukg %s opportunity %s: %w", s.host, id, err)
			}
			jobs = append(jobs, model.Job{
				ID:             s.jobID(id),
				Company:        s.company,
				Title:          title,
				Location:       ukgLocationText(posting.Locations),
				URL:            s.detailURL(id),
				EmploymentType: ukgEmploymentType(*posting.FullTime),
				PostedAt:       postedAt,
			})
		}

		skip += len(postings)
		if skip == expectedTotal {
			return jobs, nil
		}
		if len(postings) < ukgPageSize {
			return nil, fmt.Errorf("ukg %s: short page of %d at offset %d before reported total %d", s.host, len(postings), skip-len(postings), expectedTotal)
		}
	}
}

func (s *ukg) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("ukg %s: nil job", s.host)
	}
	prefix := s.jobID("")
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("ukg %s: job ID does not belong to this board", s.host)
	}
	id, err := canonicalUKGUUID(strings.TrimPrefix(job.ID, prefix))
	if err != nil {
		return fmt.Errorf("ukg %s: invalid job ID %q: %w", s.host, job.ID, err)
	}

	body, err := fetchHTML(ctx, s.client, s.detailURL(id), customDetailBodyLimit)
	if err != nil {
		return fmt.Errorf("ukg %s opportunity %s detail: %w", s.host, id, err)
	}
	detail, err := parseUKGDetail(body)
	if err != nil {
		return fmt.Errorf("ukg %s opportunity %s detail: %w", s.host, id, err)
	}
	detailID, err := canonicalUKGUUID(detail.ID)
	if err != nil {
		return fmt.Errorf("ukg %s opportunity %s detail: invalid response Id: %w", s.host, id, err)
	}
	if detailID != id {
		return fmt.Errorf("ukg %s opportunity %s detail: response Id is %q", s.host, id, detailID)
	}
	if detail.OpportunityClosed == nil {
		return fmt.Errorf("ukg %s opportunity %s detail: response omitted OpportunityIsClosed", s.host, id)
	}
	if *detail.OpportunityClosed {
		return fmt.Errorf("ukg %s opportunity %s closed before detail fetch", s.host, id)
	}
	title := strings.TrimSpace(detail.Title)
	if title == "" {
		return fmt.Errorf("ukg %s opportunity %s detail: missing Title", s.host, id)
	}
	if detail.FullTime == nil {
		return fmt.Errorf("ukg %s opportunity %s detail: response omitted FullTime", s.host, id)
	}
	description := strings.TrimSpace(htmltext.ToText(detail.Description))
	if description == "" {
		return fmt.Errorf("ukg %s opportunity %s detail: missing Description", s.host, id)
	}

	job.Title = title
	job.Description = description
	job.EmploymentType = ukgEmploymentType(*detail.FullTime)
	job.URL = s.detailURL(id)
	if location := ukgLocationText(detail.Locations); location != "" {
		job.Location = location
	}
	if job.PostedAt.IsZero() {
		postedAt, err := parsePostingDate(detail.PostedDate)
		if err != nil {
			return fmt.Errorf("ukg %s opportunity %s detail: %w", s.host, id, err)
		}
		job.PostedAt = postedAt
	}
	return nil
}

func parseUKGDetail(body []byte) (ukgOpportunity, error) {
	match := ukgDetailRE.FindIndex(body)
	if match == nil {
		return ukgOpportunity{}, fmt.Errorf("page omitted %s JSON", ukgDetailJSONMarker)
	}
	var detail ukgOpportunity
	decoder := json.NewDecoder(bytes.NewReader(body[match[1]:]))
	if err := decoder.Decode(&detail); err != nil {
		return ukgOpportunity{}, fmt.Errorf("decode %s JSON: %w", ukgDetailJSONMarker, err)
	}
	return detail, nil
}

func canonicalUKGUUID(raw string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if !ukgUUIDRE.MatchString(id) {
		return "", fmt.Errorf("expected a canonical UUID, got %q", raw)
	}
	return id, nil
}

func ukgEmploymentType(fullTime bool) string {
	if fullTime {
		return "Full Time"
	}
	return "Part Time"
}

func ukgLocationText(locations []ukgLocation) string {
	names := make([]string, 0, len(locations))
	for _, location := range locations {
		name := firstNonemptyUKG(location.LocalizedName, location.LocalizedDescription)
		if name == "" {
			state := ""
			if location.Address.State != nil {
				state = firstNonemptyUKG(location.Address.State.Name, location.Address.State.Code)
			}
			country := ""
			if location.Address.Country != nil {
				country = firstNonemptyUKG(location.Address.Country.Name, location.Address.Country.Code)
			}
			city := firstNonemptyUKG(location.Address.City, location.Address.Line1)
			name = strings.Join(distinctStrings([]string{
				city, state, location.Address.PostalCode, country,
			}), ", ")
		}
		names = append(names, name)
	}
	return strings.Join(distinctStrings(names), "; ")
}

func firstNonemptyUKG(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (s *ukg) boardPath() string {
	return "/" + url.PathEscape(s.tenant) + "/JobBoard/" + url.PathEscape(s.board)
}

func (s *ukg) searchURL() string {
	return strings.TrimRight(s.baseURL, "/") + s.boardPath() + "/JobBoardView/LoadSearchResults"
}

func (s *ukg) detailURL(id string) string {
	query := url.Values{"opportunityId": {id}}
	return strings.TrimRight(s.baseURL, "/") + s.boardPath() + "/OpportunityDetail?" + query.Encode()
}

func (s *ukg) jobID(id string) string {
	return fmt.Sprintf("ukg/%s/%s/%s/%s", s.host, s.tenant, s.board, id)
}
