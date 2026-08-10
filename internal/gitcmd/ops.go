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

// RemoteNames returns the names of all remotes configured for the repository.
// `git remote` exits successfully and prints nothing when the repository has
// no remotes.
func (r *Runner) RemoteNames() ([]string, error) {
	res, err := r.run("remote")
	if err != nil {
		return nil, err
	}
	return strings.Fields(res.Stdout), nil
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

// Merge merges upstream into the current branch
// (`git merge --no-ff --no-edit <upstream>`), creating a merge commit with
// git's default message. Callers only use Merge once local and upstream
// have diverged, where a fast-forward is never possible anyway; --no-ff
// makes that explicit so git doesn't refuse the merge asking which mode was
// intended. --no-edit avoids spawning an editor for the default message,
// which would otherwise hang a non-interactive daemon. If the merge stops
// on a conflict, Merge returns (true, nil) rather than an error: a paused
// merge is an expected outcome the caller must handle, not a failure of the
// git invocation.
func (r *Runner) Merge(upstream string) (conflict bool, err error) {
	_, runErr := r.run("merge", "--no-ff", "--no-edit", upstream)
	if runErr == nil {
		return false, nil
	}
	if r.hasMergeInProgress() {
		return true, nil
	}
	return false, runErr
}

// MergeAbort cancels an in-progress merge and restores the working tree to
// its pre-merge state (`git merge --abort`).
func (r *Runner) MergeAbort() error {
	_, err := r.run("merge", "--abort")
	return err
}

// CheckoutTheirs replaces path's working-tree and index content with the
// incoming side of an in-progress merge conflict
// (`git checkout --theirs -- <path>`).
func (r *Runner) CheckoutTheirs(path string) error {
	_, err := r.run("checkout", "--theirs", "--", path)
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
//
// The returned content is the smudged (working-tree-equivalent) form of the
// blob: `git cat-file --filters` runs the path's configured clean/smudge
// filters before returning bytes. Without this, backing up an "ours" or
// "theirs" side of a filtered file (e.g. git-crypt, which stores encrypted
// blobs and decrypts on checkout) would preserve the raw stored blob rather
// than the plaintext the user wrote — and re-adding that backup would run
// the clean filter over already-encrypted content, producing an unrecoverable
// double-encrypted mess. For paths with no filter configured, `--filters`
// returns the raw blob unchanged, matching the previous `git show :N:<path>`
// behavior.
func (r *Runner) ShowStage(stage int, path string) (content string, ok bool, err error) {
	res, runErr := r.run("cat-file", "--filters", fmt.Sprintf(":%d:%s", stage, path))
	if runErr != nil {
		return "", false, nil
	}
	return res.Stdout, true, nil
}

// hasMergeInProgress reports whether .git/MERGE_HEAD exists, i.e. a merge is
// currently paused on a conflict.
func (r *Runner) hasMergeInProgress() bool {
	_, err := os.Stat(filepath.Join(r.Dir, ".git", "MERGE_HEAD"))
	return err == nil
}
