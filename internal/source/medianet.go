package source

// Media.net's small first-party WordPress careers site is fully
// server-rendered. The home page enumerates departments and counts, each
// department page lists detail links, and each detail page exposes a stable
// hidden post_id plus the complete description.

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

const mediaNetMaxDepartments = 50

var (
	mediaNetCountRE        = regexp.MustCompile(`(?i)^\s*([0-9]+)\s+Positions?\s*$`)
	mediaNetOpeningsListRE = regexp.MustCompile(`(?is)<ul\b[^>]*class\s*=\s*(?:"[^"]*\bopenings-list\b[^"]*"|'[^']*\bopenings-list\b[^']*')[^>]*>(.*?)</ul>`)
	mediaNetInputRE        = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	mediaNetHeadingRE      = regexp.MustCompile(`(?is)<h2\b[^>]*>.*?</h2>`)
	mediaNetPostBodyRE     = regexp.MustCompile(`(?is)<div\b[^>]*class\s*=\s*(?:"post-body"|'post-body')[^>]*>(.*?)</div>\s*<div\b[^>]*class\s*=\s*(?:"[^"]*\bsocial-share-wrapper\b[^"]*"|'[^']*\bsocial-share-wrapper\b[^']*')`)
)

func init() {
	Register("medianet", func(company string, p params.Map, client *http.Client) (Source, error) {
		maxPostings, err := p.Int("max_postings", 200)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &mediaNet{
			company: company, baseURL: "https://careers.media.net/",
			maxPostings: maxPostings, client: client,
		}, nil
	})
}

type mediaNet struct {
	company     string
	baseURL     string
	maxPostings int
	client      *http.Client
}

type mediaNetDepartment struct {
	URL   string
	Count int
}

type mediaNetLink struct {
	Title string
	URL   string
}

func (s *mediaNet) Company() string { return s.company }

func (s *mediaNet) Fetch(ctx context.Context) ([]model.Job, error) {
	home, err := fetchHTML(ctx, s.client, s.baseURL, customListBodyLimit)
	if err != nil {
		return nil, err
	}
	departments, expectedJobs, err := s.parseDepartments(home)
	if err != nil {
		return nil, err
	}
	if len(departments) == 0 || expectedJobs == 0 {
		return nil, fmt.Errorf("medianet: homepage exposed no active departments")
	}

	var links []mediaNetLink
	seenURLs := make(map[string]struct{})
	for _, department := range departments {
		body, err := fetchHTML(ctx, s.client, department.URL, customListBodyLimit)
		if err != nil {
			return nil, fmt.Errorf("medianet department %s: %w", department.URL, err)
		}
		departmentLinks, err := s.parseDepartment(body)
		if err != nil {
			return nil, fmt.Errorf("medianet department %s: %w", department.URL, err)
		}
		if len(departmentLinks) != department.Count {
			return nil, fmt.Errorf("medianet department %s listed %d jobs, homepage reported %d", department.URL, len(departmentLinks), department.Count)
		}
		for _, link := range departmentLinks {
			if _, duplicate := seenURLs[link.URL]; duplicate {
				return nil, fmt.Errorf("medianet: duplicate detail URL %q", link.URL)
			}
			seenURLs[link.URL] = struct{}{}
			links = append(links, link)
		}
	}
	if len(links) != expectedJobs {
		return nil, fmt.Errorf("medianet: collected %d detail links, homepage reported %d", len(links), expectedJobs)
	}
	if len(links) > s.maxPostings {
		links = links[:s.maxPostings]
	}

	jobs := make([]model.Job, 0, len(links))
	seenIDs := make(map[int64]struct{}, len(links))
	for _, link := range links {
		body, err := fetchHTML(ctx, s.client, link.URL, customDetailBodyLimit)
		if err != nil {
			return nil, fmt.Errorf("medianet detail %s: %w", link.URL, err)
		}
		id, title, description, err := parseMediaNetDetail(body)
		if err != nil {
			return nil, fmt.Errorf("medianet detail %s: %w", link.URL, err)
		}
		if !strings.EqualFold(title, link.Title) {
			return nil, fmt.Errorf("medianet detail %s title %q does not match list title %q", link.URL, title, link.Title)
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, fmt.Errorf("medianet: duplicate post_id %d", id)
		}
		seenIDs[id] = struct{}{}
		jobs = append(jobs, model.Job{
			ID:          "medianet/" + strconv.FormatInt(id, 10),
			Company:     s.company,
			Title:       title,
			URL:         link.URL,
			Description: description,
		})
	}
	return jobs, nil
}

func (s *mediaNet) parseDepartments(body []byte) ([]mediaNetDepartment, int, error) {
	var departments []mediaNetDepartment
	expectedJobs := 0
	for _, anchor := range htmlAnchorRE.FindAll(body, -1) {
		tag := string(anchor)
		if !hasHTMLClass(tag, "flex-btn-link") {
			continue
		}
		match := mediaNetCountRE.FindStringSubmatch(cleanText(tag))
		if match == nil {
			return nil, 0, fmt.Errorf("medianet: department anchor has invalid count %q", cleanText(tag))
		}
		count, _ := strconv.Atoi(match[1])
		if count == 0 {
			continue
		}
		href := htmlAttribute(tag, "href")
		departmentURL, err := s.sameSiteURL(href)
		if err != nil {
			return nil, 0, fmt.Errorf("medianet: department URL %q: %w", href, err)
		}
		departments = append(departments, mediaNetDepartment{URL: departmentURL, Count: count})
		expectedJobs += count
		if len(departments) > mediaNetMaxDepartments {
			return nil, 0, fmt.Errorf("medianet: active departments exceed safety limit %d", mediaNetMaxDepartments)
		}
	}
	return departments, expectedJobs, nil
}

func (s *mediaNet) parseDepartment(body []byte) ([]mediaNetLink, error) {
	match := mediaNetOpeningsListRE.FindSubmatch(body)
	if match == nil {
		return nil, fmt.Errorf("page omitted openings-list")
	}
	var links []mediaNetLink
	for _, anchor := range htmlAnchorRE.FindAll(match[1], -1) {
		tag := string(anchor)
		title := cleanText(tag)
		href := htmlAttribute(tag, "href")
		if title == "" || href == "" {
			return nil, fmt.Errorf("opening anchor omitted title or href")
		}
		publicURL, err := s.sameSiteURL(href)
		if err != nil {
			return nil, err
		}
		links = append(links, mediaNetLink{Title: title, URL: publicURL})
	}
	return links, nil
}

func (s *mediaNet) sameSiteURL(ref string) (string, error) {
	publicURL, err := resolveReference(s.baseURL, ref)
	if err != nil {
		return "", err
	}
	base, _ := url.Parse(s.baseURL)
	parsed, err := url.Parse(publicURL)
	if err != nil || parsed.Host != base.Host {
		return "", fmt.Errorf("URL %q is not on %s", publicURL, base.Host)
	}
	return publicURL, nil
}

func parseMediaNetDetail(body []byte) (int64, string, string, error) {
	var postID int64
	for _, input := range mediaNetInputRE.FindAll(body, -1) {
		tag := string(input)
		if htmlAttribute(tag, "name") != "post_id" {
			continue
		}
		id, err := strconv.ParseInt(htmlAttribute(tag, "value"), 10, 64)
		if err != nil || id <= 0 {
			return 0, "", "", fmt.Errorf("invalid post_id")
		}
		if postID != 0 && postID != id {
			return 0, "", "", fmt.Errorf("conflicting post_id values %d and %d", postID, id)
		}
		postID = id
	}
	if postID == 0 {
		return 0, "", "", fmt.Errorf("page omitted post_id")
	}
	title := ""
	for _, heading := range mediaNetHeadingRE.FindAll(body, -1) {
		tag := string(heading)
		if htmlAttribute(tag, "id") == "jobProfile" {
			title = cleanText(tag)
			break
		}
	}
	if title == "" {
		return 0, "", "", fmt.Errorf("page omitted jobProfile heading")
	}
	descriptionMatch := mediaNetPostBodyRE.FindSubmatch(body)
	if descriptionMatch == nil {
		return 0, "", "", fmt.Errorf("page omitted post-body")
	}
	description := htmltext.ToText(string(descriptionMatch[1]))
	if strings.TrimSpace(description) == "" {
		return 0, "", "", fmt.Errorf("post-body is empty")
	}
	return postID, title, description, nil
}
