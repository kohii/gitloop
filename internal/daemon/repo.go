package daemon

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kohii/gitloop/internal/commitmsg"
	"github.com/kohii/gitloop/internal/config"
	"github.com/kohii/gitloop/internal/gitcmd"
	"github.com/kohii/gitloop/internal/statemachine"
)

// cycleResult summarizes what one sync cycle did, for logging and status
// reporting.
type cycleResult struct {
	// Skipped is the PreCheck guard reason, non-empty if the cycle bailed
	// out before touching the repository.
	Skipped   string
	Committed bool
	Action    statemachine.Action
	Pushed    bool
	// Conflict is true if this cycle hit a merge conflict that was resolved
	// via the backup policy, leaving unresolved-by-a-human conflict backup
	// files in the working tree that still need attention.
	Conflict bool
	// BlockedReason is a stable status-facing reason for a deliberate
	// committed-sync refusal, such as a dirty working tree or a divergence
	// that could not be replayed. It is intentionally separate from Err so
	// callers can display a concise actionable status while retaining the
	// full error.
	BlockedReason string
	Err           error
}

const (
	BlockedDirtyTree = "dirty-working-tree"
	BlockedDiverged  = "diverged-history"
	BlockedOperation = "operation-in-progress"
)

// runPreCheckPhase runs the guard step that opens a sync cycle: is there a
// rebase or merge already in progress that gitloop should stay out of? It
// never touches the working tree, so the caller runs it before (and outside
// of) the save lock. proceed is false when the cycle should stop right here
// — the returned cycleResult carries the Skipped or Err to surface, and no
// further phases should run.
func runPreCheckPhase(repoPath string, logger *slog.Logger) (result cycleResult, proceed bool) {
	guard, err := statemachine.PreCheck(repoPath)
	if err != nil {
		return cycleResult{Err: fmt.Errorf("precheck: %w", err)}, false
	}
	if !guard.Safe {
		logger.Warn("skipping sync cycle: repository has a rebase/merge in progress", "reason", guard.Reason)
		return cycleResult{Skipped: guard.Reason}, false
	}
	return cycleResult{}, true
}

// runCommitPhase auto-commits any dirty changes in the working tree. It is
// the minimum useful work of a sync cycle: even when we can't classify or
// integrate (e.g. fetch failed because the network is down), running this
// alone keeps local edits from piling up uncommitted while the user is
// offline. The caller is expected to hold the save lock across this phase.
func runCommitPhase(git GitClient, hostname string, logger *slog.Logger) (result cycleResult) {
	committed, err := commitIfDirty(git, hostname, logger)
	if err != nil {
		result.Err = fmt.Errorf("auto-commit: %w", err)
		return
	}
	result.Committed = committed
	return
}

// runIntegratePhase classifies the local branch against the freshly fetched
// upstream and merges or fast-forwards if it's behind or diverged (running
// the conflict-resolution policy if the merge stops on a conflict). It
// assumes runCommitPhase has already handled any dirty working tree, and
// leaves pushing to the caller — pushing is network I/O that should run
// outside the save lock. When needPush is true, the caller should push
// repo.Remote/branch (the branch here is the effective one, so an empty
// repo.Branch has already been resolved to the currently-checked-out
// branch). The caller is expected to hold the save lock across this phase.
func runIntegratePhase(git GitClient, repo config.Repository, hostname string, logger *slog.Logger, recorder *statusRecorder) (result cycleResult, branch string, needPush bool) {
	var err error
	branch, err = checkedOutBranch(git, repo.Branch)
	if err != nil {
		result.Err = err
		return
	}
	upstream := repo.Remote + "/" + branch

	ahead, behind, err := git.RevListLeftRightCount(branch, upstream)
	if err != nil {
		result.Err = fmt.Errorf("rev-list: %w", err)
		return
	}
	state := statemachine.Classify(ahead, behind)
	action := statemachine.ActionFor(state)
	result.Action = action
	logger.Info("classified sync state", "state", state.String(), "action", action.String(), "ahead", ahead, "behind", behind)

	switch action {
	case statemachine.NoOp:
		// Nothing to do.

	case statemachine.Push:
		needPush = true

	case statemachine.FastForwardMerge:
		if err := git.MergeFF(upstream); err != nil {
			result.Err = fmt.Errorf("fast-forward merge: %w", err)
			return
		}

	case statemachine.MergeThenPush:
		conflict, err := git.Merge(upstream)
		if err != nil {
			result.Err = fmt.Errorf("merge: %w", err)
			return
		}
		if conflict {
			logger.Warn("merge stopped on a conflict, applying conflict policy", "on_conflict", repo.OnConflict)
			completed, backup := resolveConflicts(git, repo.Path, upstream, repo.OnConflict, hostname, logger, recorder)
			if !completed {
				result.Err = fmt.Errorf("merge conflict was not auto-resolved (see logs)")
				return
			}
			result.Conflict = backup
		}
		needPush = true
	}
	return
}

// replayGuard remembers the one divergence whose replay already failed, so it
// is not attempted again until something about it changes. It is per-repository
// loop state, owned by runRepoLoop.
//
// Retrying is not merely wasteful, it does not terminate: a rebase rewrites the
// working tree and so does the abort that undoes it, and those writes are the
// file events that schedule the next cycle. Without this guard, one conflicting
// divergence has the daemon replaying and aborting the same rebase every settle
// window — fetching the remote each time — until a human intervenes.
type replayGuard struct {
	local    string
	upstream string
}

// blocks reports whether this is the same divergence — both sides still at the
// commits they were at — whose replay already failed. Either side moving means
// the situation is new and worth another attempt.
func (g *replayGuard) blocks(local, upstream string) bool {
	return g.local != "" && g.local == local && g.upstream == upstream
}

func (g *replayGuard) remember(local, upstream string) {
	g.local, g.upstream = local, upstream
}

// runCommittedSyncPhase transports only commits that already exist. It never
// stages or creates a commit; a divergence is integrated by replaying the
// local commits onto upstream, never by authoring a merge commit. A push is
// safe with a dirty working tree because it changes the remote ref, not the
// checkout, and a fast-forward is left to git to accept or refuse.
//
// The caller holds the configured save lock. With save-lock coordination
// disabled, git's own overwrite checks remain the guard, but external
// writers have no advisory-lock handshake with the daemon.
func runCommittedSyncPhase(git GitClient, repo config.Repository, guard *replayGuard, logger *slog.Logger) (result cycleResult, branch string, needPush bool) {
	var err error
	branch, err = checkedOutBranch(git, repo.Branch)
	if err != nil {
		result.Err = err
		return
	}
	upstream := repo.Remote + "/" + branch

	ahead, behind, err := git.RevListLeftRightCount(branch, upstream)
	if err != nil {
		result.Err = fmt.Errorf("rev-list: %w", err)
		return
	}
	state := statemachine.Classify(ahead, behind)
	action := statemachine.CommittedSyncActionFor(state)
	result.Action = action
	logger.Info("classified committed-sync state", "state", state.String(), "action", action.String(), "ahead", ahead, "behind", behind)

	switch action {
	case statemachine.NoOp:
		// A dirty tree is fine when no remote commit needs to be checked out.

	case statemachine.Push:
		needPush = true

	case statemachine.FastForwardMerge:
		// Ask git instead of pre-screening the working tree. `git merge
		// --ff-only` refuses — before touching a single file — exactly when
		// checking out the incoming tree would overwrite modified or
		// untracked content, and accepts everything else. Uncommitted work in
		// files the incoming commits leave alone, the usual case for two
		// machines editing different parts of one checkout, therefore no
		// longer holds the branch behind upstream. Trying and being refused
		// is also atomic in a way a status check followed by a merge is not.
		if err := git.MergeFF(upstream); err != nil {
			if refusedOverUncommittedWork(git) {
				result.BlockedReason = BlockedDirtyTree
				result.Err = fmt.Errorf("postponing fast-forward: %w", err)
			} else {
				result.Err = fmt.Errorf("fast-forward merge: %w", err)
			}
			return
		}

	case statemachine.RebaseThenPush:
		result.BlockedReason, needPush, result.Err = replayDivergence(git, branch, upstream, guard, logger)
	}
	return
}

// replayDivergence integrates a divergence the way committed-sync's contract
// allows: the local commits are replayed onto upstream rather than joined to it
// by a merge commit gitloop wrote itself. Two machines committing to the same
// branch is the ordinary way a shared checkout diverges, and neither side's
// commits need a human to reconcile them. Rewriting them is safe because they
// are unpushed — that is what Ahead means.
func replayDivergence(git GitClient, branch, upstream string, guard *replayGuard, logger *slog.Logger) (blockedReason string, needPush bool, err error) {
	localHead, err := git.RevParse(branch)
	if err != nil {
		return "", false, fmt.Errorf("resolving %s: %w", branch, err)
	}
	upstreamHead, err := git.RevParse(upstream)
	if err != nil {
		return "", false, fmt.Errorf("resolving %s: %w", upstream, err)
	}
	if guard.blocks(localHead, upstreamHead) {
		return BlockedDiverged, false, fmt.Errorf("replaying local commits onto %s already failed and neither side has moved since; manual merge required", upstream)
	}

	paused, err := git.Rebase(upstream)
	if paused {
		guard.remember(localHead, upstreamHead)
		// A conflict is the divergence the user has to merge by hand; a
		// failing commit hook or an unavailable signing key pauses a rebase
		// identically and needs a different fix entirely, so the blocked
		// status is only claimed when a conflicted path is actually there.
		conflicted, listErr := git.ConflictedFiles()
		if listErr != nil {
			logger.Warn("could not list conflicted paths for a paused rebase", "error", listErr)
		}
		if len(conflicted) > 0 {
			blockedReason = BlockedDiverged
		}
		// Leaving the rebase paused would block every later cycle at PreCheck
		// and hand the user a half-applied branch to reason about. Abort puts
		// the branch and working tree back exactly as they were.
		if abortErr := git.RebaseAbort(); abortErr != nil {
			return "", false, fmt.Errorf("replaying local commits onto %s stopped and could not be aborted: %w", upstream, abortErr)
		}
		return blockedReason, false, fmt.Errorf("replaying local commits onto %s stopped: %w", upstream, err)
	}
	if err != nil {
		// A rebase, unlike a fast-forward, refuses over any uncommitted change
		// rather than only the ones in its way — so this is the refusal to
		// expect on a shared checkout, labeled the same way.
		if refusedOverUncommittedWork(git) {
			return BlockedDirtyTree, false, fmt.Errorf("postponing rebase: %w", err)
		}
		return "", false, fmt.Errorf("rebase: %w", err)
	}
	return "", true, nil
}

// refusedOverUncommittedWork labels a refused fast-forward or rebase without
// parsing git's localizable message. Uncommitted work is the refusal to expect
// in both places, so a dirty tree earns the blocked status that tells the user
// what to do.
//
// The attribution is a guess, not a diagnosis: the classification is already
// stale by the time the command runs, so a human committing in between turns
// one refusal into another and gets reported as a dirty tree. Only the label
// is at stake — the git error saying what actually happened is kept either way.
func refusedOverUncommittedWork(git GitClient) bool {
	// A status that won't even read is no basis for blaming the user's edits.
	entries, err := git.StatusPorcelain()
	return err == nil && len(entries) > 0
}

// checkedOutBranch resolves the branch to operate on and refuses to use a
// configured branch that is not currently checked out. Git's merge and
// fast-forward commands always update the checked-out branch, so classifying
// one branch and then updating another would be silently destructive.
func checkedOutBranch(git GitClient, configured string) (string, error) {
	current, err := git.CurrentBranch()
	if err != nil {
		return "", fmt.Errorf("current branch: %w", err)
	}
	if configured != "" && configured != current {
		return "", fmt.Errorf("configured branch %q is not checked out (currently on %q)", configured, current)
	}
	if configured != "" {
		return configured, nil
	}
	return current, nil
}

// commitIfDirty stages and commits any pending working-tree changes. It
// returns false, nil if the working tree is already clean.
func commitIfDirty(git GitClient, hostname string, logger *slog.Logger) (bool, error) {
	entries, err := git.StatusPorcelain()
	if err != nil {
		return false, err
	}
	changes := changesFromStatus(entries)
	if len(changes) == 0 {
		return false, nil
	}

	if err := git.AddAll(); err != nil {
		return false, err
	}
	msg := commitmsg.Build(hostname, time.Now(), changes)
	if err := git.Commit(msg); err != nil {
		return false, err
	}
	logger.Info("committed working tree changes", "message", msg, "files", len(changes))
	return true, nil
}

func changesFromStatus(entries []gitcmd.StatusEntry) []commitmsg.Change {
	var changes []commitmsg.Change
	for _, e := range entries {
		if e.IsConflicted() {
			// Guarded against by PreCheck in the normal case; skip
			// defensively rather than mis-categorize.
			continue
		}
		changes = append(changes, commitmsg.Change{Kind: classifyChange(e), Path: e.Path})
	}
	return changes
}

func classifyChange(e gitcmd.StatusEntry) commitmsg.Kind {
	switch {
	case e.OrigPath != "":
		return commitmsg.Renamed
	case e.X == '?' || e.X == 'A':
		return commitmsg.Added
	case e.X == 'D' || e.Y == 'D':
		return commitmsg.Deleted
	default:
		return commitmsg.Updated
	}
}
