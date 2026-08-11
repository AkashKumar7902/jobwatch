package run

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

const maxLogFieldRunes = 120

// FETCH/BOARD/WARN/POLL/RUN are a public protocol consumed by the Actions
// summary; keep their field order and closed vocabularies in sync with
// scripts/run_summary.py.
type boardOutcome struct {
	ordinal int
	src     source.Source

	emitted         bool
	fetched         bool
	processed       bool
	problem         string
	open            int
	new             int
	matched         int
	deferred        int
	detailFailed    int
	retries         int
	fetchDuration   time.Duration
	processStart    time.Time
	processDuration time.Duration
	fetchErr        error
	diagnostics     *diagnostic.Collector
}

var (
	errPersistence = errors.New("run persistence failure")
	errRunNotify   = errors.New("run notification failure")
	errRunReport   = errors.New("run report failure")
	errRunMatch    = errors.New("run matcher failure")
	errRunSeed     = errors.New("run seed failure")
	errRunFetch    = errors.New("run fetch failure")
)

func markRunFailure(marker, err error) error {
	if err == nil || errors.Is(err, marker) {
		return err
	}
	return errors.Join(marker, err)
}

func (r *Runner) finishRun(ctx context.Context, outcomes []boardOutcome, started time.Time, persistenceStart store.Persistence, dryRun bool, runErr error) {
	counts := map[string]int{}
	open, newJobs, matched, deferred := 0, 0, 0, 0
	for i := range outcomes {
		b := &outcomes[i]
		if !b.processed {
			if !b.processStart.IsZero() && b.processDuration == 0 {
				b.processDuration = time.Since(b.processStart).Round(time.Millisecond)
			}
			if !b.fetched {
				b.problem = "not_run"
			} else if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				b.problem = "cancelled"
			} else {
				b.problem = "not_run"
			}
		}
		r.emitBoard(ctx, b)
		counts[b.status()]++
		open += b.open
		newJobs += b.new
		matched += b.matched
		deferred += b.deferred
	}
	r.Log.Printf("POLL boards=%d ok=%d recovered=%d capped=%d degraded=%d partial=%d failed=%d open=%d new=%d matched=%d deferred=%d",
		len(outcomes), counts["ok"], counts["recovered"], counts["capped"], counts["degraded"], counts["partial"], counts["failed"],
		nonnegative(open), nonnegative(newJobs), nonnegative(matched), nonnegative(deferred))

	status, code := "ok", classifyRunError(ctx, runErr)
	switch code {
	case "cancelled":
		status = "cancelled"
	case "none":
		if counts["failed"]+counts["partial"]+counts["degraded"]+counts["capped"]+counts["recovered"] > 0 {
			status = "degraded"
		}
	default:
		status = "failed"
	}
	persistenceEnd := r.Store.Persistence()
	cycleSaved := persistenceEnd.SuccessfulSaves > persistenceStart.SuccessfulSaves
	snapshotSaved := persistenceEnd.Revision == persistenceEnd.SavedRevision
	localState := "not_saved"
	if dryRun {
		localState = "not_applicable"
	} else if cycleSaved && snapshotSaved {
		localState = "saved"
	} else if cycleSaved {
		localState = "checkpointed"
	}
	if runErr != nil {
		r.Log.Printf("WARN scope=run index=0 step=terminal code=%s count=1", safeToken(code))
	}
	r.Log.Printf("RUN status=%s local_state=%s code=%s duration_ms=%d boards=%d",
		status, localState, safeToken(code), millis(time.Since(started)), len(outcomes))
}

func (r *Runner) emitBoard(ctx context.Context, b *boardOutcome) {
	if b.emitted {
		return
	}
	r.Log.Print(b.logLine(ctx))
	if code := b.errorClass(ctx); code != "none" {
		step := "process"
		switch {
		case !b.fetched:
			step = "setup"
		case !b.processed:
			step = "process"
		case b.fetchErr != nil:
			step = "fetch"
		}
		r.Log.Printf("WARN scope=board index=%d step=%s code=%s count=1", b.ordinal, step, safeToken(code))
	}
	b.emitted = true
}

func (b *boardOutcome) finish() {
	if !b.processStart.IsZero() && b.processDuration == 0 {
		b.processDuration = time.Since(b.processStart).Round(time.Millisecond)
	}
	b.processed = true
}

func (b *boardOutcome) snapshot() diagnostic.Snapshot {
	if b.diagnostics == nil {
		return diagnostic.Snapshot{}
	}
	return b.diagnostics.Snapshot()
}

func (b *boardOutcome) status() string {
	d := b.snapshot()
	switch {
	case !b.processed:
		return "failed"
	case b.fetchErr != nil && b.open == 0:
		return "failed"
	case b.fetchErr != nil:
		return "partial"
	case b.detailFailed > 0 || b.deferred > 0:
		return "degraded"
	case d.Caps > 0:
		return "capped"
	case d.Retries+b.retries > 0:
		return "recovered"
	default:
		return "ok"
	}
}

func (b *boardOutcome) errorClass(ctx context.Context) string {
	if b.problem != "" {
		return b.problem
	}
	if b.fetchErr == nil {
		if b.detailFailed > 0 && b.deferred > 0 {
			return "detail_and_match"
		}
		if b.detailFailed > 0 {
			return "detail"
		}
		if b.deferred > 0 {
			return "match"
		}
		return "none"
	}
	return classifyFetchError(ctx, b.fetchErr)
}

func (b *boardOutcome) logLine(ctx context.Context) string {
	d := b.snapshot()
	return fmt.Sprintf(
		"BOARD index=%d adapter=%s company=%s status=%s open=%d new=%d matched=%d deferred=%d detail_failed=%d retries=%d caps=%d fetch_ms=%d process_ms=%d",
		b.ordinal, safeToken(source.Adapter(b.src)), quoteLogField(b.src.Company()), b.status(),
		nonnegative(b.open), nonnegative(b.new), nonnegative(b.matched),
		nonnegative(b.deferred), nonnegative(b.detailFailed), nonnegative(d.Retries+b.retries), nonnegative(d.Caps),
		millis(b.fetchDuration), millis(b.processDuration),
	)
}

func classifyRunError(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, errPersistence):
		return "persistence"
	case ctx != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return "cancelled"
	case errors.Is(err, errRunNotify):
		return "notify"
	case errors.Is(err, errRunReport):
		return "report"
	case errors.Is(err, errRunMatch):
		return "match"
	case errors.Is(err, errRunSeed):
		return "seed"
	case errors.Is(err, errRunFetch):
		return "fetch"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	default:
		return "unknown"
	}
}

func classifyFetchError(_ context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	// Only response and transport evidence maps into the board vocabulary.
	// Run ownership is carried by sentinels at Runner operation boundaries,
	// so hostile adapter text cannot claim persistence/notify/report/etc.
	s := strings.ToLower(err.Error())
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "transport"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "transport"
	}
	// Adapter response text is likewise used only for a closed classification.
	switch {
	case strings.Contains(s, "403 forbidden"):
		return "forbidden"
	case strings.Contains(s, "401 unauthorized"):
		return "unauthorized"
	case strings.Contains(s, "404 not found"):
		return "not_found"
	case strings.Contains(s, "429 too many requests"), strings.Contains(s, "rate limit"):
		return "rate_limited"
	case strings.Contains(s, "500 internal server error"), strings.Contains(s, "502 bad gateway"),
		strings.Contains(s, "503 service unavailable"), strings.Contains(s, "504 gateway timeout"):
		return "server"
	case strings.Contains(s, "duplicate"):
		return "duplicate"
	case strings.Contains(s, "missing"), strings.Contains(s, "omitted"):
		return "missing_field"
	case strings.Contains(s, "mismatch"), strings.Contains(s, "conflicting"), strings.Contains(s, "does not match"),
		strings.Contains(s, "does not identify"), strings.Contains(s, "disagrees"):
		return "mismatch"
	case strings.Contains(s, "did not stabilize"), strings.Contains(s, "no stable snapshot"),
		strings.Contains(s, "changed between consecutive"), strings.Contains(s, "changed between traversals"):
		return "unstable_snapshot"
	case strings.Contains(s, "changed from"), strings.Contains(s, "schema"):
		return "contract"
	case strings.Contains(s, "decode"), strings.Contains(s, "parsing"), strings.Contains(s, "invalid"):
		return "invalid_response"
	default:
		return "unknown"
	}
}

func quoteLogField(s string) string { return strconv.Quote(sanitizeLogField(s)) }

func sanitizeLogField(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var out strings.Builder
	count := 0
	for _, r := range s {
		if count == maxLogFieldRunes {
			out.WriteRune('…')
			break
		}
		if unicode.IsSpace(r) {
			r = ' '
		} else if !unicode.IsGraphic(r) || unicode.IsControl(r) || isDirectionalControl(r) || !utf8.ValidRune(r) {
			r = '�'
		}
		out.WriteRune(r)
		count++
	}
	return out.String()
}

func isDirectionalControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
}

func safeToken(s string) string {
	if s == "" {
		return "unknown"
	}
	var out strings.Builder
	for _, r := range s {
		if out.Len() == 48 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			out.WriteRune(r)
		}
	}
	if out.Len() == 0 {
		return "unknown"
	}
	return out.String()
}

func millis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	ms := d.Milliseconds()
	if ms > 86_400_000 {
		return 86_400_000
	}
	return ms
}

func nonnegative(n int) int {
	if n < 0 {
		return 0
	}
	if n > 1_000_000_000 {
		return 1_000_000_000
	}
	return n
}
