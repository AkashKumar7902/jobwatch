package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	customListBodyLimit   = 12 << 20
	customDetailBodyLimit = 6 << 20
)

var htmlAnchorRE = regexp.MustCompile(`(?is)<a\b[^>]*>.*?</a>`)

func fetchHTML(ctx context.Context, client *http.Client, endpoint string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: response exceeds %d-byte safety limit", endpoint, limit)
	}
	return body, nil
}

func validateHostParam(name, host string) error {
	if host == "" {
		return fmt.Errorf("missing required param %q", name)
	}
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host != host || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("param %q: invalid host %q", name, host)
	}
	return nil
}

func resolveReference(baseURL, ref string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	relative, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", err
	}
	if relative.Scheme != "" && relative.Scheme != "http" && relative.Scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme %q", relative.Scheme)
	}
	return base.ResolveReference(relative).String(), nil
}

func htmlAttribute(tag, name string) string {
	if end := strings.IndexByte(tag, '>'); end >= 0 {
		tag = tag[:end]
	}
	re := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(name) + `\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	match := re.FindStringSubmatch(tag)
	if match == nil {
		return ""
	}
	if match[1] != "" {
		return match[1]
	}
	return match[2]
}

func hasHTMLClass(tag, class string) bool {
	for _, candidate := range strings.Fields(htmlAttribute(tag, "class")) {
		if candidate == class {
			return true
		}
	}
	return false
}

func joinDescriptionParts(parts ...string) string {
	cleaned := distinctStrings(parts)
	return strings.Join(cleaned, "\n\n")
}
