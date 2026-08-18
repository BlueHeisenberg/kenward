package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// LoreDeviceName is the lore device name kenward registers itself under when it
// creates a store for a household. It matches cmd/kenward's, so a machine set up by
// the wizard and then run does not appear twice in `lore devices`.
const LoreDeviceName = "kenward"

// SpaceLister lists the spaces a lore home holds. Nil in Options means asking the
// real lore; tests supply their own so that the wizard's behaviour given a listing
// can be exercised without one.
type SpaceLister func(ctx context.Context) ([]memory.Space, error)

// SpaceMaker creates one shared lore space and returns it. Nil in Options means
// asking the real lore.
//
// It is the seam that makes an install one command. The wizard used to offer the
// spaces a lore home already held and stop if there were none, which meant an
// installer had to fetch lore, run `lore init`, run `lore space create` once per
// person, run `lore spaces`, and copy an id column into answers — five external
// steps to reach a wizard that could only ever have written down what they produced.
// lore exports CreateSpace, so the wizard makes them itself, as the dashboard's
// first-run wizard already did.
type SpaceMaker func(ctx context.Context, name string) (memory.Space, error)

// defaultSpaceLister and defaultSpaceMaker are what a Wizard uses when Options names
// neither. They are variables rather than direct references so that this package's own
// tests cannot reach a real lore home: the maker *writes*, and a test suite that
// defaults to the real one creates spaces in whatever ~/.lore the machine running it
// has — which it did, once, in somebody's own knowledge store. See TestMain.
var (
	defaultSpaceLister SpaceLister = loreSpaces
	defaultSpaceMaker  SpaceMaker  = loreCreateSpace
)

// openLore opens this machine's lore store, creating the home first if the machine
// has never had one.
//
// Setup is the first thing anyone runs, so it is the first thing to meet a machine
// with no lore home, and until this it answered that by printing `lore init`.
// memory.InitHome leaves an existing store exactly as it was, so a person who already
// runs lore has kenward's spaces made in their own store rather than beside it.
func openLore(ctx context.Context) (*memory.Client, error) {
	if _, err := memory.InitHome(ctx, memory.DefaultLoreHome(), LoreDeviceName); err != nil {
		return nil, err
	}
	return memory.NewClient(memory.Config{})
}

// loreSpaces asks lore for its spaces, opening the lore home this machine's
// kenward will be served from — so if this works, what the wizard writes will work
// too.
func loreSpaces(ctx context.Context) ([]memory.Space, error) {
	client, err := openLore(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.Spaces(ctx)
}

// loreCreateSpace makes one shared space in that same home.
func loreCreateSpace(ctx context.Context, name string) (memory.Space, error) {
	client, err := openLore(ctx)
	if err != nil {
		return memory.Space{}, err
	}
	defer client.Close()
	return client.CreateSpace(ctx, name)
}

// spaces returns the listing, fetched once and remembered. A nil error with no
// spaces is a real answer: a lore home that holds nothing.
func (w *Wizard) spaces(ctx context.Context) ([]memory.Space, error) {
	if !w.spacesLoaded {
		w.spacesLoaded = true
		w.spaceList, w.spaceErr = w.lister(ctx)
	}
	return w.spaceList, w.spaceErr
}

// makeSpace creates the space named label and returns its id.
//
// Every space kenward uses is a *shared* space, including the private ones, and
// memory.CreateSpace makes no other kind. A member's private space is a shared-kind
// lore space with two members in it, the person and the node; lore's personal kind
// never crosses accounts, so kenward could not read one. That is why the wizard can
// create these at all and why it never offers to reuse a personal one.
//
// # Running setup twice
//
// A name another space already holds is memory.ErrSpaceExists and nothing is created.
// That is `kenward setup --force` over a household that was already set up, and the
// answer is to find the space of that name and keep using it: making a second one
// would split the household's memory in half at the exact moment somebody was trying
// to correct a typo in their endpoint list.
func (w *Wizard) makeSpace(ctx context.Context, label, use string) (string, error) {
	label = strings.TrimSpace(label)
	sp, err := w.maker(ctx, label)
	if err == nil {
		w.spacesLoaded = false // the listing this wizard cached is now one short
		w.takenSpaces[sp.ID] = true
		return sp.ID, nil
	}
	if errors.Is(err, memory.ErrSpaceExists) {
		if id := w.spaceNamed(ctx, label); id != "" {
			w.takenSpaces[id] = true
			return id, nil
		}
	}
	return "", fmt.Errorf("setup: creating %s in lore: %w", use, err)
}

// spaceNamed is the id of the shared space with this display name, or "".
func (w *Wizard) spaceNamed(ctx context.Context, name string) string {
	all, err := w.spaces(ctx)
	if err != nil {
		return ""
	}
	for _, s := range all {
		if s.Name == name && s.Kind == "shared" {
			return s.ID
		}
	}
	return ""
}

// checkSpaceID verifies a space id supplied by a scripted install, when lore can be
// reached. A script is not a person and can be told the truth bluntly.
//
// A scripted install may still name spaces it made itself — that is how the
// dashboard's wizard drives this package — so supplying an id is supported and
// checked. Supplying none is the ordinary case and means "make them".
func (w *Wizard) checkSpaceID(ctx context.Context, id, use string) error {
	all, err := w.spaces(ctx)
	if err != nil {
		// Nothing to check against. The id form is the caller's responsibility.
		return nil
	}
	for _, s := range all {
		if s.ID != id {
			continue
		}
		if s.Kind != "shared" {
			return fmt.Errorf("setup: %s: lore space %s is %s, and a %s space cannot hold %s — it can never cross accounts",
				id, s.Name, s.Kind, s.Kind, use)
		}
		return nil
	}
	return fmt.Errorf("setup: %s: lore holds no space with that id (spaces are named by id here, not by display name; `lore spaces` lists them)", id)
}

// loreNotInitialised reports a lore home with no account in it.
//
// Nothing in setup reaches this by accident any more: openLore initialises a home
// before it opens one, so the remaining way to see it is a home kenward could not
// write to. It is kept because the message that distinguishes it is still the useful
// one when that happens.
func loreNotInitialised(err error) bool {
	return errors.Is(err, memory.ErrStoreUnavailable)
}
