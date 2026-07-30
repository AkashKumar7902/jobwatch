package source

// The 42Gears careers page initially renders seven cards, but its anonymous
// Next.js flight payload contains the complete opening set. This source
// decodes that payload rather than mistaking the client-side first page for
// the whole board. Detail pages include the full WordPress role HTML in a
// separate flight segment.
//
//	GET https://www.42gears.com/careers/
//	GET https://www.42gears.com/careers/{slug}/

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var (
	forty2GearsSlugRE       = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	forty2GearsTitleRE      = regexp.MustCompile(`(?is)<h1 class=["']text-4xl[^"']*["'][^>]*>(.*?)</h1>`)
	forty2GearsDOMDetailRE  = regexp.MustCompile(`(?is)<div class=["']prose prose-lg[^"']*["'][^>]*>(.*?)</div>\s*</div>\s*</div>\s*</article>`)
	forty2GearsFlightMarker = `"heading":"Current Openings"`
)

func init() {
	Register("forty2gears", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &forty2Gears{company: company, base: "https://www.42gears.com", client: client}, nil
	})
}

type forty2Gears struct {
	company string
	base    string
	client  *http.Client
}

type forty2GearsPosting struct {
	Title        string   `json:"title"`
	Excerpt      string   `json:"excerpt"`
	Href         string   `json:"href"`
	JobTypes     []string `json:"jobTypes"`
	JobLocations []string `json:"jobLocations"`
}

func (s *forty2Gears) Company() string { return s.company }

func (s *forty2Gears) Fetch(ctx context.Context) ([]model.Job, error) {
	endpoint := s.base + "/careers/"
	body, err := fetchCustomBoardHTML(ctx, s.client, endpoint)
	if err != nil {
		return nil, err
	}
	parts, err := extractNextFlightStrings(string(body))
	if err != nil {
		return nil, fmt.Errorf("forty2gears: %w", err)
	}
	flight := strings.Join(parts, "")
	headingAt := strings.Index(flight, forty2GearsFlightMarker)
	if headingAt < 0 {
		return nil, fmt.Errorf("forty2gears: Current Openings flight model not found")
	}
	flight = flight[headingAt:]
	firstItems := strings.Index(flight, `,"items":`)
	if firstItems < 0 {
		return nil, fmt.Errorf("forty2gears: Current Openings model omitted items")
	}
	flight = flight[firstItems+len(`,"items":`):]
	secondItems := strings.Index(flight, `,"items":`)
	if secondItems < 0 {
		return nil, fmt.Errorf("forty2gears: Current Openings component omitted complete items")
	}
	array, err := jsonArrayAfter(flight[secondItems:], `,"items":`)
	if err != nil {
		return nil, fmt.Errorf("forty2gears: %w", err)
	}
	var postings []forty2GearsPosting
	if err := json.Unmarshal(array, &postings); err != nil {
		return nil, fmt.Errorf("forty2gears: decoding complete items: %w", err)
	}
	if len(postings) == 0 {
		return nil, fmt.Errorf("forty2gears: Current Openings contained no jobs")
	}

	jobs := make([]model.Job, 0, len(postings))
	seen := make(map[string]struct{}, len(postings))
	for i, posting := range postings {
		href := strings.TrimSpace(posting.Href)
		if !strings.HasPrefix(href, "/careers/") || !strings.HasSuffix(href, "/") {
			return nil, fmt.Errorf("forty2gears: item %d has invalid href %q", i, posting.Href)
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(href, "/careers/"), "/")
		if !forty2GearsSlugRE.MatchString(slug) || strings.Contains(slug, "/") {
			return nil, fmt.Errorf("forty2gears: item %d has invalid slug %q", i, slug)
		}
		if _, duplicate := seen[slug]; duplicate {
			return nil, fmt.Errorf("forty2gears: duplicate slug %q", slug)
		}
		seen[slug] = struct{}{}
		title := strings.TrimSpace(posting.Title)
		if title == "" {
			return nil, fmt.Errorf("forty2gears: item %d has an empty title", i)
		}
		jobTypes := distinctStrings(posting.JobTypes)
		locations := distinctStrings(posting.JobLocations)
		if len(jobTypes) == 0 || len(locations) == 0 {
			return nil, fmt.Errorf("forty2gears: item %d omitted job type or location", i)
		}
		jobs = append(jobs, model.Job{
			ID:             "forty2gears/" + slug,
			Company:        s.company,
			Title:          title,
			Location:       strings.Join(locations, ", "),
			URL:            s.base + href,
			EmploymentType: strings.Join(jobTypes, "; "),
		})
	}
	return jobs, nil
}

func (s *forty2Gears) Detail(ctx context.Context, job *model.Job) error {
	const prefix = "forty2gears/"
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("forty2gears: job id %q does not have prefix %q", job.ID, prefix)
	}
	slug := strings.TrimPrefix(job.ID, prefix)
	if !forty2GearsSlugRE.MatchString(slug) {
		return fmt.Errorf("forty2gears: job id %q has an invalid slug", job.ID)
	}
	endpoint := s.base + "/careers/" + slug + "/"
	body, err := fetchCustomBoardHTML(ctx, s.client, endpoint)
	if err != nil {
		return err
	}
	titleMatch := forty2GearsTitleRE.FindSubmatch(body)
	if titleMatch == nil {
		return fmt.Errorf("forty2gears: detail %q omitted title", slug)
	}
	title := cleanText(string(titleMatch[1]))
	if title == "" {
		return fmt.Errorf("forty2gears: detail %q has an empty title", slug)
	}

	var descriptionHTML string
	if parts, flightErr := extractNextFlightStrings(string(body)); flightErr == nil {
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if strings.HasPrefix(trimmed, "<") && strings.Contains(trimmed, "wp-block-") &&
				len(trimmed) > len(descriptionHTML) {
				descriptionHTML = trimmed
			}
		}
	}
	if descriptionHTML == "" {
		if match := forty2GearsDOMDetailRE.FindSubmatch(body); match != nil {
			descriptionHTML = string(match[1])
		}
	}
	description := htmltext.ToText(descriptionHTML)
	if description == "" {
		return fmt.Errorf("forty2gears: detail %q omitted full role details", slug)
	}
	job.Title = title
	job.Description = description
	job.URL = endpoint
	return nil
}
