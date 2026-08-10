package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReloadCmdValidatesInstalledConfigAndKickstartsAgent(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("repositories:\n  - path: /tmp/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(dir, "agent.plist")
	if err := os.WriteFile(plistPath, []byte(buildPlist("/usr/local/bin/gitloop", configPath, filepath.Join(dir, "gitloop.log"))), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls [][]string
	run := func(args []string, stdout, stderr io.Writer) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := reloadCmdWith(nil, &stdout, &stderr, func() (string, error) { return plistPath, nil }, run)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != strings.Join(kickstartArgs(), " ") {
		t.Errorf("launchctl calls = %v, want %v", calls, kickstartArgs())
	}
	if !strings.Contains(stdout.String(), configPath) {
		t.Errorf("stdout = %q, want config path", stdout.String())
	}
}

func TestReloadCmdRejectsInvalidConfigBeforeKickstart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("repositories:\n  - unknown: value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(dir, "agent.plist")
	if err := os.WriteFile(plistPath, []byte(buildPlist("/usr/local/bin/gitloop", configPath, filepath.Join(dir, "gitloop.log"))), 0o644); err != nil {
		t.Fatal(err)
	}

	called := false
	run := func(args []string, stdout, stderr io.Writer) error {
		called = true
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := reloadCmdWith(nil, &stdout, &stderr, func() (string, error) { return plistPath, nil }, run)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if called {
		t.Error("launchctl was called for an invalid config")
	}
	if !strings.Contains(stderr.String(), "invalid") {
		t.Errorf("stderr = %q, want invalid-config error", stderr.String())
	}
}

func TestReloadCmdReportsMissingAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := reloadCmdWith(nil, &stdout, &stderr, func() (string, error) {
		return filepath.Join(t.TempDir(), "missing.plist"), nil
	}, func(args []string, stdout, stderr io.Writer) error {
		return errors.New("must not be called")
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gitloop install") {
		t.Errorf("stderr = %q, want install guidance", stderr.String())
	}
}

func TestReloadCmdRejectsExtraArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := reloadCmdWith([]string{"unexpected"}, &stdout, &stderr, func() (string, error) {
		t.Fatal("plist path should not be resolved")
		return "", nil
	}, func(args []string, stdout, stderr io.Writer) error {
		t.Fatal("launchctl should not be called")
		return nil
	})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestReloadCmdReportsKickstartFailure(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("repositories:\n  - path: /tmp/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(dir, "agent.plist")
	if err := os.WriteFile(plistPath, []byte(buildPlist("/usr/local/bin/gitloop", configPath, filepath.Join(dir, "gitloop.log"))), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := reloadCmdWith(nil, &stdout, &stderr, func() (string, error) { return plistPath, nil }, func(args []string, stdout, stderr io.Writer) error {
		return errors.New("service not loaded")
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gitloop install") {
		t.Errorf("stderr = %q, want install guidance", stderr.String())
	}
}
