package setup

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
func TestSpacesAreWrittenAsIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	_, cfg, io, err := runWizard(t, "linux", Options{ConfigPath: path}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}

	if cfg.Household.SharedSpace != householdSpaceID {
		t.Errorf("shared_space = %q, want the id", cfg.Household.SharedSpace)
	}
	if cfg.Members[0].PrivateSpace != davidSpaceID {
		t.Errorf("David's private_space = %q, want the id", cfg.Members[0].PrivateSpace)
	}

	// Every space in the file has to be an id lore actually holds. Nothing derived
	// from a member's name may survive anywhere in it.
	known := map[string]bool{}
	for _, s := range testSpaces {
		known[s.ID] = true
	}
	for _, space := range append([]string{cfg.Household.SharedSpace}, spacesOf(cfg.Members)...) {
		if !known[space] {
			t.Errorf("configured space %q is not a space lore holds", space)
		}
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

// TestPersonalSpaceIsNeverOffered: a lore personal space belongs to one account and
// can never cross accounts, so kenward can be neither the second member of it nor
// the group in it. Offering one would produce a configuration that cannot work.
func TestPersonalSpaceIsNeverOffered(t *testing.T) {
	_, cfg, io, err := runWizard(t, "linux", Options{}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Household.SharedSpace == personalSpaceID {
		t.Fatal("a personal space was configured as the household's shared memory")
	}
	transcript := io.Transcript()
	if strings.Contains(transcript, personalSpaceID) {
		t.Error("a personal space appeared in the list of choices")
	}
	// And the operator is told why the space they can see in lore is missing here,
	// rather than being left to wonder.
	if !strings.Contains(transcript, "personal space belongs to one") {
		t.Error("nothing explained why the personal space is not listed")
	}
}

// TestNoUsableSpaceStopsRatherThanInventingOne covers a lore home with nothing
// kenward can use, which is what a household that has installed lore and not yet
// made a space looks like.
func TestNoUsableSpaceStopsRatherThanInventingOne(t *testing.T) {
	only := []memory.Space{{ID: personalSpaceID, Name: "personal", Kind: "personal", Entries: 480}}
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	_, _, io, err := runWizard(t, "linux",
		Options{ConfigPath: path, Spaces: fixedSpaces(only)},
		"1", "Casa")
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a configuration was written with no space to put memory in")
	}
	transcript := io.Transcript()
	if !strings.Contains(transcript, "Nothing has been written") {
		t.Error("the operator was not told that nothing was written")
	}
	if !strings.Contains(transcript, "run setup again") {
		t.Error("the operator was not told they can run setup again")
	}
	// It must not guess at lore's own verb for making a space.
	if strings.Contains(transcript, "lore space create") || strings.Contains(transcript, "lore new") {
		t.Error("the wizard invented a lore command")
	}
}

// TestRunningOutOfSpacesMidwayStops covers three people and two spaces.
func TestRunningOutOfSpacesMidwayStops(t *testing.T) {
	two := []memory.Space{
		{ID: householdSpaceID, Name: "household", Kind: "shared"},
		{ID: davidSpaceID, Name: "david", Kind: "shared"},
	}
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	_, _, io, err := runWizard(t, "linux",
		Options{ConfigPath: path, Spaces: fixedSpaces(two)},
		"1", "Casa", "1", realToken, "n", "David", "María", "", "1")
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped\n%s", err, io.Transcript())
	}
	if !strings.Contains(io.Transcript(), "María's private memory") {
		t.Error("the message does not say which person has no space")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a configuration was written for a household with a member who has no memory")
	}
}

// TestOperatorCanSayNoneOfThese: the last option exists so that somebody who can see
// their spaces and knows none of them is right is not forced to pick a wrong one.
func TestOperatorCanSayNoneOfThese(t *testing.T) {
	// Five shared spaces in the listing, so the escape is the sixth option.
	_, _, io, err := runWizard(t, "linux", Options{}, "1", "Casa", "6")
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped\n%s", err, io.Transcript())
	}
	if !strings.Contains(io.Transcript(), "None of these") {
		t.Error("the escape option was not offered")
	}
}

// TestLoreUnreachableFallsBackToAskingForTheID: usable without pretending. The one
// thing it will not do is quietly accept a display name, because that is the
// configuration this whole step exists to stop writing.
func TestLoreUnreachableFallsBackToAskingForTheID(t *testing.T) {
	answers := []string{
		"1", "Casa",
		householdSpaceID, // typed, because nothing could be listed
		realToken, "n",
		"David", "",
		davidSpaceID,
		"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
		"n",
	}
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	w, cfg, io, err := runWizard(t, "linux",
		Options{ConfigPath: path, Spaces: unreachableLore(nil)}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Household.SharedSpace != householdSpaceID || cfg.Members[0].PrivateSpace != davidSpaceID {
		t.Errorf("the typed ids were not used: %q, %q", cfg.Household.SharedSpace, cfg.Members[0].PrivateSpace)
	}
	assertLoadable(t, path, w)

	transcript := io.Transcript()
	for _, want := range []string{
		"lore is not installed, or not on this PATH.",
		"lore spaces",
		"A display name\n  will not work here",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the fallback does not say %q:\n%s", want, transcript)
		}
	}
	// Said once, not before every question.
	if n := strings.Count(transcript, "Tried: lore mcp"); n != 1 {
		t.Errorf("the lore warning was printed %d times, want 1", n)
	}
}

// TestLoreFailedForSomeOtherReasonSaysSo distinguishes "not installed" from "ran and
// broke", because the two have entirely different first moves.
func TestLoreFailedForSomeOtherReasonSaysSo(t *testing.T) {
	broken := unreachableLore(errors.New("mcp handshake: unexpected EOF"))
	_, _, io, err := runWizard(t, "linux", Options{Spaces: broken}, "1", "Casa", householdSpaceID)
	if !errors.Is(err, ErrInputClosed) {
		t.Fatalf("err = %v", err)
	}
	transcript := io.Transcript()
	if strings.Contains(transcript, "not installed") {
		t.Error("a lore that ran and failed was reported as missing")
	}
	if !strings.Contains(transcript, "could not be started") || !strings.Contains(transcript, "unexpected EOF") {
		t.Errorf("the underlying failure was not shown:\n%s", transcript)
	}
}

func TestLoreNotFoundRecognisesAnExecError(t *testing.T) {
	if !loreNotFound(&exec.Error{Name: "lore", Err: exec.ErrNotFound}) {
		t.Error("a missing executable was not recognised")
	}
	if loreNotFound(errors.New("handshake failed")) {
		t.Error("an unrelated failure was reported as a missing executable")
	}
}

// TestScriptedInstallIsCheckedAgainstLore: a script cannot see the numbered list, so
// the ids it supplies are checked instead.
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
		// responsibility. Refusing here would make setup impossible on a machine
		// that has not installed lore yet.
		if err := run(t, base(), unreachableLore(nil)); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
}

// TestSpacesAreListedOnce: the listing spawns a subprocess, and asking a household of
// four means four questions off one answer.
func TestSpacesAreListedOnce(t *testing.T) {
	calls := 0
	lister := func(ctx context.Context) ([]memory.Space, error) {
		calls++
		return testSpaces, nil
	}
	if _, _, io, err := runWizard(t, "linux", Options{Spaces: lister}, simpleAnswers()...); err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if calls != 1 {
		t.Errorf("lore was listed %d times, want 1", calls)
	}
}
