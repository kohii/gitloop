package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kohii/gitloop/internal/config"
)

// reloadCmd restarts the registered launchd agent after validating the
// configuration path stored in its plist. It deliberately does not rewrite
// the plist; use install when changing the executable or config path.
func reloadCmd(args []string, stdout, stderr io.Writer) int {
	return reloadCmdWith(args, stdout, stderr, launchAgentPlistPath, runLaunchctl)
}

func reloadCmdWith(args []string, stdout, stderr io.Writer, plistPathFunc func() (string, error), run launchctlRunner) int {
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gitloop reload")
		fmt.Fprintln(stderr, "Validate the installed config and restart the launchd agent.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "gitloop reload: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	plistPath, err := plistPathFunc()
	if err != nil {
		fmt.Fprintf(stderr, "gitloop reload: %v\n", err)
		return 1
	}

	configPath, err := configPathFromPlist(plistPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "gitloop reload: launchd agent is not installed (missing %s); run gitloop install first\n", plistPath)
		} else {
			fmt.Fprintf(stderr, "gitloop reload: %v\n", err)
		}
		return 1
	}
	if _, err := config.Load(configPath); err != nil {
		fmt.Fprintf(stderr, "gitloop reload: config at %s is invalid: %v\n", configPath, err)
		return 1
	}

	if err := run(kickstartArgs(), stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "gitloop reload: launchctl kickstart failed: %v (run gitloop install if the launch agent is not loaded)\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "gitloop reload: restarted %s using %s\n", launchAgentLabel, configPath)
	return 0
}
