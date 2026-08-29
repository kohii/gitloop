package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// processLockName is the file a running daemon flocks for its whole lifetime,
// kept next to the status file in gitloop's state directory.
const processLockName = "daemon.lock"

// ErrDaemonAlreadyRunning is returned when another gitloop daemon already
// holds the process lock.
var ErrDaemonAlreadyRunning = errors.New("another gitloop daemon is already running")

// processLock is a daemon's claim on being the one daemon for this user.
//
// Two daemons sharing a state directory would overwrite each other's status
// file and fight over the control socket, and would drive the same checkouts
// from two sets of watch loops. The lock is what makes both of those the
// startup error they should be — and it is also what makes removing a
// leftover socket file safe, since holding it proves no live daemon is
// listening on one.
//
// The lock is advisory and released by the kernel when the process exits, so
// a daemon that was killed leaves nothing to clean up by hand.
type processLock struct {
	file *os.File
}

func acquireProcessLock(path string) (*processLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the state directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrDaemonAlreadyRunning
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return &processLock{file: f}, nil
}

func (l *processLock) release() {
	if l == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
}
