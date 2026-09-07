package match

// The "llm" matcher asks a language model whether a job fits your profile.
// It speaks the OpenAI-compatible chat-completions API. The configured
// endpoint must support JSON-schema structured outputs (`response_format`
// with type `json_schema`):
//
//	OpenAI      base_url: https://api.openai.com/v1        model: gpt-4o-mini
//	Anthropic   base_url: https://api.anthropic.com/v1     model: claude-opus-4-8
//	Groq        base_url: https://api.groq.com/openai/v1   model: llama-3.3-70b-versatile
//	OpenRouter  base_url: https://openrouter.ai/api/v1     model: anything it serves
//	Ollama      base_url: http://localhost:11434/v1        model: llama3.1  (free, local, no key)
//
// Config (put it LAST under an `all` combinator — children are evaluated in
// order and the first veto short-circuits, so cheap matchers filter first
// and the LLM is only called for jobs that already passed them):
//
//	- name: llm
//	  params:
//	    profile: "Backend engineer with 1 year of Go/Python experience, based in India, needs remote roles open to India"
//	    base_url: https://api.openai.com/v1
//	    model: gpt-4o-mini
//	    api_key_env: OPENAI_API_KEY  # omit for keyless endpoints like local Ollama
//
// Provider and protocol failures return an error. The runner leaves that job
// unprocessed for a later run instead of guessing at a verdict.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("llm", func(p params.Map, children []Matcher) (Matcher, error) {
		if err := RequireNoChildren("llm", children); err != nil {
			return nil, err
		}
		profile, err := p.Require("profile")
		if err != nil {
			return nil, err
		}
		baseURL, err := p.Require("base_url")
		if err != nil {
			return nil, err
		}
		modelName, err := p.Require("model")
		if err != nil {
			return nil, err
		}
		var apiKey string
		if envName := p.Get("api_key_env"); envName != "" {
			apiKey = os.Getenv(envName)
			if apiKey == "" {
				return nil, fmt.Errorf("api_key_env %s is set in config but the environment variable is empty", envName)
			}
		}
		if _, ok := p["on_error"]; ok {
			return nil, fmt.Errorf(`param "on_error" was removed; delete it because matcher failures now always defer the job`)
		}
		maxDescChars, err := p.Int("max_desc_chars", 6000)
		if err != nil {
			return nil, err
		}
		maxTokens, err := p.Int("max_tokens", 700)
		if err != nil {
			return nil, err
		}
		system := llmSystemPrompt
		if extra := strings.TrimSpace(p.Get("instructions")); extra != "" {
			system += "\n\nAdditional matching rules from the user (these override the defaults above where they conflict):\n" + extra
		}
		return &llm{
			profile:      profile,
			system:       system,
			endpoint:     strings.TrimSuffix(baseURL, "/") + "/chat/completions",
			model:        modelName,
			apiKey:       apiKey,
			maxDescChars: maxDescChars,
			maxTokens:    maxTokens,
			client:       &http.Client{Timeout: 90 * time.Second},
			timeout:      90 * time.Second,
		}, nil
	})
}

type llm struct {
	profile      string
	system       string
	endpoint     string
	model        string
	apiKey       string
	maxDescChars int
	maxTokens    int
	client       *http.Client
	timeout      time.Duration

	// Match calls are serialized so a concurrent caller cannot race past the
	// threshold and issue an unbounded burst of requests after a permanent
	// failure. Runner evaluates sequentially, so this adds no production
	// contention today.
	breakerMu sync.Mutex
	breaker   llmBreaker
}

const llmBreakerThreshold = 3

type llmBreaker struct {
	kind        diagnostic.LLMErrorKind
	status      int
	provider    diagnostic.LLMProviderStatus
	consecutive int
	open        bool
}

// llmError is deliberately safe to print. The wrapped cause exists only for
// errors.Is/errors.As; Error never renders it because transports routinely
// include URLs and providers routinely put request details in response text.
type llmError struct {
	info  diagnostic.LLMErrorInfo
	cause error
}

func (e *llmError) Error() string {
	if e == nil {
		return "llm failure"
	}
	if e.info.HTTPStatus > 0 {
		return fmt.Sprintf("llm failure: %s (HTTP %d, provider %s)",
			e.info.Kind.Token(), e.info.HTTPStatus, e.info.ProviderStatus.Token())
	}
	return "llm failure: " + e.info.Kind.Token()
}

func (e *llmError) Unwrap() error { return e.cause }

// LLMErrorInfo is an alias for the diagnostic package's closed, public-safe
// representation so preflight and runtime reporting share one vocabulary.
type LLMErrorInfo = diagnostic.LLMErrorInfo

// LLMErrorInfoOf extracts safe structured evidence from an LLM failure,
// including when a combinator has wrapped or joined it.
func LLMErrorInfoOf(err error) (LLMErrorInfo, bool) {
	var target *llmError
	if !errors.As(err, &target) || !target.info.Valid() {
		return LLMErrorInfo{}, false
	}
	return target.info, true
}

// LLMErrorKindOf is the compatibility convenience for callers that only need
// the top-level category.
func LLMErrorKindOf(err error) (diagnostic.LLMErrorKind, bool) {
	info, ok := LLMErrorInfoOf(err)
	return info.Kind, ok
}

func newLLMError(kind diagnostic.LLMErrorKind, status int, provider diagnostic.LLMProviderStatus, cause error) error {
	if status < 100 || status > 599 {
		status = 0
	}
	return &llmError{info: diagnostic.LLMErrorInfo{
		Kind: kind, HTTPStatus: status, ProviderStatus: provider,
	}, cause: cause}
}

func (l *llm) Name() string { return "llm" }

const llmSystemPrompt = `You judge whether a job posting fits a candidate. Consider role fit, seniority, stated experience requirements, employment type, and location/timezone eligibility. Be practical: a posting the candidate could reasonably be hired for is a fit; a posting clearly above their level or closed to their location is not. Respond with ONLY a JSON object: {"match": true|false, "reason": "<why>"}`

func (l *llm) Match(ctx context.Context, job model.Job) (Result, error) {
	l.breakerMu.Lock()
	defer l.breakerMu.Unlock()
	if l.breaker.open {
		return Result{}, newLLMError(diagnostic.LLMCircuitOpen, l.breaker.status, l.breaker.provider, nil)
	}

	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	verdict, err := l.ask(ctx, job)
	if err != nil {
		l.observeFailure(err)
		return Result{}, err
	}
	l.breaker = llmBreaker{}
	return Result{Matched: verdict.Match, Reason: "llm: " + verdict.Reason}, nil
}

// ResetRun clears only run-scoped protection; it does not change matcher
// configuration or HTTP client state.
func (l *llm) ResetRun() {
	l.breakerMu.Lock()
	l.breaker = llmBreaker{}
	l.breakerMu.Unlock()
}

func (l *llm) observeFailure(err error) {
	var typed *llmError
	if !errors.As(err, &typed) || !typed.info.Kind.Permanent() {
		l.breaker = llmBreaker{}
		return
	}
	if l.breaker.kind == typed.info.Kind && l.breaker.status == typed.info.HTTPStatus &&
		l.breaker.provider == typed.info.ProviderStatus {
		l.breaker.consecutive++
	} else {
		l.breaker = llmBreaker{
			kind: typed.info.Kind, status: typed.info.HTTPStatus,
			provider: typed.info.ProviderStatus, consecutive: 1,
		}
	}
	if l.breaker.consecutive >= llmBreakerThreshold {
		l.breaker.open = true
	}
}

type llmVerdict struct {
	Match  bool   `json:"match"`
	Reason string `json:"reason"`
}

func (l *llm) ask(ctx context.Context, job model.Job) (llmVerdict, error) {
	return l.askWithAttempts(ctx, job, 4)
}

// askWithAttempts is the single production request path. Runtime uses four
// attempts; preflight uses one so a validation probe is deterministic and
// cannot spend its full timeout retrying.
func (l *llm) askWithAttempts(ctx context.Context, job model.Job, maxAttempts int) (llmVerdict, error) {
	return l.askWithClientAndAttempts(ctx, job, l.client, maxAttempts)
}

// askWithClientAndAttempts lets preflight apply stricter redirect behavior
// without copying a live llm (which contains a mutex) or mutating its shared
// production client.
func (l *llm) askWithClientAndAttempts(ctx context.Context, job model.Job, client *http.Client, maxAttempts int) (llmVerdict, error) {
	if client == nil {
		return llmVerdict{}, newLLMError(diagnostic.LLMTransport, 0, diagnostic.LLMProviderNone, nil)
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	} else if maxAttempts > 4 {
		maxAttempts = 4
	}
	desc := job.Description
	if len(desc) > l.maxDescChars {
		desc = desc[:l.maxDescChars] + "\n[truncated]"
	}
	var user strings.Builder
	fmt.Fprintf(&user, "Candidate profile: %s\n\nJob posting:\nCompany: %s\nTitle: %s\n", l.profile, job.Company, job.Title)
	if job.Location != "" {
		fmt.Fprintf(&user, "Location: %s\n", job.Location)
	}
	if job.EmploymentType != "" {
		fmt.Fprintf(&user, "Employment type: %s\n", job.EmploymentType)
	}
	fmt.Fprintf(&user, "Description:\n%s", desc)

	body, err := json.Marshal(map[string]any{
		"model": l.model,
		"messages": []map[string]string{
			{"role": "system", "content": l.system},
			{"role": "user", "content": user.String()},
		},
		"max_tokens":      l.maxTokens,
		"response_format": verdictResponseFormat(),
	})
	if err != nil {
		return llmVerdict{}, newLLMError(diagnostic.LLMTransport, 0, diagnostic.LLMProviderNone, err)
	}

	// Rate limits are routine on free tiers (Gemini free: ~10 req/min), so
	// retry 429s and transient 5xx with a pause before deferring the job.
	var resp *http.Response
	var raw []byte
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint, bytes.NewReader(body))
		if err != nil {
			return llmVerdict{}, newLLMError(diagnostic.LLMTransport, 0, diagnostic.LLMProviderNone, err)
		}
		req.Header.Set("Content-Type", "application/json")
		if l.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+l.apiKey)
		}
		resp, err = client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return llmVerdict{}, classifyRequestError(ctx.Err())
			}
			// Transport errors (timeouts, resets) are as transient as 5xx.
			if attempt >= maxAttempts {
				return llmVerdict{}, classifyRequestError(err)
			}
			diagnostic.Retry(ctx, diagnostic.RetryTransport, attempt, maxAttempts, 5*time.Second)
			if err := waitForRetry(ctx, 5*time.Second); err != nil {
				return llmVerdict{}, classifyRequestError(err)
			}
			continue
		}
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxLLMResponseBytes+1))
		resp.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				return llmVerdict{}, classifyRequestError(ctx.Err())
			}
			return llmVerdict{}, newLLMError(diagnostic.LLMRead, resp.StatusCode, diagnostic.LLMProviderNone, err)
		}
		if len(raw) > maxLLMResponseBytes {
			return llmVerdict{}, newLLMError(diagnostic.LLMTooLarge, resp.StatusCode, diagnostic.LLMProviderNone, nil)
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retryable || attempt >= maxAttempts {
			return llmVerdict{}, newLLMError(
				classifyHTTPStatus(resp.StatusCode), resp.StatusCode,
				normalizeProviderStatus(raw, resp.StatusCode), nil,
			)
		}
		wait := 20 * time.Second
		if resp.StatusCode >= 500 {
			wait = 5 * time.Second
		}
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, err := time.ParseDuration(s + "s"); err == nil && secs > 0 && secs < 2*time.Minute {
				wait = secs
			}
		}
		kind := diagnostic.RetryServer
		if resp.StatusCode == http.StatusTooManyRequests {
			kind = diagnostic.RetryRateLimit
		}
		diagnostic.Retry(ctx, kind, attempt, maxAttempts, wait)
		if err := waitForRetry(ctx, wait); err != nil {
			return llmVerdict{}, classifyRequestError(err)
		}
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		return llmVerdict{}, newLLMError(diagnostic.LLMDecode, http.StatusOK, diagnostic.LLMProviderNone, err)
	}
	if len(completion.Choices) == 0 {
		return llmVerdict{}, newLLMError(diagnostic.LLMNoChoices, http.StatusOK, diagnostic.LLMProviderNone, nil)
	}
	verdict, err := parseVerdict(completion.Choices[0].Message.Content)
	if err != nil {
		return llmVerdict{}, newLLMError(diagnostic.LLMInvalidVerdict, http.StatusOK, diagnostic.LLMProviderNone, err)
	}
	return verdict, nil
}

func classifyHTTPStatus(status int) diagnostic.LLMErrorKind {
	switch status {
	case http.StatusBadRequest:
		return diagnostic.LLMBadRequest
	case http.StatusUnauthorized:
		return diagnostic.LLMUnauthorized
	case http.StatusForbidden:
		return diagnostic.LLMForbidden
	case http.StatusNotFound:
		return diagnostic.LLMNotFound
	case http.StatusTooManyRequests:
		return diagnostic.LLMRateLimited
	default:
		if status >= 500 {
			return diagnostic.LLMServer
		}
		return diagnostic.LLMHTTPStatus
	}
}

// normalizeProviderStatus inspects only bounded machine fields and returns a
// closed enum. Free-text messages are absent from the decode shape and can
// therefore never cross into an error or diagnostic record.
func normalizeProviderStatus(raw []byte, httpStatus int) diagnostic.LLMProviderStatus {
	type detail struct {
		Reason string `json:"reason"`
	}
	type fields struct {
		Details []detail        `json:"details"`
		Code    json.RawMessage `json:"code"`
		Type    string          `json:"type"`
		Status  string          `json:"status"`
		Reason  string          `json:"reason"`
	}
	var envelope struct {
		Error  fields          `json:"error"`
		Code   json.RawMessage `json:"code"`
		Type   string          `json:"type"`
		Status string          `json:"status"`
		Reason string          `json:"reason"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		detailLimit := len(envelope.Error.Details)
		if detailLimit > 16 {
			detailLimit = 16
		}
		candidates := make([]string, 0, detailLimit+8)
		for _, item := range envelope.Error.Details[:detailLimit] {
			candidates = append(candidates, item.Reason)
		}
		candidates = append(candidates,
			jsonString(envelope.Error.Code), envelope.Error.Type,
			envelope.Error.Status, envelope.Error.Reason,
			jsonString(envelope.Code), envelope.Type, envelope.Status, envelope.Reason,
		)
		sawCandidate := false
		for _, candidate := range candidates {
			if strings.TrimSpace(candidate) == "" {
				continue
			}
			sawCandidate = true
			if status := providerStatusToken(candidate); status != diagnostic.LLMProviderUnknown {
				return status
			}
		}
		if sawCandidate {
			return diagnostic.LLMProviderUnknown
		}
	}
	return providerStatusFromHTTP(httpStatus)
}

func jsonString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || len(raw) > 256 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func providerStatusToken(value string) diagnostic.LLMProviderStatus {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 64 {
		return diagnostic.LLMProviderUnknown
	}
	var normalized strings.Builder
	separator := false
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case char >= 'a' && char <= 'z':
			normalized.WriteByte(char - ('a' - 'A'))
			separator = false
		case char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			normalized.WriteByte(char)
			separator = false
		case char == '_', char == '-', char == '.', char == '/', char == ' ', char == '\t':
			if normalized.Len() > 0 && !separator {
				normalized.WriteByte('_')
				separator = true
			}
		default:
			return diagnostic.LLMProviderUnknown
		}
	}
	token := strings.TrimSuffix(normalized.String(), "_")
	switch token {
	case "API_KEY_INVALID", "INVALID_API_KEY", "API_KEY_EXPIRED", "AUTHENTICATION_ERROR", "UNAUTHENTICATED", "UNAUTHORIZED":
		return diagnostic.LLMProviderUnauthenticated
	case "PERMISSION_DENIED", "FORBIDDEN", "PERMISSION_ERROR", "SERVICE_DISABLED":
		return diagnostic.LLMProviderPermissionDenied
	case "INVALID_ARGUMENT", "BAD_REQUEST", "INVALID_REQUEST", "INVALID_REQUEST_ERROR":
		return diagnostic.LLMProviderInvalidArgument
	case "NOT_FOUND", "MODEL_NOT_FOUND":
		return diagnostic.LLMProviderNotFound
	case "RESOURCE_EXHAUSTED", "RATE_LIMIT", "RATE_LIMIT_EXCEEDED", "RATE_LIMIT_ERROR",
		"QUOTA_EXCEEDED", "INSUFFICIENT_QUOTA":
		return diagnostic.LLMProviderResourceExhausted
	case "FAILED_PRECONDITION":
		return diagnostic.LLMProviderFailedPrecondition
	case "ABORTED", "CONFLICT":
		return diagnostic.LLMProviderAborted
	case "DEADLINE_EXCEEDED", "TIMEOUT", "REQUEST_TIMEOUT":
		return diagnostic.LLMProviderDeadlineExceeded
	case "UNAVAILABLE", "SERVICE_UNAVAILABLE", "OVERLOADED":
		return diagnostic.LLMProviderUnavailable
	case "INTERNAL", "INTERNAL_ERROR", "SERVER_ERROR":
		return diagnostic.LLMProviderInternal
	case "UNIMPLEMENTED":
		return diagnostic.LLMProviderUnimplemented
	case "SAFETY", "POLICY_VIOLATION", "CONTENT_FILTER", "CONTENT_POLICY_VIOLATION":
		return diagnostic.LLMProviderPolicyBlocked
	default:
		if strings.HasPrefix(token, "API_KEY_") && strings.HasSuffix(token, "_BLOCKED") {
			return diagnostic.LLMProviderPermissionDenied
		}
		return diagnostic.LLMProviderUnknown
	}
}

func providerStatusFromHTTP(status int) diagnostic.LLMProviderStatus {
	switch status {
	case http.StatusBadRequest:
		return diagnostic.LLMProviderInvalidArgument
	case http.StatusUnauthorized:
		return diagnostic.LLMProviderUnauthenticated
	case http.StatusForbidden:
		return diagnostic.LLMProviderPermissionDenied
	case http.StatusNotFound:
		return diagnostic.LLMProviderNotFound
	case http.StatusRequestTimeout:
		return diagnostic.LLMProviderDeadlineExceeded
	case http.StatusConflict:
		return diagnostic.LLMProviderAborted
	case http.StatusTooManyRequests:
		return diagnostic.LLMProviderResourceExhausted
	case http.StatusNotImplemented:
		return diagnostic.LLMProviderUnimplemented
	case http.StatusServiceUnavailable:
		return diagnostic.LLMProviderUnavailable
	default:
		if status >= 500 && status <= 599 {
			return diagnostic.LLMProviderInternal
		}
		return diagnostic.LLMProviderUnknown
	}
}

func classifyRequestError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return newLLMError(diagnostic.LLMTimeout, 0, diagnostic.LLMProviderNone, context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return newLLMError(diagnostic.LLMTimeout, 0, diagnostic.LLMProviderNone, err)
	}
	return newLLMError(diagnostic.LLMTransport, 0, diagnostic.LLMProviderNone, err)
}

const maxLLMResponseBytes = 1 << 20

func verdictResponseFormat() map[string]any {
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "job_match",
			"strict": true,
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"match":  map[string]any{"type": "boolean"},
					"reason": map[string]any{"type": "string"},
				},
				"required":             []string{"match", "reason"},
				"additionalProperties": false,
			},
		},
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// parseVerdict accepts exactly one object with the two schema fields. Token
// parsing keeps field names case-sensitive and detects duplicates, unlike
// decoding directly into a Go struct.
func parseVerdict(content string) (llmVerdict, error) {
	dec := json.NewDecoder(strings.NewReader(content))
	token, err := dec.Token()
	if err != nil {
		return llmVerdict{}, fmt.Errorf("parsing model reply: %w", err)
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return llmVerdict{}, fmt.Errorf("model reply must be one JSON object")
	}

	var verdict llmVerdict
	seen := make(map[string]bool, 2)
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return llmVerdict{}, fmt.Errorf("parsing model reply field: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return llmVerdict{}, fmt.Errorf("model reply contains a non-string field name")
		}
		if seen[name] {
			return llmVerdict{}, fmt.Errorf("model reply contains duplicate field %q", name)
		}
		seen[name] = true

		switch name {
		case "match":
			var value *bool
			if err := dec.Decode(&value); err != nil || value == nil {
				return llmVerdict{}, fmt.Errorf(`model reply field "match" must be a boolean`)
			}
			verdict.Match = *value
		case "reason":
			var value *string
			if err := dec.Decode(&value); err != nil || value == nil {
				return llmVerdict{}, fmt.Errorf(`model reply field "reason" must be a string`)
			}
			verdict.Reason = *value
		default:
			return llmVerdict{}, fmt.Errorf("model reply contains unknown field %q", name)
		}
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') {
		return llmVerdict{}, fmt.Errorf("parsing model reply: incomplete JSON object")
	}
	for _, name := range []string{"match", "reason"} {
		if !seen[name] {
			return llmVerdict{}, fmt.Errorf("model reply is missing required field %q", name)
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return llmVerdict{}, fmt.Errorf("model reply contains trailing content")
	}
	return verdict, nil
}

// truncateStr is retained for compact test names and other internal display
// use. LLM failure paths intentionally never pass provider data to it.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
