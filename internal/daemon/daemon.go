package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kohii/gitloop/internal/config"
)

const (
	// initialBackoff is how long a repository's watch loop waits before its
	// first retry after a failure.
	initialBackoff = 30 * time.Second
	// maxBackoff caps the exponential backoff between retries.
	maxBackoff = 10 * time.Minute
	// heartbeatInterval is how often the daemon stamps the status file's
	// LastHeartbeatAt, independent of any repository's own sync cycle.
	heartbeatInterval = 5 * time.Second
)

// Daemon watches every configured repository and keeps it synced with its
// remote. Each repository runs in its own goroutine so that one
// repository's failure (panic or repeated error) never affects the others.
type Daemon struct {
	cfg               *config.Config
	logger            *slog.Logger
	statusPath        string
	hostname          string
	gitFor            gitFactory
	initialBackoff    time.Duration
	maxBackoff        time.Duration
	heartbeatInterval time.Duration
}

// Option configures a Daemon constructed with New.
type Option func(*Daemon)

// WithLogger overrides the base logger. Per-repository loops attach a
// "repo" attribute to it.
func WithLogger(logger *slog.Logger) Option {
	return func(d *Daemon) { d.logger = logger }
}

// WithStatusPath overrides where the daemon persists repository status for
// `gitloop status` to read. It defaults to DefaultStatusPath().
func WithStatusPath(path string) Option {
	return func(d *Daemon) { d.statusPath = path }
}

// withGitFactory overrides how a GitClient is constructed for a repository
// directory. It exists for tests; production code always uses
// defaultGitFactory (gitcmd.New).
func withGitFactory(f gitFactory) Option {
	return func(d *Daemon) { d.gitFor = f }
}

// withBackoff overrides the retry backoff schedule. It exists for tests;
// production code uses initialBackoff/maxBackoff.
func withBackoff(initial, max time.Duration) Option {
	return func(d *Daemon) { d.initialBackoff = initial; d.maxBackoff = max }
}

// withHeartbeatInterval overrides how often the status file's
// LastHeartbeatAt is stamped. It exists for tests; production code uses
// heartbeatInterval.
func withHeartbeatInterval(interval time.Duration) Option {
	return func(d *Daemon) { d.heartbeatInterval = interval }
}

// New builds a Daemon for cfg. It does not start watching until Run is
// called.
func New(cfg *config.Config, opts ...Option) (*Daemon, error) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}

	d := &Daemon{
		cfg:               cfg,
		logger:            slog.Default(),
		hostname:          host,
		gitFor:            defaultGitFactory,
		initialBackoff:    initialBackoff,
		maxBackoff:        maxBackoff,
		heartbeatInterval: heartbeatInterval,
	}
	for _, opt := range opts {
		opt(d)
	}

	if d.statusPath == "" {
		p, err := DefaultStatusPath()
		if err != nil {
			return nil, fmt.Errorf("daemon: resolving default status path: %w", err)
		}
		d.statusPath = p
	}

	return d, nil
}

// stateDir is where the daemon keeps everything a second process needs to
// find: the status file, the process lock, and the control socket. All three
// are derived from the status path so that a test pointing that at a temp
// directory moves the whole set.
func (d *Daemon) stateDir() string {
	return filepath.Dir(d.statusPath)
}

// Run starts one watch loop per configured repository, plus the heartbeat
// goroutine and the control socket, and blocks until ctx is canceled and all
// of them have shut down gracefully.
//
// It returns ErrDaemonAlreadyRunning if another daemon holds the process
// lock.
func (d *Daemon) Run(ctx context.Context) error {
	lock, err := acquireProcessLock(filepath.Join(d.stateDir(), processLockName))
	if err != nil {
		return err
	}
	defer lock.release()

	recorder, err := newStatusRecorder(d.statusPath)
	if err != nil {
		return fmt.Errorf("daemon: loading status file %s: %w", d.statusPath, err)
	}
	if err := recorder.setPid(os.Getpid()); err != nil {
		d.logger.Error("writing status file failed", "error", err)
	}

	// Built before any loop starts, and outliving all of them: a request that
	// arrives while a repository's loop is restarting has somewhere to wait.
	triggers := make(map[string]*repoTrigger, len(d.cfg.Repositories))
	for _, repo := range d.cfg.Repositories {
		triggers[repo.Path] = newRepoTrigger()
	}

	control, err := listenControl(filepath.Join(d.stateDir(), controlSocketName), triggers, d.logger)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		control.serve(ctx.Done())
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.runHeartbeat(ctx, recorder)
	}()

	for _, repo := range d.cfg.Repositories {
		wg.Add(1)
		go func(repo config.Repository) {
			defer wg.Done()
			d.superviseRepo(ctx, repo, triggers[repo.Path], recorder)
		}(repo)
	}
	wg.Wait()
	return nil
}

// runHeartbeat stamps the status file's LastHeartbeatAt every
// d.heartbeatInterval until ctx is canceled. It runs independently of every
// repository's watch/sync loop so a consumer of the status file can tell
// the daemon process itself is alive even while every repository loop is
// idle, backed off, or blocked on a slow git command.
func (d *Daemon) runHeartbeat(ctx context.Context, recorder *statusRecorder) {
	if err := recorder.heartbeat(); err != nil {
		d.logger.Error("writing status file failed", "error", err)
	}

	ticker := time.NewTicker(d.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := recorder.heartbeat(); err != nil {
				d.logger.Error("writing status file failed", "error", err)
			}
		}
	}
}

// superviseRepo runs repo's watch loop, restarting it with exponential
// backoff on failure (including recovered panics) until ctx is canceled.
func (d *Daemon) superviseRepo(ctx context.Context, repo config.Repository, trigger *repoTrigger, recorder *statusRecorder) {
	logger := d.logger.With("repo", filepath.Base(repo.Path))
	backoff := d.initialBackoff

	for ctx.Err() == nil {
		err := d.runRepoGuarded(ctx, repo, trigger, logger, recorder)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// A nil, non-canceled return means the loop chose to stop on
			// its own; nothing to retry.
			return
		}

		logger.Error("repository watch loop failed, will retry", "error", err, "backoff", backoff)
		if updateErr := recorder.update(repo.Path, func(s *RepoStatus) { s.LastError = err.Error() }); updateErr != nil {
			logger.Error("writing status file failed", "error", updateErr)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > d.maxBackoff {
			backoff = d.maxBackoff
		}
	}
}

// runRepoGuarded runs one repository's watch loop with panic recovery, so a
// bug triggered by one repository's state can't take down the others or the
// parent process.
func (d *Daemon) runRepoGuarded(ctx context.Context, repo config.Repository, trigger *repoTrigger, logger *slog.Logger, recorder *statusRecorder) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return runRepoLoop(ctx, d.gitFor(repo.Path), repo, trigger, d.hostname, logger, recorder)
}
