package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kohii/gitloop/internal/daemon"
)

func TestStatusCmdPrintsTableForEachConfiguredRepo(t *testing.T) {
	dir := t.TempDir()

	configPath := filepath.Join(dir, "config.yaml")
	yaml := "repositories:\n  - path: " + filepath.Join(dir, "synced") + "\n  - path: " + filepath.Join(dir, "never-synced") + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	statusPath := filepath.Join(dir, "status.json")
	sf := &daemon.StatusFile{Repos: map[string]daemon.RepoStatus{
		filepath.Join(dir, "synced"): {
			Path:       filepath.Join(dir, "synced"),
			LastCommit: "2026-07-07T15:30:00Z",
			LastPush:   "2026-07-07T15:30:05Z",
		},
	}}
	if err := sf.Save(statusPath); err != nil {
		t.Fatal(err)
	}

	origStatusPathFunc := statusPathFunc
	statusPathFunc = func() (string, error) { return statusPath, nil }
	defer func() { statusPathFunc = origStatusPathFunc }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, filepath.Join(dir, "synced")) || !strings.Contains(out, "2026-07-07T15:30:00Z") {
		t.Errorf("stdout missing synced repo details:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(dir, "never-synced")) || !strings.Contains(out, "not yet synced") {
		t.Errorf("stdout missing never-synced repo placeholder:\n%s", out)
	}
	if !strings.Contains(out, "LAST_AI_RESOLVE") {
		t.Errorf("stdout missing LAST_AI_RESOLVE column header:\n%s", out)
	}
}

func TestStatusCmdShowsLastAIResolveOutcome(t *testing.T) {
	dir := t.TempDir()

	configPath := filepath.Join(dir, "config.yaml")
	yaml := "repositories:\n  - path: " + filepath.Join(dir, "resolved") + "\n  - path: " + filepath.Join(dir, "failing") + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	statusPath := filepath.Join(dir, "status.json")
	sf := &daemon.StatusFile{Repos: map[string]daemon.RepoStatus{
		filepath.Join(dir, "resolved"): {
			Path:            filepath.Join(dir, "resolved"),
			LastAIResolveAt: time.Date(2026, 7, 7, 15, 30, 0, 0, time.UTC),
		},
		filepath.Join(dir, "failing"): {
			Path:               filepath.Join(dir, "failing"),
			LastAIResolveError: "claude: token expired",
		},
	}}
	if err := sf.Save(statusPath); err != nil {
		t.Fatal(err)
	}

	origStatusPathFunc := statusPathFunc
	statusPathFunc = func() (string, error) { return statusPath, nil }
	defer func() { statusPathFunc = origStatusPathFunc }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "2026-07-07T15:30:00Z") {
		t.Errorf("stdout missing LastAIResolveAt for the resolved repo:\n%s", out)
	}
	if !strings.Contains(out, "error: claude: token expired") {
		t.Errorf("stdout missing LastAIResolveError for the failing repo:\n%s", out)
	}
}
