package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kohii/gitloop/internal/config"
	"github.com/kohii/gitloop/internal/gitcmd"
)

// fakeGit is a minimal GitClient double used to exercise the watch loop's
// debounce behavior without a real git checkout. It reports "dirty" once
// per markDirty call and counts commits.
type fakeGit struct {
	mu      sync.Mutex
	dirty   bool
	commits int
}

func (f *fakeGit) markDirty() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty = true
}

func (f *fakeGit) commitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commits
}

func (f *fakeGit) StatusPorcelain() ([]gitcmd.StatusEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirty {
		return []gitcmd.StatusEntry{{X: '?', Y: '?', Path: "a.md"}}, nil
	}
	return nil, nil
}

func (f *fakeGit) AddAll() error        { return nil }
func (f *fakeGit) AddPath(string) error { return nil }

func (f *fakeGit) Commit(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits++
	f.dirty = false
	return nil
}

func (f *fakeGit) Fetch(string) error                                     { return nil }
func (f *fakeGit) CurrentBranch() (string, error)                         { return "main", nil }
func (f *fakeGit) RevListLeftRightCount(string, string) (int, int, error) { return 0, 0, nil }
func (f *fakeGit) MergeFF(string) error                                   { return nil }
func (f *fakeGit) Merge(string) (bool, error)                             { return false, nil }
func (f *fakeGit) MergeAbort() error                                      { return nil }
func (f *fakeGit) CheckoutTheirs(string) error                            { return nil }
func (f *fakeGit) Push(string, string) error                              { return nil }
func (f *fakeGit) ConflictedFiles() ([]string, error)                     { return nil, nil }
func (f *fakeGit) ShowStage(int, string) (string, bool, error)            { return "", false, nil }

var _ GitClient = (*fakeGit)(nil)

func TestRunRepoLoopDebouncesRapidChangesIntoOneCommit(t *testing.T) {
	dir := t.TempDir()

	repo := config.Repository{
		Path:          dir,
		Settle:        50 * time.Millisecond,
		MaxWait:       2 * time.Second,
		FetchInterval: time.Hour, // effectively disabled for this test
		Remote:        "origin",
		Branch:        "main",
		OnConflict:    config.OnConflictBackup,
	}

	git := &fakeGit{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder, err := newStatusRecorder(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runRepoLoop(ctx, git, repo, "test-host", logger, recorder) }()

	// Give the watcher goroutine time to start and register its watches.
	time.Sleep(100 * time.Millisecond)

	// Rapid-fire several writes within the settle window; they must
	// collapse into a single commit rather than one per write.
	for i := 0; i < 5; i++ {
		git.markDirty()
		if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(fmt.Sprintf("v%d", i)), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	waitFor(t, 2*time.Second, func() bool { return git.commitCount() >= 1 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRepoLoop returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down within 2s of context cancellation")
	}

	if got := git.commitCount(); got != 1 {
		t.Errorf("commit count = %d, want 1 (rapid writes within the settle window should debounce)", got)
	}
}

func TestRunRepoLoopSetsIdlePhaseAfterASuccessfulCycle(t *testing.T) {
	dir := t.TempDir()

	repo := config.Repository{
		Path:          dir,
		Settle:        50 * time.Millisecond,
		MaxWait:       2 * time.Second,
		FetchInterval: time.Hour,
		Remote:        "origin",
		Branch:        "main",
		OnConflict:    config.OnConflictBackup,
	}

	git := &fakeGit{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	statusPath := filepath.Join(dir, "status.json")
	recorder, err := newStatusRecorder(statusPath)
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runRepoLoop(ctx, git, repo, "test-host", logger, recorder) }()

	time.Sleep(100 * time.Millisecond)
	git.markDirty()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("v0"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return git.commitCount() >= 1 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down within 2s of context cancellation")
	}

	sf, err := LoadStatusFile(statusPath)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	if got := sf.Repos[dir].Phase; got != PhaseIdle {
		t.Errorf("Phase after a clean cycle = %q, want %q", got, PhaseIdle)
	}
	if sf.Repos[dir].LastSuccessfulSyncAt.IsZero() {
		t.Error("LastSuccessfulSyncAt is zero, want it stamped after a cycle with no error")
	}
}

func TestRunRepoLoopSkipsCycleWhileSaveLockIsHeldElsewhere(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "save.lock")
	holder := openAndFlock(t, lockPath)
	defer func() { holder.Close() }()

	repo := config.Repository{
		Path:          dir,
		Settle:        10 * time.Millisecond,
		MaxWait:       2 * time.Second,
		FetchInterval: time.Hour,
		Remote:        "origin",
		Branch:        "main",
		OnConflict:    config.OnConflictBackup,
		SaveLockPath:  lockPath,
	}

	git := &fakeGit{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	statusPath := filepath.Join(dir, "status.json")
	recorder, err := newStatusRecorder(statusPath)
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runRepoLoop(ctx, git, repo, "test-host", logger, recorder) }()

	time.Sleep(50 * time.Millisecond)
	git.markDirty()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("v0"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		sf, err := LoadStatusFile(statusPath)
		return err == nil && sf.Repos[dir].LastError == "skipped: save in-flight"
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down within 2s of context cancellation")
	}

	if got := git.commitCount(); got != 0 {
		t.Errorf("commit count = %d, want 0: the cycle should have been skipped while the save lock was held", got)
	}
}

// callbackFakeGit wraps fakeGit and lets a test observe the moment fetch is
// called. It exists so a test can assert what the surrounding save-lock state
// looks like *during* fetch, without racing on wall-clock timing.
type callbackFakeGit struct {
	*fakeGit
	onFetch func()
}

func (f *callbackFakeGit) Fetch(remote string) error {
	if f.onFetch != nil {
		f.onFetch()
	}
	return f.fakeGit.Fetch(remote)
}

// TestRunRepoLoopDoesNotHoldSaveLockDuringFetch pins the "fetch is outside
// the save lock" invariant. If a future refactor slid fetch back inside the
// lock window, tryAcquireSaveLock here would observe the lock as held and
// this test would fail — the whole point of narrowing the lock is that
// external writers aren't blocked on gitloop's network round-trip.
func TestRunRepoLoopDoesNotHoldSaveLockDuringFetch(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "save.lock")

	observed := make(chan bool, 8)
	git := &callbackFakeGit{
		fakeGit: &fakeGit{},
		onFetch: func() {
			l, ok, err := tryAcquireSaveLock(lockPath)
			free := err == nil && ok
			if ok {
				l.release()
			}
			select {
			case observed <- free:
			default:
			}
		},
	}

	repo := config.Repository{
		Path:          dir,
		Settle:        20 * time.Millisecond,
		MaxWait:       2 * time.Second,
		FetchInterval: time.Hour,
		Remote:        "origin",
		Branch:        "main",
		OnConflict:    config.OnConflictBackup,
		SaveLockPath:  lockPath,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder, err := newStatusRecorder(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runRepoLoop(ctx, git, repo, "test-host", logger, recorder) }()

	time.Sleep(50 * time.Millisecond)
	git.fakeGit.markDirty()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("v0"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	select {
	case free := <-observed:
		if !free {
			t.Fatal("save lock was held while gitloop was in fetch; it should only be held for the commit/merge phase")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fetch never ran within timeout")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down within 2s of context cancellation")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met before timeout")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
