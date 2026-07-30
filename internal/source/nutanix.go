package source

// Nutanix publishes its complete public job inventory as an XML feed. The
// normal careers pages are protected by an interactive CDN challenge, while
// this first-party feed is explicitly linked from the board and includes full
// descriptions.
//
//	GET https://careers.nutanix.com/en/jobs/xml/?rss=true

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	nutanixFeedBodyLimit = 16 << 20
	nutanixMaxJobs       = 5_000
)

var (
	nutanixOpaqueIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{3,100}$`)
)

func init() {
	Register("nutanix", func(company string, p params.Map, client *http.Client) (Source, error) {
		if len(p) != 0 {
			keys := make([]string, 0, len(p))
			for key := range p {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("nutanix source accepts no params (got %s)", strings.Join(keys, ", "))
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &nutanix{
			company: company,
			base:    "https://careers.nutanix.com",
			client:  client,
		}, nil
	})
}

type nutanix struct {
	company string
	base    string
	client  *http.Client
}

type nutanixFeed struct {
	XMLName       xml.Name         `xml:"source"`
	Publisher     string           `xml:"publisher"`
	PublisherURL  string           `xml:"publisherurl"`
	LastBuildDate string           `xml:"lastBuildDate"`
	Jobs          []nutanixFeedJob `xml:"job"`
}

type nutanixFeedJob struct {
	Title            string `xml:"title"`
	Date             string `xml:"date"`
	RequisitionID    string `xml:"requisitionid"`
	ReferenceNumber  string `xml:"referencenumber"`
	APIJobID         string `xml:"apijobid"`
	URL              string `xml:"url"`
	Company          string `xml:"company"`
	City             string `xml:"city"`
	State            string `xml:"state"`
	Country          string `xml:"country"`
	PostalCode       string `xml:"postalcode"`
	Description      string `xml:"description"`
	JobType          string `xml:"jobtype"`
	Category         string `xml:"category"`
	LastActivityDate string `xml:"lastactivitydate"`
}

func (s *nutanix) Company() string { return s.company }

func (s *nutanix) Fetch(ctx context.Context) ([]model.Job, error) {
	endpoint := s.base + "/en/jobs/xml/?rss=true"
	body, err := fetchNutanixFeed(ctx, s.client, endpoint)
	if err != nil {
		return nil, fmt.Errorf("nutanix: %w", err)
	}
	return s.parseFeed(body)
}

func (s *nutanix) parseFeed(body []byte) ([]model.Job, error) {
	var feed nutanixFeed
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	if err := decoder.Decode(&feed); err != nil {
		return nil, fmt.Errorf("nutanix: decoding XML feed: %w", err)
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("nutanix: XML feed contains a second document")
		}
		return nil, fmt.Errorf("nutanix: XML feed has trailing data: %w", err)
	}
	if strings.TrimSpace(feed.Publisher) != "Nutanix" {
		return nil, fmt.Errorf("nutanix: feed publisher is %q, want Nutanix", feed.Publisher)
	}
	if strings.TrimSpace(feed.PublisherURL) != "https://www.nutanix.com/" {
		return nil, fmt.Errorf("nutanix: feed publisher URL is %q", feed.PublisherURL)
	}
	if _, err := parsePostingDate(feed.LastBuildDate); err != nil || strings.TrimSpace(feed.LastBuildDate) == "" {
		return nil, fmt.Errorf("nutanix: invalid lastBuildDate %q", feed.LastBuildDate)
	}
	if len(feed.Jobs) > nutanixMaxJobs {
		return nil, fmt.Errorf("nutanix: feed contains %d jobs, exceeding safety limit %d", len(feed.Jobs), nutanixMaxJobs)
	}

	jobs := make([]model.Job, 0, len(feed.Jobs))
	seen := make(map[string]struct{}, len(feed.Jobs))
	for index, posting := range feed.Jobs {
		job, err := s.normalizeFeedJob(posting)
		if err != nil {
			return nil, fmt.Errorf("nutanix: job %d: %w", index, err)
		}
		if _, duplicate := seen[job.ID]; duplicate {
			return nil, fmt.Errorf("nutanix: duplicate stable job ID %q", job.ID)
		}
		seen[job.ID] = struct{}{}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *nutanix) normalizeFeedJob(posting nutanixFeedJob) (model.Job, error) {
	requisitionID := strings.TrimSpace(posting.RequisitionID)
	referenceNumber := strings.TrimSpace(posting.ReferenceNumber)
	apiJobID := strings.TrimSpace(posting.APIJobID)
	if !nutanixOpaqueIDRE.MatchString(requisitionID) {
		return model.Job{}, fmt.Errorf("invalid requisition ID %q", requisitionID)
	}
	if referenceNumber != requisitionID {
		return model.Job{}, fmt.Errorf(
			"reference number %q does not match requisition ID %q",
			referenceNumber, requisitionID,
		)
	}
	if !nutanixOpaqueIDRE.MatchString(apiJobID) {
		return model.Job{}, fmt.Errorf("invalid API job ID %q", apiJobID)
	}
	title := compactSpaces(posting.Title)
	description := cleanHTMLFragment(posting.Description)
	if title == "" || len(title) > 300 {
		return model.Job{}, fmt.Errorf("invalid title length %d", len(title))
	}
	if description == "" {
		return model.Job{}, fmt.Errorf("job %q has an empty description", requisitionID)
	}
	if strings.TrimSpace(posting.Company) != "Nutanix" {
		return model.Job{}, fmt.Errorf("job %q company is %q", requisitionID, posting.Company)
	}

	jobURL, err := canonicalNutanixJobURL(s.base, posting.URL, apiJobID)
	if err != nil {
		return model.Job{}, fmt.Errorf("job %q URL: %w", requisitionID, err)
	}
	postedAt, err := parsePostingDate(posting.Date)
	if err != nil || postedAt.IsZero() {
		return model.Job{}, fmt.Errorf("job %q has invalid posting date %q", requisitionID, posting.Date)
	}
	if _, err := parsePostingDate(posting.LastActivityDate); err != nil ||
		strings.TrimSpace(posting.LastActivityDate) == "" {
		return model.Job{}, fmt.Errorf(
			"job %q has invalid last activity date %q",
			requisitionID, posting.LastActivityDate,
		)
	}

	locationParts := make([]string, 0, 3)
	for _, raw := range []string{posting.City, posting.State, posting.Country} {
		value := compactSpaces(raw)
		if value != "" && !containsText(locationParts, value) {
			locationParts = append(locationParts, value)
		}
	}
	if len(locationParts) == 0 {
		return model.Job{}, fmt.Errorf("job %q has no location", requisitionID)
	}
	employmentType := compactSpaces(posting.JobType)
	if employmentType == "" || len(employmentType) > 100 {
		return model.Job{}, fmt.Errorf("job %q has invalid job type", requisitionID)
	}
	if category := compactSpaces(posting.Category); category == "" || len(category) > 200 {
		return model.Job{}, fmt.Errorf("job %q has invalid category", requisitionID)
	}

	return model.Job{
		ID:             "nutanix/" + requisitionID,
		Company:        s.company,
		Title:          title,
		Location:       strings.Join(locationParts, ", "),
		URL:            jobURL,
		EmploymentType: employmentType,
		Description:    description,
		PostedAt:       postedAt,
	}, nil
}

func canonicalNutanixJobURL(base, raw, apiJobID string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if parsed.User != nil || parsed.Scheme != baseURL.Scheme ||
		parsed.Port() != baseURL.Port() || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	if !strings.EqualFold(parsed.Hostname(), baseURL.Hostname()) {
		return "", fmt.Errorf("URL host %q does not match %q", parsed.Hostname(), baseURL.Hostname())
	}
	escapedParts := strings.Split(parsed.EscapedPath(), "/")
	decodedParts := strings.Split(parsed.Path, "/")
	if len(escapedParts) != 6 || len(decodedParts) != 6 ||
		escapedParts[0] != "" || escapedParts[1] != "en" ||
		escapedParts[2] != "jobs" || escapedParts[3] != apiJobID ||
		escapedParts[4] == "" || escapedParts[5] != "" ||
		decodedParts[4] == "." || decodedParts[4] == ".." ||
		strings.TrimSpace(decodedParts[4]) == "" {
		return "", fmt.Errorf("URL path does not contain API job ID %q", apiJobID)
	}
	return parsed.String(), nil
}

func fetchNutanixFeed(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/xml,text/xml")

	resp, err := clientWithoutRedirects(client).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil ||
		resp.Request.URL.String() != req.URL.String() ||
		resp.Request.URL.ForceQuery != req.URL.ForceQuery {
		finalURL := "<missing>"
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		return nil, fmt.Errorf("GET %s: unexpected final URL %q", endpoint, finalURL)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/xml" && mediaType != "text/xml") {
		return nil, fmt.Errorf("GET %s: unexpected Content-Type %q", endpoint, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, nutanixFeedBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if len(body) > nutanixFeedBodyLimit {
		return nil, fmt.Errorf("GET %s: response exceeds %d-byte safety limit", endpoint, nutanixFeedBodyLimit)
	}
	return body, nil
}
