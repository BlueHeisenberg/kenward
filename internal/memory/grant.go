package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/BlueHeisenberg/lore"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Membership across two lore stores, which is what isolated mode needs and simple
// mode has no use for.
//
// One lore home is one lore account, so an isolated household of pods is three
// accounts with three id spaces and the single `household.shared_space` in
// kenward.yaml can only ever have been created in one of them. Creating it again in
// the others would make three different spaces that happen to share an id and can
// never converge: a space's key is generated where it is created, peers intersect
// `HMAC(space_key, "lore-blind"||space_id)`, and three keys are three blinded ids.
// The space has to be carried — one store's, admitted into the others.
//
// lore exposes that as three calls (its grant.go), and this file is kenward's view of
// them. What travels between the two stores is a public identity one way and an opaque
// grant the other, and internal/link is what carries them.

// Identity is the public half of a lore store's account identity: what the owner of a
// space needs in order to admit that store, and nothing else. No private key, no space
// key, no entry. It is safe to send over the household's container network in the
// clear — every value in it is already on the wire in each sync handshake the store
// performs.
type Identity struct {
	AccountID string
	EncPub    string
	EncPubSig string
}

// Identity returns this store's own public identity.
func (c *Client) Identity(context.Context) (Identity, error) {
	id, err := c.store.PublicIdentity()
	if err != nil {
		return Identity{}, fmt.Errorf("memory: reading this store's public identity: %w", mapErr(err))
	}
	return Identity{AccountID: id.AccountID, EncPub: id.EncPub, EncPubSig: id.EncPubSig}, nil
}

// Grant admits another store into a shared space this one owns, and returns the opaque
// grant that store applies with AcceptGrant.
//
// The role is always writer and is not a parameter. A household member reads the
// household's memory and adds to it; there is no configuration in which one of them is
// admitted read-only, and offering the choice here would be a setting nothing sets.
//
// It is idempotent: a store already in the member list is not admitted twice, and the
// current member list is re-sealed for it instead. So a caller may run it every time it
// is asked, without keeping any record of who it has already admitted.
//
// Only the space's owner can do this — the pod that created it — and only for a shared
// space. Anything else is a refusal from lore, not a silent no-op.
func (c *Client) Grant(ctx context.Context, space domain.SpaceID, to Identity) ([]byte, error) {
	id := strings.TrimSpace(string(space))
	if id == "" {
		return nil, fmt.Errorf("memory: a grant needs a space: %w", ErrInvalidArgument)
	}
	grant, err := c.store.GrantMembership(ctx, id, lore.PublicIdentity{
		AccountID: to.AccountID, EncPub: to.EncPub, EncPubSig: to.EncPubSig,
	}, lore.Writer)
	if err != nil {
		return nil, fmt.Errorf("memory: granting membership of space %s: %w", id, mapErr(err))
	}
	return grant, nil
}

// AcceptGrant applies a grant made for this store and returns the space it joined.
//
// Everything it takes on faith it verifies: lore refuses a grant that does not open
// with this store's own key, whose signed member list does not verify, or whose latest
// version does not name this account. A grant addressed to a sibling is inert here.
//
// The caller must still check that the space it returns is the space it was expecting.
// A grant is proof that somebody who owns *a* space admitted this store to it; that the
// space is the household's is kenward's question, and internal/link is where it is
// asked.
func (c *Client) AcceptGrant(ctx context.Context, grant []byte) (Space, error) {
	sp, err := c.store.AcceptMembership(ctx, grant)
	if err != nil {
		return Space{}, fmt.Errorf("memory: accepting a space grant: %w", mapErr(err))
	}
	return Space{ID: sp.ID, Name: sp.Name, Kind: string(sp.Kind)}, nil
}

// HasSpace reports whether this store holds a space. Locally present is lore's
// membership check: a space this store does not hold was never carried here.
func (c *Client) HasSpace(ctx context.Context, space domain.SpaceID) (bool, error) {
	id := strings.TrimSpace(string(space))
	if id == "" {
		return false, fmt.Errorf("memory: a space is required: %w", ErrInvalidArgument)
	}
	switch _, err := c.store.GetSpace(ctx, id); {
	case err == nil:
		return true, nil
	case errors.Is(err, lore.ErrSpaceNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("memory: looking up space %s: %w", id, mapErr(err))
	}
}
