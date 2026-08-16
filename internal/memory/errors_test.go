package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/BlueHeisenberg/lore"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// TestMapErr is what replaced the golden corpus of lore's error prose.
//
// The classifier this replaces matched substrings of lore's human-readable
// rejections — "no entry with id", "is not a writer/owner of the space",
// "database is locked" — against a table of literals copied out of lore's source,
// and a reworded message became an unrecognised failure. This is the same table
// against lore's exported sentinels, where a rename is a compile error.
func TestMapErr(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"not found", lore.ErrNotFound, ErrNotFound},
		{"wrong space reads as absent", lore.ErrWrongSpace, ErrNotFound},
		{"space not found", lore.ErrSpaceNotFound, ErrUnknownSpace},
		{"user model", lore.ErrUserModel, ErrUserModel},
		{"not writer", lore.ErrNotWriter, ErrNotWriter},
		{"busy", lore.ErrBusy, ErrBusy},
		{"closed", lore.ErrClosed, ErrClosed},
		{"invalid argument", lore.ErrInvalidArgument, ErrInvalidArgument},
		{"no account", lore.ErrNoAccount, ErrStoreUnavailable},
		{"schema too new", lore.ErrSchemaTooNew, ErrStoreUnavailable},
		{"read only", lore.ErrReadOnly, ErrStoreUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := mapErr(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("mapErr(nil) = %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tt.want) {
				t.Errorf("mapErr(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// A wrapped lore error maps the same as a bare one: lore wraps its own sentinels
// with the space or entry id, and mapErr must not stop recognising them for it.
func TestMapErrSeesThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("lore: space %s: %w", "some-id", lore.ErrSpaceNotFound)
	if got := mapErr(wrapped); !errors.Is(got, ErrUnknownSpace) {
		t.Errorf("mapErr(wrapped) = %v, want ErrUnknownSpace", got)
	}
}

// An error this build does not recognise is returned as it is. Inventing a
// sentinel for an unknown failure is a worse lie than an opaque error, and it is
// the rule the prose classifier followed too.
func TestMapErrDoesNotGuess(t *testing.T) {
	odd := errors.New("lore: something this build has never heard of")
	got := mapErr(odd)
	if !errors.Is(got, odd) {
		t.Errorf("mapErr swallowed an unrecognised error: %v", got)
	}
	for _, s := range []error{ErrNotFound, ErrUnknownSpace, ErrBusy, ErrStoreUnavailable, ErrInvalidArgument} {
		if errors.Is(got, s) {
			t.Errorf("an unrecognised error was classified as %v", s)
		}
	}
}

// A context error belongs to the caller and is passed through, so a cancelled
// turn does not read as a store fault.
func TestMapErrPassesContextErrorsThrough(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := mapErr(err); !errors.Is(got, err) {
			t.Errorf("mapErr(%v) = %v", err, got)
		}
	}
}

// TestNotFoundIsTheInterfaceSentinel: Memory documents ErrNotFound for Get, Share
// and Delete, and it is declared in memory.go rather than here. Callers branch on
// it; nothing may shadow it with a second one.
func TestNotFoundIsTheInterfaceSentinel(t *testing.T) {
	if !errors.Is(mapErr(lore.ErrNotFound), ErrNotFound) {
		t.Error("lore's not-found does not satisfy the interface's ErrNotFound")
	}
}

// --- fault injection ---------------------------------------------------------

// faultyMemory decorates a real Memory with a failure it cannot be made to
// produce on its own.
//
// It exists for one narrow reason and should not grow. A real store in a temp dir
// is a better double than any fake in every way but one: it will essentially
// never return SQLITE_BUSY, and ErrBusy is live in production, because `lore
// serve` and any `lore` command an operator runs open the same database file. The
// scripted subprocess this package used to test against could produce contention
// on demand and a temp store cannot, so without this the busy path goes from
// covered to hoped-for.
//
// It decorates rather than replaces: every call it is not injecting into reaches
// the real client, so a test can put a fault in front of one method and still
// have the rest be lore.
type faultyMemory struct {
	Memory
	// fail is returned by the named methods instead of calling through. The key
	// is the method name, so a test can fault Search and leave Put real.
	fail map[string]error
}

func (f *faultyMemory) faulted(method string) error { return f.fail[method] }

func (f *faultyMemory) Search(ctx context.Context, q SearchQuery) ([]Entry, error) {
	if err := f.faulted("Search"); err != nil {
		return nil, err
	}
	return f.Memory.Search(ctx, q)
}

func (f *faultyMemory) Get(ctx context.Context, space domain.SpaceID, id string) (Entry, error) {
	if err := f.faulted("Get"); err != nil {
		return Entry{}, err
	}
	return f.Memory.Get(ctx, space, id)
}

func (f *faultyMemory) Put(ctx context.Context, space domain.SpaceID, d Draft) (Entry, error) {
	if err := f.faulted("Put"); err != nil {
		return Entry{}, err
	}
	return f.Memory.Put(ctx, space, d)
}

func (f *faultyMemory) Share(ctx context.Context, from, to domain.SpaceID, id string) (Entry, error) {
	if err := f.faulted("Share"); err != nil {
		return Entry{}, err
	}
	return f.Memory.Share(ctx, from, to, id)
}

func (f *faultyMemory) Delete(ctx context.Context, space domain.SpaceID, id string) error {
	if err := f.faulted("Delete"); err != nil {
		return err
	}
	return f.Memory.Delete(ctx, space, id)
}

var _ Memory = (*faultyMemory)(nil)

// TestFaultInjectionOverARealClient: the injected sentinel reaches a caller
// unchanged, and the methods that were not faulted are still the real store.
func TestFaultInjectionOverARealClient(t *testing.T) {
	c, ids := newStore(t, "alpha")
	space := ids["alpha"]
	mem := &faultyMemory{Memory: c, fail: map[string]error{"Search": ErrBusy}}

	e := put(t, c, space, draft("Still real", "a findable subject"))

	if _, err := mem.Search(t.Context(), SearchQuery{Text: "a findable subject", Spaces: []domain.SpaceID{space}}); !errors.Is(err, ErrBusy) {
		t.Errorf("Search under an injected fault = %v, want ErrBusy", err)
	}
	// Everything else is lore.
	got, err := mem.Get(t.Context(), space, e.ID)
	if err != nil {
		t.Fatalf("Get through the decorator: %v", err)
	}
	if got.Body != e.Body {
		t.Errorf("the decorator changed an unfaulted read: %+v", got)
	}
}
