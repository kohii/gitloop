package gitcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AddAll stages all working-tree changes, respecting .gitignore
// (`git add -A`).
func (r *Runner) AddAll() error {
	_, err := r.run("add", "-A")
	return err
}

// AddPath stages a single path (`git add -- <path>`).
func (r *Runner) AddPath(path string) error {
	_, err := r.run("add", "--", path)
	return err
}

// Commit records staged changes with the given message.
func (r *Runner) Commit(message string) error {
	_, err := r.run("commit", "-m", message)
	return err
}

// Fetch runs `git fetch <remote>`.
func (r *Runner) Fetch(remote string) error {
	_, err := r.run("fetch", remote)
	return err
}

// CurrentBranch returns the checked-out branch name.
func (r *Runner) CurrentBranch() (string, error) {
	res, err := r.run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// RevListLeftRightCount runs
// `git rev-list --left-right --count <local>...<upstream>` and returns how
// many commits are only on local (ahead) and only on upstream (behind).
func (r *Runner) RevListLeftRightCount(local, upstream string) (ahead, behind int, err error) {
	res, err := r.run("rev-list", "--left-right", "--count", local+"..."+upstream)
	if err != nil {
		return 0, 0, err
	}
	return parseLeftRightCount(res.Stdout)
}

func parseLeftRightCount(output string) (ahead, behind int, err error) {
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("gitcmd: unexpected rev-list --left-right --count output %q", output)
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("gitcmd: parsing ahead count from %q: %w", output, err)
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("gitcmd: parsing behind count from %q: %w", output, err)
	}
	return ahead, behind, nil
}

// MergeFF fast-forwards the current branch to upstream
// (`git merge --ff-only <upstream>`). It fails if a fast-forward isn't
// possible, which should not happen for callers that only invoke it in the
// Behind state.
func (r *Runner) MergeFF(upstream string) error {
	_, err := r.run("merge", "--ff-only", upstream)
	return err
}

// Rebase replays the current branch's commits onto upstream
// (`git rebase <upstream>`). If the rebase stops on a conflict, Rebase
// returns (true, nil) rather than an error: a paused rebase is an expected
// outcome the caller must handle, not a failure of the git invocation.
func (r *Runner) Rebase(upstream string) (conflict bool, err error) {
	_, runErr := r.run("rebase", upstream)
	if runErr == nil {
		return false, nil
	}
	if r.hasRebaseInProgress() {
		return true, nil
	}
	return false, runErr
}

// RebaseContinue resumes a paused rebase after conflicts have been resolved
// and staged (`git rebase --continue`). Like Rebase, it returns
// (true, nil) if the rebase stops again on a later conflicting commit.
func (r *Runner) RebaseContinue() (conflict bool, err error) {
	_, runErr := r.run("rebase", "--continue")
	if runErr == nil {
		return false, nil
	}
	if r.hasRebaseInProgress() {
		return true, nil
	}
	return false, runErr
}

// RebaseAbort cancels an in-progress rebase and restores the branch to its
// pre-rebase state (`git rebase --abort`).
func (r *Runner) RebaseAbort() error {
	_, err := r.run("rebase", "--abort")
	return err
}

// ResetHard resets the current branch to rev, discarding local commits and
// working-tree changes (`git reset --hard <rev>`). It is used to recover
// from an unresolvable conflict: once both sides of the conflict are backed
// up as plain files, the local branch is reset to upstream so the
// repository stops re-diverging (and re-conflicting) on every cycle.
func (r *Runner) ResetHard(rev string) error {
	_, err := r.run("reset", "--hard", rev)
	return err
}

// Push runs `git push <remote> <branch>`.
func (r *Runner) Push(remote, branch string) error {
	_, err := r.run("push", remote, branch)
	return err
}

// ConflictedFiles lists paths with unresolved merge/rebase conflicts.
func (r *Runner) ConflictedFiles() ([]string, error) {
	entries, err := r.StatusPorcelain()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsConflicted() {
			files = append(files, e.Path)
		}
	}
	return files, nil
}

// ShowStage returns the content of path at the given index stage (1 =
// common ancestor, 2 = ours, 3 = theirs) during a conflict. ok is false, with
// no error, if that stage does not exist for path (e.g. one side deleted the
// file) — that is an expected shape of some conflicts, not a failure.
func (r *Runner) ShowStage(stage int, path string) (content string, ok bool, err error) {
	res, runErr := r.run("show", fmt.Sprintf(":%d:%s", stage, path))
	if runErr != nil {
		return "", false, nil
	}
	return res.Stdout, true, nil
}

// hasRebaseInProgress reports whether .git/rebase-merge or .git/rebase-apply
// exists, i.e. a rebase is currently paused (on a conflict or otherwise).
func (r *Runner) hasRebaseInProgress() bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(r.Dir, ".git", name)); err == nil {
			return true
		}
	}
	return false
}
