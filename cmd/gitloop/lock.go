package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Exit codes for `gitloop lock hold`. 0 (success) and 2 (usage error, from
// flag.ContinueOnError below) follow the CLI convention; 1 and 3 are
// deliberately split so the caller can tell "I lost the race for the lock"
// (retry with backoff) from "something else went wrong" (surface the error):
//
//	1 — I/O or filesystem error (mkdir, open, unexpected flock failure).
//	3 — lock is already held by another process (EWOULDBLOCK/EAGAIN).
const (
	lockHoldExitIOError    = 1
	lockHoldExitContention = 3
)

const lockUsage = `gitloop lock coordinates advisory locks with external writers.

Usage:
  gitloop lock hold <path>   Acquire flock on <path>, print "acquired", hold until stdin closes.

Exit codes for "gitloop lock hold":
  0 - lock acquired and released cleanly.
  1 - I/O error (couldn't open path, unexpected flock failure).
  2 - usage error (missing/relative path, unknown flag).
  3 - lock is already held by another process.
`

const lockHoldUsage = `gitloop lock hold <path>

  Take an exclusive, non-blocking flock on <path>, print "acquired" on stdout,
  then hold the lock until EOF on stdin. Intended to be spawned by an external
  writer (e.g. a Node process) that needs to coordinate with gitloop's own
  save-lock: the caller waits for "acquired", performs its writes, then closes
  stdin to release. Killing the process also releases the lock via fd close.

  <path> must be absolute. The parent directory is created if missing.

  Exit codes:
    0 - lock acquired and released cleanly (stdin closed).
    1 - I/O error (couldn't open path, unexpected flock failure).
    2 - usage error (missing/relative path).
    3 - lock is already held by another process — the caller can retry.
`

// lockCmd dispatches "gitloop lock" subcommands. It is separate from lockHoldCmd
// so future subcommands (e.g. `lock status`) can slot in without another
// top-level command.
func lockCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, lockUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, lockUsage)
		return 0
	case "hold":
		return lockHoldCmd(rest, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gitloop lock: unknown subcommand %q\n\n%s", sub, lockUsage)
		return 2
	}
}

// lockHoldCmd acquires an exclusive flock on <path> and holds it until stdin
// closes. It is meant to be spawned as a child process by an external writer
// (e.g. a Node server) sharing a repository with gitloop: the writer waits
// for the "acquired\n" line on stdout before touching the working tree, and
// closes stdin (or exits, killing this process) to release.
//
// The flock is tied to the file descriptor, so an unclean exit (SIGKILL, host
// crash) still releases the lock at the OS level — the caller does not need
// to depend on a graceful shutdown path.
func lockHoldCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lock hold", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, lockHoldUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, lockHoldUsage)
		return 2
	}
	path := fs.Arg(0)
	if !filepath.IsAbs(path) {
		fmt.Fprintf(stderr, "gitloop lock hold: <path> must be absolute (got %q)\n", path)
		return 2
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(stderr, "gitloop lock hold: %v\n", err)
		return lockHoldExitIOError
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "gitloop lock hold: %v\n", err)
		return lockHoldExitIOError
	}
	// f.Close releases the flock via fd close (the primary release path for
	// crash-safety too). Best-effort — if the caller has already gone we can't
	// do anything about a Close error.
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// LOCK_NB signals "already held" via EWOULDBLOCK/EAGAIN; anything
		// else is an unexpected failure the caller shouldn't just retry on.
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			fmt.Fprintf(stderr, "gitloop lock hold: lock on %s is already held\n", path)
			return lockHoldExitContention
		}
		fmt.Fprintf(stderr, "gitloop lock hold: could not acquire lock on %s: %v\n", path, err)
		return lockHoldExitIOError
	}

	if _, err := io.WriteString(stdout, "acquired\n"); err != nil {
		fmt.Fprintf(stderr, "gitloop lock hold: writing acquired line: %v\n", err)
		return lockHoldExitIOError
	}
	// os.Stdout is unbuffered on Unix, but be explicit when the caller
	// happens to be an *os.File so a pipe reader sees the line immediately.
	if syncer, ok := stdout.(interface{ Sync() error }); ok {
		_ = syncer.Sync()
	}

	// Block until stdin closes (EOF). Reads are discarded — the caller uses
	// stdin only as a "release" channel, not to send data.
	_, _ = io.Copy(io.Discard, stdin)
	return 0
}
