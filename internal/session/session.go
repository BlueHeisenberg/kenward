// Package session holds unwrapped space keys in memory for as long as a member is
// actively talking, and no longer.
//
// A model must see plaintext to answer, so a server-side assistant cannot be
// end-to-end encrypted with respect to its own machine. What this package buys is
// narrower and still worth having: keys are never written to disk, are zeroed when a
// session ends, and are absent entirely while a member is away — so a backup, a stolen
// disk or an idle machine yields nothing.
package session

import (
	"context"
	"errors"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// ErrLocked is returned when a member's key is not currently unwrapped.
var ErrLocked = errors.New("session: locked")

// ErrBadPassphrase is returned when unwrapping fails. It is deliberately
// indistinguishable from a missing wrapped key, so probing reveals nothing.
var ErrBadPassphrase = errors.New("session: could not unlock")

// Sessions manages unwrapped key material.
//
// Implementations must zero key bytes on Lock and LockAll, must never persist an
// unwrapped key, and must expire idle sessions on their own schedule.
type Sessions interface {
	// Unlock derives and unwraps the member's key. It is safe to call repeatedly.
	Unlock(ctx context.Context, id domain.MemberID, passphrase string) error
	// Key returns the unwrapped key if the member has an active session.
	Key(id domain.MemberID) ([]byte, bool)
	// Touch resets the idle timer.
	Touch(id domain.MemberID)
	// Lock zeroes and forgets one member's key.
	Lock(id domain.MemberID)
	// LockAll zeroes everything. Called on shutdown.
	LockAll()
}
