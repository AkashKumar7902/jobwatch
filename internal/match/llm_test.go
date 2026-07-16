package match

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func newLLM(t *testing.T, baseURL string, extra params.Map) Matcher {
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
	return m
}

var llmJob = model.Job{Company: "Acme", Title: "Junior Go Developer", Location: "Remote, Worldwide", Description: "Build APIs in Go."}

func TestLLMMatcher(t *testing.T) {
	var gotBody map[string]any
	reply := `{"match": true, "reason": "junior Go role, globally remote"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		fmt.Fprint(w, completionReply(reply))
	}))
	defer srv.Close()

	m := newLLM(t, srv.URL, nil)
	res := m.Match(llmJob)
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

	reply = `Sure! Here is my verdict:` + "\n```json\n" + `{"match": false, "reason": "requires 7 years"}` + "\n```"
	res = m.Match(llmJob)
	if res.Matched || !strings.Contains(res.Reason, "requires 7 years") {
		t.Errorf("prose-wrapped verdict misparsed: %+v", res)
	}
}

func TestLLMErrorPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	failOpen := newLLM(t, srv.URL, nil) // default on_error: match
	if res := failOpen.Match(llmJob); !res.Matched {
		t.Errorf("default policy should fail open, got %+v", res)
	}
	failClosed := newLLM(t, srv.URL, params.Map{"on_error": "skip"})
	if res := failClosed.Match(llmJob); res.Matched {
		t.Errorf("on_error=skip should fail closed, got %+v", res)
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

	m := newLLM(t, srv.URL, params.Map{"api_key_env": "JOBWATCH_TEST_LLM_KEY"})
	m.Match(llmJob)
	if gotAuth != "Bearer sk-test-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestLLMConfigErrors(t *testing.T) {
	for _, p := range []params.Map{
		{"base_url": "https://x", "model": "m"},                                  // no profile
		{"profile": "p", "model": "m"},                                           // no base_url
		{"profile": "p", "base_url": "https://x"},                                // no model
		{"profile": "p", "base_url": "https://x", "model": "m", "on_error": "z"}, // bad policy
	} {
		if _, err := Build(Spec{Name: "llm", Params: p}); err == nil {
			t.Errorf("Build with params %v should fail", p)
		}
	}
}
