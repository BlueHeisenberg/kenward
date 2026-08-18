package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// TestSpacesAreWrittenAsIDs is the whole point of this step. A display name is
// accepted by lore's Put and rejected by its Get, so a configuration naming one
// starts, accepts messages, saves memory, and finds nothing the first time somebody
// asks the assistant to remember something.
//
// It used to be the reason the wizard asked which space to use and printed ids beside
// names: the id was the operator's to copy, and copying the wrong column was the
// failure. The wizard makes the spaces now, so the id is never typed by anybody and
// never can be wrong — what is asserted here is that the ids lore minted are what
// reached the file, and that nothing derived from a person's name did.
func TestSpacesAreWrittenAsIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	_, cfg, io, err := runWizard(t, "linux", Options{ConfigPath: path}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}

	if want := fakeSpaceID("Casa — household"); cfg.Household.SharedSpace != want {
		t.Errorf("shared_space = %q, want the id lore minted, %q", cfg.Household.SharedSpace, want)
	}
	if want := fakeSpaceID("Casa — David"); cfg.Members[0].PrivateSpace != want {
		t.Errorf("David's private_space = %q, want %q", cfg.Members[0].PrivateSpace, want)
	}

	// No two people share one, which would be one person reading another's memory.
	seen := map[string]bool{cfg.Household.SharedSpace: true}
	for _, space := range spacesOf(cfg.Members) {
		if seen[space] {
			t.Errorf("space %q was configured twice", space)
		}
		seen[space] = true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, invented := range []string{"david-private", "maria-private", "shared_space: household"} {
		if strings.Contains(string(data), invented) {
			t.Errorf("the file contains the invented space name %q", invented)
		}
	}
}

func spacesOf(members []config.MemberConfig) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.PrivateSpace)
	}
	return out
}

// TestTheWizardMakesTheSpacesItself is the standalone claim at the wizard's own seam.
//
// Nobody installs lore, nobody runs `lore space create`, nobody reads an id column.
// An installer answers the questions and the household exists — which is what the
// listing-and-choosing flow this replaced could never do, because every space it
// offered had to have been made by hand first.
func TestTheWizardMakesTheSpacesItself(t *testing.T) {
	var made []string
	maker := func(_ context.Context, name string) (memory.Space, error) {
		made = append(made, name)
		return memory.Space{ID: fakeSpaceID(name), Name: name, Kind: "shared"}, nil
	}
	// Nothing is listed either: a lore home with no spaces in it is the ordinary
	// case now, and a wizard that consulted one would be back to needing it seeded.
	lister := func(context.Context) ([]memory.Space, error) {
		t.Error("the wizard listed lore's spaces; it has no reason to on a first run")
		return nil, nil
	}
	_, cfg, io, err := runWizard(t, "linux",
		Options{Spaces: lister, CreateSpace: maker}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}

	want := []string{"Casa — household", "Casa — David", "Casa — María"}
	if len(made) != len(want) {
		t.Fatalf("made %v, want one space for the household and one per person: %v", made, want)
	}
	for i, name := range want {
		if made[i] != name {
			t.Errorf("space %d was named %q, want %q", i, made[i], name)
		}
	}
	if cfg.Household.SharedSpace != fakeSpaceID(want[0]) {
		t.Errorf("shared_space = %q, want the id of the space just made", cfg.Household.SharedSpace)
	}

	// And the operator is told, rather than having spaces appear in their lore
	// store with no explanation.
	if !strings.Contains(io.Transcript(), "setup makes them for you") {
		t.Errorf("nothing said the spaces were being created:\n%s", io.Transcript())
	}
}

// TestSetupRunAgainReusesTheSpacesItAlreadyMade.
//
// `kenward setup --force` over a household that already exists is somebody fixing an
// endpoint address, not somebody asking for a second memory. lore refuses a duplicate
// name, and the wrong answer to that refusal is to fail the install; the right one is
// to keep using the space of that name, which is the one the household has been
// talking into.
func TestSetupRunAgainReusesTheSpacesItAlreadyMade(t *testing.T) {
	existing := []memory.Space{
		{ID: "e1000000-0000-4000-8000-000000000001", Name: "Casa — household", Kind: "shared"},
		{ID: "e1000000-0000-4000-8000-000000000002", Name: "Casa — David", Kind: "shared"},
		{ID: "e1000000-0000-4000-8000-000000000003", Name: "Casa — María", Kind: "shared"},
	}
	maker := func(_ context.Context, name string) (memory.Space, error) {
		return memory.Space{}, fmt.Errorf("memory: creating lore space %q: %w", name, memory.ErrSpaceExists)
	}
	_, cfg, io, err := runWizard(t, "linux",
		Options{Spaces: fixedSpaces(existing), CreateSpace: maker}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Household.SharedSpace != existing[0].ID {
		t.Errorf("shared_space = %q, want the household's existing space %q; a second one splits their memory",
			cfg.Household.SharedSpace, existing[0].ID)
	}
	if cfg.Members[0].PrivateSpace != existing[1].ID {
		t.Errorf("David's private_space = %q, want %q", cfg.Members[0].PrivateSpace, existing[1].ID)
	}
}

// TestAFailedSpaceCreationStopsRatherThanWritingAHouseholdWithNoMemory.
func TestAFailedSpaceCreationStopsRatherThanWritingAHouseholdWithNoMemory(t *testing.T) {
	maker := func(context.Context, string) (memory.Space, error) {
		return memory.Space{}, errors.New("database disk image is malformed")
	}
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	_, _, io, err := runWizard(t, "linux",
		Options{ConfigPath: path, CreateSpace: maker}, "1", "Casa")
	if err == nil {
		t.Fatalf("a household with no memory was written\n%s", io.Transcript())
	}
	if !strings.Contains(err.Error(), "disk image is malformed") {
		t.Errorf("the underlying failure was not reported: %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a configuration was written with no space to put memory in")
	}
}

func TestLoreNotInitialisedRecognisesAnUnusableHome(t *testing.T) {
	if !loreNotInitialised(fmt.Errorf("memory: opening the lore store: %w", memory.ErrStoreUnavailable)) {
		t.Error("a home with no account in it was not recognised")
	}
	if loreNotInitialised(errors.New("disk on fire")) {
		t.Error("an unrelated failure was reported as an uninitialised home")
	}
}

// TestScriptedInstallIsCheckedAgainstLore: a script may still name spaces it made
// itself — that is how the dashboard's wizard drives this package — so the ids it
// supplies are checked.
func TestScriptedInstallIsCheckedAgainstLore(t *testing.T) {
	base := func() *Answers {
		return &Answers{
			SharedSpace:  householdSpaceID,
			MemberNames:  []string{"David"},
			MemberSpaces: map[string]string{"david": davidSpaceID},
			Endpoints:    []EndpointAnswer{{Name: "m", BaseURL: "http://m.local:8000/v1", Model: "q"}},
		}
	}
	run := func(t *testing.T, a *Answers, lister SpaceLister) error {
		t.Helper()
		w := New(NewScriptIO(), Options{
			ConfigPath: filepath.Join(t.TempDir(), DefaultConfigFileName),
			GOOS:       "linux",
			Probe:      fixedProbe(Answered),
			Spaces:     lister,
			LookupEnv:  noEnv,
			Answers:    a,
		})
		_, err := w.Run(context.Background())
		return err
	}

	t.Run("good ids pass", func(t *testing.T) {
		if err := run(t, base(), fixedSpaces(testSpaces)); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	t.Run("a display name is refused", func(t *testing.T) {
		a := base()
		a.SharedSpace = "kenward-test-household"
		err := run(t, a, fixedSpaces(testSpaces))
		if err == nil || !strings.Contains(err.Error(), "named by id here") {
			t.Fatalf("err = %v, want a refusal naming the id rule", err)
		}
	})

	t.Run("a personal space is refused", func(t *testing.T) {
		a := base()
		a.MemberSpaces["david"] = personalSpaceID
		err := run(t, a, fixedSpaces(testSpaces))
		if err == nil || !strings.Contains(err.Error(), "cross accounts") {
			t.Fatalf("err = %v, want a refusal naming the kind", err)
		}
	})

	t.Run("unreachable lore does not block a script", func(t *testing.T) {
		// Nothing to check against, and a script's ids are its author's
		// responsibility. Refusing here would make an install impossible on a
		// machine whose store cannot be listed for some unrelated reason.
		if err := run(t, base(), unreachableLore(nil)); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
}

// TestScriptedInstallWithNoSpacesMakesThem is `kenward setup --non-interactive` as an
// installer would actually run it: names and endpoints, no lore, no ids.
func TestScriptedInstallWithNoSpacesMakesThem(t *testing.T) {
	var made []string
	w := New(NewScriptIO(), Options{
		ConfigPath: filepath.Join(t.TempDir(), DefaultConfigFileName),
		GOOS:       "linux",
		Probe:      fixedProbe(Answered),
		LookupEnv:  noEnv,
		CreateSpace: func(_ context.Context, name string) (memory.Space, error) {
			made = append(made, name)
			return memory.Space{ID: fakeSpaceID(name), Name: name, Kind: "shared"}, nil
		},
		Answers: &Answers{
			HouseholdName: "Casa",
			MemberNames:   []string{"David", "María"},
			Endpoints:     []EndpointAnswer{{Name: "m", BaseURL: "http://m.local:8000/v1", Model: "q"}},
		},
	})
	cfg, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"Casa — household", "Casa — David", "Casa — María"}
	if len(made) != 3 {
		t.Fatalf("made %v, want %v", made, want)
	}
	if cfg.Household.SharedSpace != fakeSpaceID(want[0]) {
		t.Errorf("shared_space = %q", cfg.Household.SharedSpace)
	}
	for i, m := range cfg.Members {
		if m.PrivateSpace != fakeSpaceID(want[i+1]) {
			t.Errorf("%s's private_space = %q, want %q", m.Name, m.PrivateSpace, fakeSpaceID(want[i+1]))
		}
	}
}
