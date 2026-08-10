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
	"regexp"
	"sort"
	"strings"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	enphaseBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	enphaseSnapshotAttempts = 3
)

var (
	enphaseJobviteIDRE   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	enphaseRequisitionRE = regexp.MustCompile(`^[0-9]+$`)
)

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
	Rows  []enphaseRow `json:"rows"`
	Pager struct {
		CurrentPage  flexibleInt `json:"current_page"`
		TotalItems   flexibleInt `json:"total_items"`
		TotalPages   flexibleInt `json:"total_pages"`
		ItemsPerPage flexibleInt `json:"items_per_page"`
	} `json:"pager"`
}

type enphaseRow struct {
	JID         string `json:"jid"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	ApplyURL    string `json:"applyUrl"`
	Description string `json:"description__value"`
	Location    string `json:"location"`
	Requisition string `json:"requisitionid"`
}

type enphasePager struct {
	total int
	pages int
	size  int
}

type enphaseCore struct {
	title       string
	category    string
	requisition string
	applyURL    string
	description string
}

type enphaseRawSignature struct {
	JID         string `json:"jid"`
	JobviteID   string `json:"jobvite_id"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Requisition string `json:"requisition"`
	ApplyURL    string `json:"apply_url"`
	Description string `json:"description"`
	Location    string `json:"location"`
}

type enphaseSnapshot struct {
	jobs          []model.Job
	pager         enphasePager
	fingerprint   string
	hasDuplicates bool
}

func (s enphaseSnapshot) sameContent(other enphaseSnapshot) bool {
	// Compare both the pager and the sorted, normalized raw rows. Comparing
	// only merged jobs would hide aliases appearing or disappearing between
	// otherwise coherent snapshots.
	return s.pager == other.pager && s.fingerprint == other.fingerprint
}

func (e *enphase) Fetch(ctx context.Context) ([]model.Job, error) {
	if e.maxPages <= 0 {
		return nil, fmt.Errorf("enphase: max_pages must be positive")
	}
	var previous *enphaseSnapshot
	requireStable := false
	lastReason := ""
	for attempt := 1; attempt <= enphaseSnapshotAttempts; attempt++ {
		snapshot, retryable, err := e.fetchSnapshot(ctx)
		if err != nil {
			if !retryable {
				return nil, err
			}
			requireStable = true
			previous = nil
			lastReason = err.Error()
			continue
		}
		if !requireStable && !snapshot.hasDuplicates {
			return snapshot.jobs, nil
		}
		requireStable = true
		if previous != nil && previous.sameContent(snapshot) {
			return snapshot.jobs, nil
		}
		current := snapshot
		previous = &current
		lastReason = "coherent snapshot did not repeat in two consecutive attempts"
	}
	return nil, fmt.Errorf("enphase: snapshot did not stabilize after %d attempts: %s", enphaseSnapshotAttempts, lastReason)
}

func (e *enphase) fetchSnapshot(ctx context.Context) (enphaseSnapshot, bool, error) {
	type group struct {
		core      enphaseCore
		locations map[string]struct{}
	}
	groups := make(map[string]*group)
	var order []string
	var signatures []string
	expected := enphasePager{total: -1, pages: -1, size: -1}
	rawRows := 0
	hasDuplicates := false

	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= e.maxPages {
			return enphaseSnapshot{}, false, fmt.Errorf("enphase: pagination exceeded max_pages=%d", e.maxPages)
		}
		endpoint := fmt.Sprintf("%s/api/v2/jobs?page=%d", e.base, pageNumber)
		var page enphaseResponse
		if err := e.fetchPage(ctx, endpoint, &page); err != nil {
			return enphaseSnapshot{}, false, fmt.Errorf("enphase page %d: %w", pageNumber, err)
		}
		current := int(page.Pager.CurrentPage)
		pager := enphasePager{
			total: int(page.Pager.TotalItems), pages: int(page.Pager.TotalPages), size: int(page.Pager.ItemsPerPage),
		}
		if current < 0 || pager.total < 0 || pager.pages < 0 || pager.size < 0 {
			return enphaseSnapshot{}, false, fmt.Errorf(
				"enphase page %d: invalid pager current=%d total_items=%d total_pages=%d items_per_page=%d",
				pageNumber, current, pager.total, pager.pages, pager.size,
			)
		}
		if pager.pages > e.maxPages {
			return enphaseSnapshot{}, false, fmt.Errorf("enphase page %d: total_pages=%d exceeds max_pages=%d", pageNumber, pager.pages, e.maxPages)
		}
		if pager.total == 0 {
			if pager.pages != 0 || len(page.Rows) != 0 {
				return enphaseSnapshot{}, true, fmt.Errorf("enphase page %d: zero total has %d pages and %d rows", pageNumber, pager.pages, len(page.Rows))
			}
			if current != pageNumber {
				return enphaseSnapshot{}, true, fmt.Errorf("enphase page %d: pager reported current_page=%d", pageNumber, current)
			}
			return enphaseSnapshot{jobs: []model.Job{}, pager: pager}, false, nil
		}
		if pager.pages <= 0 || pager.size <= 0 {
			return enphaseSnapshot{}, false, fmt.Errorf("enphase page %d: invalid pager total_pages=%d items_per_page=%d", pageNumber, pager.pages, pager.size)
		}
		if current != pageNumber {
			return enphaseSnapshot{}, true, fmt.Errorf("enphase page %d: pager reported current_page=%d", pageNumber, current)
		}
		if expected.total < 0 {
			expected = pager
			wantPages := 1 + (pager.total-1)/pager.size
			if pager.pages != wantPages {
				return enphaseSnapshot{}, true, fmt.Errorf("enphase page %d: pager reports %d pages, want %d for %d items at %d per page", pageNumber, pager.pages, wantPages, pager.total, pager.size)
			}
		} else if pager != expected {
			return enphaseSnapshot{}, true, fmt.Errorf("enphase page %d: pager changed during pagination", pageNumber)
		}
		wantRows := expected.size
		if pageNumber == expected.pages-1 {
			wantRows = expected.total - expected.size*(expected.pages-1)
		}
		if len(page.Rows) != wantRows {
			return enphaseSnapshot{}, true, fmt.Errorf("enphase page %d: got %d rows, want %d", pageNumber, len(page.Rows), wantRows)
		}

		for rowIndex, row := range page.Rows {
			jobviteID, core, location, signature, err := e.normalizeRow(row)
			if err != nil {
				return enphaseSnapshot{}, false, fmt.Errorf("enphase page %d row %d: %w", pageNumber, rowIndex, err)
			}
			encoded, _ := json.Marshal(signature)
			signatures = append(signatures, string(encoded))
			if existing, duplicate := groups[jobviteID]; duplicate {
				hasDuplicates = true
				if existing.core != core {
					return enphaseSnapshot{}, false, fmt.Errorf("enphase page %d: conflicting rows for canonical jid %q", pageNumber, jobviteID)
				}
				if location != "" {
					existing.locations[location] = struct{}{}
				}
				continue
			}
			locations := make(map[string]struct{})
			if location != "" {
				locations[location] = struct{}{}
			}
			groups[jobviteID] = &group{core: core, locations: locations}
			order = append(order, jobviteID)
		}
		rawRows += len(page.Rows)
		if pageNumber == expected.pages-1 {
			break
		}
	}
	if rawRows != expected.total {
		return enphaseSnapshot{}, true, fmt.Errorf("enphase: fetched %d raw rows, pager reported %d", rawRows, expected.total)
	}

	jobs := make([]model.Job, 0, len(order))
	for _, jobviteID := range order {
		entry := groups[jobviteID]
		locations := make([]string, 0, len(entry.locations))
		for location := range entry.locations {
			locations = append(locations, location)
		}
		sort.Strings(locations)
		jobs = append(jobs, model.Job{
			ID: "enphase/" + jobviteID, Company: e.company, Title: entry.core.title,
			Location: strings.Join(locations, "; "), URL: entry.core.applyURL, Description: entry.core.description,
		})
	}
	sort.Strings(signatures)
	return enphaseSnapshot{
		jobs: jobs, pager: expected, fingerprint: strings.Join(signatures, "\n"), hasDuplicates: hasDuplicates,
	}, false, nil
}

func (e *enphase) normalizeRow(row enphaseRow) (string, enphaseCore, string, enphaseRawSignature, error) {
	jid := strings.TrimSpace(row.JID)
	title := strings.TrimSpace(row.Name)
	if jid == "" || title == "" {
		return "", enphaseCore{}, "", enphaseRawSignature{}, fmt.Errorf("missing jid or name")
	}
	applyURL, jobviteID, err := normalizeEnphaseApplyURL(row.ApplyURL, jid)
	if err != nil {
		return "", enphaseCore{}, "", enphaseRawSignature{}, err
	}
	requisition := strings.TrimSpace(row.Requisition)
	if !enphaseRequisitionRE.MatchString(requisition) {
		return "", enphaseCore{}, "", enphaseRawSignature{}, fmt.Errorf("jid %s has invalid requisitionid %q", jid, row.Requisition)
	}
	description := strings.TrimSpace(htmltext.ToText(html.UnescapeString(row.Description)))
	if description == "" {
		return "", enphaseCore{}, "", enphaseRawSignature{}, fmt.Errorf("jid %s has missing description", jid)
	}
	category := strings.TrimSpace(row.Category)
	location := strings.TrimSpace(row.Location)
	core := enphaseCore{
		title: title, category: category, requisition: requisition, applyURL: applyURL, description: description,
	}
	return jobviteID, core, location, enphaseRawSignature{
		JID: jid, JobviteID: jobviteID, Title: title, Category: category,
		Requisition: requisition, ApplyURL: applyURL, Description: description, Location: location,
	}, nil
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

func normalizeEnphaseApplyURL(raw, jid string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
	if err != nil {
		return "", "", fmt.Errorf("invalid apply URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", fmt.Errorf("apply URL has unsupported scheme %q", parsed.Scheme)
	}
	if parsed.User != nil || parsed.Port() != "" || !strings.EqualFold(parsed.Hostname(), "app.jobvite.com") {
		return "", "", fmt.Errorf("apply URL is not on app.jobvite.com")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", "", fmt.Errorf("invalid apply URL query: %w", err)
	}
	jobviteIDs := query["j"]
	if len(jobviteIDs) != 1 || !enphaseJobviteIDRE.MatchString(jobviteIDs[0]) {
		return "", "", fmt.Errorf("apply URL must contain exactly one safe nonempty j")
	}
	jobviteID := jobviteIDs[0]
	if jid != jobviteID {
		suffix, ok := strings.CutPrefix(jid, jobviteID+"-")
		if !ok || !enphaseJobviteIDRE.MatchString(suffix) {
			return "", "", fmt.Errorf("apply URL j %q does not match jid %q", jobviteID, jid)
		}
	}
	parsed.Scheme = "https"
	parsed.Host = "app.jobvite.com"
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String(), jobviteID, nil
}
