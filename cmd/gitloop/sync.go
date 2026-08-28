package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/kohii/gitloop/internal/daemon"
)

// controlSocketPathFunc resolves the control socket location. It's a package
// variable so tests can point it at a temp socket instead of the real one
// under ~/Library/Application Support/gitloop.
var controlSocketPathFunc = daemon.ControlSocketPath

const syncUsage = `gitloop sync asks the running daemon to sync now, instead of waiting for its
next interval.

Usage:
  gitloop sync [<path>...]

With no arguments, every configured repository is synced. Paths must name
repositories from the daemon's config; they are matched after being resolved
to absolute paths.

The command returns as soon as the daemon has accepted the request, not when
the sync has finished.
`

func syncCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, syncUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	paths := make([]string, 0, fs.NArg())
	for _, arg := range fs.Args() {
		abs, err := filepath.Abs(arg)
		if err != nil {
			fmt.Fprintf(stderr, "gitloop sync: %v\n", err)
			return 1
		}
		paths = append(paths, abs)
	}

	socketPath, err := controlSocketPathFunc()
	if err != nil {
		fmt.Fprintf(stderr, "gitloop sync: %v\n", err)
		return 1
	}

	resp, err := daemon.RequestSync(socketPath, paths)
	if errors.Is(err, daemon.ErrDaemonNotRunning) {
		fmt.Fprintln(stderr, "gitloop sync: no gitloop daemon is running (start one with `gitloop run` or `gitloop install`)")
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "gitloop sync: %v\n", err)
		return 1
	}
	if resp.Error != "" {
		fmt.Fprintf(stderr, "gitloop sync: %s\n", resp.Error)
		return 1
	}

	for _, path := range resp.Triggered {
		fmt.Fprintf(stdout, "syncing %s\n", path)
	}
	return 0
}
