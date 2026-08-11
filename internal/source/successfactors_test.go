package source

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/model"
)

type successFactorsFixtureJob struct {
	id    string
	title string
}

type successFactorsFixture struct {
	server *httptest.Server

	mu         sync.Mutex
	scripts    [][]successFactorsFixtureJob
	traversal  int
	requestCnt int
}

func newSuccessFactorsFixture(t *testing.T, scripts ...[]successFactorsFixtureJob) *successFactorsFixture {
	t.Helper()
	fixture := &successFactorsFixture{scripts: scripts, traversal: -1}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/" {
			http.NotFound(w, r)
			return
		}
		start, err := strconv.Atoi(r.URL.Query().Get("startrow"))
		if err != nil {
			http.Error(w, "invalid startrow", http.StatusBadRequest)
			return
		}

		fixture.mu.Lock()
		fixture.requestCnt++
		if start == 0 {
			fixture.traversal++
		}
		traversal := fixture.traversal
		if traversal < 0 || traversal >= len(fixture.scripts) {
			fixture.mu.Unlock()
			http.Error(w, "unexpected traversal", http.StatusInternalServerError)
			return
		}
		jobs := append([]successFactorsFixtureJob(nil), fixture.scripts[traversal]...)
		fixture.mu.Unlock()

		fmt.Fprint(w, successFactorsFixturePage(jobs, start))
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *successFactorsFixture) source(client *http.Client) *successFactors {
	if client == nil {
		client = f.server.Client()
	}
	return &successFactors{
		company: "Acme", host: "jobs.example.com", base: f.server.URL,
		maxPages: 10, client: client,
	}
}

func (f *successFactorsFixture) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestCnt
}

func successFactorsFixturePage(jobs []successFactorsFixtureJob, start int) string {
	const pageSize = 2
	total := len(jobs)
	pageCount := (total + pageSize - 1) / pageSize
	page := start/pageSize + 1
	end := min(start+pageSize, total)

	var body strings.Builder
	fmt.Fprintf(&body, `<table aria-label="Search results for . Page %d of %d, Results %d to %d of %d">`,
		page, pageCount, start+1, end, total)
	for _, job := range jobs[start:end] {
		fmt.Fprintf(&body, `<tr class="data-row"><td><a class="jobTitle-link" href="/job/Job-%s/%s/">%s</a><span class="jobLocation">Remote</span></td></tr>`,
			job.id, job.id, job.title)
	}
	body.WriteString(`</table>`)
	return body.String()
}

func successFactorsJobs(ids ...string) []successFactorsFixtureJob {
	jobs := make([]successFactorsFixtureJob, 0, len(ids))
	for _, id := range ids {
		jobs = append(jobs, successFactorsFixtureJob{id: id, title: "Job " + id})
	}
	return jobs
}

func successFactorsJobIDs(jobs []model.Job) map[string]struct{} {
	ids := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		ids[job.ID] = struct{}{}
	}
	return ids
}

func TestSuccessFactorsDuplicateTraversalThenTwoMatchingSnapshots(t *testing.T) {
	fixture := newSuccessFactorsFixture(t,
		successFactorsJobs("101", "102", "102"),
		successFactorsJobs("101", "102", "103"),
		successFactorsJobs("103", "101", "102"),
	)

	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err := fixture.source(nil).Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{}{
		"successfactors/jobs.example.com/101": {},
		"successfactors/jobs.example.com/102": {},
		"successfactors/jobs.example.com/103": {},
	}
	if got := successFactorsJobIDs(jobs); len(got) != len(want) {
		t.Fatalf("got IDs %v, want %v", got, want)
	} else {
		for id := range want {
			if _, ok := got[id]; !ok {
				t.Fatalf("got IDs %v, want %v", got, want)
			}
		}
	}
	if got := fixture.requests(); got != 6 {
		t.Fatalf("requests = %d, want 6 (one rejected traversal and two matching traversals)", got)
	}
	if got := collector.Snapshot().Retries; got != 1 {
		t.Fatalf("retry diagnostics = %d, want one snapshot-drift retry", got)
	}
}

func TestSuccessFactorsPaginationDriftThenTwoMatchingSnapshots(t *testing.T) {
	var (
		mu        sync.Mutex
		traversal = -1
		requests  int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, err := strconv.Atoi(r.URL.Query().Get("startrow"))
		if err != nil {
			http.Error(w, "invalid startrow", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests++
		if start == 0 {
			traversal++
		}
		current := traversal
		mu.Unlock()

		if current == 0 && start == 2 {
			fmt.Fprint(w, `<table aria-label="Search results for . Page 2 of 2, Results 3 to 4 of 4">`+
				`<tr class="data-row"><td><a class="jobTitle-link" href="/job/Job-103/103/">Job 103</a></td></tr>`+
				`<tr class="data-row"><td><a class="jobTitle-link" href="/job/Job-104/104/">Job 104</a></td></tr></table>`)
			return
		}
		fmt.Fprint(w, successFactorsFixturePage(successFactorsJobs("101", "102", "103"), start))
	}))
	defer server.Close()
	src := &successFactors{
		company: "Acme", host: "jobs.example.com", base: server.URL,
		maxPages: 10, client: server.Client(),
	}
	ctx, collector := diagnostic.WithCollector(context.Background())

	jobs, err := src.Fetch(ctx)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 6 {
		t.Fatalf("requests=%d, want one drifting and two matching traversals", gotRequests)
	}
	if got := collector.Snapshot().Retries; got != 1 {
		t.Fatalf("retry diagnostics=%d, want one snapshot-drift retry", got)
	}
}

func TestSuccessFactorsCleanConfirmationIsNotARecovery(t *testing.T) {
	fixture := newSuccessFactorsFixture(t,
		successFactorsJobs("101", "102", "103"),
		successFactorsJobs("103", "101", "102"),
	)
	ctx, collector := diagnostic.WithCollector(context.Background())

	jobs, err := fixture.source(nil).Fetch(ctx)
	if err != nil || len(jobs) != 3 || fixture.requests() != 4 {
		t.Fatalf("requests=%d jobs=%v err=%v", fixture.requests(), jobs, err)
	}
	if got := collector.Snapshot().Retries; got != 0 {
		t.Fatalf("mandatory clean confirmation recorded %d retries, want 0", got)
	}
}

func TestSuccessFactorsUsesFreshStickySessionForEachTraversal(t *testing.T) {
	var (
		mu            sync.Mutex
		sessionCount  int
		requestTrace  []string
		jobsBySession = map[string][]successFactorsFixtureJob{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, err := strconv.Atoi(r.URL.Query().Get("startrow"))
		if err != nil {
			http.Error(w, "invalid startrow", http.StatusBadRequest)
			return
		}

		mu.Lock()
		cookie, cookieErr := r.Cookie("JSESSIONID")
		if errors.Is(cookieErr, http.ErrNoCookie) {
			if start != 0 {
				mu.Unlock()
				http.Error(w, "later page omitted session cookie", http.StatusConflict)
				return
			}
			sessionCount++
			session := "snapshot-" + strconv.Itoa(sessionCount)
			cookie = &http.Cookie{Name: "JSESSIONID", Value: session, Path: "/"}
			http.SetCookie(w, cookie)
			requestTrace = append(requestTrace, "new:"+session)
			if sessionCount == 1 {
				jobsBySession[session] = successFactorsJobs("101", "102", "103")
			} else {
				jobsBySession[session] = successFactorsJobs("103", "101", "102")
			}
		} else if cookieErr != nil {
			mu.Unlock()
			http.Error(w, cookieErr.Error(), http.StatusBadRequest)
			return
		} else {
			requestTrace = append(requestTrace, cookie.Value)
		}
		jobs, ok := jobsBySession[cookie.Value]
		mu.Unlock()
		if !ok {
			http.Error(w, "unknown session", http.StatusConflict)
			return
		}
		fmt.Fprint(w, successFactorsFixturePage(jobs, start))
	}))
	defer server.Close()
	src := &successFactors{
		company: "Acme", host: "jobs.example.com", base: server.URL,
		maxPages: 10, client: server.Client(),
	}

	jobs, err := src.Fetch(context.Background())
	if err != nil || len(jobs) != 3 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	mu.Lock()
	gotTrace := strings.Join(requestTrace, ",")
	gotSessions := sessionCount
	mu.Unlock()
	if gotSessions != 2 || gotTrace != "new:snapshot-1,snapshot-1,new:snapshot-2,snapshot-2" {
		t.Fatalf("sessions=%d trace=%q, want two independent sticky traversals", gotSessions, gotTrace)
	}
}

func TestSuccessFactorsCoherentSnapshotsMustStabilize(t *testing.T) {
	fixture := newSuccessFactorsFixture(t,
		successFactorsJobs("101", "102", "103"),
		successFactorsJobs("201", "202", "203"),
		successFactorsJobs("203", "201", "202"),
	)

	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err := fixture.source(nil).Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := successFactorsJobIDs(jobs)
	if _, stale := ids["successfactors/jobs.example.com/101"]; stale {
		t.Fatalf("accepted the first coherent but unstable traversal: %v", ids)
	}
	for _, id := range []string{"201", "202", "203"} {
		if _, ok := ids["successfactors/jobs.example.com/"+id]; !ok {
			t.Fatalf("stabilized IDs = %v, missing %s", ids, id)
		}
	}
	if got := fixture.requests(); got != 6 {
		t.Fatalf("requests = %d, want 6", got)
	}
	if got := collector.Snapshot().Retries; got != 1 {
		t.Fatalf("retry diagnostics = %d, want one changed-snapshot retry", got)
	}
}

func TestSuccessFactorsRejectsThreeChangingSnapshots(t *testing.T) {
	fixture := newSuccessFactorsFixture(t,
		successFactorsJobs("101", "102", "103"),
		successFactorsJobs("201", "202", "203"),
		successFactorsJobs("301", "302", "303"),
	)

	jobs, err := fixture.source(nil).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "snapshot did not stabilize after 3 attempts") {
		t.Fatalf("got jobs=%v err=%v, want stabilization error", jobs, err)
	}
	if got := fixture.requests(); got != 6 {
		t.Fatalf("requests = %d, want 6", got)
	}
}

func TestSuccessFactorsStructuralRowFailureIsNotRetried(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `<table aria-label="Search results for . Page 1 of 1, Results 1 to 1 of 1"><tr class="data-row"><td>missing link</td></tr></table>`)
	}))
	defer server.Close()
	src := &successFactors{
		company: "Acme", host: "jobs.example.com", base: server.URL,
		maxPages: 10, client: server.Client(),
	}

	jobs, err := src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing job link") {
		t.Fatalf("got jobs=%v err=%v, want missing-link error", jobs, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want one fatal structural attempt", got)
	}
}

type successFactorsRoundTripper func(*http.Request) (*http.Response, error)

func (f successFactorsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSuccessFactorsRetriesTransientTransportThenStabilizes(t *testing.T) {
	fixture := newSuccessFactorsFixture(t,
		successFactorsJobs("101", "102", "103"),
		successFactorsJobs("103", "101", "102"),
	)
	baseClient := fixture.server.Client()
	baseTransport := baseClient.Transport
	var requests atomic.Int32
	client := *baseClient
	client.Transport = successFactorsRoundTripper(func(req *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return nil, &net.DNSError{Err: "temporary timeout", IsTimeout: true, IsTemporary: true}
		}
		return baseTransport.RoundTrip(req)
	})

	ctx, collector := diagnostic.WithCollector(context.Background())
	jobs, err := fixture.source(&client).Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || requests.Load() != 5 || fixture.requests() != 4 {
		t.Fatalf("jobs=%d transport requests=%d server requests=%d", len(jobs), requests.Load(), fixture.requests())
	}
	if got := collector.Snapshot().Retries; got != 1 {
		t.Fatalf("retry diagnostics = %d, want one transport retry", got)
	}
}

func TestSuccessFactorsRetryBackoffHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := waitSuccessFactorsSnapshot(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled retry wait took %s", elapsed)
	}
}
