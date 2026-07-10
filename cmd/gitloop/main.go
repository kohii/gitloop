// Command gitloop is a daemon that watches one or more git repositories'
// working trees and keeps them synced with their remotes (auto commit /
// push / rebase).
package main

import (
	"fmt"
	"io"
	"os"
)

// version is the build version. It is "dev" for local builds; release builds
// can override it with -ldflags "-X main.version=...".
var version = "dev"

const usage = `gitloop watches one or more git repositories and keeps them synced with their remotes.

Usage:
  gitloop <command> [flags]

Commands:
  run         Start the sync daemon in the foreground
  install     Install and start the launchd agent
  uninstall   Stop and remove the launchd agent
  status      Show each configured repository's last known sync state
  lock        Advisory-lock helpers for external writers (see "gitloop lock -h")
  version     Print the version and exit

Use "gitloop <command> -h" for details on a command.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version":
		fmt.Fprintf(stdout, "gitloop %s\n", version)
		return 0
	case "run":
		return runCmd(rest, stdout, stderr)
	case "install":
		return installCmd(rest, stdout, stderr)
	case "uninstall":
		return uninstallCmd(rest, stdout, stderr)
	case "status":
		return statusCmd(rest, stdout, stderr)
	case "lock":
		return lockCmd(rest, os.Stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gitloop: unknown command %q\n\n%s", cmd, usage)
		return 1
	}
}
