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

// runSyncCycle runs one full sync cycle against git: guard, auto-commit,
// fetch, classify, act. It never panics; git/filesystem failures are
// reported via cycleResult.Err instead so the caller can log and continue
// rather than crash the watch loop.
func runSyncCycle(git GitClient, repo config.Repository, hostname string, logger *slog.Logger, recorder *statusRecorder) cycleResult {
	guard, err := statemachine.PreCheck(repo.Path)
	if err != nil {
		return cycleResult{Err: fmt.Errorf("precheck: %w", err)}
	}
	if !guard.Safe {
		logger.Warn("skipping sync cycle: repository has a rebase/merge in progress", "reason", guard.Reason)
		return cycleResult{Skipped: guard.Reason}
	}

	committed, err := commitIfDirty(git, hostname, logger)
	if err != nil {
		return cycleResult{Err: fmt.Errorf("auto-commit: %w", err)}
	}
	result := cycleResult{Committed: committed}

	if err := git.Fetch(repo.Remote); err != nil {
		result.Err = fmt.Errorf("fetch: %w", err)
		return result
	}

	branch := repo.Branch
	if branch == "" {
		b, err := git.CurrentBranch()
		if err != nil {
			result.Err = fmt.Errorf("current branch: %w", err)
			return result
		}
		branch = b
	}
	upstream := repo.Remote + "/" + branch

	ahead, behind, err := git.RevListLeftRightCount(branch, upstream)
	if err != nil {
		result.Err = fmt.Errorf("rev-list: %w", err)
		return result
	}
	state := statemachine.Classify(ahead, behind)
	action := statemachine.ActionFor(state)
	result.Action = action
	logger.Info("classified sync state", "state", state.String(), "action", action.String(), "ahead", ahead, "behind", behind)

	switch action {
	case statemachine.NoOp:
		// Nothing to do.

	case statemachine.Push:
		if err := git.Push(repo.Remote, branch); err != nil {
			result.Err = fmt.Errorf("push: %w", err)
			return result
		}
		result.Pushed = true

	case statemachine.FastForwardMerge:
		if err := git.MergeFF(upstream); err != nil {
			result.Err = fmt.Errorf("fast-forward merge: %w", err)
			return result
		}

	case statemachine.MergeThenPush:
		conflict, err := git.Merge(upstream)
		if err != nil {
			result.Err = fmt.Errorf("merge: %w", err)
			return result
		}
		if conflict {
			logger.Warn("merge stopped on a conflict, applying conflict policy", "on_conflict", repo.OnConflict)
			completed, backup := resolveConflicts(git, repo.Path, upstream, repo.OnConflict, hostname, logger, recorder)
			if !completed {
				result.Err = fmt.Errorf("merge conflict was not auto-resolved (see logs)")
				return result
			}
			result.Conflict = backup
		}
		if err := git.Push(repo.Remote, branch); err != nil {
			result.Err = fmt.Errorf("push after merge: %w", err)
			return result
		}
		result.Pushed = true
	}

	return result
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
