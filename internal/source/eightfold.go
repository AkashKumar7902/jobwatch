package source

// Eightfold PCS public careers API (no auth).
//
//	GET https://{host}/api/pcsx/search?domain={domain}&query=&location={location}&start=N
//	GET https://{host}/api/pcsx/position_details?position_id={id}&domain={domain}&hl=en
//
// Search results are paged ten at a time. Full descriptions are deliberately
// fetched lazily through Detail.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	eightfoldPageSize        = 10
	eightfoldMaxAttempts     = 3
	eightfoldMaxRetryAfter   = 60 * time.Second
	eightfoldDefaultRetryGap = 250 * time.Millisecond
)

func init() {
	Register("eightfold", func(company string, p params.Map, client *http.Client) (Source, error) {
		host, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		if err := validateHostParam("host", host); err != nil {
			return nil, err
		}
		domain, err := p.Require("domain")
		if err != nil {
			return nil, err
		}
		if err := validateHostParam("domain", domain); err != nil {
			return nil, err
		}
		maxPostings, err := p.Int("max_postings", 1000)
		if err != nil {
			return nil, err
		}
		if maxPostings <= 0 {
			return nil, fmt.Errorf("param %q: expected a positive integer, got %d", "max_postings", maxPostings)
		}
		return &eightfold{
			company: company, host: host, domain: domain, location: strings.TrimSpace(p.Get("location")),
			query:       strings.TrimSpace(p.Get("query")),
			base:        "https://" + host,
			keyPrefix:   "eightfold/" + domain + "/",
			maxPostings: maxPostings, client: client, retryGap: eightfoldDefaultRetryGap,
		}, nil
	})
}

type eightfold struct {
	company  string
	host     string
	domain   string
	location string
	query    string
	base     string
	// keyPrefix excludes the host. Eightfold serves the same board from a
	// vanity domain (jobs.ericsson.com) or its own (morganstanley.eightfold.ai)
	// and customers move between them; `domain` is the employer key Eightfold's
	// API itself is queried with, so it is what survives.
	// Must stay equal to source.StatePrefix for these params.
	keyPrefix   string // eightfold/{domain}/
	maxPostings int
	client      *http.Client
	retryGap    time.Duration
}

type eightfoldPosition struct {
	ID                    int64    `json:"id"`
	DisplayJobID          string   `json:"displayJobId"`
	Name                  string   `json:"name"`
	Locations             []string `json:"locations"`
	StandardizedLocations []string `json:"standardizedLocations"`
	PostedTS              int64    `json:"postedTs"`
	Department            string   `json:"department"`
	ATSJobID              string   `json:"atsJobId"`
	PositionURL           string   `json:"positionUrl"`
}

type eightfoldSearchResponse struct {
	Status int `json:"status"`
	Error  struct {
		Message string `json:"message"`
		Body    string `json:"body"`
	} `json:"error"`
	Data struct {
		Count     *int                 `json:"count"`
		Positions *[]eightfoldPosition `json:"positions"`
	} `json:"data"`
}

func (s *eightfold) Company() string { return s.company }

func (s *eightfold) Fetch(ctx context.Context) ([]model.Job, error) {
	var previous *eightfoldSnapshot
	var lastErr error
	stabilizing := false

	for attempt := 1; attempt <= eightfoldMaxAttempts; attempt++ {
		snapshot, err := s.fetchSnapshot(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var retryable *eightfoldRetryableError
			if !errors.As(err, &retryable) {
				return nil, err
			}
			lastErr = err
			stabilizing = true
			// A failed traversal breaks consecutiveness. No records from a
			// partial attempt are ever merged into a later snapshot.
			previous = nil
			if attempt == eightfoldMaxAttempts {
				break
			}
			delay := s.retryDelay(attempt, retryable.retryAfter)
			diagnostic.Retry(ctx, retryable.kind, attempt, eightfoldMaxAttempts, delay)
			if err := eightfoldWait(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}

		// Preserve the fast path for a clean, internally coherent traversal.
		// Once any instability or exact duplicate has been observed, however,
		// only two consecutive identical full traversals are trustworthy.
		if !stabilizing && !snapshot.hadExactDuplicate {
			return s.finishSnapshot(ctx, snapshot), nil
		}
		if previous != nil && bytes.Equal(previous.fingerprint, snapshot.fingerprint) {
			return s.finishSnapshot(ctx, snapshot), nil
		}

		if previous == nil {
			lastErr = fmt.Errorf("eightfold %s: coherent snapshot requires a consecutive confirmation", s.host)
		} else {
			lastErr = fmt.Errorf("eightfold %s: coherent snapshot changed between consecutive traversals", s.host)
		}
		stabilizing = true
		copyOfSnapshot := snapshot
		previous = &copyOfSnapshot
		if attempt == eightfoldMaxAttempts {
			break
		}
		delay := s.retryDelay(attempt, 0)
		diagnostic.Retry(ctx, diagnostic.RetrySnapshot, attempt, eightfoldMaxAttempts, delay)
		if err := eightfoldWait(ctx, delay); err != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf(
		"eightfold %s: no stable snapshot after %d attempts: %w",
		s.host, eightfoldMaxAttempts, lastErr,
	)
}

type eightfoldSnapshot struct {
	jobs              []model.Job
	fingerprint       []byte
	expectedTotal     int
	hadExactDuplicate bool
}

type eightfoldRetryableError struct {
	err        error
	kind       diagnostic.RetryKind
	retryAfter time.Duration
}

func (e *eightfoldRetryableError) Error() string { return e.err.Error() }
func (e *eightfoldRetryableError) Unwrap() error { return e.err }

func eightfoldRetryable(err error, kind diagnostic.RetryKind) error {
	return &eightfoldRetryableError{err: err, kind: kind}
}

func (s *eightfold) fetchSnapshot(ctx context.Context) (eightfoldSnapshot, error) {
	expectedTotal := -1
	jobs := make([]model.Job, 0)
	seen := make(map[int64][]byte)
	rawPositions := make([]eightfoldPosition, 0)
	hadExactDuplicate := false

	for start := 0; start < s.maxPostings; {
		query := url.Values{
			"domain": {s.domain},
			"query":  {s.query},
			"start":  {strconv.Itoa(start)},
		}
		if s.location != "" {
			query.Set("location", s.location)
		}
		var page eightfoldSearchResponse
		endpoint := s.base + "/api/pcsx/search?" + query.Encode()
		if err := s.fetchSearchPage(ctx, endpoint, &page); err != nil {
			return eightfoldSnapshot{}, fmt.Errorf("eightfold %s: page at start %d: %w", s.host, start, err)
		}
		if page.Status != http.StatusOK {
			err := fmt.Errorf("eightfold %s: page at start %d reported status %d (%s)", s.host, start, page.Status, page.Error.Message)
			if page.Status == http.StatusTooManyRequests {
				return eightfoldSnapshot{}, eightfoldRetryable(err, diagnostic.RetryRateLimit)
			}
			if page.Status >= 500 && page.Status <= 599 {
				return eightfoldSnapshot{}, eightfoldRetryable(err, diagnostic.RetryServer)
			}
			return eightfoldSnapshot{}, err
		}
		if page.Data.Count == nil || page.Data.Positions == nil {
			return eightfoldSnapshot{}, fmt.Errorf("eightfold %s: page at start %d omitted count or positions", s.host, start)
		}
		if *page.Data.Count < 0 {
			return eightfoldSnapshot{}, fmt.Errorf("eightfold %s: page at start %d reported negative count", s.host, start)
		}
		if expectedTotal < 0 {
			expectedTotal = *page.Data.Count
		} else if *page.Data.Count != expectedTotal {
			return eightfoldSnapshot{}, eightfoldRetryable(fmt.Errorf(
				"eightfold %s: count changed from %d to %d at start %d",
				s.host, expectedTotal, *page.Data.Count, start,
			), diagnostic.RetrySnapshot)
		}

		positions := *page.Data.Positions
		if len(positions) > eightfoldPageSize {
			return eightfoldSnapshot{}, fmt.Errorf("eightfold %s: page at start %d returned %d positions, safety limit is %d", s.host, start, len(positions), eightfoldPageSize)
		}
		if len(positions) == 0 {
			if start < expectedTotal {
				return eightfoldSnapshot{}, eightfoldRetryable(fmt.Errorf(
					"eightfold %s: empty page at start %d before reported total %d",
					s.host, start, expectedTotal,
				), diagnostic.RetrySnapshot)
			}
			break
		}

		for _, posting := range positions {
			if len(jobs) >= s.maxPostings {
				break
			}
			encoded, err := json.Marshal(posting)
			if err != nil {
				return eightfoldSnapshot{}, fmt.Errorf("eightfold %s: encode position %d for consistency: %w", s.host, posting.ID, err)
			}
			rawPositions = append(rawPositions, posting)
			if prior, duplicate := seen[posting.ID]; duplicate {
				if !bytes.Equal(prior, encoded) {
					return eightfoldSnapshot{}, eightfoldRetryable(fmt.Errorf(
						"eightfold %s: conflicting duplicate position id %d",
						s.host, posting.ID,
					), diagnostic.RetrySnapshot)
				}
				hadExactDuplicate = true
				continue
			}
			job, err := s.normalize(posting)
			if err != nil {
				return eightfoldSnapshot{}, fmt.Errorf("eightfold %s: item at raw offset %d: %w", s.host, start, err)
			}
			seen[posting.ID] = encoded
			jobs = append(jobs, job)
		}

		scanned := start + len(positions)
		if scanned > expectedTotal {
			return eightfoldSnapshot{}, eightfoldRetryable(fmt.Errorf(
				"eightfold %s: page at start %d overran reported total %d with %d positions",
				s.host, start, expectedTotal, len(positions),
			), diagnostic.RetrySnapshot)
		}
		if scanned < expectedTotal && len(positions) < eightfoldPageSize {
			return eightfoldSnapshot{}, eightfoldRetryable(fmt.Errorf(
				"eightfold %s: short page of %d at start %d before reported total %d",
				s.host, len(positions), start, expectedTotal,
			), diagnostic.RetrySnapshot)
		}
		if scanned >= expectedTotal || len(jobs) >= s.maxPostings {
			break
		}
		start = scanned
	}

	if expectedTotal > 0 && len(jobs) == 0 {
		return eightfoldSnapshot{}, fmt.Errorf("eightfold %s: reported %d postings but produced none", s.host, expectedTotal)
	}
	fingerprint, err := json.Marshal(struct {
		Total     int                 `json:"total"`
		Positions []eightfoldPosition `json:"positions"`
	}{Total: expectedTotal, Positions: rawPositions})
	if err != nil {
		return eightfoldSnapshot{}, fmt.Errorf("eightfold %s: encode snapshot for consistency: %w", s.host, err)
	}
	return eightfoldSnapshot{
		jobs: jobs, fingerprint: fingerprint, expectedTotal: expectedTotal,
		hadExactDuplicate: hadExactDuplicate,
	}, nil
}

func (s *eightfold) fetchSearchPage(ctx context.Context, endpoint string, page *eightfoldSearchResponse) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &eightfoldRetryableError{
			err: fmt.Errorf("GET %s: %w", endpoint, err), kind: diagnostic.RetryTransport,
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		statusErr := fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(snippet))
		if resp.StatusCode == http.StatusTooManyRequests {
			return &eightfoldRetryableError{
				err: statusErr, kind: diagnostic.RetryRateLimit,
				retryAfter: eightfoldRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			}
		}
		if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
			return &eightfoldRetryableError{
				err: statusErr, kind: diagnostic.RetryServer,
				retryAfter: eightfoldRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			}
		}
		return statusErr
	}
	if err := json.NewDecoder(resp.Body).Decode(page); err != nil {
		return fmt.Errorf("GET %s: decoding response: %w", endpoint, err)
	}
	return nil
}

func (s *eightfold) finishSnapshot(ctx context.Context, snapshot eightfoldSnapshot) []model.Job {
	if snapshot.expectedTotal > s.maxPostings {
		diagnostic.Cap(ctx, len(snapshot.jobs), snapshot.expectedTotal)
	}
	return snapshot.jobs
}

func (s *eightfold) retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	delay := time.Duration(attempt) * s.retryGap
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > eightfoldMaxRetryAfter {
		return eightfoldMaxRetryAfter
	}
	if delay < 0 {
		return 0
	}
	return delay
}

func eightfoldRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(eightfoldMaxRetryAfter/time.Second) {
			return eightfoldMaxRetryAfter
		}
		delay := time.Duration(seconds) * time.Second
		return delay
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := retryAt.Sub(now)
	if delay <= 0 {
		return 0
	}
	if delay > eightfoldMaxRetryAfter {
		return eightfoldMaxRetryAfter
	}
	return delay
}

func eightfoldWait(ctx context.Context, delay time.Duration) error {
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

func (s *eightfold) normalize(posting eightfoldPosition) (model.Job, error) {
	if posting.ID <= 0 {
		return model.Job{}, fmt.Errorf("invalid position id %d", posting.ID)
	}
	title := strings.TrimSpace(posting.Name)
	if title == "" {
		return model.Job{}, fmt.Errorf("position %d has empty name", posting.ID)
	}
	idText := strconv.FormatInt(posting.ID, 10)
	positionURL := strings.TrimSpace(posting.PositionURL)
	if positionURL == "" {
		positionURL = "/careers/job/" + idText
	}
	parsed, err := url.Parse(positionURL)
	if err != nil || !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/"+idText) {
		return model.Job{}, fmt.Errorf("position %d has invalid positionUrl %q", posting.ID, posting.PositionURL)
	}
	publicURL, err := resolveReference(s.base+"/", positionURL)
	if err != nil {
		return model.Job{}, fmt.Errorf("position %d URL: %w", posting.ID, err)
	}

	var postedAt time.Time
	if posting.PostedTS > 0 {
		postedAt = time.Unix(posting.PostedTS, 0)
	}
	return model.Job{
		ID:       s.keyPrefix + idText,
		Company:  s.company,
		Title:    title,
		Location: strings.Join(distinctStrings(posting.Locations), "; "),
		URL:      publicURL,
		PostedAt: postedAt,
	}, nil
}

func (s *eightfold) Detail(ctx context.Context, job *model.Job) error {
	if job == nil {
		return fmt.Errorf("eightfold %s: nil job", s.host)
	}
	if !strings.HasPrefix(job.ID, s.keyPrefix) {
		return fmt.Errorf("eightfold %s: job id %q does not have prefix %q", s.host, job.ID, s.keyPrefix)
	}
	positionID := strings.TrimPrefix(job.ID, s.keyPrefix)
	numericID, err := strconv.ParseInt(positionID, 10, 64)
	if err != nil || numericID <= 0 {
		return fmt.Errorf("eightfold %s: invalid position id %q", s.host, positionID)
	}
	query := url.Values{"position_id": {positionID}, "domain": {s.domain}, "hl": {"en"}}
	var detail struct {
		Status int `json:"status"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
		Data *struct {
			ID                         int64    `json:"id"`
			Name                       string   `json:"name"`
			Location                   string   `json:"location"`
			Locations                  []string `json:"locations"`
			PostedTS                   int64    `json:"postedTs"`
			JobDescription             string   `json:"jobDescription"`
			PublicURL                  string   `json:"publicUrl"`
			EFCustomTextEmploymentType []string `json:"efcustomTextEmploymentType"`
		} `json:"data"`
	}
	endpoint := s.base + "/api/pcsx/position_details?" + query.Encode()
	if err := fetchJSON(ctx, s.client, http.MethodGet, endpoint, nil, &detail); err != nil {
		return err
	}
	if detail.Status != http.StatusOK || detail.Data == nil {
		return fmt.Errorf("eightfold %s: detail %s reported status %d (%s)", s.host, positionID, detail.Status, detail.Error.Message)
	}
	if detail.Data.ID != numericID {
		return fmt.Errorf("eightfold %s: detail id %d does not match %d", s.host, detail.Data.ID, numericID)
	}
	description := htmltext.ToText(detail.Data.JobDescription)
	if strings.TrimSpace(description) == "" {
		return fmt.Errorf("eightfold %s: detail %s has empty jobDescription", s.host, positionID)
	}
	job.Description = description
	job.EmploymentType = strings.Join(distinctStrings(detail.Data.EFCustomTextEmploymentType), "; ")
	if locations := distinctStrings(detail.Data.Locations); len(locations) > 0 {
		job.Location = strings.Join(locations, "; ")
	} else if location := strings.TrimSpace(detail.Data.Location); location != "" {
		job.Location = location
	}
	if detail.Data.PostedTS > 0 {
		job.PostedAt = time.Unix(detail.Data.PostedTS, 0)
	}
	if publicURL := strings.TrimSpace(detail.Data.PublicURL); publicURL != "" {
		job.URL = publicURL
	}
	return nil
}
