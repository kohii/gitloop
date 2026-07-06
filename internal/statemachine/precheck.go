package statemachine

import (
	"os"
	"path/filepath"
	"strings"
)

// GuardResult is the outcome of PreCheck: whether it is safe to start a sync
// cycle, and if not, why.
type GuardResult struct {
	Safe   bool
	Reason string
}

var safeGuard = GuardResult{Safe: true}

// PreCheck inspects repoPath for signs that a git operation (rebase or
// merge) is already in progress. Sync cycles must not touch a repository in
// that state — doing so could turn a paused rebase/merge into a much harder
// mess to recover from.
//
// PreCheck only performs filesystem checks; it does not shell out to git.
func PreCheck(repoPath string) (GuardResult, error) {
	gitDir, err := resolveGitDir(repoPath)
	if err != nil {
		return GuardResult{}, err
	}
	if gitDir == "" {
		// No .git found; nothing to guard against here. Callers that need a
		// repository to exist should check that separately.
		return safeGuard, nil
	}

	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		if exists(filepath.Join(gitDir, name)) {
			return GuardResult{Safe: false, Reason: "rebase-in-progress"}, nil
		}
	}
	if exists(filepath.Join(gitDir, "MERGE_HEAD")) {
		return GuardResult{Safe: false, Reason: "merge-in-progress"}, nil
	}

	return safeGuard, nil
}

// resolveGitDir returns the absolute path of repoPath's .git directory. It
// follows the "gitdir: <path>" indirection used by worktrees and submodules.
// It returns "" (no error) if repoPath has no .git entry at all.
func resolveGitDir(repoPath string) (string, error) {
	dotGit := filepath.Join(repoPath, ".git")
	info, err := os.Stat(dotGit)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	if info.IsDir() {
		return dotGit, nil
	}

	// .git is a file, e.g. `gitdir: /path/to/real/gitdir`.
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	content := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(content, prefix) {
		return "", nil
	}
	target := strings.TrimSpace(strings.TrimPrefix(content, prefix))
	if !filepath.IsAbs(target) {
		target = filepath.Join(repoPath, target)
	}
	return target, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
