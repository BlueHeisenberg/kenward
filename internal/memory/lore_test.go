package memory

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// The space ids below are the ones in testdata/spaces_list.txt, which is what the
// fake returns for lore_spaces.
const (
	spacePrivate   = domain.SpaceID("2d1b0000-0000-4000-8000-000000000000")
	spaceHousehold = domain.SpaceID("3e2c0000-0000-4000-8000-000000000000")
	spaceMissing   = domain.SpaceID("00000000-0000-4000-8000-00000000dead")
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// TestSearchRefusesAnEmptySpaceSet is the mechanical guarantee that no code path
// can search everything: it must fail before any subprocess is even started.
func TestSearchRefusesAnEmptySpaceSet(t *testing.T) {
	f := newFake(t, fakeScript{}, nil)
	_, err := f.Search(ctxT(t), SearchQuery{Text: "anything"})
	if !errors.Is(err, ErrEmptySpaceSet) {
		t.Fatalf("want ErrEmptySpaceSet, got %v", err)
	}
	if got := f.calls(t); len(got) != 0 {
		t.Fatalf("an empty space set must not reach lore, got %d calls", len(got))
	}
}

// TestSearchRefusesAnEmptySpaceID closes the same hole from the inside: a slice
// with an empty id in it has not named a space either, and lore reads an empty
// space argument as an invitation to work one out for itself.
func TestSearchRefusesAnEmptySpaceID(t *testing.T) {
	f := newFake(t, fakeScript{}, nil)
	for _, spaces := range [][]domain.SpaceID{{""}, {spacePrivate, ""}} {
		_, err := f.Search(ctxT(t), SearchQuery{Text: "anything", Spaces: spaces})
		if !errors.Is(err, ErrEmptySpaceSet) {
			t.Fatalf("Spaces %q: want ErrEmptySpaceSet, got %v", spaces, err)
		}
	}
	if got := f.calls(t); len(got) != 0 {
		t.Fatalf("an unnamed space must not reach lore, got %d calls", len(got))
	}
}

func TestSearchRefusesEmptyText(t *testing.T) {
	f := newFake(t, fakeScript{}, nil)
	_, err := f.Search(ctxT(t), SearchQuery{Text: "   ", Spaces: []domain.SpaceID{spacePrivate}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if got := f.calls(t); len(got) != 0 {
		t.Fatalf("a blank query must not reach lore, got %d calls", len(got))
	}
}

// TestSearchGroupsInCallerOrder covers the interface's central promise: one lore
// call per space, run concurrently, results grouped in the caller's order and
// never re-ranked across spaces.
func TestSearchGroupsInCallerOrder(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {
			{Match: string(spacePrivate), Text: golden(t, "search_basic.txt"), DelayMS: 60},
			{Match: string(spaceHousehold), Text: golden(t, "search_markers.txt")},
		},
	}}, nil)

	// The private space is listed first but answers last; grouping must follow
	// the caller's order, not arrival order.
	got, err := f.Search(ctxT(t), SearchQuery{
		Text:   "bin day",
		Spaces: []domain.SpaceID{spacePrivate, spaceHousehold},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	wantSpaces := []domain.SpaceID{spacePrivate, spacePrivate, spaceHousehold}
	if len(got) != len(wantSpaces) {
		t.Fatalf("got %d entries, want %d", len(got), len(wantSpaces))
	}
	for i, want := range wantSpaces {
		if got[i].Space != want {
			t.Errorf("entry %d is in space %s, want %s", i, got[i].Space, want)
		}
	}
	if got[2].Title != "Boiler service window" {
		t.Errorf("the second group is not the household result: %q", got[2].Title)
	}

	calls := callsTo(f.calls(t), toolSearch)
	if len(calls) != 2 {
		t.Fatalf("want one call per space, got %d", len(calls))
	}
	seen := map[string]bool{}
	for _, c := range calls {
		args := argsOf(t, c)
		sp, ok := args["space"].(string)
		if !ok || sp == "" {
			t.Fatalf("every search must name its space explicitly, got %v", args)
		}
		if _, dup := seen[sp]; dup {
			t.Fatalf("space %s searched twice", sp)
		}
		seen[sp] = true
		if _, hasScope := args["scope"]; hasScope {
			t.Errorf("scope must never be sent: it makes lore consult its working directory")
		}
	}
}

// TestSearchChecksTheEchoedSpace covers the other half of the space check: a
// scoped search asks for one space, and a hit lore labels with a different one
// must not be relabelled with the caller's id on the way out. Every row is
// checked, not just the first.
func TestSearchChecksTheEchoedSpace(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		// The second hit in the fixture is labelled "household".
		toolSearch: {{Text: golden(t, "search_wrong_space.txt")}},
	}}, nil)

	got, err := f.Search(ctxT(t), SearchQuery{
		Text: "bin day", Spaces: []domain.SpaceID{spacePrivate},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a hit from another lore space must be ErrNotFound, got %v (%d entries)", err, len(got))
	}
	if len(got) != 0 {
		t.Errorf("nothing may be handed back once a space mismatch is seen, got %+v", got)
	}
	if !strings.Contains(err.Error(), "household") {
		t.Errorf("the error must name the space lore actually returned: %v", err)
	}
}

// TestSearchReturnsExcerptsNotEntries covers the distinction the assistant's
// prompt renderer depends on: a search hit is an excerpt with the match
// highlighting stripped, and it must announce itself as partial.
func TestSearchReturnsExcerptsNotEntries(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: golden(t, "search_basic.txt")}},
	}}, nil)

	xs, err := f.SearchExcerpts(ctxT(t), SearchQuery{
		Text: "bin day", Spaces: []domain.SpaceID{spacePrivate},
	})
	if err != nil {
		t.Fatalf("SearchExcerpts: %v", err)
	}
	if len(xs) != 2 {
		t.Fatalf("got %d excerpts, want 2", len(xs))
	}
	for _, x := range xs {
		if x.Entry.Space != spacePrivate {
			t.Errorf("excerpt in the wrong space: %+v", x.Entry)
		}
		if strings.ContainsAny(x.Entry.Body, "[]") {
			t.Errorf("the rendered body still carries FTS5 highlighting: %q", x.Entry.Body)
		}
		if x.Snippet == "" {
			t.Errorf("the raw snippet must be kept for diagnosis: %+v", x)
		}
		if !IsExcerpt(x.Entry) {
			t.Errorf("a search hit must report as partial: %+v", x.Entry)
		}
	}
	if !strings.Contains(xs[0].Snippet, "[bin]") {
		t.Errorf("the raw snippet should be untouched, got %q", xs[0].Snippet)
	}

	// The interface method returns the same values, still partial.
	es, err := f.Search(ctxT(t), SearchQuery{Text: "bin day", Spaces: []domain.SpaceID{spacePrivate}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(es) != 2 || es[0].Body != xs[0].Entry.Body {
		t.Fatalf("Search and SearchExcerpts disagree: %+v vs %+v", es, xs)
	}
	for _, e := range es {
		if !IsExcerpt(e) {
			t.Errorf("Search must not hand out entries that look complete: %+v", e)
		}
	}
}

// TestPartialInvariantAcrossTheAPI asserts the invariant directly, on every
// method that hands out an Entry, so that Entry.Partial and the Excerpt type
// cannot drift apart: a search hit is always partial and everything else never
// is.
func TestPartialInvariantAcrossTheAPI(t *testing.T) {
	check := func(t *testing.T, source string, want bool, entries ...Entry) {
		t.Helper()
		if len(entries) == 0 {
			t.Fatalf("%s returned nothing to check", source)
		}
		for _, e := range entries {
			if e.Partial != want {
				t.Errorf("%s: Partial = %v, want %v: %+v", source, e.Partial, want, e)
			}
			if IsExcerpt(e) != e.Partial {
				t.Errorf("%s: IsExcerpt disagrees with Partial: %+v", source, e)
			}
		}
	}

	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: golden(t, "search_basic.txt")}},
		toolGet: {
			{Match: "3f1c9e2a", Text: golden(t, "get_minimal.txt")},
			{Match: "5f5f0000", Text: golden(t, "get_copy.txt")},
		},
		toolPut:   {{Text: golden(t, "put_stored.txt")}},
		toolShare: {{Text: golden(t, "share_copied.txt")}},
	}}, nil)
	ctx := ctxT(t)
	id := "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f"

	found, err := f.Search(ctx, SearchQuery{Text: "bin day", Spaces: []domain.SpaceID{spacePrivate}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	check(t, "Search", true, found...)

	xs, err := f.SearchExcerpts(ctx, SearchQuery{Text: "bin day", Spaces: []domain.SpaceID{spacePrivate}})
	if err != nil {
		t.Fatalf("SearchExcerpts: %v", err)
	}
	for _, x := range xs {
		check(t, "SearchExcerpts", true, x.Entry)
	}

	got, err := f.Get(ctx, spacePrivate, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	check(t, "Get", false, got)

	put, err := f.Put(ctx, spacePrivate, Draft{Domain: "home/routine", Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	check(t, "Put", false, put)

	shared, err := f.Share(ctx, spacePrivate, spaceHousehold, id)
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	check(t, "Share", false, shared)
}

// TestPartialSurvivesTheWriteFallbacks: the reconstructed entries returned when a
// read-back fails are whole, not excerpts. Their bodies are the caller's own text.
func TestPartialSurvivesTheWriteFallbacks(t *testing.T) {
	t.Run("put", func(t *testing.T) {
		f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
			toolPut: {{Text: golden(t, "put_stored.txt")}},
			// Every read-back fails, so Put must fall back to the draft.
			toolGet: {{Text: `no entry with id "x"`, IsError: true}},
		}}, nil)
		put, err := f.Put(ctxT(t), spacePrivate, Draft{Domain: "home/routine", Title: "t", Body: "b"})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if put.Body != "b" {
			t.Fatalf("expected the draft's own body in the fallback, got %q", put.Body)
		}
		if put.Partial {
			t.Errorf("the reconstructed put entry has a real body and must not be Partial: %+v", put)
		}
	})

	t.Run("share", func(t *testing.T) {
		f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
			toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
			toolGet: {
				{Match: "3f1c9e2a", Text: golden(t, "get_minimal.txt")},
				// The read-back of the new copy fails.
				{Match: "5f5f0000", Text: `no entry with id "x"`, IsError: true},
			},
			toolShare: {{Text: golden(t, "share_copied.txt")}},
		}}, nil)
		shared, err := f.Share(ctxT(t), spacePrivate, spaceHousehold, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f")
		if err != nil {
			t.Fatalf("Share: %v", err)
		}
		if shared.ID != "5f5f0000-0000-4000-8000-000000000000" || shared.Space != spaceHousehold {
			t.Fatalf("expected the source-derived copy, got %+v", shared)
		}
		if !shared.UpdatedAt.IsZero() {
			t.Errorf("the copy's own timestamp is unknown and must not be the source's: %v", shared.UpdatedAt)
		}
		if shared.Partial {
			t.Errorf("the source-derived share entry must not be Partial: %+v", shared)
		}
	})
}

func TestSearchDeduplicatesSpaces(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: golden(t, "search_markers.txt")}},
	}}, nil)
	got, err := f.Search(ctxT(t), SearchQuery{
		Text:   "boiler",
		Spaces: []domain.SpaceID{spaceHousehold, spaceHousehold},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if n := len(callsTo(f.calls(t), toolSearch)); n != 1 {
		t.Fatalf("want 1 call, got %d", n)
	}
}

func TestSearchPassesDomainAndLimit(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: golden(t, "search_none.txt")}},
	}}, nil)
	if _, err := f.Search(ctxT(t), SearchQuery{
		Text: "bin", Spaces: []domain.SpaceID{spacePrivate}, Domain: "home/routine", Limit: 3,
	}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	args := argsOf(t, callsTo(f.calls(t), toolSearch)[0])
	if args["domain"] != "home/routine" {
		t.Errorf("domain not forwarded: %v", args)
	}
	if args["limit"] != float64(3) {
		t.Errorf("limit not forwarded: %v", args)
	}
}

// TestSearchFailsWhenAnySpaceFails: a dropped space would silently narrow the
// answer, which is worse than an error.
func TestSearchFailsWhenAnySpaceFails(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {
			{Match: string(spacePrivate), Text: golden(t, "search_basic.txt")},
			{Match: string(spaceHousehold), Text: `unknown space "x" (try lore_spaces)`, IsError: true},
		},
	}}, nil)
	_, err := f.Search(ctxT(t), SearchQuery{
		Text: "bin day", Spaces: []domain.SpaceID{spacePrivate, spaceHousehold},
	})
	if !errors.Is(err, ErrUnknownSpace) {
		t.Fatalf("want ErrUnknownSpace, got %v", err)
	}
}

func TestSearchSurfacesAFormatChange(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: golden(t, "bad_search_header.txt")}},
	}}, nil)
	_, err := f.Search(ctxT(t), SearchQuery{Text: "bin", Spaces: []domain.SpaceID{spacePrivate}})
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a *ParseError, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

// TestGetChecksTheSpace covers the hole lore leaves open: lore_get is not
// space-scoped, so an id from another space must not satisfy a scoped Get.
func TestGetChecksTheSpace(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		// The fixture entry lives in "hearth-private".
		toolGet: {{Text: golden(t, "get_minimal.txt")}},
	}}, nil)
	ctx := ctxT(t)

	got, err := f.Get(ctx, spacePrivate, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f")
	if err != nil {
		t.Fatalf("Get from the right space: %v", err)
	}
	if got.Space != spacePrivate {
		t.Errorf("Space must be the caller's id, got %q", got.Space)
	}
	if got.Title != "Bin day is Tuesday" || got.Confidence != "provisional" || got.Origin != "evidence" {
		t.Errorf("unexpected entry: %+v", got)
	}
	if !got.CreatedAt.IsZero() {
		t.Errorf("lore never reports created_at, got %v", got.CreatedAt)
	}

	_, err = f.Get(ctx, spaceHousehold, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("an entry from another space must be ErrNotFound, got %v", err)
	}
}

func TestGetUnknownSpace(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolGet:    {{Text: golden(t, "get_minimal.txt")}},
	}}, nil)
	_, err := f.Get(ctxT(t), spaceMissing, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f")
	if !errors.Is(err, ErrUnknownSpace) {
		t.Fatalf("want ErrUnknownSpace, got %v", err)
	}
	if n := len(callsTo(f.calls(t), toolGet)); n != 0 {
		t.Errorf("a bad space must be caught before fetching, got %d lore_get calls", n)
	}
}

func TestGetNotFound(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolGet:    {{Text: `no entry with id "nope"`, IsError: true}},
	}}, nil)
	_, err := f.Get(ctxT(t), spacePrivate, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestSpacesListingIsCachedAndRefreshed proves the id-to-name mapping is fetched
// once and then re-fetched when a space id is not yet known.
func TestSpacesListingIsCachedAndRefreshed(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolGet:    {{Text: golden(t, "get_minimal.txt")}},
	}}, nil)
	ctx := ctxT(t)
	for range 3 {
		if _, err := f.Get(ctx, spacePrivate, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f"); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	if n := len(callsTo(f.calls(t), toolSpaces)); n != 1 {
		t.Fatalf("want the space listing fetched once, got %d", n)
	}
	// An id that is not cached forces exactly one refresh.
	if _, err := f.Get(ctx, spaceMissing, "x"); !errors.Is(err, ErrUnknownSpace) {
		t.Fatalf("want ErrUnknownSpace, got %v", err)
	}
	if n := len(callsTo(f.calls(t), toolSpaces)); n != 2 {
		t.Fatalf("want one refresh on a miss, got %d listings", n)
	}
}

// TestSpacesLists covers what a caller choosing a space needs: the id to
// configure, and the kind, which is what catches a personal space picked as a
// household's shared memory.
func TestSpacesLists(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
	}}, nil)

	got, err := f.Spaces(ctxT(t))
	if err != nil {
		t.Fatalf("Spaces: %v", err)
	}
	want := []Space{
		{ID: "1c0a0000-0000-4000-8000-000000000000", Name: "personal", Kind: "personal", Entries: 42},
		{ID: "2d1b0000-0000-4000-8000-000000000000", Name: "hearth-private", Kind: "shared", Entries: 7},
		{ID: "3e2c0000-0000-4000-8000-000000000000", Name: "household", Kind: "shared", Entries: 19},
		{ID: "4f3d0000-0000-4000-8000-000000000000", Name: "kenward", Kind: "shared", Entries: 3},
		{ID: "5a4e0000-0000-4000-8000-000000000000", Name: "kenward-test-household", Kind: "shared", Entries: 0},
	}
	if !slices.Equal(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

// TestSpacesKeepsDuplicateNames: lore does not enforce unique display names, so
// two spaces called the same thing are two rows. Collapsing them here would make
// this package pick which space an operator meant, which is exactly the choice it
// must not make.
func TestSpacesKeepsDuplicateNames(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: "household  kind:shared  members:1  entries:1  id:aaa0\n" +
			"household  kind:shared  members:1  entries:2  id:bbb0\n"}},
	}}, nil)

	got, err := f.Spaces(ctxT(t))
	if err != nil {
		t.Fatalf("Spaces: %v", err)
	}
	if len(got) != 2 || got[0].ID != "aaa0" || got[1].ID != "bbb0" {
		t.Fatalf("both rows must survive, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Put
// ---------------------------------------------------------------------------

func TestPut(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolPut: {{Text: golden(t, "put_stored.txt")}},
		toolGet: {{Text: golden(t, "get_minimal.txt")}},
	}}, nil)

	got, err := f.Put(ctxT(t), spacePrivate, Draft{
		Domain:     "home/routine",
		Title:      "Bin day is Tuesday",
		Body:       "The green bin goes out Tuesday night.",
		Confidence: "provisional",
		Markers:    []string{"CONTEXT", "[IMPORTANT]"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got.ID != "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f" || got.Space != spacePrivate {
		t.Errorf("unexpected entry: %+v", got)
	}
	// The entry must come from the read-back, not from the draft.
	if got.UpdatedAt.IsZero() {
		t.Errorf("the stored entry should carry lore's updated_at")
	}

	args := argsOf(t, callsTo(f.calls(t), toolPut)[0])
	if args["space"] != string(spacePrivate) {
		t.Errorf("put must name the space explicitly, got %v", args)
	}
	if _, ok := args["subject"]; ok {
		t.Errorf("subject must never be sent: it is lore's working-directory routing")
	}
	if args["markers"] != "CONTEXT,[IMPORTANT]" {
		t.Errorf("markers must be a comma-separated list, got %v", args["markers"])
	}
	if args["confidence"] != "provisional" {
		t.Errorf("confidence not forwarded: %v", args)
	}
}

// TestPutSucceedsWhenTheReadBackFails: the write landed, so the call must not
// report failure just because the follow-up read did.
func TestPutSucceedsWhenTheReadBackFails(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolPut: {{Text: golden(t, "put_stored.txt")}},
		toolGet: {{Text: `no entry with id "x"`, IsError: true}},
	}}, nil)
	got, err := f.Put(ctxT(t), spacePrivate, Draft{
		Domain: "home/routine", Title: "Bin day is Tuesday", Body: "b", Markers: []string{"context"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got.ID != "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f" {
		t.Errorf("id not taken from the write receipt: %+v", got)
	}
	if len(got.Markers) != 1 || got.Markers[0] != "[CONTEXT]" {
		t.Errorf("the fallback entry must carry lore's normalised markers, got %v", got.Markers)
	}
	if got.Confidence != "provisional" || got.Origin != "evidence" {
		t.Errorf("defaults must come from the receipt, got %+v", got)
	}
	// The body is the draft's own, so this is a whole entry even though its
	// timestamp is unknown; it must not be mistaken for a search excerpt.
	if IsExcerpt(got) {
		t.Errorf("the reconstructed entry has a real body and must not report as partial: %+v", got)
	}
}

func TestPutRejectsBadDrafts(t *testing.T) {
	f := newFake(t, fakeScript{}, nil)
	tests := []struct {
		name string
		d    Draft
	}{
		{"no title", Draft{Domain: "d", Body: "b"}},
		{"no body", Draft{Domain: "d", Title: "t"}},
		{"no domain", Draft{Title: "t", Body: "b"}},
		{"bad confidence", Draft{Domain: "d", Title: "t", Body: "b", Confidence: "quite-sure"}},
		{"marker with a comma", Draft{Domain: "d", Title: "t", Body: "b", Markers: []string{"a,b"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.Put(ctxT(t), spacePrivate, tc.d); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
		})
	}
	if n := len(f.calls(t)); n != 0 {
		t.Fatalf("invalid drafts must not reach lore, got %d calls", n)
	}
}

func TestPutNotWriter(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolPut: {{Text: "put: space household: this account is not a writer/owner of the space", IsError: true}},
	}}, nil)
	_, err := f.Put(ctxT(t), spaceHousehold, Draft{Domain: "d", Title: "t", Body: "b"})
	if !errors.Is(err, ErrNotWriter) {
		t.Fatalf("want ErrNotWriter, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Share
// ---------------------------------------------------------------------------

func TestShare(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolGet: {
			{Match: "3f1c9e2a", Text: golden(t, "get_minimal.txt")},
			{Match: "5f5f0000", Text: golden(t, "get_copy.txt")},
		},
		toolShare: {{Text: golden(t, "share_copied.txt")}},
	}}, nil)

	got, err := f.Share(ctxT(t), spacePrivate, spaceHousehold, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f")
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if got.ID != "5f5f0000-0000-4000-8000-000000000000" {
		t.Errorf("the returned entry must be the copy, got %s", got.ID)
	}
	if got.Space != spaceHousehold {
		t.Errorf("the copy must be reported in the destination space, got %s", got.Space)
	}

	share := callsTo(f.calls(t), toolShare)
	if len(share) != 1 {
		t.Fatalf("want one lore_share call, got %d", len(share))
	}
	args := argsOf(t, share[0])
	if args["confirm"] != true {
		t.Errorf("share must execute, not preview: %v", args)
	}
	if args["to_space"] != string(spaceHousehold) {
		t.Errorf("to_space not passed as an id: %v", args)
	}
}

// TestShareChecksTheSourceSpace: lore_share takes no source space, so an entry
// the caller was not authorized to read must not be copyable out of it.
func TestShareChecksTheSourceSpace(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolGet:    {{Text: golden(t, "get_minimal.txt")}}, // lives in hearth-private
		toolShare:  {{Text: golden(t, "share_copied.txt")}},
	}}, nil)
	_, err := f.Share(ctxT(t), spaceHousehold, spacePrivate, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if n := len(callsTo(f.calls(t), toolShare)); n != 0 {
		t.Fatalf("nothing must be copied when the source space does not match, got %d shares", n)
	}
}

func TestShareUserModelRefusal(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolGet:    {{Text: golden(t, "get_minimal.txt")}},
		toolShare: {{
			Text:    `refused: "profile/preferences" is a user-model entry (profile/, feedback/); those never leave the personal space`,
			IsError: true,
		}},
	}}, nil)
	_, err := f.Share(ctxT(t), spacePrivate, spaceHousehold, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f")
	if !errors.Is(err, ErrUserModel) {
		t.Fatalf("want ErrUserModel, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Contention, lifecycle and timeouts
// ---------------------------------------------------------------------------

func TestBusyIsRetried(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {
			{Text: "search: database is locked (5) (SQLITE_BUSY)", IsError: true},
			{Text: "search: database is locked (5) (SQLITE_BUSY)", IsError: true},
			{Text: golden(t, "search_markers.txt")},
		},
	}}, nil)
	got, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spaceHousehold}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if n := len(callsTo(f.calls(t), toolSearch)); n != 3 {
		t.Fatalf("want 3 attempts, got %d", n)
	}
}

func TestBusyRetriesAreBounded(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: "search: database is locked", IsError: true}},
	}}, func(c *Config) { c.BusyRetries = 2 })
	_, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if n := len(callsTo(f.calls(t), toolSearch)); n != 3 {
		t.Fatalf("want 1 attempt plus 2 retries, got %d", n)
	}
}

// TestSubprocessIsRestarted: a lore that dies must not be a permanent failure.
func TestSubprocessIsRestarted(t *testing.T) {
	f := newFake(t, fakeScript{
		// The listing is call 1; the search itself is the one that dies.
		DieOnCall: 2,
		Replies: map[string][]fakeReply{
			toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
			toolSearch: {{Text: golden(t, "search_markers.txt")}},
		},
	}, nil)
	got, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spaceHousehold}})
	if err != nil {
		t.Fatalf("a read must survive one subprocess death: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if n := len(pids(f.calls(t))); n != 2 {
		t.Fatalf("want the call served by a second subprocess, saw %d pids", n)
	}
}

// TestWritesAreNotReplayedAfterADeath: a lore_put whose answer was lost may have
// landed, so it must be reported rather than repeated.
func TestWritesAreNotReplayedAfterADeath(t *testing.T) {
	f := newFake(t, fakeScript{
		DieOnCall: 1,
		Replies: map[string][]fakeReply{
			toolPut: {{Text: golden(t, "put_stored.txt")}},
		},
	}, nil)
	_, err := f.Put(ctxT(t), spacePrivate, Draft{Domain: "d", Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("want an error when the subprocess dies mid-write")
	}
	if !strings.Contains(err.Error(), "subprocess ended") {
		t.Errorf("the error should say the subprocess ended, got %v", err)
	}
	// lore has no delete, so the caller has to be able to say "this may not have
	// saved" instead of inviting a retry that duplicates the entry permanently.
	if !errors.Is(err, ErrWriteUncertain) {
		t.Errorf("want ErrWriteUncertain, got %v", err)
	}
	if n := len(callsTo(f.calls(t), toolPut)); n != 1 {
		t.Fatalf("a write must not be retried, saw %d attempts", n)
	}
	// The next call restarts it.
	if _, err := f.Put(ctxT(t), spacePrivate, Draft{Domain: "d", Title: "t", Body: "b"}); err != nil {
		t.Fatalf("the next write should reach a fresh subprocess: %v", err)
	}
	if n := len(pids(f.calls(t))); n != 2 {
		t.Fatalf("want a second subprocess, saw %d pids", n)
	}
}

// TestUncertainWrites separates the two write outcomes a member has to be told
// apart. lore has no delete: a duplicate is permanent, so "this may not have
// saved" and "this did not save" must never be conflated.
func TestUncertainWrites(t *testing.T) {
	t.Run("timed out after the request went out", func(t *testing.T) {
		f := newFake(t, fakeScript{
			HangOnCall: 1,
			Replies:    map[string][]fakeReply{toolPut: {{Text: golden(t, "put_stored.txt")}}},
		}, func(c *Config) { c.CallTimeout = 250 * time.Millisecond })
		_, err := f.Put(ctxT(t), spacePrivate, Draft{Domain: "d", Title: "t", Body: "b"})
		if !errors.Is(err, ErrWriteUncertain) {
			t.Fatalf("want ErrWriteUncertain, got %v", err)
		}
	})

	t.Run("receipt in an unknown format", func(t *testing.T) {
		f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
			toolPut: {{Text: golden(t, "bad_put_line.txt")}},
		}}, nil)
		_, err := f.Put(ctxT(t), spacePrivate, Draft{Domain: "d", Title: "t", Body: "b"})
		if !errors.Is(err, ErrWriteUncertain) {
			t.Fatalf("lore answered, so the entry exists; want ErrWriteUncertain, got %v", err)
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Errorf("the underlying cause must still be inspectable, got %T", err)
		}
	})

	t.Run("share whose answer was lost", func(t *testing.T) {
		f := newFake(t, fakeScript{
			DieOnCall: 3, // lore_spaces, lore_get, then the share itself
			Replies: map[string][]fakeReply{
				toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
				toolGet:    {{Text: golden(t, "get_minimal.txt")}},
				toolShare:  {{Text: golden(t, "share_copied.txt")}},
			},
		}, nil)
		_, err := f.Share(ctxT(t), spacePrivate, spaceHousehold, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f")
		if !errors.Is(err, ErrWriteUncertain) {
			t.Fatalf("want ErrWriteUncertain, got %v", err)
		}
	})

	t.Run("rejections and reads are certain", func(t *testing.T) {
		f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
			toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
			toolPut:    {{Text: "put: space household: this account is not a writer/owner of the space", IsError: true}},
			toolSearch: {{Text: "search: database is locked", IsError: true}},
		}}, func(c *Config) { c.BusyRetries = 1 })

		// lore rejecting a write means it did not apply it.
		_, err := f.Put(ctxT(t), spaceHousehold, Draft{Domain: "d", Title: "t", Body: "b"})
		if errors.Is(err, ErrWriteUncertain) {
			t.Errorf("a rejection from lore must not be reported as uncertain: %v", err)
		}
		// So does exhausting the contention retries: nothing was committed.
		_, err = f.Search(ctxT(t), SearchQuery{Text: "x", Spaces: []domain.SpaceID{spacePrivate}})
		if errors.Is(err, ErrWriteUncertain) {
			t.Errorf("a read must never be reported as an uncertain write: %v", err)
		}
		// And a draft this client refuses never reaches lore at all.
		_, err = f.Put(ctxT(t), spacePrivate, Draft{Domain: "d", Title: "t", Body: "b", Confidence: "nope"})
		if errors.Is(err, ErrWriteUncertain) {
			t.Errorf("a rejected draft must not be reported as uncertain: %v", err)
		}
	})
}

// TestHungSubprocessDoesNotHangTheCaller bounds a wedged lore, and pins the
// recovery: a process that is alive but no longer answering is as useless as a
// dead one, so it must be retired rather than handed to the next call. Without
// that, one timeout wedges every later call for the life of the client.
func TestHungSubprocessDoesNotHangTheCaller(t *testing.T) {
	f := newFake(t, fakeScript{
		// The listing is call 1; the search is the call that never answers.
		HangOnCall: 2,
		Replies: map[string][]fakeReply{
			toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
			toolSearch: {{Text: golden(t, "search_markers.txt")}},
		},
	}, func(c *Config) {
		c.CallTimeout = 250 * time.Millisecond
		c.BusyRetries = 1
	})

	start := time.Now()
	_, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spaceHousehold}})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if !strings.Contains(err.Error(), "no answer within") {
		t.Errorf("the error should name the timeout, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the call took %s; a hung lore must not hold a conversation open", elapsed)
	}

	got, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spaceHousehold}})
	if err != nil {
		t.Fatalf("the next search should reach a fresh subprocess: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if n := len(pids(f.calls(t))); n != 2 {
		t.Fatalf("the wedged subprocess was reused: saw %d pids", n)
	}
}

func TestStartFailureExplainsItself(t *testing.T) {
	f := newFake(t, fakeScript{
		ExitBeforeServe: true,
		Stderr:          "lore: load account (run `lore init` first?)\n",
	}, func(c *Config) { c.StartTimeout = 10 * time.Second })

	_, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}})
	if err == nil {
		t.Fatal("want a start-up error")
	}
	// The stderr tail explains the failure but is lore's output, not kenward's, so
	// it travels behind an explicit accessor rather than in the string a caller
	// logs by default.
	if strings.Contains(err.Error(), "lore init") {
		t.Errorf("the subprocess stderr must not be in the default rendering, got %v", err)
	}
	var pe *ProcessError
	if !errors.As(err, &pe) {
		t.Fatalf("want a *ProcessError, got %T: %v", err, err)
	}
	if !strings.Contains(pe.Detail(), "lore init") {
		t.Errorf("the subprocess stderr must reach an operator who asks for it, got %q", pe.Detail())
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Errorf("the error must still say what failed, got %v", err)
	}
}

func TestCloseTerminatesTheSubprocess(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: golden(t, "search_markers.txt")}},
	}}, nil)
	if _, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spaceHousehold}}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	f.mu.Lock()
	s := f.cur
	f.mu.Unlock()
	if s == nil {
		t.Fatal("no subprocess after a successful call")
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-s.proc.exited:
	default:
		t.Fatal("Close returned with the subprocess still running")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	_, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spaceHousehold}})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

// TestNoGoroutineLeak covers both leaks the design has to avoid: the supervisor
// and transport goroutines of every session, including sessions retired by a
// restart.
func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	for range 3 {
		f := newFake(t, fakeScript{
			DieOnCall: 2,
			Replies: map[string][]fakeReply{
				toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
				toolSearch: {{Text: golden(t, "search_markers.txt")}},
				toolGet:    {{Text: golden(t, "get_minimal.txt")}},
			},
		}, nil)
		ctx := ctxT(t)
		if _, err := f.Search(ctx, SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spaceHousehold}}); err != nil {
			t.Fatalf("Search: %v", err)
		}
		if _, err := f.Get(ctx, spacePrivate, "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f"); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	var after int
	for range 100 {
		after = runtime.NumGoroutine()
		if after <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines grew from %d to %d", before, after)
}

func TestNewClientRejectsAnEmptyCommand(t *testing.T) {
	if _, err := NewClient(Config{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

// TestLoreHomeIsPerClient pins the isolation mechanism: LORE_HOME is the only
// thing separating two lore instances on one host, so it must be settable per
// client rather than per process.
func TestLoreHomeIsPerClient(t *testing.T) {
	home := t.TempDir()
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: golden(t, "search_none.txt")}},
	}}, func(c *Config) { c.LoreHome = home })
	if _, err := f.Search(ctxT(t), SearchQuery{Text: "x", Spaces: []domain.SpaceID{spacePrivate}}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, kv := range f.cfg.childEnv() {
		if kv == "LORE_HOME="+home {
			found = true
		}
	}
	if !found {
		t.Fatalf("LORE_HOME=%s not exported to the subprocess", home)
	}
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

// deleteFake builds a client whose lore answers a delete with text.
func deleteFake(t *testing.T, text string, isErr bool) *fake {
	t.Helper()
	return newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolDelete: {{Text: text, IsError: isErr}},
	}}, nil)
}

const deletedEntryID = "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f"

// TestDeletePassesTheSpaceToLore. The space is not decoration on the call: entry
// ids are global and lore_get is not space-scoped, so an id alone is a capability
// to name an entry anywhere. lore compares space ids rather than the display names
// this client has to fall back on elsewhere, so passing it through is a stronger
// check than Get's — but only if it is actually passed.
func TestDeletePassesTheSpaceToLore(t *testing.T) {
	f := deleteFake(t, golden(t, "delete_done.txt"), false)
	if err := f.Delete(ctxT(t), spacePrivate, deletedEntryID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	calls := callsTo(f.calls(t), toolDelete)
	if len(calls) != 1 {
		t.Fatalf("%d lore_delete calls, want 1", len(calls))
	}
	args := argsOf(t, calls[0])
	if got := args["space"]; got != string(spacePrivate) {
		t.Errorf("space argument = %v, want %s", got, spacePrivate)
	}
	if got := args["id"]; got != deletedEntryID {
		t.Errorf("id argument = %v, want %s", got, deletedEntryID)
	}
}

// TestDeleteOfAnAlreadyDeletedEntryIsSuccess. lore reports the no-op rather than
// failing, and the caller wants the entry gone rather than the credit for removing
// it. Undo is the only caller, and telling a member "I couldn't take that back"
// about an entry that is correctly absent would be a failure notice about a
// success.
func TestDeleteOfAnAlreadyDeletedEntryIsSuccess(t *testing.T) {
	f := deleteFake(t, golden(t, "delete_already.txt"), false)
	if err := f.Delete(ctxT(t), spacePrivate, deletedEntryID); err != nil {
		t.Errorf("Delete of an already-deleted entry = %v, want nil", err)
	}
}

// TestDeleteRejectionsAreClassified. Every one of these means the entry is still
// there, and none of them may come back as ErrWriteUncertain: undo says one thing
// when the store answered and a different thing when it did not, and the two
// sentences are only different if this classification holds.
func TestDeleteRejectionsAreClassified(t *testing.T) {
	cases := []struct {
		name string
		text string
		want error
	}{
		{
			"an unknown id",
			`no entry with id "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f" — nothing was deleted`,
			ErrNotFound,
		},
		{
			"an entry in a different space",
			`entry 3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f is not in space "hearth-private" — nothing was deleted (delete is space-scoped; pass the space the entry actually lives in)`,
			nil, // a ToolError with no sentinel: lore refused, and nothing was deleted
		},
		{
			"the store is locked",
			"delete: database is locked",
			ErrBusy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := deleteFake(t, tc.text, true)
			err := f.Delete(ctxT(t), spacePrivate, deletedEntryID)
			if err == nil {
				t.Fatal("Delete = nil, want the rejection reported")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("Delete = %v, want %v", err, tc.want)
			}
			// The whole point of the classification: a refusal is not an
			// uncertainty, and the member is told the entry is still there
			// rather than that nobody knows.
			if errors.Is(err, ErrWriteUncertain) {
				t.Errorf("a rejection lore itself sent came back as uncertain: %v", err)
			}
		})
	}
}

// TestDeleteWithNoAnswerIsUncertain: the subprocess died mid-call, so the tombstone
// may or may not have been committed. Reporting this as a flat failure would have
// undo tell a member their entry is still there when it may well be gone — the
// mirror of the uncertain-write rule, and wrong in the same way.
func TestDeleteWithNoAnswerIsUncertain(t *testing.T) {
	f := newFake(t, fakeScript{
		Replies:   map[string][]fakeReply{toolSpaces: {{Text: golden(t, "spaces_list.txt")}}},
		DieOnCall: 2, // 1 is lore_spaces, 2 is the delete
	}, nil)
	err := f.Delete(ctxT(t), spacePrivate, deletedEntryID)
	if !errors.Is(err, ErrWriteUncertain) {
		t.Errorf("Delete after a lost subprocess = %v, want ErrWriteUncertain", err)
	}
}

// TestDeleteWithAnUnreadableReceiptIsUncertain. lore answered without an error, so
// the tombstone is written — but this client cannot read the receipt, and saying
// "removed" on the strength of a line it did not understand is a guess about the
// one thing it is about to tell a member.
func TestDeleteWithAnUnreadableReceiptIsUncertain(t *testing.T) {
	f := deleteFake(t, "gone, probably", false)
	err := f.Delete(ctxT(t), spacePrivate, deletedEntryID)
	if !errors.Is(err, ErrWriteUncertain) {
		t.Errorf("Delete with an unparseable receipt = %v, want ErrWriteUncertain", err)
	}
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Tool != toolDelete {
		t.Errorf("the cause should be a %s ParseError, got %v", toolDelete, err)
	}
}

// TestDeleteAnsweredForAnotherEntryIsUncertain. lore reporting a different id than
// the one asked about means something was deleted and it may not have been this;
// the client can say neither "removed" nor "still there" about the entry it named.
func TestDeleteAnsweredForAnotherEntryIsUncertain(t *testing.T) {
	f := deleteFake(t, golden(t, "delete_done.txt"), false)
	err := f.Delete(ctxT(t), spacePrivate, "some-other-entry")
	if !errors.Is(err, ErrWriteUncertain) {
		t.Errorf("Delete answered for another id = %v, want ErrWriteUncertain", err)
	}
}

// TestDeleteRefusesAnArgumentItCannotSend, before any subprocess is started. An
// empty space would let lore work the destination out from its working directory,
// which is the one thing this client never permits.
func TestDeleteRefusesAnArgumentItCannotSend(t *testing.T) {
	cases := []struct {
		name  string
		space domain.SpaceID
		id    string
	}{
		{"no id", spacePrivate, "  "},
		{"no space", "", deletedEntryID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, fakeScript{}, nil)
			if err := f.Delete(ctxT(t), tc.space, tc.id); !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("Delete = %v, want ErrInvalidArgument", err)
			}
			if n := len(f.calls(t)); n != 0 {
				t.Errorf("%d calls reached lore; a refused argument must never leave this process", n)
			}
		})
	}
}

// TestDeleteInAnUnknownSpaceNeverReachesLore: the space is resolved first, so a
// configuration naming a space this lore home does not hold is a configuration
// fault reported as one, rather than an id sent off with a space name lore will
// interpret however it likes.
func TestDeleteInAnUnknownSpaceNeverReachesLore(t *testing.T) {
	f := deleteFake(t, golden(t, "delete_done.txt"), false)
	if err := f.Delete(ctxT(t), spaceMissing, deletedEntryID); !errors.Is(err, ErrUnknownSpace) {
		t.Errorf("Delete = %v, want ErrUnknownSpace", err)
	}
	if n := len(callsTo(f.calls(t), toolDelete)); n != 0 {
		t.Errorf("%d deletes reached lore for a space it does not hold", n)
	}
}

// TestParseDeleted covers the receipt parser on its own, including the shapes that
// must not be mistaken for a success.
func TestParseDeleted(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		wantID      string
		wantAlready bool
		wantErr     bool
	}{
		{"a fresh delete", golden(t, "delete_done.txt"), deletedEntryID, false, false},
		{"an already-deleted entry", golden(t, "delete_already.txt"), deletedEntryID, true, false},
		{"empty output", "", "", false, true},
		{"a rejection that reached the parser", `no entry with id "x" — nothing was deleted`, "", false, true},
		// "deleted" with nothing after the id is not lore's format, and treating
		// it as one would accept a truncated answer as a completed delete.
		{"a truncated receipt", "deleted " + deletedEntryID, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, already, err := parseDeleted(tc.text)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDeleted(%q) = (%q, %v, nil), want an error", tc.text, id, already)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDeleted: %v", err)
			}
			if id != tc.wantID || already != tc.wantAlready {
				t.Errorf("parseDeleted = (%q, %v), want (%q, %v)", id, already, tc.wantID, tc.wantAlready)
			}
		})
	}
}
