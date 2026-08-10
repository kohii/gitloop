package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPlistContainsExpectedFields(t *testing.T) {
	plist := buildPlist("/usr/local/bin/gitloop", "/Users/kohii/.config/gitloop/config.yaml", "/Users/kohii/Library/Logs/gitloop.log")

	for _, want := range []string{
		"<key>Label</key>\n\t<string>dev.kohii.gitloop</string>",
		"<string>/usr/local/bin/gitloop</string>",
		"<string>run</string>",
		"<string>--config</string>",
		"<string>/Users/kohii/.config/gitloop/config.yaml</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>StandardOutPath</key>\n\t<string>/Users/kohii/Library/Logs/gitloop.log</string>",
		"<key>StandardErrorPath</key>\n\t<string>/Users/kohii/Library/Logs/gitloop.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q\nfull plist:\n%s", want, plist)
		}
	}
}

func TestBootstrapArgs(t *testing.T) {
	got := bootstrapArgs("/Users/kohii/Library/LaunchAgents/dev.kohii.gitloop.plist")
	want := []string{"bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), "/Users/kohii/Library/LaunchAgents/dev.kohii.gitloop.plist"}
	if len(got) != len(want) {
		t.Fatalf("bootstrapArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bootstrapArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBootoutArgs(t *testing.T) {
	got := bootoutArgs()
	want := []string{"bootout", "--wait", fmt.Sprintf("gui/%d/dev.kohii.gitloop", os.Getuid())}
	if len(got) != len(want) {
		t.Fatalf("bootoutArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bootoutArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestKickstartArgs(t *testing.T) {
	got := kickstartArgs()
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/dev.kohii.gitloop", os.Getuid())}
	if len(got) != len(want) {
		t.Fatalf("kickstartArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("kickstartArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestConfigPathFromPlist(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "config&shared.yaml")
	plistPath := filepath.Join(dir, "agent.plist")
	if err := os.WriteFile(plistPath, []byte(buildPlist("/usr/local/bin/gitloop", want, filepath.Join(dir, "gitloop.log"))), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := configPathFromPlist(plistPath)
	if err != nil {
		t.Fatalf("configPathFromPlist: %v", err)
	}
	if got != want {
		t.Errorf("configPathFromPlist = %q, want %q", got, want)
	}
}

func TestConfigPathFromPlistAcceptsEqualsForm(t *testing.T) {
	plistPath := filepath.Join(t.TempDir(), "agent.plist")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist><dict>
	<key>ProgramArguments</key>
	<array><string>/usr/local/bin/gitloop</string><string>run</string><string>--config=/tmp/config.yaml</string></array>
</dict></plist>`
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := configPathFromPlist(plistPath)
	if err != nil {
		t.Fatalf("configPathFromPlist: %v", err)
	}
	if got != "/tmp/config.yaml" {
		t.Errorf("configPathFromPlist = %q, want /tmp/config.yaml", got)
	}
}

func TestBuildPlistEscapesXMLValues(t *testing.T) {
	plist := buildPlist("/bin/gitloop", "/tmp/config&shared.yaml", "/tmp/log<1>.log")
	for _, want := range []string{
		"<string>/tmp/config&amp;shared.yaml</string>",
		"<string>/tmp/log&lt;1&gt;.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing escaped value %q:\n%s", want, plist)
		}
	}
}

func TestRegisterLaunchAgentReplacesExistingRegistration(t *testing.T) {
	var calls [][]string
	run := func(args []string, stdout, stderr io.Writer) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}

	if err := registerLaunchAgentWith(run, "/tmp/gitloop.plist", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("registerLaunchAgentWith: %v", err)
	}

	want := [][]string{bootoutArgs(), bootstrapArgs("/tmp/gitloop.plist")}
	if len(calls) != len(want) {
		t.Fatalf("launchctl calls = %v, want %v", calls, want)
	}
	for i := range want {
		if fmt.Sprint(calls[i]) != fmt.Sprint(want[i]) {
			t.Errorf("launchctl call %d = %v, want %v", i, calls[i], want[i])
		}
	}
}

func TestRegisterLaunchAgentIgnoresMissingExistingRegistration(t *testing.T) {
	var calls [][]string
	run := func(args []string, stdout, stderr io.Writer) error {
		calls = append(calls, append([]string(nil), args...))
		if len(calls) == 1 {
			return errors.New("service not found")
		}
		return nil
	}

	if err := registerLaunchAgentWith(run, "/tmp/gitloop.plist", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("registerLaunchAgentWith: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("launchctl call count = %d, want 2", len(calls))
	}
}
