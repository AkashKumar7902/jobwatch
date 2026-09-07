package match

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/params"
)

func requirePreflightResult(t *testing.T, got LLMPreflightResult, category string, httpStatus int, providerStatus string) {
	t.Helper()
	if got.CategoryToken() != category || got.HTTPStatus() != httpStatus || got.ProviderStatusToken() != providerStatus {
		t.Fatalf("preflight result = (%s, %d, %s), want (%s, %d, %s)",
			got.CategoryToken(), got.HTTPStatus(), got.ProviderStatusToken(), category, httpStatus, providerStatus)
	}
}

func preflightSpec(baseURL string) Spec {
	return Spec{
		Name: "all",
		Of: []Spec{
			// This sibling deliberately rejects the fixed synthetic job. The
			// preflight must locate and probe the LLM leaf directly rather than
			// short-circuiting through the configured combinator root.
			{Name: "keywords", Params: params.Map{"field": "title", "include": "definitely-not-in-the-synthetic-title"}},
			{Name: "llm", Params: params.Map{
				"profile":      "preflight test profile",
				"base_url":     baseURL,
				"model":        "test-model",
				"api_key_env":  "JOBWATCH_PREFLIGHT_TEST_KEY",
				"max_tokens":   "9123",
				"instructions": "Apply the configured preflight instruction.",
			}},
		},
	}
}

func TestLLMPreflightUsesProductionRequestOnce(t *testing.T) {
	t.Setenv("JOBWATCH_PREFLIGHT_TEST_KEY", "secret-test-key")
	var requests atomic.Int32
	var authorization atomic.Value
	var contentType atomic.Value
	var requestPath atomic.Value
	var requestBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		authorization.Store(r.Header.Get("Authorization"))
		contentType.Store(r.Header.Get("Content-Type"))
		requestPath.Store(r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		requestBody.Store(raw)
		fmt.Fprint(w, completionReply(`{"match":false,"reason":"valid synthetic verdict"}`))
	}))
	defer srv.Close()

	configured, err := Build(preflightSpec(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	result := LLMPreflight(context.Background(), configured)
	requirePreflightResult(t, result, "none", http.StatusOK, "none")
	if !result.OK() {
		t.Fatal("successful preflight is not OK")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want exactly 1", got)
	}
	if authorization.Load() != "Bearer secret-test-key" {
		t.Fatalf("Authorization header does not use the configured key")
	}
	if contentType.Load() != "application/json" {
		t.Fatalf("Content-Type = %v", contentType.Load())
	}
	if requestPath.Load() != "/chat/completions" {
		t.Fatalf("request path = %v", requestPath.Load())
	}
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		ResponseFormat map[string]any `json:"response_format"`
	}
	if err := json.Unmarshal(requestBody.Load().([]byte), &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "test-model" || body.MaxTokens != 9123 || len(body.Messages) != 2 {
		t.Fatalf("preflight request shape = %+v", body)
	}
	wantSystem := llmSystemPrompt + "\n\nAdditional matching rules from the user (these override the defaults above where they conflict):\nApply the configured preflight instruction."
	if body.Messages[0].Role != "system" || body.Messages[0].Content != wantSystem {
		t.Fatalf("preflight system prompt differs from configured production prompt")
	}
	wantUser := "Candidate profile: preflight test profile\n\n" +
		"Job posting:\n" +
		"Company: JobWatch synthetic preflight\n" +
		"Title: Software Engineer I\n" +
		"Location: India\n" +
		"Employment type: Full-time\n" +
		"Description:\nSynthetic diagnostic posting requiring 0-1 years of software development experience."
	if body.Messages[1].Role != "user" || body.Messages[1].Content != wantUser {
		t.Fatalf("preflight synthetic prompt changed: %q", body.Messages[1].Content)
	}
	if body.ResponseFormat["type"] != "json_schema" {
		t.Fatalf("preflight response format = %v", body.ResponseFormat)
	}
	gotFormat, _ := json.Marshal(body.ResponseFormat)
	wantFormat, _ := json.Marshal(verdictResponseFormat())
	if !bytes.Equal(gotFormat, wantFormat) {
		t.Fatalf("preflight response schema differs from production schema:\n got %s\nwant %s", gotFormat, wantFormat)
	}
}

func TestLLMPreflightNeverRetries(t *testing.T) {
	t.Setenv("JOBWATCH_PREFLIGHT_TEST_KEY", "secret-test-key")
	tests := []struct {
		name         string
		status       int
		wantCategory string
		wantProvider string
		completion   string
	}{
		{name: "bad request", status: http.StatusBadRequest, wantCategory: "bad_request", wantProvider: "invalid_argument"},
		{name: "authentication", status: http.StatusUnauthorized, wantCategory: "unauthorized", wantProvider: "unauthenticated"},
		{name: "authorization", status: http.StatusForbidden, wantCategory: "forbidden", wantProvider: "permission_denied"},
		{name: "not found", status: http.StatusNotFound, wantCategory: "not_found", wantProvider: "not_found"},
		{name: "rate limited", status: http.StatusTooManyRequests, wantCategory: "rate_limited", wantProvider: "resource_exhausted"},
		{name: "provider error", status: http.StatusServiceUnavailable, wantCategory: "server", wantProvider: "unavailable"},
		{name: "no choices", status: http.StatusOK, completion: `{"choices":[]}`, wantCategory: "no_choices", wantProvider: "none"},
		{name: "decode", status: http.StatusOK, completion: `not JSON`, wantCategory: "decode", wantProvider: "none"},
		{name: "invalid verdict", status: http.StatusOK, completion: completionReply(`{"match":true}`), wantCategory: "invalid_verdict", wantProvider: "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "0.001")
				w.WriteHeader(tc.status)
				if tc.completion != "" {
					fmt.Fprint(w, tc.completion)
				} else {
					fmt.Fprint(w, `provider body with secret-test-key and private prompt text`)
				}
			}))
			defer srv.Close()

			configured, err := Build(preflightSpec(srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			result := LLMPreflight(context.Background(), configured)
			requirePreflightResult(t, result, tc.wantCategory, tc.status, tc.wantProvider)
			if result.OK() {
				t.Fatal("failed preflight reported OK")
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("provider requests = %d, want exactly 1", got)
			}
		})
	}
}

func TestLLMPreflightDoesNotFollowRedirects(t *testing.T) {
	t.Setenv("JOBWATCH_PREFLIGHT_TEST_KEY", "secret-test-key")
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		fmt.Fprint(w, completionReply(`{"match":true,"reason":"must not be reached"}`))
	}))
	defer target.Close()

	var originRequests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests.Add(1)
		http.Redirect(w, r, target.URL+"/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	configured, err := Build(preflightSpec(origin.URL))
	if err != nil {
		t.Fatal(err)
	}
	requirePreflightResult(t, LLMPreflight(context.Background(), configured), "http_status", http.StatusTemporaryRedirect, "unknown")
	if got := originRequests.Load(); got != 1 {
		t.Fatalf("origin requests = %d, want 1", got)
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
}

func TestLLMPreflightTransportFailureIsOneAttempt(t *testing.T) {
	t.Setenv("JOBWATCH_PREFLIGHT_TEST_KEY", "secret-test-key")
	configured, err := Build(preflightSpec("https://preflight.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	var leaves []*llm
	collectLLMLeaves(configured, &leaves)
	var requests atomic.Int32
	leaves[0].client.Transport = llmRoundTripper(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("private transport failure")
	})

	requirePreflightResult(t, LLMPreflight(context.Background(), configured), "transport", 0, "none")
	if got := requests.Load(); got != 1 {
		t.Fatalf("transport attempts = %d, want exactly 1", got)
	}
}

type preflightTimeoutError struct{}

func (preflightTimeoutError) Error() string { return "private timeout detail" }
func (preflightTimeoutError) Timeout() bool { return true }

func TestLLMPreflightClassifiesCancellationAndTimeout(t *testing.T) {
	t.Setenv("JOBWATCH_PREFLIGHT_TEST_KEY", "secret-test-key")

	t.Run("cancelled before request", func(t *testing.T) {
		configured, err := Build(preflightSpec("https://preflight.invalid"))
		if err != nil {
			t.Fatal(err)
		}
		var leaves []*llm
		collectLLMLeaves(configured, &leaves)
		var requests atomic.Int32
		leaves[0].client.Transport = llmRoundTripper(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, context.Canceled
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		requirePreflightResult(t, LLMPreflight(ctx, configured), "cancelled", 0, "none")
		if got := requests.Load(); got > 1 {
			t.Fatalf("transport attempts = %d, want at most 1", got)
		}
	})

	t.Run("transport timeout", func(t *testing.T) {
		configured, err := Build(preflightSpec("https://preflight.invalid"))
		if err != nil {
			t.Fatal(err)
		}
		var leaves []*llm
		collectLLMLeaves(configured, &leaves)
		var requests atomic.Int32
		leaves[0].client.Transport = llmRoundTripper(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, preflightTimeoutError{}
		})

		requirePreflightResult(t, LLMPreflight(context.Background(), configured), "timeout", 0, "none")
		if got := requests.Load(); got != 1 {
			t.Fatalf("transport attempts = %d, want exactly 1", got)
		}
	})
}

func TestLLMPreflightRequiresExactlyOneConfiguredLeaf(t *testing.T) {
	without, err := Build(Spec{Name: "experience", Params: params.Map{"years": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	requirePreflightResult(t, LLMPreflight(context.Background(), without), "configuration", 0, "none")

	t.Setenv("JOBWATCH_PREFLIGHT_TEST_KEY", "secret-test-key")
	leaf := func() Spec { return preflightSpec("https://example.invalid").Of[1] }
	withTwo, err := Build(Spec{Name: "any", Of: []Spec{leaf(), leaf()}})
	if err != nil {
		t.Fatal(err)
	}
	requirePreflightResult(t, LLMPreflight(context.Background(), withTwo), "configuration", 0, "none")
}

func TestPreflightClassifiersAreClosed(t *testing.T) {
	requirePreflightResult(t, ClassifyPreflightSetupError(nil), "unknown", 0, "none")
	requirePreflightResult(t,
		ClassifyPreflightSetupError(errors.New("api_key_env X is set in config but the environment variable is empty")),
		"credential_missing", 0, "none")
	requirePreflightResult(t,
		ClassifyPreflightSetupError(errors.New("private malformed configuration detail")),
		"configuration", 0, "none")
	requirePreflightResult(t, PreflightSetupResult(PreflightCategory("private arbitrary token")), "unknown", 0, "none")

	infos := []diagnostic.LLMErrorInfo{
		{Kind: diagnostic.LLMBadRequest, HTTPStatus: 400, ProviderStatus: diagnostic.LLMProviderInvalidArgument},
		{Kind: diagnostic.LLMUnauthorized, HTTPStatus: 401, ProviderStatus: diagnostic.LLMProviderUnauthenticated},
		{Kind: diagnostic.LLMForbidden, HTTPStatus: 403, ProviderStatus: diagnostic.LLMProviderPermissionDenied},
		{Kind: diagnostic.LLMNotFound, HTTPStatus: 404, ProviderStatus: diagnostic.LLMProviderNotFound},
		{Kind: diagnostic.LLMRateLimited, HTTPStatus: 429, ProviderStatus: diagnostic.LLMProviderResourceExhausted},
		{Kind: diagnostic.LLMServer, HTTPStatus: 503, ProviderStatus: diagnostic.LLMProviderUnavailable},
		{Kind: diagnostic.LLMHTTPStatus, HTTPStatus: 307, ProviderStatus: diagnostic.LLMProviderUnknown},
		{Kind: diagnostic.LLMTransport, ProviderStatus: diagnostic.LLMProviderNone},
		{Kind: diagnostic.LLMTimeout, ProviderStatus: diagnostic.LLMProviderNone},
		{Kind: diagnostic.LLMRead, HTTPStatus: 200, ProviderStatus: diagnostic.LLMProviderNone},
		{Kind: diagnostic.LLMTooLarge, HTTPStatus: 200, ProviderStatus: diagnostic.LLMProviderNone},
		{Kind: diagnostic.LLMDecode, HTTPStatus: 200, ProviderStatus: diagnostic.LLMProviderNone},
		{Kind: diagnostic.LLMNoChoices, HTTPStatus: 200, ProviderStatus: diagnostic.LLMProviderNone},
		{Kind: diagnostic.LLMInvalidVerdict, HTTPStatus: 200, ProviderStatus: diagnostic.LLMProviderNone},
		{Kind: diagnostic.LLMCircuitOpen, HTTPStatus: 400, ProviderStatus: diagnostic.LLMProviderInvalidArgument},
	}
	for _, info := range infos {
		result := LLMPreflightResult{info: info}
		requirePreflightResult(t, result, info.Kind.Token(), info.HTTPStatus, info.ProviderStatus.Token())
	}
}
