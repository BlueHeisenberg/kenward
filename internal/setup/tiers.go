package setup

import "github.com/BlueHeisenberg/kenward/internal/config"

// HostIsLocal reports whether a base URL names a machine on the household's own
// network.
//
// It is exported because three packages need the answer and there must not be three
// answers. `cmd/kenward/tiers.go` carried a second copy of this rule with a comment
// saying so — "if these two ever disagree, the setup wizard and `kenward doctor` will
// make different claims about the same kenward.yaml" — and naming exporting this as the
// right fix. The dashboard would have been the third copy, which is where a documented
// duplication stops being a note and starts being a defect.
//
// It is deliberately conservative: anything it cannot place is treated as leaving the
// house, because the two mistakes do not cost the same. Calling a household machine
// "cloud" understates what the household has; calling a provider "local" tells somebody
// their private conversations stay home when they do not.
func HostIsLocal(baseURL string) bool { return isLocal(baseURL) }

// LocalTiers returns, for each tier name, whether every endpoint carrying it is a
// machine the household controls.
//
// One cloud endpoint tagged with a tier makes the whole tier a way out, which is the
// honest reading: routing may pick any endpoint in a tier, so a chain naming that tier
// may reach a provider.
func LocalTiers(endpoints []config.EndpointConfig) map[string]bool {
	local := map[string]bool{}
	for _, ep := range endpoints {
		for _, tag := range ep.Tags {
			if _, seen := local[tag]; !seen {
				local[tag] = true
			}
			if !isLocal(ep.BaseURL) {
				local[tag] = false
			}
		}
	}
	return local
}

// StaysHome reports whether every tier in a chain is one the household controls, in the
// sense privacy.TierNote's local parameter means.
//
// An empty chain is not local. It is a configuration that refuses everything, and
// TierNote says so in its own words rather than being told it stays home.
func StaysHome(local map[string]bool, chain []string) bool {
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
