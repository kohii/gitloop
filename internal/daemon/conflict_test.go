package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// newTestRecorder builds a statusRecorder backed by a status.json under a
// fresh temp dir, for tests that only care about the RepoStatus fields
// resolveConflicts writes and don't need to share a directory with the repo
// under test.
func newTestRecorder(t *testing.T) *statusRecorder {
	t.Helper()
	recorder, err := newStatusRecorder(filepath.Join(t.TempDir(), "status.json"))
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}
	return recorder
}

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

	completed, backup := resolveConflicts(git, dir, "origin/main", config.OnConflictBackup, "test-host", logger, newTestRecorder(t))
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

	completed, backup := resolveConflicts(git, dir, "origin/main", config.OnConflictClaude, "test-host", logger, newTestRecorder(t))
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

	statusPath := filepath.Join(t.TempDir(), "status.json")
	recorder, err := newStatusRecorder(statusPath)
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	git := &fakeConflictGit{conflictedFiles: []string{"a.md"}}
	completed, backup := resolveConflicts(git, dir, "origin/main", config.OnConflictClaude, "test-host", logger, recorder)
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
	if len(git.committed) != 1 ||
		!strings.HasPrefix(git.committed[0], "[ai-resolved] ") ||
		!strings.Contains(git.committed[0], "AI-resolved") {
		t.Errorf("committed messages = %v, want exactly one starting with \"[ai-resolved] \" and mentioning \"AI-resolved\"", git.committed)
	}

	sf, err := LoadStatusFile(statusPath)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	st := sf.Repos[dir]
	if st.LastAIResolveAt.IsZero() {
		t.Error("LastAIResolveAt is zero, want it stamped after a successful AI resolution")
	}
	if st.LastAIResolveError != "" {
		t.Errorf("LastAIResolveError = %q, want empty after a successful AI resolution", st.LastAIResolveError)
	}
}

// TestResolveConflictsWithClaudeSkipsMarkerlessConflictAndBacksUp guards
// against a fatal shape for git-crypt (and any other binary or opaque merge
// driver): git treats the file as binary, leaves no conflict markers in the
// working tree, and records both sides in the index at stages 2 and 3. If we
// were to call claude on that file, there is nothing there for it to
// reconcile; the subsequent `git add` would then silently collapse the index
// stages to whatever the working tree holds — irrevocably dropping theirs.
//
// The safe behavior is to refuse claude for marker-less conflicts and let
// backup preserve both sides. This test asserts that (a) runClaudeOnFile is
// never invoked, (b) the failure is recorded on the recorder, and (c) the
// backup path runs to completion (both sides on disk, incoming side accepted,
// merge commit created).
func TestResolveConflictsWithClaudeSkipsMarkerlessConflictAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	origAvailable := isClaudeAvailable
	isClaudeAvailable = func() bool { return true }
	defer func() { isClaudeAvailable = origAvailable }()

	claudeCalls := 0
	origRun := runClaudeOnFile
	runClaudeOnFile = func(repoPath, file string) error {
		claudeCalls++
		return nil
	}
	defer func() { runClaudeOnFile = origRun }()

	// Simulate git's state after a binary-attribute merge: the working
	// tree holds only "ours" (no <<<<<<< markers), while both sides
	// remain in the index (surfaced here via ShowStage).
	if err := os.WriteFile(filepath.Join(dir, "s.secret"), []byte("ours plaintext\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	statusPath := filepath.Join(t.TempDir(), "status.json")
	recorder, err := newStatusRecorder(statusPath)
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	git := &fakeConflictGit{
		conflictedFiles: []string{"s.secret"},
		contents: map[string]map[int]string{
			"s.secret": {2: "ours plaintext\n", 3: "theirs plaintext\n"},
		},
	}
	completed, backup := resolveConflicts(git, dir, "origin/main", config.OnConflictClaude, "test-host", logger, recorder)
	if !completed || !backup {
		t.Fatalf("resolveConflicts() = (%v, %v), want (true, true): a marker-less conflict must fall back to backup", completed, backup)
	}

	if claudeCalls != 0 {
		t.Errorf("runClaudeOnFile called %d times, want 0: claude must not be invoked on marker-less conflicts", claudeCalls)
	}
	if len(git.checkedOutTheirs) != 1 || git.checkedOutTheirs[0] != "s.secret" {
		t.Errorf("checkedOutTheirs = %v, want [s.secret] via the backup path", git.checkedOutTheirs)
	}
	if !git.addAllCalled {
		t.Error("expected AddAll to stage the backup files")
	}
	if len(git.committed) != 1 || !strings.Contains(git.committed[0], "merged upstream with backups") {
		t.Errorf("committed messages = %v, want exactly one backup-path commit", git.committed)
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

	sf, err := LoadStatusFile(statusPath)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	st := sf.Repos[dir]
	if st.LastAIResolveError == "" {
		t.Error("LastAIResolveError is empty, want the marker-less-conflict reason recorded")
	}
	if !st.LastAIResolveAt.IsZero() {
		t.Error("LastAIResolveAt is set, want zero: claude never succeeded on this conflict")
	}
}

// TestResolveConflictsWithClaudeFailureRecordsStatus verifies that a failed
// AI resolution attempt is recorded in status.json (LastAIResolveError) even
// though the cycle falls back to the backup policy and still completes —
// otherwise a silently and repeatedly failing AI path (e.g. an expired
// ANTHROPIC_API_KEY) would be invisible in `gitloop status`.
func TestResolveConflictsWithClaudeFailureRecordsStatus(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	origAvailable := isClaudeAvailable
	isClaudeAvailable = func() bool { return true }
	defer func() { isClaudeAvailable = origAvailable }()

	origRun := runClaudeOnFile
	runClaudeOnFile = func(repoPath, file string) error {
		return fmt.Errorf("claude: token expired")
	}
	defer func() { runClaudeOnFile = origRun }()

	statusPath := filepath.Join(t.TempDir(), "status.json")
	recorder, err := newStatusRecorder(statusPath)
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	git := &fakeConflictGit{
		conflictedFiles: []string{"a.md"},
		contents: map[string]map[int]string{
			"a.md": {2: "ours\n", 3: "theirs\n"},
		},
	}
	completed, backup := resolveConflicts(git, dir, "origin/main", config.OnConflictClaude, "test-host", logger, recorder)
	if !completed || !backup {
		t.Fatalf("resolveConflicts() = (%v, %v), want (true, true): a failed AI attempt should fall back to the backup policy", completed, backup)
	}

	sf, err := LoadStatusFile(statusPath)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	st := sf.Repos[dir]
	if st.LastAIResolveError == "" {
		t.Error("LastAIResolveError is empty, want a reason recorded after a failed AI resolution attempt")
	}
	if !st.LastAIResolveAt.IsZero() {
		t.Error("LastAIResolveAt is set, want zero: this AI attempt never succeeded")
	}
}
