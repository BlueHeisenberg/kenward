package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// dashboardConfig is the only part of kenward.yaml this binary reads.
//
// It is decoded on its own rather than through internal/config because the desktop
// wrapper has no business validating a household — that is `kenward run`'s job and
// `kenward doctor`'s, and a second validator here would be a second opinion to
// disagree with. yaml.v3 ignores every key it is not asked about, so this survives
// the file gaining fields.
//
// ponytail: the dashboard does not exist in this tree yet — it is being built
// alongside. This struct is the one place its configuration is read, so wiring the
// real key names is an edit here and nowhere else. Until then a household with no
// dashboard block gets a greyed-out menu item, which is the correct outcome either
// way and the reason the item is disabled rather than opening a dead URL.
type dashboardConfig struct {
	Dashboard struct {
		// Enabled is a pointer so that an absent key and an explicit `false` are
		// distinguishable: a file that configures an address but says nothing
		// about enablement means the dashboard is wanted.
		Enabled *bool  `yaml:"enabled"`
		Addr    string `yaml:"addr"`
		Port    int    `yaml:"port"`
	} `yaml:"dashboard"`
}

// dashboardURL returns the address to open, or "" when this household has no
// dashboard.
//
// Empty is a normal answer, not an error. The dashboard is optional, headless is
// first-class, and the menu item is disabled rather than launching a browser at a
// port nothing is listening on — a blank error page is a worse answer than a greyed
// item that says why.
func dashboardURL(configPath string) string {
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	var c dashboardConfig
	if err := yaml.NewDecoder(f).Decode(&c); err != nil {
		return ""
	}
	d := c.Dashboard
	if d.Enabled != nil && !*d.Enabled {
		return ""
	}
	switch {
	case d.Addr != "":
		return "http://" + loopback(d.Addr)
	case d.Port != 0:
		return "http://127.0.0.1:" + strconv.Itoa(d.Port)
	default:
		return ""
	}
}

// loopback rewrites a listen address into one a browser on this machine can reach.
//
// A server configured to listen on 0.0.0.0:8080 or on :8080 is listening on every
// interface, and "http://0.0.0.0:8080" is not a thing a browser can usefully open. An
// address that names a real host is left alone.
func loopback(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not host:port at all — a bare port, most likely.
		if _, convErr := strconv.Atoi(addr); convErr == nil {
			return "127.0.0.1:" + addr
		}
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// resolveConfigPath mirrors the rule in cmd/kenward/paths.go: the environment, then
// kenward.yaml in the working directory, then the per-OS configuration location.
//
// It is a copy, and that is the lesser of two evils. The original lives in the daemon's
// own `main` package where nothing can import it, and the shared home for it would be
// internal/config — which the daemon must keep building without cgo and which is not
// this change's to reshape. Both copies are eight lines and are checked against each
// other by a test; the flag form is absent here because this binary takes no flags.
func resolveConfigPath() string {
	if v := os.Getenv("KENWARD_CONFIG"); v != "" {
		return v
	}
	if _, err := os.Stat(setup.DefaultConfigFileName); err == nil {
		return setup.DefaultConfigFileName
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "kenward", setup.DefaultConfigFileName)
	}
	return setup.DefaultConfigFileName
}
