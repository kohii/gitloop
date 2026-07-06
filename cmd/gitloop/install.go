package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kohii/gitloop/internal/config"
)

func installCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPathOrEmpty(), "path to the gitloop config file to run with")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "gitloop install: -config is required (could not determine a default)")
		return 2
	}

	absConfigPath, err := filepath.Abs(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "gitloop install: resolving config path: %v\n", err)
		return 1
	}
	if _, err := config.Load(absConfigPath); err != nil {
		fmt.Fprintf(stderr, "gitloop install: config at %s is invalid: %v\n", absConfigPath, err)
		return 1
	}

	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "gitloop install: resolving executable path: %v\n", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}

	logPath, err := defaultLogPath()
	if err != nil {
		fmt.Fprintf(stderr, "gitloop install: %v\n", err)
		return 1
	}
	plistPath, err := launchAgentPlistPath()
	if err != nil {
		fmt.Fprintf(stderr, "gitloop install: %v\n", err)
		return 1
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "gitloop install: %v\n", err)
		return 1
	}
	plist := buildPlist(execPath, absConfigPath, logPath)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		fmt.Fprintf(stderr, "gitloop install: writing %s: %v\n", plistPath, err)
		return 1
	}
	fmt.Fprintf(stdout, "gitloop install: wrote %s\n", plistPath)

	cmd := exec.Command("launchctl", bootstrapArgs(plistPath)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "gitloop install: launchctl bootstrap failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "gitloop install: registered %s with launchd (logs: %s)\n", launchAgentLabel, logPath)
	return 0
}

func uninstallCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cmd := exec.Command("launchctl", bootoutArgs()...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// Not fatal: the agent may already be stopped (e.g. after a reboot
		// without a re-bootstrap), and we still want to remove the plist.
		fmt.Fprintf(stderr, "gitloop uninstall: launchctl bootout failed, continuing to remove the plist: %v\n", err)
	}

	plistPath, err := launchAgentPlistPath()
	if err != nil {
		fmt.Fprintf(stderr, "gitloop uninstall: %v\n", err)
		return 1
	}
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gitloop uninstall: removing %s: %v\n", plistPath, err)
		return 1
	}
	fmt.Fprintf(stdout, "gitloop uninstall: removed %s\n", plistPath)
	return 0
}
