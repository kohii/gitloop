package statemachine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPreCheckSafeWhenNoGitDir(t *testing.T) {
	dir := t.TempDir()
	got, err := PreCheck(dir)
	if err != nil {
		t.Fatalf("PreCheck: %v", err)
	}
	if !got.Safe {
		t.Errorf("PreCheck(%s) = %+v, want Safe", dir, got)
	}
}

func TestPreCheckSafeWhenClean(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, ".git"))

	got, err := PreCheck(dir)
	if err != nil {
		t.Fatalf("PreCheck: %v", err)
	}
	if !got.Safe {
		t.Errorf("PreCheck(%s) = %+v, want Safe", dir, got)
	}
}

func TestPreCheckDetectsRebaseInProgress(t *testing.T) {
	for _, marker := range []string{"rebase-merge", "rebase-apply"} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			mustMkdir(t, filepath.Join(dir, ".git", marker))

			got, err := PreCheck(dir)
			if err != nil {
				t.Fatalf("PreCheck: %v", err)
			}
			if got.Safe || got.Reason != "rebase-in-progress" {
				t.Errorf("PreCheck(%s) = %+v, want unsafe rebase-in-progress", dir, got)
			}
		})
	}
}

func TestPreCheckDetectsMergeInProgress(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, ".git"))
	mustWriteFile(t, filepath.Join(dir, ".git", "MERGE_HEAD"), "deadbeef\n")

	got, err := PreCheck(dir)
	if err != nil {
		t.Fatalf("PreCheck: %v", err)
	}
	if got.Safe || got.Reason != "merge-in-progress" {
		t.Errorf("PreCheck(%s) = %+v, want unsafe merge-in-progress", dir, got)
	}
}

func TestPreCheckFollowsWorktreeGitFile(t *testing.T) {
	dir := t.TempDir()
	realGitDir := filepath.Join(t.TempDir(), "real-gitdir")
	mustMkdir(t, filepath.Join(realGitDir, "rebase-merge"))
	mustWriteFile(t, filepath.Join(dir, ".git"), "gitdir: "+realGitDir+"\n")

	got, err := PreCheck(dir)
	if err != nil {
		t.Fatalf("PreCheck: %v", err)
	}
	if got.Safe || got.Reason != "rebase-in-progress" {
		t.Errorf("PreCheck(%s) = %+v, want unsafe rebase-in-progress", dir, got)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
