// Package capture runs the confirmation state machine that sits between the assistant
// proposing something worth remembering and that thing being written.
//
// The assistant proposes; the member decides, with buttons, in the chat. Nothing
// reaches memory without an explicit confirmation, and there is deliberately no option
// to turn the question off — the question is the product, not a safety belt bolted onto
// it. A timeout is a decline, never an accept and never a retry.
//
// The scope is the authorization decision and this package obeys it: a group scope may
// never offer a private destination, because a household chat that can write into
// someone's private space is the one thing the memory model exists to prevent.
package capture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Target is where the model thinks a proposed memory belongs.
//
// The zero value is TargetUnsure: a proposal that has said nothing about its
// destination is asked about in full rather than assumed into a space.
type Target int

const (
	// TargetUnsure means the model did not commit to a destination. In a direct scope
	// the member is offered both; in a group scope it makes no difference, because
	// only the shared space is ever offered there.
	TargetUnsure Target = iota
	// TargetPersonal means the model proposed the member's private space. It is a
	// proposal, not an instruction: the member still confirms, and in a group scope
	// the personal destination is dropped rather than honoured.
	TargetPersonal
	// TargetShared means the model proposed the household's shared space.
	TargetShared
)

func (t Target) String() string {
	switch t {
	case TargetPersonal:
		return "personal"
	case TargetShared:
		return "shared"
	default:
		return "unsure"
	}
}

// Proposal is one candidate memory write, as the model produced it.
type Proposal struct {
	// Draft is the entry that would be stored, unchanged, if the member accepts.
	Draft memory.Draft
	// Target is the model's suggestion, never a decision.
	Target Target
}

// OutcomeKind enumerates every way a capture can end. Callers switch on it rather than
// inspecting the other fields, all of which may be zero.
type OutcomeKind int

const (
	// OutcomeUnknown is the zero value and is never returned with a nil error.
	OutcomeUnknown OutcomeKind = iota
	// OutcomeSaved means the entry was written. Space and EntryID name it.
	OutcomeSaved
	// OutcomeDeclined means the member chose not to save.
	OutcomeDeclined
	// OutcomeTimedOut means the question expired unanswered. Nothing was written; it
	// is a decline that the member did not have to type.
	OutcomeTimedOut
	// OutcomeDuplicate means the proposal repeated a title the member declined
	// recently and was suppressed without asking again.
	OutcomeDuplicate
	// OutcomeLimited means the per-turn proposal budget was already spent, so the
	// member was not asked.
	OutcomeLimited
	// OutcomeNotApplicable means the flow does not exist in this scope — a promotion
	// asked for from the household group, for instance.
	OutcomeNotApplicable
)

func (k OutcomeKind) String() string {
	switch k {
	case OutcomeSaved:
		return "saved"
	case OutcomeDeclined:
		return "declined"
	case OutcomeTimedOut:
		return "timed out"
	case OutcomeDuplicate:
		return "suppressed as duplicate"
	case OutcomeLimited:
		return "suppressed by per-turn limit"
	case OutcomeNotApplicable:
		return "not applicable"
	default:
		return "unknown"
	}
}

// Outcome is what a capture attempt did. Only OutcomeSaved carries Space and EntryID.
type Outcome struct {
	Kind OutcomeKind
	// Space is the space written to, set only when Kind is OutcomeSaved.
	Space domain.SpaceID
	// EntryID is the stored entry, set only when Kind is OutcomeSaved.
	EntryID string
	// Title is the proposal's title, carried on every outcome so a caller logging a
	// suppression can say which proposal it suppressed.
	Title string
}

// Stored reports whether this outcome wrote to memory. It is true for exactly one kind.
func (o Outcome) Stored() bool { return o.Kind == OutcomeSaved }

// Errors returned by this package. All of them mean nothing was written.
var (
	// ErrUnresolvedScope means capture was handed a scope that was never resolved.
	// Reaching capture without an authorization decision is a programming error.
	ErrUnresolvedScope = errors.New("capture: scope is unresolved")
	// ErrEmptyDraft means the proposal had no title. Titles identify entries to the
	// member and to the duplicate suppressor, so a proposal without one is refused.
	ErrEmptyDraft = errors.New("capture: proposal has no title")
	// ErrNoSharedSpace means the household's shared space could not be determined from
	// the scope and none was configured, so no shared destination can be offered.
	ErrNoSharedSpace = errors.New("capture: no shared space available in this scope")
	// ErrPersonalNotAllowed means a personal destination was selected in a scope that
	// does not allow private capture. It can only happen if a transport returns a
	// choice that was never offered; nothing is written.
	ErrPersonalNotAllowed = errors.New("capture: personal destination not allowed in this scope")
)

// Choice ids. They are stable and machine-readable: they travel through the transport
// as callback payloads and come back in transport.Answer.
const (
	// ChoicePersonal saves to the member's private space.
	ChoicePersonal = "capture.personal"
	// ChoiceShared saves to the household's shared space.
	ChoiceShared = "capture.shared"
	// ChoiceDecline saves nothing.
	ChoiceDecline = "capture.decline"
	// ChoicePublish publishes an existing private entry to the household.
	ChoicePublish = "capture.publish"
	// ChoiceCancel abandons a promotion.
	ChoiceCancel = "capture.cancel"
)

// Defaults applied by New to a zero-valued Options.
const (
	// DefaultMaxProposalsPerTurn matches capture.max_proposals_per_turn in the config.
	DefaultMaxProposalsPerTurn = 1
	// DefaultDeclineWindow is how many turns a declined title stays suppressed for,
	// counting the turn it was declined in.
	DefaultDeclineWindow = 10
	// DefaultAskTimeout bounds how long a capture question waits for a tap.
	DefaultAskTimeout = 5 * time.Minute
)

// maxDeclines bounds the per-scope decline history. The window prunes it on every turn
// boundary; this is the second bound, for a caller that never begins a turn.
const maxDeclines = 32

// maxScopes bounds how many scopes one Engine remembers. An Engine normally belongs to
// a single unit and therefore sees one scope, but nothing breaks if it is shared.
const maxScopes = 32

// Options tunes the capture machine. The zero value is valid and gets the defaults.
type Options struct {
	// MaxProposalsPerTurn caps how many proposals a single turn may put to the member.
	// Defaults to DefaultMaxProposalsPerTurn. Values below one are raised to it: there
	// is no configuration that silences capture entirely.
	MaxProposalsPerTurn int
	// DeclineWindow is how many turns a declined title is suppressed for. Defaults to
	// DefaultDeclineWindow.
	DeclineWindow int
	// AskTimeout is the timeout put on every question. Defaults to DefaultAskTimeout.
	// Expiry is a decline.
	AskTimeout time.Duration
	// Shared names the household's shared space, for the case where a direct scope's
	// Read set does not carry it. It is a fallback: the scope wins when it knows.
	Shared domain.SpaceID
}

func (o Options) normalized() Options {
	if o.MaxProposalsPerTurn < 1 {
		o.MaxProposalsPerTurn = DefaultMaxProposalsPerTurn
	}
	if o.DeclineWindow < 1 {
		o.DeclineWindow = DefaultDeclineWindow
	}
	if o.AskTimeout <= 0 {
		o.AskTimeout = DefaultAskTimeout
	}
	return o
}

// decline is one refused title, remembered with the turn it was refused in.
type decline struct {
	title string
	turn  int
}

// scopeState is the whole of capture's memory: which turn a scope is on, how much of
// this turn's budget is spent, and what was recently refused. It is in-process only and
// is never persisted — a restart legitimately forgets that you said no.
type scopeState struct {
	turnToken string
	turn      int
	offered   int
	declines  []decline
	touched   uint64
}

// Engine asks members to confirm memory writes and performs the ones they accept.
//
// An Engine is safe for concurrent use. It holds no per-member state beyond the small
// bounded history described above, keyed by scope rather than by member, so a unit may
// own one and nothing is shared between units that do not share a scope.
type Engine struct {
	mem  memory.Memory
	tr   transport.Transport
	opts Options

	mu     sync.Mutex
	states map[string]*scopeState
	clock  uint64
}

// New builds an Engine over a memory store and a transport. Options are normalized:
// the zero value gives one proposal per turn, a ten-turn decline window and a
// five-minute question timeout.
func New(m memory.Memory, t transport.Transport, opts Options) *Engine {
	return &Engine{
		mem:    m,
		tr:     t,
		opts:   opts.normalized(),
		states: make(map[string]*scopeState),
	}
}

// BeginTurn tells the Engine that a new turn has started in this scope, identified by a
// token the caller supplies — anything stable for the turn and different from the last
// one, such as the inbound message id.
//
// It resets the per-turn proposal budget and ages the decline history by one turn.
// Calling it twice with the same token is a no-op, so a caller that begins the turn
// defensively at more than one point does not skew the window. A caller that never
// begins a turn gets a single implicit turn, which spends its budget once and keeps it
// spent: the failure mode is asking too little, never writing too much.
func (e *Engine) BeginTurn(sc domain.Scope, turn string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stateLocked(sc)
	if st.turn != 0 && st.turnToken == turn {
		return
	}
	st.turnToken = turn
	st.turn++
	st.offered = 0
	st.pruneDeclines(e.opts.DeclineWindow)
}

// Offer puts one proposal to the member named by askUserID and, if they accept, writes
// it to the space they chose.
//
// The buttons depend on the scope, and only on the scope:
//
//	direct, target unsure  [Personal] [Household] [Don't save]
//	direct, target known   [Save to …] [Don't save]
//	group, any target      [Household] [Don't save]
//
// A group scope never offers a personal destination, whatever the model proposed.
//
// Proposals repeating a recently declined title are suppressed silently, as are
// proposals beyond the turn's budget; both return an Outcome saying so and ask nothing.
// A timeout is recorded as a decline.
func (e *Engine) Offer(ctx context.Context, sc domain.Scope, p Proposal, askUserID int64) (Outcome, error) {
	if sc.Kind == domain.ScopeUnknown {
		return Outcome{}, ErrUnresolvedScope
	}
	title := strings.TrimSpace(p.Draft.Title)
	if title == "" {
		return Outcome{}, ErrEmptyDraft
	}

	// Duplicate suppression comes before the budget check on purpose: a proposal the
	// member already refused must not consume the one question this turn is allowed.
	e.mu.Lock()
	st := e.stateLocked(sc)
	switch {
	case st.recentlyDeclined(title, e.opts.DeclineWindow):
		e.mu.Unlock()
		return Outcome{Kind: OutcomeDuplicate, Title: title}, nil
	case st.offered >= e.opts.MaxProposalsPerTurn:
		e.mu.Unlock()
		return Outcome{Kind: OutcomeLimited, Title: title}, nil
	}
	st.offered++
	turn := st.turn
	e.mu.Unlock()

	q, err := e.question(sc, p, title, askUserID)
	if err != nil {
		return Outcome{}, err
	}

	ans, err := e.tr.Ask(ctx, q)
	if err != nil {
		return Outcome{}, fmt.Errorf("capture: asking about %q: %w", title, err)
	}

	// A timeout is a decline. So is a tap the transport should have filtered out: the
	// only answer that writes anything is a save chosen by the member we asked.
	if ans.TimedOut {
		e.recordDecline(sc, title, turn)
		return Outcome{Kind: OutcomeTimedOut, Title: title}, nil
	}
	if ans.UserID != askUserID {
		e.recordDecline(sc, title, turn)
		return Outcome{Kind: OutcomeDeclined, Title: title}, nil
	}

	var space domain.SpaceID
	switch ans.ChoiceID {
	case ChoicePersonal:
		if !sc.AllowsPrivateCapture() {
			return Outcome{}, ErrPersonalNotAllowed
		}
		space = personalSpace(sc)
	case ChoiceShared:
		space, err = e.sharedSpace(sc)
		if err != nil {
			return Outcome{}, err
		}
	default:
		e.recordDecline(sc, title, turn)
		return Outcome{Kind: OutcomeDeclined, Title: title}, nil
	}

	draft := p.Draft
	draft.Title = title
	entry, err := e.mem.Put(ctx, space, draft)
	if err != nil {
		return Outcome{}, fmt.Errorf("capture: storing %q in %s: %w", title, space, err)
	}

	out := Outcome{Kind: OutcomeSaved, Space: space, EntryID: entry.ID, Title: title}
	// The write has happened; a failure to confirm it is reported but does not unsay
	// it, so the outcome is returned alongside the error.
	if err := e.tr.Send(ctx, transport.Outbound{
		ChatID: sc.ChatID,
		Text:   fmt.Sprintf("Saved %q to %s (%s).", title, destinationPhrase(sc, space), space),
	}); err != nil {
		return out, fmt.Errorf("capture: confirming %q: %w", title, err)
	}
	return out, nil
}

// OfferPromotion asks a member whether to publish one of their private entries to the
// household, and publishes it if they say yes.
//
// It exists only in a direct scope; from the group it returns OutcomeNotApplicable
// without touching memory. The member is shown the entry's full text before deciding,
// because from the household's side publication cannot be taken back: they must see
// exactly what everyone else will see.
//
// Acceptance calls memory.Share, never a Get followed by a Put, so lore keeps the
// entry's provenance instead of recording the household as its author.
//
// Promotions are neither counted against the per-turn proposal budget nor suppressed by
// the decline history. They are a deliberate act the member asked for, not a suggestion
// the assistant volunteered.
func (e *Engine) OfferPromotion(ctx context.Context, sc domain.Scope, entryID string, askUserID int64) (Outcome, error) {
	if sc.Kind == domain.ScopeUnknown {
		return Outcome{}, ErrUnresolvedScope
	}
	if !sc.AllowsPrivateCapture() {
		return Outcome{Kind: OutcomeNotApplicable}, nil
	}
	from := personalSpace(sc)
	to, err := e.sharedSpace(sc)
	if err != nil {
		return Outcome{}, err
	}

	entry, err := e.mem.Get(ctx, from, entryID)
	if err != nil {
		return Outcome{}, fmt.Errorf("capture: reading %s from %s: %w", entryID, from, err)
	}

	ans, err := e.tr.Ask(ctx, transport.Question{
		ChatID:        sc.ChatID,
		Text:          promotionText(entry),
		Choices:       []transport.Choice{{ID: ChoicePublish, Label: "Publish to household"}, {ID: ChoiceCancel, Label: "Cancel"}},
		AllowedUserID: askUserID,
		Timeout:       e.opts.AskTimeout,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("capture: asking about publishing %s: %w", entryID, err)
	}

	switch {
	case ans.TimedOut:
		return Outcome{Kind: OutcomeTimedOut, Title: entry.Title}, nil
	case ans.UserID != askUserID, ans.ChoiceID != ChoicePublish:
		return Outcome{Kind: OutcomeDeclined, Title: entry.Title}, nil
	}

	shared, err := e.mem.Share(ctx, from, to, entryID)
	if err != nil {
		return Outcome{}, fmt.Errorf("capture: publishing %s to %s: %w", entryID, to, err)
	}

	out := Outcome{Kind: OutcomeSaved, Space: to, EntryID: shared.ID, Title: entry.Title}
	if err := e.tr.Send(ctx, transport.Outbound{
		ChatID: sc.ChatID,
		Text:   fmt.Sprintf("Published %q to the household memory (%s). Everyone can see it now.", entry.Title, to),
	}); err != nil {
		return out, fmt.Errorf("capture: confirming publication of %q: %w", entry.Title, err)
	}
	return out, nil
}

// question renders the proposal into the exact button set the scope allows.
func (e *Engine) question(sc domain.Scope, p Proposal, title string, askUserID int64) (transport.Question, error) {
	q := transport.Question{
		ChatID:        sc.ChatID,
		AllowedUserID: askUserID,
		Timeout:       e.opts.AskTimeout,
	}

	// Outside a direct scope there is no personal destination to offer, so an unsure
	// or personal target collapses to the shared one.
	target := p.Target
	if !sc.AllowsPrivateCapture() {
		target = TargetShared
	}

	switch target {
	case TargetUnsure:
		q.Text = proposalText(p, title, "")
		q.Choices = []transport.Choice{
			{ID: ChoicePersonal, Label: "Personal"},
			{ID: ChoiceShared, Label: "Household"},
			{ID: ChoiceDecline, Label: "Don't save"},
		}
	case TargetPersonal:
		space := personalSpace(sc)
		q.Text = proposalText(p, title, destinationPhrase(sc, space))
		q.Choices = []transport.Choice{
			{ID: ChoicePersonal, Label: "Save to personal"},
			{ID: ChoiceDecline, Label: "Don't save"},
		}
	case TargetShared:
		space, err := e.sharedSpace(sc)
		if err != nil {
			return transport.Question{}, err
		}
		label := "Save to household"
		if !sc.AllowsPrivateCapture() {
			// In the group there is nothing to choose between, so the button says
			// where it goes rather than pretending to be a choice of space.
			label = "Household"
		}
		q.Text = proposalText(p, title, destinationPhrase(sc, space))
		q.Choices = []transport.Choice{
			{ID: ChoiceShared, Label: label},
			{ID: ChoiceDecline, Label: "Don't save"},
		}
	}
	return q, nil
}

// personalSpace is the member's private space. A direct scope writes there by
// definition, so it is Write and never re-derived from configuration.
func personalSpace(sc domain.Scope) domain.SpaceID { return sc.Write }

// sharedSpace is the household space this scope can publish to. A group scope writes
// there already; a direct scope carries it in Read, after the member's private space.
// Options.Shared is the fallback for a scope that lists neither.
func (e *Engine) sharedSpace(sc domain.Scope) (domain.SpaceID, error) {
	if !sc.AllowsPrivateCapture() {
		if sc.Write == "" {
			return "", ErrNoSharedSpace
		}
		return sc.Write, nil
	}
	for _, s := range sc.Read {
		if s != sc.Write && s != "" {
			return s, nil
		}
	}
	if e.opts.Shared != "" {
		return e.opts.Shared, nil
	}
	return "", ErrNoSharedSpace
}

// destinationPhrase names a space the way it is spoken about in the chat.
func destinationPhrase(sc domain.Scope, space domain.SpaceID) string {
	if sc.AllowsPrivateCapture() && space == sc.Write {
		return "your private memory"
	}
	return "the household memory"
}

func proposalText(p Proposal, title, destination string) string {
	var b strings.Builder
	b.WriteString("I can remember this:\n\n")
	b.WriteString(title)
	if body := strings.TrimSpace(p.Draft.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
	}
	if destination == "" {
		b.WriteString("\n\nWhere should it go?")
	} else {
		b.WriteString("\n\nSave it to ")
		b.WriteString(destination)
		b.WriteString("?")
	}
	return b.String()
}

func promotionText(entry memory.Entry) string {
	var b strings.Builder
	b.WriteString("This would be published to the household exactly as it stands, and cannot be unpublished:\n\n")
	b.WriteString(entry.Title)
	if body := strings.TrimSpace(entry.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
	}
	b.WriteString("\n\nPublish it?")
	return b.String()
}

// recordDecline remembers a refusal so the same title is not put to the member again
// for the next DeclineWindow turns.
func (e *Engine) recordDecline(sc domain.Scope, title string, turn int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stateLocked(sc)
	st.declines = append(st.declines, decline{title: normalizeTitle(title), turn: turn})
	if len(st.declines) > maxDeclines {
		st.declines = append(st.declines[:0], st.declines[len(st.declines)-maxDeclines:]...)
	}
}

func (st *scopeState) recentlyDeclined(title string, window int) bool {
	want := normalizeTitle(title)
	for _, d := range st.declines {
		if d.title == want && st.turn-d.turn < window {
			return true
		}
	}
	return false
}

func (st *scopeState) pruneDeclines(window int) {
	kept := st.declines[:0]
	for _, d := range st.declines {
		if st.turn-d.turn < window {
			kept = append(kept, d)
		}
	}
	st.declines = kept
}

// normalizeTitle makes duplicate detection insensitive to the cosmetic differences a
// model produces between turns.
func normalizeTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

// stateLocked returns the state for a scope, creating it if needed. Callers hold e.mu.
func (e *Engine) stateLocked(sc domain.Scope) *scopeState {
	key := scopeKey(sc)
	e.clock++
	if st, ok := e.states[key]; ok {
		st.touched = e.clock
		return st
	}
	if len(e.states) >= maxScopes {
		e.evictOldestLocked()
	}
	st := &scopeState{touched: e.clock}
	e.states[key] = st
	return st
}

func (e *Engine) evictOldestLocked() {
	var oldestKey string
	var oldest uint64
	for k, st := range e.states {
		if oldestKey == "" || st.touched < oldest {
			oldestKey, oldest = k, st.touched
		}
	}
	delete(e.states, oldestKey)
}

// scopeKey identifies a conversation. A member's direct chat and the household group
// are different scopes and keep different histories, which is why the kind and the
// write space are both in the key.
func scopeKey(sc domain.Scope) string {
	return fmt.Sprintf("%d:%d:%s", sc.Kind, sc.ChatID, sc.Write)
}
