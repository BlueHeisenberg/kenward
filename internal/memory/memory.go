// Package memory is kenward's view of the knowledge store.
//
// kenward owns no knowledge model. This package defines the narrow surface kenward
// needs and adapts lore to it; lore's spaces, entries, markers, confidence and origin
// fields are taken as given.
package memory

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

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
	// Partial reports that this is a search excerpt rather than a whole entry:
	// Body may be elided in the middle, and Origin, CreatedAt and UpdatedAt are
	// absent because lore's search does not return them.
	//
	// It is a field rather than something a caller infers, because the consequence
	// of getting it wrong is invisible. Anything rendering an entry into a prompt
	// must say which it has — a model shown a fragment under a heading claiming
	// completeness will answer confidently from the part it can see, and neither it
	// nor the member has any way to know something was cut out.
	Partial bool
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
// lore's search is a conjunctive full-text match over bare words, and this is what it
// does to a query, measured against a real store rather than inferred:
//
//   - Everything that is not a letter or a digit is a separator, so "boiler, service",
//     "boiler service" and `"boiler service"` are the same query.
//   - It is case-insensitive.
//   - There are no operators. "boiler AND service" finds nothing, because "and" is
//     just another word every entry must contain; "boiler OR service" likewise. The
//     trailing "*" of "boiler*" is stripped rather than honoured — it finds what
//     "boiler" finds, and "boil*" finds nothing at all. There is no prefix matching.
//   - There is no stemming. An entry saying "service" is not found by "servicing".
//   - There are no stopwords, and no minimum term length that matters here: "is" and
//     "the" match entries containing them, and rule out entries that do not.
//   - Every term must be present. One absent word — "what", in "what is the boiler
//     service code" — excludes an entry that holds the answer.
//
// Terms exists so that a caller can reason in the units lore counts in. Matching a
// whole word rather than a substring is part of that: lore tokenises
// "quillfeather921834100" as one word, so "quillfeather" does not find it.
func Terms(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
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
