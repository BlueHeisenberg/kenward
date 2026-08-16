package setup

import (
	"context"
	"os/exec"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// testSpaces is a lore home in the shape of the real one this was developed
// against: shared spaces for the household and for each person, and a personal
// space that kenward can never use.
var testSpaces = []memory.Space{
	{ID: "dac31e70-72e4-4b10-9cef-a6276c4a87b8", Name: "kenward-test-household", Kind: "shared", Entries: 12},
	{ID: "7d5047bb-d939-4539-b3db-8b6221a2e245", Name: "kenward-test-david", Kind: "shared", Entries: 3},
	{ID: "b1c2d3e4-0000-4000-8000-000000000001", Name: "kenward-test-maria", Kind: "shared"},
	{ID: "b1c2d3e4-0000-4000-8000-000000000002", Name: "kenward-test-ana", Kind: "shared"},
	{ID: "b1c2d3e4-0000-4000-8000-000000000003", Name: "spare", Kind: "shared"},
	{ID: "9f000000-0000-4000-8000-00000000000p", Name: "personal", Kind: "personal", Entries: 480},
}

const (
	householdSpaceID = "dac31e70-72e4-4b10-9cef-a6276c4a87b8"
	davidSpaceID     = "7d5047bb-d939-4539-b3db-8b6221a2e245"
	personalSpaceID  = "9f000000-0000-4000-8000-00000000000p"
)

// fixedSpaces returns a SpaceLister that answers with a fixed listing.
func fixedSpaces(spaces []memory.Space) SpaceLister {
	return func(context.Context) ([]memory.Space, error) { return spaces, nil }
}

// unreachableLore is a SpaceLister that fails the way an uninstalled lore does.
func unreachableLore(err error) SpaceLister {
	if err == nil {
		// What an uninstalled lore actually produces.
		err = &exec.Error{Name: "lore", Err: exec.ErrNotFound}
	}
	return func(context.Context) ([]memory.Space, error) { return nil, err }
}

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
