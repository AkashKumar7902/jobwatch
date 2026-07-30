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
)

func (s *komprise) Fetch(ctx context.Context) ([]model.Job, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/job_listing-sitemap.xml", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/job_listing-sitemap.xml: %s", s.baseURL, resp.Status)
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
		parsed, err := url.Parse(jobURL)
		if err != nil || !strings.HasPrefix(jobURL, s.baseURL+"/job/") {
			return nil, fmt.Errorf("komprise: sitemap item %d has invalid URL %q", i, jobURL)
		}
		slug := strings.Trim(strings.TrimPrefix(parsed.Path, "/job/"), "/")
		if slug == "" {
			return nil, fmt.Errorf("komprise: sitemap item %d has an empty job slug", i)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jobURL, nil)
	if err != nil {
		return model.Job{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return model.Job{}, err
	}
	defer resp.Body.Close()
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
