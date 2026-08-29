package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kohii/gitloop/internal/daemon"
)

// shortSocketDir returns a temp directory whose path is short enough to hold
// a unix socket. t.TempDir() embeds the test's name, which routinely pushes a
// socket path past the ~104-byte limit the kernel imposes on one.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gitloop")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func withSocketPath(t *testing.T, path string) {
	t.Helper()
	orig := controlSocketPathFunc
	controlSocketPathFunc = func() (string, error) { return path, nil }
	t.Cleanup(func() { controlSocketPathFunc = orig })
}

func TestSyncCmdReportsNoRunningDaemon(t *testing.T) {
	withSocketPath(t, filepath.Join(shortSocketDir(t), "control.sock"))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"sync"}, &stdout, &stderr); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no gitloop daemon is running") {
		t.Errorf("stderr = %q, want it to say no daemon is running", stderr.String())
	}
}

// TestSyncCmdResolvesArgumentsToAbsolutePaths pins how a repository is named.
// The daemon matches on the absolute paths its config resolved to, so a
// relative argument typed from inside a repository has to be resolved here
// rather than sent through as-is.
func TestSyncCmdResolvesArgumentsToAbsolutePaths(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "control.sock")
	withSocketPath(t, socketPath)

	received := make(chan daemon.ControlRequest, 1)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req daemon.ControlRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		received <- req
		_ = json.NewEncoder(conn).Encode(daemon.ControlResponse{Triggered: req.Paths})
	}()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"sync", "./notes"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d (stderr: %s), want 0", code, stderr.String())
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, "notes")

	select {
	case req := <-received:
		if req.Command != daemon.CommandSync {
			t.Errorf("command = %q, want %q", req.Command, daemon.CommandSync)
		}
		if len(req.Paths) != 1 || req.Paths[0] != want {
			t.Errorf("paths = %v, want [%s]", req.Paths, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the daemon side never received a request")
	}

	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout = %q, want it to name %s", stdout.String(), want)
	}
}
