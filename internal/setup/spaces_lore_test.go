//go:build integration

package setup

import (
	"context"
	"testing"
	"time"
)

// TestLoreSpacesAgainstARealLore is the one check here that spawns lore.
//
// Everything else in this package proves the wizard behaves correctly given a
// listing. This proves the listing is real: that the argv the wizard writes into
// kenward.yaml is the argv that works, and that what comes back has the shape the
// choice is built from. It is tagged integration and excluded from the default run,
// per CLAUDE.md — a unit test may not spawn a subprocess.
//
//	go test -tags integration ./internal/setup -run TestLoreSpacesAgainstARealLore -v
func TestLoreSpacesAgainstARealLore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spaces, err := loreSpaces(ctx)
	if err != nil {
		t.Fatalf("listing spaces with %v: %v", DefaultLoreCommand, err)
	}
	if len(spaces) == 0 {
		t.Skip("this lore home holds no spaces")
	}

	for _, s := range spaces {
		t.Logf("%-28s %-9s %s (%d entries)", s.Name, s.Kind, s.ID, s.Entries)
		if s.ID == "" {
			t.Errorf("space %q came back with no id, and the id is what kenward is configured with", s.Name)
		}
		if s.ID == s.Name {
			t.Errorf("space %q has its name as its id, so this test cannot tell the two apart", s.Name)
		}
		switch s.Kind {
		case "shared", "personal":
		default:
			t.Errorf("space %q has kind %q, which the wizard's usable/not-usable split does not know about", s.Name, s.Kind)
		}
	}

	if len(usableSpaces(spaces, nil)) == len(spaces) {
		t.Log("no personal space in this home, so the wrong-kind path is not exercised here")
	}
}
