package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/BlueHeisenberg/keel/vault"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Store errors. Neither carries key material; both are safe to log — but note
// that Unlock deliberately never surfaces ErrUnknownMember, folding it into
// ErrBadPassphrase so probing for members reveals nothing.
var (
	// ErrUnknownMember is returned by Store.Load when no wrapped key has been
	// provisioned for the member.
	ErrUnknownMember = errors.New("session: no wrapped key for member")

	// ErrDuplicateMember is returned by Store.Save when the member already has
	// a wrapped key. Replacing a wrapped key silently would discard the only
	// path to the space key it protects, so a duplicate is always an error,
	// never an overwrite.
	ErrDuplicateMember = errors.New("session: member already has a wrapped key")
)

// Record is the persisted wrapped-key material for one member: the KDF salt
// and parameters, the member's data-encryption key wrapped under the
// passphrase-derived key, and the member's space key sealed under that
// data-encryption key. Nothing in it is secret in the clear — it is exactly
// what an offline attacker is assumed to hold — but anyone who can replace it
// can deny access, so it gets the same write protection as data.
//
// Record deliberately carries no member id. The identity a key is bound to
// flows from the caller of Unlock — it is the AAD under which SealedKey was
// sealed — and a stored record must never be able to supply its own identity:
// a record that could name itself would reopen the ciphertext-relocation hole
// the AAD exists to close.
type Record struct {
	// Salt is the KDF salt for this member's key-encryption key.
	Salt []byte `json:"salt"`
	// Params is the JSON-encoded KDF configuration, opaque to this package;
	// keel/vault wrote it and keel/vault interprets it.
	Params json.RawMessage `json:"params"`
	// WrappedDEK is the member's data-encryption key wrapped under the
	// passphrase-derived key, in keel/vault's envelope format.
	WrappedDEK []byte `json:"wrappedDEK"`
	// SealedKey is the member's space key sealed under the data-encryption
	// key, with the member's identity bound in as AAD.
	SealedKey []byte `json:"sealedKey"`
}

// validate rejects a record with any field missing. A partial record can only
// ever produce ErrBadPassphrase at unlock time, which would read as a wrong
// passphrase forever; better to refuse it at write time.
func (r Record) validate() error {
	switch {
	case len(r.Salt) == 0:
		return errors.New("session: record has no salt")
	case len(r.Params) == 0:
		return errors.New("session: record has no kdf parameters")
	case len(r.WrappedDEK) == 0:
		return errors.New("session: record has no wrapped key")
	case len(r.SealedKey) == 0:
		return errors.New("session: record has no sealed space key")
	}
	return nil
}

// keyRecord adapts the persisted fields to keel/vault's shape.
func (r Record) keyRecord() vault.KeyRecord {
	return vault.KeyRecord{ID: "default", Salt: r.Salt, Params: []byte(r.Params), WrappedKey: r.WrappedDEK}
}

// clone returns a deep copy, so a caller mutating the returned slices cannot
// reach back into a store's internals.
func (r Record) clone() Record {
	return Record{
		Salt:       append([]byte(nil), r.Salt...),
		Params:     append(json.RawMessage(nil), r.Params...),
		WrappedDEK: append([]byte(nil), r.WrappedDEK...),
		SealedKey:  append([]byte(nil), r.SealedKey...),
	}
}

// validateSave applies the checks shared by every Store implementation.
func validateSave(id domain.MemberID, r Record) error {
	if id == "" {
		return errors.New("session: empty member id")
	}
	return r.validate()
}

// Store persists wrapped keys, one Record per member.
//
// Implementations hold wrapped material only — never an unwrapped key, never
// a passphrase. There is no database behind this seam and there is not meant
// to be: a household enrols a handful of members in its lifetime, and a file
// is the right size of mechanism.
type Store interface {
	// Save records a newly provisioned wrapped key. It fails with
	// ErrDuplicateMember if the member already has one; replacement is not a
	// thing this interface offers.
	Save(ctx context.Context, id domain.MemberID, r Record) error

	// Load returns the member's wrapped key, or ErrUnknownMember.
	Load(ctx context.Context, id domain.MemberID) (Record, error)

	// List returns the ids of every member holding a wrapped key, in no
	// particular order.
	List(ctx context.Context) ([]domain.MemberID, error)
}

// MemStore is a Store that keeps records in memory only.
//
// It is for tests and for nothing else in production: wrapped keys that
// vanish on restart would lock every member out. The zero value is not
// usable; call NewMemStore.
type MemStore struct {
	mu   sync.Mutex
	recs map[domain.MemberID]Record
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{recs: make(map[domain.MemberID]Record)}
}

// Save records a newly provisioned wrapped key.
func (s *MemStore) Save(ctx context.Context, id domain.MemberID, r Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSave(id, r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[id]; ok {
		return ErrDuplicateMember
	}
	s.recs[id] = r.clone()
	return nil
}

// Load returns the member's wrapped key, or ErrUnknownMember.
func (s *MemStore) Load(ctx context.Context, id domain.MemberID) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	if !ok {
		return Record{}, ErrUnknownMember
	}
	return r.clone(), nil
}

// List returns the ids of every member holding a wrapped key.
func (s *MemStore) List(ctx context.Context) ([]domain.MemberID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]domain.MemberID, 0, len(s.recs))
	for id := range s.recs {
		ids = append(ids, id)
	}
	return ids, nil
}
