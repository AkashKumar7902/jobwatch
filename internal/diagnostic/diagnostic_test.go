package diagnostic

import (
	"context"
	"testing"
	"time"
)

func TestMissingCollectorIsNoOp(t *testing.T) {
	Cap(context.Background(), 5, 10)
	Retry(context.Background(), RetryServer, 1, 4, time.Second)
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
