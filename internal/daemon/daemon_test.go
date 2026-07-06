package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kohii/gitloop/internal/config"
)

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
		WithStatusPath(filepath.Join(dir, "status.json")),
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
