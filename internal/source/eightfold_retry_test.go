package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/diagnostic"
)

func TestEightfoldConflictingDuplicateForcesTwoFreshStableSnapshots(t *testing.T) {
	var traversal atomic.Int32
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		if start == 0 {
			traversal.Add(1)
		}
		switch traversal.Load() {
		case 1:
			if start == 0 {
				positions := make([]eightfoldPosition, 10)
				for index := range positions {
					positions[index] = eightfoldRetryPosition(int64(index+1), fmt.Sprintf("Engineer %d", index+1))
				}
				eightfoldWriteSearch(t, w, 11, positions)
				return
			}
			// Same stable ID, different record: this attempt is discarded.
			eightfoldWriteSearch(t, w, 11, []eightfoldPosition{eightfoldRetryPosition(1, "Changed title")})
		default:
			eightfoldWriteSearch(t, w, 1, []eightfoldPosition{eightfoldRetryPosition(99, "Stable engineer")})
		}
	}))
	defer server.Close()

	src := eightfoldRetrySource(server)
	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err := src.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "eightfold/acme.com/99" {
		t.Fatalf("jobs = %+v; partial/conflicting attempt must not be merged", jobs)
	}
	if calls.Load() != 4 || traversal.Load() != 3 {
		t.Fatalf("calls/traversals = %d/%d, want 4/3", calls.Load(), traversal.Load())
	}
	if got := collector.Snapshot().Retries; got != 2 {
		t.Fatalf("retry diagnostics = %d, want 2", got)
	}
}

func TestEightfoldExactDuplicateRequiresIdenticalTraversalBeforeDedup(t *testing.T) {
	var calls atomic.Int32
	position := eightfoldRetryPosition(42, "Platform engineer")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		eightfoldWriteSearch(t, w, 2, []eightfoldPosition{position, position})
	}))
	defer server.Close()

	src := eightfoldRetrySource(server)
	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err := src.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "eightfold/acme.com/42" {
		t.Fatalf("jobs = %+v, want one safely deduplicated posting", jobs)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want two identical traversals", calls.Load())
	}
	if got := collector.Snapshot().Retries; got != 1 {
		t.Fatalf("retry diagnostics = %d, want 1", got)
	}
}

func TestEightfoldUnknownFieldDifferenceMakesDuplicateConflicting(t *testing.T) {
	// Adjacent integers above 2^53 ensure canonicalization cannot pass through
	// float64 without collapsing two genuinely different raw records.
	const (
		id    = int64(9007199254740991)
		first = `{"id":9007199254740991,"name":"Platform engineer","locations":["India"],` +
			`"postedTs":1785237164,"positionUrl":"/careers/job/9007199254740991",` +
			`"creationTs":9007199254740992}`
		second = `{"id":9007199254740991,"name":"Platform engineer","locations":["India"],` +
			`"postedTs":1785237164,"positionUrl":"/careers/job/9007199254740991",` +
			`"creationTs":9007199254740993}`
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		eightfoldWriteRawSearch(t, w, 2, first, second)
	}))
	defer server.Close()

	_, err := eightfoldRetrySource(server).fetchSnapshot(context.Background())
	var retryable *eightfoldRetryableError
	if !errors.As(err, &retryable) || retryable.kind != diagnostic.RetrySnapshot ||
		!strings.Contains(err.Error(), "conflicting duplicate position id "+strconv.FormatInt(id, 10)) {
		t.Fatalf("fetchSnapshot error = %v (%T), want unknown-field conflict", err, err)
	}
}

func TestEightfoldCanonicalDuplicateIgnoresObjectKeyOrder(t *testing.T) {
	const (
		first = `{"id":42,"name":"Platform engineer","locations":["India"],` +
			`"positionUrl":"/careers/job/42","creationTs":9007199254740993}`
		second = `{"creationTs":9007199254740993,"positionUrl":"/careers/job/42",` +
			`"locations":["India"],"name":"Platform engineer","id":42}`
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		eightfoldWriteRawSearch(t, w, 2, first, second)
	}))
	defer server.Close()

	snapshot, err := eightfoldRetrySource(server).fetchSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.hadExactDuplicate || len(snapshot.jobs) != 1 ||
		snapshot.jobs[0].ID != "eightfold/acme.com/42" {
		t.Fatalf("snapshot = %+v, want one canonical exact duplicate", snapshot)
	}
}

func TestEightfoldUnknownFieldChangesWholeSnapshotFingerprint(t *testing.T) {
	var traversal atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") == "0" {
			traversal.Add(1)
		}
		score := 1
		if traversal.Load() >= 2 {
			score = 2
		}
		position := fmt.Sprintf(
			`{"id":42,"name":"Platform engineer","locations":["India"],`+
				`"positionUrl":"/careers/job/42","solrScore":%d}`,
			score,
		)
		eightfoldWriteRawSearch(t, w, 2, position, position)
	}))
	defer server.Close()

	jobs, err := eightfoldRetrySource(server).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if traversal.Load() != 3 || len(jobs) != 1 || jobs[0].ID != "eightfold/acme.com/42" {
		t.Fatalf("traversals/jobs = %d/%+v, want changed unknown field to require a third traversal", traversal.Load(), jobs)
	}
}

func TestEightfoldChangedSnapshotNeedsTwoConsecutiveMatches(t *testing.T) {
	var traversal atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") == "0" {
			traversal.Add(1)
		}
		if traversal.Load() == 1 {
			position := eightfoldRetryPosition(1, "First snapshot")
			eightfoldWriteSearch(t, w, 2, []eightfoldPosition{position, position})
			return
		}
		eightfoldWriteSearch(t, w, 1, []eightfoldPosition{eightfoldRetryPosition(2, "Replacement snapshot")})
	}))
	defer server.Close()

	jobs, err := eightfoldRetrySource(server).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if traversal.Load() != 3 || len(jobs) != 1 || !strings.HasSuffix(jobs[0].ID, "/2") {
		t.Fatalf("traversals/jobs = %d/%+v, want three traversals ending in snapshot 2", traversal.Load(), jobs)
	}
}

func TestEightfoldPermanentHTTPAndSchemaFailuresAreImmediate(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: "unauthorized", wantErr: "401 Unauthorized"},
		{name: "forbidden", status: http.StatusForbidden, body: "forbidden", wantErr: "403 Forbidden"},
		{name: "not found", status: http.StatusNotFound, body: "missing", wantErr: "404 Not Found"},
		{name: "malformed JSON", status: http.StatusOK, body: `{`, wantErr: "decoding response"},
		{name: "schema field missing", status: http.StatusOK, body: `{"status":200,"data":{"positions":[]}}`, wantErr: "omitted count or positions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			jobs, err := eightfoldRetrySource(server).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) || jobs != nil {
				t.Fatalf("Fetch = %+v, %v; want immediate %q error", jobs, err, test.wantErr)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestEightfoldPaginationDriftIsTypedRetryable(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "count changes",
			handler: func(w http.ResponseWriter, r *http.Request) {
				start, _ := strconv.Atoi(r.URL.Query().Get("start"))
				if start == 0 {
					positions := make([]eightfoldPosition, 10)
					for index := range positions {
						positions[index] = eightfoldRetryPosition(int64(index+1), "Engineer")
					}
					eightfoldWriteSearch(t, w, 11, positions)
					return
				}
				eightfoldWriteSearch(t, w, 10, []eightfoldPosition{eightfoldRetryPosition(11, "Engineer")})
			},
			wantErr: "count changed",
		},
		{
			name: "premature empty page",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				eightfoldWriteSearch(t, w, 1, []eightfoldPosition{})
			},
			wantErr: "empty page",
		},
		{
			name: "short page",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				eightfoldWriteSearch(t, w, 11, []eightfoldPosition{eightfoldRetryPosition(1, "Engineer")})
			},
			wantErr: "short page",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			_, err := eightfoldRetrySource(server).fetchSnapshot(context.Background())
			var retryable *eightfoldRetryableError
			if !errors.As(err, &retryable) || retryable.kind != diagnostic.RetrySnapshot ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("fetchSnapshot error = %v (%T), want RetrySnapshot containing %q", err, err, test.wantErr)
			}
		})
	}
}

func TestEightfoldTransportFailureIsTypedWithoutStringMatching(t *testing.T) {
	sentinel := errors.New("synthetic dial failure")
	src := &eightfold{
		company: "Acme", host: "test.eightfold", domain: "acme.com",
		base: "https://test.eightfold", keyPrefix: "eightfold/acme.com/", maxPostings: 100,
		client: &http.Client{Transport: customARoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, sentinel
		})},
	}
	_, err := src.fetchSnapshot(context.Background())
	var retryable *eightfoldRetryableError
	if !errors.As(err, &retryable) || retryable.kind != diagnostic.RetryTransport || !errors.Is(err, sentinel) {
		t.Fatalf("fetchSnapshot error = %v (%T), want typed transport error", err, err)
	}
}

func TestEightfoldTransientHTTPFailureRequiresTwoStableSnapshots(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		eightfoldWriteSearch(t, w, 1, []eightfoldPosition{eightfoldRetryPosition(7, "Recovered")})
	}))
	defer server.Close()

	jobs, err := eightfoldRetrySource(server).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(jobs) != 1 || !strings.HasSuffix(jobs[0].ID, "/7") {
		t.Fatalf("calls/jobs = %d/%+v, want failure plus two stable snapshots", calls.Load(), jobs)
	}
}

func TestEightfoldRetryWaitIsContextCancellable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "temporary", http.StatusBadGateway)
	}))
	defer server.Close()

	src := eightfoldRetrySource(server)
	src.retryGap = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	jobs, err := src.Fetch(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || jobs != nil {
		t.Fatalf("Fetch = %+v, %v; want context deadline", jobs, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 before cancelled backoff", calls.Load())
	}
}

func TestEightfoldRetryAfterParsingIsBounded(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"5", 5 * time.Second},
		{"600", eightfoldMaxRetryAfter},
		{"9223372036854775807", eightfoldMaxRetryAfter},
		{now.Add(20 * time.Second).Format(http.TimeFormat), 20 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"invalid", 0},
	} {
		if got := eightfoldRetryAfter(test.value, now); got != test.want {
			t.Errorf("eightfoldRetryAfter(%q) = %s, want %s", test.value, got, test.want)
		}
	}
	src := &eightfold{retryGap: 250 * time.Millisecond}
	if got := src.retryDelay(1, 5*time.Second); got != 5*time.Second {
		t.Fatalf("retryDelay ignored Retry-After: got %s want 5s", got)
	}
}

func eightfoldRetrySource(server *httptest.Server) *eightfold {
	return &eightfold{
		company: "Acme", host: "test.eightfold", domain: "acme.com",
		base: server.URL, keyPrefix: "eightfold/acme.com/", maxPostings: 100,
		client: server.Client(),
	}
}

func eightfoldRetryPosition(id int64, name string) eightfoldPosition {
	return eightfoldPosition{
		ID: id, Name: name, Locations: []string{"India"},
		PostedTS: 1_785_237_164, PositionURL: "/careers/job/" + strconv.FormatInt(id, 10),
	}
}

func eightfoldWriteSearch(t *testing.T, w http.ResponseWriter, count int, positions []eightfoldPosition) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"status": 200,
		"data":   map[string]any{"count": count, "positions": positions},
	}); err != nil {
		t.Error(err)
	}
}

func eightfoldWriteRawSearch(t *testing.T, w http.ResponseWriter, count int, positions ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(
		w, `{"status":200,"data":{"count":%d,"positions":[%s]}}`,
		count, strings.Join(positions, ","),
	)
}
