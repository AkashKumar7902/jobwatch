package source

// Google Careers exposes an anonymous batched RPC used by its own search
// page. This source intentionally fixes both server-side filters to the
// Google company and India: the wider portal also contains Alphabet
// affiliates, and its global sitemap has no company or location metadata.
//
// The response is a private positional protocol, so every field consumed
// below is type-checked and pagination fails atomically on any drift.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	googleRPCID             = "r06xKb"
	googleRPCPageSize       = 20
	googleRPCBodyLimit      = 2 << 20
	googleMaxJobs           = 5_000
	googleMaxDescriptionLen = 200_000
	googleMaxPageAttempts   = 3
	googleMaxRetryAfter     = 60 * time.Second

	googleRPCURL = "https://www.google.com/about/careers/applications/_/HiringCportalFrontendUi/data/batchexecute?rpcids=r06xKb&source-path=%2Fabout%2Fcareers%2Fapplications%2Fjobs%2Fresults%2F&hl=en-IN&rt=c"

	googleCompanyResource = "projects/gweb-careers-proto/tenants/60107626-8e00-0000-0000-0071646e0806/companies/ebbbf0d1-8121-483c-8f99-ee92597591fc"
	googlePublicJobBase   = "https://www.google.com/about/careers/applications/jobs/results/"
	googleApplicationPath = "/about/careers/applications/signin"
)

var (
	googleJobIDRE       = regexp.MustCompile(`^[1-9][0-9]{16,17}$`)
	googleCountryCodeRE = regexp.MustCompile(`^[A-Z]{2}$`)
	googleOpaqueApplyID = regexp.MustCompile(`^[A-Za-z0-9_=-]{40,512}$`)
)

func init() {
	Register("google", func(company string, p params.Map, client *http.Client) (Source, error) {
		if len(p) != 0 {
			keys := make([]string, 0, len(p))
			for key := range p {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("google source accepts no params (got %s)", strings.Join(keys, ", "))
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &googleIndia{
			company: company, endpoint: googleRPCURL, client: client,
			pageDelay: 100 * time.Millisecond,
		}, nil
	})
}

type googleIndia struct {
	company   string
	endpoint  string
	client    *http.Client
	pageDelay time.Duration
}

type googleRPCPage struct {
	Records  []json.RawMessage
	Total    int
	PageSize int
}

func (s *googleIndia) Company() string { return s.company }

func (s *googleIndia) Fetch(ctx context.Context) ([]model.Job, error) {
	// The offset protocol exposes no snapshot token. Accept only two
	// identical full traversals so a mutation on any page cannot silently
	// shift an open job past a page boundary.
	first, err := s.fetchSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	second, err := s.fetchSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("google: consistency traversal: %w", err)
	}
	if len(second) != len(first) {
		return nil, fmt.Errorf(
			"google: snapshot changed from %d to %d jobs between traversals",
			len(first), len(second),
		)
	}
	for index := range first {
		if first[index] != second[index] {
			return nil, fmt.Errorf(
				"google: snapshot changed at position %d from %q to %q",
				index, first[index].ID, second[index].ID,
			)
		}
	}
	return first, nil
}

func (s *googleIndia) fetchSnapshot(ctx context.Context) ([]model.Job, error) {
	first, err := s.fetchPage(ctx, 1)
	if err != nil {
		return nil, err
	}
	if first.Total <= 0 {
		return nil, fmt.Errorf("google: search returned no Google India jobs")
	}
	if first.Total > googleMaxJobs {
		return nil, fmt.Errorf(
			"google: reported total %d exceeds safety limit %d",
			first.Total, googleMaxJobs,
		)
	}
	if first.PageSize != googleRPCPageSize {
		return nil, fmt.Errorf(
			"google: page size is %d, want %d",
			first.PageSize, googleRPCPageSize,
		)
	}

	pageCount := (first.Total + first.PageSize - 1) / first.PageSize
	jobs := make([]model.Job, 0, first.Total)
	seen := make(map[string]struct{}, first.Total)

	for pageNumber := 1; pageNumber <= pageCount; pageNumber++ {
		page := first
		if pageNumber > 1 {
			if err := googleWait(ctx, s.pageDelay); err != nil {
				return nil, err
			}
			page, err = s.fetchPage(ctx, pageNumber)
			if err != nil {
				return nil, err
			}
		}
		if page.Total != first.Total {
			return nil, fmt.Errorf(
				"google: total changed from %d to %d on page %d",
				first.Total, page.Total, pageNumber,
			)
		}
		if page.PageSize != first.PageSize {
			return nil, fmt.Errorf(
				"google: page size changed from %d to %d on page %d",
				first.PageSize, page.PageSize, pageNumber,
			)
		}
		expected := min(first.PageSize, first.Total-(pageNumber-1)*first.PageSize)
		if len(page.Records) != expected {
			return nil, fmt.Errorf(
				"google: page %d returned %d jobs, want %d for total %d",
				pageNumber, len(page.Records), expected, first.Total,
			)
		}

		for index, record := range page.Records {
			job, err := s.normalizeJob(record)
			if err != nil {
				return nil, fmt.Errorf("google: page %d job %d: %w", pageNumber, index, err)
			}
			if _, duplicate := seen[job.ID]; duplicate {
				return nil, fmt.Errorf("google: duplicate stable job ID %q", job.ID)
			}
			seen[job.ID] = struct{}{}
			jobs = append(jobs, job)
		}
	}
	if len(jobs) != first.Total || len(seen) != first.Total {
		return nil, fmt.Errorf(
			"google: collected %d unique jobs, reported total is %d",
			len(seen), first.Total,
		)
	}
	return jobs, nil
}

func (s *googleIndia) fetchPage(ctx context.Context, pageNumber int) (googleRPCPage, error) {
	if pageNumber <= 0 || pageNumber > (googleMaxJobs+googleRPCPageSize-1)/googleRPCPageSize {
		return googleRPCPage{}, fmt.Errorf("google: invalid page number %d", pageNumber)
	}
	var lastErr error
	for attempt := 1; attempt <= googleMaxPageAttempts; attempt++ {
		page, err := s.fetchPageOnce(ctx, pageNumber)
		if err == nil {
			return page, nil
		}
		lastErr = err
		var transient *googleTransientError
		if !errors.As(err, &transient) || attempt == googleMaxPageAttempts {
			return googleRPCPage{}, err
		}
		delay := time.Duration(attempt) * 250 * time.Millisecond
		if transient.retryAfter > delay {
			delay = transient.retryAfter
		}
		if err := googleWait(ctx, delay); err != nil {
			return googleRPCPage{}, err
		}
	}
	return googleRPCPage{}, lastErr
}

func (s *googleIndia) fetchPageOnce(ctx context.Context, pageNumber int) (googleRPCPage, error) {
	formBody, err := googleRPCRequestBody(pageNumber)
	if err != nil {
		return googleRPCPage{}, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.endpoint, strings.NewReader(formBody),
	)
	if err != nil {
		return googleRPCPage{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")

	resp, err := clientWithoutRedirects(s.client).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return googleRPCPage{}, ctx.Err()
		}
		return googleRPCPage{}, &googleTransientError{
			err: fmt.Errorf("google: POST page %d: %w", pageNumber, err),
		}
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil ||
		resp.Request.URL.String() != req.URL.String() ||
		resp.Request.URL.ForceQuery != req.URL.ForceQuery {
		finalURL := "<missing>"
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		return googleRPCPage{}, fmt.Errorf(
			"google: POST page %d: unexpected final URL %q",
			pageNumber, finalURL,
		)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		statusErr := fmt.Errorf(
			"google: POST page %d: %s: %s",
			pageNumber, resp.Status, bytes.TrimSpace(snippet),
		)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return googleRPCPage{}, &googleTransientError{
				err:        statusErr,
				retryAfter: googleRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			}
		}
		return googleRPCPage{}, statusErr
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return googleRPCPage{}, fmt.Errorf(
			"google: POST page %d: unexpected Content-Type %q",
			pageNumber, resp.Header.Get("Content-Type"),
		)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, googleRPCBodyLimit+1))
	if err != nil {
		return googleRPCPage{}, fmt.Errorf("google: POST page %d: reading response: %w", pageNumber, err)
	}
	if len(body) > googleRPCBodyLimit {
		return googleRPCPage{}, fmt.Errorf(
			"google: POST page %d: response exceeds %d-byte safety limit",
			pageNumber, googleRPCBodyLimit,
		)
	}
	page, err := parseGoogleRPCPage(body)
	if err != nil {
		return googleRPCPage{}, fmt.Errorf("google: page %d response: %w", pageNumber, err)
	}
	return page, nil
}

type googleTransientError struct {
	err        error
	retryAfter time.Duration
}

func (e *googleTransientError) Error() string { return e.err.Error() }
func (e *googleTransientError) Unwrap() error { return e.err }

func googleRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(googleMaxRetryAfter/time.Second) {
			return googleMaxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := retryAt.Sub(now)
	if delay <= 0 {
		return 0
	}
	if delay > googleMaxRetryAfter {
		return googleMaxRetryAfter
	}
	return delay
}

func googleWait(ctx context.Context, delay time.Duration) error {
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

func googleRPCRequestBody(pageNumber int) (string, error) {
	query, err := json.Marshal([]any{[]any{
		nil,
		[]string{"Google"},
		nil,
		nil,
		"en-GB",
		nil,
		[][]string{{"India"}},
		pageNumber,
	}})
	if err != nil {
		return "", fmt.Errorf("google: encoding page query: %w", err)
	}
	batch, err := json.Marshal([]any{[]any{[]any{
		googleRPCID, string(query), nil, "generic",
	}}})
	if err != nil {
		return "", fmt.Errorf("google: encoding RPC request: %w", err)
	}
	return url.Values{"f.req": {string(batch)}}.Encode(), nil
}

func parseGoogleRPCPage(body []byte) (googleRPCPage, error) {
	frames, err := parseGoogleRPCFrames(body)
	if err != nil {
		return googleRPCPage{}, err
	}

	var inner string
	found := false
	allowedTags := map[string]bool{
		"wrb.fr": true, "di": true, "af.httprm": true, "e": true,
	}
	for frameIndex, frame := range frames {
		var rows []json.RawMessage
		if err := json.Unmarshal(frame, &rows); err != nil {
			return googleRPCPage{}, fmt.Errorf("frame %d is not a row array: %w", frameIndex, err)
		}
		if len(rows) == 0 {
			return googleRPCPage{}, fmt.Errorf("frame %d is empty", frameIndex)
		}
		for rowIndex, rawRow := range rows {
			var columns []json.RawMessage
			if err := json.Unmarshal(rawRow, &columns); err != nil || len(columns) == 0 {
				return googleRPCPage{}, fmt.Errorf(
					"frame %d row %d is malformed", frameIndex, rowIndex,
				)
			}
			var tag string
			if err := json.Unmarshal(columns[0], &tag); err != nil || !allowedTags[tag] {
				return googleRPCPage{}, fmt.Errorf(
					"frame %d row %d has unexpected tag", frameIndex, rowIndex,
				)
			}
			if tag != "wrb.fr" {
				continue
			}
			if len(columns) < 3 {
				return googleRPCPage{}, fmt.Errorf("RPC response row has %d fields, want at least 3", len(columns))
			}
			var rpcID string
			if err := json.Unmarshal(columns[1], &rpcID); err != nil || rpcID != googleRPCID {
				return googleRPCPage{}, fmt.Errorf("RPC response has unexpected ID")
			}
			if found {
				return googleRPCPage{}, fmt.Errorf("RPC response contains duplicate %s payloads", googleRPCID)
			}
			if err := json.Unmarshal(columns[2], &inner); err != nil || inner == "" {
				return googleRPCPage{}, fmt.Errorf("RPC response has no successful payload")
			}
			found = true
		}
	}
	if !found {
		return googleRPCPage{}, fmt.Errorf("RPC response omitted %s payload", googleRPCID)
	}

	var payload []json.RawMessage
	if err := json.Unmarshal([]byte(inner), &payload); err != nil {
		return googleRPCPage{}, fmt.Errorf("decoding embedded payload: %w", err)
	}
	if len(payload) != 4 {
		return googleRPCPage{}, fmt.Errorf("embedded payload has %d fields, want 4", len(payload))
	}
	if !bytes.Equal(bytes.TrimSpace(payload[1]), []byte("null")) {
		return googleRPCPage{}, fmt.Errorf("embedded payload field 1 is not null")
	}

	var total, pageSize int
	if err := json.Unmarshal(payload[2], &total); err != nil || total < 0 {
		return googleRPCPage{}, fmt.Errorf("embedded payload has invalid total")
	}
	if err := json.Unmarshal(payload[3], &pageSize); err != nil || pageSize != googleRPCPageSize {
		return googleRPCPage{}, fmt.Errorf(
			"embedded payload has page size %d, want %d",
			pageSize, googleRPCPageSize,
		)
	}
	if total > googleMaxJobs {
		return googleRPCPage{}, fmt.Errorf("embedded payload total %d exceeds safety limit %d", total, googleMaxJobs)
	}
	if bytes.Equal(bytes.TrimSpace(payload[0]), []byte("null")) {
		if total != 0 {
			return googleRPCPage{}, fmt.Errorf("embedded payload omitted jobs for nonzero total %d", total)
		}
		return googleRPCPage{Total: total, PageSize: pageSize}, nil
	}
	var records []json.RawMessage
	if err := json.Unmarshal(payload[0], &records); err != nil {
		return googleRPCPage{}, fmt.Errorf("decoding job rows: %w", err)
	}
	if len(records) > pageSize {
		return googleRPCPage{}, fmt.Errorf(
			"embedded payload has %d jobs, exceeding page size %d",
			len(records), pageSize,
		)
	}
	return googleRPCPage{Records: records, Total: total, PageSize: pageSize}, nil
}

func parseGoogleRPCFrames(body []byte) ([]json.RawMessage, error) {
	const prefix = ")]}'\n\n"
	if !bytes.HasPrefix(body, []byte(prefix)) {
		return nil, fmt.Errorf("missing XSSI prefix")
	}
	rest := body[len(prefix):]
	if !utf8.Valid(rest) {
		return nil, fmt.Errorf("response is not valid UTF-8")
	}
	lines := bytes.Split(rest, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || len(lines)%2 != 0 {
		return nil, fmt.Errorf("malformed response framing")
	}

	frames := make([]json.RawMessage, 0, len(lines)/2)
	for index := 0; index < len(lines); index += 2 {
		lengthText := string(lines[index])
		declared, err := strconv.Atoi(lengthText)
		if err != nil || declared <= 0 || strconv.Itoa(declared) != lengthText {
			return nil, fmt.Errorf("frame %d has invalid length %q", index/2, lengthText)
		}
		frame := lines[index+1]
		if declared != utf8.RuneCount(frame)+2 {
			return nil, fmt.Errorf(
				"frame %d length is %d, want %d",
				index/2, declared, utf8.RuneCount(frame)+2,
			)
		}
		if !json.Valid(frame) {
			return nil, fmt.Errorf("frame %d is not valid JSON", index/2)
		}
		frames = append(frames, append(json.RawMessage(nil), frame...))
	}
	return frames, nil
}

func (s *googleIndia) normalizeJob(raw json.RawMessage) (model.Job, error) {
	var fields []json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return model.Job{}, fmt.Errorf("job row is not an array: %w", err)
	}
	if len(fields) < 20 || len(fields) > 64 {
		return model.Job{}, fmt.Errorf("job row has %d fields, want 20 to 64", len(fields))
	}

	id, err := googleStringField(fields, 0, "job ID")
	if err != nil || !googleJobIDRE.MatchString(id) {
		return model.Job{}, fmt.Errorf("invalid job ID %q", id)
	}
	title, err := googleStringField(fields, 1, "title")
	title = compactSpaces(title)
	if err != nil || title == "" || utf8.RuneCountInString(title) > 300 {
		return model.Job{}, fmt.Errorf("job %s has invalid title", id)
	}
	applicationURL, err := googleStringField(fields, 2, "application URL")
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s: %w", id, err)
	}
	if err := validateGoogleApplicationURL(applicationURL); err != nil {
		return model.Job{}, fmt.Errorf("job %s application URL: %w", id, err)
	}

	resource, err := googleStringField(fields, 5, "company resource")
	if err != nil || resource != googleCompanyResource {
		return model.Job{}, fmt.Errorf("job %s has unexpected company resource", id)
	}
	organization, err := googleStringField(fields, 7, "company")
	if err != nil || organization != "Google" {
		return model.Job{}, fmt.Errorf("job %s company is %q, want Google", id, organization)
	}
	locale, err := googleStringField(fields, 8, "locale")
	if err != nil || locale != "en-US" {
		return model.Job{}, fmt.Errorf("job %s locale is %q, want en-US", id, locale)
	}

	location, err := googleLocations(fields[9])
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s locations: %w", id, err)
	}

	responsibilities, err := googleHTMLTuple(fields[3], "responsibilities", true)
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s: %w", id, err)
	}
	qualifications, err := googleHTMLTuple(fields[4], "qualifications", true)
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s: %w", id, err)
	}
	about, err := googleHTMLTuple(fields[10], "about", true)
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s: %w", id, err)
	}
	supplemental, err := googleHTMLTuple(fields[15], "supplemental", false)
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s: %w", id, err)
	}
	locationNote, err := googleHTMLTuple(fields[18], "location note", false)
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s: %w", id, err)
	}
	if _, err := googleHTMLTuple(fields[19], "minimum qualifications", true); err != nil {
		return model.Job{}, fmt.Errorf("job %s: %w", id, err)
	}

	createdSeconds, createdNanos, err := googleTimestamp(fields[12])
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s created timestamp: %w", id, err)
	}
	publishedSeconds, publishedNanos, err := googleTimestamp(fields[13])
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s published timestamp: %w", id, err)
	}
	modifiedSeconds, modifiedNanos, err := googleTimestamp(fields[14])
	if err != nil {
		return model.Job{}, fmt.Errorf("job %s modified timestamp: %w", id, err)
	}
	if googleTimestampAfter(createdSeconds, createdNanos, publishedSeconds, publishedNanos) ||
		googleTimestampAfter(publishedSeconds, publishedNanos, modifiedSeconds, modifiedNanos) {
		return model.Job{}, fmt.Errorf("job %s timestamps are out of order", id)
	}

	sections := []string{
		"About the job\n" + about,
		qualifications,
		"Responsibilities\n" + responsibilities,
	}
	if supplemental != "" {
		sections = append(sections, supplemental)
	}
	if locationNote != "" {
		sections = append(sections, locationNote)
	}
	description := strings.Join(sections, "\n")
	if description == "" || len(description) > googleMaxDescriptionLen {
		return model.Job{}, fmt.Errorf("job %s has invalid description length %d", id, len(description))
	}

	return model.Job{
		ID:          "google/" + id,
		Company:     s.company,
		Title:       title,
		Location:    location,
		URL:         googlePublicJobBase + id,
		Description: description,
		// Google exposes undocumented internal timestamps, but does not name
		// one as the public posting date. Leave PostedAt zero rather than
		// presenting an inference as source data.
	}, nil
}

func googleStringField(fields []json.RawMessage, index int, name string) (string, error) {
	if index < 0 || index >= len(fields) {
		return "", fmt.Errorf("missing %s field", name)
	}
	var value string
	if err := json.Unmarshal(fields[index], &value); err != nil {
		return "", fmt.Errorf("%s field is not a string", name)
	}
	return value, nil
}

func googleHTMLTuple(raw json.RawMessage, name string, required bool) (string, error) {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 ||
		!bytes.Equal(bytes.TrimSpace(tuple[0]), []byte("null")) {
		return "", fmt.Errorf("%s field has unexpected shape", name)
	}
	var fragment string
	if err := json.Unmarshal(tuple[1], &fragment); err != nil {
		return "", fmt.Errorf("%s field is not HTML text", name)
	}
	text := cleanHTMLFragment(fragment)
	if required && text == "" {
		return "", fmt.Errorf("%s field is empty", name)
	}
	return text, nil
}

func googleLocations(raw json.RawMessage) (string, error) {
	var rawLocations []json.RawMessage
	if err := json.Unmarshal(raw, &rawLocations); err != nil ||
		len(rawLocations) == 0 || len(rawLocations) > 20 {
		return "", fmt.Errorf("expected 1 to 20 structured locations")
	}
	var displays []string
	seen := make(map[string]struct{}, len(rawLocations))
	hasIndia := false
	for index, rawLocation := range rawLocations {
		var fields []json.RawMessage
		if err := json.Unmarshal(rawLocation, &fields); err != nil || len(fields) != 6 {
			return "", fmt.Errorf("location %d has unexpected shape", index)
		}
		var display, country string
		if err := json.Unmarshal(fields[0], &display); err != nil {
			return "", fmt.Errorf("location %d display is not a string", index)
		}
		if err := json.Unmarshal(fields[5], &country); err != nil ||
			!googleCountryCodeRE.MatchString(country) {
			return "", fmt.Errorf("location %d has invalid country code", index)
		}
		display = compactSpaces(display)
		if display == "" || utf8.RuneCountInString(display) > 300 {
			return "", fmt.Errorf("location %d has invalid display text", index)
		}
		if _, duplicate := seen[display]; !duplicate {
			seen[display] = struct{}{}
			displays = append(displays, display)
		}
		if country == "IN" {
			hasIndia = true
		}
	}
	if !hasIndia {
		return "", fmt.Errorf("no location is in India")
	}
	return strings.Join(displays, "; "), nil
}

func validateGoogleApplicationURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL")
	}
	if parsed.User != nil || parsed.Scheme != "https" || parsed.Host != "www.google.com" ||
		parsed.Path != googleApplicationPath || parsed.RawPath != "" || parsed.Opaque != "" ||
		parsed.Fragment != "" || parsed.ForceQuery || parsed.RawQuery == "" {
		return fmt.Errorf("URL is outside the trusted Google application endpoint")
	}
	query := parsed.Query()
	if len(query) != 3 {
		return fmt.Errorf("URL has unexpected query keys")
	}
	for _, key := range []string{"jobId", "loc", "title"} {
		if len(query[key]) != 1 || compactSpaces(query[key][0]) == "" {
			return fmt.Errorf("URL has invalid %s query value", key)
		}
	}
	if !googleOpaqueApplyID.MatchString(query.Get("jobId")) {
		return fmt.Errorf("URL has invalid opaque job ID")
	}
	if !googleCountryCodeRE.MatchString(query.Get("loc")) {
		return fmt.Errorf("URL has invalid location code")
	}
	if utf8.RuneCountInString(query.Get("title")) > 300 {
		return fmt.Errorf("URL title is too long")
	}
	return nil
}

func googleTimestamp(raw json.RawMessage) (int64, int64, error) {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
		return 0, 0, fmt.Errorf("unexpected shape")
	}
	var seconds, nanos int64
	if err := json.Unmarshal(tuple[0], &seconds); err != nil || seconds <= 0 {
		return 0, 0, fmt.Errorf("invalid seconds")
	}
	if err := json.Unmarshal(tuple[1], &nanos); err != nil || nanos < 0 || nanos >= 1_000_000_000 {
		return 0, 0, fmt.Errorf("invalid nanoseconds")
	}
	return seconds, nanos, nil
}

func googleTimestampAfter(aSeconds, aNanos, bSeconds, bNanos int64) bool {
	return aSeconds > bSeconds || (aSeconds == bSeconds && aNanos > bNanos)
}
