package enrol

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// DefaultTutorialTimeout is how long the tutorial waits for one answer before it
// decides the member has gone.
//
// It is per question and the first expiry ends the whole thing, so it is also the
// longest a member who taps nothing waits for the explanation. Five minutes is long
// enough to answer the door and come back and short enough that an abandoned
// tutorial is not still holding a goroutine when the household sits down to dinner.
const DefaultTutorialTimeout = 5 * time.Minute

// Asker is the slice of a transport the tutorial needs: it sends messages and it
// asks questions with buttons. transport.Transport satisfies it, and so does a Mux
// view, which is what the supervisor actually passes.
//
// It is narrowed rather than taken whole so that the tutorial cannot read the
// update stream. Free-text answers arrive on the Answers channel, which the caller
// fills from the one reader that already owns that stream; a second consumer would
// race the enrolment pump for the next message.
type Asker interface {
	Send(ctx context.Context, o transport.Outbound) error
	Ask(ctx context.Context, q transport.Question) (transport.Answer, error)
}

// Tutorial is one member's walk through the personal setup and the explanation.
//
// It is single-use: construct one per enrolment, call Run once, discard it. Nothing
// about a tutorial in progress is written down — see Persona.Explained for the one
// exception and why — so a Tutorial that stops existing, whether because the member
// stopped answering or because the process died, leaves a member configured with
// exactly the answers they had already given and defaults for the rest.
type Tutorial struct {
	// Member is the freshly enrolled member. Only ID and Name are read.
	Member domain.Member
	// ChatID is the private chat the claim arrived in.
	ChatID int64
	// Asker sends the messages and puts the questions.
	Asker Asker
	// Answers carries this member's typed messages for as long as the tutorial is
	// running. It may be nil, which turns the two typed questions into skips; the
	// button questions still work.
	Answers <-chan transport.Inbound
	// Personas is where each answer is committed as it is given. It may be nil, in
	// which case the tutorial still runs and records nothing — which is a
	// misconfiguration, not a mode, and the caller should not do it.
	Personas PersonaStore
	// Household is the household's language tag. It is the language the greeting and
	// the first question are in, and the default if the member skips the question.
	Household string
	// OneEach says the household chose one agent per member, which is the only
	// arrangement in which asking a member to name their agent means anything.
	OneEach bool
	// AskPrivate mirrors the Claimer's: capture.private_writes decides which promise
	// the last message of the explanation makes.
	AskPrivate bool
	// Mode is the household's deployment topology, and it decides how strong a claim
	// the first message of the explanation is allowed to make. Unset means unknown,
	// which makes the weaker claim. See Explanation.
	Mode privacy.Mode
	// Timeout bounds the wait for each answer. Zero means DefaultTutorialTimeout.
	Timeout time.Duration
	// Nudge, if set, is handed the sentence to send a member who types while a button
	// question is on screen, and "" whenever no such question is up. It may be nil,
	// which is the behaviour before it existed: the message is dropped and nothing is
	// said.
	//
	// It is a callback rather than a message this package sends, because the message
	// is not this package's to send. What the member typed never reaches here at all
	// — the tutorial is blocked inside Asker.Ask, so nothing is reading Answers, and
	// the non-blocking send that drops it happens in whoever owns the update stream.
	// That reader is the only code that knows a message arrived, and this is the only
	// code that knows what language to answer it in: a member who chose Spanish at
	// question one is not in the household's language any more. So the language comes
	// this way and the sending stays there.
	//
	// The contract is the reader's: it is called with a sentence when a question goes
	// up and with "" when it comes down, and sending it at most once per question is
	// what stops a chatty member collecting one nudge per message.
	Nudge func(string)
	// Logger receives what the member does not: why a tutorial ended early.
	Logger *slog.Logger

	// cur is the language the tutorial is currently being delivered in. It starts as
	// the household's and changes exactly once, at the language question.
	cur text
	// pending is the message id of the question on screen right now, and zero between
	// questions. Every save carries it, so what is on disk while a question is up is
	// what the next start needs to retire it. See ask.
	pending int
	// answered is what the member has settled on, kept so the caller can read it
	// once Run has returned. See Answered.
	answered Persona
}

// Answered is what the member chose, as of the last question they answered.
//
// It is for the one caller that has to act on the answers rather than store them: the
// supervisor gives this member a unit the moment the tutorial ends, and a unit built
// from a configuration that does not yet carry their persona would answer in
// kenward's voice until the next restart. The store has the same values and is the
// record; this saves the caller reading its own write back.
//
// Valid only after Run has returned, and read from the goroutine that called it.
func (t *Tutorial) Answered() Persona { return t.answered }

// outcome is what one question decided.
type outcome int

const (
	// advance moves to the next question.
	advance outcome = iota
	// back returns to the previous one.
	back
	// retry puts the same question again, because what came back was not usable.
	retry
	// abandon ends the questions: the member has stopped answering, or the transport
	// has gone. The explanation is still sent.
	abandon
)

// Run walks the member through the tutorial and then sends the explanation.
//
// The explanation is sent on every ending, including abandonment: the questions are
// a convenience and the memory model is not. Run returns an error only for a failure
// the operator needs to see — a member who answers nothing is an ordinary outcome
// and returns nil.
func (t *Tutorial) Run(ctx context.Context) error {
	t.cur = textFor(t.Household)
	p := Persona{ChatID: t.ChatID}

	// Written before the first question rather than after the first answer.
	//
	// It is the one fact the sweep needs to find this member again — which chat to
	// finish in — and a member who is killed before they tap anything has no answers
	// to carry it. Recorded after the first answer instead, that member had no row at
	// all, FinishInterrupted skipped them for ever, and the memory model was never
	// delivered: the one thing the product owes rather than asks. It is not a second
	// writer of the record — it is the same save, one question earlier — so
	// config.Binder still owns the state file and its clone-write-swap alone.
	t.answered = p
	t.save(ctx, p)

	steps := t.steps()

	gone := false
	for i := 0; i < len(steps); {
		switch steps[i](ctx, &p) {
		case advance:
			i++
		case back:
			if i > 0 {
				i--
			}
		case retry:
			// Same question again; the step has already said why.
		case abandon:
			gone = true
			i = len(steps)
		default:
			// skipped, which a step is expected to have turned into one of the four
			// above because what skipping means differs per question. Reaching here
			// is a bug, and moving on is the only ending that is not an infinite
			// loop in front of a member.
			i++
		}
		t.answered = p
		t.save(ctx, p)
	}

	if gone {
		t.log("supervisor: tutorial abandoned; member left on defaults")
		// Said before the explanation rather than after, so a member coming back to
		// three messages of memory model knows why the questions stopped.
		_ = t.send(ctx, t.cur.abandoned)
	}

	for _, out := range Explanation(t.ChatID, lang.For(cmp.Or(p.Language, t.Household)), t.AskPrivate, t.Mode) {
		if err := t.Asker.Send(ctx, out); err != nil {
			// The member has part of the explanation and the rest is not coming.
			// Explained stays false, so the next start finishes the job.
			return err
		}
	}
	p.Explained = true
	t.answered = p
	t.save(ctx, p)
	return nil
}

// steps is the questions this tutorial will ask, in order.
//
// It is a method rather than a literal inside Run because the greeting promises how
// many there are before Run exists, and a number written out twice is a number that
// drifts — it already had, and every member enrolled under one agent per household
// was promised four questions and asked three.
//
// Under one shared assistant the list is the language question and nothing else, and
// the rule that decides it is the one askName was already written to: do not ask a
// question whose answer this household throws away. Name, register and character are
// the household's voice under AgentsShared — config.PersonaFor resolves all three to
// the household's whatever a member answered — so asking a member for them recorded
// three fields into state.json that no conversation would ever read. It did: a live
// member's file held a language, a tone and a character, and the default install used
// none of them.
//
// Language survives because config.PersonaFor now honours it in both topologies, for
// the reasons written there. It is also the question that has to be asked first
// whatever else follows, since every message after it is delivered in the answer.
func (t *Tutorial) steps() []func(context.Context, *Persona) outcome {
	steps := []func(context.Context, *Persona) outcome{t.askLanguage}
	if t.OneEach {
		// Nothing to name, no register and no character of their own when the
		// household shares one agent: it has one voice and this member is not it.
		steps = append(steps, t.askName, t.askRegister, t.askCharacter)
	}
	return steps
}

// questionCount is how many questions a member of this household will be asked. It
// is the greeting's promise, counted from the step list itself.
func questionCount(oneEach bool) int { return len((&Tutorial{OneEach: oneEach}).steps()) }

// askLanguage is question one, and everything after it is delivered in the answer.
//
// The choices are exactly the languages this package is written in, plus a way to
// name one it is not. Offering a language the tutorial cannot then be held in would
// be asking a question and ignoring the answer for five messages, which is the
// failure this ordering exists to prevent.
func (t *Tutorial) askLanguage(ctx context.Context, p *Persona) outcome {
	choices := []transport.Choice{
		{ID: choiceLangEnglish, Label: english.name},
		{ID: choiceLangSpanish, Label: spanish.name},
		{ID: choiceLangOther, Label: t.cur.languageOther},
		{ID: choiceSkip, Label: t.cur.sameAsHousehold},
	}
	ans, out := t.ask(ctx, t.cur.languageQ, choices)
	if out != advance {
		return out
	}
	switch ans.ChoiceID {
	case choiceSkip:
		// Inherit. Left empty rather than copied, so a household that changes its
		// language later carries this member with it.
		return advance
	case choiceLangEnglish, choiceLangSpanish:
		// The language's own name is recorded, not the tag. config's Language is
		// free text on purpose — it is passed to the model rather than looked up in
		// a table — and a member's persona reading `language: es` would put a tag in
		// a system prompt. The tag is this package's index into its own copy and
		// nothing else's; TagFor recovers it.
		t.cur = textFor(strings.TrimPrefix(ans.ChoiceID, "lang."))
		p.Language = t.cur.name
		return advance
	}

	// A language the tutorial is not written in. Ask which, record it, and say
	// plainly that the rest of this is in English anyway.
	if err := t.send(ctx, t.cur.otherPrompt); err != nil {
		return abandon
	}
	named, out := t.typed(ctx)
	if out == skipped {
		// They opened the door and then shut it. Inherit, same as tapping the
		// household's language.
		return advance
	}
	if out != advance {
		return out
	}
	if utf8.RuneCountInString(named) > MaxAgentNameLen {
		named = string([]rune(named)[:MaxAgentNameLen])
	}
	p.Language = named
	t.cur = english
	if err := t.send(ctx, t.cur.otherNoted(named)); err != nil {
		return abandon
	}
	return advance
}

// askName asks what this member's agent is called. Under one agent per household
// there is nothing to name, and this step is not in the list at all.
func (t *Tutorial) askName(ctx context.Context, p *Persona) outcome {
	if err := t.send(ctx, t.cur.nameQ); err != nil {
		return abandon
	}
	name, out := t.typed(ctx)
	switch {
	case out == skipped:
		p.AgentName = ""
		return t.confirm(ctx, t.cur.nameKept)
	case out != advance:
		return out
	}
	name = oneLine(name)
	if name == "" {
		return retry
	}
	if utf8.RuneCountInString(name) > MaxAgentNameLen {
		if err := t.send(ctx, t.cur.nameTooLong); err != nil {
			return abandon
		}
		return retry
	}
	p.AgentName = name
	return t.confirm(ctx, t.cur.nameSet(name))
}

// askRegister asks how the agent should sound. The flat register is kenward's
// default and stays the first choice.
func (t *Tutorial) askRegister(ctx context.Context, p *Persona) outcome {
	choices := []transport.Choice{
		{ID: choiceToneFlat, Label: t.cur.registerFlat},
		{ID: choiceToneWarm, Label: t.cur.registerWarm},
		{ID: choiceTonePlayful, Label: t.cur.registerPlayful},
		{ID: choiceBack, Label: t.cur.back},
		{ID: choiceSkip, Label: t.cur.skip},
	}
	ans, out := t.ask(ctx, t.cur.registerQ, choices)
	if out != advance {
		return out
	}
	switch ans.ChoiceID {
	case choiceBack:
		return back
	case choiceSkip:
		return advance
	}
	p.Tone = strings.TrimPrefix(ans.ChoiceID, "tone.")
	return advance
}

// askCharacter is the last question and the only one where a member writes anything
// that ends up in a system prompt. What it accepts is bounded here — one line, three
// hundred characters — so that whatever renders it downstream is handed something
// already the right shape.
func (t *Tutorial) askCharacter(ctx context.Context, p *Persona) outcome {
	if err := t.send(ctx, t.cur.characterQ); err != nil {
		return abandon
	}
	desc, out := t.typed(ctx)
	switch {
	case out == skipped:
		p.Character = ""
		return advance
	case out != advance:
		return out
	}
	desc = oneLine(desc)
	if desc == "" {
		return retry
	}
	if utf8.RuneCountInString(desc) > MaxCharacterLen {
		if err := t.send(ctx, t.cur.characterTooLong); err != nil {
			return abandon
		}
		return retry
	}
	p.Character = desc
	// Acknowledged like the name is. Without it a member who has just written a
	// sentence about themselves goes straight into three messages of memory model
	// with nothing to say the sentence landed.
	return t.confirm(ctx, t.cur.characterNoted)
}

// skipped is the outcome of a typed answer that was the skip word. It is not one of
// the four the loop switches on: a step turns it into whatever skipping that
// particular question means, which is not always "advance with nothing set".
const skipped outcome = -1

// ask puts a question with buttons and maps the answer onto an outcome. A timeout,
// a tap from anyone but this member, or a transport that has gone all mean the same
// thing: stop asking.
func (t *Tutorial) ask(ctx context.Context, q string, choices []transport.Choice) (transport.Answer, outcome) {
	// Around the whole call, in this language, because this is the window in which a
	// typed message is not an answer to anything. Cleared on every ending, including
	// an abandoned one: a nudge left armed after the questions stop would answer a
	// message the tutorial is no longer waiting for.
	t.nudge(t.cur.typedNotAnAnswer)
	defer t.nudge("")
	ans, err := t.Asker.Ask(ctx, transport.Question{
		ChatID:        t.ChatID,
		Text:          q,
		Choices:       choices,
		AllowedUserID: t.Member.TelegramID,
		Timeout:       t.timeout(),
		RetiredNote:   t.cur.retired,
		// Written down while the keyboard is live, because that is the only moment
		// this process is sure of it. Ask retires its own message on every ending it
		// can see; the one it cannot see is the node being killed, and then the id is
		// all the next start has to work with. Posted runs on this goroutine before
		// any answer can arrive, so the write is ordered before the read below.
		Posted: func(id int) {
			t.pending = id
			t.save(ctx, t.answered)
		},
	})
	t.pending = 0
	switch {
	case err != nil:
		t.log("supervisor: tutorial question failed", "error", err)
		return ans, abandon
	case ans.TimedOut:
		return ans, abandon
	}
	return ans, advance
}

// typed waits for the member to write something back.
//
// The channel is fed by the enrolment pump, which is the process's only reader of
// this member's messages until they are promoted to a unit of their own; reading it
// here is therefore not a second consumer but the same one, handing over.
func (t *Tutorial) typed(ctx context.Context) (string, outcome) {
	if t.Answers == nil {
		return "", skipped
	}
	timer := time.NewTimer(t.timeout())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", abandon
		case <-timer.C:
			return "", abandon
		case in, ok := <-t.Answers:
			if !ok {
				return "", abandon
			}
			s := strings.TrimSpace(in.Text)
			switch strings.ToLower(s) {
			case skipWord:
				return "", skipped
			case backWord:
				return "", back
			case "":
				continue // a sticker, a photo, an empty edit: not an answer
			}
			return s, advance
		}
	}
}

// confirm sends a short acknowledgement and advances. A failure to send it is a
// transport that has gone, not a reason to re-ask.
func (t *Tutorial) confirm(ctx context.Context, s string) outcome {
	if err := t.send(ctx, s); err != nil {
		return abandon
	}
	return advance
}

func (t *Tutorial) send(ctx context.Context, s string) error {
	return t.Asker.Send(ctx, transport.Outbound{ChatID: t.ChatID, Text: s})
}

// nudge publishes what to say to a member who types right now, or "" for nothing.
func (t *Tutorial) nudge(s string) {
	if t.Nudge != nil {
		t.Nudge(s)
	}
}

func (t *Tutorial) timeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return DefaultTutorialTimeout
}

// save commits the answers so far. It is called after every question, which is what
// makes an interrupted tutorial equivalent to a skipped one: what was answered is
// already written, and what was not is a zero field meaning "the household's".
func (t *Tutorial) save(ctx context.Context, p Persona) {
	if t.Personas == nil {
		return
	}
	p.QuestionMsg = t.pending
	// Detached from ctx: this runs on the way out of a cancelled tutorial too, and
	// losing the answers a member did give because the node is shutting down would
	// be the one failure this write exists to prevent.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := t.Personas.SetPersona(saveCtx, t.Member.ID, p); err != nil {
		t.log("supervisor: could not record the member's setup answers", "error", err)
	}
}

func (t *Tutorial) log(msg string, args ...any) {
	if t.Logger == nil {
		return
	}
	t.Logger.Info(msg, append([]any{"member", string(t.Member.ID)}, args...)...)
}

// oneLine flattens whatever a member typed into a single line with single spaces.
//
// Member-written text reaches a system prompt, where a newline at column zero is
// how an instruction stops looking like data. Doing it here means nothing
// downstream has to remember to.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// keyboardRetirer is the part of a transport that can strip the buttons off a
// question an earlier process asked. transport.Telegram has it, and so do a Mux view
// and the Fake; an Asker without it leaves the keyboard where it is, which is what
// happened to every interrupted member before this.
//
// It is asserted for rather than added to Asker because Asker is implemented by
// fakes in several packages, and none of them has an onboarding to clean up after.
type keyboardRetirer interface {
	RetireKeyboard(ctx context.Context, chatID int64, messageID int) error
}

// FinishInterrupted sends the explanation to every member whose tutorial started but
// never reached it, and marks them done.
//
// It exists for one case: the node restarted between a member's greeting and the end
// of their tutorial. Everything else about an interrupted tutorial resolves itself —
// unanswered questions are unset fields and unset fields are the household's
// defaults — but the explanation is the part kenward owes the member rather than
// asks of them, and nothing else would ever send it. A member enrolled before
// personas existed has no record here and is not swept; they were explained to
// under the old one-shot onboarding.
//
// household is the language to explain in for a member who never reached the
// language question, which is exactly the member this sweep exists for. Their
// persona's language is empty because empty means "the household's", and reading it
// as English would explain the memory model in a language nobody here asked for.
func FinishInterrupted(ctx context.Context, a Asker, ps PersonaStore, household string, askPrivate bool, mode privacy.Mode, log *slog.Logger) error {
	if a == nil || ps == nil {
		return nil
	}
	all, err := ps.Personas(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for id, p := range all {
		if p.Explained || p.ChatID == 0 {
			continue
		}
		if log != nil {
			log.Info("supervisor: finishing an onboarding the last run did not", "member", string(id))
		}
		spoken := cmp.Or(p.Language, household)
		cur := textFor(TagFor(spoken))

		// The keyboard the killed process could not retire. Its token died with that
		// process, so it answers nothing at all when it is tapped while still looking
		// live; retiring it here is the same thing Ask does on every ending it can
		// see. Best effort, and never a reason to withhold the explanation: a failure
		// leaves a dead keyboard, and returning early would leave the member with the
		// dead keyboard and no memory model either.
		if r, ok := a.(keyboardRetirer); ok && p.QuestionMsg != 0 {
			if err := r.RetireKeyboard(ctx, p.ChatID, p.QuestionMsg); err != nil && log != nil {
				log.Warn("supervisor: could not retire the question the last run left on screen",
					"member", string(id), "error", err)
			}
		}
		p.QuestionMsg = 0

		sent := true
		// Said first, as Run says it: a member coming back to three messages of memory
		// model is owed the reason the questions stopped.
		msgs := append([]transport.Outbound{{ChatID: p.ChatID, Text: cur.abandoned}},
			Explanation(p.ChatID, lang.For(spoken), askPrivate, mode)...)
		for _, out := range msgs {
			if err := a.Send(ctx, out); err != nil {
				errs = append(errs, err)
				sent = false
				break
			}
		}
		if !sent {
			continue
		}
		p.Explained = true
		if err := ps.SetPersona(ctx, id, p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
