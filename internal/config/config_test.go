package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseMinimalConfigUsesDefaults(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Repositories) != 1 {
		t.Fatalf("len(Repositories) = %d, want 1", len(cfg.Repositories))
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := Repository{
		Path:          filepath.Join(home, "notes"),
		Settle:        3 * time.Second,
		MaxWait:       60 * time.Second,
		FetchInterval: 5 * time.Minute,
		Remote:        "origin",
		Branch:        "",
		OnConflict:    OnConflictClaude,
		SaveLockPath:  filepath.Join(home, "notes", ".notesapp", "state", "save.lock"),
	}
	if got := cfg.Repositories[0]; got != want {
		t.Errorf("Repositories[0] = %+v, want %+v", got, want)
	}
}

func TestParsePerRepoOverridesAndDefaultsSection(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
  - path: ~/dev/journal
    settle: 5s
    on_conflict: backup
defaults:
  settle: 3s
  max_wait: 60s
  fetch_interval: 5m
  remote: origin
  branch: ""
  on_conflict: claude
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Repositories) != 2 {
		t.Fatalf("len(Repositories) = %d, want 2", len(cfg.Repositories))
	}

	first := cfg.Repositories[0]
	if first.Settle != 3*time.Second || first.OnConflict != OnConflictClaude {
		t.Errorf("Repositories[0] = %+v, want defaults applied", first)
	}

	second := cfg.Repositories[1]
	if second.Settle != 5*time.Second {
		t.Errorf("Repositories[1].Settle = %v, want 5s", second.Settle)
	}
	if second.OnConflict != OnConflictBackup {
		t.Errorf("Repositories[1].OnConflict = %v, want backup", second.OnConflict)
	}
	if second.MaxWait != 60*time.Second {
		t.Errorf("Repositories[1].MaxWait = %v, want 60s (from defaults)", second.MaxWait)
	}
}

func TestParseSaveLockPathDefaultsToDotNotesappUnderRepoPath(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Repositories[0].SaveLockPath
	want := filepath.Join(cfg.Repositories[0].Path, ".notesapp", "state", "save.lock")
	if got != want {
		t.Errorf("SaveLockPath = %q, want %q", got, want)
	}
}

func TestParseSaveLockPathCanBeOverriddenPerRepo(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
    save_lock_path: /tmp/custom.lock
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Repositories[0].SaveLockPath; got != "/tmp/custom.lock" {
		t.Errorf("SaveLockPath = %q, want /tmp/custom.lock", got)
	}
}

func TestParseSaveLockPathEmptyStringDisablesIt(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
    save_lock_path: ""
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Repositories[0].SaveLockPath; got != "" {
		t.Errorf("SaveLockPath = %q, want \"\" (disabled)", got)
	}
}

func TestParseSaveLockPathFromDefaultsBlock(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
defaults:
  save_lock_path: ""
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Repositories[0].SaveLockPath; got != "" {
		t.Errorf("SaveLockPath = %q, want \"\" (disabled via defaults block)", got)
	}
}

func TestParseRequiresAtLeastOneRepository(t *testing.T) {
	if _, err := Parse([]byte(`repositories: []`)); err == nil {
		t.Fatal("Parse with no repositories: want error, got nil")
	}
}

func TestParseRequiresPath(t *testing.T) {
	if _, err := Parse([]byte("repositories:\n  - settle: 3s\n")); err == nil {
		t.Fatal("Parse with missing path: want error, got nil")
	}
}

func TestParseRejectsUnknownOnConflict(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
    on_conflict: yolo
`)
	if _, err := Parse(yaml); err == nil {
		t.Fatal("Parse with unknown on_conflict: want error, got nil")
	}
}

func TestParseRejectsBadDuration(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
    settle: "not-a-duration"
`)
	if _, err := Parse(yaml); err == nil {
		t.Fatal("Parse with invalid duration: want error, got nil")
	}
}

func TestLoadReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("repositories:\n  - path: ~/notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Repositories) != 1 {
		t.Fatalf("len(Repositories) = %d, want 1", len(cfg.Repositories))
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/notes", filepath.Join(home, "notes")},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
	}
	for _, c := range cases {
		got, err := expandHome(c.in)
		if err != nil {
			t.Fatalf("expandHome(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
