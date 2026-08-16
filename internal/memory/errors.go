package memory

import (
	"errors"
	"fmt"

	"github.com/BlueHeisenberg/lore"
)

// The sentinels callers branch on.
//
// They are kenward's vocabulary, not lore's, and they stay kenward's: mapErr
// translates, so a rename inside lore is a compile error here rather than a
// silent change of meaning at thirty call sites. There is no prose in this file
// and no classification of prose — lore's Go API returns typed errors, and the
// parser that used to recover these from human-readable text is gone.
var (
	// ErrClosed is returned by every method once Close has been called.
	ErrClosed = errors.New("memory: client is closed")

	// ErrInvalidArgument is returned when a call is rejected before it reaches
	// lore, or by lore for an argument it cannot form a write from. It is a
	// programming error, not a user-facing condition.
	ErrInvalidArgument = errors.New("memory: invalid argument")

	// ErrUnknownSpace is returned when lore does not hold the space that was
	// named. A kenward deployment reaching this has a configuration fault: the
	// space id in the household configuration does not exist in this lore home.
	ErrUnknownSpace = errors.New("memory: unknown lore space")

	// ErrUserModel is returned when a share is refused because the entry lives in
	// lore's user-model domains (profile/, feedback/), which never leave the
	// personal space on any path.
	ErrUserModel = errors.New("memory: user-model entry cannot leave its space")

	// ErrNotWriter is returned when this lore account holds only the reader role
	// in the target space and so may not author into it.
	ErrNotWriter = errors.New("memory: not a writer of the target space")

	// ErrSpaceExists is returned when CreateSpace is given a name another space
	// already has. Nothing was created.
	//
	// It is a state conflict rather than a programming error, and it is typed
	// because the only sound answer is to ask a person for another name.
	// Reusing the existing space instead — a get-or-create — is how one member's
	// private memory becomes another member's.
	ErrSpaceExists = errors.New("memory: a lore space with that name already exists")

	// ErrBusy is returned when lore's SQLite store was held by another process
	// past its retry budget. lore opens the database with WAL, a single
	// connection and a five second busy timeout, and retries a contended call
	// itself before reporting this — so a call that returns it did nothing and
	// may be retried.
	//
	// Contention is not hypothetical and did not go away with the subprocess:
	// `lore serve` and any `lore` command the operator runs open the same file.
	ErrBusy = errors.New("memory: lore store is busy")

	// ErrStoreUnavailable is returned when the lore home is not usable at all:
	// it was never initialised (`lore init`), or its database was written by a
	// newer lore than this build and opening it would risk misreading the
	// schema. Unlike ErrBusy it does not clear itself on a retry — it is an
	// operator fault, and it is typed so that it can be reported as one.
	ErrStoreUnavailable = errors.New("memory: lore store is unavailable")
)

// mapErr translates lore's error contract into this package's.
//
// Anything unrecognised is returned as it is. Inventing a sentinel for a failure
// this build does not know about would be a worse lie than an opaque error, and
// the same rule the parser followed for unrecognised prose.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil

	// An entry in another space is reported as absent rather than as located
	// elsewhere. Callers hold a space they are entitled to and an id they may
	// not be; "not here" is the whole of what they are owed, and every caller
	// already branches on ErrNotFound.
	case errors.Is(err, lore.ErrNotFound), errors.Is(err, lore.ErrWrongSpace):
		return ErrNotFound
	case errors.Is(err, lore.ErrSpaceNotFound):
		return ErrUnknownSpace
	case errors.Is(err, lore.ErrUserModel):
		return ErrUserModel
	case errors.Is(err, lore.ErrNotWriter):
		return ErrNotWriter
	case errors.Is(err, lore.ErrSpaceExists):
		return ErrSpaceExists
	case errors.Is(err, lore.ErrBusy):
		return ErrBusy
	case errors.Is(err, lore.ErrClosed):
		return ErrClosed
	case errors.Is(err, lore.ErrInvalidArgument):
		return ErrInvalidArgument

	// Three different ways for the home itself to be unusable, and one answer:
	// an operator has to do something, and no retry will help. lore's own words
	// are kept here and nowhere else, because they name the file and the path
	// that has to be fixed and this is the one failure a person acts on.
	//
	// Keeping them is safe in a way it was not when this package classified
	// prose: lore's error contract states that its errors carry a space or entry
	// id where that helps and never an entry's title or body. That promise is
	// what makes a lore error loggable at all.
	case errors.Is(err, lore.ErrNoAccount),
		errors.Is(err, lore.ErrSchemaTooNew),
		errors.Is(err, lore.ErrReadOnly):
		return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	return err
}
