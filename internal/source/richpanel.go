package source

// Richpanel publishes a small server-rendered careers page. The list has the
// stable first-party detail URL and summary fields; the complete description
// is fetched lazily from that detail page.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var (
	richpanelSlugRe      = regexp.MustCompile(`^/careers/([a-z0-9]+(?:-[a-z0-9]+)*)/?$`)
	richpanelSlugValueRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

func init() {
	Register("richpanel", func(company string, p params.Map, client *http.Client) (Source, error) {
		return &richpanel{
			company: company, base: "https://www.richpanel.com", client: client,
		}, nil
	})
}

type richpanel struct {
	company string
	base    string
	client  *http.Client
}

func (r *richpanel) Company() string { return r.company }

func (r *richpanel) Fetch(ctx context.Context) ([]model.Job, error) {
	body, err := fetchHTMLPage(ctx, r.client, r.base+"/careers", nil)
	if err != nil {
		return nil, fmt.Errorf("richpanel careers: %w", err)
	}
	doc := string(body)
	if !strings.Contains(strings.ToLower(cleanHTMLFragment(doc)), "current openings") {
		return nil, fmt.Errorf("richpanel careers: missing Current Openings marker")
	}
	var jobs []model.Job
	seen := make(map[string]struct{})
	for anchorIndex, anchor := range htmlAnchors(doc) {
		if !hasClass(anchor.attrs, "job-wrap") {
			continue
		}
		jobURL, err := resolveBoardURL(r.base, anchor.attrs["href"])
		if err != nil {
			return nil, fmt.Errorf("richpanel careers link %d: %w", anchorIndex, err)
		}
		parsedURL, _ := url.Parse(jobURL)
		parsedURL.RawQuery = ""
		jobURL = parsedURL.String()
		slugMatch := richpanelSlugRe.FindStringSubmatch(parsedURL.Path)
		if slugMatch == nil {
			return nil, fmt.Errorf("richpanel careers link %d: invalid detail path %q", anchorIndex, parsedURL.Path)
		}
		slug := slugMatch[1]
		titleElement, ok := firstHTMLClass(anchor.inner, "job-txt-title")
		if !ok || cleanHTMLFragment(titleElement.inner) == "" {
			return nil, fmt.Errorf("richpanel careers job %s: missing title", slug)
		}
		if _, duplicate := seen[slug]; duplicate {
			return nil, fmt.Errorf("richpanel careers: duplicate job slug %q", slug)
		}
		seen[slug] = struct{}{}
		jobs = append(jobs, model.Job{
			ID:             "richpanel/" + slug,
			Company:        r.company,
			Title:          cleanHTMLFragment(titleElement.inner),
			Location:       richpanelField(anchor.inner, "Location"),
			URL:            jobURL,
			EmploymentType: richpanelField(anchor.inner, "Type"),
		})
	}
	return jobs, nil
}

func richpanelField(fragment, label string) string {
	elements := htmlElementsByClass(fragment, "job-txt-depart")
	for _, element := range elements {
		if !strings.EqualFold(strings.TrimSpace(cleanHTMLFragment(element.inner)), label) {
			continue
		}
		tail := fragment[element.end:]
		if value, ok := firstHTMLClass(tail, "job-txt-depart-head"); ok {
			return cleanHTMLFragment(value.inner)
		}
	}
	return ""
}

func (r *richpanel) Detail(ctx context.Context, job *model.Job) error {
	if job == nil || !strings.HasPrefix(job.ID, "richpanel/") {
		return fmt.Errorf("richpanel: job ID does not belong to this board")
	}
	slug := strings.TrimPrefix(job.ID, "richpanel/")
	if !richpanelSlugValueRe.MatchString(slug) {
		return fmt.Errorf("richpanel: invalid job ID %q", job.ID)
	}
	detailURL, err := resolveBoardURL(r.base, job.URL)
	if err != nil {
		return fmt.Errorf("richpanel job %s: %w", slug, err)
	}
	parsedURL, _ := url.Parse(detailURL)
	if match := richpanelSlugRe.FindStringSubmatch(parsedURL.Path); match == nil || match[1] != slug {
		return fmt.Errorf("richpanel job %s: detail URL does not contain the job slug", slug)
	}
	body, err := fetchHTMLPage(ctx, r.client, detailURL, nil)
	if err != nil {
		return fmt.Errorf("richpanel job %s detail: %w", slug, err)
	}
	doc := string(body)
	var description string
	for _, element := range htmlElementsByClass(doc, "job-rte", "w-richtext") {
		if hasClass(element.attrs, "hide-static") || hasClass(element.attrs, "benefits") {
			continue
		}
		description = cleanHTMLFragment(element.inner)
		if description != "" {
			break
		}
	}
	if description == "" {
		return fmt.Errorf("richpanel job %s detail: missing visible job description", slug)
	}
	updated := *job
	updated.Description = description
	if title, ok := firstHTMLClass(doc, "job-txt-title"); ok {
		if text := cleanHTMLFragment(title.inner); text != "" {
			updated.Title = text
		}
	}
	if location := richpanelField(doc, "Location"); location != "" {
		updated.Location = location
	}
	if employmentType := richpanelField(doc, "Type"); employmentType != "" {
		updated.EmploymentType = employmentType
	}
	*job = updated
	return nil
}
