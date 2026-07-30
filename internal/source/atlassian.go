package source

// Atlassian publishes its complete global job set as one first-party JSON
// array. Each item already includes the full overview, responsibilities, and
// qualifications.
//
//	GET https://www.atlassian.com/endpoint/careers/listings

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("atlassian", func(company string, p params.Map, client *http.Client) (Source, error) {
		maxPostings, err := p.Int("max_postings", 1000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &atlassian{
			company: company, endpoint: "https://www.atlassian.com/endpoint/careers/listings",
			detailBase: "https://www.atlassian.com/company/careers/details", maxPostings: maxPostings, client: client,
		}, nil
	})
}

type atlassian struct {
	company     string
	endpoint    string
	detailBase  string
	maxPostings int
	client      *http.Client
}

type atlassianPosting struct {
	ID               int64    `json:"id"`
	PortalID         int64    `json:"portalId"`
	Title            string   `json:"title"`
	Locations        []string `json:"locations"`
	Category         string   `json:"category"`
	Overview         string   `json:"overview"`
	Responsibilities string   `json:"responsibilities"`
	Qualifications   string   `json:"qualifications"`
	Compensation     string   `json:"compensation"`
	ApplyURL         string   `json:"applyUrl"`
	PortalJobPost    *struct {
		PortalID    int64  `json:"portalId"`
		PortalURL   string `json:"portalUrl"`
		ID          int64  `json:"id"`
		UpdatedDate string `json:"updatedDate"`
	} `json:"portalJobPost"`
}

func (s *atlassian) Company() string { return s.company }

func (s *atlassian) Fetch(ctx context.Context) ([]model.Job, error) {
	var postings []atlassianPosting
	if err := fetchJSON(ctx, s.client, http.MethodGet, s.endpoint, nil, &postings); err != nil {
		return nil, err
	}
	if postings == nil {
		return nil, fmt.Errorf("atlassian listings returned null instead of an array")
	}
	if len(postings) == 0 {
		return nil, fmt.Errorf("atlassian listings returned no jobs (endpoint or schema may have changed)")
	}
	if len(postings) > s.maxPostings {
		log.Printf("atlassian: listing %d of %d postings (max_postings cap)", s.maxPostings, len(postings))
		postings = postings[:s.maxPostings]
	}

	jobs := make([]model.Job, 0, len(postings))
	seen := make(map[int64]struct{}, len(postings))
	for i, posting := range postings {
		if posting.ID <= 0 {
			return nil, fmt.Errorf("atlassian item %d has invalid id %d", i, posting.ID)
		}
		if _, duplicate := seen[posting.ID]; duplicate {
			return nil, fmt.Errorf("atlassian duplicate posting id %d", posting.ID)
		}
		seen[posting.ID] = struct{}{}
		if posting.PortalJobPost == nil {
			return nil, fmt.Errorf("atlassian posting %d omitted portalJobPost", posting.ID)
		}
		if posting.PortalJobPost.ID != posting.ID {
			return nil, fmt.Errorf("atlassian posting %d portalJobPost id is %d", posting.ID, posting.PortalJobPost.ID)
		}
		title := strings.TrimSpace(posting.Title)
		if title == "" {
			return nil, fmt.Errorf("atlassian posting %d has empty title", posting.ID)
		}
		description := joinDescriptionParts(
			htmltext.ToText(posting.Overview),
			htmltext.ToText(posting.Responsibilities),
			htmltext.ToText(posting.Qualifications),
			htmltext.ToText(posting.Compensation),
		)
		if description == "" {
			return nil, fmt.Errorf("atlassian posting %d has no description fields", posting.ID)
		}
		id := strconv.FormatInt(posting.ID, 10)
		jobs = append(jobs, model.Job{
			ID:          "atlassian/" + id,
			Company:     s.company,
			Title:       title,
			Location:    strings.Join(distinctStrings(posting.Locations), "; "),
			URL:         strings.TrimRight(s.detailBase, "/") + "/" + id,
			Description: description,
		})
	}
	return jobs, nil
}
