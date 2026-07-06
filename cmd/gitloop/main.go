// Command gitloop is a daemon that watches a git repository's working tree
// and keeps it synced with its remote (auto commit / push / rebase).
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is the build version. It is "dev" for local builds; release builds
// can override it with -ldflags "-X main.version=...".
var version = "dev"

const usage = `gitloop watches a git repository and keeps it synced with its remote.

Usage:
  gitloop <command> [flags]

Commands:
  run       Start the sync daemon
  version   Print the version and exit

Use "gitloop <command> -h" for details on a command.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}

	switch cmd, rest := args[0], args[1:]; cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version":
		fmt.Fprintf(stdout, "gitloop %s\n", version)
		return 0
	case "run":
		return runCmd(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gitloop: unknown command %q\n\n%s", cmd, usage)
		return 1
	}
}

func runCmd(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the gitloop config file (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *configPath == "" {
		fmt.Fprintln(stderr, "gitloop run: -config is required")
		return 2
	}

	// TODO: load config, start the daemon's watch/debounce/sync loop.
	fmt.Fprintf(stdout, "gitloop run: not implemented yet (config: %s)\n", *configPath)
	return 0
}
