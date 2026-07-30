package source

// Kula's public hosted-careers API returns complete postings, including the
// description and office metadata:
//
//	GET https://careers.kula.ai/api/internal/ats_job_posts
//	    ?accountName={account}&page=N&type=ats_job_post.index&items=99

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	kulaPageSize = 99
	kulaMaxPages = 100
)

var kulaAccountRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func init() {
	Register("kula", func(company string, p params.Map, client *http.Client) (Source, error) {
		account, err := p.Require("account_name")
		if err != nil {
			return nil, err
		}
		if !kulaAccountRE.MatchString(account) {
			return nil, fmt.Errorf("param %q: invalid Kula account name %q", "account_name", account)
		}
		maxPostings, err := p.Int("max_postings", 2000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &kula{
			company: company, account: account, apiBase: "https://careers.kula.ai/api/internal/ats_job_posts",
			boardBase: "https://careers.kula.ai/" + account, maxPostings: maxPostings, client: client,
		}, nil
	})
}

type kula struct {
	company     string
	account     string
	apiBase     string
	boardBase   string
	maxPostings int
	client      *http.Client
}

type kulaPosting struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	Listed         *bool  `json:"listed"`
	Kind           string `json:"kind"`
	IsConfidential *bool  `json:"is_confidential"`
	ATSJob         *struct {
		JobDescription string `json:"job_description"`
		Workplace      string `json:"workplace"`
		EmploymentType string `json:"employment_type"`
		ATSDepartment  *struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"ats_department"`
		Offices []struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			Location  string `json:"location"`
			Country   string `json:"country"`
			State     string `json:"state"`
			City      string `json:"city"`
			Remote    bool   `json:"remote"`
			Workplace string `json:"workplace"`
		} `json:"offices"`
	} `json:"ats_job"`
}

type kulaResponse struct {
	Data   *[]kulaPosting    `json:"data"`
	Errors []json.RawMessage `json:"errors"`
	Meta   *struct {
		Count int `json:"count"`
		Page  int `json:"page"`
		Items int `json:"items"`
		Pages int `json:"pages"`
	} `json:"meta"`
}

func (s *kula) Company() string { return s.company }

func (s *kula) Fetch(ctx context.Context) ([]model.Job, error) {
	var (
		expectedCount = -1
		expectedPages = -1
		jobs          []model.Job
		seen          = make(map[int64]struct{})
	)
	for pageNumber := 1; ; pageNumber++ {
		query := url.Values{
			"accountName": {s.account},
			"page":        {strconv.Itoa(pageNumber)},
			"type":        {"ats_job_post.index"},
			"items":       {strconv.Itoa(kulaPageSize)},
		}
		var page kulaResponse
		if err := fetchJSON(ctx, s.client, http.MethodGet, s.apiBase+"?"+query.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("kula %s: page %d: %w", s.account, pageNumber, err)
		}
		if page.Data == nil || page.Meta == nil {
			return nil, fmt.Errorf("kula %s: page %d omitted data or meta", s.account, pageNumber)
		}
		if len(page.Errors) != 0 {
			return nil, fmt.Errorf("kula %s: page %d returned %d errors", s.account, pageNumber, len(page.Errors))
		}
		if page.Meta.Count < 0 || page.Meta.Pages < 0 || page.Meta.Page != pageNumber {
			return nil, fmt.Errorf("kula %s: page %d has invalid pagination metadata %+v", s.account, pageNumber, *page.Meta)
		}
		if page.Meta.Pages > kulaMaxPages {
			return nil, fmt.Errorf("kula %s: pages %d exceeds safety limit %d", s.account, page.Meta.Pages, kulaMaxPages)
		}
		if expectedCount < 0 {
			expectedCount, expectedPages = page.Meta.Count, page.Meta.Pages
			calculatedPages := 0
			if expectedCount > 0 {
				calculatedPages = (expectedCount + kulaPageSize - 1) / kulaPageSize
			}
			if expectedPages != calculatedPages {
				return nil, fmt.Errorf("kula %s: count %d is inconsistent with pages %d", s.account, expectedCount, expectedPages)
			}
		} else if page.Meta.Count != expectedCount || page.Meta.Pages != expectedPages {
			return nil, fmt.Errorf("kula %s: pagination totals changed on page %d", s.account, pageNumber)
		}

		postings := *page.Data
		if len(postings) > kulaPageSize {
			return nil, fmt.Errorf("kula %s: page %d returned %d items, limit is %d", s.account, pageNumber, len(postings), kulaPageSize)
		}
		wantItems := min(kulaPageSize, max(0, expectedCount-(pageNumber-1)*kulaPageSize))
		if len(postings) != wantItems {
			return nil, fmt.Errorf("kula %s: page %d returned %d items, want %d", s.account, pageNumber, len(postings), wantItems)
		}
		for i, posting := range postings {
			if len(jobs) >= s.maxPostings {
				break
			}
			job, include, err := s.normalize(posting)
			if err != nil {
				return nil, fmt.Errorf("kula %s: page %d item %d: %w", s.account, pageNumber, i, err)
			}
			if !include {
				continue
			}
			if _, duplicate := seen[posting.ID]; duplicate {
				return nil, fmt.Errorf("kula %s: duplicate posting id %d", s.account, posting.ID)
			}
			seen[posting.ID] = struct{}{}
			jobs = append(jobs, job)
		}
		if pageNumber >= expectedPages || len(jobs) >= s.maxPostings {
			break
		}
	}
	if expectedCount > s.maxPostings {
		log.Printf("kula %s: listing %d of %d postings (max_postings cap)", s.account, len(jobs), expectedCount)
	}
	if expectedCount > 0 && len(jobs) == 0 {
		return nil, fmt.Errorf("kula %s: %d postings produced no public jobs", s.account, expectedCount)
	}
	return jobs, nil
}

func (s *kula) normalize(posting kulaPosting) (model.Job, bool, error) {
	if posting.ID <= 0 {
		return model.Job{}, false, fmt.Errorf("invalid id %d", posting.ID)
	}
	if posting.Listed == nil || posting.IsConfidential == nil {
		return model.Job{}, false, fmt.Errorf("posting %d omitted visibility flags", posting.ID)
	}
	if !*posting.Listed || *posting.IsConfidential {
		return model.Job{}, false, nil
	}
	if posting.ATSJob == nil {
		return model.Job{}, false, fmt.Errorf("posting %d omitted ats_job", posting.ID)
	}
	title := strings.TrimSpace(posting.Title)
	if title == "" {
		return model.Job{}, false, fmt.Errorf("posting %d has empty title", posting.ID)
	}
	description := htmltext.ToText(posting.ATSJob.JobDescription)
	if strings.TrimSpace(description) == "" {
		return model.Job{}, false, fmt.Errorf("posting %d has empty job_description", posting.ID)
	}
	locations := make([]string, 0, len(posting.ATSJob.Offices))
	for _, office := range posting.ATSJob.Offices {
		location := strings.TrimSpace(office.Location)
		if location == "" {
			location = strings.Join(trimEmpty(office.City, office.State, office.Country), ", ")
		}
		if office.Remote && !strings.Contains(strings.ToLower(location), "remote") {
			location = strings.TrimSpace("Remote " + location)
		}
		locations = append(locations, location)
	}
	idText := strconv.FormatInt(posting.ID, 10)
	return model.Job{
		ID:             fmt.Sprintf("kula/%s/%s", s.account, idText),
		Company:        s.company,
		Title:          title,
		Location:       strings.Join(distinctStrings(locations), "; "),
		URL:            strings.TrimRight(s.boardBase, "/") + "/" + idText,
		EmploymentType: strings.TrimSpace(posting.ATSJob.EmploymentType),
		Description:    description,
	}, true, nil
}
