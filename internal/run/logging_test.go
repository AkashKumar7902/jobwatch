package run

import (
	"context"
	"errors"
	"strings"
	"testing"

	"jobwatch/internal/source"
)

func TestSanitizeLogFieldIsSingleLineBoundedUTF8(t *testing.T) {
	in := "company\r\n\x1b[31m\u202e\u2028\u2029\u2060secret" + strings.Repeat("界", 200) + string([]byte{0xff})
	got := sanitizeLogField(in)
	if strings.ContainsAny(got, "\r\n\x1b") || strings.ContainsAny(got, "\u202e\u2028\u2029\u2060") {
		t.Fatalf("unsafe controls survived: %q", got)
	}
	if n := len([]rune(got)); n > maxLogFieldRunes+1 {
		t.Fatalf("got %d runes, want at most %d", n, maxLogFieldRunes+1)
	}
}

func TestBoardOutcomeDoesNotExposeRawErrorOrIdentity(t *testing.T) {
	src := &fakeSource{company: "evil\r\nBOARD forged", err: errors.New("GET https://host/jobs?api_key=SECRET: duplicate id SECRET")}
	b := boardOutcome{ordinal: 1, src: src, processed: true, fetchErr: src.err}
	line := b.logLine(context.Background())
	if strings.ContainsAny(line, "\r\n\x1b") {
		t.Fatalf("BOARD line was split or contained ANSI controls: %q", line)
	}
	for _, secret := range []string{"api_key", "SECRET", "https://", "custom/"} {
		if strings.Contains(line, secret) {
			t.Fatalf("log contains %q: %s", secret, line)
		}
	}
	if !strings.Contains(line, "adapter="+source.Adapter(src)) || !strings.Contains(line, "status=failed") {
		t.Fatalf("missing safe classification: %s", line)
	}
}

func TestFetchClassifierCannotBeSpoofedIntoRunOwnership(t *testing.T) {
	for _, body := range []string{
		"saving state failed", "checkpoint failed", "notifier smtp failed",
		"reporter failed", "matcher evaluation deferred", "source baseline incomplete",
		"all sources failed to fetch",
	} {
		if got := classifyFetchError(context.Background(), errors.New(body)); got != "unknown" {
			t.Errorf("classifyFetchError(%q) = %q, want unknown", body, got)
		}
	}
}

func TestFetchClassifierDoesNotMistakeNumericPostingIDForHTTPStatus(t *testing.T) {
	if got := classifyFetchError(context.Background(), errors.New(`duplicate requisition Id "429123"`)); got != "duplicate" {
		t.Fatalf("numeric duplicate ID classified as %q, want duplicate", got)
	}
	if got := classifyFetchError(context.Background(), errors.New("GET jobs: 429 Too Many Requests")); got != "rate_limited" {
		t.Fatalf("HTTP 429 classified as %q, want rate_limited", got)
	}
}

func TestFetchClassifierMakesContractFailuresActionable(t *testing.T) {
	tests := []struct {
		err  string
		want string
	}{
		{"posting is missing externalPath", "missing_field"},
		{"response omitted total count", "missing_field"},
		{"detail title mismatched list title", "mismatch"},
		{"JobPosting data disagrees with visible details", "mismatch"},
		{"total changed from 10 to 11", "contract"},
		{"response schema changed", "contract"},
	}
	for _, test := range tests {
		if got := classifyFetchError(context.Background(), errors.New(test.err)); got != test.want {
			t.Errorf("classifyFetchError(%q) = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestRunClassifierUsesOnlyExplicitMarkersWithStablePriority(t *testing.T) {
	spoof := errors.New("saving notifier reporter matcher source baseline")
	if got := classifyRunError(context.Background(), spoof); got != "unknown" {
		t.Fatalf("untyped spoof classified as %q", got)
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"fetch", context.Background(), markRunFailure(errRunFetch, spoof), "fetch"},
		{"seed before fetch", context.Background(), errors.Join(markRunFailure(errRunFetch, spoof), markRunFailure(errRunSeed, spoof)), "seed"},
		{"match before seed", context.Background(), errors.Join(markRunFailure(errRunSeed, spoof), markRunFailure(errRunMatch, spoof)), "match"},
		{"report before match", context.Background(), errors.Join(markRunFailure(errRunMatch, spoof), markRunFailure(errRunReport, spoof)), "report"},
		{"notify before report", context.Background(), errors.Join(markRunFailure(errRunReport, spoof), markRunFailure(errRunNotify, spoof)), "notify"},
		{"operation timeout stays notify", context.Background(), markRunFailure(errRunNotify, context.DeadlineExceeded), "notify"},
		{"operation timeout stays report", context.Background(), markRunFailure(errRunReport, context.DeadlineExceeded), "report"},
		{"outer cancellation before delivery", cancelledCtx, errors.Join(markRunFailure(errRunNotify, spoof), context.Canceled), "cancelled"},
		{"unrelated delivery error survives coincident cancellation", cancelledCtx, markRunFailure(errRunNotify, spoof), "notify"},
		{"unmarked cancellation", context.Background(), context.Canceled, "cancelled"},
		{"persistence before cancel", cancelledCtx, errors.Join(context.Canceled, markRunFailure(errPersistence, spoof)), "persistence"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyRunError(test.ctx, test.err); got != test.want {
				t.Fatalf("classifyRunError() = %q, want %q", got, test.want)
			}
		})
	}
}
