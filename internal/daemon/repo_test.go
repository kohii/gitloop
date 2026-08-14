package daemon

import (
	"context"
	"errors"
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
	// ffErr stands in for git's refusal to fast-forward. Real git only
	// refuses when checking out the incoming tree would overwrite something,
	// so a fake that fails while dirty models a genuine collision.
	ffErr   error
	ffCount int
	// rebaseConflict and rebaseErr stand in for the two ways git declines to
	// replay local commits: stopping on a conflict mid-replay, and refusing to
	// start at all (a dirty working tree being the case to expect).
	rebaseConflict   bool
	rebaseErr        error
	abortErr         error
	rebaseCount      int
	rebaseAbortCount int
	pushCount        int
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
	if f.ffErr != nil {
		return f.ffErr
	}
	f.ffCount++
	return nil
}

func (f *committedSyncFakeGit) Merge(string) (bool, error) {
	panic("committed-sync must not run a merge")
}

func (f *committedSyncFakeGit) Rebase(string) (bool, error) {
	if f.rebaseErr != nil {
		return false, f.rebaseErr
	}
	f.rebaseCount++
	return f.rebaseConflict, nil
}

func (f *committedSyncFakeGit) RebaseAbort() error {
	f.rebaseAbortCount++
	return f.abortErr
}

func (f *committedSyncFakeGit) Push(context.Context, string, string) error {
	f.pushCount++
	return nil
}

// errRefusedFF stands in for git declining to move the checkout forward.
var errRefusedFF = errors.New("local changes would be overwritten by merge")

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
	git := &committedSyncFakeGit{fakeGit: &fakeGit{dirty: true}, ahead: 0, behind: 1, ffErr: errRefusedFF}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.BlockedReason != BlockedDirtyTree {
		t.Errorf("BlockedReason = %q, want %q", result.BlockedReason, BlockedDirtyTree)
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

func TestRunCommittedSyncPhaseFastForwardsCleanTree(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{}, ahead: 0, behind: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.Err != nil || needPush || git.ffCount != 1 {
		t.Errorf("clean behind result = err:%v needPush:%v ff:%d, want nil/false/1", result.Err, needPush, git.ffCount)
	}
}

// TestRunCommittedSyncPhaseFastForwardsOverADirtyTree pins the point of
// leaving the decision to git: uncommitted work no longer pre-empts the
// fast-forward, so a checkout stays current while unrelated files are edited.
func TestRunCommittedSyncPhaseFastForwardsOverADirtyTree(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{dirty: true}, ahead: 0, behind: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.Err != nil || result.BlockedReason != "" {
		t.Fatalf("result = err:%v reason:%q, want nil/empty", result.Err, result.BlockedReason)
	}
	if git.ffCount != 1 {
		t.Errorf("ffCount = %d, want 1: a dirty tree git is willing to fast-forward must not block", git.ffCount)
	}
	if needPush || git.commitCount() != 0 {
		t.Errorf("phase overreached: needPush=%v commits=%d", needPush, git.commitCount())
	}
}

// TestRunCommittedSyncPhaseLeavesACleanTreeRefusalUnclassified keeps the
// dirty-working-tree status honest: a fast-forward that fails for some other
// reason must surface the git error rather than blame the user's edits.
func TestRunCommittedSyncPhaseLeavesACleanTreeRefusalUnclassified(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{}, ahead: 0, behind: 1, ffErr: errRefusedFF}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, _ := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.Err == nil {
		t.Fatal("Err = nil, want the underlying fast-forward failure")
	}
	if result.BlockedReason != "" {
		t.Errorf("BlockedReason = %q, want empty on a clean tree", result.BlockedReason)
	}
}

// TestRunCommittedSyncPhaseRebasesDivergedHistory pins the resolution a shared
// checkout needs: two machines committing to one branch is ordinary, so the
// local commits are replayed onto upstream and pushed rather than parking the
// repository until a human notices.
func TestRunCommittedSyncPhaseRebasesDivergedHistory(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{}, ahead: 1, behind: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, branch, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.Err != nil || result.BlockedReason != "" {
		t.Fatalf("diverged result = err:%v reason:%q, want nil/empty", result.Err, result.BlockedReason)
	}
	if git.rebaseCount != 1 || branch != "main" || !needPush {
		t.Errorf("diverged result = rebases:%d branch:%q needPush:%v, want 1/main/true", git.rebaseCount, branch, needPush)
	}
	if git.commitCount() != 0 || git.ffCount != 0 || git.rebaseAbortCount != 0 {
		t.Errorf("phase overreached: commits=%d ff=%d aborts=%d", git.commitCount(), git.ffCount, git.rebaseAbortCount)
	}
}

// TestRunCommittedSyncPhaseAbortsAConflictingRebase keeps a failed replay from
// leaving the repository worse off than the divergence it tried to resolve: a
// paused rebase would block every later cycle at PreCheck.
func TestRunCommittedSyncPhaseAbortsAConflictingRebase(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{}, ahead: 1, behind: 1, rebaseConflict: true}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.BlockedReason != BlockedDiverged || result.Err == nil {
		t.Errorf("conflicting rebase result = reason:%q err:%v, want blocked error", result.BlockedReason, result.Err)
	}
	if git.rebaseAbortCount != 1 {
		t.Errorf("rebaseAbortCount = %d, want 1", git.rebaseAbortCount)
	}
	if needPush || git.pushCount != 0 || git.commitCount() != 0 {
		t.Errorf("conflicting rebase mutated repository: needPush=%v push=%d commits=%d", needPush, git.pushCount, git.commitCount())
	}
}

// TestRunCommittedSyncPhaseReportsAFailedRebaseAbort surfaces the one state
// gitloop cannot clean up after itself, rather than reporting the divergence
// as if the checkout were back where it started.
func TestRunCommittedSyncPhaseReportsAFailedRebaseAbort(t *testing.T) {
	git := &committedSyncFakeGit{
		fakeGit:        &fakeGit{},
		ahead:          1,
		behind:         1,
		rebaseConflict: true,
		abortErr:       errors.New("could not restore HEAD"),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.Err == nil {
		t.Fatal("Err = nil, want the failed abort surfaced")
	}
	if result.BlockedReason != "" {
		t.Errorf("BlockedReason = %q, want empty: this is a failure, not a deliberate block", result.BlockedReason)
	}
	if needPush || git.pushCount != 0 {
		t.Errorf("failed abort still pushed: needPush=%v push=%d", needPush, git.pushCount)
	}
}

// TestRunCommittedSyncPhaseDefersARebaseOverUncommittedWork covers the refusal
// a rebase makes that a fast-forward does not: git declines any uncommitted
// change, not only the ones the incoming commits would overwrite.
func TestRunCommittedSyncPhaseDefersARebaseOverUncommittedWork(t *testing.T) {
	git := &committedSyncFakeGit{
		fakeGit:   &fakeGit{dirty: true},
		ahead:     1,
		behind:    1,
		rebaseErr: errors.New("cannot rebase: You have unstaged changes"),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), logger)

	if result.BlockedReason != BlockedDirtyTree || result.Err == nil {
		t.Errorf("dirty diverged result = reason:%q err:%v, want %q with an error", result.BlockedReason, result.Err, BlockedDirtyTree)
	}
	if needPush || git.pushCount != 0 || git.rebaseAbortCount != 0 || git.commitCount() != 0 {
		t.Errorf("refused rebase mutated repository: needPush=%v push=%d aborts=%d commits=%d", needPush, git.pushCount, git.rebaseAbortCount, git.commitCount())
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

func (f *committedSyncLoopFakeGit) Push(ctx context.Context, remote, branch string) error {
	if err := f.committedSyncFakeGit.Push(ctx, remote, branch); err != nil {
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
