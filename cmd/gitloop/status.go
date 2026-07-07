package main

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/kohii/gitloop/internal/config"
	"github.com/kohii/gitloop/internal/daemon"
)

// statusPathFunc resolves the status file location. It's a package
// variable so tests can point it at a temp file instead of the real
// ~/Library/Caches/gitloop/status.json.
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
	fmt.Fprintln(w, "PATH\tPHASE\tLAST_COMMIT\tLAST_PUSH\tLAST_ERROR")
	for _, repo := range cfg.Repositories {
		st, ok := sf.Repos[repo.Path]
		phase, lastCommit, lastPush, lastError := "-", "-", "-", "-"
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
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", repo.Path, phase, lastCommit, lastPush, lastError)
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
