package source

// Komprise publishes its active WP Job Manager postings in a first-party
// sitemap. The board is small, so Fetch reads each detail page to normalize
// its title, location, date, and complete description.

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("komprise", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &komprise{
			company: company, baseURL: "https://www.komprise.com", client: client,
		}, nil
	})
}

type komprise struct {
	company string
	baseURL string
	client  *http.Client
}

func (s *komprise) Company() string { return s.company }

var (
	kompriseTitleRE       = regexp.MustCompile(`(?s)<h1 class="entry-title">(.*?)</h1>`)
	kompriseLocationRE    = regexp.MustCompile(`(?s)<li class="location">.*?>(.*?)</a></li>`)
	kompriseDateRE        = regexp.MustCompile(`<li class="date-posted"><time datetime="([^"]+)"`)
	kompriseDescriptionRE = regexp.MustCompile(`(?s)<div class="job_description">(.*?)</div>\s*<div class="job_application`)
	kompriseJobPathRE     = regexp.MustCompile(`^/job/([a-z0-9]+(?:-[a-z0-9]+)*)/$`)
)

func (s *komprise) Fetch(ctx context.Context) ([]model.Job, error) {
	baseURL, err := kompriseCanonicalBaseURL(s.baseURL)
	if err != nil {
		return nil, err
	}
	sitemapURL := &url.URL{
		Scheme: baseURL.Scheme,
		Host:   baseURL.Host,
		Path:   "/job_listing-sitemap.xml",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sitemapURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := clientWithoutRedirects(s.client).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := kompriseRequireResponseURL(resp, sitemapURL, "sitemap"); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", sitemapURL, resp.Status)
	}
	var sitemap struct {
		URLs []struct {
			Location string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&sitemap); err != nil {
		return nil, fmt.Errorf("komprise: decoding job sitemap: %w", err)
	}
	if len(sitemap.URLs) == 0 {
		return nil, fmt.Errorf("komprise: job sitemap contained no postings")
	}

	jobs := make([]model.Job, 0, len(sitemap.URLs))
	seen := make(map[string]struct{}, len(sitemap.URLs))
	for i, item := range sitemap.URLs {
		jobURL := strings.TrimSpace(item.Location)
		slug, err := kompriseCanonicalJobURL(baseURL, jobURL)
		if err != nil {
			return nil, fmt.Errorf("komprise: sitemap item %d has invalid URL %q: %w", i, jobURL, err)
		}
		if _, duplicate := seen[slug]; duplicate {
			return nil, fmt.Errorf("komprise: duplicate job slug %q", slug)
		}
		seen[slug] = struct{}{}
		job, err := s.fetchJob(ctx, jobURL, slug)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *komprise) fetchJob(ctx context.Context, jobURL, slug string) (model.Job, error) {
	baseURL, err := kompriseCanonicalBaseURL(s.baseURL)
	if err != nil {
		return model.Job{}, err
	}
	canonicalSlug, err := kompriseCanonicalJobURL(baseURL, jobURL)
	if err != nil {
		return model.Job{}, fmt.Errorf("komprise detail has invalid URL %q: %w", jobURL, err)
	}
	if slug != canonicalSlug {
		return model.Job{}, fmt.Errorf(
			"komprise detail URL slug %q does not match requested slug %q",
			canonicalSlug,
			slug,
		)
	}
	expectedURL, err := url.Parse(jobURL)
	if err != nil {
		return model.Job{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jobURL, nil)
	if err != nil {
		return model.Job{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := clientWithoutRedirects(s.client).Do(req)
	if err != nil {
		return model.Job{}, err
	}
	defer resp.Body.Close()
	if err := kompriseRequireResponseURL(resp, expectedURL, "detail"); err != nil {
		return model.Job{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return model.Job{}, fmt.Errorf("komprise detail %s: %s", jobURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return model.Job{}, err
	}
	title := kompriseCapture(body, kompriseTitleRE)
	location := kompriseCapture(body, kompriseLocationRE)
	description := kompriseCapture(body, kompriseDescriptionRE)
	if title == "" || description == "" {
		return model.Job{}, fmt.Errorf("komprise detail %s omitted title or description", jobURL)
	}
	var postedAt time.Time
	if date := kompriseCaptureRaw(body, kompriseDateRE); date != "" {
		postedAt, err = time.Parse("2006-01-02", date)
		if err != nil {
			return model.Job{}, fmt.Errorf(
				"komprise detail %s has invalid posted date %q: %w",
				jobURL,
				date,
				err,
			)
		}
	}
	return model.Job{
		ID:          "komprise/" + slug,
		Company:     s.company,
		Title:       title,
		Location:    location,
		URL:         jobURL,
		Description: description,
		PostedAt:    postedAt,
	}, nil
}

func kompriseCanonicalBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("komprise: invalid base URL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("komprise: invalid base URL %q: unsupported scheme", raw)
	}
	if parsed.Host == "" ||
		parsed.Host != strings.ToLower(parsed.Host) ||
		parsed.Hostname() == "" ||
		parsed.Port() != "" ||
		parsed.User != nil ||
		parsed.Opaque != "" ||
		parsed.Path != "" ||
		parsed.RawPath != "" ||
		parsed.RawQuery != "" ||
		parsed.ForceQuery ||
		parsed.Fragment != "" {
		return nil, fmt.Errorf("komprise: invalid non-canonical base URL %q", raw)
	}
	if raw != parsed.Scheme+"://"+parsed.Host {
		return nil, fmt.Errorf("komprise: invalid non-canonical base URL %q", raw)
	}
	return parsed, nil
}

func kompriseCanonicalJobURL(baseURL *url.URL, raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != baseURL.Scheme ||
		parsed.Host != baseURL.Host ||
		parsed.User != nil ||
		parsed.Port() != "" ||
		parsed.Opaque != "" ||
		parsed.RawPath != "" ||
		parsed.RawQuery != "" ||
		parsed.ForceQuery ||
		parsed.Fragment != "" {
		return "", fmt.Errorf("URL escaped the canonical Komprise origin")
	}
	match := kompriseJobPathRE.FindStringSubmatch(parsed.Path)
	if len(match) != 2 {
		return "", fmt.Errorf("URL path must be exactly /job/<canonical-slug>/")
	}
	slug := match[1]
	canonical := baseURL.Scheme + "://" + baseURL.Host + "/job/" + slug + "/"
	if raw != canonical {
		return "", fmt.Errorf("URL is not in canonical form")
	}
	return slug, nil
}

func kompriseRequireResponseURL(resp *http.Response, expected *url.URL, kind string) error {
	if resp.Request == nil ||
		resp.Request.URL == nil ||
		resp.Request.URL.String() != expected.String() {
		actual := "<missing>"
		if resp.Request != nil && resp.Request.URL != nil {
			actual = resp.Request.URL.String()
		}
		return fmt.Errorf(
			"komprise %s response URL %q does not match requested URL %q",
			kind,
			actual,
			expected,
		)
	}
	return nil
}

func kompriseCapture(body []byte, re *regexp.Regexp) string {
	return htmltext.ToText(kompriseCaptureRaw(body, re))
}

func kompriseCaptureRaw(body []byte, re *regexp.Regexp) string {
	match := re.FindSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(string(match[1]))
}
