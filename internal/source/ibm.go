package source

// IBM's public search service exposes complete job descriptions in a paged
// anonymous JSON response. appid and scope identify the search collection;
// rc is IBM's two-letter country filter.
//
//	GET https://www-api.ibm.com/search/api/v1/ibmcom/appid/{appid}/responseFormat/json
//	    ?appid={appid}&scope={scope}&rc={rc}&rmdt=ALL&query=&fr=N&nr=30

import (
	"context"
	"encoding/json"
	"fmt"
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
	ibmAPIBase  = "https://www-api.ibm.com"
	ibmPageSize = 30
	ibmMaxJobs  = 100000
)

var (
	ibmCollectionRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	ibmCountryRE    = regexp.MustCompile(`^[a-z]{2}$`)
	ibmJobIDRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func init() {
	Register("ibm", func(company string, p params.Map, client *http.Client) (Source, error) {
		appID, err := ibmRequiredCollectionParam(p, "appid")
		if err != nil {
			return nil, err
		}
		scope, err := ibmRequiredCollectionParam(p, "scope")
		if err != nil {
			return nil, err
		}
		rc, err := p.Require("rc")
		if err != nil {
			return nil, err
		}
		if !ibmCountryRE.MatchString(rc) {
			return nil, fmt.Errorf("param %q: expected a lowercase two-letter country code, got %q", "rc", rc)
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &ibm{
			company: company,
			appID:   appID,
			scope:   scope,
			rc:      rc,
			apiBase: ibmAPIBase,
			client:  client,
		}, nil
	})
}

func ibmRequiredCollectionParam(p params.Map, key string) (string, error) {
	value, err := p.Require(key)
	if err != nil {
		return "", err
	}
	if !ibmCollectionRE.MatchString(value) {
		return "", fmt.Errorf("param %q: expected an IBM collection identifier, got %q", key, value)
	}
	return value, nil
}

type ibm struct {
	company string
	appID   string
	scope   string
	rc      string
	apiBase string
	client  *http.Client
}

type ibmSearchResponse struct {
	ResultSet *ibmResultSet `json:"resultset"`
}

type ibmResultSet struct {
	SearchQuery   *ibmSearchQuery   `json:"searchquery"`
	SearchResults *ibmSearchResults `json:"searchresults"`
}

type ibmSearchQuery struct {
	AppID *string `json:"appid"`
}

type ibmSearchResults struct {
	TotalResults     *int               `json:"totalresults"`
	StartIndex       *int               `json:"startindex"`
	NumResults       *int               `json:"numresults"`
	SearchResultList *[]ibmSearchResult `json:"searchresultlist"`
}

type ibmSearchResult struct {
	ResultNum     *int                          `json:"resultnum"`
	ID            string                        `json:"id"`
	Title         string                        `json:"title"`
	URL           string                        `json:"url"`
	DocAttributes *[]map[string]json.RawMessage `json:"docattributes"`
}

func (s *ibm) Company() string { return s.company }

func (s *ibm) Fetch(ctx context.Context) ([]model.Job, error) {
	jobs := make([]model.Job, 0)
	seen := make(map[string]struct{})
	expectedTotal := -1

	for offset := 0; ; {
		page, err := s.fetchPage(ctx, offset)
		if err != nil {
			return nil, err
		}
		results, total, err := s.validatePage(page, offset, expectedTotal)
		if err != nil {
			return nil, err
		}
		if expectedTotal < 0 {
			expectedTotal = total
			if expectedTotal > ibmMaxJobs {
				return nil, fmt.Errorf("ibm %s/%s/%s: totalresults %d exceeds hard limit %d", s.appID, s.scope, s.rc, expectedTotal, ibmMaxJobs)
			}
			jobs = make([]model.Job, 0, expectedTotal)
		}

		for index, result := range results {
			job, externalID, err := s.normalize(result)
			if err != nil {
				return nil, fmt.Errorf("ibm %s/%s/%s: result at offset %d: %w", s.appID, s.scope, s.rc, offset+index, err)
			}
			if _, duplicate := seen[externalID]; duplicate {
				return nil, fmt.Errorf("ibm %s/%s/%s: duplicate job id %q", s.appID, s.scope, s.rc, externalID)
			}
			seen[externalID] = struct{}{}
			jobs = append(jobs, job)
		}

		offset += len(results)
		if offset == expectedTotal {
			return jobs, nil
		}
		if offset > expectedTotal {
			return nil, fmt.Errorf("ibm %s/%s/%s: collected %d jobs, exceeding totalresults %d", s.appID, s.scope, s.rc, offset, expectedTotal)
		}
	}
}

func (s *ibm) fetchPage(ctx context.Context, offset int) (ibmSearchResponse, error) {
	query := url.Values{
		"appid": {s.appID},
		"scope": {s.scope},
		"rc":    {s.rc},
		"rmdt":  {"ALL"},
		"query": {""},
		"fr":    {strconv.Itoa(offset)},
		"nr":    {strconv.Itoa(ibmPageSize)},
	}
	endpoint := strings.TrimRight(s.apiBase, "/") +
		"/search/api/v1/ibmcom/appid/" + url.PathEscape(s.appID) + "/responseFormat/json?" + query.Encode()
	var page ibmSearchResponse
	if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &page); err != nil {
		return ibmSearchResponse{}, fmt.Errorf("ibm %s/%s/%s: page at offset %d: %w", s.appID, s.scope, s.rc, offset, err)
	}
	return page, nil
}

func (s *ibm) validatePage(page ibmSearchResponse, offset, expectedTotal int) ([]ibmSearchResult, int, error) {
	prefix := fmt.Sprintf("ibm %s/%s/%s: page at offset %d", s.appID, s.scope, s.rc, offset)
	if page.ResultSet == nil || page.ResultSet.SearchQuery == nil || page.ResultSet.SearchResults == nil {
		return nil, 0, fmt.Errorf("%s omitted resultset, searchquery, or searchresults", prefix)
	}
	query := page.ResultSet.SearchQuery
	if query.AppID == nil || *query.AppID != s.appID {
		return nil, 0, fmt.Errorf("%s reported appid %q, want %q", prefix, ibmStringValue(query.AppID), s.appID)
	}
	search := page.ResultSet.SearchResults
	if search.TotalResults == nil || search.StartIndex == nil || search.NumResults == nil || search.SearchResultList == nil {
		return nil, 0, fmt.Errorf("%s omitted totalresults, startindex, numresults, or searchresultlist", prefix)
	}
	total := *search.TotalResults
	if total < 0 {
		return nil, 0, fmt.Errorf("%s reported negative totalresults %d", prefix, total)
	}
	if expectedTotal >= 0 && total != expectedTotal {
		return nil, 0, fmt.Errorf("%s totalresults changed from %d to %d", prefix, expectedTotal, total)
	}
	if *search.StartIndex != offset {
		return nil, 0, fmt.Errorf("%s reported startindex %d", prefix, *search.StartIndex)
	}
	results := *search.SearchResultList
	if *search.NumResults != len(results) {
		return nil, 0, fmt.Errorf("%s reported numresults %d but returned %d results", prefix, *search.NumResults, len(results))
	}
	if len(results) > ibmPageSize {
		return nil, 0, fmt.Errorf("%s returned %d results, page size is %d", prefix, len(results), ibmPageSize)
	}
	if offset > total {
		return nil, 0, fmt.Errorf("%s starts beyond totalresults %d", prefix, total)
	}
	want := min(ibmPageSize, total-offset)
	if len(results) != want {
		return nil, 0, fmt.Errorf("%s returned %d results, want %d for totalresults %d", prefix, len(results), want, total)
	}
	for index, result := range results {
		if result.ResultNum == nil || *result.ResultNum != index {
			return nil, 0, fmt.Errorf("%s result %d reported resultnum %s", prefix, index, ibmIntValue(result.ResultNum))
		}
	}
	return results, total, nil
}

func (s *ibm) normalize(result ibmSearchResult) (model.Job, string, error) {
	externalID, err := ibmAttribute(result.DocAttributes, "field_text_01")
	if err != nil {
		return model.Job{}, "", err
	}
	externalID = strings.TrimSpace(externalID)
	if !ibmJobIDRE.MatchString(externalID) {
		return model.Job{}, "", fmt.Errorf("invalid field_text_01 job id %q", externalID)
	}
	title := strings.TrimSpace(result.Title)
	if title == "" {
		return model.Job{}, "", fmt.Errorf("job %s has empty title", externalID)
	}
	country, err := ibmAttribute(result.DocAttributes, "country")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", externalID, err)
	}
	if strings.TrimSpace(country) != s.rc {
		return model.Job{}, "", fmt.Errorf("job %s has country %q, want %q", externalID, country, s.rc)
	}
	scope, err := ibmAttribute(result.DocAttributes, "scope")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", externalID, err)
	}
	if strings.TrimSpace(scope) != s.scope {
		return model.Job{}, "", fmt.Errorf("job %s has scope %q, want %q", externalID, scope, s.scope)
	}
	location, err := ibmAttribute(result.DocAttributes, "field_keyword_19")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", externalID, err)
	}
	location = strings.TrimSpace(location)
	if location == "" {
		return model.Job{}, "", fmt.Errorf("job %s has empty field_keyword_19 location", externalID)
	}
	rawBody, err := ibmAttribute(result.DocAttributes, "raw_body")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", externalID, err)
	}
	description := htmltext.ToText(rawBody)
	if description == "" {
		return model.Job{}, "", fmt.Errorf("job %s has empty raw_body description", externalID)
	}
	posted, err := ibmAttribute(result.DocAttributes, "dcdate")
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s: %w", externalID, err)
	}
	postedAt, err := time.Parse("2006-01-02", strings.TrimSpace(posted))
	if err != nil {
		return model.Job{}, "", fmt.Errorf("job %s has invalid dcdate %q", externalID, posted)
	}
	publicURL, err := url.Parse(strings.TrimSpace(result.URL))
	if err != nil || publicURL.Scheme != "https" || !strings.EqualFold(publicURL.Hostname(), "careers.ibm.com") ||
		publicURL.Query().Get("jobId") != externalID {
		return model.Job{}, "", fmt.Errorf("job %s has invalid URL %q", externalID, result.URL)
	}

	return model.Job{
		ID:          fmt.Sprintf("ibm/%s/%s/%s/%s", s.appID, s.scope, s.rc, externalID),
		Company:     s.company,
		Title:       title,
		Location:    location,
		URL:         publicURL.String(),
		Description: description,
		PostedAt:    postedAt,
	}, externalID, nil
}

func ibmAttribute(attributes *[]map[string]json.RawMessage, key string) (string, error) {
	if attributes == nil {
		return "", fmt.Errorf("omitted docattributes")
	}
	var (
		value string
		found bool
	)
	for _, attribute := range *attributes {
		raw, ok := attribute[key]
		if !ok {
			continue
		}
		if found {
			return "", fmt.Errorf("docattributes contains duplicate %q", key)
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("docattribute %q is not a string", key)
		}
		found = true
	}
	if !found {
		return "", fmt.Errorf("docattributes omitted %q", key)
	}
	return value, nil
}

func ibmStringValue(value *string) string {
	if value == nil {
		return "<missing>"
	}
	return *value
}

func ibmIntValue(value *int) string {
	if value == nil {
		return "<missing>"
	}
	return strconv.Itoa(*value)
}
