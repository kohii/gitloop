package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const lockUsage = `gitloop lock coordinates advisory locks with external writers.

Usage:
  gitloop lock hold <path>   Acquire flock on <path>, print "acquired", hold until stdin closes.
`

const lockHoldUsage = `gitloop lock hold <path>

  Take an exclusive, non-blocking flock on <path>, print "acquired" on stdout,
  then hold the lock until EOF on stdin. Intended to be spawned by an external
  writer (e.g. a Node process) that needs to coordinate with gitloop's own
  save-lock: the caller waits for "acquired", performs its writes, then closes
  stdin to release. Killing the process also releases the lock via fd close.

  <path> must be absolute. The parent directory is created if missing.
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
		return 1
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "gitloop lock hold: %v\n", err)
		return 1
	}
	// f.Close releases the flock via fd close (the primary release path for
	// crash-safety too). Best-effort — if the caller has already gone we can't
	// do anything about a Close error.
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintf(stderr, "gitloop lock hold: could not acquire lock on %s: %v\n", path, err)
		return 1
	}

	if _, err := io.WriteString(stdout, "acquired\n"); err != nil {
		fmt.Fprintf(stderr, "gitloop lock hold: writing acquired line: %v\n", err)
		return 1
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
