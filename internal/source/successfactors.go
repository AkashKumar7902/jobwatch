package source

// SAP SuccessFactors career sites render their paged result set server-side.
// Detail pages expose JobPosting metadata as JSON-LD on some versions and as
// schema.org microdata on others.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var (
	successFactorsPageRe = regexp.MustCompile(`(?i)Page\s+([0-9]+)\s+of\s+([0-9]+)\s*,\s*Results\s+([0-9]+)\s+to\s+([0-9]+)\s+of\s+([0-9]+)`)
	successFactorsIDRe   = regexp.MustCompile(`/([0-9]+)/?$`)
)

const (
	successFactorsSnapshotAttempts = 3
	successFactorsSnapshotDelay    = 250 * time.Millisecond
)

func init() {
	Register("successfactors", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawHost, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		host, err := normalizeBoardHost(rawHost)
		if err != nil {
			return nil, err
		}
		maxPages, err := positiveCappedParam(p, "max_pages", 100, 500)
		if err != nil {
			return nil, err
		}
		return &successFactors{
			company: company, host: host, base: "https://" + host,
			maxPages: maxPages, snapshotDelay: successFactorsSnapshotDelay, client: client,
		}, nil
	})
}

type successFactors struct {
	company  string
	host     string
	base     string
	maxPages int
	// snapshotDelay is nonzero for production sources and left at zero by
	// unit tests. Full traversals already take time; this short, bounded pause
	// only gives a changing offset result set a chance to settle before retry.
	snapshotDelay time.Duration
	client        *http.Client
}

func (s *successFactors) Company() string { return s.company }

func (s *successFactors) Fetch(ctx context.Context) ([]model.Job, error) {
	if s.maxPages <= 0 {
		return nil, fmt.Errorf("successfactors %s: max_pages must be positive", s.host)
	}

	// SuccessFactors exposes offset pagination without a snapshot token. A
	// posting added, removed, or reordered while pages are being read can move
	// another posting across an offset. Duplicates expose one direction of that
	// drift, but a removal before the cursor can omit a still-open posting while
	// leaving the final unique count equal to the newly reported total. Accept
	// only two consecutive, complete traversals with identical normalized
	// posting sets. Compare by ID rather than row order because referencedate
	// ties do not have a documented secondary sort.
	var (
		previous     []model.Job
		havePrevious bool
	)
	lastReason := "complete snapshot did not repeat in two consecutive attempts"
	for attempt := 1; attempt <= successFactorsSnapshotAttempts; attempt++ {
		shouldBackoff := false
		snapshot, err := s.fetchSnapshot(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var retryable *successFactorsRetryableError
			if !errors.As(err, &retryable) {
				return nil, err
			}
			previous = nil
			havePrevious = false
			lastReason = err.Error()
			shouldBackoff = true
			if attempt < successFactorsSnapshotAttempts {
				diagnostic.Retry(ctx, retryable.kind, attempt, successFactorsSnapshotAttempts, s.snapshotDelay)
			}
		} else {
			if havePrevious && sameSuccessFactorsSnapshot(previous, snapshot) {
				return snapshot, nil
			}
			if havePrevious {
				lastReason = fmt.Sprintf(
					"complete posting set changed between traversals (%d then %d jobs)",
					len(previous), len(snapshot),
				)
				shouldBackoff = true
				if attempt < successFactorsSnapshotAttempts {
					diagnostic.Retry(
						ctx, diagnostic.RetrySnapshot, attempt,
						successFactorsSnapshotAttempts, s.snapshotDelay,
					)
				}
			}
			previous = snapshot
			havePrevious = true
		}

		if attempt < successFactorsSnapshotAttempts && shouldBackoff {
			if err := waitSuccessFactorsSnapshot(ctx, s.snapshotDelay); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf(
		"successfactors %s: snapshot did not stabilize after %d attempts: %s",
		s.host, successFactorsSnapshotAttempts, lastReason,
	)
}

type successFactorsRetryableError struct {
	kind diagnostic.RetryKind
	err  error
}

func (e *successFactorsRetryableError) Error() string { return e.err.Error() }
func (e *successFactorsRetryableError) Unwrap() error { return e.err }

func retryableSuccessFactorsError(kind diagnostic.RetryKind, format string, args ...any) error {
	return &successFactorsRetryableError{kind: kind, err: fmt.Errorf(format, args...)}
}

func waitSuccessFactorsSnapshot(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sameSuccessFactorsSnapshot(a, b []model.Job) bool {
	if len(a) != len(b) {
		return false
	}
	byID := make(map[string]model.Job, len(a))
	for _, job := range a {
		byID[job.ID] = job
	}
	for _, job := range b {
		previous, ok := byID[job.ID]
		if !ok || previous != job {
			return false
		}
	}
	return true
}

func (s *successFactors) fetchSnapshot(ctx context.Context) ([]model.Job, error) {
	// SuccessFactors sets JSESSIONID on the first search response. Without a
	// CookieJar, every page can land on a different backend snapshot; in live
	// traffic that made totals alternate between 823 and 824 within one sweep.
	// Keep one fresh sticky session for this traversal, and deliberately do not
	// reuse it for the next traversal whose independent agreement is the proof.
	baseClient := s.client
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	snapshotClient := *baseClient
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("successfactors %s: creating snapshot cookie jar: %w", s.host, err)
	}
	snapshotClient.Jar = jar

	var jobs []model.Job
	seen := make(map[string]struct{})
	startRow := 0
	expectedPage := 1
	expectedPages := -1
	expectedTotal := -1
	for pageAttempt := 1; pageAttempt <= s.maxPages; pageAttempt++ {
		query := url.Values{
			"q":             {""},
			"sortColumn":    {"referencedate"},
			"sortDirection": {"desc"},
			"startrow":      {strconv.Itoa(startRow)},
		}
		endpoint := s.base + "/search/?" + query.Encode()
		body, err := fetchHTMLPage(ctx, &snapshotClient, endpoint, nil)
		if err != nil {
			var netErr net.Error
			if ctx.Err() == nil && errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
				return nil, retryableSuccessFactorsError(diagnostic.RetryTransport,
					"successfactors %s page %d: transient transport: %w", s.host, expectedPage, err,
				)
			}
			return nil, fmt.Errorf("successfactors %s page %d: %w", s.host, expectedPage, err)
		}
		doc := string(body)
		pageMatch := successFactorsPageRe.FindStringSubmatch(doc)
		if pageMatch == nil {
			if startRow == 0 && len(htmlElementsByClass(doc, "data-row")) == 0 &&
				(strings.Contains(strings.ToLower(doc), "no jobs") || strings.Contains(strings.ToLower(doc), "0 results")) {
				return []model.Job{}, nil
			}
			return nil, fmt.Errorf("successfactors %s page %d: missing pagination metadata", s.host, expectedPage)
		}
		pageNumber, _ := strconv.Atoi(pageMatch[1])
		pageCount, _ := strconv.Atoi(pageMatch[2])
		firstResult, _ := strconv.Atoi(pageMatch[3])
		lastResult, _ := strconv.Atoi(pageMatch[4])
		totalResults, _ := strconv.Atoi(pageMatch[5])
		if totalResults == 0 {
			if startRow != 0 || len(htmlElementsByClass(doc, "data-row")) != 0 {
				return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
					"successfactors %s: zero-result pagination contains job rows", s.host,
				)
			}
			return []model.Job{}, nil
		}
		if pageNumber != expectedPage || pageCount < pageNumber || firstResult != startRow+1 ||
			lastResult < firstResult || lastResult > totalResults {
			return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
				"successfactors %s: inconsistent pagination page=%d/%d results=%d-%d/%d for startrow=%d",
				s.host, pageNumber, pageCount, firstResult, lastResult, totalResults, startRow,
			)
		}
		if expectedTotal < 0 {
			expectedTotal = totalResults
			expectedPages = pageCount
			if expectedPages > s.maxPages {
				return nil, fmt.Errorf(
					"successfactors %s: pagination reports %d pages, exceeds max_pages=%d",
					s.host, expectedPages, s.maxPages,
				)
			}
		} else if totalResults != expectedTotal || pageCount != expectedPages {
			return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
				"successfactors %s page %d: pagination changed from %d pages/%d jobs to %d pages/%d jobs",
				s.host, pageNumber, expectedPages, expectedTotal, pageCount, totalResults,
			)
		}
		rows := htmlElementsByClass(doc, "data-row")
		if len(rows) == 0 {
			return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
				"successfactors %s page %d: pagination reports jobs but no data rows were found", s.host, pageNumber,
			)
		}
		if len(rows) != lastResult-firstResult+1 {
			return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
				"successfactors %s page %d: found %d rows for result range %d-%d", s.host, pageNumber, len(rows), firstResult, lastResult,
			)
		}
		for rowIndex, row := range rows {
			var jobAnchor *htmlElement
			for _, anchor := range htmlAnchors(row.inner) {
				if hasClass(anchor.attrs, "jobTitle-link") {
					copy := anchor
					jobAnchor = &copy
					break
				}
			}
			if jobAnchor == nil {
				return nil, fmt.Errorf("successfactors %s page %d row %d: missing job link", s.host, pageNumber, rowIndex)
			}
			jobURL, err := resolveBoardURL(s.base, jobAnchor.attrs["href"])
			if err != nil {
				return nil, fmt.Errorf("successfactors %s page %d row %d: %w", s.host, pageNumber, rowIndex, err)
			}
			parsedURL, _ := url.Parse(jobURL)
			parsedURL.RawQuery = ""
			jobURL = parsedURL.String()
			idMatch := successFactorsIDRe.FindStringSubmatch(parsedURL.Path)
			title := cleanHTMLFragment(jobAnchor.inner)
			if idMatch == nil || title == "" {
				return nil, fmt.Errorf("successfactors %s page %d row %d: missing numeric job ID or title", s.host, pageNumber, rowIndex)
			}
			id := idMatch[1]
			if _, duplicate := seen[id]; duplicate {
				return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
					"successfactors %s page %d: duplicate job ID %q", s.host, pageNumber, id,
				)
			}
			seen[id] = struct{}{}
			location := ""
			if element, ok := firstHTMLClass(row.inner, "jobLocation"); ok {
				location = cleanHTMLFragment(element.inner)
			}
			var postedAtRaw string
			if element, ok := firstHTMLClass(row.inner, "jobDate"); ok {
				postedAtRaw = cleanHTMLFragment(element.inner)
			}
			postedAt, err := parsePostingDate(postedAtRaw)
			if err != nil {
				return nil, fmt.Errorf("successfactors %s job %s: %w", s.host, id, err)
			}
			jobs = append(jobs, model.Job{
				ID:       fmt.Sprintf("successfactors/%s/%s", s.host, id),
				Company:  s.company,
				Title:    title,
				Location: location,
				URL:      jobURL,
				PostedAt: postedAt,
			})
		}
		if lastResult == totalResults {
			if pageNumber != pageCount {
				return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
					"successfactors %s page %d: result range ended at %d jobs before reported final page %d",
					s.host, pageNumber, totalResults, pageCount,
				)
			}
			if len(jobs) != totalResults {
				return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
					"successfactors %s: parsed %d unique jobs, pagination reported %d", s.host, len(jobs), totalResults,
				)
			}
			return jobs, nil
		}
		if pageNumber == pageCount {
			return nil, retryableSuccessFactorsError(diagnostic.RetrySnapshot,
				"successfactors %s page %d: last page ended at %d of %d", s.host, pageNumber, lastResult, totalResults,
			)
		}
		startRow = lastResult
		expectedPage++
	}
	return nil, fmt.Errorf("successfactors %s: pagination exceeded max_pages=%d", s.host, s.maxPages)
}

func (s *successFactors) Detail(ctx context.Context, job *model.Job) error {
	prefix := "successfactors/" + s.host + "/"
	if job == nil || !strings.HasPrefix(job.ID, prefix) {
		return fmt.Errorf("successfactors %s: job ID does not belong to this board", s.host)
	}
	id := strings.TrimPrefix(job.ID, prefix)
	if id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("successfactors %s: invalid job ID %q", s.host, job.ID)
	}
	detailURL, err := resolveBoardURL(s.base, job.URL)
	if err != nil {
		return fmt.Errorf("successfactors %s job %s: %w", s.host, id, err)
	}
	parsedURL, _ := url.Parse(detailURL)
	if match := successFactorsIDRe.FindStringSubmatch(parsedURL.Path); match == nil || match[1] != id {
		return fmt.Errorf("successfactors %s job %s: detail URL does not contain the job ID", s.host, id)
	}
	body, err := fetchHTMLPage(ctx, s.client, detailURL, nil)
	if err != nil {
		return fmt.Errorf("successfactors %s job %s detail: %w", s.host, id, err)
	}
	doc := string(body)
	posting, structuredErr := extractStructuredJobPosting(doc)
	if structuredErr != nil || strings.TrimSpace(posting.Description) == "" {
		posting = structuredJobPosting{
			Title:          microdataValue(doc, "title"),
			EmploymentType: microdataValue(doc, "employmentType"),
			DatePosted:     microdataValue(doc, "datePosted"),
		}
		if description, ok := firstHTMLClass(doc, "jobdescription"); ok {
			posting.Description = description.inner
		}
		var locationParts []string
		for _, property := range []string{"streetAddress", "addressLocality", "addressRegion", "addressCountry"} {
			value := compactSpaces(microdataValue(doc, property))
			if value != "" && !containsText(locationParts, value) {
				locationParts = append(locationParts, value)
			}
		}
		posting.Location = strings.Join(locationParts, ", ")
	}
	description := cleanHTMLFragment(posting.Description)
	if description == "" {
		return fmt.Errorf("successfactors %s job %s detail: missing description", s.host, id)
	}
	postedAt, err := parsePostingDate(posting.DatePosted)
	if err != nil {
		return fmt.Errorf("successfactors %s job %s detail: %w", s.host, id, err)
	}
	updated := *job
	if title := compactSpaces(posting.Title); title != "" {
		updated.Title = title
	}
	updated.Description = description
	if location := compactSpaces(posting.Location); location != "" {
		updated.Location = location
	}
	if employmentType := compactSpaces(posting.EmploymentType); employmentType != "" {
		updated.EmploymentType = employmentType
	}
	if !postedAt.IsZero() {
		updated.PostedAt = postedAt
	}
	*job = updated
	return nil
}

func containsText(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted || strings.Contains(value, wanted) {
			return true
		}
	}
	return false
}
