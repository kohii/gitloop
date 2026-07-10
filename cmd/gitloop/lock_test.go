package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gitloopHelperEnv is the env var that flips this test binary into
// "act as the gitloop CLI" mode via TestMain re-exec, so the lock-hold
// subprocess tests can spawn a real gitloop process without a separately
// built binary. The value is the CLI args, null-separated.
const gitloopHelperEnv = "GITLOOP_TEST_HELPER_ARGS"

func TestMain(m *testing.M) {
	if raw := os.Getenv(gitloopHelperEnv); raw != "" {
		os.Exit(run(strings.Split(raw, "\x1f"), os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// spawnLockHold starts this test binary as `gitloop lock hold <path>` and
// returns the running command plus pipes to its stdin/stdout. Callers close
// stdin (or kill the process) to release the lock; both are exercised by
// separate tests below.
func spawnLockHold(t *testing.T, path string) (cmd *exec.Cmd, stdin io.WriteCloser, stdout *bufio.Reader) {
	t.Helper()
	cmd = exec.Command(os.Args[0])
	cmd.Args = []string{os.Args[0]}
	cmd.Env = append(os.Environ(), gitloopHelperEnv+"=lock\x1fhold\x1f"+path)

	sin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	sout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	// Send subprocess stderr to the test's stderr so a failure explains
	// itself instead of dying with a mysterious exit code.
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	return cmd, sin, bufio.NewReader(sout)
}

// readLine reads a single '\n'-terminated line with a timeout so a stuck
// subprocess fails the test loudly instead of hanging until `go test`
// times out.
func readLine(t *testing.T, r *bufio.Reader, timeout time.Duration) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("reading stdout: %v", res.err)
		}
		return strings.TrimRight(res.line, "\n")
	case <-time.After(timeout):
		t.Fatal("timed out reading stdout")
		return ""
	}
}

// waitCmd waits for a subprocess with a timeout, returning its exit code
// (-1 on non-ExitError failures).
func waitCmd(t *testing.T, cmd *exec.Cmd, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0
		}
		var exitErr *exec.ExitError
		if ok := errorsAs(err, &exitErr); ok {
			return exitErr.ExitCode()
		}
		t.Fatalf("cmd.Wait: %v", err)
		return -1
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatal("subprocess did not exit within timeout")
		return -1
	}
}

// errorsAs is a tiny stand-in for errors.As so this test file doesn't need
// yet another import for a one-liner.
func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	*target = e
	return true
}

func TestLockHoldRejectsRelativePath(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{os.Args[0]}
	cmd.Env = append(os.Environ(), gitloopHelperEnv+"=lock\x1fhold\x1frelative/save.lock")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit on relative path, got success (output: %s)", out)
	}
	var exitErr *exec.ExitError
	if ok := errorsAs(err, &exitErr); !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("unexpected error type/code: %v (output: %s)", err, out)
	}
}

func TestLockHoldAcquiresAndBlocksSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "save.lock")

	first, stdin1, stdout1 := spawnLockHold(t, path)
	// Belt-and-braces cleanup so a failure between here and the explicit
	// close/wait below doesn't leak a subprocess.
	t.Cleanup(func() {
		_ = stdin1.Close()
		_ = first.Wait()
	})

	if got := readLine(t, stdout1, 3*time.Second); got != "acquired" {
		t.Fatalf("first process stdout = %q, want %q", got, "acquired")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file missing at %s: %v", path, err)
	}

	// A second subprocess trying the same path must fail while the first
	// still holds it.
	second, stdin2, _ := spawnLockHold(t, path)
	_ = stdin2.Close()
	if code := waitCmd(t, second, 3*time.Second); code == 0 {
		t.Fatalf("second holder exit code = 0, want non-zero while first still holds")
	}

	// Releasing via stdin close lets a fresh subprocess acquire the same
	// path.
	if err := stdin1.Close(); err != nil {
		t.Fatalf("close first stdin: %v", err)
	}
	if code := waitCmd(t, first, 3*time.Second); code != 0 {
		t.Fatalf("first holder exit code = %d, want 0", code)
	}

	third, stdin3, stdout3 := spawnLockHold(t, path)
	if got := readLine(t, stdout3, 3*time.Second); got != "acquired" {
		t.Fatalf("third process stdout = %q, want %q after first released", got, "acquired")
	}
	_ = stdin3.Close()
	if code := waitCmd(t, third, 3*time.Second); code != 0 {
		t.Errorf("third holder exit code = %d, want 0", code)
	}
}

func TestLockHoldReleasesWhenProcessKilled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "save.lock")

	first, _, stdout1 := spawnLockHold(t, path)
	if got := readLine(t, stdout1, 3*time.Second); got != "acquired" {
		t.Fatalf("first process stdout = %q, want %q", got, "acquired")
	}

	// SIGKILL leaves the process no chance to run defers — the flock must
	// still be released by the OS when the fd closes.
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("Process.Kill: %v", err)
	}
	if _, err := first.Process.Wait(); err != nil {
		// Wait after Kill can surface the killed status as an error;
		// tolerate any non-nil result since we only care that the process is
		// reaped.
		t.Logf("Process.Wait after Kill: %v", err)
	}

	// An in-process flock now must succeed.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	// Retry briefly: the killed process's fd release is observed by the OS
	// synchronously, but process reaping is best-effort.
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("flock never became available after killing holder: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
