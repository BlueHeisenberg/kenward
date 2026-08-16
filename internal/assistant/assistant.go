// Package assistant runs one conversation's turns, end to end.
//
// A Unit is one member's assistant, or the household group's. It is the thing that
// runs as a goroutine in simple mode and as a pod in isolated mode, and it does not
// know which: every dependency arrives through Deps, every policy through Options,
// and nothing in this package consults configuration, the environment or any global.
// If a change to Unit needs to ask what mode it is in, that change is wrong.
//
// A turn is the sequence fixed in docs/IMPLEMENTATION.md section 5: resolve scope,
// ensure session, retrieve concurrently per space, assemble the prompt exactly as
// docs/PROMPT.md specifies, route, reply, run capture, record history. The prompt is
// a product surface and its rendered output is golden-tested; changing wording is a
// deliberate fixture edit, not a refactor.
//
// One consequence of the turn's shape deserves an operator's attention: the prompt is
// assembled before the router picks an endpoint, so a Unit budgets against a single
// context size — Options.ContextBudget — which must be the smallest context window of
// any endpoint its tier chain can reach. Mixing endpoints with materially different
// context windows inside one tier therefore wastes the larger ones: every prompt is
// trimmed to fit the smallest machine that might answer.
package assistant

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/scope"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// ResolveFunc turns an inbound message into the turn's authorization decision.
//
// It is a function rather than a concrete dependency on the scope package so that a
// Unit can be tested against fixed scopes; production wiring uses ConfigResolver. An
// unrecognised sender must resolve to scope.ErrNotEnrolled, which the Unit answers
// with silence — not a refusal, because even a refusal confirms the bot exists.
type ResolveFunc func(in transport.Inbound) (domain.Scope, error)

// ConfigResolver adapts scope.Resolve over a fixed configuration. It is what the
// supervisor hands to New in both modes.
func ConfigResolver(cfg *config.Config) ResolveFunc {
	return func(in transport.Inbound) (domain.Scope, error) {
		return scope.Resolve(cfg, in)
	}
}

// Deps are the seams a Unit works through. All of them are required except Logger.
// They are constructor-injected and never reached for globally, which is what lets
// the same Unit run in one address space with its siblings or alone in a pod.
type Deps struct {
	// Resolve is the authorization boundary for every inbound message.
	Resolve ResolveFunc
	// Memory is retrieval and, through the capture engine, storage.
	Memory memory.Memory
	// Router walks the scope's tier chain and never widens it.
	Router routing.Router
	// Transport carries replies, refusals and capture questions.
	Transport transport.Transport
	// Sessions says whether a member's key is currently unwrapped.
	Sessions session.Sessions
	// Capture runs the confirmation state machine for proposed memory writes.
	Capture *capture.Engine
	// Logger receives dropped tool calls and degraded retrievals. Optional; nil
	// discards.
	Logger *slog.Logger
}

func (d Deps) validate() error {
	switch {
	case d.Resolve == nil:
		return errors.New("assistant: Deps.Resolve is required")
	case d.Memory == nil:
		return errors.New("assistant: Deps.Memory is required")
	case d.Router == nil:
		return errors.New("assistant: Deps.Router is required")
	case d.Transport == nil:
		return errors.New("assistant: Deps.Transport is required")
	case d.Sessions == nil:
		return errors.New("assistant: Deps.Sessions is required")
	case d.Capture == nil:
		return errors.New("assistant: Deps.Capture is required")
	}
	return nil
}

// Defaults applied by New to a zero-valued Options.
const (
	// DefaultSearchLimit matches memory.search_limit in the configuration.
	DefaultSearchLimit = 8
	// DefaultHistoryLimit bounds the unit-local history ring, in turns.
	DefaultHistoryLimit = 20
	// DefaultContextBudget is the assumed endpoint context window, in tokens as this
	// package estimates them. The Unit cannot know which endpoint will answer — the
	// router decides after the prompt is assembled — so the budget is the smallest
	// window in the pool, supplied by wiring.
	DefaultContextBudget = 8192
	// DefaultMaxTokens bounds the completion and is reserved out of the budget.
	DefaultMaxTokens = 1024
	// DefaultQueueLimit bounds how many messages may wait behind a running turn.
	DefaultQueueLimit = 8
	// DefaultQueueNoticeAfter is how long a queued message waits before the member
	// is told it is queued. Short waits are normal and telling the member about
	// every one of them would be noise.
	DefaultQueueNoticeAfter = 2 * time.Second
)

// Options tunes a Unit. The zero value is valid and gets the defaults.
type Options struct {
	// HouseholdName is rendered into the prompt's identity and scope disclosure.
	HouseholdName string
	// SearchLimit is the per-space retrieval budget for one turn. Defaults to
	// DefaultSearchLimit.
	SearchLimit int
	// HistoryLimit bounds the history ring in turns. Defaults to
	// DefaultHistoryLimit. History is in memory only and is never written to lore:
	// lore holds distilled knowledge, not transcripts.
	HistoryLimit int
	// ContextBudget is the endpoint context window in estimated tokens. Defaults to
	// DefaultContextBudget.
	//
	// Set it to the smallest context window of any endpoint reachable through this
	// conversation's tier chain: the router chooses the endpoint after the prompt
	// is assembled, so the budget must hold everywhere the turn might land. This
	// also means endpoints with materially different context windows are better
	// kept in different tiers — inside one tier, the smallest window trims every
	// prompt, wasting the larger machines.
	ContextBudget int
	// MaxTokens bounds the completion. Defaults to DefaultMaxTokens.
	MaxTokens int
	// Temperature is passed through to the router unchanged. Nil means unset —
	// the provider's default — and a pointer to zero means greedy sampling, which
	// the routing seam keeps distinguishable on purpose.
	Temperature *float64
	// QueueLimit bounds how many messages may wait behind a running turn; beyond
	// it the member is told their message was dropped. Defaults to
	// DefaultQueueLimit.
	QueueLimit int
	// QueueNoticeAfter is how long a queued message waits in silence before the
	// member is told it is queued. Defaults to DefaultQueueNoticeAfter.
	QueueNoticeAfter time.Duration
	// Now supplies the prompt's date. Defaults to time.Now; tests fix it so the
	// rendered prompt is stable.
	Now func() time.Time
}

func (o Options) normalized() Options {
	if o.SearchLimit < 1 {
		o.SearchLimit = DefaultSearchLimit
	}
	if o.HistoryLimit < 1 {
		o.HistoryLimit = DefaultHistoryLimit
	}
	if o.ContextBudget < 1 {
		o.ContextBudget = DefaultContextBudget
	}
	if o.MaxTokens < 1 {
		o.MaxTokens = DefaultMaxTokens
	}
	if o.QueueLimit < 1 {
		o.QueueLimit = DefaultQueueLimit
	}
	if o.QueueNoticeAfter <= 0 {
		o.QueueNoticeAfter = DefaultQueueNoticeAfter
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Notices the Unit sends around a turn rather than as part of one. They are product
// surface like the refusals and are golden-tested.
const (
	// lockedText is sent when a direct conversation arrives while the member's key
	// is not unwrapped. The turn stops there: retrieval without a session would be
	// retrieval without the member present.
	//
	// It deliberately does not invite the member to send anything. There is no
	// unlock flow over Telegram and there cannot be one: a passphrase typed into a
	// chat travels through Telegram, stays in the member's own history, and in
	// simple mode is readable by whoever holds the bot token. Passphrases are
	// supplied to the process when it starts and never travel in a message, so a
	// prompt that hinted otherwise would train exactly the habit the design
	// refuses to support.
	lockedText = "Your assistant is locked. It needs to be unlocked on the machine it runs on."
	// contentFilterText is sent when the model declined the turn. It reports what
	// happened and stops: the assistant neither apologises for the model nor
	// explains a policy it cannot see.
	contentFilterText = "The model declined to answer this."
	// queuedText is sent when a message has waited behind a long-running turn for
	// more than Options.QueueNoticeAfter.
	queuedText = "Still working on your last message — this one is queued and I'll take it next."
	// droppedText is sent when the queue behind a running turn is full. Dropping
	// with a notice is honest; dropping silently would read as being ignored.
	droppedText = "I'm backed up and had to drop that message. Send it again in a moment."
	// noAnswerText is sent when the turn ran to the end and produced nothing the
	// member could see: the model returned no text and no tool call, or it returned
	// only a tool call whose proposal the capture engine suppressed without asking.
	// Neither is a failure the node can classify further, and neither is a reason to
	// go quiet — a turn that ends in silence teaches a household the assistant is
	// broken (IMPLEMENTATION.md sections 5 and 10).
	noAnswerText = "I didn't get a usable answer to that. Try asking again."
)

// Unit is one conversation's assistant: a member's direct chat, or the household
// group's. It owns nothing shared — no state keyed by member id lives anywhere in
// this package — so units are isolated from one another by construction, in both
// deployment modes.
//
// Concurrency policy: turns are serialised. A Unit runs at most one turn at a time;
// messages arriving mid-turn wait in a bounded queue (Options.QueueLimit), the member
// is told when their message has been queued for longer than Options.QueueNoticeAfter,
// and messages beyond the bound are dropped with a notice. Order between messages
// queued concurrently is not guaranteed — Telegram's own delivery makes no such
// promise either — but no message is ever processed twice and no state is touched
// outside the running turn.
//
// The turn slot covers the work the node does — retrieval, routing, the reply — and
// deliberately not the capture question, which waits on the member. A member who
// ignores the buttons and asks something else must get an answer, not a queue notice
// blaming them for a turn that is really waiting on their own tap.
type Unit struct {
	deps Deps
	opts Options

	// slot serialises turns: it has capacity one and holding it is holding the turn.
	slot chan struct{}
	// waitersMu guards waiters, the count of messages queued behind the slot.
	waitersMu sync.Mutex
	waiters   int

	// turnSeq makes every turn token unique. Telegram message ids alone are not
	// trusted for this: a repeated or zero MessageID would repeat the token, and a
	// repeated token reads to the capture engine as the same turn, silently
	// spending a budget that should have been fresh.
	turnSeq atomic.Uint64

	// history is the unit-local turn ring. It is only written by the running turn
	// but carries its own lock so a snapshot is always consistent.
	history *historyRing
}

// New builds a Unit over its dependencies. It fails fast on a missing dependency
// rather than letting the first turn discover the hole.
func New(deps Deps, opts Options) (*Unit, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	opts = opts.normalized()
	// The completion is reserved out of the context budget, so a reservation the
	// budget cannot hold is a configuration contradiction: honouring it would leave
	// nothing for the prompt, and ignoring it would assemble prompts that fill the
	// whole window and starve the answer. Failing here is the only honest option.
	if opts.MaxTokens >= opts.ContextBudget {
		return nil, fmt.Errorf("assistant: MaxTokens (%d) must be smaller than ContextBudget (%d): the completion is reserved out of the context window", opts.MaxTokens, opts.ContextBudget)
	}
	return &Unit{
		deps:    deps,
		opts:    opts,
		slot:    make(chan struct{}, 1),
		history: newHistoryRing(opts.HistoryLimit),
	}, nil
}

// Handle runs one turn for one inbound message.
//
// An unrecognised sender returns scope.ErrNotEnrolled (wrapped) and sends nothing at
// all — the caller drops it in the same silence. Infrastructure failures are returned
// for the caller to log; refusals and notices are not errors, they are the product
// answering, and Handle returns nil for them.
//
// Cancellation of ctx stops the turn at the next boundary: nothing is sent after the
// context is done, a capture question in flight is abandoned by the transport, and a
// turn that never delivered its reply is not recorded in history.
func (u *Unit) Handle(ctx context.Context, in transport.Inbound) error {
	sc, err := u.deps.Resolve(in)
	if err != nil {
		// Silence is deliberate: replying at all, even to refuse, confirms to a
		// stranger that this bot is a kenward node serving a real household.
		return fmt.Errorf("assistant: resolving scope: %w", err)
	}

	release, err := u.admit(ctx, sc, in)
	if err != nil || release == nil {
		return err
	}
	defer release()

	followup, err := u.turn(ctx, sc, in)
	if err != nil {
		return err
	}
	if followup != nil {
		// The capture question waits on the member, not on the node, so it must
		// not hold the turn slot: a member who ignores the buttons and asks
		// something else gets an answer, not a frozen assistant. release is
		// idempotent; the deferred call becomes a no-op.
		release()
		followup(ctx)
	}
	return nil
}

// admit serialises the turn. It returns an idempotent release function once the
// turn slot is held, or (nil, nil) when the message was dropped because the queue
// was full. The scope is already resolved when admit runs, so its notices never
// reach a stranger.
func (u *Unit) admit(ctx context.Context, sc domain.Scope, in transport.Inbound) (func(), error) {
	release := func() func() {
		var once sync.Once
		return func() { once.Do(func() { <-u.slot }) }
	}

	select {
	case u.slot <- struct{}{}:
		return release(), nil
	default:
	}

	// A turn is in flight. Join the bounded queue or drop with a notice.
	u.waitersMu.Lock()
	if u.waiters >= u.opts.QueueLimit {
		u.waitersMu.Unlock()
		if err := u.send(ctx, sc, in, droppedText); err != nil {
			return nil, fmt.Errorf("assistant: reporting dropped message: %w", err)
		}
		return nil, nil
	}
	u.waiters++
	u.waitersMu.Unlock()
	defer func() {
		u.waitersMu.Lock()
		u.waiters--
		u.waitersMu.Unlock()
	}()

	notice := time.NewTimer(u.opts.QueueNoticeAfter)
	defer notice.Stop()
	for {
		select {
		case u.slot <- struct{}{}:
			return release(), nil
		case <-notice.C:
			// The wait is long enough to be felt; say so, once, then keep waiting.
			if err := u.send(ctx, sc, in, queuedText); err != nil {
				u.deps.Logger.Warn("assistant: queue notice failed", "error", err)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// turn is one full pass of the sequence in docs/IMPLEMENTATION.md section 5. The
// caller holds the turn slot. The returned followup, when non-nil, is the capture
// question; the caller runs it after releasing the slot, because it waits on the
// member rather than on the node.
func (u *Unit) turn(ctx context.Context, sc domain.Scope, in transport.Inbound) (func(context.Context), error) {
	// Session. Only a member has a key; the group conversation has no session of
	// its own, so a group turn proceeds without one.
	if sc.Member != nil {
		if _, ok := u.deps.Sessions.Key(sc.Member.ID); !ok {
			return nil, u.send(ctx, sc, in, lockedText)
		}
		u.deps.Sessions.Touch(sc.Member.ID)
	}

	// The capture engine ages its decline window per turn whether or not this turn
	// proposes anything. The sequence number makes the token unique even when the
	// transport repeats a message id, because a repeated token would read as the
	// same turn and silently spend a budget that should have been fresh.
	u.deps.Capture.BeginTurn(sc, fmt.Sprintf("%d:%d:%d", in.ChatID, in.MessageID, u.turnSeq.Add(1)))

	// Retrieve, one search per space in scope order, concurrently. The groups stay
	// grouped: ranking a private space against the shared one is a policy decision
	// this package makes only by keeping scope order, never by re-ranking.
	groups := u.retrieve(ctx, sc, in.Text)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.err != nil {
			u.deps.Logger.Warn("assistant: retrieval degraded",
				"space", string(g.space), "error", g.err)
		}
	}

	// Assemble within budget, then route. The chain is the scope's and only the
	// scope's: a *routing.NoBackendError comes back when it is exhausted, and it
	// becomes an explicit refusal — the node speaks it directly, because a model
	// that cannot be reached cannot explain why.
	req := u.assemble(sc, groups, in.Text)
	comp, err := u.deps.Router.Complete(ctx, sc.Tiers, req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var nbe *routing.NoBackendError
		if errors.As(err, &nbe) {
			return nil, u.send(ctx, sc, in, refusalText(sc, nbe))
		}
		// Every other failure still gets the member a reply. A member who sends a
		// message always gets one — a stack trace in a log the operator reads next
		// week is not an answer, and silence teaches a household the assistant is
		// broken and unpredictable. The error goes to the log; a short classified
		// notice goes to the chat.
		u.deps.Logger.Warn("assistant: turn failed", "error", err)
		return nil, u.send(ctx, sc, in, completionFailureText(err))
	}

	// A content filter is the model declining, which is a final answer: the member
	// is told what happened, briefly, and nothing else runs — no reply, no capture,
	// no history. The routing seam already guarantees the refused content was not
	// re-offered to another machine.
	if comp.FinishReason == routing.FinishContentFilter {
		return nil, u.send(ctx, sc, in, contentFilterText)
	}

	proposal, warn := extractProposal(comp.ToolCalls)
	if warn != "" {
		// A malformed tool call is dropped with a log line, never a crashed turn
		// and never a write. Nothing is written without the member's button press
		// regardless; this just spares them a broken question.
		u.deps.Logger.Warn("assistant: remember call dropped", "reason", warn)
	}
	reply, warn := sanitizeReply(comp.Text)
	if warn != "" {
		u.deps.Logger.Warn("assistant: reply text sanitized", "reason", warn)
	}

	// A publish request resolves to an entry id or to nothing. A title that did not
	// come back from this turn's search in this scope is dropped with a log line,
	// exactly like a malformed remember: the node does not guess at an id, because
	// an id it did not retrieve itself is an id it cannot vouch for.
	publishID := ""
	title, warn := extractPublishTitle(comp.ToolCalls)
	if title != "" {
		var ok bool
		if publishID, ok = publishTarget(sc, groups, title); !ok {
			warn = joinWarn(warn, "publish title matched no single entry retrieved from this scope this turn")
		}
	}
	if warn != "" {
		u.deps.Logger.Warn("assistant: publish call dropped", "reason", warn)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reply != "" {
		if err := u.send(ctx, sc, in, reply); err != nil {
			return nil, fmt.Errorf("assistant: sending reply: %w", err)
		}
		// Record only a turn with both sides delivered. A turn whose reply was
		// empty — a bare tool call — is not recorded at all: an empty assistant
		// side would leave two consecutive user messages in the next request,
		// which several local chat templates reject or silently merge. History is
		// unit-local, bounded and in memory only; it is never written to lore.
		u.history.add(in.Text, reply)
	} else if proposal == nil && publishID == "" {
		// Nothing to say, nothing to propose and nothing to publish. The model
		// misbehaved, but the member still sent a message, so they still get an
		// answer.
		u.deps.Logger.Warn("assistant: model returned an empty reply", "chat", in.ChatID)
		return nil, u.send(ctx, sc, in, noAnswerText)
	}

	// A publish is something the member asked for; a remember proposal is something
	// the assistant volunteered. If the model does both in one turn the request
	// wins, and either way exactly one question reaches the member.
	if publishID != "" {
		return func(fctx context.Context) {
			if _, err := u.deps.Capture.OfferPromotion(fctx, sc, publishID, in.UserID); err != nil {
				// As with capture, the engine has already spoken to the member
				// wherever they saw anything; this is for the operator.
				u.deps.Logger.Warn("assistant: publish failed", "error", err)
				// Except where it has not. A publish call is routinely the model's
				// whole turn — the prompt tells it to call the tool and say nothing —
				// so a failure the engine did not speak for leaves the member with no
				// message at all. The engine's own notice wins where there is one:
				// after a failed publication this generic notice would read as an
				// invitation to retry something that cannot be taken back.
				if reply == "" && !errors.Is(err, capture.ErrMemberNotified) {
					if err := u.send(fctx, sc, in, noAnswerText); err != nil {
						u.deps.Logger.Warn("assistant: reporting an empty turn", "error", err)
					}
				}
			}
		}, nil
	}
	if proposal == nil {
		return nil, nil
	}
	// The member who spoke is the member who decides; in the group chat the
	// transport ignores taps from anyone else. The question runs after the caller
	// releases the turn slot — it waits on a button, not on the node.
	p := *proposal
	return func(fctx context.Context) {
		out, err := u.deps.Capture.Offer(fctx, sc, p, in.UserID)
		if err != nil {
			// The capture engine speaks for every failure the member could be
			// waiting on, including the ones on a bare-tool-call turn; this is
			// for the operator. There is deliberately no generic fallback here
			// to mirror the publish callback's: an unmarked error says nothing
			// about whether the write landed, so "try asking again" would be
			// safe for a failed one and an invitation to duplicate a stored one.
			// The engine is where that distinction exists, so the engine speaks.
			u.deps.Logger.Warn("assistant: capture failed", "error", err)
			return
		}
		// A suppressed proposal asks nothing, by design. On a turn that also had
		// no reply text that leaves the member with nothing at all, so the node
		// says so — the suppression is the assistant's business, not theirs.
		if reply == "" && (out.Kind == capture.OutcomeDuplicate || out.Kind == capture.OutcomeLimited) {
			if err := u.send(fctx, sc, in, noAnswerText); err != nil {
				u.deps.Logger.Warn("assistant: reporting an empty turn", "error", err)
			}
		}
	}, nil
}

// publishTarget resolves a publish request to an entry id using this turn's own
// retrieval, and nothing else.
//
// This is the whole reason the publish tool takes a title rather than an id. lore's
// ids are global and lore_get is not space-scoped, so an id is a capability: whoever
// holds one can name an entry in any space, including one this conversation may not
// read. An id must therefore originate from a search performed inside the current
// Scope (CLAUDE.md; IMPLEMENTATION.md section 12), and the model is not a source —
// everything it writes is derived from member text. So the model names a title, and
// the id comes from the search hit that title matched, in the space this scope writes.
//
// Anything less than exactly one match resolves to nothing: an unmatched title is a
// title the member's private memory did not return this turn, and two entries sharing
// a title is a choice the node must not make on the member's behalf.
func publishTarget(sc domain.Scope, groups []spaceGroup, title string) (string, bool) {
	if !sc.AllowsPrivateCapture() {
		return "", false
	}
	want := normalizeTitle(title)
	var id string
	for _, g := range groups {
		if g.space != sc.Write {
			continue
		}
		for _, e := range g.entries {
			if normalizeTitle(e.Title) != want || e.ID == "" {
				continue
			}
			if id != "" {
				return "", false
			}
			id = e.ID
		}
	}
	return id, id != ""
}

// normalizeTitle makes the match insensitive to the cosmetic differences a model
// produces when it copies a title back out of the prompt.
func normalizeTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

// spaceGroup is one space's retrieval result, kept in scope order.
type spaceGroup struct {
	space   domain.SpaceID
	entries []memory.Entry
	err     error
}

// retrieve searches every space in sc.Read, concurrently, and returns the results
// grouped in scope order. A failed search degrades that group rather than failing
// the turn; the prompt says the space could not be read, because an error rendered
// as "nothing found" would be a lie the member might act on.
//
// The member's message is not the query. lore matches conjunctively over bare words
// (memory.Terms), so "what is the boiler service code" retrieves nothing from a store
// whose entry reads "the boiler service code is ..." — "what" is not in it. That is
// not an edge case, it is how people ask questions, and a node that answers "the
// household has not recorded that" while holding the answer is worse than one that
// says nothing.
//
// So each content word is searched on its own and the hits are unioned here, ranked by
// how many of them found an entry. Retrieval then degrades instead of failing outright:
// one relevant word among six filler ones still finds the entry, and the entry every
// word found still sorts above it. Nothing is re-ranked across spaces — the groups stay
// grouped, in scope order — because that remains a policy decision, and this is not it.
func (u *Unit) retrieve(ctx context.Context, sc domain.Scope, text string) []spaceGroup {
	terms := searchTerms(text)
	groups := make([]spaceGroup, len(sc.Read))
	var wg sync.WaitGroup
	for i, sp := range sc.Read {
		wg.Add(1)
		go func(i int, sp domain.SpaceID) {
			defer wg.Done()
			groups[i] = u.searchSpace(ctx, sp, terms)
		}(i, sp)
	}
	wg.Wait()
	return groups
}

// searchSpace runs one search per term against one space and unions the hits.
//
// Any term failing fails the group. A union assembled from the searches that happened
// to succeed is a narrower answer presented as a complete one, which is the same lie
// as rendering an error as "nothing found".
func (u *Unit) searchSpace(ctx context.Context, sp domain.SpaceID, terms []string) spaceGroup {
	hits := make([][]memory.Entry, len(terms))
	errs := make([]error, len(terms))
	var wg sync.WaitGroup
	for i, term := range terms {
		wg.Add(1)
		go func(i int, term string) {
			defer wg.Done()
			hits[i], errs[i] = u.deps.Memory.Search(ctx, memory.SearchQuery{
				Text:   term,
				Spaces: []domain.SpaceID{sp},
				Limit:  u.opts.SearchLimit,
			})
		}(i, term)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return spaceGroup{space: sp, err: err}
	}
	return spaceGroup{space: sp, entries: rankUnion(hits, u.opts.SearchLimit)}
}

// maxSearchTerms bounds the searches one turn may make per space.
//
// It has been suspected of costing recall once, when the live suite's store had
// accumulated nine near-identical entries and the one term that told them apart sat
// seventh in the question. Measured against a real store, raising the cap fixed
// nothing: the discriminating terms were being searched and their hits discarded, in
// rankUnion, which is where the fix went. The cap stays where it is until something
// measured argues otherwise.
//
// ponytail: fixed cap, and it is the first terms that survive — a question's subject
// is usually near its start. A message longer than this is searched on its opening
// words only, and the same measurement showed three of six slots going to "hey",
// "kenward" and "remind", which found nothing at all. Widening searchStopwords buys
// those slots back for a longer question, if one is ever shown to need them.
const maxSearchTerms = 6

// searchStopwords are the words dropped before searching.
//
// They are not dropped because lore ignores them — lore has none, and treats "the" as
// a word an entry must contain like any other. They are dropped because each one costs
// a search and buys hits on every entry that happens to contain it, which is noise the
// ranking then has to out-vote.
//
// ponytail: hand-written English list, and a household speaking anything else gets no
// benefit from it (correctness is unaffected — a stopword that survives is just a
// wasted search). A real stopword source, or scoring by term rarity, when that matters.
var searchStopwords = map[string]bool{
	"a": true, "about": true, "am": true, "an": true, "and": true, "any": true,
	"are": true, "as": true, "at": true, "be": true, "been": true, "but": true,
	"by": true, "can": true, "could": true, "did": true, "do": true, "does": true,
	"for": true, "from": true, "get": true, "had": true, "has": true, "have": true,
	"how": true, "i": true, "if": true, "in": true, "is": true, "it": true,
	"its": true, "just": true, "know": true, "me": true, "my": true, "of": true,
	"on": true, "or": true, "our": true, "please": true, "s": true, "she": true,
	"should": true, "so": true, "such": true, "tell": true, "than": true,
	"that": true, "the": true, "their": true, "them": true, "then": true,
	"there": true, "these": true, "they": true, "this": true, "to": true,
	"us": true, "was": true, "we": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "who": true, "why": true, "will": true,
	"with": true, "would": true, "you": true, "your": true,
}

// searchTerms picks the words from a member's message that are worth a search:
// lore's own tokens, minus stopwords, minus repeats, capped.
//
// A message with nothing left — a greeting, an emoji — yields no terms and therefore
// no searches, which is the honest result. Searching for "hi" would only find entries
// that say "hi".
func searchTerms(text string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, w := range memory.Terms(text) {
		// Single characters are dropped with the stopwords: no entry is found by
		// "a" that is not found better by the word next to it.
		if len(w) < 2 || searchStopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		if out = append(out, w); len(out) == maxSearchTerms {
			break
		}
	}
	return out
}

// rankUnion merges one space's per-term hits into a single result, best first, and
// truncates to limit.
//
// A term is worth what it narrowed down. Counting terms alone — one point per term
// that found an entry — sounds right and measurably loses the answer, because the
// per-term search budget is the same limit this function truncates to: a word the
// store holds many entries for returns that whole budget by itself, and two such
// words fill every slot before a precise word is looked at. Measured against a real
// lore store holding a household's worth of entries, "hey kenward, can you remind me
// what the wifi password for the guest cottage is?" put twelve wifi entries in front
// of the searches for "guest" and "cottage", each of which found the guest cottage
// entry and nothing else. Under term counting the right entry scored two, the eight
// wrong ones also scored two, the tie broke on arrival order, and the one entry that
// answered the question was the one cut. Raising the term cap does not touch this;
// the discriminating terms were searched and their answer was discarded.
//
// So each term contributes 1/(entries it found) instead of 1. A word that matched one
// entry is worth a whole point to it; a word that filled the budget is worth an eighth
// to each of them. An entry every word found still outranks an entry one word brushed,
// because the weights sum. The count of hits is already in hand, so this costs a
// division and no extra search.
//
// ponytail: hit count within one space is a crude rarity proxy — it saturates at the
// search limit, so "found in 8" and "found in 800" weigh the same, and it says nothing
// about the store as a whole. Real inverse document frequency, if the saturation
// starts mattering. Ties keep the order they arrived in, which is lore's own relevance
// ordering for the earliest term that found them. Entries are identified by id; lore
// always gives one, and an entry without one is treated as its own hit rather than
// silently merged with a stranger that shares a title.
func rankUnion(hits [][]memory.Entry, limit int) []memory.Entry {
	type ranked struct {
		entry memory.Entry
		score float64
	}
	var out []*ranked
	index := make(map[string]*ranked)
	for _, group := range hits {
		if len(group) == 0 {
			continue
		}
		weight := 1 / float64(len(group))
		// One term matching an entry twice is still one term, so each search's
		// contribution to a score is counted once.
		counted := make(map[string]bool)
		for _, e := range group {
			key := e.ID
			if key == "" {
				key = "\x00" + e.Title + "\x00" + e.Body
			}
			r, ok := index[key]
			if !ok {
				r = &ranked{entry: e}
				index[key] = r
				out = append(out, r)
			}
			if !counted[key] {
				counted[key] = true
				r.score += weight
			}
		}
	}
	slices.SortStableFunc(out, func(a, b *ranked) int { return cmp.Compare(b.score, a.score) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	entries := make([]memory.Entry, len(out))
	for i, r := range out {
		entries[i] = r.entry
	}
	return entries
}

// send delivers one outbound message in this scope. Group replies quote the message
// they answer, because several conversations interleave there; direct replies do not,
// because quoting the only other party is noise.
func (u *Unit) send(ctx context.Context, sc domain.Scope, in transport.Inbound, text string) error {
	replyTo := 0
	if sc.Kind == domain.ScopeGroup {
		replyTo = in.MessageID
	}
	return u.deps.Transport.Send(ctx, transport.Outbound{
		ChatID:  sc.ChatID,
		Text:    text,
		ReplyTo: replyTo,
	})
}
