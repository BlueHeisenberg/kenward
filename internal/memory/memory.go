// Package memory is kenward's view of the knowledge store.
//
// kenward owns no knowledge model. This package defines the narrow surface kenward
// needs and adapts lore to it; lore's spaces, entries, markers, confidence and origin
// fields are taken as given.
package memory

import (
	"context"
	"errors"
	"time"

	"github.com/BlueHeisenberg/lore"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Entry is a stored piece of knowledge as kenward sees it.
//
// Every Entry this package returns is whole. Body is the entry's whole body on
// every path, search included, and Origin and the timestamps are always the
// stored ones. That has not always been true: while lore was reached by parsing
// the text of `lore mcp`, a search hit was about twelve tokens of the body with
// no origin and no timestamps, and this struct carried a Partial flag so that
// nothing could render a fragment as a memory. lore's Go API returns the entry,
// so there is no fragment to disclose and no flag to check.
type Entry struct {
	ID         string
	Space      domain.SpaceID
	Domain     string
	Title      string
	Body       string
	Confidence string
	Markers    []string
	Origin     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Draft is a proposed new entry, before it has been confirmed and stored.
type Draft struct {
	Domain     string
	Title      string
	Body       string
	Confidence string
	// Markers is lore's, and nothing in kenward fills it. The capture flow shows a
	// member the title and the body and nothing else, so a marker would be a stored
	// field they approved without reading — and a later prompt renders markers back
	// to the model. The remember tool therefore does not offer the field at all
	// (internal/assistant's rememberSchema). It stays here because it is part of
	// lore's write surface and Put must be able to carry it if a caller ever
	// legitimately has one; do not wire it back to anything the model writes.
	Markers []string
}

// SearchQuery asks for entries from an explicit set of spaces.
//
// Spaces is required. There is deliberately no "search everything" mode: the space set
// comes from a resolved domain.Scope, so a code path that has not made an authorization
// decision cannot retrieve anything.
//
// Text is matched the way lore matches it, which is narrower than it looks — see Terms.
// A caller passing a member's sentence through unchanged will retrieve nothing, almost
// always; turning a sentence into queries is the caller's job.
type SearchQuery struct {
	Text   string
	Spaces []domain.SpaceID
	Domain string
	Limit  int
}

// Terms reduces text to the words lore will actually search for.
//
// It is lore.Terms, re-exported so that callers here need not import lore to
// reason in the units it counts in. The rules — conjunctive, case-insensitive, no
// operators, no stemming, no prefix matching, no stopwords, every term required —
// are documented on lore.Terms, which is where they belong: they are properties
// of lore's index, and this package used to describe them from the outside.
func Terms(text string) []string { return lore.Terms(text) }

// ErrEmptySpaceSet is returned when a search is attempted without an explicit space
// set. It is a programming error, not a user-facing condition.
var ErrEmptySpaceSet = errors.New("memory: search requires an explicit space set")

// ErrNotFound is returned by Get, Share and Delete for an unknown entry id.
var ErrNotFound = errors.New("memory: entry not found")

// Memory is the knowledge store kenward talks to.
//
// Implementations must not re-rank results across spaces: Search returns entries
// grouped in the order the caller listed the spaces, primary first. Ranking across a
// private and a shared space is a policy decision that belongs to the assistant.
type Memory interface {
	Search(ctx context.Context, q SearchQuery) ([]Entry, error)
	Get(ctx context.Context, space domain.SpaceID, id string) (Entry, error)
	Put(ctx context.Context, space domain.SpaceID, d Draft) (Entry, error)
	// Share copies an entry from one space to another, preserving provenance. It is
	// used for the deliberate act of publishing something private to the household,
	// and must never be emulated with a read followed by a Put.
	Share(ctx context.Context, from, to domain.SpaceID, entryID string) (Entry, error)
	// Delete removes one entry from one space.
	//
	// It is space-scoped for the same reason Get is: an entry id is global, so an id
	// alone would let a caller delete out of a space it was never authorized for.
	// An entry that is not in space is ErrNotFound and nothing is deleted.
	//
	// A nil error means the entry is gone from that space — whether this call
	// removed it or found it already removed. Deleting twice is not an error,
	// because the caller that needs this is undoing a write it just made and
	// "already gone" is the outcome it wanted.
	//
	// A non-nil error means the entry is still there. There is no third case:
	// the store commits or it returns, so a write's outcome is never unknown.
	// It was, while lore was a subprocess — a lost response left a tombstone
	// that may or may not have landed — and this interface used to carry an
	// ErrWriteUncertain for it.
	Delete(ctx context.Context, space domain.SpaceID, entryID string) error
	Close() error
}
