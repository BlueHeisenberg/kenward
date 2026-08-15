package memory

import (
	"context"
	"errors"
	"runtime"
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

func TestSearchDeduplicatesSpaces(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSearch: {{Text: golden(t, "search_markers.txt")}},
	}}, nil)
	got, err := f.Search(ctxT(t), SearchQuery{
		Text:   "boiler",
		Spaces: []domain.SpaceID{spacePrivate, spacePrivate},
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
		toolSearch: {
			{Text: "search: database is locked (5) (SQLITE_BUSY)", IsError: true},
			{Text: "search: database is locked (5) (SQLITE_BUSY)", IsError: true},
			{Text: golden(t, "search_markers.txt")},
		},
	}}, nil)
	got, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}})
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
		DieOnCall: 1,
		Replies: map[string][]fakeReply{
			toolSearch: {{Text: golden(t, "search_markers.txt")}},
		},
	}, nil)
	got, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}})
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

// TestHungSubprocessDoesNotHangTheCaller bounds a wedged lore.
func TestHungSubprocessDoesNotHangTheCaller(t *testing.T) {
	f := newFake(t, fakeScript{
		HangOnCall: 1,
		Replies:    map[string][]fakeReply{toolSearch: {{Text: golden(t, "search_markers.txt")}}},
	}, func(c *Config) {
		c.CallTimeout = 250 * time.Millisecond
		c.BusyRetries = 1
	})

	start := time.Now()
	_, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}})
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
	if !strings.Contains(err.Error(), "lore init") {
		t.Errorf("the subprocess stderr must reach the operator, got %v", err)
	}
}

func TestCloseTerminatesTheSubprocess(t *testing.T) {
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSearch: {{Text: golden(t, "search_markers.txt")}},
	}}, nil)
	if _, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}}); err != nil {
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
	_, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}})
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
		if _, err := f.Search(ctx, SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}}); err != nil {
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
