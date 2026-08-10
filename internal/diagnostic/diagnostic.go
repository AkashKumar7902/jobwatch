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

type collectorKey struct{}

// Collector is safe for the fetch goroutine and the later matcher calls that
// share its context. Callers can only add sealed numeric events through the
// functions below.
type Collector struct {
	mu      sync.Mutex
	retries int
	caps    int
}

// Snapshot is the bounded aggregate consumed by a BOARD outcome.
type Snapshot struct {
	Retries int
	Caps    int
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

// Snapshot returns an immutable aggregate.
func (c *Collector) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{Retries: c.retries, Caps: c.caps}
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
