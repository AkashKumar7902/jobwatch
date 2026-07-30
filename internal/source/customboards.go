package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const customBoardHTMLLimit = 8 << 20

var (
	customBoardUUIDRE = regexp.MustCompile(`^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$`)
	nextFlightPushRE  = regexp.MustCompile(`(?s)self\.__next_f\.push\(\[1,("(?:\\.|[^"\\])*")\]\)`)
)

func fetchCustomBoardHTML(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
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
	return readCustomBoardHTML(resp.Body, endpoint)
}

func readCustomBoardHTML(r io.Reader, endpoint string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, customBoardHTMLLimit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if len(body) > customBoardHTMLLimit {
		return nil, fmt.Errorf("GET %s: HTML response exceeds %d bytes", endpoint, customBoardHTMLLimit)
	}
	return body, nil
}

func extractNextFlightStrings(document string) ([]string, error) {
	matches := nextFlightPushRE.FindAllStringSubmatch(document, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no Next.js flight payload found")
	}
	parts := make([]string, 0, len(matches))
	for i, match := range matches {
		var part string
		if err := json.Unmarshal([]byte(match[1]), &part); err != nil {
			return nil, fmt.Errorf("decoding Next.js flight segment %d: %w", i, err)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func jsonArrayAfter(s, marker string) ([]byte, error) {
	markerAt := strings.Index(s, marker)
	if markerAt < 0 {
		return nil, fmt.Errorf("marker %q not found", marker)
	}
	arrayAt := strings.IndexByte(s[markerAt+len(marker):], '[')
	if arrayAt < 0 {
		return nil, fmt.Errorf("JSON array after marker %q not found", marker)
	}
	arrayAt += markerAt + len(marker)

	depth := 0
	inString := false
	escaped := false
	for i := arrayAt; i < len(s); i++ {
		switch {
		case inString && escaped:
			escaped = false
		case inString && s[i] == '\\':
			escaped = true
		case s[i] == '"':
			inString = !inString
		case !inString && s[i] == '[':
			depth++
		case !inString && s[i] == ']':
			depth--
			if depth == 0 {
				return []byte(s[arrayAt : i+1]), nil
			}
		}
	}
	return nil, fmt.Errorf("unterminated JSON array after marker %q", marker)
}
