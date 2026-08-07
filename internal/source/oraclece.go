package source

// Oracle Recruiting Candidate Experience exposes a public paged REST list
// plus one detail endpoint per requisition. The board hostname and site name
// are the two stable connection coordinates.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const oracleCEPageSize = 100

var oracleCESiteRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func init() {
	Register("oraclece", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawHost, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		host, err := normalizeBoardHost(rawHost)
		if err != nil {
			return nil, err
		}
		site, err := p.Require("site")
		if err != nil {
			return nil, err
		}
		if !oracleCESiteRe.MatchString(site) {
			return nil, fmt.Errorf("param %q: invalid Oracle site %q", "site", site)
		}
		location := strings.TrimSpace(p.Get("location"))
		if len(location) > 200 || strings.ContainsAny(location, ",;") {
			return nil, fmt.Errorf("param %q: invalid Oracle location %q", "location", location)
		}
		maxPages, err := positiveCappedParam(p, "max_pages", 100, 200)
		if err != nil {
			return nil, err
		}
		pod, _, _ := strings.Cut(host, ".")
		return &oracleCE{
			company: company, host: host, site: site, base: "https://" + host,
			keyPrefix: fmt.Sprintf("oraclece/%s/%s/", pod, site),
			location:  location, maxPages: maxPages, client: client,
		}, nil
	})
}

type oracleCE struct {
	company string
	host    string
	site    string
	base    string
	// keyPrefix drops the host's DNS suffix: "eofe.fa.us2.oraclecloud.com" is
	// a customer pod (eofe) inside an Oracle data-centre region
	// (fa.us2.oraclecloud.com), and Oracle relocates pods between regions.
	// Only the pod goes into job IDs; the live URL is rebuilt from base.
	// Must stay equal to source.StatePrefix for these params.
	keyPrefix string // oraclece/{pod}/{site}/
	location  string
	maxPages  int
	client    *http.Client
}

func (o *oracleCE) Company() string { return o.company }

type oracleCEPosting struct {
	ID                     stringish `json:"Id"`
	Title                  string    `json:"Title"`
	PostedDate             string    `json:"PostedDate"`
	PrimaryLocation        string    `json:"PrimaryLocation"`
	PrimaryLocationCountry string    `json:"PrimaryLocationCountry"`
	WorkplaceType          string    `json:"WorkplaceType"`
}

type oracleCEList struct {
	Items []struct {
		TotalJobsCount int               `json:"TotalJobsCount"`
		Offset         int               `json:"Offset"`
		Limit          int               `json:"Limit"`
		SiteNumber     string            `json:"SiteNumber"`
		Requisitions   []oracleCEPosting `json:"requisitionList"`
	} `json:"items"`
	Count   int  `json:"count"`
	HasMore bool `json:"hasMore"`
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
}

func (o *oracleCE) Fetch(ctx context.Context) ([]model.Job, error) {
	if o.maxPages <= 0 {
		return nil, fmt.Errorf("oraclece %s: max_pages must be positive", o.host)
	}
	var jobs []model.Job
	seen := make(map[string]struct{})
	total := -1
	offset := 0
	for pageNumber := 1; pageNumber <= o.maxPages; pageNumber++ {
		finder := fmt.Sprintf(
			"findReqs;siteNumber=%s,limit=%d,offset=%d",
			o.site, oracleCEPageSize, offset,
		)
		if o.location != "" {
			finder += ",location=" + o.location
		}
		query := url.Values{
			"onlyData": {"true"},
			"expand":   {"requisitionList"},
			"finder":   {finder},
		}
		var page oracleCEList
		endpoint := o.base + "/hcmRestApi/resources/latest/recruitingCEJobRequisitions?" + query.Encode()
		if err := fetchJSON(ctx, o.client, http.MethodGet, endpoint, nil, &page); err != nil {
			return nil, fmt.Errorf("oraclece %s page %d: %w", o.host, pageNumber, err)
		}
		if len(page.Items) != 1 {
			return nil, fmt.Errorf("oraclece %s page %d: expected one search result object, got %d", o.host, pageNumber, len(page.Items))
		}
		result := page.Items[0]
		if result.Offset != offset {
			return nil, fmt.Errorf("oraclece %s page %d: response offset %d, requested %d", o.host, pageNumber, result.Offset, offset)
		}
		if result.Limit <= 0 {
			return nil, fmt.Errorf("oraclece %s page %d: invalid response limit %d", o.host, pageNumber, result.Limit)
		}
		if result.SiteNumber != "" && result.SiteNumber != o.site {
			return nil, fmt.Errorf("oraclece %s page %d: response site %q, requested %q", o.host, pageNumber, result.SiteNumber, o.site)
		}
		if result.TotalJobsCount < 0 {
			return nil, fmt.Errorf("oraclece %s page %d: negative total %d", o.host, pageNumber, result.TotalJobsCount)
		}
		if total < 0 {
			total = result.TotalJobsCount
		} else if total != result.TotalJobsCount {
			return nil, fmt.Errorf("oraclece %s page %d: total changed from %d to %d", o.host, pageNumber, total, result.TotalJobsCount)
		}
		if len(result.Requisitions) == 0 && offset < total {
			return nil, fmt.Errorf("oraclece %s page %d: empty page before reported total %d", o.host, pageNumber, total)
		}
		for index, posting := range result.Requisitions {
			id := strings.TrimSpace(string(posting.ID))
			title := strings.TrimSpace(posting.Title)
			if id == "" || title == "" {
				return nil, fmt.Errorf("oraclece %s page %d row %d: posting is missing Id or Title", o.host, pageNumber, index)
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("oraclece %s page %d: duplicate requisition Id %q", o.host, pageNumber, id)
			}
			seen[id] = struct{}{}
			postedAt, err := parsePostingDate(posting.PostedDate)
			if err != nil {
				return nil, fmt.Errorf("oraclece %s requisition %s: %w", o.host, id, err)
			}
			location := strings.TrimSpace(posting.PrimaryLocation)
			if location == "" {
				location = strings.TrimSpace(posting.PrimaryLocationCountry)
			}
			jobs = append(jobs, model.Job{
				ID:       o.keyPrefix + id,
				Company:  o.company,
				Title:    title,
				Location: location,
				URL: fmt.Sprintf(
					"%s/hcmUI/CandidateExperience/en/sites/%s/job/%s",
					o.base, url.PathEscape(o.site), url.PathEscape(id),
				),
				EmploymentType: strings.TrimSpace(posting.WorkplaceType),
				PostedAt:       postedAt,
			})
		}
		offset += len(result.Requisitions)
		if offset >= total {
			if offset != total {
				return nil, fmt.Errorf("oraclece %s: received %d postings, reported total is %d", o.host, offset, total)
			}
			return jobs, nil
		}
	}
	return nil, fmt.Errorf("oraclece %s: pagination exceeded max_pages=%d after %d of %d postings", o.host, o.maxPages, offset, total)
}

type oracleCEDetail struct {
	ID                       stringish `json:"Id"`
	Title                    string    `json:"Title"`
	ExternalPostedStartDate  string    `json:"ExternalPostedStartDate"`
	ExternalDescription      string    `json:"ExternalDescriptionStr"`
	ExternalResponsibilities string    `json:"ExternalResponsibilitiesStr"`
	ExternalQualifications   string    `json:"ExternalQualificationsStr"`
	CorporateDescription     string    `json:"CorporateDescriptionStr"`
	OrganizationDescription  string    `json:"OrganizationDescriptionStr"`
	ShortDescription         string    `json:"ShortDescriptionStr"`
	PrimaryLocation          string    `json:"PrimaryLocation"`
	PrimaryLocationCountry   string    `json:"PrimaryLocationCountry"`
	JobSchedule              string    `json:"JobSchedule"`
	JobType                  string    `json:"JobType"`
	WorkerType               string    `json:"WorkerType"`
	ContractType             string    `json:"ContractType"`
	WorkplaceType            string    `json:"WorkplaceType"`
}

func (o *oracleCE) Detail(ctx context.Context, job *model.Job) error {
	if job == nil || !strings.HasPrefix(job.ID, o.keyPrefix) {
		return fmt.Errorf("oraclece %s: job ID does not belong to this board", o.host)
	}
	id := strings.TrimPrefix(job.ID, o.keyPrefix)
	if id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("oraclece %s: invalid job ID %q", o.host, job.ID)
	}
	query := url.Values{
		"onlyData": {"true"},
		"finder":   {fmt.Sprintf("ById;Id=%s,siteNumber=%s", id, o.site)},
	}
	var envelope struct {
		Items []oracleCEDetail `json:"items"`
	}
	endpoint := o.base + "/hcmRestApi/resources/latest/recruitingCEJobRequisitionDetails?" + query.Encode()
	if err := fetchJSON(ctx, o.client, http.MethodGet, endpoint, nil, &envelope); err != nil {
		return fmt.Errorf("oraclece %s requisition %s detail: %w", o.host, id, err)
	}
	if len(envelope.Items) != 1 {
		return fmt.Errorf("oraclece %s requisition %s detail: expected one item, got %d", o.host, id, len(envelope.Items))
	}
	detail := envelope.Items[0]
	if got := strings.TrimSpace(string(detail.ID)); got != id {
		return fmt.Errorf("oraclece %s requisition %s detail: response Id is %q", o.host, id, got)
	}
	if strings.TrimSpace(detail.Title) == "" {
		return fmt.Errorf("oraclece %s requisition %s detail: missing title", o.host, id)
	}
	descriptionParts := []string{
		detail.ExternalDescription,
		detail.ExternalResponsibilities,
		detail.ExternalQualifications,
	}
	var descriptions []string
	seenDescriptions := make(map[string]struct{})
	for _, fragment := range descriptionParts {
		text := strings.TrimSpace(htmltext.ToText(fragment))
		if text == "" {
			continue
		}
		if _, duplicate := seenDescriptions[text]; !duplicate {
			seenDescriptions[text] = struct{}{}
			descriptions = append(descriptions, text)
		}
	}
	if len(descriptions) == 0 {
		for _, fragment := range []string{
			detail.ShortDescription,
			detail.OrganizationDescription,
			detail.CorporateDescription,
		} {
			if text := strings.TrimSpace(htmltext.ToText(fragment)); text != "" {
				descriptions = append(descriptions, text)
			}
		}
	}
	if len(descriptions) == 0 {
		return fmt.Errorf("oraclece %s requisition %s detail: missing description", o.host, id)
	}
	postedAt, err := parsePostingDate(detail.ExternalPostedStartDate)
	if err != nil {
		return fmt.Errorf("oraclece %s requisition %s detail: %w", o.host, id, err)
	}
	updated := *job
	updated.Title = strings.TrimSpace(detail.Title)
	updated.Description = strings.Join(descriptions, "\n\n")
	if location := strings.TrimSpace(detail.PrimaryLocation); location != "" {
		updated.Location = location
	} else if country := strings.TrimSpace(detail.PrimaryLocationCountry); country != "" {
		updated.Location = country
	}
	for _, employmentType := range []string{detail.JobType, detail.ContractType, detail.WorkerType, detail.JobSchedule, detail.WorkplaceType} {
		if employmentType = strings.TrimSpace(employmentType); employmentType != "" {
			updated.EmploymentType = employmentType
			break
		}
	}
	if !postedAt.IsZero() {
		updated.PostedAt = postedAt
	}
	*job = updated
	return nil
}
