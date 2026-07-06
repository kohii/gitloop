package main

import (
	"fmt"
	"os"
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
	want := []string{"bootout", fmt.Sprintf("gui/%d/dev.kohii.gitloop", os.Getuid())}
	if len(got) != len(want) {
		t.Fatalf("bootoutArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bootoutArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
