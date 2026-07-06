package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// launchAgentLabel is the launchd service label gitloop registers itself
// under.
const launchAgentLabel = "dev.kohii.gitloop"

// launchAgentPlistPath returns where the launchd agent plist lives:
// ~/Library/LaunchAgents/dev.kohii.gitloop.plist.
func launchAgentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// defaultLogPath returns where the launchd agent's stdout/stderr are
// redirected: ~/Library/Logs/gitloop.log.
func defaultLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "gitloop.log"), nil
}

// buildPlist renders the launchd agent property list. It is a pure string
// builder so both the install command and tests can inspect its output
// without touching the filesystem or launchctl.
func buildPlist(execPath, configPath, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>run</string>
		<string>--config</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchAgentLabel, execPath, configPath, logPath, logPath)
}

// bootstrapArgs returns the launchctl arguments that load and start the
// agent: `launchctl bootstrap gui/<uid> <plistPath>`.
func bootstrapArgs(plistPath string) []string {
	return []string{"bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath}
}

// bootoutArgs returns the launchctl arguments that stop and unload the
// agent: `launchctl bootout gui/<uid>/<label>`.
func bootoutArgs() []string {
	return []string{"bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)}
}
