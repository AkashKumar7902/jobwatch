package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Some job boards reject requests without a browser-ish User-Agent.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) jobwatch/1.0"

// fetchJSON performs an HTTP request and decodes the JSON response into
// out. body may be nil for GET requests.
func fetchJSON(ctx context.Context, client *http.Client, method, url string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, bytes.TrimSpace(snippet))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", method, url, err)
	}
	return nil
}
