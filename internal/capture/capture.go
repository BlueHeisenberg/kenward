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
//
// # Errors carry no conversation content
//
// Every error returned from this package names the failure by outcome, space and entry
// id, and never by the proposal's title or body. Those are written by the model out of
// what the member just said — a title is frequently a one-line summary of precisely the
// private thing that was said — and an error is the part of a failure that reaches the
// operator's log by default. In simple mode the operator can read the conversation
// anyway; in isolated mode a pod's log is aggregated on the host, and the whole point of
// that mode is that the host operator cannot read a member's memory. An id identifies
// the failed capture well enough to act on and is not content.
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
	// Title is the proposal's title, carried on every outcome so a caller can put
	// it back in front of the member it belongs to.
	//
	// It is member content, not diagnostics. A title is written by the model out
	// of what the member just said, so it is routinely a one-line summary of
	// exactly the private thing they said it about. Do not log it and do not put
	// it in an error: an outcome is identified for an operator by Kind, Space and
	// EntryID, all three of which are safe.
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

// decline is one refused title, remembered with the turn it was refused in and the
// member who refused it. The speaker matters in the household group: one member's
// refusal must not decide what another member is asked, so suppression matches on
// who declined, not just what was declined. In a direct scope there is one speaker
// and the field changes nothing.
type decline struct {
	title   string
	turn    int
	speaker int64
}

// scopeState is the whole of capture's memory: which turn a scope is on, how much of
// this turn's budget is spent, and what was recently refused. It is in-process only and
// is never persisted — a restart legitimately forgets that you said no.
//
// The proposal budget is per speaker within the turn. In the household group a
// capture question can still be waiting on one member's tap when another member's
// turn begins, and a budget shared across speakers would let the first member's
// late question consume the second member's only slot. A direct scope has one
// speaker, so the map holds one entry and behaves exactly as a counter did.
type scopeState struct {
	turnToken string
	turn      int
	offered   map[int64]int
	declines  []decline
	touched   uint64
}

// Engine asks members to confirm memory writes and performs the ones they accept.
//
// An Engine is safe for concurrent use. It holds no per-member state beyond the small
// bounded history described above, keyed by scope, so a unit may own one and nothing
// is shared between units that do not share a scope. Within a scope's state the
// decline records and the per-turn budget carry the speaker, because the household
// group is one scope with many speakers and neither one member's refusals nor their
// pending question may decide what another member is asked.
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
	st.offered = nil
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
// Destinations are resolved before they are offered: an unsure proposal in a scope
// with no reachable shared space offers only the personal button, never one whose
// tap can only fail.
//
// Proposals repeating a recently declined title are suppressed silently, as are
// proposals beyond the turn's budget; both return an Outcome saying so and ask nothing.
// A timeout is recorded as a decline. A proposal that never became a question — the
// question could not be built or could not be sent — does not spend the turn's budget.
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
	case st.recentlyDeclined(title, askUserID, e.opts.DeclineWindow):
		e.mu.Unlock()
		return Outcome{Kind: OutcomeDuplicate, Title: title}, nil
	case st.offered[askUserID] >= e.opts.MaxProposalsPerTurn:
		e.mu.Unlock()
		return Outcome{Kind: OutcomeLimited, Title: title}, nil
	}
	if st.offered == nil {
		st.offered = make(map[int64]int, 1)
	}
	st.offered[askUserID]++
	turn := st.turn
	e.mu.Unlock()

	q, err := e.question(sc, p, title, askUserID)
	if err != nil {
		// No question reached the member, so the budget was not spent on them.
		// Refunding it follows the duplicate-suppression reasoning above: a
		// proposal that never became a question must not consume the one question
		// this turn is allowed.
		e.refundOffer(sc, askUserID, turn)
		return Outcome{}, err
	}

	ans, err := e.tr.Ask(ctx, q)
	if err != nil {
		e.refundOffer(sc, askUserID, turn)
		// The member may have seen a question that will never resolve; the one
		// thing they must not be left believing is that something was stored.
		e.notify(ctx, sc, fmt.Sprintf("I meant to ask about remembering %q, but the question didn't go through. Nothing was written.", title))
		return Outcome{}, fmt.Errorf("capture: asking the member to confirm a proposal: %w", err)
	}

	// A timeout is a decline.
	if ans.TimedOut {
		e.recordDecline(sc, askUserID, title, turn)
		return Outcome{Kind: OutcomeTimedOut, Title: title}, nil
	}
	// A tap from anyone but the member we asked is ignored — not honoured, and not
	// recorded as the asked member's decline either: suppressing a title for ten
	// turns on the strength of someone else's tap would let another member decide
	// what this one is never asked about. The transport filters these before they
	// get here; this is the defence for a transport that does not.
	if ans.UserID != askUserID {
		return Outcome{Kind: OutcomeTimedOut, Title: title}, nil
	}

	var space domain.SpaceID
	switch ans.ChoiceID {
	case ChoicePersonal:
		if !sc.AllowsPrivateCapture() {
			e.notify(ctx, sc, "I couldn't save that — nothing was written.")
			return Outcome{}, ErrPersonalNotAllowed
		}
		space = personalSpace(sc)
	case ChoiceShared:
		space, err = e.sharedSpace(sc)
		if err != nil {
			e.notify(ctx, sc, "I couldn't save that — nothing was written.")
			return Outcome{}, err
		}
	default:
		e.recordDecline(sc, askUserID, title, turn)
		return Outcome{Kind: OutcomeDeclined, Title: title}, nil
	}

	draft := p.Draft
	draft.Title = title
	entry, err := e.mem.Put(ctx, space, draft)
	if err != nil {
		// The write may have landed even though the store reported failure, and
		// lore has no delete, so a retry that duplicates is permanent. Report the
		// uncertainty to the member (IMPLEMENTATION.md section 12) instead of
		// retrying silently, and suppress the title so the model does not
		// immediately re-propose the thing they were just told to verify first.
		e.recordDecline(sc, askUserID, title, turn)
		e.notify(ctx, sc, fmt.Sprintf("I can't confirm whether %q was saved — the memory store didn't answer. Check before saving it again; a duplicate can't be deleted.", title))
		return Outcome{}, fmt.Errorf("capture: storing a confirmed entry in %s: %w", space, err)
	}

	// The confirmation reports the outcome, never echoes the intention: it is
	// derived from the entry the store returned, so the destination the member is
	// told and the destination that was written come from different values and can
	// disagree. A store reporting a different space than the one confirmed is
	// treated as a failure — telling a member their private note is private while
	// it sits in the shared space is the exact failure this product exists to
	// prevent, and a confirmation built from the intended space could never notice.
	if entry.Space != space {
		e.recordDecline(sc, askUserID, title, turn)
		e.notify(ctx, sc, fmt.Sprintf("Something went wrong: %q was not stored where you chose. Tell whoever runs this node before saving it again.", title))
		return Outcome{}, fmt.Errorf("capture: store reported space %s for a write confirmed to %s", entry.Space, space)
	}

	out := Outcome{Kind: OutcomeSaved, Space: entry.Space, EntryID: entry.ID, Title: title}
	// The write has happened; a failure to confirm it is reported but does not unsay
	// it, so the outcome is returned alongside the error.
	if err := e.tr.Send(ctx, transport.Outbound{
		ChatID: sc.ChatID,
		Text:   fmt.Sprintf("Saved %q to %s (%s).", title, destinationPhrase(sc, entry.Space), entry.Space),
	}); err != nil {
		return out, fmt.Errorf("capture: confirming entry %s stored in %s: %w", out.EntryID, entry.Space, err)
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

	// As with Offer, the confirmation reports where the copy actually landed, and
	// a store reporting a different space than the one the member approved is a
	// failure, not something to confirm.
	if shared.Space != to {
		e.notify(ctx, sc, fmt.Sprintf("Something went wrong: %q was not published where you chose. Tell whoever runs this node.", entry.Title))
		return Outcome{}, fmt.Errorf("capture: store reported space %s for a publication confirmed to %s", shared.Space, to)
	}

	out := Outcome{Kind: OutcomeSaved, Space: shared.Space, EntryID: shared.ID, Title: entry.Title}
	if err := e.tr.Send(ctx, transport.Outbound{
		ChatID: sc.ChatID,
		Text:   fmt.Sprintf("Published %q to the household memory (%s). Everyone can see it now.", entry.Title, shared.Space),
	}); err != nil {
		return out, fmt.Errorf("capture: confirming publication of entry %s as %s in %s: %w", entryID, out.EntryID, shared.Space, err)
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
		// Both destinations are resolved before either is offered. A button whose
		// tap can only fail is worse than no button: the member chooses Household,
		// the resolution fails after the fact, and nothing they can see explains
		// why nothing happened. If no shared destination exists, offer what does.
		if _, err := e.sharedSpace(sc); err != nil {
			space := personalSpace(sc)
			q.Text = proposalText(p, title, destinationPhrase(sc, space))
			q.Choices = []transport.Choice{
				{ID: ChoicePersonal, Label: "Save to personal"},
				{ID: ChoiceDecline, Label: "Don't save"},
			}
			break
		}
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

// refundOffer returns a spent slot to the speaker's proposal budget. It is called
// only when no question reached the member; a question they saw — answered, ignored
// or declined — keeps the budget spent. The turn is checked so a refund arriving
// after the turn has moved on does not credit the next one.
func (e *Engine) refundOffer(sc domain.Scope, speaker int64, turn int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stateLocked(sc)
	if st.turn == turn && st.offered[speaker] > 0 {
		st.offered[speaker]--
	}
}

// notify tells the member what happened to their capture when the normal flow could
// not. Delivery failure is swallowed deliberately: every caller is already returning
// the error that matters, and a transport that cannot send will report the same
// fault there.
func (e *Engine) notify(ctx context.Context, sc domain.Scope, text string) {
	_ = e.tr.Send(ctx, transport.Outbound{ChatID: sc.ChatID, Text: text})
}

// recordDecline remembers a refusal so the same title is not put to that member
// again for the next DeclineWindow turns. The speaker is part of the record: in the
// household group, one member's refusal suppresses the title for them alone.
func (e *Engine) recordDecline(sc domain.Scope, speaker int64, title string, turn int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stateLocked(sc)
	st.declines = append(st.declines, decline{title: normalizeTitle(title), turn: turn, speaker: speaker})
	if len(st.declines) > maxDeclines {
		st.declines = append(st.declines[:0], st.declines[len(st.declines)-maxDeclines:]...)
	}
}

func (st *scopeState) recentlyDeclined(title string, speaker int64, window int) bool {
	want := normalizeTitle(title)
	for _, d := range st.declines {
		if d.title == want && d.speaker == speaker && st.turn-d.turn < window {
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
