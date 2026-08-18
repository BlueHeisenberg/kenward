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
	"github.com/BlueHeisenberg/kenward/internal/privacy"
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

// TestTutorialOrder is the ordering the identity design settles on: language first, then the
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
	// One agent each, because that is the household with a question after the
	// language one for the answer to reach. Under one shared assistant the register
	// and the character are the household's and are not asked; what the language
	// still has to carry there is the explanation, and TestSharedTutorialAsksOnly-
	// TheLanguage asserts that half.
	mustRun(t, tutorialFor(t, a, ps, []string{skipWord, skipWord},
		func(tu *Tutorial) { tu.OneEach = true }))

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
	// One agent each: cancelAt counts button questions, and a household that shares
	// one assistant has exactly one of those, so there would be no second question
	// to die at.
	tu := tutorialFor(t, first, ps, []string{skipWord}, func(tu *Tutorial) { tu.OneEach = true })
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
	if err := FinishInterrupted(context.Background(), second, ps, LangSpanish, false, privacy.ModeSimple, nil); err != nil {
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
	if err := FinishInterrupted(context.Background(), third, ps, LangSpanish, false, privacy.ModeSimple, nil); err != nil {
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
	if q.Posted != nil {
		q.Posted(questionMsgID)
	}
	close(a.posted)
	<-ctx.Done()
	return transport.Answer{}, ctx.Err()
}

// questionMsgID is the message the parked question is posted as.
const questionMsgID = 77

// retiringAsker is a scriptedAsker that can also strip a keyboard, which is what the
// transport a real sweep is handed can do.
type retiringAsker struct {
	scriptedAsker
	retired []transport.Retired
}

func (a *retiringAsker) RetireKeyboard(ctx context.Context, chatID int64, messageID int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retired = append(a.retired, transport.Retired{ChatID: chatID, MessageID: messageID})
	return nil
}

// TestRestartRetiresTheKeyboardItLeftOnScreen. A question killed mid-flight keeps its
// buttons, and the token behind them died with the process: tapping one produced no
// outcome line, no acknowledgement and not a single log line, on a keyboard that
// still looked live. The timeout path retires its message; so must the next start.
func TestRestartRetiresTheKeyboardItLeftOnScreen(t *testing.T) {
	ps := newPersonas()
	a := &parkedAsker{posted: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())

	tu := tutorialFor(t, a, ps, nil, nil)
	tu.Timeout = time.Minute
	done := make(chan struct{})
	go func() { defer close(done); _ = tu.Run(ctx) }()
	<-a.posted
	// The node dies with the first question on screen.
	t.Cleanup(func() { cancel(); <-done })

	if got := ps.get("david").QuestionMsg; got != questionMsgID {
		t.Fatalf("the killed tutorial recorded keyboard %d, want the one it left on screen (%d)", got, questionMsgID)
	}

	second := &retiringAsker{}
	if err := FinishInterrupted(context.Background(), second, ps, LangEnglish, false, privacy.ModeSimple, nil); err != nil {
		t.Fatalf("FinishInterrupted: %v", err)
	}
	want := transport.Retired{ChatID: 500, MessageID: questionMsgID}
	if len(second.retired) != 1 || second.retired[0] != want {
		t.Errorf("the restart retired %+v, want exactly %+v", second.retired, want)
	}
	// And it says what it left them on, which is what the timeout path writes onto
	// the message it retires.
	if all := second.transcript(); !strings.Contains(all, english.abandoned) {
		t.Errorf("the restart retired the keyboard and said nothing about it:\n%s", all)
	}
	// Nothing left in the record pointing at a keyboard that is no longer there.
	if got := ps.get("david").QuestionMsg; got != 0 {
		t.Errorf("the record still points at retired keyboard %d", got)
	}
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
	if err := FinishInterrupted(context.Background(), second, ps, LangSpanish, false, privacy.ModeSimple, nil); err != nil {
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
	if err := FinishInterrupted(context.Background(), third, ps, LangSpanish, false, privacy.ModeSimple, nil); err != nil {
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
	// The step list is the language question alone under one shared assistant, and
	// language, name, register and character where each member has an agent of their
	// own. The phrase is quantified, not just numbered, because "One quick questions"
	// is the first thing a member would have read.
	words := map[string]map[bool]string{
		LangEnglish: {false: "One quick question", true: "Four quick questions"},
		LangSpanish: {false: "Una pregunta rápida", true: "Cuatro preguntas rápidas"},
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

// TestTutorialConfirmsWhatTheMemberWrote. The name question says "Bruno it is." and
// the last question said nothing at all: a member typed a sentence about themselves
// and the next thing they saw was the memory model, with no sign it had landed.
func TestTutorialConfirmsWhatTheMemberWrote(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceToneFlat}}
	// One agent each: the character question is only asked where the character is
	// this member's own. The first typed answer is the agent's name, skipped.
	mustRun(t, tutorialFor(t, a, newPersonas(), []string{skipWord, "a bit dry, into cycling"},
		func(tu *Tutorial) { tu.OneEach = true }))

	sent := a.sentTexts()
	asked, noted, explained := -1, -1, -1
	for i, s := range sent {
		switch {
		case strings.Contains(s, "Anything else about how"):
			asked = i
		case strings.Contains(s, "Noted."):
			noted = i
		case strings.Contains(s, "This chat is private"):
			explained = i
		}
	}
	if asked < 0 || explained < 0 {
		t.Fatalf("the tutorial did not run as expected:\n%s", strings.Join(sent, "\n---\n"))
	}
	if noted < 0 {
		t.Fatalf("the character answer got no acknowledgement:\n%s", strings.Join(sent, "\n---\n"))
	}
	if noted < asked || noted > explained {
		t.Errorf("the acknowledgement arrived at %d, not between the question (%d) and the explanation (%d)",
			noted, asked, explained)
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

// TestTutorialBack walks back from a button question to the one before it. Under one
// agent each that is the register question returning to the name, which is the same
// mechanic the step loop has always implemented; the household that shares one
// assistant is asked a single question and has nothing to walk back to.
func TestTutorialBack(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceBack, choiceToneFlat}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, []string{"Jeeves", "Bruno", skipWord},
		func(tu *Tutorial) { tu.OneEach = true }))

	qs := a.questions()
	if len(qs) != 3 {
		t.Fatalf("asked %d button questions, want 3 (language, register, register again)", len(qs))
	}
	if !strings.Contains(qs[2].Text, "How I talk") && !strings.Contains(qs[2].Text, "Cómo hablo") {
		t.Errorf("Back did not return to the question before the register one: %q", qs[2].Text)
	}
	if got := ps.get("david").AgentName; got != "Bruno" {
		t.Errorf("the answer given after going back did not win: %q", got)
	}
}

// TestTutorialBackFromTypedQuestion: /back at a typed question returns to the one
// before it.
func TestTutorialBackFromTypedQuestion(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangEnglish, choiceToneWarm, choiceToneFlat}}
	ps := newPersonas()
	// The name is skipped, the register answered, and /back is typed at the
	// character question — the typed question this household actually has.
	mustRun(t, tutorialFor(t, a, ps, []string{skipWord, backWord, skipWord},
		func(tu *Tutorial) { tu.OneEach = true }))

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

// TestSharedTutorialAsksOnlyWhatItWillUse: under one assistant for the whole household
// the tutorial asks the language and nothing else, because the language is the only one
// of the four answers that household will ever read back.
//
// The rule is the one askName was already written to, applied to the two questions it
// was not. config.PersonaFor resolves the name, the register and the character to the
// household's whenever AgentPerMember is false — and per_member is a validation error in
// simple mode, so under the default install it always is. Asking anyway wrote three
// fields into state.json that no conversation would ever read; a live file held exactly
// that, a language and a tone and a character, none of them used.
//
// The language survives because config.PersonaFor now honours a member's own, and it is
// the answer with a cost attached: it decides the button labels, the write announcement,
// the undo hint and the English-gloss line, none of which a member who does not read the
// household's language can use.
func TestSharedTutorialAsksOnlyWhatItWillUse(t *testing.T) {
	a := &scriptedAsker{script: []string{choiceLangSpanish}}
	ps := newPersonas()
	mustRun(t, tutorialFor(t, a, ps, nil, nil))

	all := a.transcript()
	for what, probe := range map[string]string{
		"an agent name": "What would you like to call me",
		"a register":    spanish.registerQ,
		"a character":   spanish.characterQ,
	} {
		if strings.Contains(all, probe) {
			t.Errorf("the tutorial asked for %s under one assistant for the household, and config.PersonaFor will resolve it to the household's whatever the member answered:\n%s", what, all)
		}
	}
	if got := len(a.questions()); got != 1 {
		t.Errorf("asked %d questions, want 1 (the language)", got)
	}

	// The one answer it does take is recorded, and nothing else is.
	got := ps.get("david")
	want := Persona{
		PersonaConfig: config.PersonaConfig{Language: spanish.name},
		ChatID:        500, Explained: true,
	}
	if got != want {
		t.Errorf("persona = %+v, want %+v", got, want)
	}
	// And the explanation still arrives: it is the half of onboarding that is not a
	// question, and it is the only place the memory model is explained to the member.
	if !strings.Contains(all, lang.For(spanish.name).EnrolPrivateBody) {
		t.Errorf("a one-question tutorial dropped the explanation:\n%s", all)
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
// can say anything follows the household's choice, per the identity design.
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
			"characterNoted": tbl.characterNoted,
			"abandoned":      tbl.abandoned,
		} {
			if strings.TrimSpace(s) == "" {
				t.Errorf("table %q has no %s", tag, name)
			}
		}
		if tbl.greeting("David", tbl.questionCountPhrase(4)) == "" || tbl.otherNoted("X") == "" || tbl.nameSet("X") == "" {
			t.Errorf("table %q builds an empty message", tag)
		}
		// Every count the step list can produce has a phrase in this language. The
		// fallback to digits exists so nothing is ever blank, not so a table can
		// leave the number out.
		for _, oneEach := range []bool{false, true} {
			n := questionCount(oneEach)
			if _, ok := tbl.questionsPhrase[n]; !ok {
				t.Errorf("table %q has no phrase for %d, the number of questions it will ask", tag, n)
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
			for _, m := range Explanation(500, c, ask, privacy.ModeSimple) {
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

// stuckAsker blocks with the question never reaching Telegram: Posted is never called,
// so the record D2 writes when a question posts is never written either.
type stuckAsker struct {
	scriptedAsker
	entered chan struct{}
}

func (a *stuckAsker) Ask(ctx context.Context, q transport.Question) (transport.Answer, error) {
	close(a.entered)
	<-ctx.Done()
	return transport.Answer{}, ctx.Err()
}

// TestTutorialKilledBeforeItsFirstQuestionPostedIsStillExplainedTo isolates the early save.
//
// The sibling test is satisfied by Posted, which writes the same record once the question
// reaches Telegram — so it keeps passing with the early save removed and proves nothing
// about it. Run's ending saves too, so an Ask that merely returns an error proves nothing
// either. The window the early save alone covers is a node that dies while the first
// question is still in flight: no Posted, no answer, and no ending.
func TestTutorialKilledBeforeItsFirstQuestionPostedIsStillExplainedTo(t *testing.T) {
	ps := newPersonas()
	a := &stuckAsker{entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())

	tu := tutorialFor(t, a, ps, nil, func(tu *Tutorial) { tu.Household = LangSpanish })
	tu.Timeout = time.Minute
	go func() { _ = tu.Run(ctx) }()
	<-a.entered
	// The node dies here, inside Ask, before anything reached Telegram.

	got := ps.get("david")
	cancel()
	if got.ChatID != 500 {
		t.Fatalf("a tutorial killed before its first question posted left no chat to finish in: %+v", got)
	}
}

// TestTheTutorialPublishesWhatToSayToSomebodyWhoTypes.
//
// A member typing while a button question is on screen gets their message dropped,
// which is right — it is not an answer to anything, and buffering it would make it
// the answer to the next question — and used to get nothing at all back, on a
// question that looks answerable by typing.
//
// The reply has to come from whoever holds the update stream, because that is the
// only code that sees the message: this tutorial is blocked inside Asker.Ask and
// nothing is reading Answers. But it is the only code that does not know what
// language to write in — the member may have chosen one at question one, and the
// household's is a different setting. So the sentence travels this way.
//
// Asserted here as the contract the pump depends on: armed in the current language
// while a button question is up, and disarmed the moment it comes down, so a nudge
// cannot answer a message the tutorial is no longer waiting for.
func TestTheTutorialPublishesWhatToSayToSomebodyWhoTypes(t *testing.T) {
	var mu sync.Mutex
	var armed []string  // every non-empty sentence published, in order
	var during []string // what was armed at the moment each question went up

	a := &nudgeAsker{}
	tu := tutorialFor(t, a, newPersonas(), []string{"Jeeves", "dry"}, func(tu *Tutorial) {
		tu.OneEach = true
	})
	tu.Nudge = func(s string) {
		mu.Lock()
		defer mu.Unlock()
		if s != "" {
			armed = append(armed, s)
		}
		a.current = s
	}
	a.observe = func(cur string) {
		mu.Lock()
		defer mu.Unlock()
		during = append(during, cur)
	}
	a.script = []string{choiceLangSpanish, choiceToneWarm}
	mustRun(t, tu)

	mu.Lock()
	defer mu.Unlock()
	if len(during) != 2 {
		t.Fatalf("saw %d button questions, want the language and the register", len(during))
	}
	// Question one is the household's language, which here is English.
	if during[0] != english.typedNotAnAnswer {
		t.Errorf("nothing usable was armed for the first question: %q", during[0])
	}
	// Question two is after the member chose Spanish, and it is the whole reason
	// this is not a constant in the pump.
	if during[1] != spanish.typedNotAnAnswer {
		t.Errorf("the second question armed %q, want the language the member just chose", during[1])
	}
	if a.current != "" {
		t.Errorf("a nudge is still armed after the last question: %q", a.current)
	}
	if len(armed) != 2 {
		t.Errorf("armed %d sentences for 2 button questions: %q", len(armed), armed)
	}
}

// nudgeAsker records what the tutorial had armed at the instant each question went
// up, which is the only moment the value means anything.
type nudgeAsker struct {
	scriptedAsker
	current string
	observe func(string)
}

func (a *nudgeAsker) Ask(ctx context.Context, q transport.Question) (transport.Answer, error) {
	if a.observe != nil {
		a.observe(a.current)
	}
	return a.scriptedAsker.Ask(ctx, q)
}
