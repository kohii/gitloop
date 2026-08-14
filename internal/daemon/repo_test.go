package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kohii/gitloop/internal/config"
	"github.com/kohii/gitloop/internal/gitcmd"
	"github.com/kohii/gitloop/internal/statemachine"
)

type committedSyncFakeGit struct {
	*fakeGit
	ahead, behind int
	// ffErr stands in for git's refusal to fast-forward. Real git only
	// refuses when checking out the incoming tree would overwrite something,
	// so a fake that fails while dirty models a genuine collision.
	ffErr   error
	ffCount int
	// rebasePauseErr and rebaseErr stand in for the two ways git declines to
	// replay local commits: stopping partway (a conflict, or anything else
	// that pauses a rebase), and refusing to start at all (a dirty working
	// tree being the case to expect).
	rebasePauseErr error
	rebaseErr      error
	// conflicted is what ConflictedFiles reports while a rebase is paused,
	// which is how the phase tells a real conflict apart from a rebase that
	// stopped for some other reason.
	conflicted       []string
	abortErr         error
	rebaseCount      int
	rebaseAbortCount int
	pushCount        int
	// heads answers RevParse, so a test can move one side of the divergence
	// between cycles.
	heads map[string]string
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
	if f.rebasePauseErr != nil {
		return true, f.rebasePauseErr
	}
	return false, nil
}

func (f *committedSyncFakeGit) RebaseAbort() error {
	f.rebaseAbortCount++
	return f.abortErr
}

func (f *committedSyncFakeGit) ConflictedFiles() ([]string, error) { return f.conflicted, nil }

// RevParse names each ref's commit after the ref itself, so the two sides of a
// divergence are distinguishable without a test having to invent hashes. A
// test that needs one side to move overrides the entry in heads.
func (f *committedSyncFakeGit) RevParse(ref string) (string, error) {
	if head, ok := f.heads[ref]; ok {
		return head, nil
	}
	return "commit-of-" + ref, nil
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

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

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

	result, branch, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

	if result.Err != nil {
		t.Fatalf("Err = %v, want nil", result.Err)
	}
	if branch != "main" || !needPush {
		t.Errorf("branch/needPush = %q/%v, want main/true", branch, needPush)
	}
	if git.commitCount() != 0 || git.ffCount != 0 || git.rebaseCount != 0 {
		t.Errorf("phase changed local checkout: commits=%d ff=%d rebases=%d", git.commitCount(), git.ffCount, git.rebaseCount)
	}
}

func TestRunCommittedSyncPhaseFastForwardsCleanTree(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{}, ahead: 0, behind: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

	if result.Err != nil || needPush || git.ffCount != 1 {
		t.Errorf("clean behind result = err:%v needPush:%v ff:%d, want nil/false/1", result.Err, needPush, git.ffCount)
	}
	if git.rebaseCount != 0 {
		t.Errorf("rebaseCount = %d, want 0: only a divergence is replayed", git.rebaseCount)
	}
}

// TestRunCommittedSyncPhaseFastForwardsOverADirtyTree pins the point of
// leaving the decision to git: uncommitted work no longer pre-empts the
// fast-forward, so a checkout stays current while unrelated files are edited.
func TestRunCommittedSyncPhaseFastForwardsOverADirtyTree(t *testing.T) {
	git := &committedSyncFakeGit{fakeGit: &fakeGit{dirty: true}, ahead: 0, behind: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

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

	result, _, _ := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

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

	result, branch, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

	if result.Err != nil || result.BlockedReason != "" {
		t.Fatalf("diverged result = err:%v reason:%q, want nil/empty", result.Err, result.BlockedReason)
	}
	if git.rebaseCount != 1 || branch != "main" || !needPush {
		t.Errorf("diverged result = rebases:%d branch:%q needPush:%v, want 1/main/true", git.rebaseCount, branch, needPush)
	}
	if result.Action != statemachine.RebaseThenPush {
		t.Errorf("Action = %v, want %v: the push error message reads this", result.Action, statemachine.RebaseThenPush)
	}
	if git.commitCount() != 0 || git.ffCount != 0 || git.rebaseAbortCount != 0 {
		t.Errorf("phase overreached: commits=%d ff=%d aborts=%d", git.commitCount(), git.ffCount, git.rebaseAbortCount)
	}
}

// conflictingRebaseFakeGit is a repository whose divergence cannot be replayed.
func conflictingRebaseFakeGit() *committedSyncFakeGit {
	return &committedSyncFakeGit{
		fakeGit:        &fakeGit{},
		ahead:          1,
		behind:         1,
		rebasePauseErr: errors.New("could not apply 1a2b3c4"),
		conflicted:     []string{"a.md"},
	}
}

// TestRunCommittedSyncPhaseAbortsAConflictingRebase keeps a failed replay from
// leaving the repository worse off than the divergence it tried to resolve: a
// paused rebase would block every later cycle at PreCheck.
func TestRunCommittedSyncPhaseAbortsAConflictingRebase(t *testing.T) {
	git := conflictingRebaseFakeGit()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

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

// TestRunCommittedSyncPhaseDoesNotRetryTheSameFailedReplay is what keeps a
// conflicting divergence from becoming a runaway loop: the rebase and the abort
// that undoes it both write the working tree, and those writes are what
// schedules the next cycle. Retrying an unchanged divergence would replay it
// every settle window, fetching the remote each time, until a human intervened.
func TestRunCommittedSyncPhaseDoesNotRetryTheSameFailedReplay(t *testing.T) {
	git := conflictingRebaseFakeGit()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard := &replayGuard{}

	runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)
	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)

	if git.rebaseCount != 1 || git.rebaseAbortCount != 1 {
		t.Errorf("second cycle retried the replay: rebases=%d aborts=%d, want 1/1", git.rebaseCount, git.rebaseAbortCount)
	}
	if result.BlockedReason != BlockedDiverged || result.Err == nil || needPush {
		t.Errorf("second cycle result = reason:%q err:%v needPush:%v, want the divergence still reported", result.BlockedReason, result.Err, needPush)
	}
}

// TestRunCommittedSyncPhaseRetriesOnceUpstreamMoves is the other half of that
// guard: it must suppress the identical attempt, not the repository. New
// commits on either side are a different divergence and worth replaying.
func TestRunCommittedSyncPhaseRetriesOnceUpstreamMoves(t *testing.T) {
	git := conflictingRebaseFakeGit()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard := &replayGuard{}

	runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)
	git.heads = map[string]string{"origin/main": "a-newer-upstream-commit"}
	runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)

	if git.rebaseCount != 2 {
		t.Errorf("rebaseCount = %d, want 2: a moved upstream is a new divergence", git.rebaseCount)
	}
}

// TestRunCommittedSyncPhaseDoesNotBlameADivergenceForAnUnrelatedPause keeps the
// blocked status actionable. A failing commit hook or an unavailable signing
// key pauses a rebase exactly like a conflict does, but telling the user to
// merge by hand would send them after the wrong problem.
func TestRunCommittedSyncPhaseDoesNotBlameADivergenceForAnUnrelatedPause(t *testing.T) {
	git := conflictingRebaseFakeGit()
	git.rebasePauseErr = errors.New("gpg failed to sign the data")
	git.conflicted = nil
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard := &replayGuard{}

	result, _, _ := runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)

	if result.BlockedReason != "" {
		t.Errorf("BlockedReason = %q, want empty: nothing conflicted", result.BlockedReason)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "gpg failed to sign the data") {
		t.Errorf("Err = %v, want the reason the rebase stopped", result.Err)
	}
	if git.rebaseAbortCount != 1 {
		t.Errorf("rebaseAbortCount = %d, want 1: a pause is aborted whatever caused it", git.rebaseAbortCount)
	}

	// The status file only keeps the most recent cycle, so a suppressed cycle
	// that re-derived its own reason would be the only thing the user ever
	// reads — and it would send them after a divergence that isn't the problem.
	suppressed, _, _ := runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)
	if suppressed.BlockedReason != "" || suppressed.Err == nil || !strings.Contains(suppressed.Err.Error(), "gpg failed to sign the data") {
		t.Errorf("suppressed cycle = reason:%q err:%v, want the original failure repeated", suppressed.BlockedReason, suppressed.Err)
	}
}

// TestRunCommittedSyncPhaseRetriesAFixableFailure covers the failure a guard
// must not make permanent: nothing about the divergence changes when a signing
// key is unlocked or a hook is fixed, so a guard that only released on new
// commits would keep the repository stopped long after the cause was gone.
func TestRunCommittedSyncPhaseRetriesAFixableFailure(t *testing.T) {
	git := conflictingRebaseFakeGit()
	git.rebasePauseErr = errors.New("gpg failed to sign the data")
	git.conflicted = nil
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard := &replayGuard{}

	runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)
	guard.retryTransientFailure()
	git.rebasePauseErr = nil // the key is available now
	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)

	if result.Err != nil || !needPush {
		t.Errorf("retried result = err:%v needPush:%v, want a completed replay", result.Err, needPush)
	}
	if git.rebaseCount != 2 {
		t.Errorf("rebaseCount = %d, want 2", git.rebaseCount)
	}
}

// TestRunCommittedSyncPhaseKeepsAConflictGuarded is the other side of that
// release: a conflict stands until someone resolves it, and retrying one on
// every periodic cycle would rewrite and restore the working tree forever.
func TestRunCommittedSyncPhaseKeepsAConflictGuarded(t *testing.T) {
	git := conflictingRebaseFakeGit()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard := &replayGuard{}

	runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)
	guard.retryTransientFailure()
	runCommittedSyncPhase(git, committedSyncRepo(), guard, logger)

	if git.rebaseCount != 1 {
		t.Errorf("rebaseCount = %d, want 1: a conflict is not retried on a timer", git.rebaseCount)
	}
}

// TestRunCommittedSyncPhaseReportsAnOperationInProgress keeps gitloop from
// blaming a user's edits for its own refusal to touch someone else's merge or
// rebase — which leaves the working tree dirty by its very nature.
func TestRunCommittedSyncPhaseReportsAnOperationInProgress(t *testing.T) {
	git := &committedSyncFakeGit{
		fakeGit:   &fakeGit{dirty: true},
		ahead:     1,
		behind:    1,
		rebaseErr: gitcmd.ErrOperationInProgress,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

	if result.BlockedReason != BlockedOperation {
		t.Errorf("BlockedReason = %q, want %q", result.BlockedReason, BlockedOperation)
	}
	if needPush || git.rebaseAbortCount != 0 {
		t.Errorf("phase touched the other operation: needPush=%v aborts=%d", needPush, git.rebaseAbortCount)
	}
}

// TestRunCommittedSyncPhaseReportsAFailedRebaseAbort surfaces the one state
// gitloop cannot clean up after itself, rather than reporting the divergence
// as if the checkout were back where it started.
func TestRunCommittedSyncPhaseReportsAFailedRebaseAbort(t *testing.T) {
	git := conflictingRebaseFakeGit()
	git.abortErr = errors.New("could not restore HEAD")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

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

	result, _, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

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

	result, branch, needPush := runCommittedSyncPhase(git, committedSyncRepo(), &replayGuard{}, logger)

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

// replayLoopFakeGit models the part of a failed replay that makes it dangerous
// to retry: both the rebase and the abort that undoes it rewrite the working
// tree, and the daemon is watching that tree.
type replayLoopFakeGit struct {
	*committedSyncFakeGit
	dir     string
	writes  int
	rebased chan struct{}
}

func (f *replayLoopFakeGit) Rebase(upstream string) (bool, error) {
	paused, err := f.committedSyncFakeGit.Rebase(upstream)
	f.touchWorkingTree()
	select {
	case f.rebased <- struct{}{}:
	default:
	}
	return paused, err
}

func (f *replayLoopFakeGit) RebaseAbort() error {
	err := f.committedSyncFakeGit.RebaseAbort()
	f.touchWorkingTree()
	return err
}

func (f *replayLoopFakeGit) touchWorkingTree() {
	f.writes++
	// A write that silently failed would leave the test passing for the wrong
	// reason: no file event, so nothing to schedule the retry it guards against.
	if err := os.WriteFile(filepath.Join(f.dir, "a.md"), []byte(fmt.Sprintf("replay %d\n", f.writes)), 0o644); err != nil {
		panic(err)
	}
}

// TestRunRepoLoopDoesNotRerunAFailedReplay is the loop-level half of the replay
// guard: one guard has to live across cycles. Per-cycle state would let the
// daemon rebase, abort, wake itself on those very writes, and do it again every
// settle window — fetching the remote each time — for as long as the divergence
// stands.
func TestRunRepoLoopDoesNotRerunAFailedReplay(t *testing.T) {
	dir := t.TempDir()
	git := &replayLoopFakeGit{
		committedSyncFakeGit: conflictingRebaseFakeGit(),
		dir:                  dir,
		rebased:              make(chan struct{}, 1),
	}
	repo := committedSyncRepo()
	repo.Path = dir
	repo.Settle = 20 * time.Millisecond
	repo.MaxWait = time.Second
	repo.FetchInterval = time.Hour // only file events may drive this test
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder, err := newStatusRecorder(filepath.Join(t.TempDir(), "status.json"))
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runRepoLoop(ctx, git, repo, "test-host", logger, recorder) }()

	select {
	case <-git.rebased:
	case <-time.After(2 * time.Second):
		t.Fatal("the startup cycle did not attempt the replay")
	}
	// Long enough for many settle windows to elapse, had the writes above
	// scheduled another cycle that replayed again.
	time.Sleep(500 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRepoLoop returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runRepoLoop did not shut down")
	}

	if git.rebaseCount != 1 {
		t.Errorf("rebaseCount = %d, want 1: the failed replay was retried on its own writes", git.rebaseCount)
	}
	if got := loadRepoStatus(t, recorder).BlockedReason; got != BlockedDiverged {
		t.Errorf("BlockedReason = %q, want %q: the divergence still needs reporting", got, BlockedDiverged)
	}
}

func loadRepoStatus(t *testing.T, recorder *statusRecorder) RepoStatus {
	t.Helper()
	sf, err := LoadStatusFile(recorder.path)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	for _, s := range sf.Repos {
		return s
	}
	t.Fatal("status file has no repository entry")
	return RepoStatus{}
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
