package match

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/model"
)

func requireLLMErrorInfo(t *testing.T, err error, want diagnostic.LLMErrorInfo) {
	t.Helper()
	got, ok := LLMErrorInfoOf(err)
	if !ok {
		t.Fatalf("LLMErrorInfoOf(%T %v) was not classified", err, err)
	}
	if got != want {
		t.Fatalf("LLM error info = %+v, want %+v", got, want)
	}
	if kind, ok := LLMErrorKindOf(err); !ok || kind != want.Kind {
		t.Fatalf("LLMErrorKindOf() = (%v, %t), want (%v, true)", kind, ok, want.Kind)
	}
}

func TestLLMHTTPStatusTaxonomyAndSingleAttemptPath(t *testing.T) {
	tests := []struct {
		status int
		kind   diagnostic.LLMErrorKind
	}{
		{http.StatusBadRequest, diagnostic.LLMBadRequest},
		{http.StatusUnauthorized, diagnostic.LLMUnauthorized},
		{http.StatusForbidden, diagnostic.LLMForbidden},
		{http.StatusNotFound, diagnostic.LLMNotFound},
		{http.StatusTooManyRequests, diagnostic.LLMRateLimited},
		{http.StatusServiceUnavailable, diagnostic.LLMServer},
		{http.StatusTeapot, diagnostic.LLMHTTPStatus},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("status_%d", test.status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				fmt.Fprint(w, `{"error":{"status":"UNRECOGNIZED_MACHINE_VALUE","message":"BODY_SECRET"}}`)
			}))
			defer srv.Close()

			m := newLLM(t, srv.URL, nil)
			_, err := m.askWithAttempts(context.Background(), llmJob, 1)
			requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
				Kind: test.kind, HTTPStatus: test.status, ProviderStatus: diagnostic.LLMProviderUnknown,
			})
			if calls.Load() != 1 {
				t.Fatalf("single-attempt request made %d calls", calls.Load())
			}
		})
	}
}

func TestLLMErrorNeverPrintsProviderOrRequestData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer KEY_SECRET" {
			t.Errorf("authorization = %q", got)
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"status":"INVALID_ARGUMENT","message":"BODY_SECRET PROMPT_SECRET JOB_SECRET KEY_SECRET"}}`)
	}))
	defer srv.Close()

	m := newLLM(t, srv.URL, nil)
	m.endpoint = srv.URL + "/chat/completions?token=URL_SECRET"
	m.apiKey = "KEY_SECRET"
	m.profile = "PROMPT_SECRET"
	job := llmJob
	job.Title = "JOB_SECRET"
	_, err := m.Match(context.Background(), job)
	requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
		Kind: diagnostic.LLMBadRequest, HTTPStatus: 400,
		ProviderStatus: diagnostic.LLMProviderInvalidArgument,
	})
	printed := fmt.Sprintf("%v", err)
	for _, secret := range []string{"BODY_SECRET", "PROMPT_SECRET", "JOB_SECRET", "KEY_SECRET", "URL_SECRET", srv.URL} {
		if strings.Contains(printed, secret) {
			t.Errorf("printable error exposed %q: %q", secret, printed)
		}
	}
	for _, want := range []string{"bad_request", "HTTP 400", "invalid_argument"} {
		if !strings.Contains(printed, want) {
			t.Errorf("printable error %q missing %q", printed, want)
		}
	}

	wrapped := errors.Join(fmt.Errorf("all: %w", err), errors.New("another matcher failed"))
	requireLLMErrorInfo(t, wrapped, diagnostic.LLMErrorInfo{
		Kind: diagnostic.LLMBadRequest, HTTPStatus: 400,
		ProviderStatus: diagnostic.LLMProviderInvalidArgument,
	})
}

func TestNormalizeProviderStatusUsesOnlyClosedMachineFields(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		http   int
		status diagnostic.LLMProviderStatus
	}{
		{"detail reason", `{"error":{"details":[{"reason":"API_KEY_INVALID"}]}}`, 400, diagnostic.LLMProviderUnauthenticated},
		{"later recognized field", `{"error":{"code":"novel","status":"INVALID_ARGUMENT"}}`, 400, diagnostic.LLMProviderInvalidArgument},
		{"openai type", `{"error":{"type":"invalid_request_error"}}`, 400, diagnostic.LLMProviderInvalidArgument},
		{"model code", `{"error":{"code":"model_not_found"}}`, 404, diagnostic.LLMProviderNotFound},
		{"blocked key alias", `{"error":{"reason":"api-key-project-blocked"}}`, 403, diagnostic.LLMProviderPermissionDenied},
		{"service disabled", `{"error":{"status":"SERVICE_DISABLED"}}`, 403, diagnostic.LLMProviderPermissionDenied},
		{"policy", `{"error":{"reason":"content_policy_violation"}}`, 400, diagnostic.LLMProviderPolicyBlocked},
		{"unknown machine field", `{"error":{"status":"SOMETHING_NEW"}}`, 401, diagnostic.LLMProviderUnknown},
		{"free text ignored", `{"error":{"message":"INVALID_ARGUMENT"}}`, 401, diagnostic.LLMProviderUnauthenticated},
		{"malformed fallback", `{`, 503, diagnostic.LLMProviderUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeProviderStatus([]byte(test.body), test.http); got != test.status {
				t.Fatalf("normalizeProviderStatus() = %s, want %s", got.Token(), test.status.Token())
			}
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestLLMResponseAndTransportFailureTaxonomy(t *testing.T) {
	tests := []struct {
		name string
		body string
		kind diagnostic.LLMErrorKind
	}{
		{"decode", `not json`, diagnostic.LLMDecode},
		{"no choices", `{"choices":[]}`, diagnostic.LLMNoChoices},
		{"invalid verdict", completionReply(`{"reason":"missing match"}`), diagnostic.LLMInvalidVerdict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, test.body)
			}))
			defer srv.Close()
			_, err := newLLM(t, srv.URL, nil).Match(context.Background(), llmJob)
			requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
				Kind: test.kind, HTTPStatus: 200, ProviderStatus: diagnostic.LLMProviderNone,
			})
		})
	}

	m := newLLM(t, "https://unused.invalid", nil)
	m.client.Transport = llmRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Body: io.NopCloser(failingReader{errors.New("READ_SECRET")}),
			Header: make(http.Header),
		}, nil
	})
	_, err := m.Match(context.Background(), llmJob)
	requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
		Kind: diagnostic.LLMRead, HTTPStatus: 200, ProviderStatus: diagnostic.LLMProviderNone,
	})
	if strings.Contains(err.Error(), "READ_SECRET") {
		t.Fatalf("read error leaked cause: %v", err)
	}

	m = newLLM(t, "https://unused.invalid", nil)
	m.client.Transport = llmRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), maxLLMResponseBytes+1))),
			Header: make(http.Header),
		}, nil
	})
	_, err = m.Match(context.Background(), llmJob)
	requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
		Kind: diagnostic.LLMTooLarge, HTTPStatus: 200, ProviderStatus: diagnostic.LLMProviderNone,
	})

	m = newLLM(t, "https://unused.invalid", nil)
	m.client.Transport = llmRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("TRANSPORT_SECRET")
	})
	_, err = m.askWithAttempts(context.Background(), llmJob, 1)
	requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
		Kind: diagnostic.LLMTransport, ProviderStatus: diagnostic.LLMProviderNone,
	})
	if strings.Contains(err.Error(), "TRANSPORT_SECRET") {
		t.Fatalf("transport error leaked cause: %v", err)
	}

	m = newLLM(t, "https://unused.invalid", nil)
	m.timeout = 5 * time.Millisecond
	m.client.Transport = llmRoundTripper(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	_, err = m.Match(context.Background(), llmJob)
	requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
		Kind: diagnostic.LLMTimeout, ProviderStatus: diagnostic.LLMProviderNone,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout lost errors.Is identity: %v", err)
	}
}

func TestLLMCircuitBreakerStopsAfterThreeEquivalentPermanentFailuresAndResets(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"status":"INVALID_ARGUMENT"}}`)
	}))
	defer srv.Close()
	m := newLLM(t, srv.URL, nil)

	for index := 0; index < 25; index++ {
		_, err := m.Match(context.Background(), llmJob)
		wantKind := diagnostic.LLMBadRequest
		if index >= llmBreakerThreshold {
			wantKind = diagnostic.LLMCircuitOpen
		}
		requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
			Kind: wantKind, HTTPStatus: 400, ProviderStatus: diagnostic.LLMProviderInvalidArgument,
		})
	}
	if got := calls.Load(); got != llmBreakerThreshold {
		t.Fatalf("HTTP calls = %d, want exactly %d", got, llmBreakerThreshold)
	}

	ResetRun(m)
	_, err := m.Match(context.Background(), llmJob)
	requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
		Kind: diagnostic.LLMBadRequest, HTTPStatus: 400,
		ProviderStatus: diagnostic.LLMProviderInvalidArgument,
	})
	if got := calls.Load(); got != llmBreakerThreshold+1 {
		t.Fatalf("HTTP calls after run reset = %d, want %d", got, llmBreakerThreshold+1)
	}
}

func TestLLMCircuitBreakerIsRaceSafe(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"status":"PERMISSION_DENIED"}}`)
	}))
	defer srv.Close()
	m := newLLM(t, srv.URL, nil)

	const workers = 40
	start := make(chan struct{})
	results := make(chan diagnostic.LLMErrorKind, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := m.Match(context.Background(), llmJob)
			info, ok := LLMErrorInfoOf(err)
			if !ok {
				results <- 0
				return
			}
			results <- info.Kind
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	counts := map[diagnostic.LLMErrorKind]int{}
	for kind := range results {
		counts[kind]++
	}
	if calls.Load() != llmBreakerThreshold || counts[diagnostic.LLMForbidden] != llmBreakerThreshold ||
		counts[diagnostic.LLMCircuitOpen] != workers-llmBreakerThreshold {
		t.Fatalf("calls=%d kinds=%v", calls.Load(), counts)
	}
}

func TestLLMCircuitBreakerRequiresEquivalentPermanentFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		status := http.StatusConflict
		if call%2 == 0 {
			status = http.StatusUnprocessableEntity
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":{"status":"UNRECOGNIZED_MACHINE_VALUE"}}`)
	}))
	defer srv.Close()
	m := newLLM(t, srv.URL, nil)

	const jobs = 12
	for range jobs {
		_, err := m.Match(context.Background(), llmJob)
		info, ok := LLMErrorInfoOf(err)
		if !ok || info.Kind != diagnostic.LLMHTTPStatus {
			t.Fatalf("non-equivalent failure = (%+v, %v)", info, err)
		}
	}
	if got := calls.Load(); got != jobs {
		t.Fatalf("alternating permanent statuses made %d calls, want %d", got, jobs)
	}
}

func TestLLMTransientFailuresNeverOpenPermanentCircuit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0.001")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer srv.Close()
	m := newLLM(t, srv.URL, nil)

	const jobs = 6
	for range jobs {
		_, err := m.Match(context.Background(), llmJob)
		requireLLMErrorInfo(t, err, diagnostic.LLMErrorInfo{
			Kind: diagnostic.LLMRateLimited, HTTPStatus: 429,
			ProviderStatus: diagnostic.LLMProviderResourceExhausted,
		})
	}
	if got, want := calls.Load(), int32(jobs*4); got != want {
		t.Fatalf("transient HTTP calls = %d, want %d (no open circuit)", got, want)
	}
}

type resetProbe struct{ calls int }

func (p *resetProbe) Name() string { return "probe" }
func (p *resetProbe) Match(context.Context, model.Job) (Result, error) {
	return Result{}, nil
}
func (p *resetProbe) ResetRun() { p.calls++ }

func TestCombinatorsForwardRunReset(t *testing.T) {
	probe := &resetProbe{}
	ResetRun(&all{children: []Matcher{&any_{children: []Matcher{&not{child: probe}}}}})
	if probe.calls != 1 {
		t.Fatalf("nested reset calls = %d, want 1", probe.calls)
	}
}
