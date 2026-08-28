package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kohii/gitloop/internal/commitmsg"
	"github.com/kohii/gitloop/internal/config"
)

// Index stages of an unmerged path. Stage 1 (the common ancestor) is never
// read: a backup is only worth writing for a side someone actually authored.
const (
	ourStage   = 2
	theirStage = 3
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
		full := filepath.Join(repoPath, f)

		// Only text conflicts — the ones where git wrote <<<<<<< / =======
		// / >>>>>>> markers into the working tree — are safe to hand to
		// claude. Marker-less conflicts (binary merges via merge=binary,
		// git-crypt files, auto-detected binaries, modify/delete conflicts)
		// leave the working tree with just one side's content. There is
		// nothing there for claude to reconcile, and if we then `git add`
		// the file, git silently collapses the higher index stages to
		// whatever the working tree holds — dropping the other side without
		// warning. Refusing claude for these and returning false takes us
		// through the backup path, which preserves both sides via
		// ShowStage.
		has, readOK := readConflictMarkers(full)
		if !readOK {
			logger.Warn("conflicted file could not be read; skipping claude and falling back to backup", "file", f)
			recordAIResolveFailure(recorder, repoPath, fmt.Sprintf("conflicted file %s could not be read", f), logger)
			return false
		}
		if !has {
			logger.Warn("conflicted file has no conflict markers (binary, modify/delete, or opaque merge); skipping claude and falling back to backup", "file", f)
			recordAIResolveFailure(recorder, repoPath, fmt.Sprintf("marker-less conflict in %s cannot be safely resolved by claude", f), logger)
			return false
		}

		if err := runClaudeOnFile(repoPath, f); err != nil {
			logger.Warn("claude failed while resolving a conflict", "file", f, "error", err)
			recordAIResolveFailure(recorder, repoPath, fmt.Sprintf("claude failed on %s: %v", f, err), logger)
			return false
		}
		if stillHas, _ := readConflictMarkers(full); stillHas {
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

// readConflictMarkers reports whether path contains git conflict markers
// (<<<<<<< / ======= / >>>>>>>). readOK is false only when the file cannot
// be opened, so callers can distinguish "no markers present" from "we don't
// know" — the two cases need opposite treatment when deciding whether to run
// claude vs. whether to accept a claude-produced resolution.
func readConflictMarkers(path string) (hasMarkers, readOK bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	s := string(data)
	has := strings.Contains(s, "<<<<<<<") ||
		strings.Contains(s, "=======") ||
		strings.Contains(s, ">>>>>>>")
	return has, true
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
//
// "Accepting upstream" means checking out their version — or, when upstream
// deleted the file, deleting it. Both settle the conflict; what must not
// happen is leaving the merge unfinished with backup files on disk, because
// `merge --abort` does not remove untracked files: the next cycle commits
// them, hits the identical conflict, and writes another set under a fresh
// timestamp, growing the pile once per cycle while the branch stays diverged
// forever. Every bail-out therefore deletes the backups it wrote (see
// abortCycle), so a retry starts from the same state this cycle did instead
// of carrying its leftovers.
func backupAndAcceptTheirs(git GitClient, repoPath, upstream string, files []string, hostname string, logger *slog.Logger) bool {
	ts := time.Now().Format("20060102150405")
	var written []string

	for _, f := range files {
		stages, err := git.ConflictStages(f)
		if err != nil {
			logger.Error("reading conflict stages failed", "file", f, "error", err)
			abortCycle(git, written, logger)
			return false
		}
		oursPresent := slices.Contains(stages, ourStage)
		theirsPresent := slices.Contains(stages, theirStage)

		oursBackup := backupStage(git, repoPath, f, ourStage, "ours", hostname, ts, logger)
		theirsBackup := backupStage(git, repoPath, f, theirStage, "theirs", hostname, ts, logger)
		for _, p := range []string{oursBackup, theirsBackup} {
			if p != "" {
				written = append(written, p)
			}
		}

		if oursPresent && oursBackup == "" {
			// The local side is unpushed, and the commit carrying it is only
			// reachable through the losing parent of a merge — which a
			// path-limited `git log` skips by default. Retry next cycle
			// rather than accept upstream over content we failed to rescue.
			logger.Error("local side could not be backed up; leaving the conflict for the next cycle", "file", f)
			abortCycle(git, written, logger)
			return false
		}

		if theirsPresent {
			logger.Warn("backed up conflicting file and accepted the upstream version",
				"file", f, "ours_saved", oursBackup != "", "theirs_saved", theirsBackup != "")
			if err := git.CheckoutTheirs(f); err != nil {
				logger.Error("checkout --theirs failed", "file", f, "error", err)
				abortCycle(git, written, logger)
				return false
			}
			if err := git.AddPath(f); err != nil {
				logger.Error("git add for conflicting file failed", "file", f, "error", err)
				abortCycle(git, written, logger)
				return false
			}
			continue
		}

		// Upstream deleted the file. There is no version to check out, so
		// accepting upstream means removing it; the local content survives
		// in the .ours backup.
		logger.Warn("backed up conflicting file and accepted the upstream deletion",
			"file", f, "ours_saved", oursBackup != "")
		if err := git.RemovePath(f); err != nil {
			logger.Error("removing file deleted upstream failed", "file", f, "error", err)
			abortCycle(git, written, logger)
			return false
		}
	}

	if err := git.AddAll(); err != nil {
		logger.Error("staging conflict backup files failed", "error", err)
		abortCycle(git, written, logger)
		return false
	}

	msg := fmt.Sprintf("[%s] %s — merged upstream with backups: %s",
		hostname, time.Now().Format("2006-01-02 15:04"), strings.Join(files, ", "))
	if err := git.Commit(msg); err != nil {
		logger.Error("committing merge with conflict backups failed", "error", err)
		abortCycle(git, written, logger)
		return false
	}
	logger.Warn("merged upstream, accepting the incoming version of conflicting files; local changes to those files were preserved in backup files",
		"upstream", upstream, "files", files)
	return true
}

// abortCycle undoes a partial backup resolution: it deletes the backup files
// written so far, then aborts the merge. Deleting them is what keeps a failed
// cycle from being cumulative — they are untracked, so aborting the merge
// alone would leave them for the next cycle to commit.
func abortCycle(git GitClient, written []string, logger *slog.Logger) {
	for _, p := range written {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.Error("removing a conflict backup after a failed resolution failed", "path", p, "error", err)
		}
	}
	abortMerge(git, logger)
}

// backupStage writes one side of a conflict to a backup file and returns the
// path it wrote. The path is empty when nothing was written: either that
// stage does not exist (one side deleted the file — an expected shape) or its
// content could not be read or written.
func backupStage(git GitClient, repoPath, file string, stage int, side, hostname, ts string, logger *slog.Logger) string {
	content, ok, err := git.ShowStage(stage, file)
	if err != nil {
		logger.Error("reading conflict side failed", "file", file, "side", side, "error", err)
		return ""
	}
	if !ok {
		return ""
	}
	path, err := writeBackupFile(repoPath, file, hostname, ts, side, content)
	if err != nil {
		logger.Error("writing conflict backup file failed", "file", file, "side", side, "error", err)
		return ""
	}
	return path
}

// writeBackupFile writes content to "<name>.conflict.<host>.<timestamp>.<side><ext>"
// next to the original file and returns the path it wrote.
func writeBackupFile(repoPath, file, hostname, ts, side, content string) (string, error) {
	dir := filepath.Dir(file)
	ext := filepath.Ext(file)
	base := strings.TrimSuffix(filepath.Base(file), ext)
	name := fmt.Sprintf("%s.conflict.%s.%s.%s%s", base, hostname, ts, side, ext)
	path := filepath.Join(repoPath, dir, name)
	return path, os.WriteFile(path, []byte(content), 0o644)
}
