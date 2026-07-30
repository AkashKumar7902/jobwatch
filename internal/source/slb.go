package source

// SLB's first-party careers page uses an anonymous Coveo search index. The
// short-lived public search token is embedded in /job-listing, then Coveo
// returns the India-scoped active postings. Full descriptions are fetched
// lazily from SLB's own detail pages.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	slbPageSize        = 100
	slbMaxPostings     = 10_000
	slbDetailBodyLimit = 4 << 20
)

func init() {
	Register("slb", func(company string, p params.Map, client *http.Client) (Source, error) {
		maxPostings, err := positiveCappedParam(p, "max_postings", 1000, slbMaxPostings)
		if err != nil {
			return nil, err
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &slb{
			company: company, baseURL: "https://careers.slb.com",
			searchURL: "https://platform.cloud.coveo.com/rest/search/v2",
			country:   "India", maxPostings: maxPostings, client: client,
		}, nil
	})
}

type slb struct {
	company     string
	baseURL     string
	searchURL   string
	country     string
	maxPostings int
	client      *http.Client
}

func (s *slb) Company() string { return s.company }

var (
	slbOrgRE          = regexp.MustCompile(`id="organizationId"\s+value="([^"]+)"`)
	slbTokenRE        = regexp.MustCompile(`id="accessToken"\s+value="([^"]+)"`)
	slbSearchHubRE    = regexp.MustCompile(`id="searchHub"\s+value="([^"]+)"`)
	slbSearchSourceRE = regexp.MustCompile(`id="searchsource"\s+value="([^"]+)"`)
	slbDescriptionRE  = regexp.MustCompile(`(?s)<section class="job-description-redesign[^>]*>(.*?)</section>`)
	slbPermanentIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

type slbSearchConfig struct {
	Organization string
	Token        string
	SearchHub    string
	Source       string
}

type slbResult struct {
	Title    string `json:"title"`
	ClickURI string `json:"clickUri"`
	Raw      struct {
		PermanentID string   `json:"permanentid"`
		Country     []string `json:"country"`
		City        string   `json:"city"`
		Date        int64    `json:"date"`
	} `json:"raw"`
}

func (s *slb) Fetch(ctx context.Context) ([]model.Job, error) {
	config, err := s.searchConfig(ctx)
	if err != nil {
		return nil, err
	}
	var results []slbResult
	total := -1
	for first := 0; first < s.maxPostings; {
		number := min(slbPageSize, s.maxPostings-first)
		request := map[string]any{
			"q":               "",
			"searchHub":       config.SearchHub,
			"pipeline":        "ATSJobsPipeline",
			"cq":              `@source==$"` + config.Source + `"`,
			"aq":              `@country=="` + s.country + `"`,
			"firstResult":     first,
			"numberOfResults": number,
			"fieldsToInclude": []string{"title", "date", "country", "city", "category"},
		}
		var page struct {
			TotalCount         *int         `json:"totalCount"`
			TotalCountFiltered *int         `json:"totalCountFiltered"`
			Results            *[]slbResult `json:"results"`
		}
		endpoint := s.searchURL + "?organizationId=" + url.QueryEscape(config.Organization)
		if err := s.search(ctx, endpoint, config.Token, request, &page); err != nil {
			return nil, err
		}
		if page.Results == nil || (page.TotalCount == nil && page.TotalCountFiltered == nil) {
			return nil, fmt.Errorf("slb: Coveo response omitted results or total")
		}
		pageTotal := 0
		if page.TotalCountFiltered != nil {
			pageTotal = *page.TotalCountFiltered
		} else {
			pageTotal = *page.TotalCount
		}
		if pageTotal < 0 {
			return nil, fmt.Errorf("slb: Coveo returned negative total %d", pageTotal)
		}
		if total < 0 {
			total = pageTotal
		} else if pageTotal != total {
			return nil, fmt.Errorf("slb: Coveo total changed from %d to %d", total, pageTotal)
		}
		pageResults := *page.Results
		if len(pageResults) > number {
			return nil, fmt.Errorf(
				"slb: Coveo returned %d results at offset %d, requested at most %d",
				len(pageResults), first, number,
			)
		}
		target := min(total, s.maxPostings)
		if len(results)+len(pageResults) > target {
			return nil, fmt.Errorf(
				"slb: Coveo returned %d collected results, exceeding declared target %d",
				len(results)+len(pageResults), target,
			)
		}
		if len(pageResults) == 0 {
			if len(results) < min(total, s.maxPostings) {
				return nil, fmt.Errorf("slb: pagination ended after %d of %d results", len(results), total)
			}
			break
		}
		results = append(results, pageResults...)
		if len(results) == target {
			break
		}
		first += len(pageResults)
	}
	target := min(total, s.maxPostings)
	if len(results) != target {
		return nil, fmt.Errorf(
			"slb: collected %d results, want %d from declared total %d",
			len(results), target, total,
		)
	}

	jobs := make([]model.Job, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for i, result := range results {
		id := strings.TrimSpace(result.Raw.PermanentID)
		title := strings.TrimSpace(result.Title)
		if !slbPermanentIDRE.MatchString(id) || title == "" {
			return nil, fmt.Errorf("slb: result %d omitted permanentid or title", i)
		}
		inCountry := false
		for _, country := range result.Raw.Country {
			if strings.TrimSpace(country) == s.country {
				inCountry = true
				break
			}
		}
		if !inCountry {
			return nil, fmt.Errorf(
				"slb: result %d permanentid %q does not include country %q",
				i, id, s.country,
			)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("slb: duplicate permanentid %q", id)
		}
		seen[id] = struct{}{}
		clickURI, err := validateSLBDetailURL(s.baseURL, result.ClickURI)
		if err != nil {
			return nil, fmt.Errorf("slb: result %d: %w", i, err)
		}
		var locationParts []string
		locationParts = append(locationParts, result.Raw.Country...)
		if result.Raw.City != "" {
			locationParts = append(locationParts, result.Raw.City)
		}
		var postedAt time.Time
		if result.Raw.Date > 0 {
			postedAt = time.UnixMilli(result.Raw.Date)
		}
		jobs = append(jobs, model.Job{
			ID:       "slb/" + id,
			Company:  s.company,
			Title:    title,
			Location: strings.Join(distinctStrings(locationParts), ", "),
			URL:      clickURI,
			PostedAt: postedAt,
		})
	}
	return jobs, nil
}

func (s *slb) searchConfig(ctx context.Context) (slbSearchConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/job-listing", nil)
	if err != nil {
		return slbSearchConfig{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := clientWithoutRedirects(s.client).Do(req)
	if err != nil {
		return slbSearchConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return slbSearchConfig{}, fmt.Errorf("GET %s/job-listing: %s", s.baseURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return slbSearchConfig{}, err
	}
	value := func(re *regexp.Regexp) string {
		match := re.FindSubmatch(body)
		if len(match) != 2 {
			return ""
		}
		return strings.TrimSpace(string(match[1]))
	}
	config := slbSearchConfig{
		Organization: value(slbOrgRE),
		Token:        value(slbTokenRE),
		SearchHub:    value(slbSearchHubRE),
		Source:       value(slbSearchSourceRE),
	}
	if config.Organization == "" || config.Token == "" || config.SearchHub == "" || config.Source == "" {
		return slbSearchConfig{}, fmt.Errorf("slb: job-listing omitted Coveo search configuration")
	}
	return config, nil
}

func (s *slb) search(ctx context.Context, endpoint, token string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := clientWithoutRedirects(s.client).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("POST %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
		return fmt.Errorf("decoding Coveo response: %w", err)
	}
	return nil
}

func (s *slb) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("slb: nil job")
	}
	if !strings.HasPrefix(job.ID, "slb/") ||
		!slbPermanentIDRE.MatchString(strings.TrimPrefix(job.ID, "slb/")) {
		return fmt.Errorf("slb: invalid job ID %q", job.ID)
	}
	detailURL, err := validateSLBDetailURL(s.baseURL, job.URL)
	if err != nil {
		return fmt.Errorf("slb: invalid detail URL for %q: %w", job.ID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := clientWithoutRedirects(s.client).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("detail %s: %s", detailURL, resp.Status)
	}
	if resp.Request == nil || resp.Request.URL == nil {
		return fmt.Errorf("slb: detail %s omitted final request URL", detailURL)
	}
	finalURL, err := validateSLBDetailURL(s.baseURL, resp.Request.URL.String())
	if err != nil || finalURL != detailURL {
		return fmt.Errorf("slb: detail %s redirected to an untrusted URL", detailURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, slbDetailBodyLimit+1))
	if err != nil {
		return err
	}
	if len(body) > slbDetailBodyLimit {
		return fmt.Errorf("slb: detail %s exceeds %d-byte safety limit", detailURL, slbDetailBodyLimit)
	}
	match := slbDescriptionRE.FindSubmatch(body)
	if len(match) != 2 {
		return fmt.Errorf("slb: no job description section found at %s", detailURL)
	}
	description := htmltext.ToText(string(match[1]))
	if description == "" {
		return fmt.Errorf("slb: empty job description at %s", detailURL)
	}
	job.Description = description
	return nil
}

func validateSLBDetailURL(baseURL, raw string) (string, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return "", fmt.Errorf("invalid configured base URL")
	}
	raw = strings.ReplaceAll(strings.TrimSpace(raw), " ", "%20")
	detail, err := url.Parse(raw)
	if err != nil || detail.User != nil || detail.Scheme != base.Scheme ||
		!strings.EqualFold(detail.Host, base.Host) ||
		!strings.EqualFold(detail.EscapedPath(), "/jobdescription.aspx") ||
		detail.Fragment != "" || detail.ForceQuery {
		return "", fmt.Errorf("unexpected detail URL %q", raw)
	}
	query := detail.Query()
	ids, ok := query["id"]
	if !ok || len(query) != 1 || len(ids) != 1 || strings.TrimSpace(ids[0]) == "" {
		return "", fmt.Errorf("detail URL %q must contain exactly one non-empty id", raw)
	}
	encodedID := strings.ReplaceAll(url.QueryEscape(ids[0]), "+", "%20")
	detail.RawQuery = "id=" + encodedID
	return detail.String(), nil
}
