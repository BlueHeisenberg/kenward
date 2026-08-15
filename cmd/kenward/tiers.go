package main

import (
	"net"
	"net/url"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// localSuffixes are the DNS suffixes that only ever name a machine the household
// controls.
//
// This list, and hostIsLocal below, mirror internal/setup/probe.go. The duplication
// is deliberate but not comfortable: internal/setup keeps its version unexported, and
// privacy.TierNote asks its caller to decide which tiers count as local precisely
// because that decision is configuration rather than something the privacy package
// can know. If these two ever disagree, the setup wizard and `kenward doctor` will
// make different claims about the same kenward.yaml, which is the failure the whole
// single-source-of-truth arrangement around internal/privacy exists to prevent.
// Exporting setup's version and deleting this one would be the right fix.
var localSuffixes = []string{".local", ".lan", ".home", ".internal", ".ts.net", ".tail", ".home.arpa"}

// hostIsLocal reports whether a base URL names a machine on the household's own
// network. It is deliberately conservative: anything it cannot place is treated as
// leaving the house, because the cost of the two mistakes is not symmetric. Calling a
// household machine "cloud" understates what the household has; calling a provider
// "local" tells somebody their private conversations stay home when they do not.
func hostIsLocal(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	// A bare name with no dots can only be resolved by something on the local
	// network — a hosts file, mDNS, a router's DNS — so it is in the house by
	// construction.
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range localSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// localTiers returns the set of tier names every one of whose endpoints is a machine
// the household controls.
//
// A tier is local only if nothing tagged with it leaves the house. One cloud endpoint
// carrying a tier's tag makes the whole tier a way out, which is the honest reading:
// routing may pick any endpoint in a tier, so a chain naming that tier may reach a
// provider.
func localTiers(cfg *config.Config) map[string]bool {
	local := map[string]bool{}
	for _, ep := range cfg.Endpoints {
		for _, tag := range ep.Tags {
			if _, seen := local[tag]; !seen {
				local[tag] = true
			}
			if !hostIsLocal(ep.BaseURL) {
				local[tag] = false
			}
		}
	}
	return local
}

// staysHome reports whether every tier in a chain is one the household controls, in
// the sense privacy.TierNote's `local` parameter means. An empty chain is not local:
// it is a configuration that refuses everything, and TierNote says so itself.
func staysHome(local map[string]bool, chain []string) bool {
	if len(chain) == 0 {
		return false
	}
	for _, tier := range chain {
		if !local[tier] {
			return false
		}
	}
	return true
}
