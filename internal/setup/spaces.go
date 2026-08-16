package setup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// SpaceLister lists the spaces a lore home holds. Nil in Options means asking the
// real lore; tests supply their own, because no test here may spawn a subprocess.
type SpaceLister func(ctx context.Context) ([]memory.Space, error)

// loreSpaces asks lore for its spaces, using the same argv the wizard writes into
// the configuration — so if this works, what it writes will work too.
func loreSpaces(ctx context.Context) ([]memory.Space, error) {
	argv := DefaultLoreCommand
	client, err := memory.NewClient(memory.Config{Command: argv[0], Args: argv[1:]})
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.Spaces(ctx)
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

// usableSpaces are the spaces kenward can be configured with: the shared ones.
//
// A lore personal space belongs to one account and can never cross accounts, so it
// can hold neither the household's shared memory nor a member's private memory —
// the latter has two members, the person and the node, which is exactly what lets
// kenward answer at all. Offering one would produce a configuration that validates,
// starts, and then cannot work.
func usableSpaces(all []memory.Space, taken map[string]bool) []memory.Space {
	var out []memory.Space
	for _, s := range all {
		if s.Kind == "shared" && !taken[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// askSpace offers the spaces this lore home holds and returns the id of the one
// chosen. It never returns a display name: lore does not enforce unique names, and
// a name configured here fails on the first retrieval rather than at startup.
func (w *Wizard) askSpace(ctx context.Context, question, use string) (string, error) {
	all, err := w.spaces(ctx)
	if err != nil {
		return w.askSpaceID(question, err)
	}

	usable := usableSpaces(all, w.takenSpaces)
	if len(usable) == 0 {
		w.blank()
		w.io.Print(noSpaceFor(use, all))
		return "", ErrStopped
	}

	options := make([]string, 0, len(usable)+1)
	for _, s := range usable {
		options = append(options, fmt.Sprintf("%s   %s   %d entries", s.Name, shortID(s.ID), s.Entries))
	}
	options = append(options, "None of these — I need to create one in lore first.")

	w.blank()
	if skipped := len(all) - len(usableSpaces(all, nil)); skipped > 0 {
		w.io.Print(personalSpacesSkipped)
		w.blank()
	}
	choice, err := w.io.AskChoice(question, options, -1)
	if err != nil {
		return "", err
	}
	if choice == len(usable) {
		w.blank()
		w.io.Print(noSpaceFor(use, all))
		return "", ErrStopped
	}

	chosen := usable[choice]
	w.takenSpaces[chosen.ID] = true
	return chosen.ID, nil
}

// askSpaceID is the fallback when lore cannot be reached: ask for the id, and say
// plainly that a display name will not do.
//
// It is deliberately not a silent degradation to names. Setup that cannot check is
// still setup that must not write something known to fail, so the one thing it
// insists on is the form.
func (w *Wizard) askSpaceID(question string, cause error) (string, error) {
	if !w.loreWarned {
		w.loreWarned = true
		w.blank()
		w.io.Print(loreUnreachable(cause))
	}
	for {
		w.blank()
		answer, err := w.io.Ask(question+" (its id)", "")
		if err != nil {
			return "", err
		}
		id := strings.TrimSpace(answer)
		if id == "" {
			w.io.Print("  The id from the id column of `lore spaces`.")
			continue
		}
		if w.takenSpaces[id] {
			w.io.Print("  That space is already spoken for; two people cannot share a private memory.")
			continue
		}
		w.takenSpaces[id] = true
		return id, nil
	}
}

// checkSpaceID verifies a space id supplied by a scripted install, when lore can be
// reached. A script is not a person and can be told the truth bluntly.
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

// shortID is the leading part of a space id, enough to tell two spaces with the
// same display name apart without filling the line with a UUID.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// loreNotFound reports whether lore could not be started because there is no such
// program, which is a different problem from lore failing once it ran.
func loreNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, exec.ErrDot)
}
