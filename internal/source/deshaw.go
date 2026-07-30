package source

// The D. E. Shaw Group renders its careers page server-side and embeds both
// complete regular-job and internship arrays in Next.js __NEXT_DATA__.
//
//	GET https://www.deshaw.com/careers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var nextDataScriptRE = regexp.MustCompile(`(?is)<script\b[^>]*\bid\s*=\s*(?:"__NEXT_DATA__"|'__NEXT_DATA__')[^>]*>(.*?)</script>`)

func init() {
	Register("deshaw", func(company string, p params.Map, client *http.Client) (Source, error) {
		maxPostings, err := p.Int("max_postings", 1000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &deshaw{
			company: company, careersURL: "https://www.deshaw.com/careers",
			siteBase: "https://www.deshaw.com", maxPostings: maxPostings, client: client,
		}, nil
	})
}

type deshaw struct {
	company     string
	careersURL  string
	siteBase    string
	maxPostings int
	client      *http.Client
}

type deshawPosting struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"displayName"`
	Office      []struct {
		Abbreviation string `json:"abbreviation"`
		Name         string `json:"name"`
	} `json:"office"`
	Header   []string `json:"header"`
	Category []string `json:"category"`
	Data     *struct {
		ID             int64  `json:"id"`
		DisplayName    string `json:"displayName"`
		ValidFromDate  string `json:"validFromDate"`
		IsActive       *bool  `json:"isActive"`
		JobURL         string `json:"jobUrl"`
		JobDescription *struct {
			WebsiteDescription    string   `json:"websiteDescription"`
			OnCampusDescription   string   `json:"onCampusDescription"`
			ResponsibilitiesHTML  string   `json:"responsibilitiesHtml"`
			PeopleWeAreLookingFor []string `json:"peopleWeAreLookingFor"`
			PeopleLookingForHTML  string   `json:"peopleWeAreLookingForHtml"`
		} `json:"jobDescription"`
		JobMetadata *struct {
			ActiveOnWebsite      *bool  `json:"activeOnWebsite"`
			ActiveOnJobsListing  *bool  `json:"activeOnJobsListing"`
			ClosingDateNotPassed *bool  `json:"closingDateNotPassed"`
			WorkStatus           string `json:"workStatus"`
		} `json:"jobMetadata"`
	} `json:"data"`
}

type deshawNextData struct {
	Props struct {
		PageProps struct {
			JobsFetchingError *bool            `json:"jobsFetchingError"`
			RegularJobs       *[]deshawPosting `json:"regularJobs"`
			Internships       *[]deshawPosting `json:"internships"`
		} `json:"pageProps"`
	} `json:"props"`
}

func (s *deshaw) Company() string { return s.company }

func (s *deshaw) Fetch(ctx context.Context) ([]model.Job, error) {
	body, err := fetchHTML(ctx, s.client, s.careersURL, customListBodyLimit)
	if err != nil {
		return nil, err
	}
	match := nextDataScriptRE.FindSubmatch(body)
	if match == nil {
		return nil, fmt.Errorf("deshaw: careers page omitted __NEXT_DATA__")
	}
	var payload deshawNextData
	if err := json.Unmarshal(match[1], &payload); err != nil {
		return nil, fmt.Errorf("deshaw: decode __NEXT_DATA__: %w", err)
	}
	page := payload.Props.PageProps
	if page.JobsFetchingError == nil || *page.JobsFetchingError {
		return nil, fmt.Errorf("deshaw: jobsFetchingError missing or true")
	}
	if page.RegularJobs == nil || page.Internships == nil {
		return nil, fmt.Errorf("deshaw: __NEXT_DATA__ omitted regularJobs or internships")
	}
	postings := append([]deshawPosting(nil), (*page.RegularJobs)...)
	postings = append(postings, (*page.Internships)...)
	if len(postings) == 0 {
		return nil, fmt.Errorf("deshaw: regularJobs and internships are both empty")
	}
	if len(postings) > s.maxPostings {
		log.Printf("deshaw: listing %d of %d postings (max_postings cap)", s.maxPostings, len(postings))
		postings = postings[:s.maxPostings]
	}

	jobs := make([]model.Job, 0, len(postings))
	seen := make(map[int64]struct{}, len(postings))
	for i, posting := range postings {
		job, err := s.normalize(posting)
		if err != nil {
			return nil, fmt.Errorf("deshaw item %d: %w", i, err)
		}
		if _, duplicate := seen[posting.ID]; duplicate {
			return nil, fmt.Errorf("deshaw: duplicate posting id %d", posting.ID)
		}
		seen[posting.ID] = struct{}{}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *deshaw) normalize(posting deshawPosting) (model.Job, error) {
	if posting.ID <= 0 || posting.Data == nil || posting.Data.ID != posting.ID {
		return model.Job{}, fmt.Errorf("invalid or mismatched posting id %d", posting.ID)
	}
	data := posting.Data
	if data.IsActive == nil || !*data.IsActive {
		return model.Job{}, fmt.Errorf("posting %d is missing isActive or inactive", posting.ID)
	}
	if data.JobMetadata == nil || data.JobDescription == nil {
		return model.Job{}, fmt.Errorf("posting %d omitted jobMetadata or jobDescription", posting.ID)
	}
	meta := data.JobMetadata
	if meta.ActiveOnWebsite != nil && !*meta.ActiveOnWebsite {
		return model.Job{}, fmt.Errorf("posting %d is not active on website", posting.ID)
	}
	if meta.ActiveOnJobsListing != nil && !*meta.ActiveOnJobsListing {
		return model.Job{}, fmt.Errorf("posting %d is not active on jobs listing", posting.ID)
	}
	if meta.ClosingDateNotPassed != nil && !*meta.ClosingDateNotPassed {
		return model.Job{}, fmt.Errorf("posting %d is past its closing date", posting.ID)
	}
	title := strings.TrimSpace(posting.DisplayName)
	if title == "" {
		title = strings.TrimSpace(data.DisplayName)
	}
	if title == "" {
		return model.Job{}, fmt.Errorf("posting %d has empty displayName", posting.ID)
	}
	idText := strconv.FormatInt(posting.ID, 10)
	jobURL := strings.TrimSpace(data.JobURL)
	if jobURL == "" || !strings.HasSuffix(strings.ToLower(jobURL), "-"+idText) {
		return model.Job{}, fmt.Errorf("posting %d has invalid jobUrl %q", posting.ID, data.JobURL)
	}
	publicURL := strings.TrimRight(s.siteBase, "/") + "/careers/" + strings.ToLower(jobURL)

	people := make([]string, 0, len(data.JobDescription.PeopleWeAreLookingFor))
	for _, item := range data.JobDescription.PeopleWeAreLookingFor {
		people = append(people, htmltext.ToText(item))
	}
	description := joinDescriptionParts(
		htmltext.ToText(data.JobDescription.WebsiteDescription),
		htmltext.ToText(data.JobDescription.ResponsibilitiesHTML),
		strings.Join(distinctStrings(people), "\n"),
		htmltext.ToText(data.JobDescription.PeopleLookingForHTML),
	)
	if description == "" {
		description = htmltext.ToText(data.JobDescription.OnCampusDescription)
	}
	if description == "" {
		return model.Job{}, fmt.Errorf("posting %d has no usable description", posting.ID)
	}

	locations := make([]string, 0, len(posting.Office))
	for _, office := range posting.Office {
		location := strings.TrimSpace(office.Name)
		if location == "" {
			location = strings.TrimSpace(office.Abbreviation)
		}
		locations = append(locations, location)
	}
	var postedAt time.Time
	if rawDate := strings.TrimSpace(data.ValidFromDate); rawDate != "" {
		var err error
		postedAt, err = time.Parse("2006-01-02", rawDate)
		if err != nil {
			return model.Job{}, fmt.Errorf("posting %d has invalid validFromDate %q", posting.ID, rawDate)
		}
	}
	return model.Job{
		ID:             "deshaw/" + idText,
		Company:        s.company,
		Title:          title,
		Location:       strings.Join(distinctStrings(locations), "; "),
		URL:            publicURL,
		EmploymentType: strings.TrimSpace(meta.WorkStatus),
		Description:    description,
		PostedAt:       postedAt,
	}, nil
}
