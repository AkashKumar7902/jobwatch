// Package diagnostic carries bounded, non-sensitive operational signals from
// adapters and matchers to the runner. It deliberately cannot carry strings,
// errors, URLs, response bodies, headers, or configuration values.
package diagnostic

import (
	"context"
	"sync"
	"time"
)

const (
	maxCount = 1_000_000_000
	maxDelay = 24 * time.Hour
)

// RetryKind is a closed classification of retry paths that are useful to an
// operator without exposing the failed request or response.
type RetryKind uint8

const (
	RetryTransport RetryKind = iota + 1
	RetryRateLimit
	RetryServer
	RetrySnapshot
	RetryPage
)

// LLMErrorKind is a closed classification of failures returned by the LLM
// matcher. Values are deliberately coarse: they are safe to aggregate in a
// public log and cannot carry response bodies, request data, endpoints, or
// credentials.
type LLMErrorKind uint8

const (
	LLMBadRequest LLMErrorKind = iota + 1
	LLMUnauthorized
	LLMForbidden
	LLMNotFound
	LLMRateLimited
	LLMServer
	LLMHTTPStatus
	LLMTransport
	LLMTimeout
	LLMRead
	LLMTooLarge
	LLMDecode
	LLMNoChoices
	LLMInvalidVerdict
	LLMCircuitOpen
	llmErrorKindEnd
)

// Token returns the fixed public-protocol spelling for a kind. The empty
// string denotes a value outside the closed taxonomy.
func (k LLMErrorKind) Token() string {
	switch k {
	case LLMBadRequest:
		return "bad_request"
	case LLMUnauthorized:
		return "unauthorized"
	case LLMForbidden:
		return "forbidden"
	case LLMNotFound:
		return "not_found"
	case LLMRateLimited:
		return "rate_limited"
	case LLMServer:
		return "server"
	case LLMHTTPStatus:
		return "http_status"
	case LLMTransport:
		return "transport"
	case LLMTimeout:
		return "timeout"
	case LLMRead:
		return "read"
	case LLMTooLarge:
		return "too_large"
	case LLMDecode:
		return "decode"
	case LLMNoChoices:
		return "no_choices"
	case LLMInvalidVerdict:
		return "invalid_verdict"
	case LLMCircuitOpen:
		return "circuit_open"
	default:
		return ""
	}
}

// Permanent reports whether repeating this failure cannot reasonably be
// repaired by an immediate retry. Only permanent failures contribute to the
// per-run LLM circuit breaker.
func (k LLMErrorKind) Permanent() bool {
	switch k {
	case LLMBadRequest, LLMUnauthorized, LLMForbidden, LLMNotFound,
		LLMHTTPStatus, LLMTooLarge, LLMDecode, LLMNoChoices, LLMInvalidVerdict:
		return true
	default:
		return false
	}
}

// LLMProviderStatus is a closed normalization of provider-owned machine
// fields. It never contains provider message text.
type LLMProviderStatus uint8

const (
	LLMProviderNone LLMProviderStatus = iota
	LLMProviderInvalidArgument
	LLMProviderUnauthenticated
	LLMProviderPermissionDenied
	LLMProviderNotFound
	LLMProviderResourceExhausted
	LLMProviderFailedPrecondition
	LLMProviderAborted
	LLMProviderDeadlineExceeded
	LLMProviderUnavailable
	LLMProviderInternal
	LLMProviderUnimplemented
	LLMProviderPolicyBlocked
	LLMProviderUnknown
)

// Token returns the fixed public-protocol spelling for a provider status.
func (s LLMProviderStatus) Token() string {
	switch s {
	case LLMProviderNone:
		return "none"
	case LLMProviderInvalidArgument:
		return "invalid_argument"
	case LLMProviderUnauthenticated:
		return "unauthenticated"
	case LLMProviderPermissionDenied:
		return "permission_denied"
	case LLMProviderNotFound:
		return "not_found"
	case LLMProviderResourceExhausted:
		return "resource_exhausted"
	case LLMProviderFailedPrecondition:
		return "failed_precondition"
	case LLMProviderAborted:
		return "aborted"
	case LLMProviderDeadlineExceeded:
		return "deadline_exceeded"
	case LLMProviderUnavailable:
		return "unavailable"
	case LLMProviderInternal:
		return "internal"
	case LLMProviderUnimplemented:
		return "unimplemented"
	case LLMProviderPolicyBlocked:
		return "policy_blocked"
	case LLMProviderUnknown:
		return "unknown"
	default:
		return ""
	}
}

// LLMErrorInfo contains only bounded values from closed vocabularies. An HTTP
// status of zero means that no valid 100..599 provider response was available.
type LLMErrorInfo struct {
	Kind           LLMErrorKind
	HTTPStatus     int
	ProviderStatus LLMProviderStatus
}

// Valid reports whether the information is safe and internally consistent.
func (i LLMErrorInfo) Valid() bool {
	if i.Kind.Token() == "" || i.ProviderStatus.Token() == "" {
		return false
	}
	hasHTTP := i.HTTPStatus >= 100 && i.HTTPStatus <= 599
	switch i.Kind {
	case LLMTransport, LLMTimeout:
		return i.HTTPStatus == 0 && i.ProviderStatus == LLMProviderNone
	case LLMRead, LLMTooLarge:
		return hasHTTP && i.ProviderStatus == LLMProviderNone
	case LLMDecode, LLMNoChoices, LLMInvalidVerdict:
		return i.HTTPStatus == 200 && i.ProviderStatus == LLMProviderNone
	case LLMCircuitOpen:
		return (i.HTTPStatus == 0 && i.ProviderStatus == LLMProviderNone) || hasHTTP
	case LLMBadRequest:
		return i.HTTPStatus == 400 && i.ProviderStatus != LLMProviderNone
	case LLMUnauthorized:
		return i.HTTPStatus == 401 && i.ProviderStatus != LLMProviderNone
	case LLMForbidden:
		return i.HTTPStatus == 403 && i.ProviderStatus != LLMProviderNone
	case LLMNotFound:
		return i.HTTPStatus == 404 && i.ProviderStatus != LLMProviderNone
	case LLMRateLimited:
		return i.HTTPStatus == 429 && i.ProviderStatus != LLMProviderNone
	case LLMServer:
		return i.HTTPStatus >= 500 && i.HTTPStatus <= 599 && i.ProviderStatus != LLMProviderNone
	case LLMHTTPStatus:
		return hasHTTP && i.ProviderStatus != LLMProviderNone
	default:
		return false
	}
}

// LLMFailureCount is one non-sensitive aggregate returned by Snapshot.
type LLMFailureCount struct {
	LLMErrorInfo
	Count int
}

const maxLLMFailureGroups = 64

type collectorKey struct{}

// Collector is safe for the fetch goroutine and the later matcher calls that
// share its context. Callers can only add sealed numeric events through the
// functions below.
type Collector struct {
	mu          sync.Mutex
	retries     int
	caps        int
	llmFailures [maxLLMFailureGroups]LLMFailureCount
}

// Snapshot is the bounded aggregate consumed by a BOARD outcome.
type Snapshot struct {
	Retries     int
	Caps        int
	llmFailures [maxLLMFailureGroups]LLMFailureCount
}

// WithCollector installs a fresh board-local collector.
func WithCollector(ctx context.Context) (context.Context, *Collector) {
	c := &Collector{}
	return context.WithValue(ctx, collectorKey{}, c), c
}

// Cap records that an adapter deliberately returned a configured subset. The
// numeric arguments are accepted to keep call sites honest, but only the event
// count is exported: raw totals are not needed in public logs.
func Cap(ctx context.Context, returned, available int) {
	if returned < 0 || available < 0 || available < returned {
		return
	}
	_, _ = bound(returned), bound(available)
	c := fromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	c.caps = 1
	c.mu.Unlock()
}

// Retry records one retry without changing the retry loop's control flow.
func Retry(ctx context.Context, kind RetryKind, attempt, limit int, delay time.Duration) {
	if kind < RetryTransport || kind > RetryPage || attempt < 1 || limit < attempt {
		return
	}
	if delay < 0 {
		delay = 0
	} else if delay > maxDelay {
		delay = maxDelay
	}
	_ = delay
	c := fromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	c.retries = bound(c.retries + 1)
	c.mu.Unlock()
}

// RecordLLMFailure records one fully classified matcher failure. Distinct
// safe classifications are kept separate up to a fixed bound; excess novel
// groups are dropped rather than allocating attacker-controlled memory.
func RecordLLMFailure(ctx context.Context, info LLMErrorInfo) {
	if !info.Valid() {
		return
	}
	c := fromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for index := range c.llmFailures {
		failure := &c.llmFailures[index]
		if failure.Count > 0 && failure.LLMErrorInfo == info {
			failure.Count = bound(failure.Count + 1)
			return
		}
	}
	for index := range c.llmFailures {
		failure := &c.llmFailures[index]
		if failure.Count == 0 {
			*failure = LLMFailureCount{LLMErrorInfo: info, Count: 1}
			return
		}
	}
}

// Snapshot returns an immutable aggregate.
func (c *Collector) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{Retries: c.retries, Caps: c.caps, llmFailures: c.llmFailures}
}

// LLMFailures returns non-zero LLM aggregates in observation order. It returns a
// copy, so callers cannot mutate the collector's immutable snapshot.
func (s Snapshot) LLMFailures() []LLMFailureCount {
	counts := make([]LLMFailureCount, 0, len(s.llmFailures))
	for _, failure := range s.llmFailures {
		if failure.Count > 0 {
			counts = append(counts, failure)
		}
	}
	return counts
}

func fromContext(ctx context.Context) *Collector {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(collectorKey{}).(*Collector)
	return c
}

func bound(n int) int {
	if n < 0 {
		return 0
	}
	if n > maxCount {
		return maxCount
	}
	return n
}
