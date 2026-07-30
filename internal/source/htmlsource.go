package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/params"
)

const htmlBodyLimit = 8 << 20

var (
	htmlOpenTagRe = regexp.MustCompile(`(?is)<([a-z][a-z0-9]*)\b([^>]*)>`)
	htmlAttrRe    = regexp.MustCompile(`(?is)([a-z_:][-a-z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	htmlAnchorRe  = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	jsonLDScript  = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	boardHostRe   = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	spaceTextRe   = regexp.MustCompile(`\s+`)
	punctuationRe = regexp.MustCompile(`[ \t]+([,.;:!?])`)
)

type htmlElement struct {
	tag   string
	attrs map[string]string
	inner string
	start int
	end   int
}

func normalizeBoardHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(raw, ".")))
	if !boardHostRe.MatchString(host) {
		return "", fmt.Errorf("invalid board host %q: expected a DNS hostname without scheme or path", raw)
	}
	return host, nil
}

func positiveCappedParam(p params.Map, key string, def, cap int) (int, error) {
	n, err := p.Int(key, def)
	if err != nil {
		return 0, err
	}
	if n <= 0 || n > cap {
		return 0, fmt.Errorf("param %q: expected an integer from 1 to %d, got %d", key, cap, n)
	}
	return n, nil
}

func fetchHTMLPage(ctx context.Context, client *http.Client, rawURL string, headers http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("GET %s: %s: %s", rawURL, resp.Status, bytes.TrimSpace(snippet))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, htmlBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", rawURL, err)
	}
	if len(body) > htmlBodyLimit {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", rawURL, htmlBodyLimit)
	}
	return body, nil
}

func parseHTMLAttrs(raw string) map[string]string {
	attrs := make(map[string]string)
	for _, match := range htmlAttrRe.FindAllStringSubmatch(raw, -1) {
		value := match[2]
		if value == "" {
			value = match[3]
		}
		attrs[strings.ToLower(match[1])] = html.UnescapeString(value)
	}
	return attrs
}

func hasClass(attrs map[string]string, class string) bool {
	for _, candidate := range strings.Fields(attrs["class"]) {
		if candidate == class {
			return true
		}
	}
	return false
}

func hasClasses(attrs map[string]string, classes ...string) bool {
	for _, class := range classes {
		if !hasClass(attrs, class) {
			return false
		}
	}
	return true
}

func htmlElementsByClass(doc string, classes ...string) []htmlElement {
	var elements []htmlElement
	offset := 0
	for offset < len(doc) {
		match := htmlOpenTagRe.FindStringSubmatchIndex(doc[offset:])
		if match == nil {
			break
		}
		absolute := make([]int, len(match))
		for i, index := range match {
			if index >= 0 {
				absolute[i] = offset + index
			} else {
				absolute[i] = -1
			}
		}
		tag := strings.ToLower(doc[absolute[2]:absolute[3]])
		attrs := parseHTMLAttrs(doc[absolute[4]:absolute[5]])
		openEnd := absolute[1]
		if !hasClasses(attrs, classes...) {
			offset = openEnd
			continue
		}
		closeStart, closeEnd, ok := matchingHTMLClose(doc, tag, openEnd)
		if !ok {
			offset = openEnd
			continue
		}
		elements = append(elements, htmlElement{
			tag: tag, attrs: attrs, inner: doc[openEnd:closeStart],
			start: absolute[0], end: closeEnd,
		})
		offset = closeEnd
	}
	return elements
}

func matchingHTMLClose(doc, tag string, contentStart int) (int, int, bool) {
	tagRe := regexp.MustCompile(`(?is)</?` + regexp.QuoteMeta(tag) + `\b[^>]*>`)
	depth := 1
	for _, match := range tagRe.FindAllStringIndex(doc[contentStart:], -1) {
		token := doc[contentStart+match[0] : contentStart+match[1]]
		if strings.HasPrefix(strings.ToLower(token), "</") {
			depth--
			if depth == 0 {
				return contentStart + match[0], contentStart + match[1], true
			}
		} else if !strings.HasSuffix(strings.TrimSpace(token[:len(token)-1]), "/") {
			depth++
		}
	}
	return 0, 0, false
}

func firstHTMLClass(doc, class string) (htmlElement, bool) {
	elements := htmlElementsByClass(doc, class)
	if len(elements) == 0 {
		return htmlElement{}, false
	}
	return elements[0], true
}

func htmlAnchors(doc string) []htmlElement {
	matches := htmlAnchorRe.FindAllStringSubmatchIndex(doc, -1)
	anchors := make([]htmlElement, 0, len(matches))
	for _, match := range matches {
		anchors = append(anchors, htmlElement{
			tag: "a", attrs: parseHTMLAttrs(doc[match[2]:match[3]]),
			inner: doc[match[4]:match[5]], start: match[0], end: match[1],
		})
	}
	return anchors
}

func cleanHTMLFragment(fragment string) string {
	return strings.TrimSpace(punctuationRe.ReplaceAllString(htmltext.ToText(fragment), "$1"))
}

func resolveBoardURL(base, href string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(strings.TrimSpace(html.UnescapeString(href)))
	if err != nil {
		return "", err
	}
	resolved := baseURL.ResolveReference(ref)
	if !strings.EqualFold(resolved.Host, baseURL.Host) || (resolved.Scheme != "http" && resolved.Scheme != "https") {
		return "", fmt.Errorf("URL %q leaves board host %q", href, baseURL.Host)
	}
	resolved.Fragment = ""
	return resolved.String(), nil
}

func parsePostingDate(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		time.RFC3339Nano,
		"Mon Jan 02 15:04:05 MST 2006",
		"Jan 2, 2006",
		"January 2, 2006",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported posting date %q", raw)
}

type stringish string

func (s *stringish) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*s = ""
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*s = stringish(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err == nil {
		*s = stringish(number.String())
		return nil
	}
	return fmt.Errorf("expected string or number, got %s", data)
}

type flexibleInt int

func (n *flexibleInt) UnmarshalJSON(data []byte) error {
	var number int
	if err := json.Unmarshal(data, &number); err == nil {
		*n = flexibleInt(number)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("expected integer or integer string, got %s", data)
	}
	parsed, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("expected integer or integer string, got %q", text)
	}
	*n = flexibleInt(parsed)
	return nil
}

type structuredJobPosting struct {
	Title          string
	Description    string
	EmploymentType string
	DatePosted     string
	URL            string
	Location       string
}

func extractStructuredJobPosting(doc string) (structuredJobPosting, error) {
	for _, match := range jsonLDScript.FindAllStringSubmatch(doc, -1) {
		attrs := parseHTMLAttrs(match[1])
		if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(attrs["type"], ";")[0])); mediaType != "application/ld+json" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(strings.TrimSpace(match[2])), &value); err != nil {
			continue
		}
		if object := findJobPostingObject(value); object != nil {
			return structuredJobPosting{
				Title:          jsonString(object["title"]),
				Description:    jsonString(object["description"]),
				EmploymentType: jsonStrings(object["employmentType"]),
				DatePosted:     jsonString(object["datePosted"]),
				URL:            jsonString(object["url"]),
				Location:       jsonLocation(object),
			}, nil
		}
	}
	return structuredJobPosting{}, fmt.Errorf("no JobPosting JSON-LD found")
}

func findJobPostingObject(value any) map[string]any {
	switch value := value.(type) {
	case map[string]any:
		if jsonTypeContains(value["@type"], "JobPosting") {
			return value
		}
		if graph, ok := value["@graph"]; ok {
			if found := findJobPostingObject(graph); found != nil {
				return found
			}
		}
		for _, child := range value {
			if found := findJobPostingObject(child); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range value {
			if found := findJobPostingObject(child); found != nil {
				return found
			}
		}
	}
	return nil
}

func jsonTypeContains(value any, wanted string) bool {
	switch value := value.(type) {
	case string:
		trimmed := strings.TrimRight(strings.TrimSpace(value), "/")
		return strings.EqualFold(trimmed, wanted) || strings.EqualFold(trimmed[strings.LastIndex(trimmed, "/")+1:], wanted)
	case []any:
		for _, item := range value {
			if jsonTypeContains(item, wanted) {
				return true
			}
		}
	}
	return false
}

func jsonString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(html.UnescapeString(text))
}

func jsonStrings(value any) string {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value)
	case []any:
		var parts []string
		for _, item := range value {
			if text := jsonString(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func jsonLocation(posting map[string]any) string {
	var locations []string
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			address := value["address"]
			if text := jsonString(address); text != "" {
				locations = append(locations, text)
				return
			}
			if object, ok := address.(map[string]any); ok {
				var parts []string
				for _, key := range []string{"streetAddress", "addressLocality", "addressRegion", "postalCode", "addressCountry"} {
					part := jsonString(object[key])
					if country, ok := object[key].(map[string]any); ok && part == "" {
						part = jsonString(country["name"])
					}
					if part != "" && !strings.EqualFold(part, "unavailable") && !strings.EqualFold(part, "n/a") {
						parts = append(parts, part)
					}
				}
				if len(parts) > 0 {
					locations = append(locations, strings.Join(parts, ", "))
				}
			}
		}
	}
	walk(posting["jobLocation"])
	if len(locations) == 0 {
		if remote := jsonString(posting["jobLocationType"]); remote != "" {
			locations = append(locations, remote)
		}
	}
	return strings.Join(locations, " | ")
}

func microdataValue(doc, property string) string {
	for _, match := range htmlOpenTagRe.FindAllStringSubmatchIndex(doc, -1) {
		attrs := parseHTMLAttrs(doc[match[4]:match[5]])
		found := false
		for _, item := range strings.Fields(attrs["itemprop"]) {
			if item == property {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if content := strings.TrimSpace(attrs["content"]); content != "" {
			return html.UnescapeString(content)
		}
		tag := strings.ToLower(doc[match[2]:match[3]])
		if closeStart, _, ok := matchingHTMLClose(doc, tag, match[1]); ok {
			return cleanHTMLFragment(doc[match[1]:closeStart])
		}
	}
	return ""
}

func compactSpaces(text string) string {
	return strings.TrimSpace(spaceTextRe.ReplaceAllString(text, " "))
}
