package gitcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// staleTimestampRepo returns a repository with one committed file whose
// timestamp no longer matches what the index cached, without its content
// having changed. That is the state in which `git status` wants to rewrite
// .git/index, so it is the state these tests need to observe the difference
// between a status that takes optional locks and one that doesn't.
func staleTimestampRepo(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	r := initRepo(t, dir)

	writeFile(t, dir, "a.md", "hello\n")
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit("init"); err != nil {
		t.Fatal(err)
	}

	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.md"), stale, stale); err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func readIndex(t *testing.T, dir string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestStatusPorcelainDoesNotRewriteTheIndex(t *testing.T) {
	requireGit(t)
	r, dir := staleTimestampRepo(t)

	before := readIndex(t, dir)
	entries, err := r.StatusPorcelain()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("StatusPorcelain() = %v, want no entries for an unmodified file", entries)
	}
	if after := readIndex(t, dir); !bytes.Equal(before, after) {
		t.Error("StatusPorcelain rewrote .git/index; a background daemon must not contend for the index lock just to answer a question")
	}
}

// TestStatusWithOptionalLocksRewritesTheIndex pins the behavior the test
// above is guarding against. Without it, a future git that stopped
// refreshing the index on status would make that test pass for the wrong
// reason.
func TestStatusWithOptionalLocksRewritesTheIndex(t *testing.T) {
	requireGit(t)
	r, dir := staleTimestampRepo(t)

	before := readIndex(t, dir)
	if _, err := r.run("status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if after := readIndex(t, dir); bytes.Equal(before, after) {
		t.Skip("this git does not refresh the index on status, so there is no optional write to suppress")
	}
}

func TestStatusPorcelainStillReportsRealChanges(t *testing.T) {
	requireGit(t)
	r, dir := staleTimestampRepo(t)

	writeFile(t, dir, "a.md", "hello\nworld\n")
	writeFile(t, dir, "b.md", "new\n")

	entries, err := r.StatusPorcelain()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Path] = string([]byte{e.X, e.Y})
	}
	want := map[string]string{"a.md": " M", "b.md": "??"}
	if len(got) != len(want) {
		t.Fatalf("StatusPorcelain() = %v, want %v", got, want)
	}
	for path, code := range want {
		if got[path] != code {
			t.Errorf("status of %s = %q, want %q", path, got[path], code)
		}
	}
}
