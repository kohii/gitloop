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
		FetchInterval: 30 * time.Second,
		RemoteTimeout: time.Minute,
		Mode:          ModeSync,
		Workflow:      WorkflowAutoCommitSync,
		Remote:        "origin",
		Branch:        "",
		OnConflict:    OnConflictBackup,
		SaveLockPath:  "",
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
  remote_timeout: 8m
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
	if first.RemoteTimeout != 8*time.Minute {
		t.Errorf("Repositories[0].RemoteTimeout = %v, want 8m from defaults", first.RemoteTimeout)
	}
	if first.ModeWasExplicitlySet() {
		t.Error("Repositories[0].ModeWasExplicitlySet() = true, want false without a mode setting")
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
	if second.ModeWasExplicitlySet() {
		t.Error("Repositories[1].ModeWasExplicitlySet() = true, want false without a mode setting")
	}
}

func TestParseMinimalConfigLeavesModeImplicit(t *testing.T) {
	cfg, err := Parse([]byte("repositories:\n  - path: ~/notes\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Repositories[0].ModeWasExplicitlySet(); got {
		t.Error("Repositories[0].ModeWasExplicitlySet() = true, want false")
	}
}

func TestParseSaveLockPathDefaultsToDisabled(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Repositories[0].SaveLockPath; got != "" {
		t.Errorf("SaveLockPath = %q, want \"\" (disabled by default)", got)
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

func TestParseSaveLockPathExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`
repositories:
  - path: ~/notes
    save_lock_path: ~/.config/gitloop/save.lock
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := filepath.Join(home, ".config", "gitloop", "save.lock")
	if got := cfg.Repositories[0].SaveLockPath; got != want {
		t.Errorf("SaveLockPath = %q, want %q", got, want)
	}
}

func TestParseWorkflowSaveLockPathExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`
repositories:
  - path: ~/dotfiles
    workflow:
      type: committed-sync
      save_lock_path: ~/.config/gitloop/save.lock
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := filepath.Join(home, ".config", "gitloop", "save.lock")
	if got := cfg.Repositories[0].SaveLockPath; got != want {
		t.Errorf("SaveLockPath = %q, want %q", got, want)
	}
}

func TestParseWorkflowEmptySaveLockPathDisablesIt(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/dotfiles
    workflow:
      type: committed-sync
      save_lock_path: ""
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Repositories[0].SaveLockPath; got != "" {
		t.Errorf("SaveLockPath = %q, want empty string (disabled)", got)
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

func TestParseModePerRepoAndFromDefaultsBlock(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/notes
  - path: ~/journal
    mode: sync
defaults:
  mode: commit-only
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Repositories[0].Mode; got != ModeCommitOnly {
		t.Errorf("Repositories[0].Mode = %q, want %q (from defaults block)", got, ModeCommitOnly)
	}
	if !cfg.Repositories[0].ModeWasExplicitlySet() {
		t.Error("Repositories[0].ModeWasExplicitlySet() = false, want true from defaults.mode")
	}
	if got := cfg.Repositories[1].Mode; got != ModeSync {
		t.Errorf("Repositories[1].Mode = %q, want %q (per-repo override wins)", got, ModeSync)
	}
	if !cfg.Repositories[1].ModeWasExplicitlySet() {
		t.Error("Repositories[1].ModeWasExplicitlySet() = false, want true from per-repository mode")
	}
}

func TestParseLegacyCommitOnlyAllowsIgnoredRemoteSettings(t *testing.T) {
	cfg, err := Parse([]byte(`
repositories:
  - path: ~/notes
    mode: commit-only
    remote: origin
    branch: main
    on_conflict: claude
    remote_timeout: 1m
`))
	if err != nil {
		t.Fatalf("Parse legacy commit-only settings: %v", err)
	}
	if got := cfg.Repositories[0].RemoteTimeout; got != time.Minute {
		t.Errorf("RemoteTimeout = %v, want parsed legacy value 1m", got)
	}
}

func TestParseRejectsUnknownMode(t *testing.T) {
	cases := map[string]string{
		"per repo": "repositories:\n  - path: ~/notes\n    mode: offline\n",
		"defaults": "repositories:\n  - path: ~/notes\ndefaults:\n  mode: offline\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Fatal("Parse with unknown mode: want error, got nil")
			}
		})
	}
}

func TestParseCommittedSyncWorkflow(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/dotfiles
    workflow:
      type: committed-sync
      remote: origin
      branch: main
      interval: 1m
      remote_timeout: 4m
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	repo := cfg.Repositories[0]
	if repo.Workflow != WorkflowCommittedSync || repo.Mode != ModeCommittedSync {
		t.Fatalf("workflow = (%q, mode %q), want committed-sync", repo.Workflow, repo.Mode)
	}
	if !repo.WorkflowWasExplicitlySet() {
		t.Error("WorkflowWasExplicitlySet() = false, want true for nested workflow")
	}
	if repo.Remote != "origin" || repo.Branch != "main" || repo.FetchInterval != time.Minute || repo.RemoteTimeout != 4*time.Minute {
		t.Errorf("repository overrides = %+v, want origin/main/1m/4m", repo)
	}
	if repo.AutoCommits() {
		t.Error("committed-sync AutoCommits() = true, want false")
	}
	if !repo.SyncsRemote() {
		t.Error("committed-sync SyncsRemote() = false, want true")
	}
}

func TestParseWorkflowRejectsInvalidCombinations(t *testing.T) {
	cases := map[string]string{
		"unknown type": `repositories:
  - path: ~/dotfiles
    workflow:
      type: no-such-workflow
`,
		"mode conflict": `repositories:
  - path: ~/dotfiles
    mode: sync
    workflow:
      type: committed-sync
`,
		"committed auto-commit setting": `repositories:
  - path: ~/dotfiles
    settle: 1s
    workflow:
      type: committed-sync
`,
		"auto-commit-only remote": `repositories:
  - path: ~/journal
    workflow:
      type: auto-commit-only
      remote: origin
`,
		"auto-commit-only remote timeout": `repositories:
  - path: ~/journal
    workflow:
      type: auto-commit-only
      remote_timeout: 1m
`,
		"duplicate remote timeout": `repositories:
  - path: ~/notes
    remote_timeout: 2m
    workflow:
      type: auto-commit-sync
      remote_timeout: 1m
`,
		"duplicate conflict policy": `repositories:
  - path: ~/notes
    on_conflict: claude
    workflow:
      type: auto-commit-sync
      on_conflict: backup
`,
		"legacy committed-sync auto-commit setting": `repositories:
  - path: ~/dotfiles
    mode: committed-sync
    max_wait: 1m
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Fatal("Parse: want error, got nil")
			}
		})
	}
}

func TestParseRejectsUnknownWorkflowField(t *testing.T) {
	yaml := []byte(`
repositories:
  - path: ~/dotfiles
    workflow:
      type: committed-sync
      intervl: 1m
`)
	if _, err := Parse(yaml); err == nil {
		t.Fatal("Parse with unknown workflow field: want error, got nil")
	}
}

func TestSyncsRemote(t *testing.T) {
	cases := map[Mode]bool{
		ModeSync:          true,
		ModeCommitOnly:    false,
		ModeCommittedSync: true,
		// A hand-built Repository that never set Mode still syncs, so a
		// forgotten field can't silently turn someone's sync into local
		// commits.
		Mode(""): true,
	}
	for mode, want := range cases {
		if got := (Repository{Mode: mode}).SyncsRemote(); got != want {
			t.Errorf("Repository{Mode: %q}.SyncsRemote() = %v, want %v", mode, got, want)
		}
	}
}

// TestParseRejectsNonPositiveDurations guards the timers these values feed:
// time.NewTicker panics on a non-positive interval, which the daemon would
// recover into an endlessly retried failure with that repository never
// actually watched. A startup error is the visible failure instead.
func TestParseRejectsNonPositiveDurations(t *testing.T) {
	for _, field := range []string{"settle", "max_wait", "fetch_interval", "remote_timeout"} {
		for _, value := range []string{"0s", "-1s"} {
			t.Run(field+"="+value, func(t *testing.T) {
				yaml := []byte("repositories:\n  - path: ~/notes\n    " + field + ": " + value + "\n")
				if _, err := Parse(yaml); err == nil {
					t.Fatalf("Parse with %s: %s: want error, got nil", field, value)
				}
			})
		}
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
