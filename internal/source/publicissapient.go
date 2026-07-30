package source

// Publicis Sapient exposes an anonymous India job-search JSON endpoint. Full
// descriptions live in structured data on each first-party detail page, so
// they are fetched lazily.

import (
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	publicisSapientPageSize     = 100
	publicisSapientMaxDocuments = 10_000
	publicisSapientApplyHost    = "sapient-publicisgroupe.icims.com"
)

var (
	publicisSapientDetailPathRE = regexp.MustCompile(`^/job-details/[A-Za-z0-9][A-Za-z0-9-]*$`)
	publicisSapientApplyPathRE  = regexp.MustCompile(`^/jobs/[1-9][0-9]*/job/login$`)
	publicisSapientJobIDRE      = regexp.MustCompile(`^[0-9]{4}-[1-9][0-9]*$`)
)

func init() {
	Register("publicissapient", func(company string, p params.Map, client *http.Client) (Source, error) {
		maxPostings, err := p.Int("max_postings", 1000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		if maxPostings > publicisSapientMaxDocuments {
			return nil, fmt.Errorf(
				"param %q: expected at most %d, got %d",
				"max_postings", publicisSapientMaxDocuments, maxPostings,
			)
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &publicisSapient{
			company: company, baseURL: "https://careers.publicissapient.com",
			country: "India", maxPostings: maxPostings, client: client,
		}, nil
	})
}

type publicisSapient struct {
	company     string
	baseURL     string
	country     string
	maxPostings int
	client      *http.Client
}

func (s *publicisSapient) Company() string { return s.company }

type publicisSapientPosting struct {
	ID               string `json:"id"`
	JobID            string `json:"jobId"`
	Name             string `json:"name"`
	DisplayLocation  string `json:"displayLocation"`
	CountryName      string `json:"countryName"`
	JobDetailURL     string `json:"jobDetailUrl"`
	TypeOfEmployment string `json:"typeOfEmployment"`
	ReleasedDate     string `json:"releasedDate"`
}

func (s *publicisSapient) Fetch(ctx context.Context) ([]model.Job, error) {
	var postings []publicisSapientPosting
	total := -1
	for start := 0; start < publicisSapientMaxDocuments; {
		rows := min(publicisSapientPageSize, publicisSapientMaxDocuments-start)
		query := url.Values{
			"searchType":  {"/search"},
			"lang":        {"en"},
			"facetFields": {"countryName,city,teams,experienceLevel,remote,typeOfEmployment"},
			"q":           {""},
			"start":       {fmt.Sprint(start)},
			"rows":        {fmt.Sprint(rows)},
			"country":     {s.country},
		}
		var page struct {
			Response *struct {
				NumFound int                      `json:"numFound"`
				Start    int                      `json:"start"`
				Docs     []publicisSapientPosting `json:"docs"`
			} `json:"response"`
		}
		endpoint := s.baseURL + "/bin/ps-redesign/careersJobsearch?" + query.Encode()
		if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &page); err != nil {
			return nil, err
		}
		if page.Response == nil {
			return nil, fmt.Errorf("publicissapient: response omitted search response")
		}
		if page.Response.Start != start {
			return nil, fmt.Errorf("publicissapient: requested start %d, response reported %d", start, page.Response.Start)
		}
		if page.Response.NumFound < 0 {
			return nil, fmt.Errorf("publicissapient: negative numFound %d", page.Response.NumFound)
		}
		if page.Response.NumFound > publicisSapientMaxDocuments {
			return nil, fmt.Errorf(
				"publicissapient: numFound %d exceeds %d-document safety limit",
				page.Response.NumFound, publicisSapientMaxDocuments,
			)
		}
		if total < 0 {
			total = page.Response.NumFound
		} else if page.Response.NumFound != total {
			return nil, fmt.Errorf("publicissapient: numFound changed from %d to %d", total, page.Response.NumFound)
		}
		if len(page.Response.Docs) == 0 {
			if len(postings) < total {
				return nil, fmt.Errorf("publicissapient: pagination ended after %d of %d postings", len(postings), total)
			}
			break
		}
		postings = append(postings, page.Response.Docs...)
		if len(postings) > total {
			return nil, fmt.Errorf(
				"publicissapient: collected %d documents, exceeding declared total %d",
				len(postings), total,
			)
		}
		if len(postings) == total {
			break
		}
		start += len(page.Response.Docs)
	}
	if len(postings) != total {
		return nil, fmt.Errorf(
			"publicissapient: collected %d documents, want declared total %d",
			len(postings), total,
		)
	}

	type aggregate struct {
		jobID, title, detailPath, employmentType, releasedDate string
		postedAt                                               time.Time
		locations                                              map[string]struct{}
	}
	aggregates := make(map[string]*aggregate, len(postings))
	order := make([]string, 0, min(len(postings), s.maxPostings))
	seenDocumentIDs := make(map[string]struct{}, len(postings))
	for i, posting := range postings {
		jobID := strings.TrimSpace(posting.JobID)
		if !publicisSapientJobIDRE.MatchString(jobID) {
			return nil, fmt.Errorf("publicissapient: item %d has invalid jobId %q", i, jobID)
		}
		documentID := strings.TrimSpace(posting.ID)
		if documentID == "" {
			return nil, fmt.Errorf("publicissapient: item %d jobId %q omitted id", i, jobID)
		}
		if documentID != jobID {
			locationID := strings.TrimPrefix(documentID, jobID+"-")
			if locationID == documentID || locationID == "" {
				return nil, fmt.Errorf(
					"publicissapient: item %d id %q does not belong to jobId %q",
					i, documentID, jobID,
				)
			}
			for _, digit := range locationID {
				if digit < '0' || digit > '9' {
					return nil, fmt.Errorf(
						"publicissapient: item %d id %q has an invalid location suffix",
						i, documentID,
					)
				}
			}
		}
		if _, duplicate := seenDocumentIDs[documentID]; duplicate {
			return nil, fmt.Errorf("publicissapient: duplicate document id %q", documentID)
		}
		seenDocumentIDs[documentID] = struct{}{}
		title := strings.TrimSpace(posting.Name)
		if title == "" {
			return nil, fmt.Errorf("publicissapient: item %d has an empty name", i)
		}
		countryName := strings.TrimSpace(posting.CountryName)
		if countryName != s.country {
			return nil, fmt.Errorf(
				"publicissapient: item %d jobId %q has countryName %q, want %q",
				i, jobID, countryName, s.country,
			)
		}
		location := strings.TrimSpace(posting.DisplayLocation)
		if location == "" {
			return nil, fmt.Errorf("publicissapient: item %d jobId %q has an empty displayLocation", i, jobID)
		}
		detailPath := strings.TrimSpace(posting.JobDetailURL)
		if !publicisSapientDetailPathRE.MatchString(detailPath) {
			return nil, fmt.Errorf("publicissapient: item %d has invalid detail path %q", i, detailPath)
		}
		employmentType := strings.TrimSpace(posting.TypeOfEmployment)
		releasedDate := strings.TrimSpace(posting.ReleasedDate)
		if releasedDate == "" {
			return nil, fmt.Errorf("publicissapient: item %d jobId %q has an empty releasedDate", i, jobID)
		}
		postedAt, err := time.Parse(time.RFC3339, releasedDate)
		if err != nil {
			return nil, fmt.Errorf(
				"publicissapient: item %d jobId %q has invalid releasedDate %q: %w",
				i, jobID, releasedDate, err,
			)
		}
		existing := aggregates[jobID]
		if existing == nil {
			existing = &aggregate{
				jobID: jobID, title: title, employmentType: employmentType, releasedDate: releasedDate,
				postedAt: postedAt, locations: map[string]struct{}{},
			}
			aggregates[jobID] = existing
			order = append(order, jobID)
		} else if existing.title != title ||
			existing.employmentType != employmentType ||
			existing.releasedDate != releasedDate {
			return nil, fmt.Errorf(
				"publicissapient: location documents for jobId %q have inconsistent shared fields",
				jobID,
			)
		}
		if documentID == jobID {
			if existing.detailPath != "" {
				return nil, fmt.Errorf(
					"publicissapient: jobId %q has multiple canonical documents",
					jobID,
				)
			}
			existing.detailPath = detailPath
		}
		existing.locations[location] = struct{}{}
	}

	for _, jobID := range order {
		if aggregates[jobID].detailPath == "" {
			return nil, fmt.Errorf("publicissapient: jobId %q omitted its canonical document", jobID)
		}
	}
	if len(order) > s.maxPostings {
		order = order[:s.maxPostings]
	}
	jobs := make([]model.Job, 0, len(order))
	for _, jobID := range order {
		posting := aggregates[jobID]
		locations := make([]string, 0, len(posting.locations))
		for location := range posting.locations {
			locations = append(locations, location)
		}
		sort.Strings(locations)
		jobs = append(jobs, model.Job{
			ID:             "publicissapient/" + posting.jobID,
			Company:        s.company,
			Title:          posting.title,
			Location:       strings.Join(locations, "; "),
			URL:            strings.TrimRight(s.baseURL, "/") + posting.detailPath,
			EmploymentType: posting.employmentType,
			PostedAt:       posting.postedAt,
		})
	}
	return jobs, nil
}

var publicisSapientPropsRE = regexp.MustCompile(`(?s)<div[^>]*class="job-details"[^>]*data-react-props='([^']+)'`)

func (s *publicisSapient) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("publicissapient: nil job")
	}
	detailURL, err := validatePublicisSapientDetailURL(s.baseURL, job.URL)
	if err != nil {
		return fmt.Errorf("publicissapient: invalid detail URL: %w", err)
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
		return fmt.Errorf("publicissapient: detail %s omitted final request URL", detailURL)
	}
	finalURL, err := validatePublicisSapientDetailURL(s.baseURL, resp.Request.URL.String())
	if err != nil || finalURL != detailURL {
		return fmt.Errorf("publicissapient: detail %s resolved to an untrusted URL", detailURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	match := publicisSapientPropsRE.FindSubmatch(body)
	if len(match) != 2 {
		return fmt.Errorf("publicissapient: no job-details structured data found at %s", detailURL)
	}
	var detail struct {
		TypeOfEmployment string `json:"typeOfEmployment"`
		PrimaryCTA       struct {
			Link string `json:"ctaLinkUrl"`
		} `json:"primaryCta"`
		Sections []struct {
			Title string
			Body  string
		}
		JobDescription struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"jobDescriptionSection"`
		Qualifications struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"qualificationDescriptionSection"`
		Additional struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"additionalInformationSection"`
		CompanyDetails struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"companyDetailsSection"`
	}
	decoded := stdhtml.UnescapeString(string(match[1]))
	if err := json.Unmarshal([]byte(decoded), &detail); err != nil {
		return fmt.Errorf("publicissapient: decoding job details: %w", err)
	}
	applyURL, err := validatePublicisSapientApplyURL(detail.PrimaryCTA.Link)
	if err != nil {
		return fmt.Errorf("publicissapient: invalid primary CTA: %w", err)
	}
	detail.Sections = append(detail.Sections,
		struct{ Title, Body string }{detail.JobDescription.Title, detail.JobDescription.Body},
		struct{ Title, Body string }{detail.Qualifications.Title, detail.Qualifications.Body},
		struct{ Title, Body string }{detail.Additional.Title, detail.Additional.Body},
		struct{ Title, Body string }{detail.CompanyDetails.Title, detail.CompanyDetails.Body},
	)
	var parts []string
	for _, section := range detail.Sections {
		text := htmltext.ToText(section.Body)
		if text == "" {
			continue
		}
		if section.Title != "" {
			text = strings.TrimSpace(section.Title) + "\n" + text
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return fmt.Errorf("publicissapient: structured data at %s has no description sections", detailURL)
	}
	description := strings.Join(parts, "\n\n")
	employmentType := job.EmploymentType
	if detail.TypeOfEmployment != "" {
		employmentType = strings.TrimSpace(detail.TypeOfEmployment)
	}
	job.Description = description
	job.EmploymentType = employmentType
	job.URL = applyURL
	return nil
}

func validatePublicisSapientDetailURL(baseURL, raw string) (string, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || !base.IsAbs() || base.Opaque != "" || base.User != nil ||
		base.Scheme == "" || base.Host == "" ||
		(base.Path != "" && base.Path != "/") ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return "", fmt.Errorf("invalid configured base URL")
	}
	detail, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !detail.IsAbs() || detail.Opaque != "" || detail.User != nil ||
		!strings.EqualFold(detail.Scheme, base.Scheme) ||
		!strings.EqualFold(detail.Host, base.Host) ||
		detail.RawPath != "" ||
		!publicisSapientDetailPathRE.MatchString(detail.Path) ||
		detail.RawQuery != "" || detail.ForceQuery || detail.Fragment != "" {
		return "", fmt.Errorf("unexpected detail URL %q", raw)
	}
	return detail.String(), nil
}

func validatePublicisSapientApplyURL(raw string) (string, error) {
	applyURL, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !applyURL.IsAbs() || applyURL.Opaque != "" || applyURL.User != nil ||
		!strings.EqualFold(applyURL.Scheme, "https") ||
		!strings.EqualFold(applyURL.Host, publicisSapientApplyHost) ||
		applyURL.RawPath != "" ||
		!publicisSapientApplyPathRE.MatchString(applyURL.Path) ||
		applyURL.RawQuery != "" || applyURL.ForceQuery || applyURL.Fragment != "" {
		return "", fmt.Errorf("unexpected apply URL %q", raw)
	}
	return applyURL.String(), nil
}
