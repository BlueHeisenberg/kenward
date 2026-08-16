package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Binder errors. Every one of them means the household is unchanged: nothing was
// written to the state file and nothing in memory moved.
var (
	// ErrNoConfig is returned by NewBinder when no configuration was supplied.
	ErrNoConfig = errors.New("config: no configuration")
	// ErrNoMemberID is returned by Bind when asked to bind the empty member id.
	ErrNoMemberID = errors.New("config: no member id")
	// ErrNoMemberName is returned when a member the configuration does not declare
	// would have to be created and the invite names nobody.
	ErrNoMemberName = errors.New("config: creating a member needs a name")
	// ErrNoTelegramID is returned by Bind for a zero Telegram id. Zero is how an
	// unclaimed member is written down, so binding it would record an enrolment that
	// every lookup in the module reads as "nobody".
	ErrNoTelegramID = errors.New("config: no telegram id")
	// ErrTelegramIDBound is returned when the Telegram id already belongs to a
	// different member. It is never resolved by moving the binding.
	ErrTelegramIDBound = errors.New("config: telegram id is already bound to another member")
	// ErrNoProvisioning is returned when a claim would create a member and the Binder
	// was built without a Provisioning to create them with. See Provisioning.
	ErrNoProvisioning = errors.New("config: no provisioning; a member the configuration does not declare cannot be created")
	// ErrPrivateSpaceTaken is returned when the space a created member would be given
	// is already somebody's. Two members sharing a private space is not a private
	// space, so the claim is refused rather than the space reused.
	ErrPrivateSpaceTaken = errors.New("config: private space is already taken")
)

// ErrUnknownMember is returned by Unbind for a member the Binder does not hold.
//
// It is deliberately not a plain errors.New. The enrolment package declares a sentinel
// of its own with this exact meaning and tests a Binder's error against it with
// errors.Is; two distinct errors.New values never match, and importing that package to
// borrow its value would invert the dependency order — configuration sits below
// enrolment and knows nothing about claim codes. So this value matches structurally,
// by the one thing the two packages agree on: the sentinel's text.
var ErrUnknownMember error = unknownMember{}

// enrolUnknownMember is the text of the enrolment package's own sentinel. Changing it
// there without changing it here breaks the match, which is why the enrolment side is
// tested from this package's tests rather than assumed.
const enrolUnknownMember = "enrol: unknown member"

type unknownMember struct{}

func (unknownMember) Error() string { return "config: unknown member" }

// Is matches the enrolment package's sentinel as well as this one, so a caller there
// can recognise what came back without either package importing the other.
func (unknownMember) Is(target error) bool {
	return target != nil && target.Error() == enrolUnknownMember
}

// Provisioning is what a member created by a claim is given.
//
// It exists because creating a member means the configuration gains someone nobody
// declared, and the two things that makes them — the space their private memory lives
// in and the tier chain their conversations may use — are decisions an operator makes,
// not defaults a claim can invent.
//
// The tier chain in particular has no default and must not acquire one. Validation
// already refuses a member whose chain is unstated, for the reason spelled out there:
// defaulting it to the household's chain would widen a member's privacy policy without
// anyone saying so, and leaving it empty would produce a member who parses, starts and
// then refuses every turn for a reason nobody can see. A member conjured by a claim is
// exactly the case where nobody is watching, so a Binder with no Provisioning refuses
// to create members at all and says so.
type Provisioning struct {
	// Tiers is the ordered tier chain a created member's private conversations may
	// use. Required: a zero Provisioning creates nobody.
	Tiers []string
}

func (p Provisioning) clone() Provisioning {
	return Provisioning{Tiers: copyStrings(p.Tiers)}
}

// Binder attaches Telegram accounts to household members and records what it did in
// the state file.
//
// It is the other half of enrolment: that package decides whether a claim is
// legitimate, this one decides what the household looks like afterwards. The interface
// it satisfies is declared over there and is satisfied structurally — nothing here
// imports it. Configuration is below enrolment in the dependency order, and an import
// would invert that for no gain: Go does not need one.
//
// It never touches the *Config it was built from. That configuration is read
// concurrently, without a lock, by everything else in the process, and the supervisor
// already folds a completed claim into its own snapshot copy-on-write from the
// domain.Member returned here. So the Binder keeps its own member set and the caller's
// Config stays exactly as it was loaded.
//
// What it cannot do is make a created member permanent. The state file holds bindings
// and nothing else — deliberately, because a member's name, space and tier chain are
// configuration and configuration is the operator's file, not kenward's to rewrite. A
// member created by a claim therefore lives for as long as this process does, and the
// operator has to write them into kenward.yaml for them to survive a restart. The same
// is true of a per-member bot token in isolated mode: a created member has none, so
// creation is a simple-mode path in practice.
//
// Safe for concurrent use.
type Binder struct {
	mu      sync.Mutex
	members map[domain.MemberID]domain.Member
	// order is the member ids in configuration order, with created members appended.
	// Every scan walks it rather than the map so an answer never depends on map
	// iteration order.
	order  []domain.MemberID
	shared domain.SpaceID
	prov   Provisioning
	state  *State
	path   string
}

// NewBinder returns a Binder over cfg's members and cfg's state file, with p as the
// policy for members a claim has to create. Pass the zero Provisioning to refuse
// creation.
//
// It reads the state file once, here, and folds the recorded bindings into its own
// member set the way MergeState folds them into a configuration — including the
// disagreement MergeState refuses to resolve: a member whose telegram_id in the file
// is not the one the state records is reported as a *ValidationError rather than
// silently decided one way or the other. A configuration that came from Load has
// already been through that and folds again to the same answer.
func NewBinder(cfg *Config, p Provisioning) (*Binder, error) {
	if cfg == nil {
		return nil, ErrNoConfig
	}
	path := cfg.StatePath()
	st, err := LoadState(path)
	if err != nil {
		return nil, err
	}

	b := &Binder{
		members: make(map[domain.MemberID]domain.Member, len(cfg.Members)),
		shared:  domain.SpaceID(cfg.Household.SharedSpace),
		prov:    p.clone(),
		state:   st,
		path:    path,
	}

	probs := &problems{}
	for _, t := range b.prov.Tiers {
		if strings.TrimSpace(t) == "" {
			probs.addf("provisioning.tiers: contains an empty tier name")
		}
	}
	for i, m := range cfg.DomainMembers() {
		switch {
		case m.ID == "":
			probs.addf("members[%d].id: required; a member with no id cannot be bound", i)
			continue
		default:
			if _, dup := b.members[m.ID]; dup {
				probs.addf("members[%d].id: duplicate member id %q", i, m.ID)
				continue
			}
		}
		if bd, ok := st.Binding(m.ID); ok {
			switch {
			case m.TelegramID == 0:
				m.TelegramID = bd.TelegramID
			case m.TelegramID != bd.TelegramID:
				probs.addf("members[%d].telegram_id: the file says %d but %s records %d for member %q; one of them is wrong and kenward will not choose for you",
					i, m.TelegramID, StateFileName, bd.TelegramID, m.ID)
				continue
			}
			m.EnrolledAt = bd.EnrolledAt
		}
		b.members[m.ID] = m
		b.order = append(b.order, m.ID)
	}
	if len(probs.list) > 0 {
		return nil, &ValidationError{Problems: probs.list}
	}
	return b, nil
}

// Bind attaches a Telegram user id to a member and returns the member as it now
// stands, creating the member from the invited name if the configuration does not
// carry one. See Provisioning for what a created member gets and why it has to be
// stated.
//
// A Telegram id already bound to a different member is refused, never moved. One
// Telegram account cannot be two household members, and re-pointing it on a second
// claim would hand whoever made that claim the first member's private space, which is
// the whole of what enrolment protects.
//
// Rebinding the same member to a new Telegram account is allowed and is the point of
// keeping bindings by member id: a member who changes accounts stays the same person
// with the same memory. Rebinding a member to the account they already hold is a
// no-op, not an error — a retried claim must not fail, and their EnrolledAt keeps
// saying when they actually enrolled rather than when the network made them try again.
//
// The invited name is used only when the member is created. For a member the
// configuration declares, the configuration's name wins: an invite is a way in, not a
// way to rename somebody.
func (b *Binder) Bind(ctx context.Context, id domain.MemberID, name string, telegramID int64, at time.Time) (domain.Member, error) {
	if err := ctx.Err(); err != nil {
		return domain.Member{}, err
	}
	if id == "" {
		return domain.Member{}, ErrNoMemberID
	}
	if telegramID == 0 {
		return domain.Member{}, fmt.Errorf("%w: member %q", ErrNoTelegramID, id)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if other, ok := b.holderOf(telegramID); ok && other != id {
		return domain.Member{}, fmt.Errorf("%w: %d belongs to member %q, not %q", ErrTelegramIDBound, telegramID, other, id)
	}

	member, held := b.members[id]
	if !held {
		created, err := b.provision(id, name)
		if err != nil {
			return domain.Member{}, err
		}
		member = created
	} else if bd, ok := b.state.Binding(id); ok && bd.TelegramID == telegramID {
		// Already recorded, identically. Returning without a write also means a
		// retried claim cannot fail on a full disk.
		return memberCopy(member), nil
	}

	next := cloneState(b.state)
	// A binding for an id this Binder does not hold is inert — MergeState ignores a
	// binding whose member has been deleted from the configuration, and every lookup
	// downstream goes through the member list — but leaving it in place while binding
	// the same account to somebody else would write a file with one Telegram account
	// bound twice. Dropping it is bookkeeping, not a decision about who may talk.
	if stale, ok := next.MemberByTelegramID(telegramID); ok && stale != id {
		next.Unbind(stale)
	}
	next.Bind(id, telegramID, at)
	if err := next.Save(b.path); err != nil {
		// Nothing above this line touched b: the in-memory household is still the one
		// the file on disk describes.
		return domain.Member{}, fmt.Errorf("config: recording the binding for member %q: %w", id, err)
	}

	b.state = next
	member.TelegramID = telegramID
	member.EnrolledAt = at
	if !held {
		b.order = append(b.order, id)
	}
	b.members[id] = member
	return memberCopy(member), nil
}

// SetMemberPersona records what a member chose about their own agent in the Telegram
// tutorial, and the tutorial progress that goes with it.
//
// It is here rather than in a store of its own because this is the object that already
// owns the state file: one mutex, one clone-write-swap, one path, and no second writer
// racing the enrolment that produced the member in the first place. The tutorial runs
// immediately after a claim and writes after every question, so the two writers would
// otherwise be the same conversation half a second apart.
//
// A member this Binder does not hold is refused rather than recorded. A persona for
// nobody is inert — MergeState only walks the configured member list — but writing one
// would leave a file naming a person the household does not have, which is the kind of
// row somebody later has to decide what to do about.
func (b *Binder) SetMemberPersona(ctx context.Context, id domain.MemberID, p MemberPersona) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		return ErrNoMemberID
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, held := b.members[id]; !held {
		return fmt.Errorf("%w: %q", ErrUnknownMember, id)
	}
	next := cloneState(b.state)
	next.SetMemberPersona(id, p)
	if err := next.Save(b.path); err != nil {
		return fmt.Errorf("config: recording the persona for member %q: %w", id, err)
	}
	b.state = next
	return nil
}

// MemberPersonas returns every persona recorded, copied.
func (b *Binder) MemberPersonas(ctx context.Context) (map[domain.MemberID]MemberPersona, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[domain.MemberID]MemberPersona, len(b.state.Personas))
	for id, p := range b.state.Personas {
		out[id] = p
	}
	return out, nil
}

// Unbind clears a member's Telegram binding and returns the member as it was before,
// so a caller can say who was revoked and which space their key still opens.
//
// It returns ErrUnknownMember for a member this Binder does not hold. A member it does
// hold who has not enrolled is not an error: there is nothing to clear, so nothing is
// written and the member comes back unchanged.
func (b *Binder) Unbind(ctx context.Context, id domain.MemberID) (domain.Member, error) {
	if err := ctx.Err(); err != nil {
		return domain.Member{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	member, held := b.members[id]
	if !held {
		return domain.Member{}, fmt.Errorf("%w: %q", ErrUnknownMember, id)
	}
	before := memberCopy(member)
	if _, recorded := b.state.Binding(id); !recorded && member.TelegramID == 0 {
		return before, nil
	}

	next := cloneState(b.state)
	next.Unbind(id)
	if err := next.Save(b.path); err != nil {
		return domain.Member{}, fmt.Errorf("config: clearing the binding for member %q: %w", id, err)
	}

	b.state = next
	member.TelegramID = 0
	member.EnrolledAt = time.Time{}
	b.members[id] = member
	return before, nil
}

// Member returns a member the Binder holds, bindings included.
func (b *Binder) Member(id domain.MemberID) (domain.Member, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.members[id]
	if !ok {
		return domain.Member{}, false
	}
	return memberCopy(m), true
}

// Members returns every member the Binder holds, in configuration order with created
// members after them.
func (b *Binder) Members() []domain.Member {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]domain.Member, 0, len(b.order))
	for _, id := range b.order {
		out = append(out, memberCopy(b.members[id]))
	}
	return out
}

// provision builds the member a claim would create. The caller holds the lock.
//
// The private space follows the same convention the setup wizard writes, <id>-private,
// so a household that later adds this member to kenward.yaml by hand writes down what
// is already there rather than discovering kenward invented something else. A space
// that collides with the household's shared one moves out of the way, exactly as the
// wizard moves it; a space that is already another member's is refused, because
// reusing it would publish one member's private memory to another.
func (b *Binder) provision(id domain.MemberID, name string) (domain.Member, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.Member{}, fmt.Errorf("%w: member %q", ErrNoMemberName, id)
	}
	if len(b.prov.Tiers) == 0 {
		return domain.Member{}, fmt.Errorf("%w: member %q", ErrNoProvisioning, id)
	}

	space := domain.SpaceID(string(id) + "-private")
	if b.shared != "" && space == b.shared {
		space += "-own"
	}
	for _, other := range b.order {
		if b.members[other].Private == space {
			return domain.Member{}, fmt.Errorf("%w: %q is already member %q's, so member %q cannot have it",
				ErrPrivateSpaceTaken, space, other, id)
		}
	}
	return domain.Member{
		ID:      id,
		Name:    name,
		Private: space,
		Tiers:   copyStrings(b.prov.Tiers),
	}, nil
}

// holderOf reports which member holds a Telegram id. The caller holds the lock and has
// already refused a zero id, which is what an unclaimed member carries.
func (b *Binder) holderOf(telegramID int64) (domain.MemberID, bool) {
	for _, id := range b.order {
		if b.members[id].TelegramID == telegramID {
			return id, true
		}
	}
	return "", false
}

// memberCopy returns a member whose tier chain is nobody else's to edit. The chain is
// the privacy policy, and handing a caller the slice the Binder is holding would let a
// later package widen it in place.
func memberCopy(m domain.Member) domain.Member {
	m.Tiers = copyStrings(m.Tiers)
	return m
}

// cloneState copies a state so a mutation can be written to disk before it is adopted
// in memory. A save that fails must leave the Binder describing the file that is
// actually there; mutating in place and rolling back on error would leave a window in
// which it did not.
func cloneState(s *State) *State {
	out := &State{
		Version:  s.Version,
		Bindings: make(map[domain.MemberID]Binding, len(s.Bindings)),
		Personas: make(map[domain.MemberID]MemberPersona, len(s.Personas)),
	}
	for id, b := range s.Bindings {
		out.Bindings[id] = b
	}
	for id, p := range s.Personas {
		out.Personas[id] = p
	}
	return out
}
