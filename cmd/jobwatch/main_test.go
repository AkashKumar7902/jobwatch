package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"jobwatch/internal/match"
)

func TestDetailedErrorsUseHumanOutputWriter(t *testing.T) {
	var out bytes.Buffer
	writeErrorDetail(&out, "run", errors.New("GET https://private.example?token=secret failed"))
	if got := out.String(); !strings.Contains(got, "private.example?token=secret") {
		t.Fatalf("local detail missing: %q", got)
	}
	writeErrorDetail(nil, "run", errors.New("ignored"))
}

func TestLLMPreflightCLIIsSealedSingleRequestAndStateFree(t *testing.T) {
	var requests atomic.Int32
	const secret = "preflight-super-secret"
	const providerDetail = "private provider body echoing prompt and preflight-super-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, providerDetail, http.StatusUnauthorized)
	}))
	defer srv.Close()

	temp := t.TempDir()
	statePath := filepath.Join(temp, "must-not-exist.json")
	configPath := filepath.Join(temp, "config.yaml")
	configText := fmt.Sprintf(`matcher:
  name: llm
  params:
    profile: private candidate profile
    base_url: %s
    model: test-model
    api_key_env: JOBWATCH_PREFLIGHT_CLI_KEY
companies:
  - {name: Must Not Fetch, source: greenhouse, params: {board_token: must-not-fetch}}
notifiers:
  - {name: must-not-construct, params: {sentinel: must-not-read}}
store: {path: %q}
`, srv.URL, statePath)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	binary := filepath.Join(temp, "jobwatch")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", binary, ".")
	if output, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("building CLI: %v\n%s", err, output)
	}
	cmd := exec.Command(binary, "-config", configPath, "-llm-preflight")
	cmd.Env = append(os.Environ(), "JOBWATCH_PREFLIGHT_CLI_KEY="+secret)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("CLI error = %v, want exit 1", err)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("preflight stdout = %q, want empty", got)
	}
	wantStderr := "LLM_PREFLIGHT status=failed category=unauthorized http_status=401 provider_status=unauthenticated\n"
	if got := stderr.String(); got != wantStderr {
		t.Fatalf("preflight stderr = %q, want %q", got, wantStderr)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want exactly 1", got)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("preflight touched state path: %v", err)
	}
	for _, private := range []string{
		secret, providerDetail, srv.URL, configPath, "private candidate profile",
		"test-model", "Must Not Fetch", "must-not-construct",
	} {
		if strings.Contains(stdout.String()+stderr.String(), private) {
			t.Fatalf("preflight output leaked private text %q", private)
		}
	}
}

func TestRunLLMPreflightOutputIsSealed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "private-config-name.yaml")
	got := runLLMPreflight(context.Background(), missing)
	if got.CategoryToken() != "configuration" || got.HTTPStatus() != 0 || got.ProviderStatusToken() != "none" {
		t.Fatalf("runLLMPreflight result = (%s, %d, %s)", got.CategoryToken(), got.HTTPStatus(), got.ProviderStatusToken())
	}
	var output bytes.Buffer
	writeLLMPreflightResult(&output, got)
	if want := "LLM_PREFLIGHT status=failed category=configuration http_status=0 provider_status=none\n"; output.String() != want {
		t.Fatalf("sealed setup output = %q, want %q", output.String(), want)
	}

	output.Reset()
	writeLLMPreflightResult(&output, match.LLMPreflightResult{})
	if want := "LLM_PREFLIGHT status=failed category=unknown http_status=0 provider_status=none\n"; output.String() != want {
		t.Fatalf("zero-value output = %q, want %q", output.String(), want)
	}

	output.Reset()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"match\":false,\"reason\":\"valid synthetic verdict\"}"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("JOBWATCH_PREFLIGHT_OUTPUT_KEY", "private-success-key")
	configPath := filepath.Join(t.TempDir(), "success-config.yaml")
	configText := fmt.Sprintf(`matcher:
  name: llm
  params:
    profile: private candidate profile
    base_url: %s
    model: test-model
    api_key_env: JOBWATCH_PREFLIGHT_OUTPUT_KEY
companies:
  - {name: Never Fetched, source: greenhouse, params: {board_token: never-fetched}}
`, srv.URL)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLLMPreflightResult(&output, runLLMPreflight(context.Background(), configPath))
	if want := "LLM_PREFLIGHT status=ok category=none http_status=200 provider_status=none\n"; output.String() != want {
		t.Fatalf("success output = %q, want %q", output.String(), want)
	}
}

func TestBuildNoReporterWarningHasPositiveOccurrenceCount(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := `companies:
  - {name: Acme, source: greenhouse, params: {board_token: acme}}
notifiers:
  - {name: webhook, params: {url: "https://example.com"}}
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	runner, err := build(configPath, filepath.Join(t.TempDir(), "state.json"), log.New(&logs, "", 0), false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Store.Close()
	if !strings.Contains(logs.String(), "WARN scope=run index=0 step=report code=no_reporter count=1") ||
		strings.Contains(logs.String(), "code=no_reporter count=0") {
		t.Fatalf("no-reporter warning is not a positive occurrence:\n%s", logs.String())
	}
}

func TestCLIConfigurationFailureSeparatesHumanAndProtocolOutput(t *testing.T) {
	temp := t.TempDir()
	binary := filepath.Join(temp, "jobwatch")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", binary, ".")
	if output, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("building CLI: %v\n%s", err, output)
	}
	missing := filepath.Join(temp, "missing-config.yaml")
	cmd := exec.Command(binary, "-config", missing)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("CLI error = %v, want exit 1", err)
	}
	if !strings.Contains(stdout.String(), missing) {
		t.Fatalf("human stdout lost configuration detail: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "STARTUP status=failed problem=configuration") || strings.Contains(stderr.String(), missing) {
		t.Fatalf("protocol stderr is wrong: %q", stderr.String())
	}
}

func TestBuildRejectsDuplicateATSBoards(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := `companies:
  - {name: First, source: greenhouse, params: {board_token: acme}}
  - {name: Renamed, source: greenhouse, params: {board_token: acme}}
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := build(
		configPath,
		filepath.Join(t.TempDir(), "state.json"),
		log.New(io.Discard, "", 0),
		false,
		false,
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "duplicates ATS board") {
		t.Fatalf("build error = %v, want duplicate-board error", err)
	}
}

// previous_state_prefix is the one config line that MOVES stored records rather
// than describing how to fetch new ones, and the user pastes it by hand out of
// an email. A typo in it is executed as a bare prefix swap over every key, so
// the checks below are the difference between reuniting one board with its
// history and quietly handing another board's history away.
func TestBuildValidatesPreviousStatePrefix(t *testing.T) {
	tests := []struct {
		name    string
		entries string
		wantErr string
	}{
		{
			name: "adopts an orphaned namespace",
			entries: `  - {name: Acme, source: greenhouse, params: {board_token: acme}, previous_state_prefix: "greenhouse/acme-old/"}
  - {name: Other, source: greenhouse, params: {board_token: other}}`,
		},
		{
			name:    "its own current prefix is already where the postings are",
			entries: `  - {name: Acme, source: greenhouse, params: {board_token: acme}, previous_state_prefix: "greenhouse/acme/"}`,
			wantErr: "this board's own current state prefix",
		},
		{
			// The catastrophic one: a live board's whole history is dragged onto
			// another board's keys, and the state branch accepts it, because a
			// declared move is exactly what authorizes those removals.
			name: "another live board's prefix",
			entries: `  - {name: Acme, source: greenhouse, params: {board_token: acme}, previous_state_prefix: "greenhouse/other/"}
  - {name: Other, source: greenhouse, params: {board_token: other}}`,
			wantErr: `overlaps the live state prefix "greenhouse/other/" of "Other"`,
		},
		{
			// A truncated paste: "greenhouse/" contains every greenhouse board.
			name: "a namespace containing live boards",
			entries: `  - {name: Acme, source: greenhouse, params: {board_token: acme}, previous_state_prefix: "greenhouse/"}
  - {name: Other, source: greenhouse, params: {board_token: other}}`,
			wantErr: "overlaps the live state prefix",
		},
		{
			// One history, two claimants, no way to split it. The state branch
			// refuses the resulting removals; saying so at startup beats
			// discovering it after the run.
			name: "two boards claiming the same history",
			entries: `  - {name: Acme, source: greenhouse, params: {board_token: acme}, previous_state_prefix: "greenhouse/legacy/"}
  - {name: Other, source: greenhouse, params: {board_token: other}, previous_state_prefix: "greenhouse/legacy/"}`,
			wantErr: `both claim previous_state_prefix "greenhouse/legacy/"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("companies:\n"+tc.entries+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runner, err := build(configPath, filepath.Join(t.TempDir(), "state.json"),
				log.New(io.Discard, "", 0), false, false, true)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("build error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer runner.Store.Close()
			want := map[string]string{"greenhouse/us/acme": "greenhouse/acme-old/"}
			if len(runner.PreviousStatePrefixes) != len(want) {
				t.Fatalf("PreviousStatePrefixes = %v, want %v", runner.PreviousStatePrefixes, want)
			}
			for identity, prefix := range want {
				if runner.PreviousStatePrefixes[identity] != prefix {
					t.Fatalf("PreviousStatePrefixes = %v, want %v", runner.PreviousStatePrefixes, want)
				}
			}
		})
	}
}

// The escape hatch must stay invisible until it is used: a config without the
// key must not acquire an empty claim that the runner would then try to apply.
func TestBuildWithoutPreviousStatePrefixDeclaresNoMoves(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath,
		[]byte("companies:\n  - {name: Acme, source: greenhouse, params: {board_token: acme}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner, err := build(configPath, filepath.Join(t.TempDir(), "state.json"),
		log.New(io.Discard, "", 0), false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Store.Close()
	if len(runner.PreviousStatePrefixes) != 0 {
		t.Fatalf("PreviousStatePrefixes = %v, want none", runner.PreviousStatePrefixes)
	}
}
