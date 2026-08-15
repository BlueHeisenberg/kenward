package capture

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
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
	asks    []askCall
	sends   []transport.Outbound
}

func (t *stubTransport) Updates(context.Context) (<-chan transport.Inbound, error) {
	return nil, transport.ErrClosed
}

func (t *stubTransport) Send(_ context.Context, o transport.Outbound) error {
	t.sends = append(t.sends, o)
	return nil
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
	entry    memory.Entry
	getErr   error
	putErr   error
	shareErr error
	puts     []putCall
	shares   []shareCall
	gets     []string
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
	return memory.Entry{ID: "entry-1", Space: space, Title: d.Title, Body: d.Body}, nil
}

func (m *stubMemory) Share(_ context.Context, from, to domain.SpaceID, id string) (memory.Entry, error) {
	m.shares = append(m.shares, shareCall{from: from, to: to, id: id})
	if m.shareErr != nil {
		return memory.Entry{}, m.shareErr
	}
	return memory.Entry{ID: id, Space: to, Title: m.entry.Title}, nil
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
	m := &stubMemory{}
	tr := &stubTransport{answers: answers}
	return New(m, tr, Options{}), m, tr
}

func accept(choice string) transport.Answer {
	return transport.Answer{ChoiceID: choice, UserID: davidID}
}

// TestOfferButtons walks every row of the capture table.
func TestOfferButtons(t *testing.T) {
	tests := []struct {
		name    string
		scope   domain.Scope
		target  Target
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
			name:  "direct personal confirms one destination",
			scope: directScope(), target: TargetPersonal,
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
			e, mem, tr := newEngine(t, tc.answer)
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
				!strings.Contains(tr.sends[0].Text, string(tc.wantIn)) {
				t.Errorf("confirmation %q lacks the title or the destination", tr.sends[0].Text)
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
// still may not route someone else's memory.
func TestAnswerFromAnotherUser(t *testing.T) {
	e, mem, _ := newEngine(t, transport.Answer{ChoiceID: ChoiceShared, UserID: otherID})
	sc := directScope()
	e.BeginTurn(sc, "turn-1")

	out, err := e.Offer(context.Background(), sc, proposal(TargetUnsure), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Kind != OutcomeDeclined {
		t.Fatalf("outcome = %v, want declined", out.Kind)
	}
	if len(mem.puts) != 0 {
		t.Fatalf("puts = %+v, want none", mem.puts)
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
}

func TestPutFailurePropagates(t *testing.T) {
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
	if len(tr.sends) != 0 {
		t.Errorf("confirmed a write that failed: %v", tr.sends)
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
