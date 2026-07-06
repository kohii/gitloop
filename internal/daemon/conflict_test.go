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
// path: it starts "mid-rebase" with pre-seeded conflict content and records
// which recovery operations get called.
type fakeConflictGit struct {
	conflictedFiles        []string
	contents               map[string]map[int]string // path -> stage -> content
	resetHardCalled        bool
	resetHardRev           string
	rebaseAbortCalled      bool
	addAllCalled           bool
	addedPaths             []string
	committed              []string
	rebaseContinueConflict bool
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
func (f *fakeConflictGit) MergeFF(string) error        { return nil }
func (f *fakeConflictGit) Rebase(string) (bool, error) { return true, nil }
func (f *fakeConflictGit) RebaseContinue() (bool, error) {
	return f.rebaseContinueConflict, nil
}
func (f *fakeConflictGit) RebaseAbort() error { f.rebaseAbortCalled = true; return nil }
func (f *fakeConflictGit) ResetHard(rev string) error {
	f.resetHardCalled = true
	f.resetHardRev = rev
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

// TestResolveConflictsBackupPolicyResetsToUpstream locks in a bug found
// during manual end-to-end testing: aborting the rebase alone leaves the
// branch exactly as diverged as before, so the very next cycle hits the
// identical conflict and backs it up again, forever. resolveConflicts must
// also reset the branch to upstream so the repository reaches a stable
// (non-conflicting) state after one backup.
func TestResolveConflictsBackupPolicyResetsToUpstream(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	git := &fakeConflictGit{
		conflictedFiles: []string{"a.md"},
		contents: map[string]map[int]string{
			"a.md": {2: "ours content\n", 3: "theirs content\n"},
		},
	}

	completed := resolveConflicts(git, dir, "origin/main", config.OnConflictBackup, "test-host", logger)
	if completed {
		t.Fatal("resolveConflicts() = true, want false for the backup policy")
	}
	if !git.rebaseAbortCalled {
		t.Error("expected RebaseAbort to be called")
	}
	if !git.resetHardCalled || git.resetHardRev != "origin/main" {
		t.Errorf("expected ResetHard(\"origin/main\"), got called=%v rev=%q", git.resetHardCalled, git.resetHardRev)
	}
	if !git.addAllCalled {
		t.Error("expected AddAll to stage the backup files")
	}
	if len(git.committed) != 1 || !strings.Contains(git.committed[0], "conflict backup") {
		t.Errorf("committed messages = %v, want exactly one mentioning \"conflict backup\"", git.committed)
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

	completed := resolveConflicts(git, dir, "origin/main", config.OnConflictClaude, "test-host", logger)
	if completed {
		t.Fatal("resolveConflicts() = true, want false (claude unavailable should fall back to backup)")
	}
	if !git.resetHardCalled {
		t.Error("expected the backup fallback to reset to upstream")
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
	completed := resolveConflicts(git, dir, "origin/main", config.OnConflictClaude, "test-host", logger)
	if !completed {
		t.Fatal("resolveConflicts() = false, want true when claude resolves the conflict cleanly")
	}
	if git.resetHardCalled {
		t.Error("did not expect ResetHard when claude resolution succeeds")
	}
	if len(git.addedPaths) != 1 || git.addedPaths[0] != "a.md" {
		t.Errorf("addedPaths = %v, want [a.md]", git.addedPaths)
	}
}
