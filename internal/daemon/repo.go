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
	Err      error
}

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
	branch = repo.Branch
	if branch == "" {
		b, err := git.CurrentBranch()
		if err != nil {
			result.Err = fmt.Errorf("current branch: %w", err)
			return
		}
		branch = b
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
