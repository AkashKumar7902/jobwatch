package source

// Amazon's first-party careers search exposes complete postings as paged
// JSON. This source intentionally scopes the otherwise enormous board by
// normalized ISO-3 country code.
//
//	GET https://www.amazon.jobs/en/search.json
//	    ?normalized_country_code[]={country}&offset=N&result_limit=100

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const amazonPageSize = 100

var amazonCountryRE = regexp.MustCompile(`^[A-Z]{3}$`)

func init() {
	Register("amazon", func(company string, p params.Map, client *http.Client) (Source, error) {
		country := strings.ToUpper(p.GetDefault("country_code", "IND"))
		if !amazonCountryRE.MatchString(country) {
			return nil, fmt.Errorf("param %q: expected ISO-3 country code, got %q", "country_code", country)
		}
		maxPostings, err := p.Int("max_postings", 5000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &amazon{
			company: company, country: country, searchURL: "https://www.amazon.jobs/en/search.json",
			siteBase: "https://www.amazon.jobs", maxPostings: maxPostings, client: client,
		}, nil
	})
}

type amazon struct {
	company     string
	country     string
	searchURL   string
	siteBase    string
	maxPostings int
	client      *http.Client
}

type amazonPosting struct {
	ID                      string `json:"id"`
	IDICIMS                 string `json:"id_icims"`
	Title                   string `json:"title"`
	Location                string `json:"location"`
	NormalizedLocation      string `json:"normalized_location"`
	CountryCode             string `json:"country_code"`
	JobPath                 string `json:"job_path"`
	JobScheduleType         string `json:"job_schedule_type"`
	Description             string `json:"description"`
	BasicQualifications     string `json:"basic_qualifications"`
	PreferredQualifications string `json:"preferred_qualifications"`
	PostedDate              string `json:"posted_date"`
}

type amazonSearchResponse struct {
	Hits *int             `json:"hits"`
	Jobs *[]amazonPosting `json:"jobs"`
}

func (s *amazon) Company() string { return s.company }

func (s *amazon) Fetch(ctx context.Context) ([]model.Job, error) {
	expectedHits := -1
	jobs := make([]model.Job, 0)
	seen := make(map[string]struct{})
	for offset := 0; offset < s.maxPostings; {
		limit := min(amazonPageSize, s.maxPostings-offset)
		query := url.Values{
			"normalized_country_code[]": {s.country},
			"offset":                    {strconv.Itoa(offset)},
			"result_limit":              {strconv.Itoa(limit)},
		}
		var page amazonSearchResponse
		if err := fetchJSON(ctx, s.client, http.MethodGet, s.searchURL+"?"+query.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("amazon %s: page at offset %d: %w", s.country, offset, err)
		}
		if page.Hits == nil || page.Jobs == nil {
			return nil, fmt.Errorf("amazon %s: page at offset %d omitted hits or jobs", s.country, offset)
		}
		if *page.Hits < 0 {
			return nil, fmt.Errorf("amazon %s: page at offset %d reported negative hits", s.country, offset)
		}
		if expectedHits < 0 {
			expectedHits = *page.Hits
		} else if *page.Hits != expectedHits {
			return nil, fmt.Errorf("amazon %s: hits changed from %d to %d at offset %d", s.country, expectedHits, *page.Hits, offset)
		}
		postings := *page.Jobs
		if len(postings) > limit {
			return nil, fmt.Errorf("amazon %s: page at offset %d returned %d jobs, limit is %d", s.country, offset, len(postings), limit)
		}
		if len(postings) == 0 {
			if offset < expectedHits {
				return nil, fmt.Errorf("amazon %s: empty page at offset %d before reported hits %d", s.country, offset, expectedHits)
			}
			break
		}
		for i, posting := range postings {
			job, err := s.normalize(posting)
			if err != nil {
				return nil, fmt.Errorf("amazon %s: item at offset %d: %w", s.country, offset+i, err)
			}
			if _, duplicate := seen[posting.IDICIMS]; duplicate {
				return nil, fmt.Errorf("amazon %s: duplicate id_icims %q", s.country, posting.IDICIMS)
			}
			seen[posting.IDICIMS] = struct{}{}
			jobs = append(jobs, job)
		}
		scanned := offset + len(postings)
		if scanned >= expectedHits || scanned >= s.maxPostings {
			break
		}
		if len(postings) < limit {
			return nil, fmt.Errorf("amazon %s: short page of %d at offset %d before reported hits %d", s.country, len(postings), offset, expectedHits)
		}
		offset = scanned
	}
	if expectedHits > s.maxPostings {
		diagnostic.Cap(ctx, len(jobs), expectedHits)
	}
	if expectedHits > 0 && len(jobs) == 0 {
		return nil, fmt.Errorf("amazon %s: reported %d hits but produced none", s.country, expectedHits)
	}
	return jobs, nil
}

func (s *amazon) normalize(posting amazonPosting) (model.Job, error) {
	id := strings.TrimSpace(posting.IDICIMS)
	if id == "" {
		return model.Job{}, fmt.Errorf("posting omitted id_icims")
	}
	title := strings.TrimSpace(posting.Title)
	if title == "" {
		return model.Job{}, fmt.Errorf("posting %s has empty title", id)
	}
	if strings.ToUpper(strings.TrimSpace(posting.CountryCode)) != s.country {
		return model.Job{}, fmt.Errorf("posting %s country_code %q does not match %s", id, posting.CountryCode, s.country)
	}
	if !strings.Contains(posting.JobPath, "/jobs/"+id+"/") && !strings.HasSuffix(posting.JobPath, "/jobs/"+id) {
		return model.Job{}, fmt.Errorf("posting %s has invalid job_path %q", id, posting.JobPath)
	}
	publicURL, err := resolveReference(s.siteBase+"/", posting.JobPath)
	if err != nil {
		return model.Job{}, fmt.Errorf("posting %s URL: %w", id, err)
	}
	description := joinDescriptionParts(
		htmltext.ToText(posting.Description),
		htmltext.ToText(posting.BasicQualifications),
		htmltext.ToText(posting.PreferredQualifications),
	)
	if description == "" {
		return model.Job{}, fmt.Errorf("posting %s has empty description and qualifications", id)
	}
	location := strings.TrimSpace(posting.NormalizedLocation)
	if location == "" {
		location = strings.TrimSpace(posting.Location)
	}
	var postedAt time.Time
	if posted := strings.TrimSpace(posting.PostedDate); posted != "" {
		postedAt, err = time.Parse("January 2, 2006", posted)
		if err != nil {
			return model.Job{}, fmt.Errorf("posting %s has invalid posted_date %q", id, posted)
		}
	}
	return model.Job{
		ID:             fmt.Sprintf("amazon/%s/%s", s.country, id),
		Company:        s.company,
		Title:          title,
		Location:       location,
		URL:            publicURL,
		EmploymentType: strings.TrimSpace(posting.JobScheduleType),
		Description:    description,
		PostedAt:       postedAt,
	}, nil
}
