package setup

import (
	"testing"

	"github.com/BlueHeisenberg/lore"
)

// TestLoreSpacesAgainstARealLore is the one check here that talks to a real store.
//
// Everything else in this package proves the wizard behaves correctly given a
// listing. This proves the listing is real: that what loreSpaces returns has the
// shape the choice is built from, against lore rather than against a fixture.
//
// It used to be tagged integration, because it spawned `lore mcp` and a unit test
// may not spawn a subprocess. It spawns nothing now, so it runs by default — and
// it runs against a home of its own rather than whatever ~/.lore the machine has,
// which matters more than it used to: opening a store migrates its schema, and a
// test has no business doing that to somebody's own knowledge store.
func TestLoreSpacesAgainstARealLore(t *testing.T) {
	home := t.TempDir()
	if _, err := lore.Init(home, "kenward-setup-test"); err != nil {
		t.Fatalf("lore.Init: %v", err)
	}
	// loreSpaces resolves the home the same way a lore client does — $LORE_HOME,
	// then ~/.lore — so this is what points it at the temp one. t.Setenv also
	// fails the test if it is run in parallel, which is the guard that keeps a
	// later edit from letting two tests race over one home.
	t.Setenv("LORE_HOME", home)

	spaces, err := loreSpaces(t.Context())
	if err != nil {
		t.Fatalf("listing spaces: %v", err)
	}
	// A freshly initialised home holds exactly the personal space.
	if len(spaces) != 1 || spaces[0].Kind != "personal" {
		t.Fatalf("a new lore home listed %+v, want one personal space", spaces)
	}

	for _, s := range spaces {
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

	// The personal space is the wrong-kind path: it belongs to one account and can
	// hold neither the household's memory nor a member's, so the wizard must not
	// offer it.
	if got := usableSpaces(spaces, nil); len(got) != 0 {
		t.Errorf("the wizard would offer %+v; a personal space cannot serve as either kind of memory", got)
	}
}
