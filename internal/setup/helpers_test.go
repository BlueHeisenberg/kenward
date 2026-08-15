package setup

import (
	"context"
	"time"
)

// fixedProbe returns a Probe that always reports the same thing, so a test can
// exercise the flow without a network and without waiting.
func fixedProbe(state Reachability) Probe {
	return func(ctx context.Context, baseURL string) ProbeResult {
		if _, err := dialAddress(baseURL); err != nil {
			return ProbeResult{State: BadURL, Err: err}
		}
		addr, _ := dialAddress(baseURL)
		return ProbeResult{State: state, Elapsed: 12 * time.Millisecond, Addr: addr}
	}
}
