package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kohii/gitloop/internal/commitmsg"
	"github.com/kohii/gitloop/internal/config"
)

// resolveConflicts is called after git.Merge reports a conflict. A merge
// exposes every conflicting file in a single stop (unlike a rebase, which
// can pause repeatedly as it replays commits one by one), so this applies
// the configured policy once per cycle and then creates the merge commit
// itself — it never calls back into git for a second round.
//
// It returns completed=true only if the merge commit was created. backup
// reports whether the backup policy (rather than a clean claude resolution)
// is what got it there: the caller uses this to tell "merged, and unresolved
// conflict backups now need human attention" apart from "merged cleanly".
//
// On failure it always leaves the repository in a clean, non-merging state
// by aborting the merge.
func resolveConflicts(git GitClient, repoPath, upstream string, policy config.OnConflict, hostname string, logger *slog.Logger, recorder *statusRecorder) (completed, backup bool) {
	files, err := git.ConflictedFiles()
	if err != nil {
		logger.Error("listing conflicted files failed", "error", err)
		abortMerge(git, logger)
		return false, false
	}
	if len(files) == 0 {
		logger.Warn("merge reported a conflict but no conflicted files were found; aborting defensively")
		abortMerge(git, logger)
		return false, false
	}

	if policy == config.OnConflictClaude && isClaudeAvailable() {
		if tryResolveWithClaude(git, repoPath, files, logger, recorder) {
			msg := commitmsg.BuildConflictResolution(hostname, time.Now(), files, true)
			if err := git.Commit(msg); err != nil {
				logger.Error("committing AI-resolved merge failed", "error", err)
				abortMerge(git, logger)
				return false, false
			}
			return true, false
		}
		logger.Warn("AI conflict resolution failed, falling back to backup policy", "files", files)
	} else if policy == config.OnConflictClaude {
		logger.Warn("on_conflict is \"claude\" but the claude CLI or ANTHROPIC_API_KEY is unavailable, falling back to backup policy")
	}

	if !backupAndAcceptTheirs(git, repoPath, upstream, files, hostname, logger) {
		return false, false
	}
	return true, true
}

func abortMerge(git GitClient, logger *slog.Logger) {
	if err := git.MergeAbort(); err != nil {
		logger.Error("merge --abort failed", "error", err)
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

func tryResolveWithClaude(git GitClient, repoPath string, files []string, logger *slog.Logger, recorder *statusRecorder) bool {
	for _, f := range files {
		if err := runClaudeOnFile(repoPath, f); err != nil {
			logger.Warn("claude failed while resolving a conflict", "file", f, "error", err)
			recordAIResolveFailure(recorder, repoPath, fmt.Sprintf("claude failed on %s: %v", f, err), logger)
			return false
		}
		if hasConflictMarkers(filepath.Join(repoPath, f)) {
			logger.Warn("claude left conflict markers in place", "file", f)
			recordAIResolveFailure(recorder, repoPath, fmt.Sprintf("claude left conflict markers in %s", f), logger)
			return false
		}
		if err := git.AddPath(f); err != nil {
			logger.Error("git add after claude resolution failed", "file", f, "error", err)
			recordAIResolveFailure(recorder, repoPath, fmt.Sprintf("git add after claude resolution failed for %s: %v", f, err), logger)
			return false
		}
	}
	if err := recorder.recordAIResolveSuccess(repoPath); err != nil {
		logger.Error("writing status file failed", "error", err)
	}
	return true
}

// recordAIResolveFailure is a small wrapper around
// statusRecorder.recordAIResolveFailure that logs (rather than propagates) a
// status-file write error, matching how every other status update in this
// package treats status reporting as best-effort.
func recordAIResolveFailure(recorder *statusRecorder, repoPath, reason string, logger *slog.Logger) {
	if err := recorder.recordAIResolveFailure(repoPath, reason); err != nil {
		logger.Error("writing status file failed", "error", err)
	}
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

// backupAndAcceptTheirs preserves both sides of each conflicted file next to
// the original (see writeBackupFile for the naming scheme), then resolves
// the conflict by accepting the incoming (upstream) side and committing the
// merge.
//
// Upstream is treated as the side of record because it has already been
// pushed and may be visible to other devices; the local ("ours") side is
// still-unshared and would otherwise be silently discarded, so it's rescued
// into a backup file for the user to reconcile by hand instead. Nothing is
// lost: both sides' content is preserved in the backup files, and the
// commit that created the local side remains reachable from git history.
func backupAndAcceptTheirs(git GitClient, repoPath, upstream string, files []string, hostname string, logger *slog.Logger) bool {
	ts := time.Now().Format("20060102150405")

	for _, f := range files {
		oursSaved := backupStage(git, repoPath, f, 2, "ours", hostname, ts, logger)
		theirsSaved := backupStage(git, repoPath, f, 3, "theirs", hostname, ts, logger)
		logger.Warn("backed up conflicting file and accepted the upstream version",
			"file", f, "ours_saved", oursSaved, "theirs_saved", theirsSaved)

		if err := git.CheckoutTheirs(f); err != nil {
			logger.Error("checkout --theirs failed", "file", f, "error", err)
			abortMerge(git, logger)
			return false
		}
		if err := git.AddPath(f); err != nil {
			logger.Error("git add for conflicting file failed", "file", f, "error", err)
			abortMerge(git, logger)
			return false
		}
	}

	if err := git.AddAll(); err != nil {
		logger.Error("staging conflict backup files failed", "error", err)
		abortMerge(git, logger)
		return false
	}

	msg := fmt.Sprintf("[%s] %s — merged upstream with backups: %s",
		hostname, time.Now().Format("2006-01-02 15:04"), strings.Join(files, ", "))
	if err := git.Commit(msg); err != nil {
		logger.Error("committing merge with conflict backups failed", "error", err)
		abortMerge(git, logger)
		return false
	}
	logger.Warn("merged upstream, accepting the incoming version of conflicting files; local changes to those files were preserved in backup files",
		"upstream", upstream, "files", files)
	return true
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
