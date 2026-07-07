package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTryAcquireSaveLockEmptyPathDisablesLocking(t *testing.T) {
	l, ok, err := tryAcquireSaveLock("")
	if err != nil || !ok {
		t.Fatalf("tryAcquireSaveLock(\"\") = (%v, %v, %v), want (nil, true, nil)", l, ok, err)
	}
	l.release() // must not panic on the disabled zero value
}

func TestTryAcquireSaveLockCreatesParentDirAndLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "save.lock")

	l, ok, err := tryAcquireSaveLock(path)
	if err != nil || !ok {
		t.Fatalf("tryAcquireSaveLock: (%v, %v, %v), want ok", l, ok, err)
	}
	defer l.release()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected lock file to exist at %s: %v", path, err)
	}
}

func TestTryAcquireSaveLockFailsWhileHeldElsewhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "save.lock")

	holder := openAndFlock(t, path)
	defer func() {
		unix.Flock(int(holder.Fd()), unix.LOCK_UN)
		holder.Close()
	}()

	l, ok, err := tryAcquireSaveLock(path)
	if err != nil {
		t.Fatalf("tryAcquireSaveLock: unexpected error %v", err)
	}
	if ok {
		l.release()
		t.Fatal("tryAcquireSaveLock() ok = true, want false while another holder has the lock")
	}
}

func TestAcquireSaveLockWithRetryGivesUpAfterHeldThroughout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "save.lock")
	holder := openAndFlock(t, path)
	defer func() {
		unix.Flock(int(holder.Fd()), unix.LOCK_UN)
		holder.Close()
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()
	l, ok := acquireSaveLockWithRetry(path, 10*time.Millisecond, logger)
	if ok {
		l.release()
		t.Fatal("acquireSaveLockWithRetry() ok = true, want false: lock is held for the whole retry window")
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("acquireSaveLockWithRetry returned after %v, want it to have retried with waits in between", elapsed)
	}
}

func TestAcquireSaveLockWithRetrySucceedsOnceReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "save.lock")
	holder := openAndFlock(t, path)

	go func() {
		time.Sleep(20 * time.Millisecond)
		unix.Flock(int(holder.Fd()), unix.LOCK_UN)
		holder.Close()
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	l, ok := acquireSaveLockWithRetry(path, 15*time.Millisecond, logger)
	if !ok {
		t.Fatal("acquireSaveLockWithRetry() ok = false, want true once the other holder releases")
	}
	l.release()
}

func openAndFlock(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock %s: %v", path, err)
	}
	return f
}
