package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
`, escapePlistText(launchAgentLabel), escapePlistText(execPath), escapePlistText(configPath), escapePlistText(logPath), escapePlistText(logPath))
}

// escapePlistText escapes a value embedded in an XML string element.
func escapePlistText(value string) string {
	var escaped strings.Builder
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

// configPathFromPlist reads the config path from the launch agent's
// ProgramArguments. The plist is the source of truth because install accepts
// a custom config path that may differ from the CLI's default.
func configPathFromPlist(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading plist %s: %w", path, err)
	}

	args, err := programArgumentsFromPlist(data)
	if err != nil {
		return "", fmt.Errorf("parsing plist %s: %w", path, err)
	}
	for i, arg := range args {
		var configPath string
		switch {
		case arg == "--config" || arg == "-config":
			if i+1 < len(args) {
				configPath = args[i+1]
			}
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-config="):
			configPath = strings.TrimPrefix(arg, "-config=")
		}
		if configPath == "" {
			continue
		}
		if !filepath.IsAbs(configPath) {
			return "", fmt.Errorf("config path %q in plist must be absolute", configPath)
		}
		return filepath.Clean(configPath), nil
	}
	return "", fmt.Errorf("plist %s has no config path in ProgramArguments", path)
}

// programArgumentsFromPlist extracts the string values under the
// ProgramArguments key without depending on the order of other plist keys.
func programArgumentsFromPlist(data []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	dictDepth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("ProgramArguments is missing")
		}
		if err != nil {
			return nil, err
		}

		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "dict" {
				dictDepth++
				continue
			}
			if value.Name.Local != "key" || dictDepth != 1 {
				continue
			}

			var key string
			if err := decoder.DecodeElement(&key, &value); err != nil {
				return nil, err
			}
			if key != "ProgramArguments" {
				continue
			}

			for {
				token, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				array, ok := token.(xml.StartElement)
				if !ok {
					continue
				}
				if array.Name.Local != "array" {
					return nil, fmt.Errorf("ProgramArguments is not an array")
				}

				var values struct {
					Strings []string `xml:"string"`
				}
				if err := decoder.DecodeElement(&values, &array); err != nil {
					return nil, err
				}
				return values.Strings, nil
			}
		case xml.EndElement:
			if value.Name.Local == "dict" && dictDepth > 0 {
				dictDepth--
			}
		}
	}
}

// bootstrapArgs returns the launchctl arguments that load and start the
// agent: `launchctl bootstrap gui/<uid> <plistPath>`.
func bootstrapArgs(plistPath string) []string {
	return []string{"bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath}
}

// bootoutArgs returns the launchctl arguments that stop and unload the
// agent. --wait is required before a replacement bootstrap: bootout can
// return while launchd is still stopping the old process.
func bootoutArgs() []string {
	return []string{"bootout", "--wait", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)}
}

// kickstartArgs returns the launchctl arguments that restart the registered
// agent while retaining its existing plist: `launchctl kickstart -k ...`.
func kickstartArgs() []string {
	return []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)}
}
