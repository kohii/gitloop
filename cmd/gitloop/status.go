package main

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/kohii/gitloop/internal/config"
	"github.com/kohii/gitloop/internal/daemon"
)

// statusPathFunc resolves the status file location. It's a package
// variable so tests can point it at a temp file instead of the real
// ~/Library/Application Support/gitloop/status.json.
var statusPathFunc = daemon.DefaultStatusPath

func statusCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPathOrEmpty(), "path to the gitloop config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "gitloop status: -config is required (could not determine a default)")
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "gitloop status: %v\n", err)
		return 1
	}

	statusPath, err := statusPathFunc()
	if err != nil {
		fmt.Fprintf(stderr, "gitloop status: %v\n", err)
		return 1
	}
	sf, err := daemon.LoadStatusFile(statusPath)
	if err != nil {
		fmt.Fprintf(stderr, "gitloop status: reading %s: %v\n", statusPath, err)
		return 1
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tPHASE\tLAST_COMMIT\tLAST_PUSH\tLAST_ERROR\tLAST_AI_RESOLVE")
	for _, repo := range cfg.Repositories {
		st, ok := sf.Repos[repo.Path]
		phase, lastCommit, lastPush, lastError, lastAIResolve := "-", "-", "-", "-", "-"
		switch {
		case !ok:
			lastError = "not yet synced"
		default:
			if st.Phase != "" {
				phase = st.Phase
			}
			if st.LastCommit != "" {
				lastCommit = st.LastCommit
			}
			if st.LastPush != "" {
				lastPush = st.LastPush
			}
			if st.LastError != "" {
				lastError = st.LastError
			}
			// An outstanding AI-resolve error takes priority over the last
			// success time: it's the more actionable, silent-failure signal
			// this column exists for.
			switch {
			case st.LastAIResolveError != "":
				lastAIResolve = "error: " + st.LastAIResolveError
			case !st.LastAIResolveAt.IsZero():
				lastAIResolve = st.LastAIResolveAt.Format(time.RFC3339)
			}
		}
		// A commit-only repository never pushes by design. Left as "-" that
		// reads like a push that hasn't succeeded yet, i.e. a stalled sync.
		if !repo.SyncsRemote() {
			lastPush = "n/a"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", repo.Path, phase, lastCommit, lastPush, lastError, lastAIResolve)
	}
	return flushOrErr(w, stderr)
}

func flushOrErr(w *tabwriter.Writer, stderr io.Writer) int {
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "gitloop status: %v\n", err)
		return 1
	}
	return 0
}
