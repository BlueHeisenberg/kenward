package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// spacesListing renders lore_spaces' own output format, so the listings these tests
// script go through the real parser rather than around it. A test that injected
// []spaceRow directly would keep passing on the day lore changed its wording, which is
// the failure this whole package's parse.go exists to catch.
func spacesListing(rows ...string) string { return strings.Join(rows, "\n") + "\n" }

func spaceRowText(name, id string) string {
	return fmt.Sprintf("%s  kind:shared  members:1  entries:0  id:%s", name, id)
}

const (
	existingSpaceID = "1111aaaa-0000-4000-8000-000000000000"
	createdSpaceID  = "2222bbbb-0000-4000-8000-000000000000"
	secondSpaceID   = "3333cccc-0000-4000-8000-000000000000"
)

// cliClient wires the scripted lore server up with a scripted command line, and reports
// the argv CreateSpace ran.
func cliClient(t *testing.T, listings []string, run func(args []string) ([]byte, error)) *fake {
	t.Helper()
	replies := make([]fakeReply, 0, len(listings))
	for _, text := range listings {
		replies = append(replies, fakeReply{Text: text})
	}
	return newFake(t, fakeScript{Replies: map[string][]fakeReply{toolSpaces: replies}}, func(c *Config) {
		c.RunCLI = func(_ context.Context, args []string, _ []string, _ string) ([]byte, error) {
			return run(args)
		}
	})
}

// TestCreateSpaceRejectsNamesBeforeRunningAnything.
//
// The name comes from a web form, so this is a trust boundary. There is no shell — the
// subprocess is exec'd with an argv — but a leading dash is still read by lore's own flag
// parser as a flag, and a newline in a name is a name nothing downstream can reason
// about. Nothing may run before the check: a refusal that happens after the subprocess
// is a refusal that has already had its effect.
func TestCreateSpaceRejectsNamesBeforeRunningAnything(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "   ", "-rf", "--help", "a\nb", "a\x00b"} {
		var ran bool
		c := cliClient(t, nil, func([]string) ([]byte, error) {
			ran = true
			return nil, nil
		})
		_, err := c.CreateSpace(t.Context(), name)
		if err == nil {
			t.Errorf("CreateSpace(%q) was accepted", name)
		} else if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("CreateSpace(%q): err = %v, want ErrInvalidArgument", name, err)
		}
		if ran {
			t.Errorf("CreateSpace(%q) ran lore before checking the name", name)
		}
	}
}

// TestCreateSpaceRunsLoresOwnCommand and returns the space that appeared.
//
// The id comes from diffing the listing rather than from parsing the command's output,
// so what is asserted is the argv and the resulting Space. Parsing that output would put
// a sixth hostage to lore's wording in this package for no gain.
func TestCreateSpaceRunsLoresOwnCommand(t *testing.T) {
	t.Parallel()
	var argv []string
	c := cliClient(t, []string{
		spacesListing(spaceRowText("Existing", existingSpaceID)),
		spacesListing(
			spaceRowText("Existing", existingSpaceID),
			spaceRowText("Casa-David", createdSpaceID),
		),
	}, func(args []string) ([]byte, error) {
		argv = args
		return []byte("created space"), nil
	})

	got, err := c.CreateSpace(t.Context(), "Casa-David")
	if err != nil {
		t.Fatal(err)
	}
	if want := "space create Casa-David"; strings.Join(argv, " ") != want {
		t.Errorf("argv = %q, want %q", argv, want)
	}
	if got.ID != createdSpaceID {
		t.Errorf("id = %q, want the one that was not there before (%q)", got.ID, createdSpaceID)
	}
	if got.Name != "Casa-David" || got.Kind != "shared" {
		t.Errorf("space = %+v", got)
	}
}

// TestCreateSpaceReportsALoreThatSaidYesAndDidNothing.
//
// Guessing here is the one thing that must not happen: this id becomes a member's
// private space, and picking the wrong one publishes their memory to whoever else holds
// it.
func TestCreateSpaceReportsALoreThatSaidYesAndDidNothing(t *testing.T) {
	t.Parallel()
	same := spacesListing(spaceRowText("Existing", existingSpaceID))
	c := cliClient(t, []string{same, same}, func([]string) ([]byte, error) { return nil, nil })

	if _, err := c.CreateSpace(t.Context(), "Casa"); !errors.Is(err, ErrSpaceNotCreated) {
		t.Fatalf("err = %v, want ErrSpaceNotCreated", err)
	}
}

// TestCreateSpaceRefusesToGuessBetweenTwoNewSpaces.
func TestCreateSpaceRefusesToGuessBetweenTwoNewSpaces(t *testing.T) {
	t.Parallel()
	c := cliClient(t, []string{
		spacesListing(spaceRowText("Existing", existingSpaceID)),
		spacesListing(
			spaceRowText("Existing", existingSpaceID),
			spaceRowText("Casa", createdSpaceID),
			spaceRowText("Casa", secondSpaceID),
		),
	}, func([]string) ([]byte, error) { return nil, nil })

	_, err := c.CreateSpace(t.Context(), "Casa")
	if !errors.Is(err, ErrSpaceNotCreated) {
		t.Fatalf("err = %v, want ErrSpaceNotCreated", err)
	}
	if !strings.Contains(err.Error(), "2 new spaces") {
		t.Errorf("the error does not say what happened: %v", err)
	}
}

// TestCreateSpaceSurfacesLoresOwnComplaint, output and all: a household whose lore is not
// initialised gets the sentence lore printed, not a wrapped nothing.
func TestCreateSpaceSurfacesLoresOwnComplaint(t *testing.T) {
	t.Parallel()
	c := cliClient(t, []string{spacesListing(spaceRowText("Existing", existingSpaceID))},
		func([]string) ([]byte, error) {
			return []byte("lore: no account at /home/nonroot/.lore/account.json (run `lore init`)"),
				errors.New("exit status 1")
		})

	_, err := c.CreateSpace(t.Context(), "Casa")
	if err == nil {
		t.Fatal("a failing lore was reported as success")
	}
	if !strings.Contains(err.Error(), "run `lore init`") {
		t.Fatalf("lore's own message was dropped: %v", err)
	}
}
