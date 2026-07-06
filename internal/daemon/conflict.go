package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kohii/gitloop/internal/config"
)

// maxConflictStops bounds how many times resolveConflicts will drive
// `git rebase --continue` into a fresh conflict before giving up. This
// covers a multi-commit rebase where more than one commit conflicts; it is
// not expected to be hit in practice.
const maxConflictStops = 50

// resolveConflicts is called after git.Rebase (or RebaseContinue) reports a
// conflict. It applies the configured policy and, on success, drives the
// rebase to completion. It returns true only if the rebase fully completed
// (with all conflicts resolved via git add + rebase --continue).
//
// On failure it always leaves the repository in a clean, non-rebasing
// state: either the backup policy already ran (which aborts the rebase and
// resets to upstream), or resolveConflicts aborts it directly.
func resolveConflicts(git GitClient, repoPath, upstream string, policy config.OnConflict, hostname string, logger *slog.Logger) bool {
	for i := 0; i < maxConflictStops; i++ {
		files, err := git.ConflictedFiles()
		if err != nil {
			logger.Error("listing conflicted files failed", "error", err)
			abortRebase(git, logger)
			return false
		}

		if len(files) > 0 {
			resolved := false
			if policy == config.OnConflictClaude && isClaudeAvailable() {
				resolved = tryResolveWithClaude(git, repoPath, files, logger)
				if !resolved {
					logger.Warn("claude conflict resolution failed, falling back to backup policy", "files", files)
				}
			} else if policy == config.OnConflictClaude {
				logger.Warn("on_conflict is \"claude\" but the claude CLI or ANTHROPIC_API_KEY is unavailable, falling back to backup policy")
			}

			if !resolved {
				backupAndAbort(git, repoPath, upstream, files, hostname, logger)
				return false
			}
		}

		conflict, err := git.RebaseContinue()
		if err != nil {
			logger.Error("rebase --continue failed", "error", err)
			abortRebase(git, logger)
			return false
		}
		if !conflict {
			return true
		}
		// Another commit in the same rebase conflicted; loop and resolve it.
	}

	logger.Error("rebase kept conflicting past the retry limit, aborting", "limit", maxConflictStops)
	abortRebase(git, logger)
	return false
}

func abortRebase(git GitClient, logger *slog.Logger) {
	if err := git.RebaseAbort(); err != nil {
		logger.Error("rebase --abort failed", "error", err)
	}
}

// isClaudeAvailable reports whether the claude conflict policy can run:
// both an API key and the claude binary are required. It is a package
// variable so tests can override it without touching the process
// environment or PATH.
var isClaudeAvailable = func() bool {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return false
	}
	_, err := exec.LookPath("claude")
	return err == nil
}

// runClaudeOnFile invokes `claude -p` to resolve the conflict markers in
// file in place. It is a package variable so tests can stub it out instead
// of shelling out to a real claude binary.
var runClaudeOnFile = func(repoPath, file string) error {
	prompt := fmt.Sprintf(
		"Resolve git conflict markers in %s. Output only the resolved file. Preserve intent from both sides.",
		file,
	)
	cmd := exec.Command("claude", "-p",
		"--allowedTools", "Read,Edit",
		"--output-format", "json",
		"--max-turns", "5",
		prompt,
	)
	cmd.Dir = repoPath
	return cmd.Run()
}

func tryResolveWithClaude(git GitClient, repoPath string, files []string, logger *slog.Logger) bool {
	for _, f := range files {
		if err := runClaudeOnFile(repoPath, f); err != nil {
			logger.Warn("claude failed while resolving a conflict", "file", f, "error", err)
			return false
		}
		if hasConflictMarkers(filepath.Join(repoPath, f)) {
			logger.Warn("claude left conflict markers in place", "file", f)
			return false
		}
		if err := git.AddPath(f); err != nil {
			logger.Error("git add after claude resolution failed", "file", f, "error", err)
			return false
		}
	}
	return true
}

func hasConflictMarkers(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		// Can't verify the file is clean; treat it as unresolved.
		return true
	}
	s := string(data)
	return strings.Contains(s, "<<<<<<<") || strings.Contains(s, "=======") || strings.Contains(s, ">>>>>>>")
}

// backupAndAbort preserves both sides of each conflicted file next to the
// original (see writeBackupFile for the naming scheme), aborts the rebase,
// and then resets the branch to upstream and commits the backup files on
// top of it.
//
// Resetting to upstream (rather than leaving the pre-rebase, still-diverged
// branch in place) is what makes this a terminal outcome instead of a
// retry loop: the local commits that caused the conflict are discarded from
// the branch, so the next cycle sees Equal/Ahead instead of hitting the
// exact same conflict again. Nothing is lost — both sides' content is
// preserved in the backup files (and the discarded commits remain reachable
// from the reflog) for the user to reconcile by hand.
func backupAndAbort(git GitClient, repoPath, upstream string, files []string, hostname string, logger *slog.Logger) {
	ts := time.Now().Format("20060102150405")

	for _, f := range files {
		oursSaved := backupStage(git, repoPath, f, 2, "ours", hostname, ts, logger)
		theirsSaved := backupStage(git, repoPath, f, 3, "theirs", hostname, ts, logger)
		logger.Warn("backed up conflicting file instead of auto-resolving",
			"file", f, "ours_saved", oursSaved, "theirs_saved", theirsSaved)
	}

	if err := git.RebaseAbort(); err != nil {
		logger.Error("rebase --abort failed", "error", err)
		return
	}
	if err := git.ResetHard(upstream); err != nil {
		logger.Error("resetting to upstream after conflict backup failed", "error", err)
		return
	}
	if err := git.AddAll(); err != nil {
		logger.Error("staging conflict backup files failed", "error", err)
		return
	}
	msg := fmt.Sprintf("[%s] %s — conflict backup: %s", hostname, time.Now().Format("2006-01-02 15:04"), strings.Join(files, ", "))
	if err := git.Commit(msg); err != nil {
		logger.Warn("committing conflict backup files failed (there may be nothing to commit)", "error", err)
	}
	logger.Warn("reset to upstream after conflict; local changes to the conflicted files were discarded from history but preserved in backup files", "upstream", upstream, "files", files)
}

func backupStage(git GitClient, repoPath, file string, stage int, side, hostname, ts string, logger *slog.Logger) bool {
	content, ok, err := git.ShowStage(stage, file)
	if err != nil {
		logger.Error("reading conflict side failed", "file", file, "side", side, "error", err)
		return false
	}
	if !ok {
		// Expected for some conflict shapes, e.g. one side deleted the file.
		return false
	}
	if err := writeBackupFile(repoPath, file, hostname, ts, side, content); err != nil {
		logger.Error("writing conflict backup file failed", "file", file, "side", side, "error", err)
		return false
	}
	return true
}

// writeBackupFile writes content to "<name>.conflict.<host>.<timestamp>.<side><ext>"
// next to the original file.
func writeBackupFile(repoPath, file, hostname, ts, side, content string) error {
	dir := filepath.Dir(file)
	ext := filepath.Ext(file)
	base := strings.TrimSuffix(filepath.Base(file), ext)
	name := fmt.Sprintf("%s.conflict.%s.%s.%s%s", base, hostname, ts, side, ext)
	return os.WriteFile(filepath.Join(repoPath, dir, name), []byte(content), 0o644)
}
