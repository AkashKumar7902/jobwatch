package source

// PayU Global renders its complete public job board and job descriptions into
// first-party WordPress HTML. The board's status line is checked to ensure the
// response contains every active position rather than a truncated view.
//
//	GET https://corporate.payu.com/job-board/
//	GET https://corporate.payu.com/job/{uuid}/?gh_jid={uuid}

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var (
	payuStatusRE      = regexp.MustCompile(`(?is)Showing\s*<span>\s*([0-9]+)\s*</span>\s*of\s*<span>\s*([0-9]+)\s*</span>\s*positions`)
	payuItemRE        = regexp.MustCompile(`(?is)<li class=["']job-entry["'][^>]*>(.*?)</li>`)
	payuHrefRE        = regexp.MustCompile(`(?is)<a href=["']([^"']+)["'][^>]*class=["']title["'][^>]*>`)
	payuTitleRE       = regexp.MustCompile(`(?is)<h3[^>]*>(.*?)</h3>`)
	payuLocationRE    = regexp.MustCompile(`(?is)<span class=["']tag["'][^>]*data-type=["']location["'][^>]*>(.*?)</span>`)
	payuJobPathRE     = regexp.MustCompile(`^/job/([0-9a-f-]{36})/$`)
	payuDetailTitleRE = regexp.MustCompile(`(?is)<section class=["']hero hero-secondary["']>.*?<h1[^>]*>(.*?)</h1>`)
	payuDetailLocRE   = regexp.MustCompile(`(?is)<div class=["']job-location["']>.*?<p class=["']hero-subtitle["']>(.*?)</p>`)
	payuDescriptionRE = regexp.MustCompile(`(?is)<div id=["']main["']>(.*?)<a href=["'][^"']+["'] class=["']btn["'][^>]*>\s*Apply for this job\s*</a>`)
	payuApplyIDRE     = regexp.MustCompile(`(?is)<a href=["']https://jobs\.lever\.co/[^/"']+/([0-9a-f-]{36})/apply["'][^>]*class=["']btn["']`)
)

func init() {
	Register("payu", func(company string, _ params.Map, client *http.Client) (Source, error) {
		return &payu{company: company, base: "https://corporate.payu.com", client: client}, nil
	})
}

type payu struct {
	company string
	base    string
	client  *http.Client
}

func (s *payu) Company() string { return s.company }

func (s *payu) Fetch(ctx context.Context) ([]model.Job, error) {
	endpoint := s.base + "/job-board/"
	body, err := fetchCustomBoardHTML(ctx, s.client, endpoint)
	if err != nil {
		return nil, err
	}
	statusMatch := payuStatusRE.FindSubmatch(body)
	if statusMatch == nil {
		return nil, fmt.Errorf("payu: job board omitted result-count metadata")
	}
	shown, _ := strconv.Atoi(string(statusMatch[1]))
	total, _ := strconv.Atoi(string(statusMatch[2]))
	if shown != total {
		return nil, fmt.Errorf("payu: job board rendered %d of %d positions", shown, total)
	}

	baseURL, _ := url.Parse(s.base)
	items := payuItemRE.FindAllSubmatch(body, -1)
	if len(items) != shown {
		return nil, fmt.Errorf("payu: board says it shows %d jobs but contains %d", shown, len(items))
	}
	jobs := make([]model.Job, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		hrefMatch := payuHrefRE.FindSubmatch(item[1])
		titleMatch := payuTitleRE.FindSubmatch(item[1])
		locationMatch := payuLocationRE.FindSubmatch(item[1])
		if hrefMatch == nil || titleMatch == nil || locationMatch == nil {
			return nil, fmt.Errorf("payu: item %d omitted URL, title, or location", i)
		}
		postingURL, err := url.Parse(string(hrefMatch[1]))
		if err != nil {
			return nil, fmt.Errorf("payu: item %d has invalid URL: %w", i, err)
		}
		if !postingURL.IsAbs() {
			postingURL = baseURL.ResolveReference(postingURL)
		}
		if postingURL.Scheme != baseURL.Scheme || postingURL.Host != baseURL.Host {
			return nil, fmt.Errorf("payu: item %d URL points outside the PayU board", i)
		}
		pathMatch := payuJobPathRE.FindStringSubmatch(postingURL.Path)
		if pathMatch == nil {
			return nil, fmt.Errorf("payu: item %d has noncanonical job path %q", i, postingURL.Path)
		}
		postingID := strings.ToLower(pathMatch[1])
		if !customBoardUUIDRE.MatchString(postingID) || postingURL.Query().Get("gh_jid") != postingID {
			return nil, fmt.Errorf("payu: item %d URL has a mismatched posting id", i)
		}
		if _, duplicate := seen[postingID]; duplicate {
			return nil, fmt.Errorf("payu: duplicate posting id %q", postingID)
		}
		seen[postingID] = struct{}{}
		title := cleanText(string(titleMatch[1]))
		location := cleanText(string(locationMatch[1]))
		if title == "" || location == "" {
			return nil, fmt.Errorf("payu: item %d has an empty title or location", i)
		}
		canonicalURL := fmt.Sprintf("%s/job/%s/?gh_jid=%s", s.base, postingID, postingID)
		jobs = append(jobs, model.Job{
			ID:       "payu/" + postingID,
			Company:  s.company,
			Title:    title,
			Location: location,
			URL:      canonicalURL,
		})
	}
	return jobs, nil
}

func (s *payu) Detail(ctx context.Context, job *model.Job) error {
	const prefix = "payu/"
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("payu: job id %q does not have prefix %q", job.ID, prefix)
	}
	postingID := strings.ToLower(strings.TrimPrefix(job.ID, prefix))
	if !customBoardUUIDRE.MatchString(postingID) {
		return fmt.Errorf("payu: job id %q has an invalid posting id", job.ID)
	}
	endpoint := fmt.Sprintf("%s/job/%s/?gh_jid=%s", s.base, postingID, postingID)
	body, err := fetchCustomBoardHTML(ctx, s.client, endpoint)
	if err != nil {
		return err
	}
	titleMatch := payuDetailTitleRE.FindSubmatch(body)
	locationMatch := payuDetailLocRE.FindSubmatch(body)
	descriptionMatch := payuDescriptionRE.FindSubmatch(body)
	applyMatch := payuApplyIDRE.FindSubmatch(body)
	if titleMatch == nil || locationMatch == nil || descriptionMatch == nil || applyMatch == nil {
		return fmt.Errorf("payu: detail %q omitted required fields", postingID)
	}
	if strings.ToLower(string(applyMatch[1])) != postingID {
		return fmt.Errorf("payu: detail %q has a mismatched apply URL", postingID)
	}
	title := cleanText(string(titleMatch[1]))
	location := cleanText(string(locationMatch[1]))
	description := htmltext.ToText(string(descriptionMatch[1]))
	if title == "" || location == "" || description == "" {
		return fmt.Errorf("payu: detail %q has an empty required field", postingID)
	}
	job.Title = title
	job.Location = location
	job.Description = description
	job.URL = endpoint
	return nil
}
