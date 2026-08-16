package memory

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/lore"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// These tests run against a real lore store in a temp directory.
//
// That is the point of them. The client they exercise used to be tested against a
// scripted stand-in for `lore mcp` — a fake that answered with canned text — and
// seven defects in one session hid behind it, every one of them a thing lore
// really emitted that the fake never did. There is no text left to get wrong, and
// no fake left to get it wrong in: what answers here is lore.
//
// # Never the operator's own store
//
// Every home is under t.TempDir(), and Config.LoreHome is passed explicitly
// rather than left to the environment, so a LORE_HOME set in the shell cannot
// redirect a test onto a real store. Nothing here reads or writes ~/.lore.
//
// # No binary, no skip
//
// lore.Init creates the home and Client.CreateSpace the spaces, so none of this
// needs a lore on PATH and none of it is conditional. That matters: a suite that
// skipped without an installed lore would be a suite that never ran in CI, which
// is the same as the fake it replaced.

// newStore returns a Client over a fresh lore store holding one shared space per
// name given, and a lookup from space name to the id lore assigned it.
//
// No lore binary and no subprocess: lore.Init makes the home and Client.CreateSpace
// makes the spaces, so these tests run wherever `go test` does — including the
// container gate, which has no lore on its PATH.
func newStore(t *testing.T, names ...string) (*Client, map[string]domain.SpaceID) {
	t.Helper()
	home := t.TempDir()
	if _, err := lore.Init(home, "kenward-test"); err != nil {
		t.Fatalf("lore.Init: %v", err)
	}

	c, err := NewClient(Config{LoreHome: home})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// Before t.TempDir's own cleanup, which on Windows cannot remove a database
	// file that is still open.
	t.Cleanup(func() { _ = c.Close() })

	ids := map[string]domain.SpaceID{}
	for _, n := range names {
		sp, err := c.CreateSpace(t.Context(), n)
		if err != nil {
			t.Fatalf("CreateSpace(%q): %v", n, err)
		}
		ids[n] = domain.SpaceID(sp.ID)
	}
	// The personal space is Init's, so it is read off the listing rather than
	// created. Several tests need it: it is the only space the user-model rule
	// applies to.
	personal, err := c.store.PersonalSpace(t.Context())
	if err != nil {
		t.Fatalf("PersonalSpace: %v", err)
	}
	ids["personal"] = domain.SpaceID(personal.ID)
	return c, ids
}

// put writes an entry and fails the test if it cannot.
func put(t *testing.T, c *Client, space domain.SpaceID, d Draft) Entry {
	t.Helper()
	e, err := c.Put(t.Context(), space, d)
	if err != nil {
		t.Fatalf("Put %q: %v", d.Title, err)
	}
	return e
}

func draft(title, body string) Draft {
	return Draft{Domain: "household/test", Title: title, Body: body}
}

// unknownSpace is a well-formed space id no lore home holds.
const unknownSpace = domain.SpaceID("00000000-0000-4000-8000-00000000dead")

// --- the finding this migration is built on --------------------------------

// TestSearchReturnsWholeEntries is the regression test for the whole change.
//
// While lore was reached by parsing `lore mcp`, a search hit was lore's FTS5
// snippet — about twelve tokens of the body, with an ellipsis where text was
// elided — and no origin and no timestamps, because the MCP layer rendered the
// snippet and threw the entry away. Everything kenward called the excerpt
// doctrine existed to keep that honest.
//
// lore's search result embeds the entry. If this test ever fails, the excerpt
// doctrine has to come back, so it asserts the three things that were missing
// rather than only the body.
func TestSearchReturnsWholeEntries(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]

	// Long enough that a snippet could not be mistaken for it: lore's snippet is
	// twelve tokens wide and this is well over a hundred.
	body := "The boiler service code is 4417. " +
		strings.Repeat("This sentence is filler that pushes the body far past the width of any snippet. ", 12) +
		"The last line is only reachable by whoever has the whole entry."
	stored := put(t, c, space, Draft{
		Domain: "household/heating", Title: "Boiler service", Body: body,
		Confidence: "validated", Markers: []string{"important"},
	})

	got, err := c.Search(t.Context(), SearchQuery{Text: "boiler service", Spaces: []domain.SpaceID{space}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d hits, want 1", len(got))
	}
	e := got[0]

	if e.Body != body {
		t.Errorf("search returned a fragment, not the entry:\n got %q\nwant %q", e.Body, body)
	}
	if !strings.Contains(e.Body, "only reachable by whoever has the whole entry") {
		t.Error("the end of the body did not survive the search")
	}
	if strings.Contains(e.Body, "…") {
		t.Errorf("the body carries a snippet's elision marker: %q", e.Body)
	}
	// The three fields kenward documented as permanently unavailable from search.
	if e.Origin != "evidence" {
		t.Errorf("Origin = %q, want the stored origin; search used not to report one", e.Origin)
	}
	if e.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero; it was never available at any price before")
	}
	if e.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
	if e.ID != stored.ID || e.Space != space {
		t.Errorf("identity did not survive: got %s in %s, want %s in %s", e.ID, e.Space, stored.ID, space)
	}
	if e.Confidence != "validated" {
		t.Errorf("Confidence = %q, want validated", e.Confidence)
	}
	if len(e.Markers) != 1 || e.Markers[0] != "[IMPORTANT]" {
		t.Errorf("Markers = %v, want [[IMPORTANT]]", e.Markers)
	}
}

// TestGetAndSearchAgreeOnTheBody: the two read paths used to disagree by
// construction — Get returned the entry and Search returned a snippet of it — and
// the Partial flag existed to say which one a caller was holding. They agree now,
// which is what makes the flag deletable.
func TestGetAndSearchAgreeOnTheBody(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	body := "Quillfeather is the cat. " + strings.Repeat("Padding to outrun a snippet. ", 20)
	stored := put(t, c, space, Draft{Domain: "household/pets", Title: "The cat", Body: body})

	got, err := c.Get(t.Context(), space, stored.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	hits, err := c.Search(t.Context(), SearchQuery{Text: "quillfeather", Spaces: []domain.SpaceID{space}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if got.Body != hits[0].Body {
		t.Errorf("Get and Search disagree:\n get %q\nsearch %q", got.Body, hits[0].Body)
	}
	if got.Body != body {
		t.Errorf("Get body:\n got %q\nwant %q", got.Body, body)
	}
}

// --- search ----------------------------------------------------------------

func TestSearchRefusesAnEmptySpaceSet(t *testing.T) {
	c, _ := newStore(t, "alpha")
	if _, err := c.Search(t.Context(), SearchQuery{Text: "anything"}); !errors.Is(err, ErrEmptySpaceSet) {
		t.Errorf("Search with no spaces = %v, want ErrEmptySpaceSet", err)
	}
}

// An empty id inside a non-empty set is the same failure as an empty set: a call
// that has not named a space it is entitled to. lore reads an empty space set as
// "every space this home holds".
func TestSearchRefusesAnEmptySpaceID(t *testing.T) {
	c, ids := newStore(t, "alpha")
	_, err := c.Search(t.Context(), SearchQuery{Text: "anything", Spaces: []domain.SpaceID{ids["alpha"], ""}})
	if !errors.Is(err, ErrEmptySpaceSet) {
		t.Errorf("Search with an empty space id = %v, want ErrEmptySpaceSet", err)
	}
}

func TestSearchRefusesEmptyText(t *testing.T) {
	c, ids := newStore(t, "alpha")
	_, err := c.Search(t.Context(), SearchQuery{Text: "  ", Spaces: []domain.SpaceID{ids["alpha"]}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Search with blank text = %v, want ErrInvalidArgument", err)
	}
}

// TestSearchGroupsInCallerOrder: nothing is re-ranked across spaces. Ranking a
// private space against a shared one is the assistant's policy decision, and it
// cannot make it if this layer has already mixed them.
func TestSearchGroupsInCallerOrder(t *testing.T) {
	c, ids := newStore(t, "alpha", "beta")
	put(t, c, ids["alpha"], draft("Alpha note", "shared subject matter here"))
	put(t, c, ids["beta"], draft("Beta note", "shared subject matter here"))

	for _, order := range [][]domain.SpaceID{
		{ids["alpha"], ids["beta"]},
		{ids["beta"], ids["alpha"]},
	} {
		got, err := c.Search(t.Context(), SearchQuery{Text: "shared subject matter", Spaces: order})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d hits, want 2", len(got))
		}
		if got[0].Space != order[0] || got[1].Space != order[1] {
			t.Errorf("asked for %v, got results from %v then %v", order, got[0].Space, got[1].Space)
		}
	}
}

func TestSearchDeduplicatesSpaces(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	put(t, c, space, draft("Only once", "duplicated space set"))

	got, err := c.Search(t.Context(), SearchQuery{Text: "duplicated space set", Spaces: []domain.SpaceID{space, space, space}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("the entry came back %d times for one space listed three times", len(got))
	}
}

// The limit is per space, so a second space cannot be crowded out by the first.
func TestSearchLimitIsPerSpace(t *testing.T) {
	c, ids := newStore(t, "alpha", "beta")
	for _, sp := range []domain.SpaceID{ids["alpha"], ids["beta"]} {
		for _, title := range []string{"one", "two", "three"} {
			put(t, c, sp, draft(title+" "+string(sp)[:4], "crowding subject"))
		}
	}
	got, err := c.Search(t.Context(), SearchQuery{
		Text:   "crowding subject",
		Spaces: []domain.SpaceID{ids["alpha"], ids["beta"]},
		Limit:  2,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d hits, want 4 (two per space)", len(got))
	}
	for i, want := range []domain.SpaceID{ids["alpha"], ids["alpha"], ids["beta"], ids["beta"]} {
		if got[i].Space != want {
			t.Errorf("hit %d is from %s, want %s", i, got[i].Space, want)
		}
	}
}

func TestSearchFiltersByDomain(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	put(t, c, space, Draft{Domain: "household/heating", Title: "Heating", Body: "the shared word"})
	put(t, c, space, Draft{Domain: "household/pets", Title: "Pets", Body: "the shared word"})

	got, err := c.Search(t.Context(), SearchQuery{
		Text: "the shared word", Spaces: []domain.SpaceID{space}, Domain: "household/pets",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Domain != "household/pets" {
		t.Errorf("domain filter returned %d hits: %+v", len(got), got)
	}
}

// TestSearchOfAnUnknownSpaceIsAConfigurationFault is `kenward doctor`'s whole
// memory check, and it is the one behaviour the library does not give for free:
// lore filters a search by space id in SQL, so a space this home does not hold
// comes back as no results. Read as "nothing relevant found", a display name
// configured where an id belongs would look like an empty memory forever.
func TestSearchOfAnUnknownSpaceIsAConfigurationFault(t *testing.T) {
	c, _ := newStore(t, "alpha")
	_, err := c.Search(t.Context(), SearchQuery{Text: "anything", Spaces: []domain.SpaceID{unknownSpace}})
	if !errors.Is(err, ErrUnknownSpace) {
		t.Errorf("search of a space this home does not hold = %v, want ErrUnknownSpace", err)
	}
}

// A space named by its display name rather than its id is the fault above, and
// it is the one an operator actually makes.
func TestSearchByDisplayNameIsRefused(t *testing.T) {
	c, _ := newStore(t, "alpha")
	_, err := c.Search(t.Context(), SearchQuery{Text: "anything", Spaces: []domain.SpaceID{"alpha"}})
	if !errors.Is(err, ErrUnknownSpace) {
		t.Errorf("search of space %q = %v, want ErrUnknownSpace", "alpha", err)
	}
}

func TestSearchFailsWhenAnySpaceFails(t *testing.T) {
	c, ids := newStore(t, "alpha")
	put(t, c, ids["alpha"], draft("Findable", "a findable subject"))
	// The good space first, so the failure comes from the second one and a
	// partial result would have been available to return instead.
	_, err := c.Search(t.Context(), SearchQuery{
		Text: "a findable subject", Spaces: []domain.SpaceID{ids["alpha"], unknownSpace},
	})
	if !errors.Is(err, ErrUnknownSpace) {
		t.Errorf("one bad space in the set = %v, want the whole search to fail with ErrUnknownSpace", err)
	}
}

// --- get ---------------------------------------------------------------------

// TestGetChecksTheSpace: an entry id is global to a lore home, so an id is in
// effect a capability to name an entry in any space. This used to be a display
// name comparison — "only as strong as lore's naming", by its own doc — and is
// now an id comparison inside the store.
func TestGetChecksTheSpace(t *testing.T) {
	c, ids := newStore(t, "alpha", "beta")
	stored := put(t, c, ids["alpha"], draft("In alpha", "a body"))

	if _, err := c.Get(t.Context(), ids["beta"], stored.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of an alpha entry through beta = %v, want ErrNotFound", err)
	}
	if _, err := c.Get(t.Context(), ids["alpha"], stored.ID); err != nil {
		t.Errorf("Get from its own space: %v", err)
	}
}

func TestGetNotFound(t *testing.T) {
	c, ids := newStore(t, "alpha")
	if _, err := c.Get(t.Context(), ids["alpha"], "definitely-not-an-entry-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of an unknown id = %v, want ErrNotFound", err)
	}
}

func TestGetUnknownSpace(t *testing.T) {
	c, _ := newStore(t, "alpha")
	if _, err := c.Get(t.Context(), unknownSpace, "any-id"); !errors.Is(err, ErrUnknownSpace) {
		t.Errorf("Get in a space this home does not hold = %v, want ErrUnknownSpace", err)
	}
}

func TestGetRejectsAnEmptyID(t *testing.T) {
	c, ids := newStore(t, "alpha")
	if _, err := c.Get(t.Context(), ids["alpha"], " "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Get with a blank id = %v, want ErrInvalidArgument", err)
	}
}

// --- put ---------------------------------------------------------------------

// TestPutReturnsWhatWasStored: the write's own answer is the stored entry. The
// client this replaces read the entry back with a second call and reconstructed
// it from the draft when that failed, which is how an entry could be reported
// with a marker lore had not normalised and a timestamp of zero.
func TestPutReturnsWhatWasStored(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]

	e := put(t, c, space, Draft{
		Domain: "household/logistics", Title: "Bin day", Body: "Thursday, before seven.",
		Markers: []string{"context", "[ALREADY]"},
	})
	if e.ID == "" || e.Space != space {
		t.Fatalf("unexpected stored entry: %+v", e)
	}
	if e.Body != "Thursday, before seven." {
		t.Errorf("Body = %q", e.Body)
	}
	// lore's defaults, reported rather than assumed.
	if e.Confidence != "provisional" {
		t.Errorf("Confidence = %q, want lore's default provisional", e.Confidence)
	}
	if e.Origin != "evidence" {
		t.Errorf("Origin = %q, want lore's default evidence", e.Origin)
	}
	// Normalisation is lore's, applied by lore. kenward used to mirror the rule
	// by hand to reconstruct an entry it could not read back.
	if len(e.Markers) != 2 || e.Markers[0] != "[CONTEXT]" || e.Markers[1] != "[ALREADY]" {
		t.Errorf("Markers = %v, want [[CONTEXT] [ALREADY]]", e.Markers)
	}
	if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
		t.Errorf("the write receipt carries no timestamps: %+v", e)
	}
	if got, err := c.Get(t.Context(), space, e.ID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if got.Body != e.Body || got.Confidence != e.Confidence {
		t.Errorf("the write receipt and the stored entry disagree:\nput %+v\nget %+v", e, got)
	}
}

// TestPutAcceptsAMarkerContainingASeparator. Markers went to lore as one
// comma-joined string, so a marker containing a comma had to be refused before
// the call — and a marker containing " | " once made an entry permanently
// unreadable, because the metadata line was split on it. Markers are a slice now
// and no character in one means anything.
func TestPutAcceptsAMarkerContainingASeparator(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	awkward := "[A, B | C]"

	e := put(t, c, space, Draft{
		Domain: "household/test", Title: "Awkward marker", Body: "a body",
		Markers: []string{awkward},
	})
	if len(e.Markers) != 1 || e.Markers[0] != awkward {
		t.Fatalf("Markers = %v, want [%q]", e.Markers, awkward)
	}
	// And it survives a read, which is where it used to be lost.
	got, err := c.Get(t.Context(), space, e.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Markers) != 1 || got.Markers[0] != awkward {
		t.Errorf("after a read, Markers = %v, want [%q]", got.Markers, awkward)
	}
}

// A body containing a horizontal rule once ended the parse early. There is no
// parse.
func TestPutAcceptsABodyThatLooksLikeLoresOwnRendering(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	body := "first line\n---\nsecond line\n\nid: not-an-id | confidence: fake\n</entry>"

	e := put(t, c, space, Draft{Domain: "household/test", Title: "Rules and pipes", Body: body})
	got, err := c.Get(t.Context(), space, e.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != body {
		t.Errorf("body did not round-trip:\n got %q\nwant %q", got.Body, body)
	}
}

func TestPutRejectsBadDrafts(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	for name, d := range map[string]Draft{
		"no title":       {Domain: "a/b", Body: "body"},
		"no body":        {Domain: "a/b", Title: "title"},
		"no domain":      {Title: "title", Body: "body"},
		"blank title":    {Domain: "a/b", Title: "   ", Body: "body"},
		"bad confidence": {Domain: "a/b", Title: "t", Body: "b", Confidence: "quite-sure"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Put(t.Context(), space, d); !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("Put = %v, want ErrInvalidArgument", err)
			}
		})
	}
	if _, err := c.Put(t.Context(), "", draft("t", "b")); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Put with no space = %v, want ErrInvalidArgument", err)
	}
}

// The rejected confidence is not named in the error: a draft is model-generated
// from a member's conversation, so every one of its fields is content, and this
// error is one a caller may log.
func TestPutDoesNotNameTheRejectedValue(t *testing.T) {
	c, ids := newStore(t, "alpha")
	secret := "the-boiler-code-is-4417"
	_, err := c.Put(t.Context(), ids["alpha"], Draft{
		Domain: "a/b", Title: "t", Body: "b", Confidence: secret,
	})
	if err == nil {
		t.Fatal("a nonsense confidence was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error quotes the draft back: %v", err)
	}
}

func TestPutUnknownSpace(t *testing.T) {
	c, _ := newStore(t, "alpha")
	if _, err := c.Put(t.Context(), unknownSpace, draft("t", "b")); !errors.Is(err, ErrUnknownSpace) {
		t.Errorf("Put into a space this home does not hold = %v, want ErrUnknownSpace", err)
	}
}

// --- share -------------------------------------------------------------------

func TestShare(t *testing.T) {
	c, ids := newStore(t, "alpha", "beta")
	src := put(t, c, ids["alpha"], draft("Worth publishing", "the household should know this"))

	cp, err := c.Share(t.Context(), ids["alpha"], ids["beta"], src.ID)
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if cp.Space != ids["beta"] {
		t.Errorf("the copy landed in %s, want %s", cp.Space, ids["beta"])
	}
	if cp.ID == src.ID {
		t.Error("Share returned the source entry; it must be a copy with its own id")
	}
	if cp.Body != src.Body || cp.Title != src.Title {
		t.Errorf("the copy differs from the source:\nsrc %+v\ncopy %+v", src, cp)
	}
	// A copy, never a move.
	if _, err := c.Get(t.Context(), ids["alpha"], src.ID); err != nil {
		t.Errorf("the source is gone after a share: %v", err)
	}
}

func TestShareChecksTheSourceSpace(t *testing.T) {
	c, ids := newStore(t, "alpha", "beta")
	src := put(t, c, ids["alpha"], draft("In alpha", "a body"))

	// Claiming the entry is in beta must not publish it out of alpha.
	if _, err := c.Share(t.Context(), ids["beta"], ids["personal"], src.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Share naming the wrong source space = %v, want ErrNotFound", err)
	}
}

// lore refuses to copy a user-model entry out of the personal space on every
// path. kenward used to recover that refusal from the wording of a sentence.
func TestShareUserModelRefusal(t *testing.T) {
	c, ids := newStore(t, "alpha")
	src := put(t, c, ids["personal"], Draft{
		Domain: "profile/preferences", Title: "How I like things", Body: "brief and specific",
	})
	_, err := c.Share(t.Context(), ids["personal"], ids["alpha"], src.ID)
	if !errors.Is(err, ErrUserModel) {
		t.Errorf("sharing a profile/ entry out of the personal space = %v, want ErrUserModel", err)
	}
}

func TestShareRejectsIncompleteArguments(t *testing.T) {
	c, ids := newStore(t, "alpha")
	for name, call := range map[string]func() error{
		"no id":   func() error { _, err := c.Share(t.Context(), ids["alpha"], ids["personal"], " "); return err },
		"no from": func() error { _, err := c.Share(t.Context(), "", ids["personal"], "id"); return err },
		"no to":   func() error { _, err := c.Share(t.Context(), ids["alpha"], "", "id"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("Share = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

// --- delete ------------------------------------------------------------------

func TestDelete(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	e := put(t, c, space, draft("Regrettable", "a regrettable subject"))

	if err := c.Delete(t.Context(), space, e.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Gone from both read paths, which is what the member was told.
	if _, err := c.Get(t.Context(), space, e.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	hits, err := c.Search(t.Context(), SearchQuery{Text: "a regrettable subject", Spaces: []domain.SpaceID{space}})
	if err != nil {
		t.Fatalf("Search after Delete: %v", err)
	}
	for _, h := range hits {
		if h.ID == e.ID {
			t.Error("the deleted entry still comes back from search")
		}
	}
}

// A tombstone served as a live entry was one of the defects the parsing layer
// carried: lore returned deleted entries by id and did not mark them, so the
// client could not tell. lore's Go API does not return a tombstone to a reader
// on any path.
func TestDeletedEntriesAreNeverServed(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	e := put(t, c, space, draft("Tombstoned", "a body"))
	if err := c.Delete(t.Context(), space, e.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, err := c.Get(t.Context(), space, e.ID); err == nil {
		t.Errorf("a tombstone was served as an entry: %+v", got)
	}
}

// Deleting twice is not an error: the caller that needs this is undoing a write
// and wants the entry gone, not the honour of being the one who removed it.
func TestDeleteOfAnAlreadyDeletedEntryIsSuccess(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	e := put(t, c, space, draft("Twice", "a body"))
	if err := c.Delete(t.Context(), space, e.ID); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := c.Delete(t.Context(), space, e.ID); err != nil {
		t.Errorf("second Delete: %v, want nil", err)
	}
}

func TestDeleteRefusals(t *testing.T) {
	c, ids := newStore(t, "alpha", "beta")
	e := put(t, c, ids["alpha"], draft("In alpha", "a body"))

	if err := c.Delete(t.Context(), unknownSpace, e.ID); !errors.Is(err, ErrUnknownSpace) {
		t.Errorf("delete against a space this home does not hold = %v, want ErrUnknownSpace", err)
	}
	if err := c.Delete(t.Context(), ids["alpha"], "definitely-not-an-entry-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete of an unknown id = %v, want ErrNotFound", err)
	}
	// The entry exists; the space does not name it. It must survive.
	if err := c.Delete(t.Context(), ids["beta"], e.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete of an alpha entry through beta = %v, want ErrNotFound", err)
	}
	if _, err := c.Get(t.Context(), ids["alpha"], e.ID); err != nil {
		t.Errorf("the entry was deleted out of a space it is not in: %v", err)
	}
	if err := c.Delete(t.Context(), ids["alpha"], " "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("delete with a blank id = %v, want ErrInvalidArgument", err)
	}
	if err := c.Delete(t.Context(), "", e.ID); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("delete with no space = %v, want ErrInvalidArgument", err)
	}
}

// --- spaces ------------------------------------------------------------------

func TestSpacesLists(t *testing.T) {
	c, ids := newStore(t, "alpha", "beta")
	put(t, c, ids["alpha"], draft("One", "a body"))
	put(t, c, ids["alpha"], draft("Two", "a body"))

	got, err := c.Spaces(t.Context())
	if err != nil {
		t.Fatalf("Spaces: %v", err)
	}
	byID := make(map[string]Space, len(got))
	for _, s := range got {
		if s.ID == "" || s.Name == "" {
			t.Errorf("incomplete row: %+v", s)
		}
		byID[s.ID] = s
	}
	// The wizard picks from this listing, and a personal space cannot serve as a
	// household's memory, so the kind has to be real.
	if k := byID[string(ids["personal"])].Kind; k != "personal" {
		t.Errorf("the personal space is reported as %q", k)
	}
	if k := byID[string(ids["alpha"])].Kind; k != "shared" {
		t.Errorf("a created space is reported as %q, want shared", k)
	}
	if n := byID[string(ids["alpha"])].Entries; n != 2 {
		t.Errorf("alpha reports %d entries, want 2", n)
	}
	if n := byID[string(ids["beta"])].Entries; n != 0 {
		t.Errorf("beta reports %d entries, want 0", n)
	}
}

// --- creating a space --------------------------------------------------------

// CreateSpace was a subprocess — `lore space create`, with the new id recovered by
// diffing the listing before and after, because parsing lore's prose has burned
// this package repeatedly. lore exports it now, so the subprocess, the diff and
// the seam that made the diff testable have all gone.

func TestCreateSpace(t *testing.T) {
	c, _ := newStore(t)
	sp, err := c.CreateSpace(t.Context(), "Casa-David")
	if err != nil {
		t.Fatalf("CreateSpace: %v", err)
	}
	if sp.ID == "" || sp.Name != "Casa-David" || sp.Kind != "shared" {
		t.Fatalf("space = %+v", sp)
	}
	// A space kenward made is a space kenward can write to. The id is the whole
	// product of this call — it becomes a member's private_space in kenward.yaml —
	// so an id that does not work is worse than an error.
	if _, err := c.Put(t.Context(), domain.SpaceID(sp.ID), draft("Proof", "written into the new space")); err != nil {
		t.Errorf("the new space does not accept a write: %v", err)
	}
	// And it is in the listing, as shared, so the wizard will offer it.
	spaces, err := c.Spaces(t.Context())
	if err != nil {
		t.Fatalf("Spaces: %v", err)
	}
	var found int
	for _, s := range spaces {
		if s.ID == sp.ID {
			found++
		}
	}
	if found != 1 {
		t.Errorf("the created space appears %d times in the listing", found)
	}
}

// TestCreateSpaceRefusesADuplicateName. There is deliberately no get-or-create:
// this id becomes a member's private space, and handing back an existing space
// because the names matched is how one member's memory becomes another's. The
// caller asks a person for a different name.
func TestCreateSpaceRefusesADuplicateName(t *testing.T) {
	c, _ := newStore(t, "Casa")
	_, err := c.CreateSpace(t.Context(), "Casa")
	if !errors.Is(err, ErrSpaceExists) {
		t.Fatalf("CreateSpace of a name already taken = %v, want ErrSpaceExists", err)
	}
	// Nothing was created: exactly one space still carries the name.
	spaces, err := c.Spaces(t.Context())
	if err != nil {
		t.Fatalf("Spaces: %v", err)
	}
	var named int
	for _, s := range spaces {
		if s.Name == "Casa" {
			named++
		}
	}
	if named != 1 {
		t.Errorf("%d spaces are named Casa; the refusal created one anyway", named)
	}
}

// TestCreateSpaceRejectsNamesBeforeCallingLore.
//
// The name comes from a web form, so this is a trust boundary. Nothing is argv
// from here any more, but `lore space invite <name>` is the very next command an
// operator runs against a space kenward made, and a name that cannot be shown on
// one line breaks every listing that has to show it.
func TestCreateSpaceRejectsNamesBeforeCallingLore(t *testing.T) {
	c, _ := newStore(t)
	for _, name := range []string{"", "   ", "-rf", "--help", "a\nb", "a\x00b"} {
		_, err := c.CreateSpace(t.Context(), name)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("CreateSpace(%q) = %v, want ErrInvalidArgument", name, err)
		}
	}
	// None of them reached the store.
	spaces, err := c.Spaces(t.Context())
	if err != nil {
		t.Fatalf("Spaces: %v", err)
	}
	if len(spaces) != 1 {
		t.Errorf("a refused name created a space: %+v", spaces)
	}
}

// "personal" is lore's own reserved name — there is exactly one personal space per
// home and it is Init's — so asking for it is a refusal rather than a second one.
func TestCreateSpaceCannotMakeASecondPersonalSpace(t *testing.T) {
	c, _ := newStore(t)
	if _, err := c.CreateSpace(t.Context(), "personal"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf(`CreateSpace("personal") = %v, want ErrInvalidArgument`, err)
	}
}

// --- lifecycle ---------------------------------------------------------------

// TestNewClientOnAnUninitialisedHome needs no lore binary, so it is the one test
// here that runs everywhere — including in the container gate, where nothing has
// a lore on PATH.
func TestNewClientOnAnUninitialisedHome(t *testing.T) {
	_, err := NewClient(Config{LoreHome: t.TempDir()})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("NewClient on an empty home = %v, want ErrStoreUnavailable", err)
	}
	// The operator has to be told which directory and what to run in it. lore's
	// error contract promises its errors never carry an entry's title or body,
	// which is what makes keeping its words here safe.
	if !strings.Contains(err.Error(), "lore init") {
		t.Errorf("the error does not say what to do about it: %v", err)
	}
}

func TestClosedClientRefusesEverything(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent.
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	for name, call := range map[string]func() error{
		"Search": func() error {
			_, err := c.Search(t.Context(), SearchQuery{Text: "x", Spaces: []domain.SpaceID{space}})
			return err
		},
		"Get":    func() error { _, err := c.Get(t.Context(), space, "id"); return err },
		"Put":    func() error { _, err := c.Put(t.Context(), space, draft("t", "b")); return err },
		"Share":  func() error { _, err := c.Share(t.Context(), space, space, "id"); return err },
		"Delete": func() error { return c.Delete(t.Context(), space, "id") },
		"Spaces": func() error { _, err := c.Spaces(t.Context()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrClosed) {
				t.Errorf("%s after Close = %v, want ErrClosed", name, err)
			}
		})
	}
}

// One Client is one lore home. A deployment runs one lore per member pod and the
// home is the only thing that isolates them, so two clients in one process must
// not see each other's entries.
func TestLoreHomeIsPerClient(t *testing.T) {
	a, aIDs := newStore(t, "alpha")
	b, bIDs := newStore(t, "alpha")
	if a.cfg.LoreHome == b.cfg.LoreHome {
		t.Fatal("the two clients share a home; this test proves nothing")
	}
	e := put(t, a, aIDs["alpha"], draft("Private to A", "a distinctive subject"))

	// B's space ids come from B's own store and are different values.
	if aIDs["alpha"] == bIDs["alpha"] {
		t.Error("two independently initialised homes minted the same space id")
	}
	if _, err := b.Get(t.Context(), bIDs["alpha"], e.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the second client can read the first's entry: %v", err)
	}
	got, err := b.Search(t.Context(), SearchQuery{Text: "a distinctive subject", Spaces: []domain.SpaceID{bIDs["alpha"]}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the second client found %d of the first's entries", len(got))
	}
}

func TestNewClientWithNoHome(t *testing.T) {
	t.Setenv("LORE_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, err := NewClient(Config{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("NewClient with nowhere to look = %v, want ErrInvalidArgument", err)
	}
}

// The entry's timestamps are real instants, not the zero time, and they are
// close enough to now that a parse failure silently swallowed would show.
func TestTimestampsAreParsed(t *testing.T) {
	c, ids := newStore(t, "alpha")
	before := time.Now().Add(-time.Minute)
	e := put(t, c, ids["alpha"], draft("Timed", "a body"))
	if e.CreatedAt.Before(before) || e.CreatedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("CreatedAt = %v, which is not around now", e.CreatedAt)
	}
	if e.UpdatedAt.Before(before) {
		t.Errorf("UpdatedAt = %v, which is not around now", e.UpdatedAt)
	}
}
