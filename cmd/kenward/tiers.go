package main

import (
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// The rule for what counts as a machine in the house lives in internal/setup, and these
// three are one-line delegations to it.
//
// This file used to carry a second copy, with a comment explaining that the duplication
// was deliberate but uncomfortable and that exporting setup's version was the right fix.
// The admin dashboard would have been the third copy, so the fix was taken. If these
// ever disagreed, the setup wizard, `kenward doctor` and the dashboard would make
// different claims about the same kenward.yaml — which is the failure the whole
// arrangement around internal/privacy exists to prevent.

// hostIsLocal reports whether a base URL names a machine on the household's own network.
func hostIsLocal(baseURL string) bool { return setup.HostIsLocal(baseURL) }

// localTiers returns the set of tier names every one of whose endpoints is a machine the
// household controls.
func localTiers(cfg *config.Config) map[string]bool { return setup.LocalTiers(cfg.Endpoints) }

// staysHome reports whether every tier in a chain is one the household controls.
func staysHome(local map[string]bool, chain []string) bool { return setup.StaysHome(local, chain) }
