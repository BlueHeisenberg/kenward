// Package session holds unwrapped space keys in the memory of the process that was
// given the passphrase, and nowhere else.
//
// A model must see plaintext to answer, so a server-side assistant cannot be
// end-to-end encrypted with respect to its own machine. What this package buys is
// narrower and still worth having: keys are never written to disk, and are zeroed on
// Lock, on LockAll and on shutdown — so a backup, a stolen disk, or a process nobody
// has unlocked yields nothing.
//
// It does not claim more than that. An unlocked key stays in memory while its process
// runs, because D-019 rules out the only re-unlock path a member has: a passphrase is
// never sent over Telegram. Idle expiry exists here as a knob a household can turn on
// (see DefaultIdleTimeout) and is off by default, because expiring a key nobody can
// replace from a chat does not protect a member, it strands them.
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
// Implementations must zero key bytes on Lock and LockAll and must never persist an
// unwrapped key. Expiring idle sessions is optional and off unless configured; an
// implementation that never expires anything is conforming, not incomplete.
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
