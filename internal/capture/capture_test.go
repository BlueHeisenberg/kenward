package capture

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// The transport package does not ship a fake yet, so these tests drive local stubs that
// implement the two seams. They are deliberately dumb: the assertions live in the tests.

type askCall struct {
	q transport.Question
}

type putCall struct {
	space domain.SpaceID
	draft memory.Draft
}

type shareCall struct {
	from, to domain.SpaceID
	id       string
}

type stubTransport struct {
	answers []transport.Answer
	askErr  error
	sendErr error
	asks    []askCall
	sends   []transport.Outbound
}

func (t *stubTransport) Updates(context.Context) (<-chan transport.Inbound, error) {
	return nil, transport.ErrClosed
}

func (t *stubTransport) Send(_ context.Context, o transport.Outbound) error {
	t.sends = append(t.sends, o)
	return t.sendErr
}

func (t *stubTransport) Ask(_ context.Context, q transport.Question) (transport.Answer, error) {
	t.asks = append(t.asks, askCall{q: q})
	if t.askErr != nil {
		return transport.Answer{}, t.askErr
	}
	if len(t.answers) == 0 {
		return transport.Answer{}, errors.New("stub transport: no scripted answer")
	}
	a := t.answers[0]
	t.answers = t.answers[1:]
	return a, nil
}

// SendTyping is inert here. The capture engine waits on a member, not on a model,
// so nothing in this package ever shows an indicator; the method exists because the
// engine holds a transport.Transport.
func (t *stubTransport) SendTyping(context.Context, int64) error { return nil }

func (t *stubTransport) Close() error { return nil }

func (t *stubTransport) lastChoiceIDs() []string {
	if len(t.asks) == 0 {
		return nil
	}
	return choiceIDs(t.asks[len(t.asks)-1].q)
}

func choiceIDs(q transport.Question) []string {
	ids := make([]string, 0, len(q.Choices))
	for _, c := range q.Choices {
		ids = append(ids, c.ID)
	}
	return ids
}

type stubMemory struct {
	entry     memory.Entry
	getErr    error
	putErr    error
	shareErr  error
	deleteErr error
	puts      []putCall
	shares    []shareCall
	gets      []string
	deletes   []string
	// putSpace and shareSpace, when set, make the stub report the write landed in
	// a different space than the one requested — a store that misroutes.
	putSpace   domain.SpaceID
	shareSpace domain.SpaceID
}

func (m *stubMemory) Search(context.Context, memory.SearchQuery) ([]memory.Entry, error) {
	return nil, nil
}

func (m *stubMemory) Get(_ context.Context, space domain.SpaceID, id string) (memory.Entry, error) {
	m.gets = append(m.gets, string(space)+"/"+id)
	if m.getErr != nil {
		return memory.Entry{}, m.getErr
	}
	e := m.entry
	e.Space, e.ID = space, id
	return e, nil
}

func (m *stubMemory) Put(_ context.Context, space domain.SpaceID, d memory.Draft) (memory.Entry, error) {
	m.puts = append(m.puts, putCall{space: space, draft: d})
	if m.putErr != nil {
		return memory.Entry{}, m.putErr
	}
	if m.putSpace != "" {
		space = m.putSpace
	}
	return memory.Entry{ID: "entry-1", Space: space, Title: d.Title, Body: d.Body}, nil
}

func (m *stubMemory) Share(_ context.Context, from, to domain.SpaceID, id string) (memory.Entry, error) {
	m.shares = append(m.shares, shareCall{from: from, to: to, id: id})
	if m.shareErr != nil {
		return memory.Entry{}, m.shareErr
	}
	if m.shareSpace != "" {
		to = m.shareSpace
	}
	return memory.Entry{ID: id, Space: to, Title: m.entry.Title}, nil
}

// Delete records the call and fails the way lore fails. deleteErr is not decoration:
// undo has three endings — gone, still there, and unknown — and the two that are not
// "gone" only exist if the double can produce them. A fake that always succeeds would
// let the code claim an entry was removed on a path where it never was, which is the
// exact class of defect this file's other error hooks were added for.
func (m *stubMemory) Delete(_ context.Context, space domain.SpaceID, id string) error {
	m.deletes = append(m.deletes, string(space)+"/"+id)
	return m.deleteErr
}

func (m *stubMemory) Close() error { return nil }

const (
	personal = domain.SpaceID("david-private")
	shared   = domain.SpaceID("household")
	davidID  = int64(4242)
	otherID  = int64(9999)
)

func directScope() domain.Scope {
	m := &domain.Member{ID: "david", Name: "David", TelegramID: davidID, Private: personal}
	return domain.Scope{
		Kind:   domain.ScopeDirect,
		Member: m,
		Write:  personal,
		Read:   []domain.SpaceID{personal, shared},
		ChatID: 100,
	}
}

func groupScope() domain.Scope {
	return domain.Scope{
		Kind:   domain.ScopeGroup,
		Write:  shared,
		Read:   []domain.SpaceID{shared},
		ChatID: -100,
	}
}

func proposal(target Target) Proposal {
	return Proposal{
		Draft: memory.Draft{
			Domain:     "household",
			Title:      "Bins go out Tuesday",
			Body:       "Green bin on alternate weeks.",
			Confidence: "settled",
		},
		Target: target,
	}
}

func newEngine(t *testing.T, answers ...transport.Answer) (*Engine, *stubMemory, *stubTransport) {
	t.Helper()
	return newEngineWith(t, Options{}, answers...)
}

// newEngineWith is newEngine for a test that cares about the policy. The default
// engine writes a personal proposal without asking, so a test about the question is a
// test that has to say which policy it is exercising.
func newEngineWith(t *testing.T, opts Options, answers ...transport.Answer) (*Engine, *stubMemory, *stubTransport) {
	t.Helper()
	m := &stubMemory{}
	tr := &stubTransport{answers: answers}
	return New(m, tr, opts), m, tr
}

func accept(choice string) transport.Answer {
	return transport.Answer{ChoiceID: choice, UserID: davidID}
}

// TestOfferButtons walks every row of the capture table — every case that puts a
// question, which is every case except a personal target under the default policy.
// That one is TestPrivateTargetIsWrittenThenAnnounced; the row here is the same
// proposal under PrivateWriteAsk, which is what a household turning the policy back
// buys and is the only thing keeping the old path from rotting.
func TestOfferButtons(t *testing.T) {
	tests := []struct {
		name    string
		scope   domain.Scope
		target  Target
		opts    Options
		want    []string
		answer  transport.Answer
		wantOut OutcomeKind
		wantIn  domain.SpaceID
	}{
		{
			name:  "direct unsure offers both destinations",
			scope: directScope(), target: TargetUnsure,
			want:   []string{ChoicePersonal, ChoiceShared, ChoiceDecline},
			answer: accept(ChoicePersonal), wantOut: OutcomeSaved, wantIn: personal,
		},
		{
			name:  "direct unsure can still choose the household",
			scope: directScope(), target: TargetUnsure,
			want:   []string{ChoicePersonal, ChoiceShared, ChoiceDecline},
			answer: accept(ChoiceShared), wantOut: OutcomeSaved, wantIn: shared,
		},
		{
			name:  "direct personal under ask confirms one destination",
			scope: directScope(), target: TargetPersonal,
			opts:   Options{PrivateWrites: PrivateWriteAsk},
			want:   []string{ChoicePersonal, ChoiceDecline},
			answer: accept(ChoicePersonal), wantOut: OutcomeSaved, wantIn: personal,
		},
		{
			name:  "direct shared confirms one destination",
			scope: directScope(), target: TargetShared,
			want:   []string{ChoiceShared, ChoiceDecline},
			answer: accept(ChoiceShared), wantOut: OutcomeSaved, wantIn: shared,
		},
		{
			name:  "group unsure offers the household only",
			scope: groupScope(), target: TargetUnsure,
			want:   []string{ChoiceShared, ChoiceDecline},
			answer: accept(ChoiceShared), wantOut: OutcomeSaved, wantIn: shared,
		},
		{
			name:  "group personal is rewritten to the household",
			scope: groupScope(), target: TargetPersonal,
			want:   []string{ChoiceShared, ChoiceDecline},
			answer: accept(ChoiceShared), wantOut: OutcomeSaved, wantIn: shared,
		},
		{
			name:  "group shared offers the household only",
			scope: groupScope(), target: TargetShared,
			want:   []string{ChoiceShared, ChoiceDecline},
			answer: accept(ChoiceShared), wantOut: OutcomeSaved, wantIn: shared,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, mem, tr := newEngineWith(t, tc.opts, tc.answer)
			e.BeginTurn(tc.scope, "turn-1")

			out, err := e.Offer(context.Background(), tc.scope, proposal(tc.target), davidID)
			if err != nil {
				t.Fatalf("Offer: %v", err)
			}
			if got := tr.lastChoiceIDs(); !equal(got, tc.want) {
				t.Errorf("choices = %v, want %v", got, tc.want)
			}
			if out.Kind != tc.wantOut {
				t.Errorf("outcome = %v, want %v", out.Kind, tc.wantOut)
			}
			if out.Space != tc.wantIn {
				t.Errorf("saved to %q, want %q", out.Space, tc.wantIn)
			}
			if len(mem.puts) != 1 || mem.puts[0].space != tc.wantIn {
				t.Fatalf("puts = %+v, want one to %q", mem.puts, tc.wantIn)
			}
			if q := tr.asks[0].q; q.AllowedUserID != davidID {
				t.Errorf("AllowedUserID = %d, want %d", q.AllowedUserID, davidID)
			}
			if q := tr.asks[0].q; q.ChatID != tc.scope.ChatID {
				t.Errorf("ChatID = %d, want %d", q.ChatID, tc.scope.ChatID)
			}
			if q := tr.asks[0].q; q.Timeout != DefaultAskTimeout {
				t.Errorf("Timeout = %v, want %v", q.Timeout, DefaultAskTimeout)
			}
			if len(tr.sends) != 1 {
				t.Fatalf("sends = %v, want one confirmation", tr.sends)
			}
			if !strings.Contains(tr.sends[0].Text, "Bins go out Tuesday") ||
				!strings.Contains(tr.sends[0].Text, wantDestination(tc.scope, tc.wantIn)) {
				t.Errorf("confirmation %q lacks the title or the destination", tr.sends[0].Text)
			}
			// The space id is the operator's handle, not the member's. It took a
			// whole line of a phone screen and named nothing a member could look
			// up. The parenthesised form is what used to be appended.
			if id := "(" + string(tc.wantIn) + ")"; strings.Contains(tr.sends[0].Text, id) {
				t.Errorf("confirmation %q shows the raw space id", tr.sends[0].Text)
			}
		})
	}
}

// TestGroupScopeNeverOffersPersonal is the invariant the memory model exists for: no
// button in a group chat may write into anyone's private space, whatever the model
// proposed and whatever the private space happens to be called.
func TestGroupScopeNeverOffersPersonal(t *testing.T) {
	for _, target := range []Target{TargetUnsure, TargetPersonal, TargetShared} {
		e, mem, tr := newEngine(t, accept(ChoiceShared))
		sc := groupScope()
		e.BeginTurn(sc, "turn-1")

		if _, err := e.Offer(context.Background(), sc, proposal(target), davidID); err != nil {
			t.Fatalf("target %v: Offer: %v", target, err)
		}
		for _, c := range tr.asks[0].q.Choices {
			if c.ID == ChoicePersonal {
				t.Fatalf("target %v: group scope offered a personal destination", target)
			}
			if strings.Contains(strings.ToLower(c.Label), "personal") {
				t.Fatalf("target %v: group button labelled %q", target, c.Label)
			}
		}
		for _, p := range mem.puts {
			if p.space != shared {
				t.Fatalf("target %v: group capture wrote to %q", target, p.space)
			}
		}
	}
}

// TestPersonalChoiceRefusedInGroup covers a transport handing back a choice that was
// never offered. Nothing may be written.
func TestPersonalChoiceRefusedInGroup(t *testing.T) {
	e, mem, _ := newEngine(t, accept(ChoicePersonal))
	sc := groupScope()
	e.BeginTurn(sc, "turn-1")

	_, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID)
	if !errors.Is(err, ErrPersonalNotAllowed) {
		t.Fatalf("err = %v, want ErrPersonalNotAllowed", err)
	}
	if len(mem.puts) != 0 {
		t.Fatalf("puts = %+v, want none", mem.puts)
	}
}

// TestDeclineWritesNothing is the whole point of the package.
func TestDeclineWritesNothing(t *testing.T) {
	e, mem, tr := newEngine(t, accept(ChoiceDecline))
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeDeclined {
		t.Fatalf("outcome = %v, want declined", out.Kind)
	}
	if out.Stored() {
		t.Error("declined outcome reports a write")
	}
	if len(mem.puts) != 0 {
		t.Fatalf("puts = %+v, want none", mem.puts)
	}
	if len(mem.shares) != 0 {
		t.Fatalf("shares = %+v, want none", mem.shares)
	}
	if len(tr.sends) != 0 {
		t.Fatalf("sends = %v, want none: a decline is not announced", tr.sends)
	}
}

// TestTimeoutIsDecline: expiry never writes, and never re-asks.
func TestTimeoutIsDecline(t *testing.T) {
	e, mem, _ := newEngine(t, transport.Answer{TimedOut: true})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeTimedOut {
		t.Fatalf("outcome = %v, want timed out", out.Kind)
	}
	if len(mem.puts) != 0 {
		t.Fatalf("puts = %+v, want none", mem.puts)
	}

	// And the timed-out title is suppressed on the next turn, like any decline.
	e.BeginTurn(sc, "turn-2")
	out, err = e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeDuplicate {
		t.Fatalf("outcome = %v, want duplicate", out.Kind)
	}
}

// TestAnswerFromAnotherUser: the transport should have filtered it, but a stray tap
// still may not route someone else's memory — and may not decide anything on the
// asked member's behalf either: nothing is written, and the title is not recorded as
// their decline.
func TestAnswerFromAnotherUser(t *testing.T) {
	e, mem, _ := newEngine(t,
		transport.Answer{ChoiceID: ChoiceShared, UserID: otherID},
		accept(ChoiceShared),
	)
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeTimedOut {
		t.Fatalf("outcome = %v, want timed out (the asked member never answered)", out.Kind)
	}
	if len(mem.puts) != 0 {
		t.Fatalf("puts = %+v, want none", mem.puts)
	}

	// The stray tap was not recorded as the asked member's decline: the same title
	// may be proposed again next turn, and this time their own tap saves it.
	e.BeginTurn(sc, "turn-2")
	out, err = e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("second Offer: %v", err)
	}
	if out.Kind != OutcomeSaved {
		t.Fatalf("outcome = %v, want saved; a stray tap suppressed the title", out.Kind)
	}
}

// TestDuplicateSuppressionWindow: a declined title stays quiet for ten turns and may be
// proposed again on the eleventh.
func TestDuplicateSuppressionWindow(t *testing.T) {
	answers := []transport.Answer{accept(ChoiceDecline), accept(ChoiceShared)}
	e, mem, tr := newEngine(t, answers...)
	sc := directScope()

	e.BeginTurn(sc, "turn-1")
	if out, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID); err != nil || out.Kind != OutcomeDeclined {
		t.Fatalf("first offer: %v %v", out.Kind, err)
	}

	// Turns 2..10 inclusive: still inside the ten-turn window, never re-asked.
	for i := 2; i <= DefaultDeclineWindow; i++ {
		e.BeginTurn(sc, turnToken(i))
		out, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if out.Kind != OutcomeDuplicate {
			t.Fatalf("turn %d: outcome = %v, want duplicate", i, out.Kind)
		}
		if out.Title == "" {
			t.Errorf("turn %d: suppressed outcome lost the title", i)
		}
	}
	if len(tr.asks) != 1 {
		t.Fatalf("asks = %d, want 1: suppression must not reach the member", len(tr.asks))
	}

	// Turn 11 is outside the window.
	e.BeginTurn(sc, turnToken(DefaultDeclineWindow+1))
	out, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("turn 11: %v", err)
	}
	if out.Kind != OutcomeSaved {
		t.Fatalf("turn 11: outcome = %v, want saved", out.Kind)
	}
	if len(mem.puts) != 1 {
		t.Fatalf("puts = %+v, want one", mem.puts)
	}
}

// TestDuplicateSuppressionIgnoresCosmetics: the same title in different dress is the
// same proposal.
func TestDuplicateSuppressionIgnoresCosmetics(t *testing.T) {
	e, _, _ := newEngine(t, accept(ChoiceDecline))
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	if _, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	e.BeginTurn(sc, "turn-2")
	p := proposal(TargetUnsure)
	p.Draft.Title = "  bins   go out   TUESDAY "
	out, err := e.Offer(context.Background(), sc, p, davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeDuplicate {
		t.Fatalf("outcome = %v, want duplicate", out.Kind)
	}
}

// TestDeclineHistoryIsPerScope: refusing something in a direct chat must not silence the
// household chat, and vice versa.
func TestDeclineHistoryIsPerScope(t *testing.T) {
	e, _, tr := newEngine(t, accept(ChoiceDecline), accept(ChoiceShared))
	direct, group := directScope(), groupScope()

	e.BeginTurn(direct, "turn-1")
	if _, err := e.Offer(context.Background(), direct, proposal(TargetUnsure), davidID); err != nil {
		t.Fatalf("direct offer: %v", err)
	}
	e.BeginTurn(group, "turn-1")
	out, err := e.Offer(context.Background(), group, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("group offer: %v", err)
	}
	if out.Kind != OutcomeSaved {
		t.Fatalf("outcome = %v, want saved", out.Kind)
	}
	if len(tr.asks) != 2 {
		t.Fatalf("asks = %d, want 2", len(tr.asks))
	}
}

// TestPerTurnLimit: the budget is spent by asking, refreshed by the next turn, and
// configurable upwards.
func TestPerTurnLimit(t *testing.T) {
	e, mem, tr := newEngine(t, accept(ChoiceShared), accept(ChoiceShared))
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	if out, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID); err != nil || out.Kind != OutcomeSaved {
		t.Fatalf("first offer: %v %v", out.Kind, err)
	}
	second := proposal(TargetShared)
	second.Draft.Title = "Recycling collection moved"
	out, err := e.Offer(context.Background(), sc, second, davidID)
	if err != nil {
		t.Fatalf("second offer: %v", err)
	}
	if out.Kind != OutcomeLimited {
		t.Fatalf("outcome = %v, want limited", out.Kind)
	}
	if len(tr.asks) != 1 {
		t.Fatalf("asks = %d, want 1", len(tr.asks))
	}

	// The next turn restores the budget.
	e.BeginTurn(sc, "turn-2")
	if out, err := e.Offer(context.Background(), sc, second, davidID); err != nil || out.Kind != OutcomeSaved {
		t.Fatalf("next turn: %v %v", out.Kind, err)
	}
	if len(mem.puts) != 2 {
		t.Fatalf("puts = %+v, want two", mem.puts)
	}
}

func TestPerTurnLimitConfigurable(t *testing.T) {
	mem := &stubMemory{}
	tr := &stubTransport{answers: []transport.Answer{accept(ChoiceShared), accept(ChoiceShared)}}
	e := New(mem, tr, Options{MaxProposalsPerTurn: 2})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	for i, title := range []string{"First thing", "Second thing"} {
		p := proposal(TargetShared)
		p.Draft.Title = title
		out, err := e.Offer(context.Background(), sc, p, davidID)
		if err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
		if out.Kind != OutcomeSaved {
			t.Fatalf("offer %d: outcome = %v, want saved", i, out.Kind)
		}
	}
	if len(mem.puts) != 2 {
		t.Fatalf("puts = %+v, want two", mem.puts)
	}
}

// TestRepeatedBeginTurnIsIdempotent: beginning the same turn twice must not refresh the
// budget or age the decline window.
func TestRepeatedBeginTurnIsIdempotent(t *testing.T) {
	e, _, tr := newEngine(t, accept(ChoiceShared))
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	if _, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	e.BeginTurn(sc, "turn-1")
	second := proposal(TargetShared)
	second.Draft.Title = "Something else"
	out, err := e.Offer(context.Background(), sc, second, davidID)
	if err != nil {
		t.Fatalf("second offer: %v", err)
	}
	if out.Kind != OutcomeLimited {
		t.Fatalf("outcome = %v, want limited", out.Kind)
	}
	if len(tr.asks) != 1 {
		t.Fatalf("asks = %d, want 1", len(tr.asks))
	}
}

func TestOfferRejectsBadInput(t *testing.T) {
	e, mem, tr := newEngine(t)

	if _, err := e.Offer(context.Background(), domain.Scope{}, proposal(TargetShared), davidID); !errors.Is(err, ErrUnresolvedScope) {
		t.Errorf("unresolved scope: err = %v, want ErrUnresolvedScope", err)
	}
	empty := proposal(TargetShared)
	empty.Draft.Title = "   "
	if _, err := e.Offer(context.Background(), directScope(), empty, davidID); !errors.Is(err, ErrEmptyDraft) {
		t.Errorf("empty title: err = %v, want ErrEmptyDraft", err)
	}
	if len(tr.asks) != 0 || len(mem.puts) != 0 {
		t.Errorf("bad input reached the member or the store")
	}
}

// TestNoSharedSpace: a direct scope that cannot name the household space must fail
// loudly rather than guess a destination.
func TestNoSharedSpace(t *testing.T) {
	e, mem, _ := newEngine(t, accept(ChoiceShared))
	sc := directScope()
	sc.Read = []domain.SpaceID{personal}

	if _, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID); !errors.Is(err, ErrNoSharedSpace) {
		t.Fatalf("err = %v, want ErrNoSharedSpace", err)
	}
	if len(mem.puts) != 0 {
		t.Fatalf("puts = %+v, want none", mem.puts)
	}
}

func TestSharedSpaceFromOptions(t *testing.T) {
	mem := &stubMemory{}
	tr := &stubTransport{answers: []transport.Answer{accept(ChoiceShared)}}
	e := New(mem, tr, Options{Shared: shared})
	sc := directScope()
	sc.Read = []domain.SpaceID{personal}
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Space != shared {
		t.Fatalf("saved to %q, want %q", out.Space, shared)
	}
}

func TestPromotionHappyPath(t *testing.T) {
	mem := &stubMemory{entry: memory.Entry{
		Title: "Where the spare key lives",
		Body:  "Under the third pot on the left, not the second.",
	}}
	tr := &stubTransport{answers: []transport.Answer{accept(ChoicePublish)}}
	e := New(mem, tr, Options{})
	sc := directScope()

	out, err := e.OfferPromotion(context.Background(), sc, "entry-7", davidID)
	if err != nil {
		t.Fatalf("OfferPromotion: %v", err)
	}
	if out.Kind != OutcomeSaved || out.Space != shared || out.EntryID != "entry-7" {
		t.Fatalf("outcome = %+v, want saved to %q", out, shared)
	}
	if len(mem.shares) != 1 {
		t.Fatalf("shares = %+v, want one", mem.shares)
	}
	if s := mem.shares[0]; s.from != personal || s.to != shared || s.id != "entry-7" {
		t.Errorf("share = %+v, want %s -> %s of entry-7", s, personal, shared)
	}
	// Provenance: Share, never a Get followed by a Put.
	if len(mem.puts) != 0 {
		t.Fatalf("puts = %+v: promotion must not re-author the entry", mem.puts)
	}
	if len(mem.gets) != 1 {
		t.Fatalf("gets = %v, want one: the member must be shown the entry", mem.gets)
	}

	q := tr.asks[0].q
	if !strings.Contains(q.Text, "Under the third pot on the left, not the second.") {
		t.Errorf("question %q does not show the full body", q.Text)
	}
	if !strings.Contains(q.Text, "Where the spare key lives") {
		t.Errorf("question %q does not show the title", q.Text)
	}
	if got := choiceIDs(q); !equal(got, []string{ChoicePublish, ChoiceCancel}) {
		t.Errorf("choices = %v, want publish and cancel", got)
	}
	if q.AllowedUserID != davidID {
		t.Errorf("AllowedUserID = %d, want %d", q.AllowedUserID, davidID)
	}
	if len(tr.sends) != 1 || !strings.Contains(tr.sends[0].Text, "Where the spare key lives") {
		t.Errorf("sends = %v, want a confirmation naming the entry", tr.sends)
	}
}

// TestPromotionRefusedFromGroup: publishing someone's private entry is not something the
// household chat can start, and it must not even read the entry.
func TestPromotionRefusedFromGroup(t *testing.T) {
	mem := &stubMemory{entry: memory.Entry{Title: "Private thing"}}
	tr := &stubTransport{}
	e := New(mem, tr, Options{})

	out, err := e.OfferPromotion(context.Background(), groupScope(), "entry-7", davidID)
	if err != nil {
		t.Fatalf("OfferPromotion: %v", err)
	}
	if out.Kind != OutcomeNotApplicable {
		t.Fatalf("outcome = %v, want not applicable", out.Kind)
	}
	if len(mem.gets) != 0 || len(mem.shares) != 0 || len(tr.asks) != 0 {
		t.Fatalf("group promotion touched memory (%v/%v) or the member (%d asks)",
			mem.gets, mem.shares, len(tr.asks))
	}
}

func TestPromotionCancelAndTimeout(t *testing.T) {
	tests := []struct {
		name   string
		answer transport.Answer
		want   OutcomeKind
	}{
		{"cancel", accept(ChoiceCancel), OutcomeDeclined},
		{"timeout", transport.Answer{TimedOut: true}, OutcomeTimedOut},
		{"stray tap", transport.Answer{ChoiceID: ChoicePublish, UserID: otherID}, OutcomeDeclined},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mem := &stubMemory{entry: memory.Entry{Title: "Private thing"}}
			tr := &stubTransport{answers: []transport.Answer{tc.answer}}
			e := New(mem, tr, Options{})

			out, err := e.OfferPromotion(context.Background(), directScope(), "entry-7", davidID)
			if err != nil {
				t.Fatalf("OfferPromotion: %v", err)
			}
			if out.Kind != tc.want {
				t.Fatalf("outcome = %v, want %v", out.Kind, tc.want)
			}
			if len(mem.shares) != 0 || len(mem.puts) != 0 {
				t.Fatalf("nothing was published, yet shares=%+v puts=%+v", mem.shares, mem.puts)
			}
		})
	}
}

// TestPromotionIgnoresTurnBudget: a promotion is something the member asked for, not a
// proposal competing for the turn's single question.
func TestPromotionIgnoresTurnBudget(t *testing.T) {
	mem := &stubMemory{entry: memory.Entry{Title: "Private thing"}}
	tr := &stubTransport{answers: []transport.Answer{accept(ChoiceShared), accept(ChoicePublish)}}
	e := New(mem, tr, Options{})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	if _, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	out, err := e.OfferPromotion(context.Background(), sc, "entry-7", davidID)
	if err != nil {
		t.Fatalf("OfferPromotion: %v", err)
	}
	if out.Kind != OutcomeSaved {
		t.Fatalf("outcome = %v, want saved", out.Kind)
	}
}

func TestPromotionGetFailure(t *testing.T) {
	mem := &stubMemory{getErr: memory.ErrNotFound}
	tr := &stubTransport{}
	e := New(mem, tr, Options{})

	_, err := e.OfferPromotion(context.Background(), directScope(), "missing", davidID)
	if !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(tr.asks) != 0 {
		t.Fatalf("asked about an entry that could not be read")
	}
}

// TestPromotionFailuresAlwaysSpeak: every way OfferPromotion can fail leaves the
// member with a message and marks the error as spoken for. A publish turn is
// routinely a bare tool call with no prose of its own, so an error path that returns
// quietly here is a member who asked for something and got nothing back — the same
// class of defect as the two empty-turn paths in IMPLEMENTATION.md section 10.
func TestPromotionFailuresAlwaysSpeak(t *testing.T) {
	stored := memory.Entry{Title: "Where the spare key lives", Body: "Under the third pot."}
	boom := errors.New("lore is unreachable")

	tests := []struct {
		name    string
		scope   func(domain.Scope) domain.Scope
		setup   func(*stubMemory, *stubTransport)
		answers []transport.Answer
		want    string
	}{
		{
			name:  "no shared space",
			scope: func(sc domain.Scope) domain.Scope { sc.Read = []domain.SpaceID{personal}; return sc },
			setup: func(m *stubMemory, _ *stubTransport) { m.entry = stored },
			want:  "nothing was published",
		},
		{
			name:  "read fails",
			setup: func(m *stubMemory, _ *stubTransport) { m.entry, m.getErr = stored, boom },
			want:  "nothing was published",
		},
		{
			name:  "question fails",
			setup: func(m *stubMemory, tr *stubTransport) { m.entry, tr.askErr = stored, boom },
			want:  "Nothing was published",
		},
		{
			// The member has already tapped Publish, so what they are told about
			// an irreversible act has to be exact. A failed copy published
			// nothing, and it says so.
			name:    "publish fails after the member confirmed",
			setup:   func(m *stubMemory, _ *stubTransport) { m.entry, m.shareErr = stored, boom },
			answers: []transport.Answer{accept(ChoicePublish)},
			want:    "nothing reached the household memory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, mem, tr := newEngine(t, tc.answers...)
			tc.setup(mem, tr)
			sc := directScope()
			if tc.scope != nil {
				sc = tc.scope(sc)
			}

			_, err := e.OfferPromotion(context.Background(), sc, "entry-7", davidID)
			if err == nil {
				t.Fatal("err = nil, want the failure")
			}
			if !errors.Is(err, ErrMemberNotified) {
				t.Errorf("err = %v, not marked as spoken for; the caller will speak over it", err)
			}
			if len(tr.sends) != 1 {
				t.Fatalf("sends = %v, want exactly one notice", tr.sends)
			}
			if got := tr.sends[0].Text; !strings.Contains(got, tc.want) {
				t.Errorf("notice %q does not contain %q", got, tc.want)
			}
			if strings.Contains(tr.sends[0].Text, "Everyone can see it now") {
				t.Errorf("notice %q reads as a confirmation", tr.sends[0].Text)
			}
		})
	}
}

func TestAskFailurePropagates(t *testing.T) {
	mem := &stubMemory{}
	boom := errors.New("boom")
	tr := &stubTransport{askErr: boom}
	e := New(mem, tr, Options{})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	if _, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(mem.puts) != 0 {
		t.Fatalf("puts = %+v, want none", mem.puts)
	}
	// The member is told nothing was written, best effort.
	if len(tr.sends) != 1 || !strings.Contains(tr.sends[0].Text, "Nothing was written") {
		t.Errorf("sends = %v, want the nothing-was-written notice", tr.sends)
	}

	// No question reached the member, so the turn's budget was not spent: the same
	// turn may still ask once the transport recovers.
	tr.askErr = nil
	tr.answers = []transport.Answer{accept(ChoiceShared)}
	out, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID)
	if err != nil {
		t.Fatalf("retry Offer: %v", err)
	}
	if out.Kind != OutcomeSaved {
		t.Fatalf("outcome = %v, want saved; a failed ask spent the turn's budget", out.Kind)
	}
}

// TestPutFailureIsReported: a write that failed after the member said yes is
// reported rather than retried or passed over in silence (IMPLEMENTATION.md
// section 12). It says nothing was stored, which it can now: while lore was a
// subprocess a lost reply could leave an entry kenward could not name and
// therefore could not delete, and this notice had to hedge about a duplicate.
func TestPutFailureIsReported(t *testing.T) {
	mem := &stubMemory{putErr: errors.New("lore is down")}
	tr := &stubTransport{answers: []transport.Answer{accept(ChoiceShared)}}
	e := New(mem, tr, Options{})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID)
	if err == nil {
		t.Fatal("err = nil, want the store failure")
	}
	if out.Stored() {
		t.Error("outcome claims a write that failed")
	}
	if len(tr.sends) != 1 {
		t.Fatalf("sends = %v, want exactly the failure notice", tr.sends)
	}
	got := tr.sends[0].Text
	if !strings.Contains(got, "couldn't save") || !strings.Contains(got, "nothing was stored") {
		t.Errorf("notice %q does not tell the member the write failed", got)
	}
	if strings.Contains(got, "Saved") {
		t.Errorf("notice %q reads as a confirmation", got)
	}

	// The title is still suppressed: a write that just failed will fail again
	// next turn, and re-proposing it is noise rather than a second chance.
	e.BeginTurn(sc, "turn-2")
	out, err = e.Offer(context.Background(), sc, proposal(TargetShared), davidID)
	if err != nil {
		t.Fatalf("second Offer: %v", err)
	}
	if out.Kind != OutcomeDuplicate {
		t.Fatalf("outcome = %v, want duplicate suppression after an uncertain write", out.Kind)
	}
}

// TestOfferQuestionFailureSpeaks: the model named the household, the scope has no
// household space to name, and so no question can be built at all. The member never
// saw a button — and a remember turn is routinely a bare tool call with no prose of
// its own, so a quiet return here is a turn that answers nothing (IMPLEMENTATION.md
// section 10). Same reasoning as every failure in OfferPromotion.
func TestOfferQuestionFailureSpeaks(t *testing.T) {
	e, mem, tr := newEngine(t)
	sc := directScope()
	sc.Read = []domain.SpaceID{personal}
	e.BeginTurn(sc, "turn-1")

	_, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID)
	if !errors.Is(err, ErrNoSharedSpace) {
		t.Fatalf("err = %v, want ErrNoSharedSpace", err)
	}
	if !errors.Is(err, ErrMemberNotified) {
		t.Errorf("err = %v, not marked as spoken for; the caller will speak over it", err)
	}
	if len(tr.asks) != 0 || len(mem.puts) != 0 {
		t.Fatalf("asks = %v, puts = %+v, want neither for a question that was never built", tr.asks, mem.puts)
	}
	if len(tr.sends) != 1 {
		t.Fatalf("sends = %v, want exactly one notice", tr.sends)
	}
	if got := tr.sends[0].Text; !strings.Contains(got, "nothing was written") {
		t.Errorf("notice %q does not say nothing was written", got)
	}
}

// TestConfirmationFailureDoesNotInviteARetry: the entry landed and only the
// confirmation failed to send. An unmarked error there hands the caller its generic
// "try asking again", which invites the member to repeat the one act this flow states
// cannot be taken back — the same shape as the bug that made the marking mechanical
// in the first place. So the error is marked even though what failed to reach the
// member was the confirmation rather than a notice about a failure.
func TestConfirmationFailureDoesNotInviteARetry(t *testing.T) {
	boom := errors.New("telegram is unreachable")

	// assert is shared by both flows: the outcome still says stored, the error is
	// marked, and the engine did not follow a failed confirmation with a second
	// message — two "Saved …" lines for one write read as two writes.
	assert := func(t *testing.T, out Outcome, err error, tr *stubTransport, want string) {
		t.Helper()
		if !out.Stored() {
			t.Fatal("the write happened; the outcome must still say so")
		}
		if !errors.Is(err, ErrMemberNotified) {
			t.Errorf("err = %v, not marked; the caller answers a stored entry with a retry", err)
		}
		if len(tr.sends) != 1 {
			t.Fatalf("sends = %v, want the one confirmation attempt and nothing after it", tr.sends)
		}
		if got := tr.sends[0].Text; !strings.Contains(got, want) {
			t.Errorf("the attempted notice %q is not the confirmation", got)
		}
	}

	t.Run("offer", func(t *testing.T) {
		// Under ask, because this is about the confirmation that follows a tap.
		// The write-first path has no such message — its announcement is the Ask
		// itself — and its own failure is TestAnnouncementFailureStillSaysItWasWritten.
		e, _, tr := newEngineWith(t, Options{PrivateWrites: PrivateWriteAsk}, accept(ChoicePersonal))
		tr.sendErr = boom
		sc := directScope()
		e.BeginTurn(sc, "turn-1")

		out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
		assert(t, out, err, tr, "I've saved")
	})

	t.Run("promotion", func(t *testing.T) {
		e, m, tr := newEngine(t, accept(ChoicePublish))
		m.entry, tr.sendErr = memory.Entry{Title: "Where the spare key lives"}, boom

		out, err := e.OfferPromotion(context.Background(), directScope(), "entry-7", davidID)
		assert(t, out, err, tr, "I've published")
	})
}

// TestUnsureWithoutSharedSpaceOffersPersonalOnly: destinations are resolved before
// they are offered, so a scope with no shared space never shows a Household button
// whose tap could only fail.
func TestUnsureWithoutSharedSpaceOffersPersonalOnly(t *testing.T) {
	e, mem, tr := newEngine(t, accept(ChoicePersonal))
	sc := directScope()
	sc.Read = []domain.SpaceID{personal}
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	want := []string{ChoicePersonal, ChoiceDecline}
	got := tr.lastChoiceIDs()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("choices = %v, want %v", got, want)
	}
	if out.Kind != OutcomeSaved || out.Space != personal {
		t.Fatalf("outcome = %+v, want saved to the personal space", out)
	}
	if len(mem.puts) != 1 || mem.puts[0].space != personal {
		t.Fatalf("puts = %+v, want one write to %s", mem.puts, personal)
	}
}

// TestWrongSpaceWriteIsNotConfirmed: the confirmation reports the outcome, not the
// intention. A store that lands the write in a different space than the member chose
// must produce a went-wrong notice, never a correct-reading "Saved to your private
// memory" — that message being derivable only from the actual outcome is the
// cross-check this test pins.
func TestWrongSpaceWriteIsNotConfirmed(t *testing.T) {
	mem := &stubMemory{putSpace: shared} // personal was intended; the store misroutes
	tr := &stubTransport{answers: []transport.Answer{accept(ChoicePersonal)}}
	e := New(mem, tr, Options{PrivateWrites: PrivateWriteAsk})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
	if err == nil {
		t.Fatal("err = nil, want the space mismatch reported as a failure")
	}
	if out.Stored() {
		t.Error("outcome claims a save that landed in the wrong space")
	}
	if len(tr.sends) != 1 {
		t.Fatalf("sends = %v, want exactly the went-wrong notice", tr.sends)
	}
	got := tr.sends[0].Text
	if strings.Contains(got, "I've saved") || strings.Contains(got, "your private memory") {
		t.Errorf("member told a misrouted write went where they chose: %q", got)
	}
	if !strings.Contains(got, "didn't store") {
		t.Errorf("notice %q does not say the destination was wrong", got)
	}
}

// TestWrongSpacePublicationIsNotConfirmed is the same cross-check for the promotion
// flow.
func TestWrongSpacePublicationIsNotConfirmed(t *testing.T) {
	mem := &stubMemory{
		entry:      memory.Entry{Title: "Where the spare key lives"},
		shareSpace: personal, // the copy never left the private space
	}
	tr := &stubTransport{answers: []transport.Answer{accept(ChoicePublish)}}
	e := New(mem, tr, Options{})

	out, err := e.OfferPromotion(context.Background(), directScope(), "entry-7", davidID)
	if err == nil {
		t.Fatal("err = nil, want the space mismatch reported as a failure")
	}
	if out.Stored() {
		t.Error("outcome claims a publication that landed in the wrong space")
	}
	for _, s := range tr.sends {
		if strings.Contains(s.Text, "Published") {
			t.Errorf("member told a misrouted copy was published: %q", s.Text)
		}
	}
}

// TestGroupDeclinesArePerMember: in the household group, one member refusing a title
// silences it for them alone. Another member is still asked.
func TestGroupDeclinesArePerMember(t *testing.T) {
	e, mem, tr := newEngine(t,
		transport.Answer{ChoiceID: ChoiceDecline, UserID: davidID},
		transport.Answer{ChoiceID: ChoiceShared, UserID: otherID},
	)
	sc := groupScope()

	e.BeginTurn(sc, "turn-1")
	if out, err := e.Offer(context.Background(), sc, proposal(TargetShared), davidID); err != nil || out.Kind != OutcomeDeclined {
		t.Fatalf("david's offer: %v %v", out.Kind, err)
	}

	// The next turn is another member's message proposing the same title.
	e.BeginTurn(sc, "turn-2")
	out, err := e.Offer(context.Background(), sc, proposal(TargetShared), otherID)
	if err != nil {
		t.Fatalf("other member's offer: %v", err)
	}
	if out.Kind != OutcomeSaved {
		t.Fatalf("outcome = %v, want saved: david's refusal suppressed the title for everyone", out.Kind)
	}
	if len(tr.asks) != 2 {
		t.Fatalf("asks = %d, want 2", len(tr.asks))
	}
	if len(mem.puts) != 1 {
		t.Fatalf("puts = %+v, want the other member's save", mem.puts)
	}

	// And david himself stays suppressed inside the window.
	e.BeginTurn(sc, "turn-3")
	out, err = e.Offer(context.Background(), sc, proposal(TargetShared), davidID)
	if err != nil {
		t.Fatalf("david's second offer: %v", err)
	}
	if out.Kind != OutcomeDuplicate {
		t.Fatalf("outcome = %v, want duplicate for the member who declined", out.Kind)
	}
}

// TestGroupBudgetIsPerMember: the per-turn proposal budget belongs to the speaker.
// One member's pending or spent question must not consume another member's slot,
// even when their turns interleave in the same group scope state.
func TestGroupBudgetIsPerMember(t *testing.T) {
	e, mem, tr := newEngine(t,
		transport.Answer{ChoiceID: ChoiceShared, UserID: davidID},
		transport.Answer{ChoiceID: ChoiceShared, UserID: otherID},
	)
	sc := groupScope()
	e.BeginTurn(sc, "turn-1")

	first := proposal(TargetShared)
	if out, err := e.Offer(context.Background(), sc, first, davidID); err != nil || out.Kind != OutcomeSaved {
		t.Fatalf("david's offer: %v %v", out.Kind, err)
	}

	// The other member's proposal arrives against the same scope state before a
	// new turn begins — the interleaving a released turn slot makes possible.
	second := proposal(TargetShared)
	second.Draft.Title = "Recycling collection moved"
	out, err := e.Offer(context.Background(), sc, second, otherID)
	if err != nil {
		t.Fatalf("other member's offer: %v", err)
	}
	if out.Kind != OutcomeSaved {
		t.Fatalf("outcome = %v, want saved: david's question spent the other member's budget", out.Kind)
	}
	if len(tr.asks) != 2 || len(mem.puts) != 2 {
		t.Fatalf("asks = %d puts = %d, want 2 and 2", len(tr.asks), len(mem.puts))
	}

	// Each member's own budget is still spent.
	third := proposal(TargetShared)
	third.Draft.Title = "A third thing"
	if out, _ := e.Offer(context.Background(), sc, third, davidID); out.Kind != OutcomeLimited {
		t.Fatalf("david's second offer = %v, want limited", out.Kind)
	}
}

func TestOptionsDefaults(t *testing.T) {
	e := New(&stubMemory{}, &stubTransport{}, Options{MaxProposalsPerTurn: -3, DeclineWindow: 0, AskTimeout: -time.Second})
	if e.opts.MaxProposalsPerTurn != DefaultMaxProposalsPerTurn {
		t.Errorf("MaxProposalsPerTurn = %d, want %d", e.opts.MaxProposalsPerTurn, DefaultMaxProposalsPerTurn)
	}
	if e.opts.DeclineWindow != DefaultDeclineWindow {
		t.Errorf("DeclineWindow = %d, want %d", e.opts.DeclineWindow, DefaultDeclineWindow)
	}
	if e.opts.AskTimeout != DefaultAskTimeout {
		t.Errorf("AskTimeout = %v, want %v", e.opts.AskTimeout, DefaultAskTimeout)
	}
}

// TestDraftIsStoredUnchanged: what the member saw is what gets written.
func TestDraftIsStoredUnchanged(t *testing.T) {
	e, mem, _ := newEngine(t, accept(ChoiceShared))
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	p := proposal(TargetShared)
	p.Draft.Markers = []string{"[CONTEXT]"}
	if _, err := e.Offer(context.Background(), sc, p, davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	got := mem.puts[0].draft
	if got.Body != p.Draft.Body || got.Domain != p.Draft.Domain || got.Confidence != p.Draft.Confidence {
		t.Errorf("stored draft = %+v, want %+v", got, p.Draft)
	}
	if len(got.Markers) != 1 || got.Markers[0] != "[CONTEXT]" {
		t.Errorf("markers = %v, want the proposal's", got.Markers)
	}
}

// TestScopeStatesAreBounded: the history is in-memory and must not grow with the number
// of conversations seen.
func TestScopeStatesAreBounded(t *testing.T) {
	e := New(&stubMemory{}, &stubTransport{}, Options{})
	for i := 0; i < maxScopes*3; i++ {
		sc := directScope()
		sc.ChatID = int64(i)
		e.BeginTurn(sc, "turn-1")
	}
	if len(e.states) > maxScopes {
		t.Fatalf("states = %d, want at most %d", len(e.states), maxScopes)
	}
}

func turnToken(i int) string { return "turn-" + strconv.Itoa(i) }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The title and body a member's private conversation produced. Every error this
// package can return is checked against both.
const (
	secretTitle = "Miriam's biopsy is on the 12th"
	secretBody  = "She has not told her sister yet."
)

// secretProposal is a proposal whose title and body are exactly the kind of thing
// the model writes out of a private conversation: a one-line summary of the
// sensitive thing itself.
func secretProposal() Proposal {
	return Proposal{
		Draft: memory.Draft{
			Domain:     "household",
			Title:      secretTitle,
			Body:       secretBody,
			Confidence: "provisional",
		},
		Target: TargetPersonal,
	}
}

// assertNoContent fails when an error's default rendering — the string that
// reaches the operator's log — quotes anything the member said.
func assertNoContent(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, leak := range []string{secretTitle, secretBody, "Miriam", "biopsy", "sister"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error text quotes conversation content %q: %s", leak, err)
		}
	}
}

// TestOfferErrorsCarryNoContent proves that no failure of the capture flow puts
// the proposal's title or body into the error a caller logs. A member discusses
// something private, the model proposes remembering it, the member taps Save, the
// write fails — and in isolated mode that error is written to a log the host
// operator can read. Identifying the capture by outcome, space and entry id is
// enough to act on and is not content.
func TestOfferErrorsCarryNoContent(t *testing.T) {
	sc := directScope()
	boom := errors.New("lore is unreachable")

	t.Run("ask fails", func(t *testing.T) {
		e, _, tr := newEngine(t)
		tr.askErr = boom
		_, err := e.Offer(context.Background(), sc, secretProposal(), davidID)
		assertNoContent(t, err)
	})

	t.Run("write fails", func(t *testing.T) {
		e, m, _ := newEngine(t, accept(ChoicePersonal))
		m.putErr = boom
		_, err := e.Offer(context.Background(), sc, secretProposal(), davidID)
		assertNoContent(t, err)
		// The space is not content and an operator needs it, so it stays.
		if !strings.Contains(err.Error(), string(personal)) {
			t.Errorf("the error should still name the space, got %s", err)
		}
	})

	t.Run("confirmation fails after the write landed", func(t *testing.T) {
		e, _, tr := newEngineWith(t, Options{PrivateWrites: PrivateWriteAsk}, accept(ChoicePersonal))
		tr.sendErr = boom
		out, err := e.Offer(context.Background(), sc, secretProposal(), davidID)
		assertNoContent(t, err)
		if !out.Stored() {
			t.Fatal("the write happened; the outcome must still say so")
		}
		// An id identifies the entry that could not be confirmed without saying
		// anything about what is in it.
		if !strings.Contains(err.Error(), out.EntryID) {
			t.Errorf("the error should name the entry id %q, got %s", out.EntryID, err)
		}
	})

	// The write-first path's two new errors. Both are reported after an entry
	// carrying the member's secret is already in the store, which is precisely when
	// an operator most wants a log line and least may have one with the words in it.
	t.Run("announcement fails after the write landed", func(t *testing.T) {
		e, _, tr := newEngine(t)
		tr.askErr = boom
		out, err := e.Offer(context.Background(), sc, secretProposal(), davidID)
		assertNoContent(t, err)
		if !out.Stored() {
			t.Fatal("the write happened; the outcome must still say so")
		}
		if !strings.Contains(err.Error(), out.EntryID) {
			t.Errorf("the error should name the entry id %q, got %s", out.EntryID, err)
		}
	})

	t.Run("undo fails", func(t *testing.T) {
		e, m, _ := newEngine(t, accept(ChoiceUndo))
		m.deleteErr = boom
		out, err := e.Offer(context.Background(), sc, secretProposal(), davidID)
		assertNoContent(t, err)
		if !out.Stored() {
			t.Fatal("the delete failed, so the entry is still there and the outcome must say so")
		}
		if !strings.Contains(err.Error(), out.EntryID) {
			t.Errorf("the error should name the entry id %q, got %s", out.EntryID, err)
		}
	})
}

// TestPromotionErrorsCarryNoContent is the same guarantee for the publish flow,
// where the title comes off a stored entry rather than a fresh proposal.
func TestPromotionErrorsCarryNoContent(t *testing.T) {
	sc := directScope()
	boom := errors.New("lore is unreachable")
	stored := memory.Entry{Title: secretTitle, Body: secretBody}

	t.Run("read fails", func(t *testing.T) {
		e, m, _ := newEngine(t)
		m.entry, m.getErr = stored, boom
		_, err := e.OfferPromotion(context.Background(), sc, "entry-7", davidID)
		assertNoContent(t, err)
		if !strings.Contains(err.Error(), "entry-7") {
			t.Errorf("the error should name the entry id, got %s", err)
		}
	})

	t.Run("ask fails", func(t *testing.T) {
		e, m, tr := newEngine(t)
		m.entry, tr.askErr = stored, boom
		_, err := e.OfferPromotion(context.Background(), sc, "entry-7", davidID)
		assertNoContent(t, err)
	})

	t.Run("share fails", func(t *testing.T) {
		e, m, _ := newEngine(t, transport.Answer{ChoiceID: ChoicePublish, UserID: davidID})
		m.entry, m.shareErr = stored, boom
		_, err := e.OfferPromotion(context.Background(), sc, "entry-7", davidID)
		assertNoContent(t, err)
	})

	t.Run("confirmation fails after the copy landed", func(t *testing.T) {
		e, m, tr := newEngine(t, transport.Answer{ChoiceID: ChoicePublish, UserID: davidID})
		m.entry, tr.sendErr = stored, boom
		out, err := e.OfferPromotion(context.Background(), sc, "entry-7", davidID)
		assertNoContent(t, err)
		if !out.Stored() {
			t.Fatal("the copy happened; the outcome must still say so")
		}
	})
}

// TestOutcomeCarriesTheTitleForTheMember: removing the title from errors must not
// remove it from the outcome. The member is who it is for, and a caller rendering
// a confirmation back into the chat still needs it — the point is that disclosing
// it is a decision the caller takes, not something an error does behind its back.
func TestOutcomeCarriesTheTitleForTheMember(t *testing.T) {
	e, _, _ := newEngine(t, transport.Answer{ChoiceID: ChoiceDecline, UserID: davidID})
	out, err := e.Offer(context.Background(), directScope(), secretProposal(), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Title != secretTitle {
		t.Errorf("Outcome.Title = %q, want the proposal's title", out.Title)
	}
}

// TestMemberMarkupNeverBecomesMarkup: a member is entitled to title a note with
// the parse mode's own metacharacters, and to read it back as the characters they
// typed.
//
// This is the security-relevant half of setting a parse mode. Every value in
// these messages that kenward did not write itself — the title, the body — comes
// from a member or from a model with a member's words in its context. If any of
// it reached Telegram as markup, an entry body could decide how the message
// announcing it renders, and the announcement is the one thing standing behind
// the promise that you always see what was written. It is the same rule
// prompt.go's oneLine keeps one layer up.
func TestMemberMarkupNeverBecomesMarkup(t *testing.T) {
	const title = `<b>Bins</b> & "stuff"`
	const body = `</blockquote><i>escaped?</i> 5 > 3 & 2 < 4`

	e, _, tr := newEngine(t, transport.Answer{TimedOut: true})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	p := proposal(TargetPersonal)
	p.Draft.Title, p.Draft.Body = title, body
	if _, err := e.Offer(context.Background(), sc, p, davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if len(tr.asks) != 1 {
		t.Fatalf("asks = %d, want the one announcement", len(tr.asks))
	}
	got := tr.asks[0].q.Text

	// Exactly six tags: the bold around the title, the blockquote around the
	// body, and the italics on the undo hint. Anything the member wrote that
	// survived as a tag would push this count up.
	if n := strings.Count(got, "<"); n != 6 {
		t.Errorf("announcement has %d tags, want the 6 kenward wrote: %q", n, got)
	}
	// And what renders is what the member typed, character for character.
	rendered := transport.PlainText(got)
	for _, want := range []string{title, body} {
		if !strings.Contains(rendered, want) {
			t.Errorf("announcement renders as %q, which does not contain %q", rendered, want)
		}
	}
}

// --- the write-first path ----------------------------------------------------

// TestPrivateTargetIsWrittenThenAnnounced is the decided behaviour in one test: a
// proposal for the member's own space reaches the store before it reaches the member,
// and the message they get says what was written, where, and offers to take it back.
//
// It asserts the body is in the announcement as well as the title. That is not
// cosmetic. Under the old flow the member read the exact words before the write and
// could refuse them; the announcement is the only place those words are now shown at
// all, so an announcement carrying a title alone would quietly turn "kenward tells you
// what it wrote" into "kenward tells you that it wrote".
func TestPrivateTargetIsWrittenThenAnnounced(t *testing.T) {
	e, m, tr := newEngine(t, transport.Answer{TimedOut: true})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeSaved || out.Space != personal {
		t.Fatalf("outcome = %v in %q, want saved in %q", out.Kind, out.Space, personal)
	}
	if len(m.puts) != 1 || m.puts[0].space != personal {
		t.Fatalf("puts = %+v, want exactly one to %q", m.puts, personal)
	}
	if len(tr.asks) != 1 {
		t.Fatalf("asks = %d, want the one announcement", len(tr.asks))
	}
	q := tr.asks[0].q
	if got := choiceIDs(q); !equal(got, []string{ChoiceUndo}) {
		t.Errorf("choices = %v, want just [%s]", got, ChoiceUndo)
	}
	for _, want := range []string{"Bins go out Tuesday", "Green bin on alternate weeks.", "your private memory"} {
		if !strings.Contains(q.Text, want) {
			t.Errorf("announcement %q does not carry %q", q.Text, want)
		}
	}
	if strings.Contains(q.Text, "("+string(personal)+")") {
		t.Errorf("announcement %q shows the raw space id", q.Text)
	}
	// The default retirement line says the question was declined. On a message
	// reporting a write that already happened, that reads as the write having been
	// called off, which is the one thing it must never say.
	if q.RetiredNote == "" {
		t.Fatal("the announcement must carry its own retirement note; the default says the question was declined")
	}
	if strings.Contains(q.RetiredNote, "declined") {
		t.Errorf("retirement note %q says declined about a write that stands", q.RetiredNote)
	}
	if !strings.Contains(q.RetiredNote, "still in your private memory") {
		t.Errorf("retirement note %q does not say the entry is still there", q.RetiredNote)
	}
	// Nothing else is sent: the announcement is the whole of the member's traffic
	// for a write, where the old flow cost a question and a confirmation.
	if len(tr.sends) != 0 {
		t.Errorf("sends = %v, want none — the announcement is the message", tr.sends)
	}
}

// TestUndoDeletesTheEntryAndSaysSo: the tap, the delete, and the sentence that is only
// true because the delete succeeded.
func TestUndoDeletesTheEntryAndSaysSo(t *testing.T) {
	e, m, tr := newEngine(t, accept(ChoiceUndo))
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeUndone {
		t.Errorf("outcome = %v, want %v", out.Kind, OutcomeUndone)
	}
	if out.Stored() {
		t.Error("Stored() is true after a confirmed undo; nothing is in memory")
	}
	if want := []string{string(personal) + "/entry-1"}; !equal(m.deletes, want) {
		t.Fatalf("deletes = %v, want %v — the undo must be space-scoped and name the entry it wrote", m.deletes, want)
	}
	if len(tr.sends) != 1 {
		t.Fatalf("sends = %v, want the one removal notice", tr.sends)
	}
	got := tr.sends[0].Text
	if !strings.Contains(got, "I've removed") || !strings.Contains(got, "Bins go out Tuesday") {
		t.Errorf("removal notice %q does not say what was removed", got)
	}
	// lore deletes by tombstone, not by shredding, and the message may promise only
	// what that delivers. ARCHITECTURE.md required the announcement to say which.
	if !strings.Contains(got, "won't come back in an answer") {
		t.Errorf("removal notice %q does not bound the promise to what a tombstone does", got)
	}
	for _, never := range []string{"erased", "destroyed", "wiped"} {
		if strings.Contains(got, never) {
			t.Errorf("removal notice %q promises %q; a tombstone is not a shred", got, never)
		}
	}
}

// TestUndoIsADeclineSoTheModelDoesNotWriteItBackNextTurn.
//
// Without this the undo achieves nothing that lasts: the same conversation produces
// the same proposal on the next turn, and the default policy writes it straight back
// without asking. The old flow got this for free — a decline was a tap on "Don't
// save" — and the new one has to record it deliberately.
func TestUndoIsADeclineSoTheModelDoesNotWriteItBackNextTurn(t *testing.T) {
	e, m, _ := newEngine(t, accept(ChoiceUndo), accept(ChoiceUndo))
	sc := directScope()

	e.BeginTurn(sc, "turn-1")
	if _, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID); err != nil {
		t.Fatalf("first Offer: %v", err)
	}

	e.BeginTurn(sc, "turn-2")
	out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
	if err != nil {
		t.Fatalf("second Offer: %v", err)
	}
	if out.Kind != OutcomeDuplicate {
		t.Errorf("outcome = %v, want %v — an undone title must be suppressed", out.Kind, OutcomeDuplicate)
	}
	if len(m.puts) != 1 {
		t.Errorf("puts = %d, want 1; the second turn rewrote what the member had just taken back", len(m.puts))
	}
}

// TestUndoFailureNeverSaysItWasUndone covers the two endings that are not "gone".
//
// A delete lore refused means the entry is still there and the member must be told
// so. A delete lore never answered means nobody knows, and both "removed" and "still
// there" would be guesses about the member's own memory. The outcome stays saved in
// each case, because in each case something may well be stored.
func TestUndoFailureNeverSaysItWasUndone(t *testing.T) {
	cases := []struct {
		name      string
		deleteErr error
		wantIn    []string
		wantNotIn []string
	}{
		{
			name:      "lore refused, so it is still there",
			deleteErr: errors.New("lore said no"),
			wantIn:    []string{"couldn't take that back", "still in", "Bins go out Tuesday"},
			wantNotIn: []string{"Removed", "can't tell"},
		},
		{
			// There is no "nobody knows" case left. A store that is busy, closed
			// or unreachable failed the delete outright, so the entry is still
			// there and the member is told so — the hedged third message this
			// replaces existed only because a lost MCP response could leave a
			// tombstone that may or may not have landed.
			name:      "the store was busy, so it is still there",
			deleteErr: fmt.Errorf("capture: undoing: %w", memory.ErrBusy),
			wantIn:    []string{"couldn't take that back", "still in", "Bins go out Tuesday"},
			wantNotIn: []string{"Removed", "can't tell"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, m, tr := newEngine(t, accept(ChoiceUndo))
			m.deleteErr = tc.deleteErr
			sc := directScope()
			e.BeginTurn(sc, "turn-1")

			out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
			if err == nil {
				t.Fatal("err = nil, want the failed undo reported")
			}
			if !errors.Is(err, ErrMemberNotified) {
				t.Errorf("err = %v, not marked; the caller will speak over a notice the engine already sent", err)
			}
			// Saved, not undone: something may well still be in the store, and an
			// outcome saying otherwise would be the same lie as the message.
			if out.Kind != OutcomeSaved {
				t.Errorf("outcome = %v, want %v", out.Kind, OutcomeSaved)
			}
			if len(tr.sends) != 1 {
				t.Fatalf("sends = %v, want exactly one notice", tr.sends)
			}
			got := tr.sends[0].Text
			for _, want := range tc.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("notice %q is missing %q", got, want)
				}
			}
			for _, never := range tc.wantNotIn {
				if strings.Contains(got, never) {
					t.Errorf("notice %q claims %q about an undo that did not complete", got, never)
				}
			}
		})
	}
}

// TestAnnouncementFailureStillSaysItWasWritten: the entry is in the store and the
// message carrying that news did not go out. Silence here would be a silent write,
// which is the one outcome this design forbids outright — so the engine falls back to
// a plain confirmation and says the undo button is missing rather than implying one
// exists.
func TestAnnouncementFailureStillSaysItWasWritten(t *testing.T) {
	e, m, tr := newEngine(t)
	tr.askErr = errors.New("telegram is unreachable")
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
	if err == nil {
		t.Fatal("err = nil, want the failed announcement reported to the caller")
	}
	if !errors.Is(err, ErrMemberNotified) {
		t.Errorf("err = %v, not marked", err)
	}
	if !out.Stored() {
		t.Error("the write landed; the outcome must say so")
	}
	if len(m.puts) != 1 {
		t.Fatalf("puts = %+v, want the one write", m.puts)
	}
	if len(tr.sends) != 1 {
		t.Fatalf("sends = %v, want the fallback confirmation", tr.sends)
	}
	got := tr.sends[0].Text
	for _, want := range []string{"I've saved", "Bins go out Tuesday", "your private memory", "undo button didn't go through"} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "("+string(personal)+")") {
		t.Errorf("fallback %q shows the raw space id", got)
	}
}

// TestWriteFirstPathIsPrivateOnly: the group has no private destination and the shared
// space is never written on the assistant's own initiative. Both halves are checked
// here rather than inferred from writesPrivateDirectly, because this is the invariant
// the whole change had to preserve.
func TestWriteFirstPathIsPrivateOnly(t *testing.T) {
	cases := []struct {
		name   string
		scope  domain.Scope
		target Target
	}{
		{"a shared target in a direct chat is still asked", directScope(), TargetShared},
		{"an unsure target is still asked", directScope(), TargetUnsure},
		{"the group asks whatever the model proposed", groupScope(), TargetPersonal},
		{"the group asks for a shared target too", groupScope(), TargetShared},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Timed out: a question nobody answers writes nothing, which is what
			// makes "was it asked or was it written" observable.
			e, m, tr := newEngine(t, transport.Answer{TimedOut: true})
			e.BeginTurn(tc.scope, "turn-1")

			out, err := e.Offer(context.Background(), tc.scope, proposal(tc.target), davidID)
			if err != nil {
				t.Fatalf("Offer: %v", err)
			}
			if out.Kind != OutcomeTimedOut {
				t.Errorf("outcome = %v, want %v", out.Kind, OutcomeTimedOut)
			}
			if len(m.puts) != 0 {
				t.Errorf("puts = %+v, want none: this proposal must be asked about, not written", m.puts)
			}
			if len(tr.asks) != 1 {
				t.Fatalf("asks = %d, want one question", len(tr.asks))
			}
			for _, id := range choiceIDs(tr.asks[0].q) {
				if id == ChoiceUndo {
					t.Errorf("choices include %s; this is a question, not an undo announcement", ChoiceUndo)
				}
			}
		})
	}
}

// TestPrivateWriteAskRestoresTheQuestionEntirely is the promise made to a household
// that turns the policy back: not an approximation of the old flow, the old flow. One
// question, no write until the tap, and no undo button anywhere.
func TestPrivateWriteAskRestoresTheQuestionEntirely(t *testing.T) {
	e, m, tr := newEngineWith(t, Options{PrivateWrites: PrivateWriteAsk}, transport.Answer{TimedOut: true})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeTimedOut {
		t.Errorf("outcome = %v, want %v: an unanswered question writes nothing", out.Kind, OutcomeTimedOut)
	}
	if len(m.puts) != 0 {
		t.Errorf("puts = %+v, want none under the ask policy", m.puts)
	}
	if got := tr.lastChoiceIDs(); !equal(got, []string{ChoicePersonal, ChoiceDecline}) {
		t.Errorf("choices = %v, want the old two-button question", got)
	}
	if tr.asks[0].q.RetiredNote != "" {
		t.Errorf("RetiredNote = %q, want empty: a question really is declined when it expires", tr.asks[0].q.RetiredNote)
	}
}

// TestUndoWindowIsTheAskTimeout: the button is live for exactly as long as a capture
// question waits, and a household that shortens one shortens the other. There is no
// second knob, deliberately — two timeouts for two flavours of the same wait is a
// setting nobody can hold in their head.
func TestUndoWindowIsTheAskTimeout(t *testing.T) {
	m, tr := &stubMemory{}, &stubTransport{answers: []transport.Answer{{TimedOut: true}}}
	e := New(m, tr, Options{AskTimeout: 90 * time.Second})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	if _, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if got := tr.asks[0].q.Timeout; got != 90*time.Second {
		t.Errorf("undo window = %v, want the configured ask timeout of 90s", got)
	}
}

// TestUndoTapFromAnotherMemberIsIgnored. The transport filters these already, and this
// is the defence for a transport that does not — the same reasoning as the equivalent
// check on a capture question, and with more at stake: a stranger's tap here deletes
// something out of somebody else's private memory.
func TestUndoTapFromAnotherMemberIsIgnored(t *testing.T) {
	e, m, tr := newEngine(t, transport.Answer{ChoiceID: ChoiceUndo, UserID: otherID})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeSaved {
		t.Errorf("outcome = %v, want the write to stand", out.Kind)
	}
	if len(m.deletes) != 0 {
		t.Errorf("deletes = %v, want none: another member tapped Undo on this entry", m.deletes)
	}
	if len(tr.sends) != 0 {
		t.Errorf("sends = %v, want none", tr.sends)
	}
}

// wantDestination is the English phrase a confirmation should name, rebuilt in the
// test rather than read from production. destinationPhrase used to be production
// code and the test called it, which meant the test agreed with whatever the code
// did; the phrase is a product surface and the test should hold its own copy.
func wantDestination(sc domain.Scope, space domain.SpaceID) string {
	if sc.AllowsPrivateCapture() && space == sc.Write {
		return "your private memory"
	}
	return "the household memory"
}

// TestAnnouncementsAndButtonsFollowTheMemberLanguage. Capture is where the destination
// slot lived: nine sentences built by dropping "your private memory" after a
// preposition. German inflects the phrase with the preposition and Dutch changes the
// preposition itself, so the slot is gone and each language writes its own sentence.
func TestAnnouncementsAndButtonsFollowTheMemberLanguage(t *testing.T) {
	de := lang.For("German")
	e, _, tr := newEngineWith(t, Options{Language: "German", PrivateWrites: PrivateWriteAsk},
		accept(ChoicePersonal))
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	if _, err := e.Offer(context.Background(), sc, proposal(TargetPersonal), davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if len(tr.asks) != 1 {
		t.Fatalf("asks = %v, want the one question", tr.asks)
	}
	q := tr.asks[0].q
	if !strings.Contains(q.Text, de.ProposalOpener) {
		t.Errorf("question %q is not in the member's language", q.Text)
	}
	if got := q.Choices[0].Label; got != de.BtnSavePersonal {
		t.Errorf("save button reads %q, want %q", got, de.BtnSavePersonal)
	}
	// The choice ids are stable constants and never translate: they travel through
	// the transport as callback payloads and come back in an Answer.
	if got := q.Choices[0].ID; got != ChoicePersonal {
		t.Errorf("choice id %q was translated; ids are machine-readable", got)
	}
	// The outcome line travels with the question so the transport can size the
	// message against this language rather than English.
	if q.Notes.Declined != de.Declined {
		t.Errorf("question carries the outcome line %q, want %q", q.Notes.Declined, de.Declined)
	}
	if len(tr.sends) != 1 {
		t.Fatalf("sends = %v, want the one confirmation", tr.sends)
	}
	if !strings.Contains(tr.sends[0].Text, de.Saved(true, "Bins go out Tuesday")) {
		t.Errorf("confirmation %q is not the German sentence", tr.sends[0].Text)
	}
	// The dative in deinem privaten Gedächtnis, not the accusative that schreiben
	// governs. A shared noun-phrase slot could not produce both.
	if !strings.Contains(tr.sends[0].Text, "in deinem privaten Gedächtnis gespeichert") {
		t.Errorf("confirmation %q does not inflect the destination for speichern", tr.sends[0].Text)
	}
	if !strings.Contains(de.WrittenOpener(true), "in dein privates Gedächtnis") {
		t.Errorf("the write announcement does not use the accusative schreiben governs: %q", de.WrittenOpener(true))
	}
}

// TestGroupCaptureUsesTheHouseholdLanguage. The group scope has no member to ask, so
// its notices are the household's — which is the same resolution the assistant's
// persona makes, made once in the supervisor so the two cannot drift.
func TestGroupCaptureUsesTheHouseholdLanguage(t *testing.T) {
	fr := lang.For("French")
	e, _, tr := newEngineWith(t, Options{Language: "French"}, accept(ChoiceShared))
	sc := groupScope()
	e.BeginTurn(sc, "turn-1")

	if _, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if len(tr.asks) != 1 {
		t.Fatalf("asks = %v, want the one question", tr.asks)
	}
	if got := tr.asks[0].q.Choices[0].Label; got != fr.BtnHousehold {
		t.Errorf("group button reads %q, want %q", got, fr.BtnHousehold)
	}
	if len(tr.sends) != 1 || !strings.Contains(tr.sends[0].Text, fr.Saved(false, "Bins go out Tuesday")) {
		t.Errorf("group confirmation %v is not the French sentence", tr.sends)
	}
}
