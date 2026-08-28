package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/kohii/gitloop/internal/config"
	"github.com/kohii/gitloop/internal/statemachine"
)

// runRepoLoop watches repo.Path for file changes and drives the settle /
// max-wait debounced sync cycle, plus a periodic timer-driven cycle and any
// out-of-band request arriving on trigger, until ctx is canceled. Under
// config.ModeCommitOnly the cycle stops after the auto-commit phase.
//
// A nil trigger means this loop has no out-of-band source, which is how tests
// that only exercise the timers run.
//
// It returns nil only on a graceful ctx cancellation. Any other return is a
// setup or watcher failure that the caller should treat as retryable.
func runRepoLoop(ctx context.Context, git GitClient, repo config.Repository, trigger *repoTrigger, hostname string, logger *slog.Logger, recorder *statusRecorder) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer watcher.Close()

	if err := addRecursive(watcher, repo.Path); err != nil {
		return fmt.Errorf("watching %s: %w", repo.Path, err)
	}

	repo = resolveRepositoryMode(git, repo, logger)

	// A commit-only repository has no remote to talk to, so its cycle stops
	// after the auto-commit phase.
	syncsRemote := repo.SyncsRemote()
	if !syncsRemote {
		logger.Info("commit-only mode: this repository will not fetch, merge, or push")
	}

	var settleTimer, maxWaitTimer *time.Timer
	defer stopTimer(settleTimer)
	defer stopTimer(maxWaitTimer)

	// With nothing to fetch, this timer still earns its keep as a safety net
	// for working-tree changes the watcher missed (dropped fsnotify events,
	// edits made while the daemon was down).
	fetchTicker := time.NewTicker(repo.FetchInterval)
	defer fetchTicker.Stop()

	runCycle := func(trigger string) {
		logger.Info("running sync cycle", "trigger", trigger)

		// PreCheck + fetch are safe outside the save lock: they never write
		// to the working tree (fetch only writes under .git/), and holding
		// the lock across fetch would block any external writer for the full
		// duration of a network round-trip.
		result, proceed := runPreCheckPhase(repo.Path, logger)
		if !proceed {
			applyCycleResult(recorder, repo.Path, result, logger)
			return
		}

		// Fetch failure does not bail us out of the cycle: when the network
		// is down (laptop offline, upstream unreachable) the daemon must
		// still auto-commit local edits so they aren't lost while the user
		// is disconnected. The error is remembered and surfaced via
		// LastError; integrate + push are skipped because we no longer have
		// fresh upstream data to classify against.
		var fetchErr error
		if syncsRemote {
			remoteCtx, cancel := remoteCommandContext(ctx, repo.RemoteTimeout)
			err := git.Fetch(remoteCtx, repo.Remote)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				fetchErr = fmt.Errorf("fetch: %w", annotateRemoteError(err, repo.RemoteTimeout))
				if repo.AutoCommits() {
					// Not called a "commit-only cycle": that names a configured
					// mode, and this is a transient failure in a repo that syncs.
					logger.Warn("fetch failed; committing local changes only this cycle", "error", fetchErr)
				} else {
					logger.Warn("fetch failed; committed-sync cycle deferred", "error", fetchErr)
				}
			}
		}

		// A committed-sync cycle has no local write phase when fetch fails:
		// there is no auto-commit to preserve, and integration against stale
		// upstream data would be unsafe. Surface the error immediately.
		if repo.IsCommittedSync() && fetchErr != nil {
			result.Err = fetchErr
			applyCycleResult(recorder, repo.Path, result, logger)
			return
		}

		// Look before taking the save lock. Most cycles — every timer tick on
		// a repository nobody has touched — turn out to have no work at all,
		// and holding the lock through one of those would shut an external
		// writer out of the working tree for no reason.
		obs, branch, err := observeCycle(git, repo, syncsRemote, fetchErr == nil)
		if err != nil {
			result.Err = err
			applyCycleResult(recorder, repo.Path, result, logger)
			return
		}
		plan := planCycle(repo.AutoCommits(), obs)
		logger.Info("planned sync cycle", "intent", plan.Intent.String(), "dirty", obs.Dirty, "upstream", upstreamLabel(obs.Upstream))

		var needPush bool
		switch plan.Intent {
		case intentNothingToDo:
			result.Err = fetchErr
			applyCycleResult(recorder, repo.Path, result, logger)
			return

		case intentBlocked:
			result.BlockedReason = plan.BlockedReason
			result.Err = fmt.Errorf("local and upstream histories diverged; manual merge required")
			applyCycleResult(recorder, repo.Path, result, logger)
			return

		case intentPush:
			// Push sends refs to the remote without touching the checkout, so
			// it needs neither the save lock nor the syncing phase.
			result.Action = statemachine.Push
			needPush = true

		case intentMutate:
			var ran bool
			result, branch, needPush, ran = runLockedPhases(git, repo, hostname, logger, recorder, fetchErr == nil)
			if !ran {
				return
			}
			if result.Err == nil && fetchErr != nil {
				result.Err = fetchErr
			}
		}

		// Push is network I/O, run outside the lock so an external writer
		// isn't blocked on a slow remote.
		if result.Err == nil && needPush {
			remoteCtx, cancel := remoteCommandContext(ctx, repo.RemoteTimeout)
			err := git.Push(remoteCtx, repo.Remote, branch)
			cancel()
			if ctx.Err() != nil {
				if err == nil {
					result.Pushed = true
					applyCycleResult(recorder, repo.Path, result, logger)
				} else {
					applyInterruptedCycleProgress(recorder, repo.Path, result, logger)
				}
				return
			}
			if err != nil {
				err = annotateRemoteError(err, repo.RemoteTimeout)
				// Preserve the "why are we pushing" context — a push after a
				// merge commit vs. a bare push of local-only commits are
				// meaningfully different failures at debug time.
				if result.Action == statemachine.MergeThenPush {
					result.Err = fmt.Errorf("push after merge: %w", err)
				} else {
					result.Err = fmt.Errorf("push: %w", err)
				}
			} else {
				result.Pushed = true
			}
		}

		applyCycleResult(recorder, repo.Path, result, logger)
	}

	if repo.IsCommittedSync() {
		// Manual commits change .git rather than the watched working tree, so
		// there may be no fsnotify event after the user commits. Run once at
		// startup to avoid waiting for the first interval tick.
		runCycle("startup")
	}

	for {
		var settleC, maxWaitC <-chan time.Time
		if settleTimer != nil {
			settleC = settleTimer.C
		}
		if maxWaitTimer != nil {
			maxWaitC = maxWaitTimer.C
		}

		select {
		case <-ctx.Done():
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("watcher events channel closed unexpectedly")
			}
			if shouldIgnore(event.Name) {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
					if err := addRecursive(watcher, event.Name); err != nil {
						logger.Warn("failed to watch new directory", "path", event.Name, "error", err)
					}
				}
			}

			if settleTimer == nil {
				settleTimer = time.NewTimer(repo.Settle)
			} else {
				stopTimer(settleTimer)
				settleTimer.Reset(repo.Settle)
			}
			if maxWaitTimer == nil {
				maxWaitTimer = time.NewTimer(repo.MaxWait)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("watcher errors channel closed unexpectedly")
			}
			logger.Warn("watcher error", "error", err)

		case <-settleC:
			settleTimer = nil
			stopTimer(maxWaitTimer)
			maxWaitTimer = nil
			runCycle("settle")

		case <-maxWaitC:
			stopTimer(settleTimer)
			settleTimer = nil
			maxWaitTimer = nil
			runCycle("max-wait")

		case <-fetchTicker.C:
			// The commit step in the integrate phase is a no-op on a clean
			// working tree, so reusing the same cycle here naturally gives
			// us "fetch, and integrate if behind/diverged" without a
			// separate code path.
			runCycle("fetch-interval")

		case <-trigger.signals():
			reason := trigger.take()
			if reason == "" {
				// A signal whose request an earlier pass already took.
				continue
			}
			// An environment event that lands mid-debounce defers to it: the
			// edit in progress will be committed within `settle` anyway, and
			// cutting that short to commit a half-written file is the exact
			// thing the debounce exists to prevent. A person asking for a
			// sync has already decided otherwise, so their request runs.
			if reason.isEnvironmental() && settleTimer != nil {
				logger.Info("environment trigger deferred to the pending settle window", "trigger", reason)
				continue
			}
			runCycle(string(reason))
		}
	}
}

// remoteCommandContext applies the repository's network-operation policy
// while preserving cancellation from daemon shutdown. Parsed configuration
// always supplies a positive value; the fallback keeps hand-built Repository
// values finite as a defense in depth.
func remoteCommandContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = config.DefaultRemoteTimeout
	}
	return context.WithTimeout(parent, timeout)
}

func annotateRemoteError(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		if timeout <= 0 {
			timeout = config.DefaultRemoteTimeout
		}
		return fmt.Errorf("timed out after %s: %w", timeout, err)
	}
	return err
}

// applyInterruptedCycleProgress records local work that completed before a
// shutdown canceled push. It deliberately preserves remote health fields: an
// interrupted push is neither a successful sync nor a new persistent error.
func applyInterruptedCycleProgress(recorder *statusRecorder, repoPath string, result cycleResult, logger *slog.Logger) {
	err := recorder.update(repoPath, func(s *RepoStatus) {
		if result.Committed {
			s.LastCommit = time.Now().Format(time.RFC3339)
		}
		if result.Conflict {
			s.Phase = PhaseConflict
		}
	})
	if err != nil {
		logger.Error("writing interrupted cycle progress failed", "error", err)
	}
}

// resolveRepositoryMode turns the legacy omitted mode into commit-only when
// the Git repository has no remotes. An explicit mode or workflow always wins:
// in particular, mode: sync remains a loud choice when its configured remote
// is missing or misspelled. A failure to inspect remotes is also not treated
// as proof that there are none, so the existing sync-mode error remains visible.
func resolveRepositoryMode(git GitClient, repo config.Repository, logger *slog.Logger) config.Repository {
	if repo.Mode == config.ModeCommitOnly || repo.ModeWasExplicitlySet() ||
		repo.WorkflowWasExplicitlySet() || repo.IsCommittedSync() {
		return repo
	}

	remotes, err := git.RemoteNames()
	if err != nil {
		logger.Warn("failed to inspect git remotes; keeping sync mode", "error", err)
		return repo
	}
	if len(remotes) == 0 {
		repo.Mode = config.ModeCommitOnly
		repo.Workflow = config.WorkflowAutoCommitOnly
		logger.Info("no git remotes configured; using commit-only mode")
	}
	return repo
}

func applyCycleResult(recorder *statusRecorder, repoPath string, result cycleResult, logger *slog.Logger) {
	err := recorder.update(repoPath, func(s *RepoStatus) {
		now := time.Now()
		nowStr := now.Format(time.RFC3339)
		if result.Committed {
			s.LastCommit = nowStr
		}
		if result.Pushed {
			s.LastPush = nowStr
		}
		switch {
		case result.Skipped != "":
			// PreCheck only skips a cycle when it finds a rebase or merge
			// already in progress — a git-level state that (like a backed
			// up conflict) needs attention before it's safe to save into
			// the repository again.
			s.LastError = "skipped: " + result.Skipped
			s.BlockedReason = BlockedOperation
			s.Phase = PhaseConflict
		case result.Err != nil:
			s.LastError = result.Err.Error()
			s.BlockedReason = result.BlockedReason
			s.Phase = PhaseIdle
		default:
			s.LastError = ""
			s.BlockedReason = ""
			s.LastSuccessfulSyncAt = now
			if result.Conflict {
				s.Phase = PhaseConflict
			} else {
				s.Phase = PhaseIdle
			}
		}
	})
	if err != nil {
		logger.Error("writing status file failed", "error", err)
	}
	if result.Err != nil {
		logger.Error("sync cycle failed", "error", result.Err)
	}
}

// addRecursive adds watches for root and every subdirectory except .git.
func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		return w.Add(path)
	})
}

// shouldIgnore reports whether path falls under a .git directory. Watches
// are never added under .git (see addRecursive), so this is a defensive
// second layer against self-triggering on gitloop's own commits.
func shouldIgnore(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}

func stopTimer(t *time.Timer) {
	if t == nil {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
