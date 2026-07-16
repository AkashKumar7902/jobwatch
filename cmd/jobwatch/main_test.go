package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
