package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("stdout = %q, want usage text", stdout.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "gitloop") {
		t.Errorf("stdout = %q, want it to mention gitloop", stdout.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "bogus") {
		t.Errorf("stderr = %q, want it to mention the unknown command", stderr.String())
	}
}

func TestRunCmdRejectsMissingConfigFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--config", "/nonexistent/gitloop-config.yaml"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Error("expected an error message on stderr")
	}
}
