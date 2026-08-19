package main

import (
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/link"
)

// TestLinkAsksForTheLAN is the counterpart of internal/memory's
// TestServeAsksForTheLANUnlessToldOtherwise, and it exists for the same reason.
//
// Both halves of the link handshake bind and browse the pod network in production
// and loopback only in a test, and the test setting is the one a developer reaches
// for when a Windows Firewall prompt interrupts them. Reaching for it here instead
// of in the test would leave every pod with a desk nobody can find and a browse
// that finds nothing — visible only on real podman, weeks later, as a household
// whose members never get the shared space.
func TestLinkAsksForTheLAN(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Household.SharedSpace = "dac31e70-72e4-4b10-9cef-a6276c4a87b8"
	opts := linkOptions(cfg, nil, nil, "a-household-link-key-of-real-length")
	if opts.LoopbackOnly {
		t.Error("a pod's link handshake is confined to loopback; its siblings are not there")
	}
	if opts.NoDiscovery {
		t.Error("a pod's link desk does not advertise itself; nothing can find it")
	}
	if string(opts.Space) != cfg.Household.SharedSpace {
		t.Errorf("Space = %q, want the household's shared space", opts.Space)
	}
	if len(opts.Addrs) != 0 {
		t.Errorf("a pod was given fixed link addresses %v; a pod's address changes on every recreation", opts.Addrs)
	}
	if len(opts.Key) < link.MinKeyLen {
		t.Error("the link key did not reach the options")
	}
}
