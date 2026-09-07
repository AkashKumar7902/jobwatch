package match

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/model"
)

// PreflightCategory is deliberately closed and contains no provider-supplied
// text. It is safe to print in CI logs even when an upstream response includes
// a request echo, credential, prompt, or job content.
type PreflightCategory string

const (
	PreflightNone              PreflightCategory = "none"
	PreflightConfiguration     PreflightCategory = "configuration"
	PreflightCredentialMissing PreflightCategory = "credential_missing"
	PreflightCancelled         PreflightCategory = "cancelled"
	PreflightUnknown           PreflightCategory = "unknown"
)

// LLMPreflightResult keeps its representation private so callers cannot
// accidentally put arbitrary strings into the public protocol. Its accessors
// return only closed diagnostic tokens and bounded numeric status values.
type LLMPreflightResult struct {
	success bool
	setup   PreflightCategory
	info    diagnostic.LLMErrorInfo
}

func (r LLMPreflightResult) OK() bool { return r.success }

func (r LLMPreflightResult) CategoryToken() string {
	if r.success {
		return string(PreflightNone)
	}
	switch r.setup {
	case PreflightConfiguration, PreflightCredentialMissing, PreflightCancelled, PreflightUnknown:
		return string(r.setup)
	}
	if r.info.Valid() {
		return r.info.Kind.Token()
	}
	return string(PreflightUnknown)
}

func (r LLMPreflightResult) HTTPStatus() int {
	if r.success {
		return http.StatusOK
	}
	if r.info.Valid() {
		return r.info.HTTPStatus
	}
	return 0
}

func (r LLMPreflightResult) ProviderStatusToken() string {
	if r.info.Valid() {
		return r.info.ProviderStatus.Token()
	}
	return diagnostic.LLMProviderNone.Token()
}

// This is intentionally synthetic, fixed, and free of state or fetched job
// content. The verdict itself is irrelevant: a schema-valid true or false reply
// proves the configured production request and response path is usable.
var llmPreflightJob = model.Job{
	ID:             "jobwatch-preflight/synthetic/1",
	Company:        "JobWatch synthetic preflight",
	Title:          "Software Engineer I",
	Location:       "India",
	EmploymentType: "Full-time",
	Description:    "Synthetic diagnostic posting requiring 0-1 years of software development experience.",
}

// LLMPreflight validates that the configured matcher tree contains exactly one
// LLM leaf, then sends exactly one request through that leaf's production
// endpoint/model/auth/schema/parser path. It never retries and returns only
// sealed status fields; callers must not print the underlying error.
func LLMPreflight(ctx context.Context, root Matcher) LLMPreflightResult {
	var leaves []*llm
	collectLLMLeaves(root, &leaves)
	if len(leaves) != 1 {
		return PreflightSetupResult(PreflightConfiguration)
	}

	provider := leaves[0]
	// http.Client follows redirects by default, and each hop is another HTTP
	// request. Use an otherwise-identical client that returns the redirect
	// response to the normal status classifier so "one request" is literal.
	var oneRequestClient *http.Client
	if provider.client != nil {
		oneRequestClient = &http.Client{
			Transport: provider.client.Transport,
			Jar:       provider.client.Jar,
			Timeout:   provider.client.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	requestCtx, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	_, err := provider.askWithClientAndAttempts(requestCtx, llmPreflightJob, oneRequestClient, 1)
	if err == nil {
		return LLMPreflightResult{success: true}
	}
	if info, ok := LLMErrorInfoOf(err); ok {
		return LLMPreflightResult{info: info}
	}
	if errors.Is(err, context.Canceled) {
		return PreflightSetupResult(PreflightCancelled)
	}
	return PreflightSetupResult(PreflightUnknown)
}

// ClassifyPreflightSetupError seals configuration/build failures before they
// reach the logger. A missing configured credential gets its own actionable
// category; every other setup detail remains private.
func ClassifyPreflightSetupError(err error) LLMPreflightResult {
	if err == nil {
		return PreflightSetupResult(PreflightUnknown)
	}
	if strings.Contains(err.Error(), "environment variable is empty") {
		return PreflightSetupResult(PreflightCredentialMissing)
	}
	return PreflightSetupResult(PreflightConfiguration)
}

// PreflightSetupResult constructs one of the closed, response-free setup
// results used before an HTTP request can be made.
func PreflightSetupResult(category PreflightCategory) LLMPreflightResult {
	switch category {
	case PreflightConfiguration, PreflightCredentialMissing, PreflightCancelled, PreflightUnknown:
	default:
		category = PreflightUnknown
	}
	return LLMPreflightResult{setup: category}
}

func collectLLMLeaves(m Matcher, leaves *[]*llm) {
	switch typed := m.(type) {
	case *llm:
		*leaves = append(*leaves, typed)
	case *all:
		for _, child := range typed.children {
			collectLLMLeaves(child, leaves)
		}
	case *any_:
		for _, child := range typed.children {
			collectLLMLeaves(child, leaves)
		}
	case *not:
		collectLLMLeaves(typed.child, leaves)
	}
}
