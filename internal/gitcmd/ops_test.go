package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireGit skips the test if the system git binary isn't available (it is
// expected to be, since gitloop shells out to it in production too).
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found in PATH")
	}
}

func initRepo(t *testing.T, dir string) *Runner {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "gitloop test")
	return New(dir)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAddAllAndCommit(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	r := initRepo(t, dir)

	writeFile(t, dir, "a.md", "hello\n")
	if err := r.AddAll(); err != nil {
		t.Fatalf("AddAll: %v", err)
	}
	if err := r.Commit("test commit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entries, err := r.StatusPorcelain()
	if err != nil {
		t.Fatalf("StatusPorcelain: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("StatusPorcelain after commit = %#v, want empty", entries)
	}
}

func TestFetchAndRevListLeftRightCount(t *testing.T) {
	requireGit(t)

	remoteDir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--bare", "-b", "main", remoteDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	localDir := t.TempDir()
	local := initRepo(t, localDir)
	writeFile(t, localDir, "a.md", "hello\n")
	if err := local.AddAll(); err != nil {
		t.Fatal(err)
	}
	if err := local.Commit("initial"); err != nil {
		t.Fatal(err)
	}
	runIn(t, localDir, "remote", "add", "origin", remoteDir)
	runIn(t, localDir, "push", "-q", "origin", "main")

	// A second clone pushes a commit that local doesn't have.
	otherDir := t.TempDir()
	runIn(t, "", "clone", "-q", remoteDir, otherDir)
	runIn(t, otherDir, "config", "user.email", "test@example.com")
	runIn(t, otherDir, "config", "user.name", "gitloop test")
	writeFile(t, otherDir, "b.md", "from other\n")
	runIn(t, otherDir, "add", "-A")
	runIn(t, otherDir, "commit", "-q", "-m", "other change")
	runIn(t, otherDir, "push", "-q", "origin", "main")

	if err := local.Fetch("origin"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	ahead, behind, err := local.RevListLeftRightCount("main", "origin/main")
	if err != nil {
		t.Fatalf("RevListLeftRightCount: %v", err)
	}
	if ahead != 0 || behind != 1 {
		t.Fatalf("RevListLeftRightCount = (%d, %d), want (0, 1)", ahead, behind)
	}

	if err := local.MergeFF("origin/main"); err != nil {
		t.Fatalf("MergeFF: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "b.md")); err != nil {
		t.Errorf("expected b.md to exist after fast-forward merge: %v", err)
	}
}

func TestMergeReportsConflict(t *testing.T) {
	requireGit(t)

	dir := t.TempDir()
	r := initRepo(t, dir)
	writeFile(t, dir, "a.md", "base\n")
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit("base"); err != nil {
		t.Fatal(err)
	}
	runIn(t, dir, "branch", "upstream")

	// Local branch diverges.
	writeFile(t, dir, "a.md", "local change\n")
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit("local change"); err != nil {
		t.Fatal(err)
	}

	// upstream branch diverges too, in a conflicting way.
	runIn(t, dir, "checkout", "-q", "upstream")
	writeFile(t, dir, "a.md", "upstream change\n")
	runIn(t, dir, "add", "-A")
	runIn(t, dir, "commit", "-q", "-m", "upstream change")
	runIn(t, dir, "checkout", "-q", "main")

	conflict, err := r.Merge("upstream")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !conflict {
		t.Fatalf("Merge() conflict = false, want true")
	}

	files, err := r.ConflictedFiles()
	if err != nil {
		t.Fatalf("ConflictedFiles: %v", err)
	}
	if len(files) != 1 || files[0] != "a.md" {
		t.Fatalf("ConflictedFiles = %v, want [a.md]", files)
	}

	// During a merge (unlike a rebase) "ours" (stage 2) is the local branch
	// and "theirs" (stage 3) is the branch being merged in.
	if content, ok, err := r.ShowStage(2, "a.md"); err != nil || !ok || content != "local change\n" {
		t.Errorf("ShowStage(2, a.md) = %q, ok=%v err=%v, want \"local change\\n\", ok", content, ok, err)
	}
	if content, ok, err := r.ShowStage(3, "a.md"); err != nil || !ok || content != "upstream change\n" {
		t.Errorf("ShowStage(3, a.md) = %q, ok=%v err=%v, want \"upstream change\\n\", ok", content, ok, err)
	}

	if err := r.CheckoutTheirs("a.md"); err != nil {
		t.Fatalf("CheckoutTheirs: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "a.md")); err != nil || string(got) != "upstream change\n" {
		t.Errorf("a.md after CheckoutTheirs = %q, err=%v, want \"upstream change\\n\"", got, err)
	}

	if err := r.MergeAbort(); err != nil {
		t.Fatalf("MergeAbort: %v", err)
	}
	entries, err := r.StatusPorcelain()
	if err != nil {
		t.Fatalf("StatusPorcelain: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("StatusPorcelain after MergeAbort = %#v, want clean", entries)
	}
}

func runIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
}
