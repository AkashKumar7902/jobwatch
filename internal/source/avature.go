package source

// Avature-hosted first-party career portals such as Electronic Arts expose
// server-rendered search and detail pages. The list is offset-paged; details
// are fetched lazily.
//
//	GET https://{host}/{site}/{search_path}?jobRecordsPerPage=20&jobOffset=N

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const avaturePageSize = 20

var (
	avatureSpanRE       = regexp.MustCompile(`(?is)<span\b[^>]*>.*?</span>`)
	avatureTotalRE      = regexp.MustCompile(`(?i)\bof\s+([0-9][0-9,]*)\s*(?:<|$)`)
	avatureRoleIDRE     = regexp.MustCompile(`(?i)\bRole\s+ID\s*([0-9]+)\b`)
	avatureLocationRE   = regexp.MustCompile(`(?is)<strong>\s*Locations?\s*</strong>\s*:\s*(.*?)<br\b`)
	avatureDetailIDRE   = regexp.MustCompile(`(?is)article__content__view__field__label[^>]*>\s*Role\s+ID\s*</div>.*?article__content__view__field__value[^>]*>\s*([0-9]+)\s*</div>`)
	avatureWorkerTypeRE = regexp.MustCompile(`(?is)article__content__view__field__label[^>]*>\s*Worker\s+Type\s*</div>.*?article__content__view__field__value[^>]*>\s*(.*?)\s*</div>`)
	avatureLinkTagRE    = regexp.MustCompile(`(?is)<link\b[^>]*>`)
)

func init() {
	Register("avature", func(company string, p params.Map, client *http.Client) (Source, error) {
		host, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		if err := validateHostParam("host", host); err != nil {
			return nil, err
		}
		site, err := cleanRelativePathParam(p, "site")
		if err != nil {
			return nil, err
		}
		searchPath, err := cleanRelativePathParam(p, "search_path")
		if err != nil {
			return nil, err
		}
		maxPostings, err := p.Int("max_postings", 1000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		base := "https://" + host
		return &avature{
			company: company, host: host, site: site,
			searchURL: base + "/" + path.Join(site, searchPath) + "/", maxPostings: maxPostings, client: client,
		}, nil
	})
}

func cleanRelativePathParam(p params.Map, name string) (string, error) {
	value, err := p.Require(name)
	if err != nil {
		return "", err
	}
	value = strings.Trim(value, "/")
	if value == "" || strings.Contains(value, `\`) {
		return "", fmt.Errorf("param %q: invalid relative path %q", name, value)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.RawQuery != "" || parsed.Fragment != "" || path.Clean("/"+value) != "/"+value {
		return "", fmt.Errorf("param %q: invalid relative path %q", name, value)
	}
	return value, nil
}

type avature struct {
	company     string
	host        string
	site        string
	searchURL   string
	maxPostings int
	client      *http.Client
}

type avatureListItem struct {
	ID             string
	Title          string
	Location       string
	EmploymentType string
	URL            string
}

func (s *avature) Company() string { return s.company }

func (s *avature) Fetch(ctx context.Context) ([]model.Job, error) {
	expectedTotal := -1
	jobs := make([]model.Job, 0)
	seen := make(map[string]struct{})
	for offset := 0; offset < s.maxPostings; {
		limit := min(avaturePageSize, s.maxPostings-offset)
		parsed, err := url.Parse(s.searchURL)
		if err != nil {
			return nil, err
		}
		query := parsed.Query()
		query.Set("jobRecordsPerPage", strconv.Itoa(limit))
		query.Set("jobOffset", strconv.Itoa(offset))
		parsed.RawQuery = query.Encode()
		body, err := fetchHTML(ctx, s.client, parsed.String(), customListBodyLimit)
		if err != nil {
			return nil, fmt.Errorf("avature %s: page at offset %d: %w", s.host, offset, err)
		}
		total, err := parseAvatureTotal(body)
		if err != nil {
			return nil, fmt.Errorf("avature %s: page at offset %d: %w", s.host, offset, err)
		}
		if expectedTotal < 0 {
			expectedTotal = total
		} else if total != expectedTotal {
			return nil, fmt.Errorf("avature %s: total changed from %d to %d at offset %d", s.host, expectedTotal, total, offset)
		}
		items, err := s.parseList(body)
		if err != nil {
			return nil, fmt.Errorf("avature %s: page at offset %d: %w", s.host, offset, err)
		}
		if len(items) > limit {
			return nil, fmt.Errorf("avature %s: page at offset %d returned %d jobs, limit is %d", s.host, offset, len(items), limit)
		}
		if len(items) == 0 {
			if offset < expectedTotal {
				return nil, fmt.Errorf("avature %s: empty page at offset %d before reported total %d", s.host, offset, expectedTotal)
			}
			break
		}
		for _, item := range items {
			if _, duplicate := seen[item.ID]; duplicate {
				return nil, fmt.Errorf("avature %s: duplicate role id %s", s.host, item.ID)
			}
			seen[item.ID] = struct{}{}
			jobs = append(jobs, model.Job{
				ID:             fmt.Sprintf("avature/%s/%s/%s", s.host, s.site, item.ID),
				Company:        s.company,
				Title:          item.Title,
				Location:       item.Location,
				URL:            item.URL,
				EmploymentType: item.EmploymentType,
			})
		}
		scanned := offset + len(items)
		if scanned >= expectedTotal || scanned >= s.maxPostings {
			break
		}
		if len(items) < limit {
			return nil, fmt.Errorf("avature %s: short page of %d at offset %d before reported total %d", s.host, len(items), offset, expectedTotal)
		}
		offset = scanned
	}
	if expectedTotal > s.maxPostings {
		log.Printf("avature %s: listing %d of %d postings (max_postings cap)", s.host, len(jobs), expectedTotal)
	}
	return jobs, nil
}

func parseAvatureTotal(body []byte) (int, error) {
	match := avatureTotalRE.FindSubmatch(body)
	if match == nil {
		return 0, fmt.Errorf("search page omitted total result count")
	}
	total, err := strconv.Atoi(strings.ReplaceAll(string(match[1]), ",", ""))
	if err != nil || total < 0 {
		return 0, fmt.Errorf("invalid total result count %q", match[1])
	}
	return total, nil
}

func (s *avature) parseList(body []byte) ([]avatureListItem, error) {
	type resultAnchor struct {
		start int
		end   int
		href  string
		title string
	}
	var anchors []resultAnchor
	for _, index := range htmlAnchorRE.FindAllIndex(body, -1) {
		tag := string(body[index[0]:index[1]])
		if !hasHTMLClass(tag, "link_result") {
			continue
		}
		href := htmlAttribute(tag, "href")
		title := cleanText(tag)
		if href == "" || title == "" {
			return nil, fmt.Errorf("result anchor omitted href or title")
		}
		anchors = append(anchors, resultAnchor{start: index[0], end: index[1], href: href, title: title})
	}
	items := make([]avatureListItem, 0, len(anchors))
	for i, anchor := range anchors {
		end := len(body)
		if i+1 < len(anchors) {
			end = anchors[i+1].start
		}
		block := body[anchor.end:end]
		idText := classText(block, "list-item-id")
		idMatch := avatureRoleIDRE.FindStringSubmatch(idText)
		if idMatch == nil {
			return nil, fmt.Errorf("result %q omitted Role ID", anchor.title)
		}
		publicURL, err := resolveReference(s.searchURL, anchor.href)
		if err != nil {
			return nil, fmt.Errorf("role %s URL: %w", idMatch[1], err)
		}
		parsed, err := url.Parse(publicURL)
		if err != nil || parsed.Host != s.host || !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/"+idMatch[1]) {
			return nil, fmt.Errorf("role %s has invalid detail URL %q", idMatch[1], publicURL)
		}
		items = append(items, avatureListItem{
			ID:             idMatch[1],
			Title:          anchor.title,
			Location:       classText(block, "list-item-location"),
			EmploymentType: classText(block, "list-item-workerType"),
			URL:            publicURL,
		})
	}
	return items, nil
}

func classText(body []byte, class string) string {
	for _, span := range avatureSpanRE.FindAll(body, -1) {
		if hasHTMLClass(string(span), class) {
			return cleanText(string(span))
		}
	}
	return ""
}

func (s *avature) Detail(ctx context.Context, job *model.Job) error {
	prefix := fmt.Sprintf("avature/%s/%s/", s.host, s.site)
	if !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("avature %s: job id %q does not have prefix %q", s.host, job.ID, prefix)
	}
	roleID := strings.TrimPrefix(job.ID, prefix)
	if _, err := strconv.ParseInt(roleID, 10, 64); err != nil {
		return fmt.Errorf("avature %s: invalid role id %q", s.host, roleID)
	}
	body, err := fetchHTML(ctx, s.client, job.URL, customDetailBodyLimit)
	if err != nil {
		return err
	}
	idMatch := avatureDetailIDRE.FindSubmatch(body)
	if idMatch == nil || string(idMatch[1]) != roleID {
		return fmt.Errorf("avature %s: detail Role ID does not match %s", s.host, roleID)
	}
	description, err := avatureDescription(body)
	if err != nil {
		return fmt.Errorf("avature %s role %s: %w", s.host, roleID, err)
	}
	job.Description = description
	if match := avatureLocationRE.FindSubmatch(body); match != nil {
		job.Location = cleanText(string(match[1]))
	}
	if match := avatureWorkerTypeRE.FindSubmatch(body); match != nil {
		job.EmploymentType = cleanText(string(match[1]))
	}
	for _, tag := range avatureLinkTagRE.FindAll(body, -1) {
		if strings.EqualFold(htmlAttribute(string(tag), "rel"), "canonical") {
			if canonical := htmlAttribute(string(tag), "href"); canonical != "" {
				job.URL = canonical
			}
			break
		}
	}
	return nil
}

func avatureDescription(body []byte) (string, error) {
	marker := bytes.Index(body, []byte("Description &amp; Requirements"))
	if marker < 0 {
		marker = bytes.Index(body, []byte("Description & Requirements"))
	}
	if marker < 0 {
		return "", fmt.Errorf("detail page omitted Description & Requirements section")
	}
	start := bytes.LastIndex(body[:marker], []byte("<article"))
	endRelative := bytes.Index(body[marker:], []byte("</article>"))
	if start < 0 || endRelative < 0 {
		return "", fmt.Errorf("malformed description article")
	}
	description := htmltext.ToText(string(body[start : marker+endRelative]))
	description = strings.TrimSpace(strings.TrimPrefix(description, "Description & Requirements"))
	if description == "" {
		return "", fmt.Errorf("description article is empty")
	}
	return description, nil
}
