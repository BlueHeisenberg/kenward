package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func openBrowser(url string) error { return exec.Command("open", url).Start() }

// --- start at login ---------------------------------------------------------------
//
// A LaunchAgent in the user's own ~/Library/LaunchAgents, which is the per-user,
// no-admin, no-elevation form: launchd loads everything there at login, so writing the
// file is the whole of turning it on and deleting it is the whole of turning it off.
//
// Nothing is bootstrapped into the running launchd session. Turning the option on
// means "start this next time I log in", which is what the words say, and running
// `launchctl bootstrap` would additionally start a second copy of a program the user
// is looking at right now.

const launchAgentLabel = "io.kenward.desktop"

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

func loginEnabled() bool {
	path, err := launchAgentPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func setLoginEnabled(on bool, configPath string) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(launchAgentPlist(self, configPath)), 0o644)
}

// launchAgentPlist is written out rather than assembled with a plist library: it is
// eight fixed lines and two paths, and a dependency to produce them would be a
// dependency to keep.
//
// KeepAlive is deliberately absent. launchd restarting this wrapper would collide with
// the wrapper restarting the daemon — two supervisors racing over one household — and
// the wrapper is the one that knows how to drain.
func launchAgentPlist(exe, configPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>--config</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`, launchAgentLabel, exe, configPath)
}
