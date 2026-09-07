package diagnostic

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestMissingCollectorIsNoOp(t *testing.T) {
	Cap(context.Background(), 5, 10)
	Retry(context.Background(), RetryServer, 1, 4, time.Second)
}

func TestLLMFailureTaxonomyAndAggregation(t *testing.T) {
	wants := []struct {
		kind      LLMErrorKind
		token     string
		permanent bool
	}{
		{LLMBadRequest, "bad_request", true},
		{LLMUnauthorized, "unauthorized", true},
		{LLMForbidden, "forbidden", true},
		{LLMNotFound, "not_found", true},
		{LLMRateLimited, "rate_limited", false},
		{LLMServer, "server", false},
		{LLMHTTPStatus, "http_status", true},
		{LLMTransport, "transport", false},
		{LLMTimeout, "timeout", false},
		{LLMRead, "read", false},
		{LLMTooLarge, "too_large", true},
		{LLMDecode, "decode", true},
		{LLMNoChoices, "no_choices", true},
		{LLMInvalidVerdict, "invalid_verdict", true},
		{LLMCircuitOpen, "circuit_open", false},
	}
	for _, want := range wants {
		if got := want.kind.Token(); got != want.token {
			t.Errorf("kind %d Token() = %q, want %q", want.kind, got, want.token)
		}
		if got := want.kind.Permanent(); got != want.permanent {
			t.Errorf("kind %s Permanent() = %t, want %t", want.token, got, want.permanent)
		}
	}
}

func TestLLMProviderStatusTaxonomy(t *testing.T) {
	wants := map[LLMProviderStatus]string{
		LLMProviderNone:               "none",
		LLMProviderInvalidArgument:    "invalid_argument",
		LLMProviderUnauthenticated:    "unauthenticated",
		LLMProviderPermissionDenied:   "permission_denied",
		LLMProviderNotFound:           "not_found",
		LLMProviderResourceExhausted:  "resource_exhausted",
		LLMProviderFailedPrecondition: "failed_precondition",
		LLMProviderAborted:            "aborted",
		LLMProviderDeadlineExceeded:   "deadline_exceeded",
		LLMProviderUnavailable:        "unavailable",
		LLMProviderInternal:           "internal",
		LLMProviderUnimplemented:      "unimplemented",
		LLMProviderPolicyBlocked:      "policy_blocked",
		LLMProviderUnknown:            "unknown",
	}
	for status, want := range wants {
		if got := status.Token(); got != want {
			t.Errorf("provider status %d Token() = %q, want %q", status, got, want)
		}
	}
	if LLMProviderStatus(255).Token() != "" {
		t.Fatal("out-of-range provider status was accepted")
	}
	for _, info := range []LLMErrorInfo{
		{Kind: LLMBadRequest, HTTPStatus: 400, ProviderStatus: LLMProviderInvalidArgument},
		{Kind: LLMHTTPStatus, HTTPStatus: 100, ProviderStatus: LLMProviderUnknown},
		{Kind: LLMServer, HTTPStatus: 599, ProviderStatus: LLMProviderInternal},
		{Kind: LLMDecode, HTTPStatus: 200, ProviderStatus: LLMProviderNone},
		{Kind: LLMTransport, ProviderStatus: LLMProviderNone},
	} {
		if !info.Valid() {
			t.Errorf("valid info rejected: %+v", info)
		}
	}
	for _, info := range []LLMErrorInfo{
		{},
		{Kind: LLMBadRequest, HTTPStatus: 99},
		{Kind: LLMBadRequest, HTTPStatus: 600},
		{Kind: LLMBadRequest, ProviderStatus: 255},
		{Kind: LLMBadRequest, HTTPStatus: 401, ProviderStatus: LLMProviderUnknown},
		{Kind: LLMDecode, HTTPStatus: 0, ProviderStatus: LLMProviderNone},
		{Kind: LLMTransport, HTTPStatus: 200, ProviderStatus: LLMProviderNone},
	} {
		if info.Valid() {
			t.Errorf("invalid info accepted: %+v", info)
		}
	}
}

func TestCollectorAggregatesDistinctSafeLLMDetails(t *testing.T) {
	ctx, collector := WithCollector(context.Background())
	badRequest := LLMErrorInfo{
		Kind: LLMBadRequest, HTTPStatus: 400, ProviderStatus: LLMProviderInvalidArgument,
	}
	circuit := LLMErrorInfo{
		Kind: LLMCircuitOpen, HTTPStatus: 400, ProviderStatus: LLMProviderInvalidArgument,
	}
	RecordLLMFailure(ctx, badRequest)
	RecordLLMFailure(ctx, badRequest)
	RecordLLMFailure(ctx, circuit)
	RecordLLMFailure(ctx, LLMErrorInfo{Kind: LLMDecode}) // invalid: missing HTTP 200

	got := collector.Snapshot().LLMFailures()
	want := []LLMFailureCount{
		{LLMErrorInfo: badRequest, Count: 2},
		{LLMErrorInfo: circuit, Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("LLMFailures() = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("LLMFailures()[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
	got[0].Count = 99
	if collector.Snapshot().LLMFailures()[0].Count != 2 {
		t.Fatal("mutating returned failures changed collector snapshot")
	}
}

func TestLLMFailureAggregationIsRaceSafeAndBounded(t *testing.T) {
	ctx, collector := WithCollector(context.Background())
	collector.llmFailures[0] = LLMFailureCount{
		LLMErrorInfo: LLMErrorInfo{Kind: LLMDecode, HTTPStatus: 200}, Count: maxCount,
	}
	RecordLLMFailure(ctx, LLMErrorInfo{Kind: LLMDecode, HTTPStatus: 200})
	if got := collector.Snapshot().LLMFailures()[0].Count; got != maxCount {
		t.Fatalf("bounded decode count = %d, want %d", got, maxCount)
	}

	const workers = 20
	const events = 100
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range events {
				RecordLLMFailure(ctx, LLMErrorInfo{Kind: LLMTransport})
			}
		}()
	}
	wg.Wait()
	counts := collector.Snapshot().LLMFailures()
	if got := counts[1]; got != (LLMFailureCount{
		LLMErrorInfo: LLMErrorInfo{Kind: LLMTransport}, Count: workers * events,
	}) {
		t.Fatalf("transport count = %+v", got)
	}
}

func TestCollectorAggregatesOnlyValidBoundedEvents(t *testing.T) {
	ctx, c := WithCollector(context.Background())
	Cap(ctx, -1, 10)
	Cap(ctx, 1, -1)
	Cap(ctx, 10, 1)
	Cap(ctx, 5, 10)
	Retry(ctx, RetryServer, 1, 4, time.Second)
	Retry(ctx, RetryKind(255), 1, 4, time.Second)
	Retry(ctx, RetryPage, 5, 4, time.Second)

	if got, want := c.Snapshot(), (Snapshot{Retries: 1, Caps: 1}); got != want {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
}
