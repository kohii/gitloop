package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
		Mode:          config.ModeSync,
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
		Mode:          config.ModeSync,
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

// noRemoteFakeGit stands in for a repository with no remote at all: every
// call that would talk to one is recorded and fails the way real git does,
// so a leaked call shows up twice over — in ops() and, because the error
// propagates, in the status file's LastError.
type noRemoteFakeGit struct {
	*fakeGit
	opsMu     sync.Mutex
	remoteOps []string
}

var errNoRemote = fmt.Errorf("'origin' does not appear to be a git repository")

func (f *noRemoteFakeGit) record(op string) {
	f.opsMu.Lock()
	defer f.opsMu.Unlock()
	f.remoteOps = append(f.remoteOps, op)
}

func (f *noRemoteFakeGit) ops() []string {
	f.opsMu.Lock()
	defer f.opsMu.Unlock()
	return append([]string(nil), f.remoteOps...)
}

func (f *noRemoteFakeGit) Fetch(string) error {
	f.record("fetch")
	return errNoRemote
}

func (f *noRemoteFakeGit) RevListLeftRightCount(string, string) (int, int, error) {
	f.record("rev-list")
	return 0, 0, errNoRemote
}

func (f *noRemoteFakeGit) Push(string, string) error {
	f.record("push")
	return errNoRemote
}

// TestRunRepoLoopCommitOnlyModeNeverTouchesARemote pins what commit-only
// mode is for: a repository with no remote still gets its edits
// auto-committed, and — unlike the same repository under sync mode, where
// every cycle fails at `git fetch` — its status stays clean, so a stale
// last_successful_sync_at keeps meaning "this repository stopped working".
func TestRunRepoLoopCommitOnlyModeNeverTouchesARemote(t *testing.T) {
	dir := t.TempDir()
	// Deliberately outside the watched directory: writing the status file
	// inside it would re-trigger the watcher and spin the loop on its own
	// output.
	statusPath := filepath.Join(t.TempDir(), "status.json")

	repo := config.Repository{
		Path:          dir,
		Settle:        20 * time.Millisecond,
		MaxWait:       2 * time.Second,
		FetchInterval: time.Hour,
		Mode:          config.ModeCommitOnly,
		Remote:        "origin",
		Branch:        "main",
		OnConflict:    config.OnConflictBackup,
	}

	git := &noRemoteFakeGit{fakeGit: &fakeGit{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder, err := newStatusRecorder(statusPath)
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

	waitFor(t, 2*time.Second, func() bool { return git.fakeGit.commitCount() >= 1 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down within 2s of context cancellation")
	}

	if got := git.ops(); len(got) != 0 {
		t.Errorf("remote operations = %v, want none in commit-only mode", got)
	}

	sf, err := LoadStatusFile(statusPath)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	st := sf.Repos[dir]
	if st.LastError != "" {
		t.Errorf("LastError = %q, want \"\": a commit-only cycle has nothing that can fail remotely", st.LastError)
	}
	if st.LastSuccessfulSyncAt.IsZero() {
		t.Error("LastSuccessfulSyncAt is zero, want it stamped — a commit-only cycle that commits is a complete cycle")
	}
	if st.Phase != PhaseIdle {
		t.Errorf("Phase = %q, want %q", st.Phase, PhaseIdle)
	}
	if st.LastCommit == "" {
		t.Error("LastCommit is empty, want it stamped after the auto-commit")
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
		Mode:          config.ModeSync,
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
		Mode:          config.ModeSync,
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

// fetchErrorFakeGit is a fakeGit whose Fetch always fails. It's used to
// simulate the offline case where the network round-trip can't reach the
// upstream.
type fetchErrorFakeGit struct {
	*fakeGit
	err error
}

func (f *fetchErrorFakeGit) Fetch(string) error { return f.err }

// TestRunRepoLoopCommitsEvenWhenFetchFailsOffline pins the "offline commit"
// invariant: fetch failure must not skip the auto-commit phase. If a laptop
// goes offline (or upstream is unreachable), local edits still need to be
// captured into commits so nothing piles up uncommitted while the user is
// disconnected; only the integrate + push steps are skipped in that case.
func TestRunRepoLoopCommitsEvenWhenFetchFailsOffline(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status.json")

	git := &fetchErrorFakeGit{
		fakeGit: &fakeGit{},
		err:     fmt.Errorf("dial tcp: no route to host"),
	}

	repo := config.Repository{
		Path:          dir,
		Settle:        20 * time.Millisecond,
		MaxWait:       2 * time.Second,
		FetchInterval: time.Hour,
		Mode:          config.ModeSync,
		Remote:        "origin",
		Branch:        "main",
		OnConflict:    config.OnConflictBackup,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder, err := newStatusRecorder(statusPath)
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

	// The commit must land even though fetch failed.
	waitFor(t, 2*time.Second, func() bool { return git.fakeGit.commitCount() >= 1 })

	// And the fetch failure must surface via LastError, phase resolved to
	// idle (working tree is clean, next cycle will retry).
	waitFor(t, 2*time.Second, func() bool {
		sf, err := LoadStatusFile(statusPath)
		if err != nil {
			return false
		}
		st := sf.Repos[dir]
		return strings.HasPrefix(st.LastError, "fetch:") && st.Phase == PhaseIdle
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down within 2s of context cancellation")
	}
}

// panicOnCommitFakeGit is a fakeGit whose Commit panics on first call. It's
// used to exercise the "panic inside the commit/integrate phase must not
// leak the save lock" invariant.
type panicOnCommitFakeGit struct {
	*fakeGit
	panicked bool
}

func (f *panicOnCommitFakeGit) Commit(string) error {
	f.mu.Lock()
	if !f.panicked {
		f.panicked = true
		f.mu.Unlock()
		panic("boom: simulated failure during commit phase")
	}
	f.mu.Unlock()
	return f.fakeGit.Commit("")
}

// TestRunRepoLoopReleasesSaveLockAfterCommitPhasePanic pins the "panic
// safety net" invariant: if the commit/integrate phase panics mid-flight
// (e.g. an unexpected bug in merge or conflict-resolution), the deferred
// release must free the flock so subsequent cycles aren't permanently
// deadlocked with "skipped: save in-flight".
func TestRunRepoLoopReleasesSaveLockAfterCommitPhasePanic(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "save.lock")

	repo := config.Repository{
		Path:          dir,
		Settle:        20 * time.Millisecond,
		MaxWait:       2 * time.Second,
		FetchInterval: time.Hour,
		Mode:          config.ModeSync,
		Remote:        "origin",
		Branch:        "main",
		OnConflict:    config.OnConflictBackup,
		SaveLockPath:  lockPath,
	}

	git := &panicOnCommitFakeGit{fakeGit: &fakeGit{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder, err := newStatusRecorder(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	panicked := make(chan any, 1)
	go func() {
		defer func() {
			panicked <- recover() // may deliver nil if no panic, so channel signals completion either way
		}()
		_ = runRepoLoop(ctx, git, repo, "test-host", logger, recorder)
	}()

	time.Sleep(50 * time.Millisecond)
	git.fakeGit.markDirty()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("v0"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	select {
	case r := <-panicked:
		if r == nil {
			t.Fatal("expected panic from runRepoLoop, got clean return")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runRepoLoop did not panic within timeout")
	}

	// If the defer released the flock as intended, this must succeed.
	// Retry briefly to tolerate any goroutine-teardown ordering.
	deadline := time.Now().Add(2 * time.Second)
	for {
		l, ok, err := tryAcquireSaveLock(lockPath)
		if err == nil && ok {
			l.release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("save lock was still held after panic: (%v, %v)", ok, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// aheadPushObserverFakeGit reports the local branch as Ahead so runCycle
// takes the push path, and lets a test observe the exact moment Push is
// called (to check surrounding state like status.json's phase).
type aheadPushObserverFakeGit struct {
	*fakeGit
	onPush func()
}

func (f *aheadPushObserverFakeGit) RevListLeftRightCount(string, string) (int, int, error) {
	return 1, 0, nil // ahead=1, behind=0 → Ahead → Push
}

func (f *aheadPushObserverFakeGit) Push(remote, branch string) error {
	if f.onPush != nil {
		f.onPush()
	}
	return f.fakeGit.Push(remote, branch)
}

// TestRunRepoLoopPhaseIsIdleDuringPush pins the "phase tracks the lock
// window, not the whole cycle" invariant: push runs outside the save lock,
// so an external observer of status.json should see phase = "idle" during
// the push. Otherwise the syncing signal overstates when the working tree
// is actually being written to.
func TestRunRepoLoopPhaseIsIdleDuringPush(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status.json")

	observed := make(chan string, 8)
	git := &aheadPushObserverFakeGit{
		fakeGit: &fakeGit{},
		onPush: func() {
			sf, err := LoadStatusFile(statusPath)
			if err != nil {
				return
			}
			select {
			case observed <- sf.Repos[dir].Phase:
			default:
			}
		},
	}

	repo := config.Repository{
		Path:          dir,
		Settle:        20 * time.Millisecond,
		MaxWait:       2 * time.Second,
		FetchInterval: time.Hour,
		Mode:          config.ModeSync,
		Remote:        "origin",
		Branch:        "main",
		OnConflict:    config.OnConflictBackup,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder, err := newStatusRecorder(statusPath)
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
	case phase := <-observed:
		if phase != PhaseIdle {
			t.Fatalf("phase observed during push = %q, want %q — push runs outside the lock, so phase should already be idle",
				phase, PhaseIdle)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("push never ran within timeout")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down within 2s of context cancellation")
	}
}
