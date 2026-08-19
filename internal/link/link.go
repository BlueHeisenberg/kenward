// Package link carries the household's shared lore space from the group's pod
// into each member's pod, with nobody running anything.
//
// # The defect
//
// Isolated mode gives every pod its own lore home on its own volume, which is the
// whole point of the mode and also the reason `household.shared_space` could only
// ever be real in one of them. A lore home is a lore account; a space belongs to
// the account that created it; and the only thing that carries a space into a
// second account was `lore space invite` in one pod and `lore join` in the other,
// typed by a person, once per member, forever. Until that was done a member's
// assistant could neither read the household's memory nor write to it, and the
// only thing that said so was `kenward doctor`.
//
// The private half was never affected and is not touched here: a member's private
// space is created inside that member's own pod, with a key that never leaves it.
//
// # Why kenward may do this and lore may not
//
// lore withholds membership from its Go API for the standalone user, and rightly:
// a program must not be able to join one person's memory to another's. What is
// different here is that the decision has already been taken by a person, and
// written down. An administrator added a member to `kenward.yaml`. That IS the
// consent, and it is recorded in the one file every unit of the household reads.
// Making the member prove it again by typing into a container was not a safety
// property; it was an unfinished implementation.
//
// So lore now exposes the two halves of an admission that a store's own owner can
// perform on its own space — grant and accept — and this package is what puts the
// two pods in touch.
//
// # The one secret, and what it is for
//
// The group's pod must not admit *anybody* who asks. On a container bridge that is
// not a theoretical worry: podman's default network is shared with every other
// container the same user runs, and a compose project's network is joinable. So a
// requester proves it holds `household.link_key` — the one secret every unit of an
// isolated household shares, provisioned exactly like a bot token.
//
// Sharing a secret between pods is the opposite of what every other secret here
// does, so it is worth being exact about what it can buy. Holding it grants one
// thing: admission to the household's shared space, which every unit of the
// household already has by configuration. It does not reach a member's private
// space — that key is generated inside that member's pod and is not in any
// grant — and it is not a member's passphrase, a bot token or a session key. The
// blast radius is the set of things it is shared among, which is what makes it not
// a widening.
//
// # The exchange
//
// Both directions are authenticated with the key, and that matters in both
// directions. The request is MACed so the group's pod knows the asker belongs to
// this household. The RESPONSE is MACed so a member's pod cannot be handed a grant
// by an impostor — an attacker who creates a space of their own at the household's
// configured id could otherwise divert a member's assistant into reading and
// writing somebody else's memory, since `AcceptMembership` overwrites a space it
// is given.
//
// Nothing confidential travels in the clear, so there is no TLS here. A request
// carries public keys; a response carries a grant sealed to the asker's encryption
// key, which is inert to everybody else. An eavesdropper on the bridge learns
// account ids that lore's own sync hello already puts on the same wire.
//
// # Discovery
//
// mDNS, the same mechanism lore's sync daemon uses on the same network for the
// same reason: a pod's address is not knowable when its spec is built and changes
// on every recreation, so anything written down would be stale by the first
// rolling update. The group's pod advertises; each member's pod browses. That
// works identically under the supervisor and under compose, which is what keeps
// the two deployment paths from needing two answers.
package link

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// Service is the mDNS service type the group's pod advertises its desk on. It is
// kenward's own and deliberately not lore's: lore's `_lore._tcp` is the sync
// daemon's, and a browser looking for one must never find the other.
const Service = "_kenward-link._tcp"

// Path is the one route the desk serves.
const Path = "/kenward/link/v1/grant"

// MinKeyLen is the shortest household link key that will be accepted. Thirty-two
// bytes is what `kenward setup` writes; the floor is lower so that a hand-written
// passphrase is usable, and non-zero so that an empty variable is a refusal rather
// than a household whose gate opens for the empty string.
const MinKeyLen = 16

// Defaults for Options.
const (
	// DefaultRetry is how long a member's pod waits between attempts once it has
	// failed one. It is short because the usual reason to fail is that the group's
	// pod has not finished starting.
	DefaultRetry = 15 * time.Second
	// DefaultBrowse bounds one mDNS browse. lore uses six seconds for the same
	// question.
	DefaultBrowse = 6 * time.Second
	// requestTimeout bounds one call to the desk. The desk does local SQLite work
	// and no network of its own, so this is generous rather than tight.
	requestTimeout = 30 * time.Second
	// maxBody bounds what either side will read from the other.
	maxBody = 1 << 20
)

// MAC domain separators. Two of them, so a response can never be replayed as a
// request or the other way round.
const (
	labelRequest = "kenward-link-request-v1"
	labelGrant   = "kenward-link-grant-v1"
)

// Memory is the part of internal/memory this package needs. It is an interface so
// the exchange can be tested without two lore homes, and narrow so that nothing
// here can reach an entry: this package moves membership and never knowledge.
type Memory interface {
	Identity(ctx context.Context) (memory.Identity, error)
	Grant(ctx context.Context, space domain.SpaceID, to memory.Identity) ([]byte, error)
	AcceptGrant(ctx context.Context, grant []byte) (memory.Space, error)
	HasSpace(ctx context.Context, space domain.SpaceID) (bool, error)
}

// Options configure both halves. Space, Key and Memory are required.
type Options struct {
	// Space is household.shared_space: the space the group's pod owns and each
	// member's pod is to be admitted to.
	Space domain.SpaceID
	// Key is the household link key.
	Key []byte
	// Memory is this pod's own lore store.
	Memory Memory
	// Logger receives one line per outcome. Nil discards.
	Logger *slog.Logger

	// Retry is the interval between a member pod's attempts. Zero means
	// DefaultRetry.
	Retry time.Duration
	// Browse bounds one mDNS browse. Zero means DefaultBrowse.
	Browse time.Duration
	// Port fixes the desk's listening port. Zero takes an ephemeral one, which is
	// what a pod wants: the port travels in the mDNS advertisement and nothing
	// writes it down.
	Port int
	// LoopbackOnly keeps the desk and the browse off every interface but loopback.
	// A pod's siblings are not on its loopback, so nothing in kenward sets it; the
	// tests do, because two halves in one process are on each other's loopback
	// already and a 0.0.0.0 bind buys a firewall prompt and nothing else.
	LoopbackOnly bool
	// Addrs, when set, replaces mDNS discovery with a fixed list of host:port. For
	// a test that would rather not depend on multicast.
	Addrs []string
	// NoDiscovery keeps the desk off mDNS. Nothing in kenward sets it — a desk
	// nobody can find is a desk for nothing — and the tests do, because a test
	// binary must not advertise a household on the developer's LAN.
	NoDiscovery bool
}

func (o Options) check() error {
	switch {
	case strings.TrimSpace(string(o.Space)) == "":
		return errors.New("link: a shared space is required")
	case len(o.Key) < MinKeyLen:
		return fmt.Errorf("link: the household link key is %d bytes; at least %d are required", len(o.Key), MinKeyLen)
	case o.Memory == nil:
		return errors.New("link: a memory store is required")
	}
	return nil
}

func (o Options) log() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (o Options) retry() time.Duration {
	if o.Retry > 0 {
		return o.Retry
	}
	return DefaultRetry
}

func (o Options) browse() time.Duration {
	if o.Browse > 0 {
		return o.Browse
	}
	return DefaultBrowse
}

// -----------------------------------------------------------------------------
// wire
// -----------------------------------------------------------------------------

// request is what a member's pod asks the desk for. Everything in it is public
// except the MAC, which is not a secret either — it is a proof that the sender
// holds one.
type request struct {
	Space     string `json:"space"`
	AccountID string `json:"account_id"`
	EncPub    string `json:"enc_pub"`
	EncPubSig string `json:"enc_pub_sig"`
	Nonce     string `json:"nonce"`
	MAC       string `json:"mac"`
}

// response is the grant, and a MAC over it bound to the request's nonce.
type response struct {
	Grant string `json:"grant"`
	MAC   string `json:"mac"`
}

// mac computes one of this protocol's two MACs. The parts are length-prefixed so
// no two different field splits can produce one input.
func mac(key []byte, label string, parts ...string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(label))
	for _, p := range parts {
		fmt.Fprintf(h, "|%d|", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r request) expectedMAC(key []byte) string {
	return mac(key, labelRequest, r.Space, r.AccountID, r.EncPub, r.EncPubSig, r.Nonce)
}

func grantMAC(key []byte, space, account, nonce, grant string) string {
	return mac(key, labelGrant, space, account, nonce, grant)
}

// equalMAC compares two hex MACs in constant time. A malformed one never matches.
func equalMAC(a, b string) bool {
	x, err := hex.DecodeString(a)
	if err != nil {
		return false
	}
	y, err := hex.DecodeString(b)
	if err != nil {
		return false
	}
	return hmac.Equal(x, y)
}
