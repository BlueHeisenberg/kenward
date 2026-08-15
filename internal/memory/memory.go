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

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Entry is a stored piece of knowledge as kenward sees it.
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
	Markers    []string
}

// SearchQuery asks for entries from an explicit set of spaces.
//
// Spaces is required. There is deliberately no "search everything" mode: the space set
// comes from a resolved domain.Scope, so a code path that has not made an authorization
// decision cannot retrieve anything.
type SearchQuery struct {
	Text   string
	Spaces []domain.SpaceID
	Domain string
	Limit  int
}

// ErrEmptySpaceSet is returned when a search is attempted without an explicit space
// set. It is a programming error, not a user-facing condition.
var ErrEmptySpaceSet = errors.New("memory: search requires an explicit space set")

// ErrNotFound is returned by Get and Share for an unknown entry id.
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
	Close() error
}
