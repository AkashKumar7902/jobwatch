package source

// Enphase's careers API returns complete postings as JSON, but its edge
// configuration requires the same Origin and Referer headers as the careers
// page. Pagination metadata is validated so a changed Drupal view cannot
// silently truncate the board.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const enphaseBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"

func init() {
	Register("enphase", func(company string, p params.Map, client *http.Client) (Source, error) {
		maxPages, err := positiveCappedParam(p, "max_pages", 50, 100)
		if err != nil {
			return nil, err
		}
		return &enphase{
			company: company, base: "https://enphase.com",
			maxPages: maxPages, client: client,
		}, nil
	})
}

type enphase struct {
	company  string
	base     string
	maxPages int
	client   *http.Client
}

func (e *enphase) Company() string { return e.company }

type enphaseResponse struct {
	Rows []struct {
		JID         string `json:"jid"`
		Name        string `json:"name"`
		Category    string `json:"category"`
		ApplyURL    string `json:"applyUrl"`
		Description string `json:"description__value"`
		Location    string `json:"location"`
		Requisition string `json:"requisitionid"`
	} `json:"rows"`
	Pager struct {
		CurrentPage  flexibleInt `json:"current_page"`
		TotalItems   flexibleInt `json:"total_items"`
		TotalPages   flexibleInt `json:"total_pages"`
		ItemsPerPage flexibleInt `json:"items_per_page"`
	} `json:"pager"`
}

func (e *enphase) Fetch(ctx context.Context) ([]model.Job, error) {
	if e.maxPages <= 0 {
		return nil, fmt.Errorf("enphase: max_pages must be positive")
	}
	var jobs []model.Job
	seen := make(map[string]struct{})
	expectedTotal := -1
	expectedPages := -1
	expectedPageSize := -1
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= e.maxPages {
			return nil, fmt.Errorf("enphase: pagination exceeded max_pages=%d after %d postings", e.maxPages, len(jobs))
		}
		endpoint := fmt.Sprintf("%s/api/v2/jobs?page=%d", e.base, pageNumber)
		var page enphaseResponse
		if err := e.fetchPage(ctx, endpoint, &page); err != nil {
			return nil, fmt.Errorf("enphase page %d: %w", pageNumber, err)
		}
		current := int(page.Pager.CurrentPage)
		total := int(page.Pager.TotalItems)
		totalPages := int(page.Pager.TotalPages)
		pageSize := int(page.Pager.ItemsPerPage)
		if current != pageNumber || total < 0 || totalPages < 0 || pageSize < 0 {
			return nil, fmt.Errorf(
				"enphase page %d: invalid pager current=%d total_items=%d total_pages=%d items_per_page=%d",
				pageNumber, current, total, totalPages, pageSize,
			)
		}
		if total == 0 {
			if totalPages != 0 || len(page.Rows) != 0 {
				return nil, fmt.Errorf("enphase page %d: zero total has %d pages and %d rows", pageNumber, totalPages, len(page.Rows))
			}
			return []model.Job{}, nil
		}
		if totalPages <= 0 || pageSize <= 0 || totalPages > e.maxPages {
			return nil, fmt.Errorf("enphase page %d: invalid or capped pager total_pages=%d items_per_page=%d", pageNumber, totalPages, pageSize)
		}
		if expectedTotal < 0 {
			expectedTotal, expectedPages, expectedPageSize = total, totalPages, pageSize
		} else if total != expectedTotal || totalPages != expectedPages || pageSize != expectedPageSize {
			return nil, fmt.Errorf("enphase page %d: pager changed during pagination", pageNumber)
		}
		if len(page.Rows) == 0 || len(page.Rows) > pageSize {
			return nil, fmt.Errorf("enphase page %d: got %d rows for page size %d", pageNumber, len(page.Rows), pageSize)
		}
		if pageNumber < totalPages-1 && len(page.Rows) != pageSize {
			return nil, fmt.Errorf("enphase page %d: short non-final page (%d of %d)", pageNumber, len(page.Rows), pageSize)
		}
		for rowIndex, row := range page.Rows {
			jid := strings.TrimSpace(row.JID)
			title := strings.TrimSpace(row.Name)
			if jid == "" || title == "" {
				return nil, fmt.Errorf("enphase page %d row %d: missing jid or name", pageNumber, rowIndex)
			}
			if _, duplicate := seen[jid]; duplicate {
				return nil, fmt.Errorf("enphase page %d: duplicate jid %q", pageNumber, jid)
			}
			applyURL, err := normalizeEnphaseApplyURL(row.ApplyURL, jid)
			if err != nil {
				return nil, fmt.Errorf("enphase page %d jid %s: %w", pageNumber, jid, err)
			}
			description := strings.TrimSpace(htmltext.ToText(html.UnescapeString(row.Description)))
			if description == "" {
				return nil, fmt.Errorf("enphase page %d jid %s: missing description", pageNumber, jid)
			}
			seen[jid] = struct{}{}
			jobs = append(jobs, model.Job{
				ID:          "enphase/" + jid,
				Company:     e.company,
				Title:       title,
				Location:    strings.TrimSpace(row.Location),
				URL:         applyURL,
				Description: description,
			})
		}
		if pageNumber == totalPages-1 {
			if len(jobs) != total {
				return nil, fmt.Errorf("enphase: parsed %d unique jobs, pager reported %d", len(jobs), total)
			}
			return jobs, nil
		}
	}
}

func (e *enphase) fetchPage(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", enphaseBrowserUserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", e.base)
	req.Header.Set("Referer", e.base+"/careers")
	client := e.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, htmlBodyLimit+1))
	if err != nil {
		return fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if len(body) > htmlBodyLimit {
		return fmt.Errorf("GET %s: response exceeds %d bytes", endpoint, htmlBodyLimit)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: decoding response: %w", endpoint, err)
	}
	return nil
}

func normalizeEnphaseApplyURL(raw, jid string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
	if err != nil {
		return "", fmt.Errorf("invalid apply URL: %w", err)
	}
	if !strings.EqualFold(parsed.Hostname(), "app.jobvite.com") || parsed.Query().Get("j") != jid {
		return "", fmt.Errorf("apply URL does not identify jid %q on app.jobvite.com", jid)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("apply URL has unsupported scheme %q", parsed.Scheme)
	}
	parsed.Scheme = "https"
	parsed.Fragment = ""
	return parsed.String(), nil
}
