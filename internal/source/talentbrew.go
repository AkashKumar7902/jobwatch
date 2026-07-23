package source

// Radancy TalentBrew powers many enterprise careers sites (Moody's, and
// others). The public search endpoint returns a JSON envelope whose
// `results` field is an HTML fragment listing jobs; each job's full
// description lives on its detail page as a schema.org JSON-LD JobPosting.
// This source parses the fragment for the list and implements Detailer to
// pull descriptions on demand.
//
//	GET https://{host}/en/search-jobs/results?SearchType=5
//	    &SearchResultsModuleName=Section 6 - Search Results List
//	    &CurrentPage={n}&RecordsPerPage=100
//
// Config:
//
//	- name: Moody's
//	  source: talentbrew
//	  params:
//	    host: careers.moodys.com

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("talentbrew", func(company string, p params.Map, client *http.Client) (Source, error) {
		host, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		maxPages, err := p.Int("max_pages", 30)
		if err != nil {
			return nil, err
		}
		return &talentbrew{company: company, host: strings.TrimSuffix(host, "/"), maxPages: maxPages, client: client}, nil
	})
}

type talentbrew struct {
	company  string
	host     string
	maxPages int
	client   *http.Client
}

func (t *talentbrew) Company() string { return t.company }

const talentbrewModule = "Section 6 - Search Results List"

// One list item: the job anchor (href, id, title) followed by its location.
var (
	tbItemRe = regexp.MustCompile(`(?s)<a class="search-results-list__job-link" href="([^"]+)" data-job-id="([^"]+)"[^>]*>(.*?)</a>.*?job-location"[^>]*>(.*?)</li>`)
	tbTagRe  = regexp.MustCompile(`<[^>]+>`)
)

func (t *talentbrew) Fetch(ctx context.Context) ([]model.Job, error) {
	var jobs []model.Job
	seen := map[string]bool{}
	for page := 1; page <= t.maxPages; page++ {
		q := url.Values{
			"SearchType":              {"5"},
			"SearchResultsModuleName": {talentbrewModule},
			"CurrentPage":             {fmt.Sprint(page)},
			"RecordsPerPage":          {"100"},
		}
		var env struct {
			Results string `json:"results"`
			HasJobs bool   `json:"hasJobs"`
		}
		u := "https://" + t.host + "/en/search-jobs/results?" + q.Encode()
		if err := fetchJSON(ctx, t.client, http.MethodGet, u, nil, &env); err != nil {
			return nil, err
		}
		matches := tbItemRe.FindAllStringSubmatch(env.Results, -1)
		if len(matches) == 0 {
			break // no more pages
		}
		for _, m := range matches {
			href, id := m[1], m[2]
			if seen[id] {
				continue
			}
			seen[id] = true
			jobs = append(jobs, model.Job{
				ID:       "talentbrew/" + t.host + "/" + id,
				Company:  t.company,
				Title:    cleanText(m[3]),
				Location: cleanText(m[4]),
				URL:      "https://" + t.host + href,
				// Description and EmploymentType arrive via Detail.
			})
		}
		if len(matches) < 100 {
			break
		}
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("talentbrew %s returned no jobs (markup or endpoint may have changed)", t.host)
	}
	return jobs, nil
}

// Detail pulls the schema.org JSON-LD JobPosting off the job's detail page.
func (t *talentbrew) Detail(ctx context.Context, job *model.Job) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, job.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("detail %s: %s", job.URL, resp.Status)
	}
	posting, err := extractJobPostingLD(resp.Body)
	if err != nil {
		return err
	}
	if posting.Description != "" {
		job.Description = htmltext.ToText(posting.Description)
	}
	if posting.EmploymentType != "" {
		job.EmploymentType = posting.EmploymentType
	}
	return nil
}

type jobPostingLD struct {
	Type           string `json:"@type"`
	Description    string `json:"description"`
	EmploymentType string `json:"employmentType"`
}

var ldScriptRe = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)

// extractJobPostingLD scans a detail page's ld+json blocks for the
// JobPosting object.
func extractJobPostingLD(r interface{ Read([]byte) (int, error) }) (jobPostingLD, error) {
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
		if sb.Len() > 4<<20 { // cap at 4 MB
			break
		}
	}
	for _, m := range ldScriptRe.FindAllStringSubmatch(sb.String(), -1) {
		var p jobPostingLD
		if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &p); err == nil && p.Type == "JobPosting" {
			return p, nil
		}
	}
	return jobPostingLD{}, fmt.Errorf("no JobPosting JSON-LD found")
}

// cleanText strips tags and decodes entities from an HTML fragment field.
func cleanText(s string) string {
	return strings.TrimSpace(htmltext.ToText(tbTagRe.ReplaceAllString(s, "")))
}
