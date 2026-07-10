package daemon

import (
	"context"
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
// max-wait debounced sync cycle, plus a periodic fetch-driven cycle, until
// ctx is canceled.
//
// It returns nil only on a graceful ctx cancellation. Any other return is a
// setup or watcher failure that the caller should treat as retryable.
func runRepoLoop(ctx context.Context, git GitClient, repo config.Repository, hostname string, logger *slog.Logger, recorder *statusRecorder) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer watcher.Close()

	if err := addRecursive(watcher, repo.Path); err != nil {
		return fmt.Errorf("watching %s: %w", repo.Path, err)
	}

	var settleTimer, maxWaitTimer *time.Timer
	defer stopTimer(settleTimer)
	defer stopTimer(maxWaitTimer)

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
		if err := git.Fetch(repo.Remote); err != nil {
			fetchErr = fmt.Errorf("fetch: %w", err)
			logger.Warn("fetch failed; running commit-only cycle", "error", fetchErr)
		}

		// From here on the cycle touches the working tree (auto-commit,
		// merge, conflict resolution), so we need to be the sole writer.
		lock, ok := acquireSaveLockWithRetry(repo.SaveLockPath, repo.Settle, logger)
		if !ok {
			logger.Warn("skipped: save in-flight")
			if err := recorder.update(repo.Path, func(s *RepoStatus) {
				s.LastError = "skipped: save in-flight"
			}); err != nil {
				logger.Error("writing status file failed", "error", err)
			}
			return
		}
		// Safety net so a panic anywhere in the commit/integrate phase can't
		// leak the flock and deadlock every subsequent cycle. The explicit
		// release below is idempotent — this second call is a no-op on the
		// non-panic path.
		defer lock.release()

		// Report `syncing` only now, when we actually start writing — a
		// consumer of status.json watches this to know when to keep out of
		// the working tree, and fetch (still `idle`) doesn't need that
		// exclusion.
		if err := recorder.update(repo.Path, func(s *RepoStatus) { s.Phase = PhaseSyncing }); err != nil {
			logger.Error("writing status file failed", "error", err)
		}

		result = runCommitPhase(git, hostname, logger)
		var branch string
		var needPush bool
		if result.Err == nil && fetchErr == nil {
			integrateResult, b, n := runIntegratePhase(git, repo, hostname, logger, recorder)
			// Preserve Committed from the commit phase — runIntegratePhase
			// returns a fresh cycleResult that doesn't know about it.
			integrateResult.Committed = result.Committed
			result, branch, needPush = integrateResult, b, n
		}
		if result.Err == nil && fetchErr != nil {
			result.Err = fetchErr
		}

		lock.release()

		// Return phase to idle before push: the working tree is no longer
		// being touched, so an external writer sharing this repo can safely
		// proceed. Push is network I/O only.
		if err := recorder.update(repo.Path, func(s *RepoStatus) { s.Phase = PhaseIdle }); err != nil {
			logger.Error("writing status file failed", "error", err)
		}

		// Push is network I/O, run outside the lock so an external writer
		// isn't blocked on a slow remote.
		if result.Err == nil && needPush {
			if err := git.Push(repo.Remote, branch); err != nil {
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
		}
	}
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
			s.Phase = PhaseConflict
		case result.Err != nil:
			s.LastError = result.Err.Error()
			s.Phase = PhaseIdle
		default:
			s.LastError = ""
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
