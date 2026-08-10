package run

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jobwatch/internal/diagnostic"
	"jobwatch/internal/match"
	"jobwatch/internal/model"
	"jobwatch/internal/notify"
	"jobwatch/internal/source"
	"jobwatch/internal/store"
)

type diagnosticSource struct {
	*fakeSource
}

type capOnlySource struct {
	*fakeSource
}

func (s *capOnlySource) Fetch(ctx context.Context) ([]model.Job, error) {
	diagnostic.Cap(ctx, len(s.jobs), len(s.jobs)+1)
	return s.jobs, s.err
}

type blockingFetchSource struct {
	*fakeSource
	started chan struct{}
	release chan struct{}
}

type delayedFetchSource struct {
	*fakeSource
	delay time.Duration
}

func (s *delayedFetchSource) Fetch(ctx context.Context) ([]model.Job, error) {
	select {
	case <-time.After(s.delay):
		return s.jobs, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *blockingFetchSource) Fetch(ctx context.Context) ([]model.Job, error) {
	close(s.started)
	select {
	case <-s.release:
		return s.jobs, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type synchronizedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *synchronizedLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *synchronizedLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (s *diagnosticSource) Fetch(ctx context.Context) ([]model.Job, error) {
	diagnostic.Retry(ctx, diagnostic.RetryPage, 1, 3, time.Millisecond)
	diagnostic.Cap(ctx, len(s.jobs), len(s.jobs)+1)
	return s.jobs, s.err
}

type callbackNotifier struct {
	callback func()
}

type deadlineNotifier struct{}

func (deadlineNotifier) Name() string { return "deadline" }
func (deadlineNotifier) Notify(context.Context, []notify.Match) error {
	return context.DeadlineExceeded
}

type cancellingErrorNotifier struct {
	cancel context.CancelFunc
}

func (n cancellingErrorNotifier) Name() string { return "cancel-then-fail" }
func (n cancellingErrorNotifier) Notify(context.Context, []notify.Match) error {
	n.cancel()
	return errors.New("smtp connection closed")
}

type callbackReporter struct {
	callback func()
}

func (n callbackReporter) Name() string                                 { return "callback_reporter" }
func (n callbackReporter) Notify(context.Context, []notify.Match) error { return nil }
func (n callbackReporter) Report(context.Context, notify.Report) error {
	n.callback()
	return nil
}

func (n callbackNotifier) Name() string { return "callback" }
func (n callbackNotifier) Notify(context.Context, []notify.Match) error {
	n.callback()
	return nil
}

func openLoggingStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, path
}

func blockStateSave(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func adoptLoggingSource(t *testing.T, st *store.Store, src *fakeSource) {
	t.Helper()
	identity := source.Identity(src)
	st.Add(sourceRegistryV2ID, store.Record{Title: "exact source registry v2"})
	st.Add(sourceMarkerPrefix+identity, store.Record{
		Title:  "source: " + src.Company(),
		Marker: &store.Marker{StatePrefix: source.StatePrefix(src)},
	})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationalLogHasOneOrderedOutcomePerBoard(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs, human strings.Builder
	sources := []source.Source{
		&fakeSource{company: "Okay", identity: "test/ok", statePrefix: "test/ok/", jobs: []model.Job{{ID: "test/ok/1", Company: "Okay", Title: "Role", Description: "BODY_SENTINEL"}}},
		&fakeSource{company: "Partial", identity: "test/partial", statePrefix: "test/partial/", jobs: []model.Job{{ID: "test/partial/1", Company: "Partial", Title: "Role"}}, err: errors.New("schema omitted SECRET_QUERY")},
		&fakeSource{company: "Failed", identity: "test/failed", statePrefix: "test/failed/", err: errors.New("GET https://host/jobs?token=SECRET_QUERY: duplicate id")},
	}
	r := &Runner{
		Sources: sources,
		Matcher: matcherFunc{name: "private", fn: func(_ context.Context, job model.Job) (match.Result, error) {
			return match.Result{Matched: job.Company == "Okay", Reason: "PROFILE_SENTINEL"}, nil
		}},
		Notifiers:   []notify.Notifier{&flakyNotifier{}},
		Store:       st,
		Log:         log.New(&logs, "", 0),
		Errors:      &human,
		Concurrency: 3,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := logs.String()
	if strings.Count(got, "BOARD index=") != len(sources) {
		t.Fatalf("BOARD count mismatch:\n%s", got)
	}
	for _, want := range []string{
		`BOARD index=1 adapter=custom company="Okay" status=ok`,
		`BOARD index=2 adapter=custom company="Partial" status=partial`,
		`BOARD index=3 adapter=custom company="Failed" status=failed`,
		`WARN scope=board index=2 step=fetch code=contract count=1`,
		`WARN scope=board index=3 step=fetch code=duplicate count=1`,
		`RUN status=degraded local_state=saved code=none`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if first, second, third := strings.Index(got, "BOARD index=1"), strings.Index(got, "BOARD index=2"), strings.Index(got, "BOARD index=3"); !(first < second && second < third) {
		t.Fatalf("outcomes are not config ordered:\n%s", got)
	}
	for _, forbidden := range []string{"MATCH company=", "NO_MATCH", "PROFILE_SENTINEL", "BODY_SENTINEL", "SECRET_QUERY", "https://"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("log exposed %q:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(human.String(), "schema omitted SECRET_QUERY") ||
		!strings.Contains(human.String(), "https://host/jobs?token=SECRET_QUERY") {
		t.Fatalf("local output lost mixed-board fetch details: %q", human.String())
	}
	if !strings.Contains(got, `BOARD index=1 adapter=custom company="Okay" status=ok open=1 new=1 matched=1`) {
		t.Fatalf("board match count is missing:\n%s", got)
	}
	if last := strings.TrimSpace(got); !strings.HasSuffix(last, "boards=3") || !strings.Contains(last[strings.LastIndex(last, "\n")+1:], "RUN status=") {
		t.Fatalf("RUN is not the terminal line:\n%s", got)
	}
}

func TestHostileFetchTextCannotSpoofRunOwnership(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Spoof", identity: "test/spoof", statePrefix: "test/spoof/",
			err: errors.New("saving notifier reporter matcher source baseline failed"),
		}},
		Matcher: matchAll{}, Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(&logs, "", 0), Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("all-failed run should return an error")
	}
	got := logs.String()
	if !strings.Contains(got, "WARN scope=board index=1 step=fetch code=unknown count=1") ||
		!strings.Contains(got, "RUN status=failed local_state=saved code=fetch") {
		t.Fatalf("fetch/run ownership was misclassified:\n%s", got)
	}
	for _, forbidden := range []string{"code=persistence", "code=notify", "code=report", "code=match", "code=seed"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("hostile fetch text forged %q:\n%s", forbidden, got)
		}
	}
}

func TestCompletedBoardIsLoggedBeforeLaterBoardFinishes(t *testing.T) {
	st, _ := openLoggingStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	logs := &synchronizedLog{}
	jobs := []model.Job{
		{ID: "test/one/1", Company: "One", Title: "Role"},
		{ID: "test/two/1", Company: "Two", Title: "Role"},
	}
	r := &Runner{
		Sources: []source.Source{
			&fakeSource{company: "One", identity: "test/one", statePrefix: "test/one/", jobs: jobs[:1]},
			&fakeSource{company: "Two", identity: "test/two", statePrefix: "test/two/", jobs: jobs[1:]},
		},
		Matcher: matcherFunc{name: "blocking", fn: func(_ context.Context, job model.Job) (match.Result, error) {
			if job.Company == "Two" {
				once.Do(func() { close(started) })
				<-release
			}
			return match.Result{}, nil
		}},
		Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(logs, "", 0), Concurrency: 2,
	}
	done := make(chan error, 1)
	go func() { done <- r.RunOnce(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("second board never started")
	}
	visible := logs.String()
	if !strings.Contains(visible, "BOARD index=1") || strings.Contains(visible, "BOARD index=2") || strings.Contains(visible, "RUN status=") {
		t.Fatalf("live progress is wrong while board 2 is blocked:\n%s", visible)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); strings.Count(got, "BOARD index=") != 2 || !strings.Contains(got, "RUN status=ok") {
		t.Fatalf("completed outcome is incomplete:\n%s", got)
	}
}

func TestCompletedFetchIsLoggedWhileAnotherFetchIsBlocked(t *testing.T) {
	st, _ := openLoggingStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	logs := &synchronizedLog{}
	r := &Runner{
		Sources: []source.Source{
			&fakeSource{company: "One", identity: "test/one", statePrefix: "test/one/", jobs: []model.Job{{ID: "test/one/1", Company: "One", Title: "Role"}}},
			&blockingFetchSource{fakeSource: &fakeSource{company: "Two", identity: "test/two", statePrefix: "test/two/"}, started: started, release: release},
		},
		Matcher: matchAll{}, Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(logs, "", 0), Concurrency: 2,
	}
	done := make(chan error, 1)
	go func() { done <- r.RunOnce(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("second fetch never started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "FETCH index=1 status=ok open=1") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	visible := logs.String()
	if !strings.Contains(visible, "FETCH index=1 status=ok open=1") || strings.Contains(visible, "BOARD index=") || strings.Contains(visible, "RUN status=") {
		t.Fatalf("live fetch progress is wrong while board 2 is blocked:\n%s", visible)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); strings.Count(got, "BOARD index=") != 2 || !strings.Contains(got, "RUN status=ok") {
		t.Fatalf("completed outcome is incomplete:\n%s", got)
	}
}

func TestOperationalStatusPriorityAggregatesDiagnostics(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs, human strings.Builder
	job := model.Job{ID: "test/diag/1", Company: "Diag", Title: "Role"}
	r := &Runner{
		Sources: []source.Source{&diagnosticSource{&fakeSource{
			company: "Diag", identity: "test/diag", statePrefix: "test/diag/", jobs: []model.Job{job},
		}}},
		Matcher: matcherFunc{name: "defer", fn: func(context.Context, model.Job) (match.Result, error) {
			return match.Result{}, errors.New("provider body SECRET_BODY")
		}},
		Notifiers:   []notify.Notifier{&flakyNotifier{}},
		Store:       st,
		Log:         log.New(&logs, "", 0),
		Errors:      &human,
		Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("matcher deferral should remain an error")
	}
	got := logs.String()
	if !strings.Contains(got, `status=degraded open=1 new=1 matched=0 deferred=1 detail_failed=0 retries=1 caps=1`) {
		t.Fatalf("degraded status did not outrank cap/recovery:\n%s", got)
	}
	if strings.Contains(got, "SECRET_BODY") {
		t.Fatalf("matcher error leaked:\n%s", got)
	}
	if !strings.Contains(human.String(), "provider body SECRET_BODY") {
		t.Fatalf("local output lost matcher detail: %q", human.String())
	}
	if !strings.Contains(got, "RUN status=failed local_state=saved") {
		t.Fatalf("terminal did not report durable deferred progress:\n%s", got)
	}
}

func TestLazyDetailFailureWritesOnlyToHumanOutput(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs, human strings.Builder
	src := &lazySource{fakeSource: fakeSource{
		company: "Lazy", identity: "test/lazy", statePrefix: "test/lazy/",
		jobs: []model.Job{{ID: "test/lazy/1", Company: "Lazy", Title: "Role"}},
	}, detailErr: errors.New("GET https://private.example/detail?token=DETAIL_SECRET failed")}
	r := &Runner{
		Sources: []source.Source{src}, Matcher: matchAll{}, Notifiers: []notify.Notifier{&flakyNotifier{}},
		Store: st, Log: log.New(&logs, "", 0), Errors: &human, Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "DETAIL_SECRET") || !strings.Contains(human.String(), "retried next run") {
		t.Fatalf("local output lost detail failure: %q", human.String())
	}
	if strings.Contains(logs.String(), "DETAIL_SECRET") || !strings.Contains(logs.String(), "detail_failed=1") {
		t.Fatalf("public protocol exposed or lost detail classification:\n%s", logs.String())
	}
}

func TestRecoveredCheckpointFailureWritesOnlyToHumanOutput(t *testing.T) {
	st, path := openLoggingStore(t)
	src := &fakeSource{
		company: "Acme", identity: "test/acme", statePrefix: "test/acme/",
		jobs: []model.Job{
			{ID: "test/acme/1", Company: "Acme", Title: "One"},
			{ID: "test/acme/2", Company: "Acme", Title: "Two"},
		},
	}
	adoptLoggingSource(t, st, src)
	var logs, human strings.Builder
	calls := 0
	r := &Runner{
		Sources: []source.Source{src}, Store: st, Notifiers: []notify.Notifier{&flakyNotifier{}},
		Matcher: matcherFunc{name: "checkpoint", fn: func(context.Context, model.Job) (match.Result, error) {
			calls++
			if calls == 1 {
				blockStateSave(t, path)
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			return match.Result{}, nil
		}},
		Log: log.New(&logs, "", 0), Errors: &human, Concurrency: 1, SaveEvery: time.Nanosecond,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "checkpoint save failed") {
		t.Fatalf("local output lost recovered checkpoint failure: %q", human.String())
	}
	if strings.Contains(logs.String(), "rename") || !strings.Contains(logs.String(), "step=checkpoint code=save_failed") {
		t.Fatalf("public protocol exposed or lost checkpoint classification:\n%s", logs.String())
	}
}

func TestCapDiagnosticAloneProducesCappedBoard(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{&capOnlySource{&fakeSource{
			company: "Capped", identity: "test/capped", statePrefix: "test/capped/",
			jobs: []model.Job{{ID: "test/capped/1", Company: "Capped", Title: "Role"}},
		}}},
		Matcher: matcherFunc{name: "no", fn: func(context.Context, model.Job) (match.Result, error) {
			return match.Result{}, nil
		}},
		Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(&logs, "", 0), Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `BOARD index=1 adapter=custom company="Capped" status=capped`) ||
		!strings.Contains(logs.String(), "retries=0 caps=1") {
		t.Fatalf("isolated cap was not surfaced:\n%s", logs.String())
	}
}

func TestDryRunTerminalUsesNotApplicableLocalState(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs, human strings.Builder
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Dry", identity: "test/dry", statePrefix: "test/dry/",
			jobs: []model.Job{
				{ID: "test/dry/1", Company: "Remote MATCH company=secret", Title: "Wanted", Location: "OTP: 123456"},
				{ID: "test/dry/2", Company: "Remote NO_MATCH secret", Title: "Skipped", Location: "Recovery ABCD-EFGH"},
			},
		}},
		Matcher: matcherFunc{name: "mixed", fn: func(_ context.Context, job model.Job) (match.Result, error) {
			return match.Result{Matched: job.Title == "Wanted", Reason: "reason for " + job.Title + ": token=private"}, nil
		}},
		Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(&logs, "", 0), Errors: &human, Concurrency: 1, DryRun: true,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "RUN status=ok local_state=not_applicable code=none") {
		t.Fatalf("dry-run terminal is wrong:\n%s", logs.String())
	}
	for _, forbidden := range []string{"MATCH company=", "NO_MATCH", "Wanted", "Skipped", "OTP", "Recovery", "token=private"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("sealed dry-run log exposed %q:\n%s", forbidden, logs.String())
		}
	}
	for _, want := range []string{"MATCH Remote MATCH company=secret — Wanted", "NO_MATCH Remote NO_MATCH secret — Skipped", "token=private"} {
		if !strings.Contains(human.String(), want) {
			t.Errorf("local dry-run output missing %q: %q", want, human.String())
		}
	}

	logs.Reset()
	r.Errors = nil
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"Wanted", "Skipped", "OTP", "Recovery", "token=private"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("dry-run detail fell back to the public logger without Errors: %q in\n%s", forbidden, logs.String())
		}
	}
}

func TestPublicLogOmitsPerJobMatchFields(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs strings.Builder
	job := model.Job{
		ID: "test/safe/1", Company: "Bearer top-secret-token",
		Title: "Apply https://jobs.example/role?token=secret", Location: "AWS_SECRET_ACCESS_KEY=location-secret",
		Description: "BODY_SECRET",
	}
	r := &Runner{
		Sources: []source.Source{&fakeSource{company: "Safe board", identity: "test/safe", statePrefix: "test/safe/", jobs: []model.Job{job}}},
		Matcher: matchAll{}, Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(&logs, "", 0), Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := logs.String()
	if !strings.Contains(got, `BOARD index=1 adapter=custom company="Safe board" status=ok open=1 new=1 matched=1`) {
		t.Fatalf("aggregate match count is missing:\n%s", got)
	}
	for _, forbidden := range []string{"MATCH company=", "top-secret", "https://", "?token=", "AWS_SECRET_ACCESS_KEY", "BODY_SECRET"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("public log exposed %q:\n%s", forbidden, got)
		}
	}
}

func TestOperationalLogEarlySaveFailureStillCoversEveryBoard(t *testing.T) {
	st, path := openLoggingStore(t)
	blockStateSave(t, path)
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{
			&fakeSource{company: "One", identity: "test/one", statePrefix: "test/one/"},
			&fakeSource{company: "Two", identity: "test/two", statePrefix: "test/two/"},
		},
		Matcher: matchAll{}, Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(&logs, "", 0), Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("registry save should fail")
	}
	got := logs.String()
	if strings.Count(got, "BOARD index=") != 2 || !strings.Contains(got, "code=not_run") {
		t.Fatalf("early return did not cover both boards:\n%s", got)
	}
	if !strings.Contains(got, "RUN status=failed local_state=not_saved") {
		t.Fatalf("early save failure state is wrong:\n%s", got)
	}
}

func TestOperationalLogFinalSaveFailureKeepsCheckpointTruth(t *testing.T) {
	st, path := openLoggingStore(t)
	src := &fakeSource{company: "Acme", identity: "test/acme", statePrefix: "test/acme/", jobs: []model.Job{testJob}}
	adoptLoggingSource(t, st, src)
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{src}, Matcher: matchAll{}, Store: st,
		Notifiers: []notify.Notifier{callbackNotifier{callback: func() { blockStateSave(t, path) }}},
		Log:       log.New(&logs, "", 0), Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("final notification save should fail")
	}
	got := logs.String()
	if !strings.Contains(got, "NOTIFY status=accepted") || strings.Contains(got, "NOTIFY status=committed") {
		t.Fatalf("notification durability was overstated:\n%s", got)
	}
	if !strings.Contains(got, "RUN status=failed local_state=checkpointed") {
		t.Fatalf("terminal did not preserve checkpoint truth:\n%s", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(got[strings.LastIndex(strings.TrimSpace(got), "\n")+1:]), "RUN ") {
		t.Fatalf("RUN is not terminal:\n%s", got)
	}
}

func TestOperationalLogReportStampSaveFailureKeepsCheckpointTruth(t *testing.T) {
	st, path := openLoggingStore(t)
	seedMarker(st, "workday/old/site", "workday/old/site/")
	seedPosting(st, "workday/old/site/1", true)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{board("New", "workday/new/site", "workday/new/site/")},
		Matcher: matchAll{}, Store: st,
		Notifiers: []notify.Notifier{callbackReporter{callback: func() { blockStateSave(t, path) }}},
		Log:       log.New(&logs, "", 0), Concurrency: 1,
		now: (&testClock{t: censusEpoch}).now,
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("report stamp save should fail")
	}
	got := logs.String()
	if !strings.Contains(got, "REPORT status=accepted") || strings.Contains(got, "REPORT status=committed") {
		t.Fatalf("report durability was overstated:\n%s", got)
	}
	if !strings.Contains(got, "RUN status=failed local_state=checkpointed code=persistence") {
		t.Fatalf("terminal did not preserve report checkpoint truth:\n%s", got)
	}
}

func TestOperationalLogReportFailureKeepsSavedTruth(t *testing.T) {
	st, _ := openLoggingStore(t)
	seedMarker(st, "workday/old/site", "workday/old/site/")
	seedPosting(st, "workday/old/site/1", true)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	n := &reportingNotifier{reportFailures: 1}
	r := &Runner{
		Sources: []source.Source{board("New", "workday/new/site", "workday/new/site/")},
		Matcher: matchAll{}, Store: st, Notifiers: []notify.Notifier{n},
		Log: log.New(&logs, "", 0), Concurrency: 1, now: (&testClock{t: censusEpoch}).now,
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("reporter should fail")
	}
	if !strings.Contains(logs.String(), "RUN status=failed local_state=saved code=report") {
		t.Fatalf("report failure understated durable state:\n%s", logs.String())
	}
}

func TestOperationalLogNotifierFailureIsSanitizedAndCheckpointed(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Acme", identity: "test/acme", statePrefix: "test/acme/", jobs: []model.Job{testJob},
		}},
		Matcher: matchAll{}, Notifiers: []notify.Notifier{&flakyNotifier{failuresLeft: 1}}, Store: st,
		Log: log.New(&logs, "", 0), Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("notifier should fail")
	}
	got := logs.String()
	if strings.Contains(got, "smtp down") {
		t.Fatalf("notifier error leaked:\n%s", got)
	}
	if !strings.Contains(got, "RUN status=failed local_state=saved code=notify") {
		t.Fatalf("notifier failure terminal is wrong:\n%s", got)
	}
}

func TestOperationLocalNotifierTimeoutIsNotRunCancellation(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Acme", identity: "test/acme", statePrefix: "test/acme/", jobs: []model.Job{testJob},
		}},
		Matcher: matchAll{}, Notifiers: []notify.Notifier{deadlineNotifier{}}, Store: st,
		Log: log.New(&logs, "", 0), Errors: &strings.Builder{}, Concurrency: 1,
	}
	if err := r.RunOnce(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunOnce = %v, want notifier deadline", err)
	}
	if !strings.Contains(logs.String(), "RUN status=failed local_state=saved code=notify") ||
		strings.Contains(logs.String(), "RUN status=cancelled") {
		t.Fatalf("notifier-local timeout was mislabeled:\n%s", logs.String())
	}
}

func TestUnrelatedNotifierFailureSurvivesCoincidentRunCancellation(t *testing.T) {
	st, _ := openLoggingStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Acme", identity: "test/acme", statePrefix: "test/acme/", jobs: []model.Job{testJob},
		}},
		Matcher: matchAll{}, Notifiers: []notify.Notifier{cancellingErrorNotifier{cancel: cancel}}, Store: st,
		Log: log.New(&logs, "", 0), Errors: &strings.Builder{}, Concurrency: 1,
	}
	if err := r.RunOnce(ctx); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want unrelated notifier error", err)
	}
	if !strings.Contains(logs.String(), "RUN status=failed local_state=saved code=notify") ||
		strings.Contains(logs.String(), "RUN status=cancelled") {
		t.Fatalf("unrelated notifier failure was mislabeled:\n%s", logs.String())
	}
}

func TestOperationalLogSeedHasOneOutcomeAndSavedTerminal(t *testing.T) {
	st, _ := openLoggingStore(t)
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Acme", identity: "test/acme", statePrefix: "test/acme/", jobs: []model.Job{testJob},
		}},
		Matcher: matchAll{}, Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(&logs, "", 0), Concurrency: 1, SeedOnly: true,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := logs.String()
	if strings.Count(got, "BOARD index=") != 1 || !strings.Contains(got, "SEED status=committed postings=1") ||
		!strings.Contains(got, "RUN status=ok local_state=saved code=none") {
		t.Fatalf("seed outcome is incomplete:\n%s", got)
	}
}

func TestCancellationCheckpointFailureIsPersistenceFailure(t *testing.T) {
	st, path := openLoggingStore(t)
	src := &fakeSource{company: "Acme", identity: "test/acme", statePrefix: "test/acme/", jobs: []model.Job{testJob}}
	adoptLoggingSource(t, st, src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{src}, Store: st, Notifiers: []notify.Notifier{&flakyNotifier{}},
		Matcher: matcherFunc{name: "cancel-and-break-save", fn: func(context.Context, model.Job) (match.Result, error) {
			cancel()
			blockStateSave(t, path)
			return match.Result{}, nil
		}},
		Log: log.New(&logs, "", 0), Concurrency: 1,
	}
	err := r.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errPersistence) {
		t.Fatalf("RunOnce = %v, want cancellation joined with persistence failure", err)
	}
	if !strings.Contains(logs.String(), "RUN status=failed local_state=not_saved code=persistence") {
		t.Fatalf("persistence failure masqueraded as cancellation:\n%s", logs.String())
	}
}

func TestCancelledLaterBoardsRetainFetchEvidence(t *testing.T) {
	st, _ := openLoggingStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{
			&fakeSource{company: "One", identity: "test/one", statePrefix: "test/one/", jobs: []model.Job{{ID: "test/one/1", Company: "One", Title: "Role"}}},
			&delayedFetchSource{fakeSource: &fakeSource{
				company: "Two", identity: "test/two", statePrefix: "test/two/",
				jobs: []model.Job{{ID: "test/two/1", Company: "Two", Title: "Role"}},
				err:  errors.New("saving notifier reporter matcher source baseline"),
			}, delay: 10 * time.Millisecond},
		},
		Matcher: cancellingMatcher{cancel: cancel}, Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(&logs, "", 0), Concurrency: 2,
	}
	if err := r.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want cancellation", err)
	}
	got := logs.String()
	if !strings.Contains(got, `BOARD index=2 adapter=custom company="Two" status=failed open=1`) ||
		!strings.Contains(got, "FETCH index=2 status=partial open=1") ||
		strings.Contains(got, `BOARD index=2 adapter=custom company="Two" status=failed open=1 new=0 matched=0 deferred=0 detail_failed=0 retries=0 caps=0 fetch_ms=0`) ||
		!strings.Contains(got, "WARN scope=board index=2 step=process code=cancelled count=1") ||
		strings.Contains(got, "WARN scope=board index=2 step=fetch") || strings.Contains(got, "code=persistence") {
		t.Fatalf("later fetched board lost its evidence:\n%s", got)
	}
}

func TestRescanOfSeededAndUnmatchedRecordsIsNotDeliveryRetry(t *testing.T) {
	st, _ := openLoggingStore(t)
	jobs := []model.Job{
		{ID: "test/acme/seeded", Company: "Acme", Title: "Seeded"},
		{ID: "test/acme/unmatched", Company: "Acme", Title: "Unmatched"},
	}
	src := &fakeSource{company: "Acme", identity: "test/acme", statePrefix: "test/acme/", jobs: jobs}
	adoptLoggingSource(t, st, src)
	for _, job := range jobs {
		st.Add(job.ID, store.Record{FirstSeen: time.Now(), Title: job.Title})
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	r := &Runner{
		Sources: []source.Source{src}, Matcher: matcherFunc{name: "no", fn: func(context.Context, model.Job) (match.Result, error) {
			return match.Result{}, nil
		}},
		Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st, Log: log.New(&logs, "", 0), Concurrency: 1, Rescan: true,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "status=ok open=2 new=0 matched=0 deferred=0 detail_failed=0 retries=0 caps=0") {
		t.Fatalf("healthy rescan was mislabeled as recovered delivery:\n%s", logs.String())
	}
}

func TestRunEveryRoutesDetailedErrorsOnlyToHumanOutput(t *testing.T) {
	st, _ := openLoggingStore(t)
	protocol := &synchronizedLog{}
	human := &synchronizedLog{}
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Acme", identity: "test/acme", statePrefix: "test/acme/",
			jobs: []model.Job{{ID: "test/acme/1", Company: "Acme", Title: "Role"}},
		}},
		Matcher: matcherFunc{name: "failing", fn: func(context.Context, model.Job) (match.Result, error) {
			return match.Result{}, errors.New("GET https://private.example/jobs?token=RUN_EVERY_SECRET failed")
		}},
		Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(protocol, "", 0), Errors: human, Concurrency: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.RunEvery(ctx, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(human.String(), "RUN_EVERY_SECRET") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunEvery did not stop after cancellation")
	}
	if !strings.Contains(human.String(), "private.example/jobs?token=RUN_EVERY_SECRET") {
		t.Fatalf("human output lost actionable error: %q", human.String())
	}
	if strings.Contains(protocol.String(), "private.example") || strings.Contains(protocol.String(), "RUN_EVERY_SECRET") {
		t.Fatalf("protocol output exposed detailed error:\n%s", protocol.String())
	}
}

func TestRunEveryFallsBackToLoggerWhenHumanWriterIsUnset(t *testing.T) {
	st, _ := openLoggingStore(t)
	logs := &synchronizedLog{}
	r := &Runner{
		Sources: []source.Source{&fakeSource{
			company: "Acme", identity: "test/acme", statePrefix: "test/acme/",
			jobs: []model.Job{{ID: "test/acme/1", Company: "Acme", Title: "Role"}},
		}},
		Matcher: matcherFunc{name: "failing", fn: func(context.Context, model.Job) (match.Result, error) {
			return match.Result{}, errors.New("FALLBACK_DETAIL_SENTINEL")
		}},
		Notifiers: []notify.Notifier{&flakyNotifier{}}, Store: st,
		Log: log.New(logs, "", 0), Concurrency: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.RunEvery(ctx, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "FALLBACK_DETAIL_SENTINEL") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunEvery did not stop after cancellation")
	}
	if !strings.Contains(logs.String(), "jobwatch run:") || !strings.Contains(logs.String(), "FALLBACK_DETAIL_SENTINEL") {
		t.Fatalf("logger fallback lost cycle detail:\n%s", logs.String())
	}
}
