package enrol

import (
	"context"
	"sync"
	"time"
)

// Store persists minted claim codes.
//
// Implementations hold digests, never plaintext: a code exists in the clear exactly
// once, in the operator's terminal, and is never recoverable afterwards.
//
// There is no database behind this seam and there is not meant to be. A household
// mints a handful of codes in its lifetime; a file is the right size of mechanism.
type Store interface {
	// Save records a newly minted code. It fails if the digest is already present.
	Save(ctx context.Context, c Code) error

	// Consume atomically finds a live code by digest and marks it used.
	//
	// Atomically is the whole point: the find, the expiry and consumed checks, and
	// the write that marks it used happen under one lock, so two simultaneous
	// redemptions of the same code cannot both come back with a Code. Exactly one
	// caller gets the code; the other gets ErrCodeConsumed.
	//
	// The returned Code is the record as it now stands, with ConsumedAt set. On any
	// failure it returns ErrUnknownCode, ErrCodeExpired or ErrCodeConsumed and
	// leaves the store untouched.
	Consume(ctx context.Context, hash string, now time.Time) (Code, error)

	// Purge drops codes that expired or were consumed before the given time. It is
	// housekeeping only; an expired code is already unredeemable.
	Purge(ctx context.Context, before time.Time) error
}

// consume applies the redemption rules to an in-memory slice of codes.
//
// It is shared by every Store implementation so that the ordering of the checks,
// and the constant-time digest comparison, cannot drift between them. The scan
// visits every record without an early exit, so the time it takes does not depend
// on where in the file a matching code happens to sit.
func consume(codes []Code, digest string, now time.Time) (int, Code, error) {
	idx := -1
	for i := range codes {
		if EqualHash(codes[i].Hash, digest) {
			idx = i
		}
	}
	if idx < 0 {
		return -1, Code{}, ErrUnknownCode
	}
	c := codes[idx]
	if c.Consumed() {
		return -1, Code{}, ErrCodeConsumed
	}
	if c.Expired(now) {
		return -1, Code{}, ErrCodeExpired
	}
	c.ConsumedAt = now
	return idx, c, nil
}

// purge returns the codes worth keeping: those neither consumed before the cutoff
// nor already expired as of it.
func purge(codes []Code, before time.Time) []Code {
	kept := make([]Code, 0, len(codes))
	for _, c := range codes {
		if c.Consumed() && c.ConsumedAt.Before(before) {
			continue
		}
		if c.Expired(before) {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// MemStore is a Store that keeps codes in memory only.
//
// It is for tests and for a deployment that would rather re-mint after a restart
// than keep digests on disk. The zero value is not usable; call NewMemStore.
type MemStore struct {
	mu    sync.Mutex
	codes []Code
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{} }

// Save records a newly minted code.
func (s *MemStore) Save(ctx context.Context, c Code) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.codes {
		if EqualHash(s.codes[i].Hash, c.Hash) {
			return ErrDuplicateCode
		}
	}
	s.codes = append(s.codes, c)
	return nil
}

// Consume atomically redeems a code by digest.
func (s *MemStore) Consume(ctx context.Context, digest string, now time.Time) (Code, error) {
	if err := ctx.Err(); err != nil {
		return Code{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, c, err := consume(s.codes, digest, now)
	if err != nil {
		return Code{}, err
	}
	s.codes[idx] = c
	return c, nil
}

// Purge drops codes finished with before the given time.
func (s *MemStore) Purge(ctx context.Context, before time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes = purge(s.codes, before)
	return nil
}

// Len reports how many code records the store holds, consumed ones included.
func (s *MemStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.codes)
}
