package daemon

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/kohii/gitloop/internal/config"
	"github.com/kohii/gitloop/internal/gitcmd"
)

// fakeConflictGit is a GitClient double focused on the conflict-resolution
// path: it starts "mid-merge" with pre-seeded conflict content and records
// which recovery operations get called.
type fakeConflictGit struct {
	conflictedFiles  []string
	contents         map[string]map[int]string // path -> stage -> content
	mergeAbortCalled bool
	checkedOutTheirs []string
	addAllCalled     bool
	addedPaths       []string
	committed        []string
}

func (f *fakeConflictGit) StatusPorcelain() ([]gitcmd.StatusEntry, error) { return nil, nil }
func (f *fakeConflictGit) AddAll() error                                  { f.addAllCalled = true; return nil }
func (f *fakeConflictGit) AddPath(p string) error                         { f.addedPaths = append(f.addedPaths, p); return nil }
func (f *fakeConflictGit) Commit(msg string) error {
	f.committed = append(f.committed, msg)
	return nil
}
func (f *fakeConflictGit) Fetch(string) error             { return nil }
func (f *fakeConflictGit) CurrentBranch() (string, error) { return "main", nil }
func (f *fakeConflictGit) RevListLeftRightCount(string, string) (int, int, error) {
	return 0, 0, nil
}
func (f *fakeConflictGit) MergeFF(string) error       { return nil }
func (f *fakeConflictGit) Merge(string) (bool, error) { return true, nil }
func (f *fakeConflictGit) MergeAbort() error          { f.mergeAbortCalled = true; return nil }
func (f *fakeConflictGit) CheckoutTheirs(p string) error {
	f.checkedOutTheirs = append(f.checkedOutTheirs, p)
	return nil
}
func (f *fakeConflictGit) Push(string, string) error          { return nil }
func (f *fakeConflictGit) ConflictedFiles() ([]string, error) { return f.conflictedFiles, nil }
func (f *fakeConflictGit) ShowStage(stage int, path string) (string, bool, error) {
	m, ok := f.contents[path]
	if !ok {
		return "", false, nil
	}
	c, ok := m[stage]
	return c, ok, nil
}

var _ GitClient = (*fakeConflictGit)(nil)

// TestResolveConflictsBackupPolicyAcceptsTheirs verifies the backup policy's
// end state: it must not leave the repository re-diverged (which would hit
// the identical conflict again on the very next cycle), so it accepts the
// incoming (upstream) side for each conflicted file — after backing up both
// sides — and completes the merge with a commit.
func TestResolveConflictsBackupPolicyAcceptsTheirs(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	git := &fakeConflictGit{
		conflictedFiles: []string{"a.md"},
		contents: map[string]map[int]string{
			"a.md": {2: "ours content\n", 3: "theirs content\n"},
		},
	}

	completed, backup := resolveConflicts(git, dir, "origin/main", config.OnConflictBackup, "test-host", logger)
	if !completed {
		t.Fatal("resolveConflicts() completed = false, want true for the backup policy")
	}
	if !backup {
		t.Error("resolveConflicts() backup = false, want true for the backup policy")
	}
	if git.mergeAbortCalled {
		t.Error("did not expect MergeAbort to be called on a successful backup resolution")
	}
	if len(git.checkedOutTheirs) != 1 || git.checkedOutTheirs[0] != "a.md" {
		t.Errorf("checkedOutTheirs = %v, want [a.md]", git.checkedOutTheirs)
	}
	if !git.addAllCalled {
		t.Error("expected AddAll to stage the backup files")
	}
	if len(git.committed) != 1 || !strings.Contains(git.committed[0], "merged upstream with backups") {
		t.Errorf("committed messages = %v, want exactly one mentioning \"merged upstream with backups\"", git.committed)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var foundOurs, foundTheirs bool
	for _, e := range entries {
		if strings.Contains(e.Name(), ".ours.") {
			foundOurs = true
		}
		if strings.Contains(e.Name(), ".theirs.") {
			foundTheirs = true
		}
	}
	if !foundOurs || !foundTheirs {
		t.Errorf("expected both .ours. and .theirs. backup files in %s, got %v", dir, entries)
	}
}

func TestResolveConflictsFallsBackToBackupWhenClaudeUnavailable(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	orig := isClaudeAvailable
	isClaudeAvailable = func() bool { return false }
	defer func() { isClaudeAvailable = orig }()

	git := &fakeConflictGit{
		conflictedFiles: []string{"a.md"},
		contents: map[string]map[int]string{
			"a.md": {2: "ours\n", 3: "theirs\n"},
		},
	}

	completed, backup := resolveConflicts(git, dir, "origin/main", config.OnConflictClaude, "test-host", logger)
	if !completed {
		t.Fatal("resolveConflicts() completed = false, want true (claude unavailable should fall back to backup)")
	}
	if !backup {
		t.Error("expected the claude-unavailable fallback to go through the backup policy")
	}
}

func TestResolveConflictsWithClaudeSucceeds(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	origAvailable := isClaudeAvailable
	isClaudeAvailable = func() bool { return true }
	defer func() { isClaudeAvailable = origAvailable }()

	origRun := runClaudeOnFile
	runClaudeOnFile = func(repoPath, file string) error {
		return os.WriteFile(repoPath+"/"+file, []byte("resolved content\n"), 0o644)
	}
	defer func() { runClaudeOnFile = origRun }()

	if err := os.WriteFile(dir+"/a.md", []byte("<<<<<<< placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	git := &fakeConflictGit{conflictedFiles: []string{"a.md"}}
	completed, backup := resolveConflicts(git, dir, "origin/main", config.OnConflictClaude, "test-host", logger)
	if !completed {
		t.Fatal("resolveConflicts() completed = false, want true when claude resolves the conflict cleanly")
	}
	if backup {
		t.Error("did not expect the backup policy to run when claude resolution succeeds")
	}
	if git.mergeAbortCalled {
		t.Error("did not expect MergeAbort when claude resolution succeeds")
	}
	if len(git.addedPaths) != 1 || git.addedPaths[0] != "a.md" {
		t.Errorf("addedPaths = %v, want [a.md]", git.addedPaths)
	}
	if len(git.committed) != 1 || !strings.Contains(git.committed[0], "claude-resolved") {
		t.Errorf("committed messages = %v, want exactly one mentioning \"claude-resolved\"", git.committed)
	}
}
