package match

// The "llm" matcher asks a language model whether a job fits your profile.
// It speaks the OpenAI-compatible chat-completions API, which nearly every
// provider serves, so it is not tied to any vendor:
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
//	    on_error: match              # match (default) or skip; see below
//
// on_error decides what happens when the provider is unreachable or returns
// garbage. The default "match" FAILS OPEN: the runner marks evaluated jobs
// as seen, so failing closed would silently lose postings forever during an
// outage — a noisy email is recoverable, a lost job is not.

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
		onError := p.GetDefault("on_error", "match")
		if onError != "match" && onError != "skip" {
			return nil, fmt.Errorf(`param "on_error": want "match" or "skip", got %q`, onError)
		}
		maxDescChars, err := p.Int("max_desc_chars", 6000)
		if err != nil {
			return nil, err
		}
		return &llm{
			profile:      profile,
			endpoint:     strings.TrimSuffix(baseURL, "/") + "/chat/completions",
			model:        modelName,
			apiKey:       apiKey,
			matchOnError: onError == "match",
			maxDescChars: maxDescChars,
			client:       &http.Client{Timeout: 90 * time.Second},
		}, nil
	})
}

type llm struct {
	profile      string
	endpoint     string
	model        string
	apiKey       string
	matchOnError bool
	maxDescChars int
	client       *http.Client
}

func (l *llm) Name() string { return "llm" }

const llmSystemPrompt = `You judge whether a job posting fits a candidate. Consider role fit, seniority, stated experience requirements, employment type, and location/timezone eligibility. Be practical: a posting the candidate could reasonably be hired for is a fit; a posting clearly above their level or closed to their location is not. Respond with ONLY a JSON object: {"match": true|false, "reason": "<one short sentence>"}`

func (l *llm) Match(job model.Job) Result {
	verdict, err := l.ask(job)
	if err != nil {
		log.Printf("llm matcher: %v", err)
		if l.matchOnError {
			return Result{Matched: true, Reason: "llm unavailable, matching by default: " + err.Error()}
		}
		return Result{Matched: false, Reason: "llm unavailable, skipping: " + err.Error()}
	}
	return Result{Matched: verdict.Match, Reason: "llm: " + verdict.Reason}
}

type llmVerdict struct {
	Match  bool   `json:"match"`
	Reason string `json:"reason"`
}

func (l *llm) ask(job model.Job) (llmVerdict, error) {
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
			{"role": "system", "content": llmSystemPrompt},
			{"role": "user", "content": user.String()},
		},
		"max_tokens": 300,
	})
	if err != nil {
		return llmVerdict{}, err
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, l.endpoint, bytes.NewReader(body))
	if err != nil {
		return llmVerdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.apiKey)
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return llmVerdict{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return llmVerdict{}, fmt.Errorf("%s returned %s: %s", l.endpoint, resp.Status, truncateStr(string(bytes.TrimSpace(raw)), 200))
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

// parseVerdict extracts the {"match":..., "reason":...} object from the
// model's reply, tolerating surrounding prose or code fences.
func parseVerdict(content string) (llmVerdict, error) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return llmVerdict{}, fmt.Errorf("no JSON object in model reply: %q", truncateStr(content, 120))
	}
	var v llmVerdict
	if err := json.Unmarshal([]byte(content[start:end+1]), &v); err != nil {
		return llmVerdict{}, fmt.Errorf("parsing model reply: %w", err)
	}
	return v, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
