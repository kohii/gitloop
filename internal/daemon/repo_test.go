package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kohii/gitloop/internal/config"
)

type committedSyncFakeGit struct {
	*fakeGit
	ahead, behind int
	ffCount       int
	pushCount     int
}

type checkedOutBranchFakeGit struct {
	*committedSyncFakeGit
	branch string
}

func (f *checkedOutBranchFakeGit) CurrentBranch() (string, error) {
	return f.branch, nil
}

func (f *committedSyncFakeGit) RevListLeftRightCount(string, string) (int, int, error) {
	return f.ahead, f.behind, nil
}

func (f *committedSyncFakeGit) MergeFF(string) error {
	f.ffCount++
	return nil
}

func (f *committedSyncFakeGit) Merge(string) (bool, error) {
	panic("committed-sync must not run a merge")
}

func (f *committedSyncFakeGit) Push(string, string) error {
	f.pushCount++
	return nil
}

func committedSyncRepo() config.Repository {
	return config.Repository{
		Path:       "/repo",
		Mode:       config.ModeCommittedSync,
		Remote:     "origin",
		Branch:     "main",
		OnConflict: config.OnConflictBackup,
	}
}

func TestRunCommittedSyncPhaseNeverCommitsDirtyChanges(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{dirty: true}, ahead: 0, behind: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.BlockedReason != BlockedDirtyBehind {
		t.Errorf("BlockedReason = %q, want %q", result.BlockedReason, BlockedDirtyBehind)
	}
	if result.Err == nil {
		t.Fatal("Err = nil, want dirty working tree error")
	}
	if needPush || git.ffCount != 0 || git.pushCount != 0 || git.commitCount() != 0 {
		t.Errorf("dirty behind phase mutated repository: needPush=%v ff=%d push=%d commits=%d", needPush, git.ffCount, git.pushCount, git.commitCount())
	}
}

func TestRunCommittedSyncPhasePushesExistingCommitsWithDirtyTree(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{dirty: true}, ahead: 1, behind: 0}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, branch, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.Err != nil {
		t.Fatalf("Err = %v, want nil", result.Err)
	}
	if branch != "main" || !needPush {
		t.Errorf("branch/needPush = %q/%v, want main/true", branch, needPush)
	}
	if git.commitCount() != 0 || git.ffCount != 0 {
		t.Errorf("phase changed local checkout: commits=%d ff=%d", git.commitCount(), git.ffCount)
	}
}

func TestRunCommittedSyncPhaseFastForwardsOnlyCleanTree(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{}, ahead: 0, behind: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.Err != nil || needPush || git.ffCount != 1 {
		t.Errorf("clean behind result = err:%v needPush:%v ff:%d, want nil/false/1", result.Err, needPush, git.ffCount)
	}
}

func TestRunCommittedSyncPhaseRefusesDivergedHistory(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{}, ahead: 1, behind: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.BlockedReason != BlockedDiverged || result.Err == nil {
		t.Errorf("diverged result = reason:%q err:%v, want blocked error", result.BlockedReason, result.Err)
	}
	if needPush || git.ffCount != 0 || git.pushCount != 0 || git.commitCount() != 0 {
		t.Errorf("diverged phase mutated repository: needPush=%v ff=%d push=%d commits=%d", needPush, git.ffCount, git.pushCount, git.commitCount())
	}
}

func TestRunCommittedSyncPhaseRefusesDifferentCheckedOutBranch(t *testing.T) {
	git := &checkedOutBranchFakeGit{
		committedSyncFakeGit: &committedSyncFakeGit{fakeGit: &fakeGit{}, ahead: 0, behind: 1},
		branch:               "feature",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, branch, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.Err == nil || branch != "" || needPush {
		t.Errorf("branch mismatch result = err:%v branch:%q needPush:%v, want error/no branch/no push", result.Err, branch, needPush)
	}
	if git.ffCount != 0 || git.pushCount != 0 {
		t.Errorf("branch mismatch phase mutated repository: ff=%d push=%d", git.ffCount, git.pushCount)
	}
}

type committedSyncLoopFakeGit struct {
	*committedSyncFakeGit
	pushed chan struct{}
}

func (f *committedSyncLoopFakeGit) Push(remote, branch string) error {
	if err := f.committedSyncFakeGit.Push(remote, branch); err != nil {
		return err
	}
	select {
	case f.pushed <- struct{}{}:
	default:
	}
	return nil
}

func TestRunRepoLoopCommittedSyncDoesNotCommitWorkingTree(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(t.TempDir(), "status.json")
	git := &committedSyncLoopFakeGit{
		committedSyncFakeGit: &committedSyncFakeGit{
			fakeGit: &fakeGit{dirty: true},
			ahead:   1,
		},
		pushed: make(chan struct{}, 1),
	}
	repo := committedSyncRepo()
	repo.Path = dir
	repo.FetchInterval = time.Hour
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder, err := newStatusRecorder(statusPath)
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runRepoLoop(ctx, git, repo, "test-host", logger, recorder) }()

	select {
	case <-git.pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("committed-sync startup cycle did not push existing commit")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRepoLoop returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down")
	}
	if got := git.commitCount(); got != 0 {
		t.Fatalf("commit count = %d, want 0", got)
	}
}
