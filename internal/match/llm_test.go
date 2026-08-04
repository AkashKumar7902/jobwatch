package match

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func completionReply(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": content}},
		},
	})
	return string(b)
}

func newLLM(t *testing.T, baseURL string, extra params.Map) *llm {
	t.Helper()
	p := params.Map{
		"profile":  "1 year of Go experience, based in India",
		"base_url": baseURL,
		"model":    "test-model",
	}
	for k, v := range extra {
		p[k] = v
	}
	m, err := Build(Spec{Name: "llm", Params: p})
	if err != nil {
		t.Fatal(err)
	}
	return m.(*llm)
}

func matchLLM(t *testing.T, m Matcher) Result {
	t.Helper()
	result, err := m.Match(context.Background(), llmJob)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

var llmJob = model.Job{Company: "Acme", Title: "Junior Go Developer", Location: "Remote, Worldwide", Description: "Build APIs in Go."}

func TestLLMMatcherSendsStructuredSchema(t *testing.T) {
	var gotBody map[string]any
	reply := `{"match":true,"reason":"junior Go role, globally remote"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("request JSON: %v", err)
		}
		fmt.Fprint(w, completionReply(reply))
	}))
	defer srv.Close()

	m := newLLM(t, srv.URL, nil)
	res := matchLLM(t, m)
	if !res.Matched || !strings.Contains(res.Reason, "globally remote") {
		t.Errorf("Match() = %+v", res)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("model not sent: %v", gotBody["model"])
	}
	msgs := gotBody["messages"].([]any)
	userMsg := msgs[1].(map[string]any)["content"].(string)
	for _, want := range []string{"1 year of Go experience", "Junior Go Developer", "Acme"} {
		if !strings.Contains(userMsg, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if gotBody["max_tokens"].(float64) != 700 {
		t.Errorf("default max_tokens = %v, want 700", gotBody["max_tokens"])
	}

	format := gotBody["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("response_format.type = %v", format["type"])
	}
	named := format["json_schema"].(map[string]any)
	if named["name"] != "job_match" || named["strict"] != true {
		t.Errorf("json_schema metadata = %v", named)
	}
	schema := named["schema"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Errorf("schema object controls = %v", schema)
	}
	properties := schema["properties"].(map[string]any)
	if properties["match"].(map[string]any)["type"] != "boolean" ||
		properties["reason"].(map[string]any)["type"] != "string" {
		t.Errorf("schema properties = %v", properties)
	}
	required := schema["required"].([]any)
	if len(required) != 2 || required[0] != "match" || required[1] != "reason" {
		t.Errorf("schema required = %v", required)
	}

	// Existing tuning parameters and prompt instructions remain unchanged.
	tuned := newLLM(t, srv.URL, params.Map{"instructions": "Skip postings with no stated experience.", "max_tokens": "900"})
	matchLLM(t, tuned)
	sysMsg := gotBody["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sysMsg, "Skip postings with no stated experience.") {
		t.Errorf("instructions not in system prompt: %q", sysMsg)
	}
	if gotBody["max_tokens"].(float64) != 900 {
		t.Errorf("max_tokens = %v, want 900", gotBody["max_tokens"])
	}

	reply = `{"match":false,"reason":"requires 7 years"}`
	res = matchLLM(t, m)
	if res.Matched || !strings.Contains(res.Reason, "requires 7 years") {
		t.Errorf("false verdict = %+v", res)
	}
}

func TestParseVerdictRejectsAnythingOutsideSchema(t *testing.T) {
	valid := []struct {
		content string
		matched bool
	}{
		{`{"match":true,"reason":"fits"}`, true},
		{" \n\t" + `{"reason":"too senior","match":false}` + " \n", false},
	}
	for _, test := range valid {
		got, err := parseVerdict(test.content)
		if err != nil || got.Match != test.matched {
			t.Errorf("parseVerdict(%q) = (%+v, %v)", test.content, got, err)
		}
	}

	invalid := []string{
		`{"reason":"missing match"}`,
		`{"match":true}`,
		`{"match":null,"reason":"x"}`,
		`{"match":true,"reason":null}`,
		`{"Match":true,"reason":"wrong case"}`,
		`{"match":true,"Reason":"wrong case"}`,
		`{"match":true,"reason":"x","extra":1}`,
		`{"match":true,"match":false,"reason":"duplicate"}`,
		`{"match":"true","reason":"wrong type"}`,
		`{"match":true,"reason":7}`,
		"```json\n" + `{"match":true,"reason":"fenced"}` + "\n```",
		`Here is the verdict: {"match":true,"reason":"prose"}`,
		`{"match":false,"reason":"truncated`,
		`{"match":true,"reason":"x"} {"match":false,"reason":"y"}`,
		`[]`,
		`null`,
		``,
	}
	for _, content := range invalid {
		if got, err := parseVerdict(content); err == nil {
			t.Errorf("parseVerdict(%q) = %+v, want error", content, got)
		}
	}
}

func TestLLMHTTPFailuresReturnErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				http.Error(w, `{"error":"provider rejected request"}`, status)
			}))
			defer srv.Close()

			if result, err := newLLM(t, srv.URL, nil).Match(context.Background(), llmJob); err == nil || result.Matched {
				t.Fatalf("Match() = (%+v, %v), want indeterminate error", result, err)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1", attempts)
			}
		})
	}
}

func TestLLMRetriesCurrentTransientStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				if attempts == 1 {
					w.Header().Set("Retry-After", "0.001")
					http.Error(w, `{"error":"transient"}`, status)
					return
				}
				fmt.Fprint(w, completionReply(`{"match":true,"reason":"fits"}`))
			}))
			defer srv.Close()

			result := matchLLM(t, newLLM(t, srv.URL, nil))
			if !result.Matched || attempts != 2 {
				t.Errorf("success after retry = %+v after %d attempts", result, attempts)
			}
		})
	}
}

func TestLLMTransientFailureExhaustion(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "0.001")
		http.Error(w, `{"error":"still unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := newLLM(t, srv.URL, nil).Match(context.Background(), llmJob); err == nil {
		t.Fatal("Match() should return an error after retry exhaustion")
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
}

func TestLLMCancellationStopsActiveRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	m := newLLM(t, srv.URL, nil)
	go func() {
		_, err := m.Match(ctx, llmJob)
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Match() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Match() did not stop after cancellation")
	}
}

func TestLLMCancellationStopsRetryWait(t *testing.T) {
	requested := make(chan struct{})
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "60")
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		close(requested)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	m := newLLM(t, srv.URL, nil)
	go func() {
		_, err := m.Match(ctx, llmJob)
		done <- err
	}()
	<-requested
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Match() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Match() remained in retry wait after cancellation")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestLLMTotalDeadlineIncludesRetryWait(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "60")
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m := newLLM(t, srv.URL, nil)
	m.timeout = 25 * time.Millisecond
	start := time.Now()
	_, err := m.Match(context.Background(), llmJob)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Match() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("total deadline took %s", elapsed)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestLLMMalformedCompletionsReturnErrors(t *testing.T) {
	responses := []string{
		`not JSON`,
		`{"choices":[]}`,
		completionReply(`{"reason":"missing match"}`),
		completionReply("```json\n" + `{"match":true,"reason":"fenced"}` + "\n```"),
	}
	for _, response := range responses {
		t.Run(truncateStr(response, 30), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, response)
			}))
			defer srv.Close()
			if _, err := newLLM(t, srv.URL, nil).Match(context.Background(), llmJob); err == nil {
				t.Fatal("Match() should reject malformed completion")
			}
		})
	}
}

func TestLLMAuthHeader(t *testing.T) {
	t.Setenv("JOBWATCH_TEST_LLM_KEY", "sk-test-123")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, completionReply(`{"match":false,"reason":"x"}`))
	}))
	defer srv.Close()

	matchLLM(t, newLLM(t, srv.URL, params.Map{"api_key_env": "JOBWATCH_TEST_LLM_KEY"}))
	if gotAuth != "Bearer sk-test-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestLLMConfigErrors(t *testing.T) {
	for _, p := range []params.Map{
		{"base_url": "https://x", "model": "m"},
		{"profile": "p", "model": "m"},
		{"profile": "p", "base_url": "https://x"},
		{"profile": "p", "base_url": "https://x", "model": "m", "on_error": "match"},
		{"profile": "p", "base_url": "https://x", "model": "m", "on_error": "skip"},
		{"profile": "p", "base_url": "https://x", "model": "m", "on_error": "defer"},
	} {
		if _, err := Build(Spec{Name: "llm", Params: p}); err == nil {
			t.Errorf("Build with params %v should fail", p)
		}
	}
}
