package source

// Keka's public careers embed API returns every active posting, including
// full HTML descriptions, in one anonymous JSON array:
//
//	GET https://{host}/careers/api/embedjobs/{portal}/active/{identifier}
//
// Config:
//
//	- name: SquadStack
//	  source: keka
//	  params:
//	    host: squadrun.keka.com
//	    portal: default
//	    identifier: c750f148-70b8-4a21-868e-f891a1b2d818

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var kekaPortalRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func init() {
	Register("keka", func(company string, p params.Map, client *http.Client) (Source, error) {
		host, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		if err := validateBareHost(host); err != nil {
			return nil, fmt.Errorf("param %q: %w", "host", err)
		}
		portal, err := p.Require("portal")
		if err != nil {
			return nil, err
		}
		if !kekaPortalRE.MatchString(portal) {
			return nil, fmt.Errorf("param %q: invalid Keka portal %q", "portal", portal)
		}
		identifier, err := p.Require("identifier")
		if err != nil {
			return nil, err
		}
		if !customBoardUUIDRE.MatchString(strings.ToLower(identifier)) {
			return nil, fmt.Errorf("param %q: invalid Keka identifier %q", "identifier", identifier)
		}
		return &keka{
			company: company, host: host, portal: portal, identifier: strings.ToLower(identifier),
			base: "https://" + host, client: client,
		}, nil
	})
}

type keka struct {
	company    string
	host       string
	portal     string
	identifier string
	base       string
	client     *http.Client
}

type kekaPosting struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	JobType     int    `json:"jobType"`
	PublishedOn string `json:"publishedOn"`
	Locations   []struct {
		Name        string `json:"name"`
		City        string `json:"city"`
		State       string `json:"state"`
		CountryName string `json:"countryName"`
	} `json:"jobLocations"`
}

func (s *keka) Company() string { return s.company }

func (s *keka) Fetch(ctx context.Context) ([]model.Job, error) {
	endpoint := fmt.Sprintf(
		"%s/careers/api/embedjobs/%s/active/%s",
		s.base, url.PathEscape(s.portal), url.PathEscape(s.identifier),
	)
	var postings *[]kekaPosting
	if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &postings); err != nil {
		return nil, err
	}
	if postings == nil {
		return nil, fmt.Errorf("keka %s: response was null or omitted the posting array", s.host)
	}

	jobs := make([]model.Job, 0, len(*postings))
	seen := make(map[int64]struct{}, len(*postings))
	for i, posting := range *postings {
		if posting.ID <= 0 {
			return nil, fmt.Errorf("keka %s: item %d has invalid id %d", s.host, i, posting.ID)
		}
		if _, duplicate := seen[posting.ID]; duplicate {
			return nil, fmt.Errorf("keka %s: duplicate posting id %d", s.host, posting.ID)
		}
		seen[posting.ID] = struct{}{}
		title := strings.TrimSpace(posting.Title)
		if title == "" {
			return nil, fmt.Errorf("keka %s: item %d has an empty title", s.host, i)
		}
		description := htmltext.ToText(posting.Description)
		if description == "" {
			return nil, fmt.Errorf("keka %s: item %d has an empty description", s.host, i)
		}
		var postedAt time.Time
		if strings.TrimSpace(posting.PublishedOn) != "" {
			var err error
			postedAt, err = time.Parse(time.RFC3339Nano, posting.PublishedOn)
			if err != nil {
				return nil, fmt.Errorf("keka %s: item %d has invalid publishedOn: %w", s.host, i, err)
			}
		}
		locationParts := make([]string, 0, len(posting.Locations))
		for _, location := range posting.Locations {
			name := strings.TrimSpace(location.Name)
			if name == "" {
				name = strings.TrimSpace(location.City)
			}
			if name == "" {
				name = strings.TrimSpace(location.CountryName)
			}
			locationParts = append(locationParts, name)
		}

		jobs = append(jobs, model.Job{
			ID:             fmt.Sprintf("keka/%s/%s/%d", s.host, s.portal, posting.ID),
			Company:        s.company,
			Title:          title,
			Location:       strings.Join(distinctStrings(locationParts), "; "),
			URL:            fmt.Sprintf("%s/careers/jobdetails/%d", s.base, posting.ID),
			EmploymentType: kekaJobType(posting.JobType),
			Description:    description,
			PostedAt:       postedAt,
		})
	}
	return jobs, nil
}

func kekaJobType(jobType int) string {
	switch jobType {
	case 1:
		return "Part time"
	case 2:
		return "Full Time"
	default:
		return ""
	}
}

func validateBareHost(host string) error {
	if strings.ContainsAny(host, "/?#") {
		return fmt.Errorf("expected a bare hostname, got %q", host)
	}
	parsed, err := url.Parse("https://" + host)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid hostname %q", host)
	}
	if net.ParseIP(parsed.Hostname()) != nil {
		return fmt.Errorf("IP addresses are not allowed")
	}
	if parsed.Port() != "" {
		return fmt.Errorf("ports are not allowed")
	}
	return nil
}
