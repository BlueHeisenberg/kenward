// Package capture runs the state machine that sits between the assistant proposing
// something worth remembering and that thing reaching memory.
//
// # Two different acts, deliberately treated differently
//
// A note in a member's own private space is theirs, and it is reversible: it is
// written, the member is shown exactly what was written and where, and one button
// takes it back. A publication to the household is not reversible — other people have
// read it by the time anyone regrets it — so it is asked about first, every time, and
// there is no setting that turns that question off.
//
// The write announcement is likewise not optional. It is the whole of what replaces
// the old question: if a write can be silent then "kenward never records anything
// without telling you" stops being true, and there is no honest way left to describe
// the product. A household that wants the question back for private writes can have it
// — Options.PrivateWrites — and gets exactly the behaviour that shipped before.
//
// A timeout is never an accept: on a question it is a decline, and on a write
// announcement it is the undo window closing on a write that already happened and
// stands.
//
// The scope is the authorization decision and this package obeys it: a group scope may
// never offer a private destination, because a household chat that can write into
// someone's private space is the one thing the memory model exists to prevent. It
// follows that nothing in the group is ever written without being asked about first:
// everything there is a shared write.
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
	"github.com/BlueHeisenberg/kenward/internal/lang"
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
	// Aliases are the member's own words for the thing this entry is about, in the
	// language the conversation is held in. They are folded into the stored body as
	// one line — see aliasLine — and they are the whole of what makes an entry
	// retrievable by the person who said it.
	//
	// Titles and bodies stay in English, deliberately. lore's search is a
	// conjunctive lexical match over title, body and domain with no stemming and no
	// translation, so an entry is found only by words it literally contains: a
	// Spanish household's question retrieves nothing from an English entry, which
	// is the defect this field exists for. Writing the entry in the member's
	// language instead would fix one household and break the shared space of any
	// household with two, because the group scope has no member's language to write
	// in and a shared memory in two languages is half-invisible to each of them. So
	// English stays the one language every entry is guaranteed to hold — which is
	// also what keeps a household's existing entries findable on the day they
	// switch languages — and the member's own words ride alongside it.
	//
	// Empty is the ordinary case for an English conversation, and an alias whose
	// every word is already in the entry is dropped rather than repeated.
	Aliases []string
}

// OutcomeKind enumerates every way a capture can end. Callers switch on it rather than
// inspecting the other fields, all of which may be zero.
type OutcomeKind int

const (
	// OutcomeUnknown is the zero value and is never returned with a nil error.
	OutcomeUnknown OutcomeKind = iota
	// OutcomeSaved means the entry was written and is still there. Space and
	// EntryID name it.
	OutcomeSaved
	// OutcomeUndone means the entry was written and the member took it back
	// before the undo window closed, and the delete is confirmed. Space and
	// EntryID name what was removed.
	//
	// An undo whose delete failed, or whose delete could not be confirmed, is not
	// this: the entry may still be there, so the outcome stays OutcomeSaved and
	// the error says what the member was told.
	OutcomeUndone
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
	case OutcomeUndone:
		return "undone"
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

// Outcome is what a capture attempt did. Only OutcomeSaved and OutcomeUndone carry
// Space and EntryID.
type Outcome struct {
	Kind OutcomeKind
	// Space is the space written to, set only when Kind is OutcomeSaved or
	// OutcomeUndone.
	Space domain.SpaceID
	// EntryID is the stored entry, set only when Kind is OutcomeSaved or
	// OutcomeUndone. On an undone outcome it names what was removed.
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

// Stored reports whether this outcome left something in memory. An undone write did
// not, which is the point of undoing it.
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
	// ErrMemberNotified is joined onto every error this package returns after it has
	// already put a notice about that failure in the chat — or tried to and could
	// not, which comes to the same thing for a caller deciding whether to speak. It
	// is for a caller whose own fallback is a generic "no answer" notice: telling the
	// member twice, in two different words, is worse than either message alone — and
	// after a failed publication the generic one reads as an invitation to retry an
	// act that cannot be taken back. Its absence means the member saw nothing and the
	// caller speaks.
	ErrMemberNotified = errors.New("capture: the member was told")
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
	// ChoiceUndo removes an entry that was written and announced without asking.
	ChoiceUndo = "capture.undo"
)

// PrivateWritePolicy says what a proposal for the member's own private space does.
//
// It has no bearing on the household's shared space, which is always asked about, and
// none on a proposal whose destination the model left unsure, which is always asked
// about because there is a genuine choice to put. It is the one part of this that a
// household configures.
type PrivateWritePolicy int

const (
	// PrivateWriteSave writes the entry, then shows the member exactly what was
	// written and where, with an undo button live for Options.AskTimeout. It is the
	// zero value and the product's behaviour.
	PrivateWriteSave PrivateWritePolicy = iota
	// PrivateWriteAsk puts the proposal as a question first and writes nothing
	// until the member taps, exactly as a shared write is handled. A household that
	// wants to be asked about everything sets this; nothing else changes.
	PrivateWriteAsk
)

func (p PrivateWritePolicy) String() string {
	if p == PrivateWriteAsk {
		return "ask"
	}
	return "save"
}

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
	// Expiry is a decline — and, on a write announcement, the undo window closing on
	// a write that stands.
	AskTimeout time.Duration
	// PrivateWrites says whether a proposal for the member's own space is written
	// and announced, or asked about first. The zero value writes and announces.
	//
	// There is no equivalent for the shared space and there will not be one:
	// publishing to the household is the one act in this product that cannot be
	// taken back, so it is always asked about. Nor is there one for the
	// announcement: a write nobody is told about would make the product's central
	// claim false.
	PrivateWrites PrivateWritePolicy
	// Shared names the household's shared space, for the case where a direct scope's
	// Read set does not carry it. It is a fallback: the scope wins when it knows.
	Shared domain.SpaceID
	// Language is the language this conversation's member reads, named the way a
	// person names one. Empty is English.
	//
	// It is per engine because an engine belongs to one unit and a unit is one
	// conversation. The group chat has no member to ask, so it gets the
	// household's — which is the same resolution the assistant's persona already
	// makes, and it is made once, in the supervisor, so the two cannot drift.
	Language string
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
	// cat is every string this engine's member reads. Announcements, button labels
	// and the outcome line on a spent question all come from it, and the outcome
	// line travels on the Question so the transport can size the message against
	// this language rather than against English.
	cat lang.Catalogue

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
		cat:    lang.For(opts.Language),
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

// Offer acts on one proposal for the member named by askUserID.
//
// A proposal for that member's own private space, under the default policy, is
// written immediately and then announced: the member sees exactly what was written
// and where, with an [Undo] button live for Options.AskTimeout. Everything else is a
// question first and a write only on a tap.
//
// Where a question is put, the buttons depend on the scope, and only on the scope:
//
//	direct, target unsure  [Personal] [Household] [Don't save]
//	direct, target shared  [Save to household] [Don't save]
//	direct, target personal, PrivateWriteAsk  [Save to personal] [Don't save]
//	group, any target      [Household] [Don't save]
//
// A group scope never offers a personal destination, whatever the model proposed, and
// therefore never writes without asking. Destinations are resolved before they are
// offered: an unsure proposal in a scope with no reachable shared space offers only
// the personal button, never one whose tap can only fail.
//
// Proposals repeating a recently declined title are suppressed silently, as are
// proposals beyond the turn's budget; both return an Outcome saying so, and neither
// writes nor asks anything. An undo counts as a decline, so the model does not
// re-propose next turn what the member has just taken back. A timeout is recorded as a
// decline on a question and changes nothing on an announcement. A proposal that never
// became a question — the question could not be built or could not be sent — does not
// spend the turn's budget; a proposal that was written does, whatever became of it
// afterwards.
func (e *Engine) Offer(ctx context.Context, sc domain.Scope, p Proposal, askUserID int64) (Outcome, error) {
	if sc.Kind == domain.ScopeUnknown {
		return Outcome{}, ErrUnresolvedScope
	}
	title := strings.TrimSpace(p.Draft.Title)
	if title == "" {
		return Outcome{}, ErrEmptyDraft
	}
	// Before anything else reads the draft, so that what is stored, what is put to
	// the member as a question and what is announced back to them are one string.
	// A member shown a body that is not the body that was written would make
	// "kenward tells you what it wrote" false in the small way that is hardest to
	// notice.
	p.Draft.Body = withAliases(e.cat, title, p.Draft.Body, p.Aliases)

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

	// The member's own space, under the default policy: write it, then say so. The
	// budget it just spent stays spent whatever happens next — unlike a question
	// that never reached them, this one is a write that did.
	if e.writesPrivateDirectly(sc, p) {
		return e.writeAndAnnounce(ctx, sc, p, title, askUserID, turn)
	}

	q, err := e.question(sc, p, title, askUserID)
	if err != nil {
		// No question reached the member, so the budget was not spent on them.
		// Refunding it follows the duplicate-suppression reasoning above: a
		// proposal that never became a question must not consume the one question
		// this turn is allowed.
		e.refundOffer(sc, askUserID, turn)
		// A remember turn is routinely a bare tool call with no prose, so a silent
		// return here is a turn that answers nothing at all — the same class of
		// defect as the publish paths below.
		return Outcome{}, e.told(ctx, sc, e.cat.SaveFailed, err)
	}

	ans, err := e.tr.Ask(ctx, q)
	if err != nil {
		e.refundOffer(sc, askUserID, turn)
		// The member may have seen a question that will never resolve; the one
		// thing they must not be left believing is that something was stored.
		return Outcome{}, e.told(ctx, sc,
			e.cat.AskFailed(title),
			fmt.Errorf("capture: asking the member to confirm a proposal: %w", err))
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
			return Outcome{}, e.told(ctx, sc, e.cat.SaveFailed, ErrPersonalNotAllowed)
		}
		space = personalSpace(sc)
	case ChoiceShared:
		space, err = e.sharedSpace(sc)
		if err != nil {
			return Outcome{}, e.told(ctx, sc, e.cat.SaveFailed, err)
		}
	default:
		e.recordDecline(sc, askUserID, title, turn)
		return Outcome{Kind: OutcomeDeclined, Title: title}, nil
	}

	out, err := e.store(ctx, sc, p, title, space, askUserID, turn)
	if err != nil {
		return Outcome{}, err
	}

	// The write has happened; a failure to confirm it is reported but does not unsay
	// it, so the outcome is returned alongside the error.
	if err := e.tr.Send(ctx, transport.Outbound{
		ChatID: sc.ChatID,
		Text:   transport.GlyphMemory + " " + e.cat.Saved(isPrivate(sc, out.Space), title),
	}); err != nil {
		// The notice the member needed was this confirmation, and it has already been
		// attempted; there is nothing useful left to say to a transport that just
		// failed. Marking it stops the caller's generic "try asking again" from
		// inviting a second write of an entry that landed.
		return out, marked(fmt.Errorf("capture: confirming entry %s stored in %s: %w", out.EntryID, out.Space, err))
	}
	return out, nil
}

// writesPrivateDirectly reports whether this proposal is the one shape that is written
// without being asked about: the member's own private space, in a scope that has one,
// with the model having actually said so, under a policy that allows it.
//
// Every clause is load-bearing. A group scope has no private destination at all. An
// unsure target is a real choice and is put as one. And a household that set
// PrivateWriteAsk gets the question back for all of it.
func (e *Engine) writesPrivateDirectly(sc domain.Scope, p Proposal) bool {
	return e.opts.PrivateWrites == PrivateWriteSave &&
		sc.AllowsPrivateCapture() &&
		p.Target == TargetPersonal
}

// writeAndAnnounce writes the proposal to the member's private space and shows them
// what it did, with an undo button.
//
// The order is the whole point: the write is real before the member is told about it,
// so the announcement is a report and not a promise. Nothing here is conditional on
// the member's attention — an ignored announcement leaves the entry exactly where the
// message says it is.
func (e *Engine) writeAndAnnounce(ctx context.Context, sc domain.Scope, p Proposal, title string, askUserID int64, turn int) (Outcome, error) {
	space := personalSpace(sc)
	out, err := e.store(ctx, sc, p, title, space, askUserID, turn)
	if err != nil {
		return Outcome{}, err
	}

	ans, err := e.tr.Ask(ctx, transport.Question{
		ChatID:        sc.ChatID,
		Text:          e.writtenText(p, title, isPrivate(sc, out.Space)),
		Choices:       []transport.Choice{{ID: ChoiceUndo, Label: e.cat.BtnUndo}},
		AllowedUserID: askUserID,
		Timeout:       e.opts.AskTimeout,
		RetiredNote:   e.cat.UndoExpiredNote,
		Notes:         e.cat.OutcomeNotes(),
	})
	if err != nil {
		// The entry is written and the message carrying that news did not go out.
		// This is the one failure in this package where saying nothing would break
		// the product's central claim rather than merely inconvenience someone, so
		// it falls back to a plain confirmation with no undo affordance — and says
		// that the affordance is missing, because a member who has been told they
		// can undo something and cannot is worse off than one who was never told.
		return out, e.told(ctx, sc,
			e.cat.SavedNoUndo(isPrivate(sc, out.Space), title),
			fmt.Errorf("capture: announcing entry %s written to %s: %w", out.EntryID, out.Space, err))
	}

	// Anything that is not this member tapping Undo leaves the write standing, and
	// the transport has already edited the announcement to say so. Nothing to add: a
	// second message repeating what the first one still says on screen is noise, and
	// the member did not ask a question to be answered twice.
	if ans.TimedOut || ans.UserID != askUserID || ans.ChoiceID != ChoiceUndo {
		return out, nil
	}
	return e.undo(ctx, sc, out, askUserID, turn)
}

// undo removes an entry the member has just taken back, and tells them what actually
// happened to it.
//
// There are three endings and they are three different sentences, because the entry is
// gone in one of them, still there in the second, and unknown in the third. Reporting
// the second or the third as "undone" would be the plainest lie this product could
// tell: the member asked for something to not exist, and would be told it does not
// while it does.
//
// A second tap cannot reach here. The transport retires the announcement on the first
// one — keyboard stripped, pending question forgotten — and every later tap on a
// keyboard still on somebody's screen is dropped silently, exactly as a stale tap on a
// capture question is. The delete is therefore attempted once per announcement, and
// lore's own idempotence is a second line rather than the first.
func (e *Engine) undo(ctx context.Context, sc domain.Scope, out Outcome, speaker int64, turn int) (Outcome, error) {
	// Recorded before the delete is attempted, and recorded whether or not it
	// succeeds: what the member has told us is that they did not want this written,
	// and that is true even if it turns out to still be there. Without it the model
	// re-proposes next turn and the default policy writes it straight back.
	e.recordDecline(sc, speaker, out.Title, turn)

	private := isPrivate(sc, out.Space)
	err := e.mem.Delete(ctx, out.Space, out.EntryID)
	switch {
	case err == nil:
		out.Kind = OutcomeUndone
		// The promise is bounded to what lore actually does. A delete is a signed
		// tombstone, not a shred: the entry stops coming back from search and from
		// get, here and on every synced device, and the row is still on the disk.
		// ARCHITECTURE.md's capture section required this sentence to say which of
		// the two it is, because "erased" and "will not be recalled" are different
		// promises and only the second one is kept.
		if serr := e.tr.Send(ctx, transport.Outbound{
			ChatID: sc.ChatID,
			Text:   transport.GlyphGone + " " + e.cat.Removed(private, out.Title),
		}); serr != nil {
			// As with the confirmation in Offer: the news has been attempted and
			// there is nothing useful left to say to a transport that just failed.
			return out, marked(fmt.Errorf("capture: confirming the removal of entry %s from %s: %w", out.EntryID, out.Space, serr))
		}
		return out, nil

	default:
		// A failed delete left the entry where it was. There is no third answer
		// to give a member here: the store either wrote the tombstone or it
		// returned an error, and while lore was a subprocess it could also lose
		// the reply and leave both of us guessing — which is what the
		// ErrWriteUncertain branch this replaces had to say out loud.
		return out, e.told(ctx, sc,
			e.cat.UndoFailed(private, out.Title),
			fmt.Errorf("capture: undoing entry %s in %s: %w", out.EntryID, out.Space, err))
	}
}

// store performs the write itself, shared by the path that asked first and the path
// that did not. Every error it returns has already been put to the member.
//
// The two failures below are the reason it is one function rather than two copies. A
// write that may have landed and a write that landed in the wrong space are the two
// ways storage can go wrong that a member must hear about in words, and they must read
// the same however the write was authorised.
func (e *Engine) store(ctx context.Context, sc domain.Scope, p Proposal, title string, space domain.SpaceID, speaker int64, turn int) (Outcome, error) {
	draft := p.Draft
	draft.Title = title
	entry, err := e.mem.Put(ctx, space, draft)
	if err != nil {
		// A failed write stored nothing, and the member is told exactly that. It
		// used to be told as a hedge — "I can't confirm whether it was saved" —
		// because a lost MCP response left an entry that might exist under an id
		// kenward never received, and therefore could not delete. In-process the
		// commit either happened or returned, so there is no duplicate to warn
		// about (IMPLEMENTATION.md section 12).
		//
		// The title is still suppressed: a write that just failed will fail again
		// next turn, and re-proposing it is noise rather than a second chance.
		e.recordDecline(sc, speaker, title, turn)
		return Outcome{}, e.told(ctx, sc,
			e.cat.StoreRefused(isPrivate(sc, space), title),
			fmt.Errorf("capture: storing an entry in %s: %w", space, err))
	}

	// What the member is told is derived from the entry the store returned, never
	// echoed back from the intention, so the destination they are told and the
	// destination that was written come from different values and can disagree. A
	// store reporting a different space is treated as a failure — telling a member
	// their private note is private while it sits in the shared space is the exact
	// failure this product exists to prevent, and a message built from the intended
	// space could never notice.
	if entry.Space != space {
		e.recordDecline(sc, speaker, title, turn)
		return Outcome{}, e.told(ctx, sc,
			e.cat.WrongSpace(title),
			fmt.Errorf("capture: store reported space %s for a write to %s", entry.Space, space))
	}

	return Outcome{Kind: OutcomeSaved, Space: entry.Space, EntryID: entry.ID, Title: title}, nil
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
		return Outcome{}, e.told(ctx, sc, e.cat.PublishNoShared, err)
	}

	entry, err := e.mem.Get(ctx, from, entryID)
	if err != nil {
		// The member asked for this and the model's own reply is frequently a bare
		// tool call with no prose, so a silent return here is a turn that answers
		// nothing at all.
		return Outcome{}, e.told(ctx, sc, e.cat.PublishUnreadable,
			fmt.Errorf("capture: reading %s from %s: %w", entryID, from, err))
	}

	ans, err := e.tr.Ask(ctx, transport.Question{
		ChatID: sc.ChatID,
		Text:   e.promotionText(entry),
		Choices: []transport.Choice{
			{ID: ChoicePublish, Label: e.cat.BtnPublishHousehold},
			{ID: ChoiceCancel, Label: e.cat.BtnCancel},
		},
		AllowedUserID: askUserID,
		Timeout:       e.opts.AskTimeout,
		Notes:         e.cat.OutcomeNotes(),
	})
	if err != nil {
		// As in Offer: the member may be looking at a question that will never
		// resolve, and the one thing they must not be left believing is that their
		// private entry is now public.
		return Outcome{}, e.told(ctx, sc,
			e.cat.PublishAskFailed(entry.Title),
			fmt.Errorf("capture: asking about publishing %s: %w", entryID, err))
	}

	switch {
	case ans.TimedOut:
		return Outcome{Kind: OutcomeTimedOut, Title: entry.Title}, nil
	case ans.UserID != askUserID, ans.ChoiceID != ChoicePublish:
		return Outcome{Kind: OutcomeDeclined, Title: entry.Title}, nil
	}

	shared, err := e.mem.Share(ctx, from, to, entryID)
	if err != nil {
		// A failed copy published nothing, so the member is told that rather than
		// asked to go and check. The hedge this replaces was the same reasoning as
		// the failed Put in store (IMPLEMENTATION.md section 12): a lost reply
		// could have left a private entry sitting in the household's memory with
		// nobody able to name it. It cannot now.
		return Outcome{}, e.told(ctx, sc,
			e.cat.PublishRefused(entry.Title),
			fmt.Errorf("capture: publishing %s to %s: %w", entryID, to, err))
	}

	// As with Offer, the confirmation reports where the copy actually landed, and
	// a store reporting a different space than the one the member approved is a
	// failure, not something to confirm.
	if shared.Space != to {
		return Outcome{}, e.told(ctx, sc,
			e.cat.PublishWrongSpace(entry.Title),
			fmt.Errorf("capture: store reported space %s for a publication confirmed to %s", shared.Space, to))
	}

	out := Outcome{Kind: OutcomeSaved, Space: shared.Space, EntryID: shared.ID, Title: entry.Title}
	if err := e.tr.Send(ctx, transport.Outbound{
		ChatID: sc.ChatID,
		Text:   transport.GlyphHousehold + " " + e.cat.Published(entry.Title),
	}); err != nil {
		// As in Offer, and worse: the publication happened and cannot be taken back,
		// so an unmarked error here would put "try asking again" in front of a member
		// whose entry is already in the household memory.
		return out, marked(fmt.Errorf("capture: confirming publication of entry %s as %s in %s: %w", entryID, out.EntryID, shared.Space, err))
	}
	return out, nil
}

// question renders the proposal into the exact button set the scope allows.
func (e *Engine) question(sc domain.Scope, p Proposal, title string, askUserID int64) (transport.Question, error) {
	q := transport.Question{
		ChatID:        sc.ChatID,
		AllowedUserID: askUserID,
		Timeout:       e.opts.AskTimeout,
		// The outcome line travels with the question so the transport can size the
		// message against this language's wording rather than English's. Choice ids
		// are stable constants and never translate; only the labels do.
		Notes: e.cat.OutcomeNotes(),
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
			q.Text = e.proposalText(p, title, dest{known: true, private: isPrivate(sc, personalSpace(sc))})
			q.Choices = []transport.Choice{
				{ID: ChoicePersonal, Label: e.cat.BtnSavePersonal},
				{ID: ChoiceDecline, Label: e.cat.BtnDontSave},
			}
			break
		}
		q.Text = e.proposalText(p, title, dest{})
		q.Choices = []transport.Choice{
			{ID: ChoicePersonal, Label: e.cat.BtnPersonal},
			{ID: ChoiceShared, Label: e.cat.BtnHousehold},
			{ID: ChoiceDecline, Label: e.cat.BtnDontSave},
		}
	case TargetPersonal:
		q.Text = e.proposalText(p, title, dest{known: true, private: isPrivate(sc, personalSpace(sc))})
		q.Choices = []transport.Choice{
			{ID: ChoicePersonal, Label: e.cat.BtnSavePersonal},
			{ID: ChoiceDecline, Label: e.cat.BtnDontSave},
		}
	case TargetShared:
		space, err := e.sharedSpace(sc)
		if err != nil {
			return transport.Question{}, err
		}
		label := e.cat.BtnSaveHousehold
		if !sc.AllowsPrivateCapture() {
			// In the group there is nothing to choose between, so the button says
			// where it goes rather than pretending to be a choice of space.
			label = e.cat.BtnHousehold
		}
		q.Text = e.proposalText(p, title, dest{known: true, private: isPrivate(sc, space)})
		q.Choices = []transport.Choice{
			{ID: ChoiceShared, Label: label},
			{ID: ChoiceDecline, Label: e.cat.BtnDontSave},
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

// isPrivate reports whether a space is this member's own rather than the
// household's, which is the only thing the catalogue needs in order to choose the
// right sentence.
//
// It used to be destinationPhrase, returning "your private memory" or "the household
// memory" to be slotted into nine sentences. That worked in English and in no
// language with a case system: German inflects the phrase with the preposition and
// contracts in + dem into im, French contracts à + le into au, and Dutch changes the
// preposition itself — opslaan in against verwijderen uit — so a shared slot
// produced "verwijderd in je persoonlijke geheugen", which claims the opposite of
// what happened. The catalogue writes each containing sentence out instead, and this
// predicate is all that is left of the slot.
//
// The phrase, however a language spells it, is still the only name a member is given
// for a space. Every message here used to append lore's space id as well — "Saved …
// to your private memory (a3a02466-c412-4da1-b931-74380179e69d)" — on the reasoning
// that the phrase is what a member reads and the id is what they can check. They
// cannot check it: there is nowhere for a member to type a space id, nothing to
// compare it against, and a household never has two private memories or two shared
// ones to tell apart. It cost a UUID's width on every confirmation and bought
// nothing. The id is still on every log line, where whoever runs the node can use it.
func isPrivate(sc domain.Scope, space domain.SpaceID) bool {
	return sc.AllowsPrivateCapture() && space == sc.Write
}

// entryBlock renders a draft or an entry as the member sees it: the title in
// bold because it is the thing the message is about, the body quoted because it
// is stored words being shown back rather than kenward's own sentence.
//
// Both are member-written and both are escaped. A note titled "<b>" is a note
// titled "<b>", not a message whose remainder is bold.
func entryBlock(title, body string) string {
	out := transport.Bold(title)
	if body = strings.TrimSpace(body); body != "" {
		out += "\n" + transport.Quote(body)
	}
	return out
}

// Bounds on the alias line. Both exist because Proposal.Aliases is model-written
// text arriving from a member's conversation: a model asked for a few words can
// return a paragraph, and the line is stored and shown.
const (
	// maxAliases is how many the line carries. Retrieval searches one content word
	// at a time and unions the hits, so the fourth phrasing of the same thing buys
	// almost nothing and costs a longer entry on every read of it.
	maxAliases = 6
	// maxAliasRunes is how long one alias may be. Anything longer is a sentence,
	// not a name for something, and a sentence belongs in the body.
	maxAliasRunes = 64
)

// withAliases returns the body to store: the draft's own body, with the member's
// words for the same thing appended as one line, in their language.
//
// The line goes in the body because that is where lore's index can reach it. lore
// matches over title, body and domain; markers are a filter and are not searched, so
// there is nowhere else to put a word and have it retrieve anything.
//
// An empty alias set — the ordinary case for a household reading English — returns
// the body unchanged, so nothing about an English conversation changes shape.
func withAliases(cat lang.Catalogue, title, body string, aliases []string) string {
	kept := usefulAliases(title, body, aliases)
	if len(kept) == 0 {
		return body
	}
	return strings.TrimRight(body, "\n") + "\n\n" + cat.AlsoKnownAs(kept)
}

// usefulAliases reduces what the model proposed to what is worth storing: trimmed,
// bounded, without repeats, and without anything the entry already says.
//
// The last of those is the load-bearing one. A model told to supply the member's own
// words will supply them in an English conversation too, where they are the words
// already in the title; storing them would put a line of duplication on the end of
// every entry in every English household for no retrieval gain at all. "Already says"
// is measured in lore's own tokens (memory.Terms), because those are the units a
// search matches in — an alias adds something exactly when it adds a word that can
// be searched for.
func usefulAliases(title, body string, aliases []string) []string {
	have := make(map[string]bool)
	for _, w := range memory.Terms(title + " " + body) {
		have[w] = true
	}
	var kept []string
	for _, a := range aliases {
		a = strings.Join(strings.Fields(a), " ")
		if a == "" || len([]rune(a)) > maxAliasRunes {
			continue
		}
		terms := memory.Terms(a)
		novel := false
		for _, t := range terms {
			if !have[t] {
				novel = true
			}
		}
		if !novel {
			continue
		}
		// Recorded before the next alias is judged, so two phrasings that differ
		// only in a word the first one already contributed do not both survive.
		for _, t := range terms {
			have[t] = true
		}
		if kept = append(kept, a); len(kept) == maxAliases {
			break
		}
	}
	return kept
}

// dest is which memory a question is about, when it is about one at all. An unsure
// proposal offers a genuine choice and names no destination, which is the zero value.
type dest struct {
	known   bool
	private bool
}

func (e *Engine) proposalText(p Proposal, title string, d dest) string {
	var b strings.Builder
	b.WriteString(transport.GlyphAsk + " " + e.cat.ProposalOpener + "\n\n")
	b.WriteString(entryBlock(title, p.Draft.Body))
	b.WriteString("\n\n")
	if d.known {
		b.WriteString(e.cat.ProposalWithDest(d.private))
	} else {
		b.WriteString(e.cat.ProposalNoDest)
	}
	return b.String()
}

// Catalogue.UndoExpiredNote is appended to a write announcement once its undo window
// has closed, in place of the transport's default "no answer, treated as declined".
//
// The default would be a lie in the worst available direction: it would leave a
// message that says an entry was written ending in a line that reads as though the
// write had been called off. The note says the two things that are true — the button
// is finished and the entry is not — and it names which memory the entry is still in,
// because this product's whole premise is that there are two of them. It rides only
// on a private write announcement, so naming that one is not a guess.

// writtenText is the announcement of a write that already happened.
//
// It shows the whole draft, title and body, because the member did not see it before
// it was stored and "kenward tells you what it wrote" means the words, not a
// reassurance that some words exist.
//
// It opens with the memory glyph rather than the question glyph, and that is the
// load-bearing difference between this message and proposalText: one reports, one
// asks, and they are otherwise the same shape on the screen. The closing hint is
// set apart in italics because it is the button being explained, not part of what
// was written.
func (e *Engine) writtenText(p Proposal, title string, private bool) string {
	var b strings.Builder
	b.WriteString(transport.GlyphMemory + " " + e.cat.WrittenOpener(private) + "\n\n")
	b.WriteString(entryBlock(title, p.Draft.Body))
	b.WriteString("\n\n")
	b.WriteString(transport.Italic(e.cat.WrittenHint))
	return b.String()
}

func (e *Engine) promotionText(entry memory.Entry) string {
	var b strings.Builder
	b.WriteString(transport.GlyphAsk + " " + e.cat.PromotionOpener + "\n\n")
	b.WriteString(entryBlock(entry.Title, entry.Body))
	b.WriteString("\n\n" + e.cat.PromotionCloser)
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

// told tells the member what happened to their capture when the normal flow could not,
// and marks err as already spoken for so a caller can tell whether the failure reached
// them (see ErrMemberNotified). Every error path that the member could be waiting on
// goes through here; that is the whole discipline of this file.
//
// Delivery failure is swallowed deliberately: every caller is already returning the
// error that matters, and a transport that cannot send will report the same fault there.
//
// The problem glyph is prepended here rather than by each caller. Everything that
// reaches this function is, by the definition above, a turn that did not do what
// it set out to do, and a mark applied at the one funnel cannot be forgotten by
// the tenth caller the way a mark copied into ten format strings can.
func (e *Engine) told(ctx context.Context, sc domain.Scope, text string, err error) error {
	_ = e.tr.Send(ctx, transport.Outbound{ChatID: sc.ChatID, Text: transport.GlyphProblem + " " + text})
	return marked(err)
}

// marked is told without the sending, for the one shape where the notice the member
// needed was the confirmation the flow already tried to send. Repeating it would risk
// two "Saved …" lines for one write, which reads as two writes; saying something else
// would contradict a confirmation that may well have arrived.
func marked(err error) error { return fmt.Errorf("%w; %w", ErrMemberNotified, err) }

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
