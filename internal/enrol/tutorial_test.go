package enrol

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// memPersonas is a PersonaStore in memory. It records every write, not just the last
// one, because the property most of these tests are about is that an answer is
// committed when it is given rather than when the tutorial finishes.
type memPersonas struct {
	mu      sync.Mutex
	current map[domain.MemberID]Persona
	writes  []Persona
	err     error
}

func newPersonas() *memPersonas {
	return &memPersonas{current: map[domain.MemberID]Persona{}}
}

func (m *memPersonas) SetPersona(ctx context.Context, id domain.MemberID, p Persona) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.current[id] = p
	m.writes = append(m.writes, p)
	return nil
}

func (m *memPersonas) Personas(ctx context.Context) (map[domain.MemberID]Persona, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[domain.MemberID]Persona, len(m.current))
	for k, v := range m.current {
		out[k] = v
	}
	return out, nil
}

func (m *memPersonas) get(id domain.MemberID) Persona {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current[id]
}

func (m *memPersonas) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.writes)
}

// scriptedAsker answers questions from a script and records everything sent.
//
// A script that runs out means the member stopped tapping, which the transport
// reports as a timeout and the tutorial must treat as abandonment. That is the
// default rather than a special case on purpose: the abandonment path is the one
// nobody exercises by hand.
type scriptedAsker struct {
	mu      sync.Mutex
	script  []string // choice ids, in order; "" means let it time out
	asked   []transport.Question
	sent    []transport.Outbound
	sendErr error
	// cancelAt is the 1-based question at which the node goes down: the context is
	// cancelled just before that Ask is served, exactly as a shutdown would.
	cancelAt int
	cancel   func()
}

func (a *scriptedAsker) Send(ctx context.Context, o transport.Outbound) error {
	// A real transport fails a send on a cancelled context, and the restart test
	// depends on it: a node shutting down must not record an explanation as
	// delivered when the bytes never left.
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sendErr != nil {
		return a.sendErr
	}
	a.sent = append(a.sent, o)
	return nil
}

func (a *scriptedAsker) Ask(ctx context.Context, q transport.Question) (transport.Answer, error) {
	if err := ctx.Err(); err != nil {
		return transport.Answer{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancelAt != 0 && len(a.asked) == a.cancelAt-1 {
		a.cancel()
		return transport.Answer{}, context.Canceled
	}
	a.asked = append(a.asked, q)
	if len(a.script) == 0 {
		return transport.Answer{TimedOut: true}, nil
	}
	id := a.script[0]
	a.script = a.script[1:]
	if id == "" {
		return transport.Answer{TimedOut: true}, nil
	}
	return transport.Answer{ChoiceID: id, UserID: q.AllowedUserID}, nil
}

func (a *scriptedAsker) transcript() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	for _, q := range a.asked {
		b.WriteString(q.Text + "\n")
	}
	for _, o := range a.sent {
		b.WriteString(o.Text + "\n")
	}
	return b.String()
}

func (a *scriptedAsker) questions() []transport.Question {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]transport.Question(nil), a.asked...)
}

func (a *scriptedAsker) sentTexts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.sent))
	for _, o := range a.sent {
		out = append(out, o.Text)
	}
	return out
}

var testMember = domain.Member{ID: "david", Name: "David", TelegramID: 42}

// tutorialFor builds a Tutorial with a fast timeout and a member who types the
// given answers.
//
// The channel is unbuffered and fed one message at a time, which is how the
// supervisor feeds it: a typed message is delivered only if the tutorial is waiting
// for one right now, so something a member sends while a button question is up
// cannot be eaten as the answer to the next typed question.
func tutorialFor(t *testing.T, a Asker, ps PersonaStore, typed []string, opts func(*Tutorial)) *Tutorial {
	t.Helper()
	ch := make(chan transport.Inbound)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for _, s := range typed {
			select {
			case ch <- transport.Inbound{ChatID: 500, UserID: 42, Text: s}:
			case <-done:
				return
			}
		}
	}()
	tu := &Tutorial{
		Member:    testMember,
		ChatID:    500,
		Asker:     a,
		Answers:   ch,
		Personas:  ps,
		Household: LangEnglish,
		Timeout:   50 * time.Millisecond,
	}
	if opts != nil {
		opts(tu)
	}
	return tu
}

func mustRun(t *testing.T, tu *Tutorial) {
	t.Helper()
	if err := tu.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestTutorialOrder is the ordering IDENTITY.md settles on: language first, then the
// name, then the register, then the character, and the explanation last.
func TestTutorialOrder(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceToneWarm}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, []string{"Jeeves", "dry, likes cycling"},
		func(tu *Tutorial) { tu.OneEach = true }))

	qs := a.questions()
	if len(qs) != 2 {
		t.Fatalf("asked %d button questions, want 2 (language, register)", len(qs))
	}
	if !strings.Contains(qs[0].Text, "Language") {
		t.Errorf("first question is not the language one: %q", qs[0].Text)
	}
	if !strings.Contains(qs[1].Text, "How I talk") {
		t.Errorf("second button question is not the register one: %q", qs[1].Text)
	}

	sent := a.sentTexts()
	order := []string{"What would you like to call me", "Anything else about how",
		"This chat is private", "The group chat is shared", "What happens when I write something down"}
	at := 0
	for _, want := range order {
		found := -1
		for i := at; i < len(sent); i++ {
			if strings.Contains(sent[i], want) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("%q never arrived, or arrived out of order, in:\n%s", want, strings.Join(sent, "\n---\n"))
		}
		at = found
	}

	got := ps.get("david")
	want := Persona{
		PersonaConfig: config.PersonaConfig{
			Language: english.name, AgentName: "Jeeves", Tone: ToneWarm, Character: "dry, likes cycling",
		},
		ChatID: 500, Explained: true,
	}
	if got != want {
		t.Errorf("persona = %+v, want %+v", got, want)
	}
}

// TestTutorialLanguageIsHonouredForEverythingAfterIt is the whole reason language is
// question one. Asking about tone in English after a member has said they speak
// Spanish is the failure this asserts against.
func TestTutorialLanguageIsHonouredForEverythingAfterIt(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangSpanish, choiceToneFlat}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, []string{skipWord}, nil))

	all := a.transcript()
	for _, want := range []string{
		spanish.registerQ,
		spanish.characterQ,
		// The explanation comes from the catalogue rather than from this package's
		// tutorial table, so this is also the assertion that the language chosen in
		// question one reaches it.
		lang.For(spanish.name).EnrolPrivateBody,
		lang.For(spanish.name).EnrolGroupBody,
		lang.For(spanish.name).EnrolMemoryBodyDefault,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("after choosing Spanish the tutorial did not say %q:\n%s", want, all)
		}
	}
	for _, notWant := range []string{
		english.registerQ,
		lang.For(lang.English).EnrolPrivateBody,
		lang.For(lang.English).EnrolMemoryBodyDefault,
	} {
		if strings.Contains(all, notWant) {
			t.Errorf("after choosing Spanish the tutorial still said the English %q", notWant)
		}
	}
	// The language's own name, not the tag: config.PersonaConfig.Language is free
	// text and reaches the model, and "es" in a system prompt is a tag leaking out
	// of this package. TagFor is what turns it back into an index into the copy.
	if got := ps.get("david").Language; got != spanish.name {
		t.Errorf("language recorded as %q, want %q", got, spanish.name)
	}
	if got := TagFor(ps.get("david").Language); got != LangSpanish {
		t.Errorf("TagFor(%q) = %q, want %q", ps.get("david").Language, got, LangSpanish)
	}
}

// TestTutorialSaysWhenItCannotSpeakYourLanguage is the explicit admission the design
// demands instead of asking a question and ignoring the answer for five messages.
func TestTutorialSaysWhenItCannotSpeakYourLanguage(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangOther, choiceToneFlat}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, []string{"Português", skipWord}, nil))

	all := a.transcript()
	if !strings.Contains(all, "only written in English and Spanish") {
		t.Errorf("the tutorial did not admit it cannot hold this conversation in Português:\n%s", all)
	}
	if !strings.Contains(all, "the rest of them will be in English") {
		t.Errorf("the tutorial did not say which language the questions carry on in:\n%s", all)
	}
	// And then the explanation arrives in Português anyway. The two language lists
	// are different on purpose: the four setup questions are written in two
	// languages and the memory model in ten, and the part the product is obliged to
	// get right is the one with the longer list.
	if !strings.Contains(all, lang.For("Português").EnrolPrivateBody) {
		t.Errorf("the explanation did not arrive in the language the member named:\n%s", all)
	}
	if got := ps.get("david").Language; got != "Português" {
		t.Errorf("the named language was not recorded: %q", got)
	}
	if Spoken("Português") {
		t.Error("Spoken claims a language the package holds no copy for")
	}
}

// TestTutorialAbandonment is a member who stops answering. They must end up with a
// working assistant on defaults, and they must still be told how their memory works.
func TestTutorialAbandonment(t *testing.T) {
	a := &scriptedAsker{} // nothing scripted: every question times out
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, nil, nil))

	if got := len(a.questions()); got != 1 {
		t.Errorf("a timed-out question was followed by %d more; the first expiry must end the questions", got-1)
	}
	all := a.transcript()
	if !strings.Contains(all, "left the rest on my defaults") {
		t.Errorf("an abandoned tutorial did not say what it had done:\n%s", all)
	}
	for _, want := range []string{"This chat is private", "The group chat is shared", "What happens when I write something down"} {
		if !strings.Contains(all, want) {
			t.Fatalf("an abandoned tutorial swallowed the explanation; missing %q:\n%s", want, all)
		}
	}
	got := ps.get("david")
	if got.Language != "" || got.AgentName != "" || got.Tone != "" || got.Character != "" {
		t.Errorf("an abandoned tutorial left settings behind: %+v", got)
	}
	if !got.Explained {
		t.Error("an abandoned tutorial did not record that the explanation was sent")
	}
}

// TestTutorialCommitsEachAnswerAsItArrives is the restart story. There is no
// in-progress record anywhere; what makes an interrupted tutorial safe is that every
// answer is already written when the next question goes out.
func TestTutorialCommitsEachAnswerAsItArrives(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangSpanish}} // then silence
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, nil, nil))

	if ps.count() < 2 {
		t.Fatalf("the store was written %d times; an answer must be committed before the next question", ps.count())
	}
	if got := ps.get("david").Language; got != spanish.name {
		t.Errorf("the one answer given before the member vanished was lost: %+v", ps.get("david"))
	}
	if got := ps.get("david").ChatID; got != 500 {
		t.Errorf("the chat the tutorial ran in was not recorded: %d", got)
	}
}

// TestTutorialSurvivesARestartMidQuestion simulates the node dying between the
// greeting and the explanation: the tutorial's context is cancelled, and on the next
// start FinishInterrupted owes that member the part kenward promised rather than
// asked.
func TestTutorialSurvivesARestartMidQuestion(t *testing.T) {
	ps := newPersonas()

	// First run: the language is answered, then the node goes down mid-question.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &scriptedAsker{script: []string{choiceLangSpanish}, cancelAt: 2, cancel: cancel}
	tu := tutorialFor(t, first, ps, nil, nil)
	tu.Timeout = time.Minute // long: the ending here is cancellation, not a timeout
	if err := tu.Run(ctx); err == nil {
		t.Fatal("a tutorial killed mid-question reported success")
	}

	saved := ps.get("david")
	if saved.Language != spanish.name {
		t.Fatalf("the answer given before the restart was lost: %+v", saved)
	}
	if saved.Explained {
		t.Fatal("a tutorial cut off before the explanation recorded it as sent")
	}

	// Second run: a fresh process sweeps for onboardings that never finished.
	second := &scriptedAsker{}
	if err := FinishInterrupted(context.Background(), second, ps, LangSpanish, false, nil); err != nil {
		t.Fatalf("FinishInterrupted: %v", err)
	}
	all := second.transcript()
	es := lang.For(spanish.name)
	for _, want := range []string{es.EnrolPrivateBody, es.EnrolGroupBody, es.EnrolMemoryBodyDefault} {
		if !strings.Contains(all, want) {
			t.Errorf("the restart did not deliver the explanation, in the language chosen before it:\n%s", all)
		}
	}
	for _, o := range second.sent {
		if o.ChatID != 500 {
			t.Errorf("the explanation went to chat %d, not the one the tutorial ran in", o.ChatID)
		}
	}
	if !ps.get("david").Explained {
		t.Error("the sweep did not mark the member explained; the next start would send it again")
	}

	// And it is not sent twice.
	third := &scriptedAsker{}
	if err := FinishInterrupted(context.Background(), third, ps, LangSpanish, false, nil); err != nil {
		t.Fatalf("FinishInterrupted (second sweep): %v", err)
	}
	if got := len(third.sentTexts()); got != 0 {
		t.Errorf("a second sweep sent %d messages to a member already explained to", got)
	}
}

// parkedAsker posts the first question and then never comes back, which is what a
// node killed while a question is on screen looks like from the store's side: no
// line of the tutorial after that Ask ever runs, so nothing after it is ever
// written. Cancelling the context instead would let Run's ending — and its writes —
// happen, which is a graceful shutdown and a different test.
type parkedAsker struct {
	mu     sync.Mutex
	sent   []transport.Outbound
	posted chan struct{}
}

func (a *parkedAsker) Send(ctx context.Context, o transport.Outbound) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, o)
	return nil
}

func (a *parkedAsker) Ask(ctx context.Context, q transport.Question) (transport.Answer, error) {
	close(a.posted)
	<-ctx.Done()
	return transport.Answer{}, ctx.Err()
}

// TestTutorialKilledBeforeTheFirstAnswerIsStillExplainedTo is the one path where the
// part kenward owes a member could be silently never paid.
//
// A member who taps nothing has no answers to record, so before this the store held
// no row for them at all, and a sweep that skips members with no chat to write to
// skipped them for ever. Restarting again never fixed it: there was nothing to find.
func TestTutorialKilledBeforeTheFirstAnswerIsStillExplainedTo(t *testing.T) {
	ps := newPersonas()
	a := &parkedAsker{posted: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())

	tu := tutorialFor(t, a, ps, nil, func(tu *Tutorial) { tu.Household = LangSpanish })
	tu.Timeout = time.Minute // the ending here is a kill, not a timeout
	done := make(chan struct{})
	go func() { defer close(done); _ = tu.Run(ctx) }()
	<-a.posted
	// The node dies here. Everything below is the next start.
	t.Cleanup(func() { cancel(); <-done })

	killed := ps.get("david")
	if killed.ChatID != 500 {
		t.Fatalf("a tutorial killed before the first answer left no chat to finish in: %+v", killed)
	}
	if killed.Explained {
		t.Fatal("a tutorial that never reached the explanation recorded it as sent")
	}

	second := &scriptedAsker{}
	if err := FinishInterrupted(context.Background(), second, ps, LangSpanish, false, nil); err != nil {
		t.Fatalf("FinishInterrupted: %v", err)
	}
	all := second.transcript()
	// In the household's language: the member never got as far as choosing one, and
	// falling back to English would explain the memory model in a language nobody in
	// this house asked for.
	es := lang.For(spanish.name)
	for _, want := range []string{es.EnrolPrivateBody, es.EnrolGroupBody, es.EnrolMemoryBodyDefault} {
		if !strings.Contains(all, want) {
			t.Errorf("the member who answered nothing was never told how memory works; missing %q in:\n%s", want, all)
		}
	}
	for _, o := range second.sent {
		if o.ChatID != 500 {
			t.Errorf("the explanation went to chat %d, not the one the tutorial ran in", o.ChatID)
		}
	}

	// Exactly once: a sweep that sent it again on every start would be its own defect.
	third := &scriptedAsker{}
	if err := FinishInterrupted(context.Background(), third, ps, LangSpanish, false, nil); err != nil {
		t.Fatalf("FinishInterrupted (second sweep): %v", err)
	}
	if got := len(third.sentTexts()); got != 0 {
		t.Errorf("a second sweep sent %d messages to a member already explained to", got)
	}
}

// TestGreetingPromisesTheQuestionsItWillAsk. The greeting said "four" whatever the
// household had chosen, and under one agent for the whole household there is nothing
// to name, so three are asked. A member counting them is owed the right number.
func TestGreetingPromisesTheQuestionsItWillAsk(t *testing.T) {
	// The step list is language, register and character, plus the agent name only
	// when each member has an agent of their own to name.
	words := map[string]map[bool]string{
		LangEnglish: {false: "Three", true: "Four"},
		LangSpanish: {false: "Tres", true: "Cuatro"},
	}
	ctx := context.Background()
	for tag := range tables {
		for _, oneEach := range []bool{false, true} {
			opts := []Option{WithLanguage(tag)}
			if oneEach {
				opts = append(opts, WithOneEach())
			}
			h := newHarness(t, nil, opts...)
			code, err := h.claimer.Mint(ctx, "David", 0)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			res, err := h.claimer.Handle(ctx, claim(500, 42, "/start "+code))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			got := res.Messages[0].Text
			if want := words[tag][oneEach]; !strings.Contains(got, want) {
				t.Errorf("%s one_each=%v: greeting does not promise %q:\n%s", tag, oneEach, want, got)
			}
			if wrong := words[tag][!oneEach]; strings.Contains(got, wrong) {
				t.Errorf("%s one_each=%v: greeting promises %q questions, which is not what it asks:\n%s",
					tag, oneEach, wrong, got)
			}
		}
	}
}

// TestTutorialSkipEverythingIsTodaysBehaviour: every question has a skip, and taking
// all of them must leave a member indistinguishable from one enrolled before any of
// this existed.
func TestTutorialSkipEverythingIsTodaysBehaviour(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceSkip, choiceSkip}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, []string{skipWord, skipWord},
		func(tu *Tutorial) { tu.OneEach = true }))

	got := ps.get("david")
	want := Persona{ChatID: 500, Explained: true}
	if got != want {
		t.Errorf("skipping everything left %+v, want %+v", got, want)
	}
	if !strings.Contains(a.transcript(), "This chat is private") {
		t.Error("skipping everything skipped the explanation too")
	}
	// Every button question offers a way out.
	for _, q := range a.questions() {
		has := false
		for _, c := range q.Choices {
			if c.ID == choiceSkip {
				has = true
			}
		}
		if !has {
			t.Errorf("question %q has no skip", q.Text)
		}
	}
}

// TestTutorialBack walks back from the register question to the language one.
func TestTutorialBack(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceBack, choiceLangSpanish, choiceToneFlat}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, []string{skipWord}, nil))

	qs := a.questions()
	if len(qs) != 4 {
		t.Fatalf("asked %d questions, want 4 (language, register, language again, register again)", len(qs))
	}
	if !strings.Contains(qs[2].Text, "Language") && !strings.Contains(qs[2].Text, "Idioma") {
		t.Errorf("Back did not return to the language question: %q", qs[2].Text)
	}
	if got := ps.get("david").Language; got != spanish.name {
		t.Errorf("the answer given after going back did not win: %q", got)
	}
}

// TestTutorialBackFromTypedQuestion: /back at a typed question returns to the one
// before it.
func TestTutorialBackFromTypedQuestion(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceToneWarm, choiceToneFlat}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, []string{backWord, skipWord}, nil))

	if got := len(a.questions()); got != 3 {
		t.Fatalf("asked %d questions, want 3: /back at the character question re-asks the register", got)
	}
	if got := ps.get("david").Tone; got != ToneFlat {
		t.Errorf("register = %q, want the answer given after going back", got)
	}
}

// TestTutorialRejectsNonsense: what a member types is bounded before it is stored,
// because it ends up in a system prompt.
func TestTutorialRejectsNonsense(t *testing.T) {
	long := strings.Repeat("x", MaxAgentNameLen+1)
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceToneFlat}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, []string{
		long,                                   // too long for a name: re-asked
		"   ",                                  // nothing at all: re-asked
		"Jeeves",                               // fine
		strings.Repeat("y", MaxCharacterLen+1), // too long for a character: re-asked
		"terse\nand\nmultiline",
	}, func(tu *Tutorial) { tu.OneEach = true }))

	all := a.transcript()
	if !strings.Contains(all, "Forty characters or fewer") {
		t.Errorf("an over-long name was not refused:\n%s", all)
	}
	if !strings.Contains(all, "Three hundred characters or fewer") {
		t.Errorf("an over-long character description was not refused:\n%s", all)
	}
	got := ps.get("david")
	if got.AgentName != "Jeeves" {
		t.Errorf("agent name = %q, want the one that passed validation", got.AgentName)
	}
	if got.Character != "terse and multiline" {
		t.Errorf("character = %q; member text must reach the store on one line", got.Character)
	}
	if strings.ContainsAny(got.Character, "\n\r") {
		t.Error("member-written character text kept its newlines")
	}
}

// TestTutorialNameOnlyUnderOneEach: there is nothing to name when the household has
// one agent, and asking would be offering a setting that does nothing.
func TestTutorialNameOnlyUnderOneEach(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceToneFlat}}
	mustRun(t, tutorialFor(t, a, newPersonas(), []string{skipWord}, nil))
	if strings.Contains(a.transcript(), "What would you like to call me") {
		t.Error("the tutorial asked for an agent name under one agent per household")
	}
}

// TestTutorialOnlyAcceptsTapsFromTheMemberItIsFor. The claim arrived in a direct
// chat, but the filter is what stops a question in any chat being answered by
// somebody else, and it costs one field to keep.
func TestTutorialOnlyAcceptsTapsFromTheMemberItIsFor(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceToneFlat}}
	mustRun(t, tutorialFor(t, a, newPersonas(), []string{skipWord}, nil))
	for _, q := range a.questions() {
		if q.AllowedUserID != testMember.TelegramID {
			t.Errorf("question %q accepts taps from %d, want only %d", q.Text, q.AllowedUserID, testMember.TelegramID)
		}
	}
}

// TestTutorialTransportFailureStillReports: a transport that dies mid-question is
// an operator's problem, and the explanation not arriving must be visible as an
// error rather than swallowed.
func TestTutorialTransportFailureStillReports(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish}, sendErr: errors.New("bot gone")}
	ps := newPersonas()
	tu := tutorialFor(t, a, ps, nil, nil)
	if err := tu.Run(context.Background()); err == nil {
		t.Fatal("Run reported success with a transport that could not send anything")
	}
	if ps.get("david").Explained {
		t.Error("a failed send recorded the explanation as delivered")
	}
}

// TestGreetingIsInTheHouseholdLanguage: the one message that arrives before a member
// can say anything follows the household's choice, per IDENTITY.md.
func TestGreetingIsInTheHouseholdLanguage(t *testing.T) {
	if got := Greeting(500, "David", textFor(LangSpanish), 4); !strings.Contains(got.Text, "Ya estás dentro") {
		t.Errorf("greeting = %q, want the Spanish one", got.Text)
	}
	if got := Greeting(500, "David", textFor("fr"), 4); !strings.Contains(got.Text, "You're in") {
		t.Errorf("an unheld language did not fall back to English: %q", got.Text)
	}
}

// TestEveryLanguageIsComplete: a table with a hole in it is an empty message in
// front of a member, which is exactly what a struct-of-strings is meant to prevent
// and exactly what a forgotten field would produce anyway.
func TestEveryLanguageIsComplete(t *testing.T) {
	for tag, tbl := range tables {
		if tbl.tag != tag {
			t.Errorf("table %q says its tag is %q", tag, tbl.tag)
		}
		if tbl.greeting == nil || tbl.otherNoted == nil || tbl.nameSet == nil {
			t.Fatalf("table %q is missing a message builder", tag)
		}
		for name, s := range map[string]string{
			"name": tbl.name, "languageQ": tbl.languageQ, "languageOther": tbl.languageOther,
			"otherPrompt": tbl.otherPrompt, "skip": tbl.skip, "back": tbl.back,
			"sameAsHousehold": tbl.sameAsHousehold, "retired": tbl.retired,
			"nameQ": tbl.nameQ, "nameTooLong": tbl.nameTooLong, "nameKept": tbl.nameKept,
			"registerQ": tbl.registerQ, "registerFlat": tbl.registerFlat,
			"registerWarm": tbl.registerWarm, "registerPlayful": tbl.registerPlayful,
			"characterQ": tbl.characterQ, "characterTooLong": tbl.characterTooLong,
			"abandoned": tbl.abandoned,
		} {
			if strings.TrimSpace(s) == "" {
				t.Errorf("table %q has no %s", tag, name)
			}
		}
		if tbl.greeting("David", tbl.number(4)) == "" || tbl.otherNoted("X") == "" || tbl.nameSet("X") == "" {
			t.Errorf("table %q builds an empty message", tag)
		}
		// Every count the step list can produce has a word in this language. The
		// fallback to digits exists so nothing is ever blank, not so a table can
		// leave the number out.
		for _, oneEach := range []bool{false, true} {
			n := questionCount(oneEach)
			if _, ok := tbl.numbers[n]; !ok {
				t.Errorf("table %q has no word for %d, the number of questions it will ask", tag, n)
			}
		}
	}
}

// TestExplanationCopyInEveryLanguage: the promise the last message makes has to
// track capture.private_writes in every language, not only the one somebody
// remembered to update. It went out wrong over real Telegram once in English.
//
// It walks the catalogue's languages rather than this package's, because that is
// where the explanation now lives — ten of them rather than two.
func TestExplanationCopyInEveryLanguage(t *testing.T) {
	for _, tag := range lang.Tags() {
		c := lang.For(tag)
		for _, ask := range []bool{false, true} {
			all := ""
			for _, m := range Explanation(500, c, ask) {
				all += m.Text + "\n"
			}
			want, reject := c.EnrolMemoryBodyDefault, c.EnrolMemoryBodyAsk
			if ask {
				want, reject = c.EnrolMemoryBodyAsk, c.EnrolMemoryBodyDefault
			}
			if !strings.Contains(all, want) {
				t.Errorf("%s askPrivate=%v: the explanation does not make the right promise", tag, ask)
			}
			if strings.Contains(all, reject) {
				t.Errorf("%s askPrivate=%v: the explanation makes the other policy's promise", tag, ask)
			}
		}
	}
}
