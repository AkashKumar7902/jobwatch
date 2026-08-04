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
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

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
}

func (l *llm) Name() string { return "llm" }

const llmSystemPrompt = `You judge whether a job posting fits a candidate. Consider role fit, seniority, stated experience requirements, employment type, and location/timezone eligibility. Be practical: a posting the candidate could reasonably be hired for is a fit; a posting clearly above their level or closed to their location is not. Respond with ONLY a JSON object: {"match": true|false, "reason": "<why>"}`

func (l *llm) Match(ctx context.Context, job model.Job) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	verdict, err := l.ask(ctx, job)
	if err != nil {
		return Result{}, err
	}
	return Result{Matched: verdict.Match, Reason: "llm: " + verdict.Reason}, nil
}

type llmVerdict struct {
	Match  bool   `json:"match"`
	Reason string `json:"reason"`
}

func (l *llm) ask(ctx context.Context, job model.Job) (llmVerdict, error) {
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
		return llmVerdict{}, err
	}

	// Rate limits are routine on free tiers (Gemini free: ~10 req/min), so
	// retry 429s and transient 5xx with a pause before deferring the job.
	var resp *http.Response
	var raw []byte
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint, bytes.NewReader(body))
		if err != nil {
			return llmVerdict{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if l.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+l.apiKey)
		}
		resp, err = l.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return llmVerdict{}, ctx.Err()
			}
			// Transport errors (timeouts, resets) are as transient as 5xx.
			if attempt >= 4 {
				return llmVerdict{}, fmt.Errorf("calling %s: %w", l.endpoint, err)
			}
			log.Printf("llm matcher: %v, retrying in 5s (attempt %d/4)", err, attempt)
			if err := waitForRetry(ctx, 5*time.Second); err != nil {
				return llmVerdict{}, err
			}
			continue
		}
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxLLMResponseBytes+1))
		resp.Body.Close()
		if err != nil {
			return llmVerdict{}, fmt.Errorf("reading completion: %w", err)
		}
		if len(raw) > maxLLMResponseBytes {
			return llmVerdict{}, fmt.Errorf("completion exceeds %d bytes", maxLLMResponseBytes)
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retryable || attempt >= 4 {
			return llmVerdict{}, fmt.Errorf("%s returned %s: %s", l.endpoint, resp.Status, truncateStr(string(bytes.TrimSpace(raw)), 200))
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
		log.Printf("llm matcher: %s, retrying in %s (attempt %d/4)", resp.Status, wait, attempt)
		if err := waitForRetry(ctx, wait); err != nil {
			return llmVerdict{}, err
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
		return llmVerdict{}, fmt.Errorf("decoding completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return llmVerdict{}, fmt.Errorf("completion has no choices")
	}
	return parseVerdict(completion.Choices[0].Message.Content)
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

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
