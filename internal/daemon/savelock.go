package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// saveLockRetries bounds how many times a cycle retries acquiring the save
// lock (each attempt after the first separated by the repository's settle
// duration) before giving up and skipping the cycle until the next trigger.
const saveLockRetries = 3

// saveLock holds an advisory lock acquired via tryAcquireSaveLock. Its zero
// value (a nil *saveLock) represents "locking is disabled", and release is
// safe to call on it.
type saveLock struct {
	file     *os.File
	released bool
}

// release unlocks and closes the underlying file. It is a no-op if locking
// was disabled (l is nil) or if release has already been called — the caller
// can safely both `defer lock.release()` as a panic-safety net and call
// `lock.release()` explicitly before a non-lock phase (e.g. push).
func (l *saveLock) release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
}

// tryAcquireSaveLock attempts a single non-blocking flock(LOCK_EX) on path.
// This is how gitloop coordinates with an external process (e.g. a
// notes-app server) that advisory-locks the same file while it's mid-save
// on the repository: both sides must avoid touching the working tree while
// the other holds the lock.
//
// An empty path means locking is disabled (some other coordination
// mechanism, or none, applies); tryAcquireSaveLock reports success without
// touching the filesystem. ok is false, with no error, if the lock is held
// by someone else — that is the expected outcome the caller retries or
// skips a cycle on, not a failure.
func tryAcquireSaveLock(path string) (l *saveLock, ok bool, err error) {
	if path == "" {
		return nil, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, fmt.Errorf("creating save lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("opening save lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("flock save lock file: %w", err)
	}
	return &saveLock{file: f}, true, nil
}

// acquireSaveLockWithRetry retries tryAcquireSaveLock up to saveLockRetries
// times, waiting settle between attempts, before giving up. ok is false if
// every attempt found the lock held (or disabled path aside, an error
// occurred, which is logged and treated the same as "couldn't acquire").
func acquireSaveLockWithRetry(path string, settle time.Duration, logger *slog.Logger) (l *saveLock, ok bool) {
	for attempt := 1; attempt <= saveLockRetries; attempt++ {
		l, ok, err := tryAcquireSaveLock(path)
		if err != nil {
			logger.Error("acquiring save lock failed", "path", path, "error", err)
			return nil, false
		}
		if ok {
			return l, true
		}
		if attempt < saveLockRetries {
			time.Sleep(settle)
		}
	}
	return nil, false
}
