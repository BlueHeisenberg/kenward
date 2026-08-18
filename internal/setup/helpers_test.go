package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// TestMain keeps this package's tests off any real lore home.
//
// The wizard creates spaces now, and a default that reached the real lore would have
// every run of `go test ./internal/setup` write spaces into whatever ~/.lore the
// machine has. It did exactly that once, into somebody's own store, before this
// existed. Tests that want to exercise a real lore point LORE_HOME at a temp
// directory and call loreSpaces or loreCreateSpace directly; see spaces_lore_test.go.
func TestMain(m *testing.M) {
	defaultSpaceLister = fixedSpaces(testSpaces)
	defaultSpaceMaker = fakeSpaceMaker()
	os.Exit(m.Run())
}

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
		// What a machine with lore installed and never initialised produces.
		err = fmt.Errorf("memory: opening the lore store at /home/someone/.lore: %w: lore: home is not initialised",
			memory.ErrStoreUnavailable)
	}
	return func(context.Context) ([]memory.Space, error) { return nil, err }
}

// fakeSpaceMaker mints a space with an id derived from its name, so that a test can
// assert which space a member was given without the ids being a lottery.
func fakeSpaceMaker() SpaceMaker {
	return func(_ context.Context, name string) (memory.Space, error) {
		return memory.Space{ID: fakeSpaceID(name), Name: name, Kind: "shared"}, nil
	}
}

// fakeSpaceID is what fakeSpaceMaker would mint for a name, so a test can say what it
// expects without copying a hash.
func fakeSpaceID(name string) string {
	sum := sha256.Sum256([]byte(name))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-4%s-8%s-%s", h[0:8], h[8:12], h[13:16], h[17:20], h[20:32])
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
