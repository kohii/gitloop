package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kohii/gitloop/internal/config"
)

// shortStateDir returns a temp directory whose path is short enough to hold a
// unix socket. t.TempDir() embeds the test's name, which routinely pushes a
// socket path past the ~104-byte limit the kernel imposes on one.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gitloop")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestDaemonRunRecoversFromPanicAndRetries verifies the error-isolation
// contract: a repository whose loop panics must be retried with backoff
// rather than taking down Run or the other repositories.
func TestDaemonRunRecoversFromPanicAndRetries(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Repositories: []config.Repository{
		{
			Path:          dir,
			Settle:        10 * time.Millisecond,
			MaxWait:       time.Second,
			FetchInterval: time.Hour,
			Remote:        "origin",
			Branch:        "main",
			OnConflict:    config.OnConflictBackup,
		},
	}}

	var calls int32
	factory := func(path string) GitClient {
		if atomic.AddInt32(&calls, 1) == 1 {
			panic("boom: simulated failure on first attempt")
		}
		return &fakeGit{}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(cfg,
		WithLogger(logger),
		WithStatusPath(filepath.Join(shortStateDir(t), "status.json")),
		withGitFactory(factory),
		withBackoff(20*time.Millisecond, 20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&calls) >= 2 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not shut down within 2s of context cancellation")
	}
}

// TestDaemonRunWritesPidAndHeartbeatsIndependentlyOfRepoLoops verifies the
// crash-detection contract: the status file must carry the daemon's own PID
// and a periodically refreshed heartbeat, from a goroutine that isn't
// blocked by (or tied to) any single repository's watch/sync loop.
func TestDaemonRunWritesPidAndHeartbeatsIndependentlyOfRepoLoops(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Repositories: []config.Repository{
		{
			Path:          dir,
			Settle:        time.Hour,
			MaxWait:       time.Hour,
			FetchInterval: time.Hour,
			Remote:        "origin",
			Branch:        "main",
			OnConflict:    config.OnConflictBackup,
		},
	}}

	statusPath := filepath.Join(shortStateDir(t), "status.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(cfg,
		WithLogger(logger),
		WithStatusPath(statusPath),
		withGitFactory(func(string) GitClient { return &fakeGit{} }),
		withHeartbeatInterval(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool {
		sf, err := LoadStatusFile(statusPath)
		return err == nil && sf.Pid == os.Getpid() && !sf.LastHeartbeatAt.IsZero()
	})

	first, err := LoadStatusFile(statusPath)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		sf, err := LoadStatusFile(statusPath)
		return err == nil && sf.LastHeartbeatAt.After(first.LastHeartbeatAt)
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not shut down within 2s of context cancellation")
	}
}
