package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDetailedErrorsUseHumanOutputWriter(t *testing.T) {
	var out bytes.Buffer
	writeErrorDetail(&out, "run", errors.New("GET https://private.example?token=secret failed"))
	if got := out.String(); !strings.Contains(got, "private.example?token=secret") {
		t.Fatalf("local detail missing: %q", got)
	}
	writeErrorDetail(nil, "run", errors.New("ignored"))
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
