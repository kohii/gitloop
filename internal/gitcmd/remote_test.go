package gitcmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFetchTimeoutKillsTransportAndAllowsNextFetch(t *testing.T) {
	requireGit(t)

	localDir := t.TempDir()
	runner := initRepo(t, localDir)
	runner.terminationGrace = 100 * time.Millisecond
	pidDir := t.TempDir()
	transport := writeHangingTransport(t, pidDir)
	t.Setenv("GIT_SSH_COMMAND", transport)

	runIn(t, localDir, "remote", "add", "origin", "ssh://example.invalid/repository")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := runner.Fetch(ctx, "origin")
	if err == nil {
		t.Fatal("Fetch = nil, want timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(Fetch, context.DeadlineExceeded) = false: %v", err)
	}
	assertRecordedProcessesGone(t, pidDir)

	remoteDir := t.TempDir()
	runIn(t, "", "init", "-q", "--bare", "-b", "main", remoteDir)
	runIn(t, localDir, "remote", "set-url", "origin", remoteDir)
	nextCtx, nextCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer nextCancel()
	if err := runner.Fetch(nextCtx, "origin"); err != nil {
		t.Fatalf("Fetch after timeout = %v, want success without a stale lock or process", err)
	}
}

func TestPushCancellationKillsCommandProcessGroup(t *testing.T) {
	dir := t.TempDir()
	runner := New(dir)
	runner.executable = writeHangingTransport(t, dir)
	runner.terminationGrace = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	err := runner.Push(ctx, "origin", "main")
	if err == nil {
		t.Fatal("Push = nil, want cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(Push, context.Canceled) = false: %v", err)
	}
	if !strings.Contains(err.Error(), "transport stalled") {
		t.Errorf("Push error = %q, want captured command stderr", err)
	}
	assertRecordedProcessesGone(t, dir)
}

func writeHangingTransport(t *testing.T, pidDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hanging-transport")
	script := `#!/bin/sh
if [ "$1" = "--probe" ]; then
  exit 0
fi
echo $$ > "` + filepath.Join(pidDir, "parent.pid") + `"
echo "transport stalled" >&2
/bin/sh -c 'trap "" TERM; while :; do sleep 1; done' &
child=$!
echo "$child" > "` + filepath.Join(pidDir, "child.pid") + `"
trap '' TERM
wait "$child"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write hanging transport: %v", err)
	}
	probe := exec.Command(path, "--probe")
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("probe hanging transport: %v: %s", err, output)
	}
	return path
}

func assertRecordedProcessesGone(t *testing.T, pidDir string) {
	t.Helper()
	for _, name := range []string{"parent.pid", "child.pid"} {
		pid := readRecordedPID(t, filepath.Join(pidDir, name))
		deadline := time.Now().Add(2 * time.Second)
		for processExists(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if processExists(pid) {
			t.Errorf("process %d from %s still exists after command returned", pid, name)
		}
	}
}

func readRecordedPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil {
				t.Fatalf("parse PID in %s: %v", path, parseErr)
			}
			return pid
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read PID from %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID file %s was not created", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
