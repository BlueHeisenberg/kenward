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

	// And the space the wizard actually makes is a shared one, because a personal
	// space belongs to one account and can hold neither the household's memory nor
	// a member's.
	made, err := loreCreateSpace(t.Context(), "Test household — household")
	if err != nil {
		t.Fatalf("creating a space: %v", err)
	}
	if made.Kind != "shared" {
		t.Errorf("the wizard made a %q space; only a shared space can hold memory kenward reads", made.Kind)
	}
	if made.ID == "" || made.ID == made.Name {
		t.Errorf("the space came back as %+v, and it is the id the configuration is written with", made)
	}
}

// TestOpenLoreInitialisesAnEmptyHome is the standalone claim, at its narrowest: setup
// against a machine that has never had lore does not fail, it makes the store.
func TestOpenLoreInitialisesAnEmptyHome(t *testing.T) {
	t.Setenv("LORE_HOME", t.TempDir())
	spaces, err := loreSpaces(t.Context())
	if err != nil {
		t.Fatalf("listing spaces on a machine with no lore home: %v; setup must need nothing installed", err)
	}
	if len(spaces) != 1 {
		t.Fatalf("a home setup had to create listed %+v, want the one personal space lore.Init makes", spaces)
	}
}
