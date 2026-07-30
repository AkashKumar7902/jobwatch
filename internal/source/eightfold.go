package source

// Eightfold PCS public careers API (no auth).
//
//	GET https://{host}/api/pcsx/search?domain={domain}&query=&location={location}&start=N
//	GET https://{host}/api/pcsx/position_details?position_id={id}&domain={domain}&hl=en
//
// Search results are paged ten at a time. Full descriptions are deliberately
// fetched lazily through Detail.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const eightfoldPageSize = 10

func init() {
	Register("eightfold", func(company string, p params.Map, client *http.Client) (Source, error) {
		host, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		if err := validateHostParam("host", host); err != nil {
			return nil, err
		}
		domain, err := p.Require("domain")
		if err != nil {
			return nil, err
		}
		if err := validateHostParam("domain", domain); err != nil {
			return nil, err
		}
		maxPostings, err := p.Int("max_postings", 1000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &eightfold{
			company: company, host: host, domain: domain, location: strings.TrimSpace(p.Get("location")),
			base: "https://" + host, maxPostings: maxPostings, client: client,
		}, nil
	})
}

type eightfold struct {
	company     string
	host        string
	domain      string
	location    string
	base        string
	maxPostings int
	client      *http.Client
}

type eightfoldPosition struct {
	ID                    int64    `json:"id"`
	DisplayJobID          string   `json:"displayJobId"`
	Name                  string   `json:"name"`
	Locations             []string `json:"locations"`
	StandardizedLocations []string `json:"standardizedLocations"`
	PostedTS              int64    `json:"postedTs"`
	Department            string   `json:"department"`
	ATSJobID              string   `json:"atsJobId"`
	PositionURL           string   `json:"positionUrl"`
}

type eightfoldSearchResponse struct {
	Status int `json:"status"`
	Error  struct {
		Message string `json:"message"`
		Body    string `json:"body"`
	} `json:"error"`
	Data struct {
		Count     *int                 `json:"count"`
		Positions *[]eightfoldPosition `json:"positions"`
	} `json:"data"`
}

func (s *eightfold) Company() string { return s.company }

func (s *eightfold) Fetch(ctx context.Context) ([]model.Job, error) {
	expectedTotal := -1
	jobs := make([]model.Job, 0)
	seen := make(map[int64]struct{})

	for start := 0; start < s.maxPostings; {
		query := url.Values{
			"domain": {s.domain},
			"query":  {""},
			"start":  {strconv.Itoa(start)},
		}
		if s.location != "" {
			query.Set("location", s.location)
		}
		var page eightfoldSearchResponse
		endpoint := s.base + "/api/pcsx/search?" + query.Encode()
		if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &page); err != nil {
			return nil, fmt.Errorf("eightfold %s: page at start %d: %w", s.host, start, err)
		}
		if page.Status != http.StatusOK {
			return nil, fmt.Errorf("eightfold %s: page at start %d reported status %d (%s)", s.host, start, page.Status, page.Error.Message)
		}
		if page.Data.Count == nil || page.Data.Positions == nil {
			return nil, fmt.Errorf("eightfold %s: page at start %d omitted count or positions", s.host, start)
		}
		if *page.Data.Count < 0 {
			return nil, fmt.Errorf("eightfold %s: page at start %d reported negative count", s.host, start)
		}
		if expectedTotal < 0 {
			expectedTotal = *page.Data.Count
		} else if *page.Data.Count != expectedTotal {
			return nil, fmt.Errorf("eightfold %s: count changed from %d to %d at start %d", s.host, expectedTotal, *page.Data.Count, start)
		}

		positions := *page.Data.Positions
		if len(positions) > eightfoldPageSize {
			return nil, fmt.Errorf("eightfold %s: page at start %d returned %d positions, safety limit is %d", s.host, start, len(positions), eightfoldPageSize)
		}
		if len(positions) == 0 {
			if start < expectedTotal {
				return nil, fmt.Errorf("eightfold %s: empty page at start %d before reported total %d", s.host, start, expectedTotal)
			}
			break
		}

		for _, posting := range positions {
			if len(jobs) >= s.maxPostings {
				break
			}
			job, err := s.normalize(posting)
			if err != nil {
				return nil, fmt.Errorf("eightfold %s: item at raw offset %d: %w", s.host, start, err)
			}
			if _, duplicate := seen[posting.ID]; duplicate {
				return nil, fmt.Errorf("eightfold %s: duplicate position id %d", s.host, posting.ID)
			}
			seen[posting.ID] = struct{}{}
			jobs = append(jobs, job)
		}

		scanned := start + len(positions)
		if scanned >= expectedTotal || len(jobs) >= s.maxPostings {
			break
		}
		if len(positions) < eightfoldPageSize {
			return nil, fmt.Errorf("eightfold %s: short page of %d at start %d before reported total %d", s.host, len(positions), start, expectedTotal)
		}
		start = scanned
	}

	if expectedTotal > s.maxPostings {
		log.Printf("eightfold %s: listing %d of %d postings (max_postings cap)", s.host, len(jobs), expectedTotal)
	}
	if expectedTotal > 0 && len(jobs) == 0 {
		return nil, fmt.Errorf("eightfold %s: reported %d postings but produced none", s.host, expectedTotal)
	}
	return jobs, nil
}

func (s *eightfold) normalize(posting eightfoldPosition) (model.Job, error) {
	if posting.ID <= 0 {
		return model.Job{}, fmt.Errorf("invalid position id %d", posting.ID)
	}
	title := strings.TrimSpace(posting.Name)
	if title == "" {
		return model.Job{}, fmt.Errorf("position %d has empty name", posting.ID)
	}
	idText := strconv.FormatInt(posting.ID, 10)
	positionURL := strings.TrimSpace(posting.PositionURL)
	if positionURL == "" {
		positionURL = "/careers/job/" + idText
	}
	parsed, err := url.Parse(positionURL)
	if err != nil || !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/"+idText) {
		return model.Job{}, fmt.Errorf("position %d has invalid positionUrl %q", posting.ID, posting.PositionURL)
	}
	publicURL, err := resolveReference(s.base+"/", positionURL)
	if err != nil {
		return model.Job{}, fmt.Errorf("position %d URL: %w", posting.ID, err)
	}

	var postedAt time.Time
	if posting.PostedTS > 0 {
		postedAt = time.Unix(posting.PostedTS, 0)
	}
	return model.Job{
		ID:       fmt.Sprintf("eightfold/%s/%s/%s", s.host, s.domain, idText),
		Company:  s.company,
		Title:    title,
		Location: strings.Join(distinctStrings(posting.Locations), "; "),
		URL:      publicURL,
		PostedAt: postedAt,
	}, nil
}

func (s *eightfold) Detail(ctx context.Context, job *model.Job) error {
	prefix := fmt.Sprintf("eightfold/%s/%s/", s.host, s.domain)
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("eightfold %s: job id %q does not have prefix %q", s.host, job.ID, prefix)
	}
	positionID := strings.TrimPrefix(job.ID, prefix)
	numericID, err := strconv.ParseInt(positionID, 10, 64)
	if err != nil || numericID <= 0 {
		return fmt.Errorf("eightfold %s: invalid position id %q", s.host, positionID)
	}
	query := url.Values{"position_id": {positionID}, "domain": {s.domain}, "hl": {"en"}}
	var detail struct {
		Status int `json:"status"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
		Data *struct {
			ID                         int64    `json:"id"`
			Name                       string   `json:"name"`
			Location                   string   `json:"location"`
			Locations                  []string `json:"locations"`
			PostedTS                   int64    `json:"postedTs"`
			JobDescription             string   `json:"jobDescription"`
			PublicURL                  string   `json:"publicUrl"`
			EFCustomTextEmploymentType []string `json:"efcustomTextEmploymentType"`
		} `json:"data"`
	}
	endpoint := s.base + "/api/pcsx/position_details?" + query.Encode()
	if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &detail); err != nil {
		return err
	}
	if detail.Status != http.StatusOK || detail.Data == nil {
		return fmt.Errorf("eightfold %s: detail %s reported status %d (%s)", s.host, positionID, detail.Status, detail.Error.Message)
	}
	if detail.Data.ID != numericID {
		return fmt.Errorf("eightfold %s: detail id %d does not match %d", s.host, detail.Data.ID, numericID)
	}
	description := htmltext.ToText(detail.Data.JobDescription)
	if strings.TrimSpace(description) == "" {
		return fmt.Errorf("eightfold %s: detail %s has empty jobDescription", s.host, positionID)
	}
	job.Description = description
	job.EmploymentType = strings.Join(distinctStrings(detail.Data.EFCustomTextEmploymentType), "; ")
	if locations := distinctStrings(detail.Data.Locations); len(locations) > 0 {
		job.Location = strings.Join(locations, "; ")
	} else if location := strings.TrimSpace(detail.Data.Location); location != "" {
		job.Location = location
	}
	if detail.Data.PostedTS > 0 {
		job.PostedAt = time.Unix(detail.Data.PostedTS, 0)
	}
	if publicURL := strings.TrimSpace(detail.Data.PublicURL); publicURL != "" {
		job.URL = publicURL
	}
	return nil
}
