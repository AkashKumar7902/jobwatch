package statebranch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	validStateOne = `{
 "lever/acme/123": {
  "first_seen": "2026-08-05T01:02:03.000000004Z",
  "title": "Software Engineer",
  "matched": true,
  "notified": false
 }
}`
	validStateTwo = `{
 "lever/acme/123": {
  "first_seen": "2026-08-05T01:02:03.000000004Z",
  "title": "Software Engineer",
  "matched": true,
  "notified": false
 },
 "greenhouse/example/456": {
  "first_seen": "2026-08-05T02:03:04Z",
  "title": "Junior Developer",
  "matched": false,
  "notified": false
 }
}`
)

func TestValidateProductionStyleRawMap(t *testing.T) {
	state := []byte(`{
 "__jobwatch_source__/amazon/IND": {
  "first_seen": "2026-08-01T12:48:28.623520366Z",
  "title": "source: Amazon India",
  "matched": false,
  "notified": false
 },
 "__jobwatch_source__/ashby/openai": {
  "first_seen": "2026-07-16T19:27:53.859972364Z",
  "title": "source: OpenAI",
  "matched": false,
  "notified": false
 },
 "__jobwatch_source_seed_in_progress__/ashby/openai": {
  "first_seen": "2026-08-05T04:05:06.123456789Z",
  "title": "source seed in progress: OpenAI",
  "matched": false,
  "notified": false
 },
 "workday/nvidia/9001": {
  "first_seen": "2026-08-05T01:02:03Z",
  "title": "Systems Software Engineer",
  "matched": true,
  "notified": true
 },
 "smartrecruiters/visa/9002": {
  "first_seen": "2026-08-05T01:03:04.123456789+05:30",
  "title": "Software Engineer",
  "matched": false,
  "notified": false
 }
}`)
	records, err := validateState(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 {
		t.Fatalf("record count = %d, want 5", len(records))
	}
	if got := records["__jobwatch_source__/amazon/IND"].title; got != "source: Amazon India" {
		t.Fatalf("marker title = %q", got)
	}
}

func TestValidateStateRejectsAmbiguousOrInvalidInput(t *testing.T) {
	validRecord := `{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","matched":false,"notified":false}`
	tests := map[string]string{
		"top level array":        `[]`,
		"duplicate ID":           `{"id":` + validRecord + `,"id":` + validRecord + `}`,
		"empty ID":               `{"   ":` + validRecord + `}`,
		"record not object":      `{"id":null}`,
		"duplicate field":        `{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","title":"Other","matched":false,"notified":false}}`,
		"unknown field":          `{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","matched":false,"notified":false,"extra":1}}`,
		"missing field":          `{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","matched":false}}`,
		"wrong timestamp type":   `{"id":{"first_seen":1,"title":"Engineer","matched":false,"notified":false}}`,
		"invalid timestamp":      `{"id":{"first_seen":"yesterday","title":"Engineer","matched":false,"notified":false}}`,
		"zero timestamp":         `{"id":{"first_seen":"0001-01-01T00:00:00Z","title":"Engineer","matched":false,"notified":false}}`,
		"empty title":            `{"id":{"first_seen":"2026-08-05T01:02:03Z","title":" ","matched":false,"notified":false}}`,
		"wrong matched type":     `{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","matched":null,"notified":false}}`,
		"wrong notified type":    `{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","matched":false,"notified":"false"}}`,
		"notified non-match":     `{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","matched":false,"notified":true}}`,
		"trailing JSON":          `{"id":` + validRecord + `} {}`,
		"trailing invalid token": `{"id":` + validRecord + `} garbage`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := validateState([]byte(input)); err == nil {
				t.Fatal("validateState unexpectedly accepted invalid input")
			}
		})
	}
}

func TestValidateStateRejectsInvalidUTF8(t *testing.T) {
	state := append([]byte(`{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"`), 0xff)
	state = append(state, []byte(`","matched":false,"notified":false}}`)...)
	if _, err := validateState(state); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateStateRejectsUnpairedSurrogateEscapes(t *testing.T) {
	tests := []string{
		`{"\ud800":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer","matched":false,"notified":false}}`,
		`{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"\udfff","matched":false,"notified":false}}`,
		`{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"\ud800\u0041","matched":false,"notified":false}}`,
	}
	for _, state := range tests {
		if _, err := validateState([]byte(state)); err == nil || !strings.Contains(err.Error(), "surrogate") {
			t.Fatalf("state %q: error = %v", state, err)
		}
	}
	paired := `{"id":{"first_seen":"2026-08-05T01:02:03Z","title":"Engineer \ud83d\ude80","matched":false,"notified":false}}`
	if _, err := validateState([]byte(paired)); err != nil {
		t.Fatalf("valid surrogate pair rejected: %v", err)
	}
}

func TestRestoreExistingStateAndWriteOutputs(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateTwo), nil)
	statePath := filepath.Join(t.TempDir(), "nested", "state.json")
	outputPath := filepath.Join(t.TempDir(), "github-output")

	result, err := Restore(context.Background(), RestoreOptions{
		RepoDir:      repo.work,
		Remote:       "origin",
		Branch:       "state",
		StatePath:    statePath,
		GitHubOutput: outputPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != ModeRestored || result.BaseSHA != base || result.Count != 2 {
		t.Fatalf("result = %+v, want restored base %s and 2 records", result, base)
	}
	assertFile(t, statePath, validStateTwo)
	assertFile(t, outputPath, "mode=restored\nbase_sha="+base+"\ncount=2\n")
	if repo.refExists(t, "refs/jobwatch-state/restore-") {
		t.Fatal("private restore ref was not cleaned up")
	}
}

func TestRestoreFetchesOnlyStateTip(t *testing.T) {
	repo := newGitFixture(t)
	oldest := repo.seedState(t, []byte(validStateOne), nil)
	middle := repo.seedState(t, []byte(validStateTwo), nil, oldest)
	latestState := stateWithExtra("ashby/example/789", "Graduate Engineer")
	latest := repo.seedState(t, []byte(latestState), nil, middle)

	if _, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: filepath.Join(t.TempDir(), "state.json"),
	}); err != nil {
		t.Fatal(err)
	}
	shallowPath := strings.TrimSpace(repo.git(t, "rev-parse", "--git-path", "shallow"))
	if !filepath.IsAbs(shallowPath) {
		shallowPath = filepath.Join(repo.work, shallowPath)
	}
	shallowData, err := os.ReadFile(shallowPath)
	if err != nil {
		t.Fatal(err)
	}
	if !linePresent(string(shallowData), latest) {
		t.Fatalf("state tip %s is not a shallow boundary:\n%s", latest, shallowData)
	}
	cmd := exec.Command("git", "-C", repo.work, "cat-file", "-e", middle+"^{commit}")
	if err := cmd.Run(); err == nil {
		t.Fatalf("parent state commit %s was fetched despite --depth=1", middle)
	}
}

func TestRestoreMissingRequiresExplicitBootstrap(t *testing.T) {
	repo := newGitFixture(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	_, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: statePath,
	})
	if err == nil || !strings.Contains(err.Error(), "--allow-bootstrap") {
		t.Fatalf("missing branch error = %v", err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("state file exists after rejected bootstrap: %v", statErr)
	}

	result, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: statePath, AllowBootstrap: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != (RestoreResult{Mode: ModeBootstrap}) {
		t.Fatalf("result = %+v", result)
	}
	assertFile(t, statePath, "{}")
}

func TestRestoreRejectsBootstrapWhenStateExists(t *testing.T) {
	repo := newGitFixture(t)
	repo.seedState(t, []byte(validStateOne), nil)
	_, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: filepath.Join(t.TempDir(), "state.json"), AllowBootstrap: true,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v", err)
	}
}

func TestRestoreTransportFailureDoesNotCreateBaseline(t *testing.T) {
	repo := newGitFixture(t)
	repo.git(t, "remote", "add", "broken", filepath.Join(t.TempDir(), "absent.git"))
	statePath := filepath.Join(t.TempDir(), "state.json")
	_, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, Remote: "broken", StatePath: statePath, AllowBootstrap: true,
	})
	if err == nil || !strings.Contains(err.Error(), "query state branch") {
		t.Fatalf("transport error = %v", err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("state file exists after transport failure: %v", statErr)
	}
}

func TestRestoreRejectsInvalidStateWithoutOverwritingDestination(t *testing.T) {
	repo := newGitFixture(t)
	repo.seedState(t, []byte(`{"id":{"title":"missing fields"}}`), nil)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), RestoreOptions{RepoDir: repo.work, StatePath: statePath})
	if err == nil || !strings.Contains(err.Error(), "validate restored state") {
		t.Fatalf("error = %v", err)
	}
	assertFile(t, statePath, "sentinel")
}

func TestRestoreRejectsStateCommitWithExtraTreeEntry(t *testing.T) {
	repo := newGitFixture(t)
	repo.seedState(t, []byte(validStateOne), map[string][]byte{"extra.txt": []byte("unexpected")})
	_, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: filepath.Join(t.TempDir(), "state.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("error = %v", err)
	}
}

func TestPublishCreatesSingleParentCommitAndThenNoOps(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateOne), nil)
	statePath := filepath.Join(t.TempDir(), "state.json")
	restored, err := Restore(context.Background(), RestoreOptions{RepoDir: repo.work, StatePath: statePath})
	if err != nil {
		t.Fatal(err)
	}
	if restored.BaseSHA != base {
		t.Fatalf("restored base = %s, want %s", restored.BaseSHA, base)
	}
	if err := os.WriteFile(statePath, []byte(validStateTwo), 0o644); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "github-output")
	result, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored,
		BaseSHA: base, SourceSHA: repo.sourceSHA, GitHubOutput: outputPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.PublishSHA == base {
		t.Fatalf("result = %+v", result)
	}
	if remoteHead := repo.remoteRef(t, "refs/heads/state"); remoteHead != result.PublishSHA {
		t.Fatalf("remote state = %s, want %s", remoteHead, result.PublishSHA)
	}
	parents := repo.commitParents(t, result.PublishSHA)
	if len(parents) != 1 || parents[0] != base {
		t.Fatalf("parents = %v, want [%s]", parents, base)
	}
	assertFile(t, outputPath, "publish_sha="+result.PublishSHA+"\nchanged=true\n")

	secondOutput := filepath.Join(t.TempDir(), "github-output")
	noOp, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored,
		BaseSHA: result.PublishSHA, SourceSHA: repo.sourceSHA, GitHubOutput: secondOutput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if noOp != (PublishResult{PublishSHA: result.PublishSHA, Changed: false}) {
		t.Fatalf("no-op result = %+v", noOp)
	}
	assertFile(t, secondOutput, "publish_sha="+result.PublishSHA+"\nchanged=false\n")
}

func TestPublishRejectsRemovedRecord(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateTwo), nil)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if _, err := Restore(context.Background(), RestoreOptions{RepoDir: repo.work, StatePath: statePath}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored, BaseSHA: base, SourceSHA: repo.sourceSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "removed existing job ID") {
		t.Fatalf("error = %v", err)
	}
	if got := repo.remoteRef(t, "refs/heads/state"); got != base {
		t.Fatalf("remote moved to %s after rejected removal, want %s", got, base)
	}
}

func TestPublishRejectsNotifiedRecordRollback(t *testing.T) {
	repo := newGitFixture(t)
	notifiedState := strings.Replace(validStateOne, `"notified": false`, `"notified": true`, 1)
	base := repo.seedState(t, []byte(notifiedState), nil)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if _, err := Restore(context.Background(), RestoreOptions{RepoDir: repo.work, StatePath: statePath}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored,
		BaseSHA: base, SourceSHA: repo.sourceSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "back to pending") {
		t.Fatalf("error = %v", err)
	}
	if got := repo.remoteRef(t, "refs/heads/state"); got != base {
		t.Fatalf("remote moved to %s after rejected notification rollback, want %s", got, base)
	}
}

func TestPublishRejectsBaseWithExtraTreeEntry(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateOne), map[string][]byte{"extra.txt": []byte("unexpected")})
	repo.git(t, "fetch", "--depth=1", "origin", "refs/heads/state")
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored,
		BaseSHA: base, SourceSHA: repo.sourceSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("error = %v", err)
	}
	if got := repo.remoteRef(t, "refs/heads/state"); got != base {
		t.Fatalf("remote moved to %s after invalid base, want %s", got, base)
	}
}

func TestPublishIgnoresLocalReplaceRefsWhenValidatingBase(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateTwo), nil)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if _, err := Restore(context.Background(), RestoreOptions{RepoDir: repo.work, StatePath: statePath}); err != nil {
		t.Fatal(err)
	}
	fake := repo.localStateCommit(t, []byte(validStateOne))
	repo.git(t, "replace", base, fake)
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored,
		BaseSHA: base, SourceSHA: repo.sourceSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "removed existing job ID") {
		t.Fatalf("error = %v", err)
	}
	if got := repo.remoteRef(t, "refs/heads/state"); got != base {
		t.Fatalf("remote moved to %s after replacement attack, want %s", got, base)
	}
}

func TestPublishBootstrapCreatesRootCommit(t *testing.T) {
	repo := newGitFixture(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if _, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: statePath, AllowBootstrap: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeBootstrap, SourceSHA: repo.sourceSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || len(repo.commitParents(t, result.PublishSHA)) != 0 {
		t.Fatalf("bootstrap result = %+v, parents = %v", result, repo.commitParents(t, result.PublishSHA))
	}
}

func TestPublishRejectsConcurrentAdvanceAndDeletion(t *testing.T) {
	t.Run("advance", func(t *testing.T) {
		repo := newGitFixture(t)
		base := repo.seedState(t, []byte(validStateOne), nil)
		statePath := filepath.Join(t.TempDir(), "state.json")
		if _, err := Restore(context.Background(), RestoreOptions{RepoDir: repo.work, StatePath: statePath}); err != nil {
			t.Fatal(err)
		}
		advanced := repo.seedState(t, []byte(validStateTwo), nil, base)
		_, err := Publish(context.Background(), PublishOptions{
			RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored, BaseSHA: base, SourceSHA: repo.sourceSHA,
		})
		if err == nil || !strings.Contains(err.Error(), "advanced after restore") {
			t.Fatalf("error = %v", err)
		}
		if got := repo.remoteRef(t, "refs/heads/state"); got != advanced {
			t.Fatalf("remote = %s, want concurrent head %s", got, advanced)
		}
	})

	t.Run("deletion", func(t *testing.T) {
		repo := newGitFixture(t)
		base := repo.seedState(t, []byte(validStateOne), nil)
		statePath := filepath.Join(t.TempDir(), "state.json")
		if _, err := Restore(context.Background(), RestoreOptions{RepoDir: repo.work, StatePath: statePath}); err != nil {
			t.Fatal(err)
		}
		repo.git(t, "push", "origin", ":refs/heads/state")
		_, err := Publish(context.Background(), PublishOptions{
			RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored, BaseSHA: base, SourceSHA: repo.sourceSHA,
		})
		if err == nil || !strings.Contains(err.Error(), "deleted after restore") {
			t.Fatalf("error = %v", err)
		}
		if repo.remoteRefOptional(t, "refs/heads/state") != "" {
			t.Fatal("publish recreated concurrently deleted state branch")
		}
	})
}

func TestConcurrentSameBasePublishExactlyOneWins(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateOne), nil)
	if _, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: filepath.Join(t.TempDir(), "restored.json"),
	}); err != nil {
		t.Fatal(err)
	}

	paths := []string{
		filepath.Join(t.TempDir(), "candidate-a.json"),
		filepath.Join(t.TempDir(), "candidate-b.json"),
	}
	states := []string{
		stateWithExtra("ashby/acme/a", "Engineer A"),
		stateWithExtra("ashby/acme/b", "Engineer B"),
	}
	for i := range paths {
		if err := os.WriteFile(paths[i], []byte(states[i]), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	type outcome struct {
		result PublishResult
		err    error
	}
	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for _, path := range paths {
		path := path
		go func() {
			<-start
			result, err := publish(context.Background(), PublishOptions{
				RepoDir: repo.work, StatePath: path, Mode: ModeRestored,
				BaseSHA: base, SourceSHA: repo.sourceSHA,
			}, func() {
				ready <- struct{}{}
				<-release
			})
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	awaitSignals(t, ready, 2)
	close(release)

	var winner PublishResult
	successes, failures := 0, 0
	for range paths {
		outcome := <-outcomes
		if outcome.err != nil {
			failures++
			continue
		}
		successes++
		winner = outcome.result
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("successes=%d failures=%d, want exactly one of each", successes, failures)
	}
	if !winner.Changed {
		t.Fatalf("winner = %+v, want changed result", winner)
	}
	if got := repo.remoteRef(t, "refs/heads/state"); got != winner.PublishSHA {
		t.Fatalf("remote = %s, winning publish = %s", got, winner.PublishSHA)
	}
}

func TestConcurrentBootstrapPublishExactlyOneWins(t *testing.T) {
	repo := newGitFixture(t)
	paths := []string{
		filepath.Join(t.TempDir(), "candidate-a.json"),
		filepath.Join(t.TempDir(), "candidate-b.json"),
	}
	states := []string{
		stateWithExtra("ashby/acme/a", "Engineer A"),
		stateWithExtra("ashby/acme/b", "Engineer B"),
	}
	for i := range paths {
		if err := os.WriteFile(paths[i], []byte(states[i]), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	type outcome struct {
		result PublishResult
		err    error
	}
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for _, path := range paths {
		path := path
		go func() {
			result, err := publish(context.Background(), PublishOptions{
				RepoDir: repo.work, StatePath: path, Mode: ModeBootstrap, SourceSHA: repo.sourceSHA,
			}, func() {
				ready <- struct{}{}
				<-release
			})
			outcomes <- outcome{result: result, err: err}
		}()
	}
	awaitSignals(t, ready, 2)
	close(release)

	var winner PublishResult
	successes, failures := 0, 0
	for range paths {
		outcome := <-outcomes
		if outcome.err != nil {
			failures++
			continue
		}
		successes++
		winner = outcome.result
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("successes=%d failures=%d, want exactly one of each", successes, failures)
	}
	if got := repo.remoteRef(t, "refs/heads/state"); got != winner.PublishSHA {
		t.Fatalf("remote = %s, winning bootstrap = %s", got, winner.PublishSHA)
	}
}

func TestNoOpPublishLeaseRejectsConcurrentAdvance(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateOne), nil)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if _, err := Restore(context.Background(), RestoreOptions{RepoDir: repo.work, StatePath: statePath}); err != nil {
		t.Fatal(err)
	}
	advanced := ""
	_, err := publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeRestored,
		BaseSHA: base, SourceSHA: repo.sourceSHA,
	}, func() {
		advanced = repo.seedState(t, []byte(validStateTwo), nil, base)
	})
	if err == nil || !strings.Contains(err.Error(), "stale info") {
		t.Fatalf("error = %v", err)
	}
	if got := repo.remoteRef(t, "refs/heads/state"); got != advanced {
		t.Fatalf("remote = %s, concurrent head = %s", got, advanced)
	}
}

func TestPublishRejectsModeAndSourceInconsistencies(t *testing.T) {
	repo := newGitFixture(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []PublishOptions{
		{Mode: ModeRestored, SourceSHA: repo.sourceSHA},
		{Mode: ModeBootstrap, BaseSHA: strings.Repeat("a", 40), SourceSHA: repo.sourceSHA},
		{Mode: "", SourceSHA: repo.sourceSHA},
		{Mode: ModeBootstrap, SourceSHA: "HEAD"},
		{Mode: ModeBootstrap, SourceSHA: strings.Repeat("a", 40)},
	}
	for i, opts := range tests {
		opts.RepoDir = repo.work
		opts.StatePath = statePath
		if _, err := Publish(context.Background(), opts); err == nil {
			t.Fatalf("case %d unexpectedly succeeded: %+v", i, opts)
		}
	}
}

func TestPublishRejectsTrackedDirtySourceCheckout(t *testing.T) {
	repo := newGitFixture(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.work, "README.md"), []byte("dirty source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeBootstrap, SourceSHA: repo.sourceSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "tracked changes") {
		t.Fatalf("error = %v", err)
	}
	if repo.remoteRefOptional(t, "refs/heads/state") != "" {
		t.Fatal("dirty source checkout unexpectedly published state")
	}
}

func TestPublishRejectsUntrackedSourceFile(t *testing.T) {
	repo := newGitFixture(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedDir := filepath.Join(repo.work, "internal", "injected")
	if err := os.MkdirAll(untrackedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(untrackedDir, "injected.go"), []byte("package injected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: ModeBootstrap, SourceSHA: repo.sourceSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "unignored file") {
		t.Fatalf("error = %v", err)
	}
	if repo.remoteRefOptional(t, "refs/heads/state") != "" {
		t.Fatal("untracked source file unexpectedly published state")
	}
}

func TestRestoreThenPublishAllowsCustomUntrackedArtifactPaths(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateOne), nil)
	repoDir := filepath.Join(repo.work, "subdir")
	statePath := filepath.Join(repo.work, "scratch", "custom-state.json")
	outputPath := filepath.Join(repo.work, "scratch", "custom-output")
	restored, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repoDir, StatePath: statePath, GitHubOutput: outputPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Publish(context.Background(), PublishOptions{
		RepoDir: repoDir, StatePath: statePath, GitHubOutput: outputPath,
		Mode: restored.Mode, BaseSHA: restored.BaseSHA, SourceSHA: repo.sourceSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.PublishSHA != base {
		t.Fatalf("result = %+v, want no-op at %s", result, base)
	}
}

func TestPublishAllowsRuntimeArtifactsLeftByPoller(t *testing.T) {
	repo := newGitFixture(t)
	base := repo.seedState(t, []byte(validStateOne), nil)
	statePath := filepath.Join(repo.work, "state.json")
	restored, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: statePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.work, ".state-interrupted.tmp"), []byte("partial checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Publish(context.Background(), PublishOptions{
		RepoDir: repo.work, StatePath: statePath, Mode: restored.Mode,
		BaseSHA: restored.BaseSHA, SourceSHA: repo.sourceSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.PublishSHA != base {
		t.Fatalf("result = %+v, want no-op at %s", result, base)
	}
}

func TestSubdirectoryRepoDirStillRejectsUntrackedSiblingSource(t *testing.T) {
	repo := newGitFixture(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(validStateOne), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedDir := filepath.Join(repo.work, "internal", "injected")
	if err := os.MkdirAll(untrackedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(untrackedDir, "injected.go"), []byte("package injected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(context.Background(), PublishOptions{
		RepoDir: filepath.Join(repo.work, "subdir"), StatePath: statePath,
		Mode: ModeBootstrap, SourceSHA: repo.sourceSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "unignored file") {
		t.Fatalf("error = %v", err)
	}
}

func TestRestoreHonorsCanceledContext(t *testing.T) {
	repo := newGitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Restore(ctx, RestoreOptions{RepoDir: repo.work, AllowBootstrap: true})
	if err == nil {
		t.Fatal("Restore unexpectedly succeeded with canceled context")
	}
}

func TestGitEnvironmentCannotRedirectRepositoryOrRemote(t *testing.T) {
	repo := newGitFixture(t)
	repo.seedState(t, []byte(validStateOne), nil)
	decoy := filepath.Join(t.TempDir(), "decoy.git")
	runExternalGit(t, repo.root, "init", "--bare", decoy)
	t.Setenv("GIT_DIR", decoy)
	t.Setenv("GIT_WORK_TREE", repo.writer)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(t.TempDir(), "missing.git"))
	maliciousConfig := filepath.Join(t.TempDir(), "malicious.gitconfig")
	if err := os.WriteFile(maliciousConfig, []byte("[remote \"origin\"]\n\turl = /definitely/missing.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG", maliciousConfig)
	t.Setenv("GIT_CONFIG_GLOBAL", maliciousConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", maliciousConfig)

	result, err := Restore(context.Background(), RestoreOptions{
		RepoDir: repo.work, StatePath: filepath.Join(t.TempDir(), "state.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != ModeRestored || result.Count != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestUnsafeGitEnvironmentVariables(t *testing.T) {
	for _, name := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_INDEX_FILE", "GIT_NAMESPACE",
		"GIT_SHALLOW_FILE", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM",
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		"GIT_TERMINAL_PROMPT", "GIT_NO_REPLACE_OBJECTS", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_AUTHOR_DATE", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
	} {
		if !unsafeGitEnvironmentVariable(name) {
			t.Errorf("%s was not classified as unsafe", name)
		}
	}
	for _, name := range []string{"SSH_AUTH_SOCK", "GIT_ASKPASS", "GIT_SSH_COMMAND", "PATH"} {
		if unsafeGitEnvironmentVariable(name) {
			t.Errorf("credential/tool environment %s was unexpectedly filtered", name)
		}
	}
}

type gitFixture struct {
	t         *testing.T
	root      string
	remote    string
	writer    string
	work      string
	sourceSHA string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &gitFixture{
		t:      t,
		root:   root,
		remote: filepath.Join(root, "remote.git"),
		writer: filepath.Join(root, "writer"),
		work:   filepath.Join(root, "work"),
	}
	runExternalGit(t, root, "init", "--bare", fixture.remote)
	runExternalGit(t, root, "init", fixture.writer)
	fixture.writerGit(t, "config", "user.name", "State Test")
	fixture.writerGit(t, "config", "user.email", "state@example.test")
	fixture.writerGit(t, "remote", "add", "origin", fixture.remote)
	if err := os.WriteFile(filepath.Join(fixture.writer, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fixture.writer, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.writer, "subdir", "keep.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.writerGit(t, "add", "README.md", "subdir/keep.txt")
	fixture.writerGit(t, "commit", "-m", "source")
	fixture.sourceSHA = strings.TrimSpace(fixture.writerGit(t, "rev-parse", "HEAD"))
	fixture.writerGit(t, "push", "origin", "HEAD:refs/heads/main")

	runExternalGit(t, root, "init", fixture.work)
	fixture.git(t, "config", "user.name", "State Test")
	fixture.git(t, "config", "user.email", "state@example.test")
	fixture.git(t, "remote", "add", "origin", fixture.remote)
	fixture.git(t, "fetch", "--depth=1", "origin", "refs/heads/main")
	fixture.git(t, "checkout", "--detach", "FETCH_HEAD")
	return fixture
}

func (repo *gitFixture) seedState(t *testing.T, state []byte, extra map[string][]byte, parents ...string) string {
	t.Helper()
	blob := strings.TrimSpace(repo.writerGitInput(t, state, "hash-object", "-w", "--stdin"))
	var tree strings.Builder
	fmt.Fprintf(&tree, "100644 blob %s\tstate.json\n", blob)
	for name, contents := range extra {
		extraBlob := strings.TrimSpace(repo.writerGitInput(t, contents, "hash-object", "-w", "--stdin"))
		fmt.Fprintf(&tree, "100644 blob %s\t%s\n", extraBlob, name)
	}
	treeSHA := strings.TrimSpace(repo.writerGitInput(t, []byte(tree.String()), "mktree"))
	args := []string{"commit-tree", treeSHA, "-m", "state fixture"}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	commit := strings.TrimSpace(repo.writerGit(t, args...))
	repo.writerGit(t, "push", "--force", "origin", commit+":refs/heads/state")
	return commit
}

func (repo *gitFixture) localStateCommit(t *testing.T, state []byte) string {
	t.Helper()
	blob := strings.TrimSpace(repo.gitInput(t, state, "hash-object", "-w", "--stdin"))
	tree := []byte("100644 blob " + blob + "\tstate.json\n")
	treeSHA := strings.TrimSpace(repo.gitInput(t, tree, "mktree"))
	return strings.TrimSpace(repo.git(t, "commit-tree", treeSHA, "-m", "replacement state"))
}

func (repo *gitFixture) remoteRef(t *testing.T, ref string) string {
	t.Helper()
	got := repo.remoteRefOptional(t, ref)
	if got == "" {
		t.Fatalf("remote ref %s does not exist", ref)
	}
	return got
}

func (repo *gitFixture) remoteRefOptional(t *testing.T, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", repo.remote, "rev-parse", "--verify", ref)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (repo *gitFixture) commitParents(t *testing.T, sha string) []string {
	t.Helper()
	line := strings.TrimSpace(repo.git(t, "rev-list", "--parents", "-n", "1", sha))
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != sha {
		t.Fatalf("malformed rev-list output %q", line)
	}
	return fields[1:]
}

func (repo *gitFixture) refExists(t *testing.T, prefix string) bool {
	t.Helper()
	out := repo.git(t, "for-each-ref", "--format=%(refname)", prefix)
	return strings.TrimSpace(out) != ""
}

func (repo *gitFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	return runGitForTest(t, repo.work, nil, args...)
}

func (repo *gitFixture) gitInput(t *testing.T, input []byte, args ...string) string {
	t.Helper()
	return runGitForTest(t, repo.work, input, args...)
}

func (repo *gitFixture) writerGit(t *testing.T, args ...string) string {
	t.Helper()
	return runGitForTest(t, repo.writer, nil, args...)
}

func (repo *gitFixture) writerGitInput(t *testing.T, input []byte, args ...string) string {
	t.Helper()
	return runGitForTest(t, repo.writer, input, args...)
}

func runExternalGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runGitForTest(t, dir, nil, args...)
}

func runGitForTest(t *testing.T, dir string, input []byte, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if input != nil {
		cmd.Stdin = strings.NewReader(string(input))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func stateWithExtra(id, title string) string {
	return `{
 "lever/acme/123": {
  "first_seen": "2026-08-05T01:02:03.000000004Z",
  "title": "Software Engineer",
  "matched": true,
  "notified": false
 },
 "` + id + `": {
  "first_seen": "2026-08-05T05:06:07Z",
  "title": "` + title + `",
  "matched": false,
  "notified": false
 }
}`
}

func linePresent(text, want string) bool {
	for _, line := range strings.Split(text, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func awaitSignals(t *testing.T, ready <-chan struct{}, count int) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for range count {
		select {
		case <-ready:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d publishers to reach push boundary", count)
		}
	}
}
