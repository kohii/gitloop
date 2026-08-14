package gitcmd

import (
	"context"
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
func (r *Runner) Fetch(ctx context.Context, remote string) error {
	_, err := r.runRemote(ctx, "fetch", remote)
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
// (`git merge --ff-only <upstream>`). It fails if the branch isn't behind
// upstream, and also if checking out the incoming tree would overwrite a
// modified or untracked file — the refusal that lets callers attempt a
// fast-forward over a dirty working tree without risking it, because git
// checks every path before it writes any of them.
//
// Overriding merge.autoStash is what keeps that refusal intact. A user's
// `merge.autostash = true` would otherwise turn it into a stash, a
// fast-forward, and a failed unstash that exits 0 while leaving conflict
// markers in the working tree and a stash entry behind — a silent success as
// far as a daemon can tell. The override is spelled as `-c` rather than
// `--no-autostash` because the flag only exists in git 2.27 and later, where
// the config key it suppresses does too: older gits ignore the unknown key
// and have no autostash to suppress, so one form works everywhere.
func (r *Runner) MergeFF(upstream string) error {
	_, err := r.run("-c", "merge.autoStash=false", "merge", "--ff-only", upstream)
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

// Rebase replays the current branch's local-only commits on top of upstream
// (`git rebase <upstream>`). Callers use it where a merge commit would be
// unwelcome; the commits it rewrites are by definition unpushed, so nothing
// outside this checkout can be built on the versions it replaces. Commits
// already applied upstream are dropped by git's own patch-id check, which is
// what makes the same edit committed on two machines converge instead of
// conflicting.
//
// If the rebase stops on a conflict, Rebase returns (true, nil) rather than an
// error, mirroring Merge: a paused rebase is an outcome the caller must decide
// about, not a failed invocation. Every other non-zero exit — a dirty working
// tree being the one to expect — is returned as an error with the rebase not
// started.
//
// rebase.autoStash is overridden for the same reason MergeFF suppresses
// merge.autoStash: under a user's `rebase.autostash = true`, git stashes the
// uncommitted work, rebases, and can fail to reapply it, leaving conflict
// markers in the working tree and the user's work in a stash entry. Refusing
// outright gives the caller a dirty tree to report instead.
func (r *Runner) Rebase(upstream string) (conflict bool, err error) {
	_, runErr := r.run("-c", "rebase.autoStash=false", "rebase", upstream)
	if runErr == nil {
		return false, nil
	}
	if r.hasRebaseInProgress() {
		return true, nil
	}
	return false, runErr
}

// RebaseAbort cancels an in-progress rebase and restores the branch and
// working tree to their pre-rebase state (`git rebase --abort`).
func (r *Runner) RebaseAbort() error {
	_, err := r.run("rebase", "--abort")
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
func (r *Runner) Push(ctx context.Context, remote, branch string) error {
	_, err := r.runRemote(ctx, "push", remote, branch)
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

// hasMergeInProgress reports whether a merge is currently paused on a
// conflict.
func (r *Runner) hasMergeInProgress() bool {
	return r.gitPathExists("MERGE_HEAD")
}

// hasRebaseInProgress reports whether a rebase is currently paused. Both
// backends are checked: the merge backend writes rebase-merge, and the older
// apply backend (`git rebase --apply`, or a git predating 2.26) writes
// rebase-apply.
func (r *Runner) hasRebaseInProgress() bool {
	return r.gitPathExists("rebase-merge") || r.gitPathExists("rebase-apply")
}

// gitPathExists reports whether name exists inside the repository's git
// directory. The location is resolved by git rather than assumed to be
// `<dir>/.git/<name>`, so a linked worktree — whose .git is a file pointing
// at a per-worktree directory elsewhere — is answered about correctly.
func (r *Runner) gitPathExists(name string) bool {
	res, err := r.run("rev-parse", "--git-path", name)
	if err != nil {
		return false
	}
	path := strings.TrimSpace(res.Stdout)
	if path == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		// --git-path answers relative to the repository the command ran in.
		path = filepath.Join(r.Dir, path)
	}
	_, statErr := os.Stat(path)
	return statErr == nil
}
