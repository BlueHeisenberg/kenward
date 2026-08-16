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
	// Timeout bounds the wait for each answer. Zero means DefaultTutorialTimeout.
	Timeout time.Duration
	// Logger receives what the member does not: why a tutorial ended early.
	Logger *slog.Logger

	// cur is the language the tutorial is currently being delivered in. It starts as
	// the household's and changes exactly once, at the language question.
	cur text
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

	for _, out := range Explanation(t.ChatID, lang.For(cmp.Or(p.Language, t.Household)), t.AskPrivate) {
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
func (t *Tutorial) steps() []func(context.Context, *Persona) outcome {
	steps := []func(context.Context, *Persona) outcome{t.askLanguage}
	if t.OneEach {
		// Nothing to name when the household shares one agent.
		steps = append(steps, t.askName)
	}
	return append(steps, t.askRegister, t.askCharacter)
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
	ans, err := t.Asker.Ask(ctx, transport.Question{
		ChatID:        t.ChatID,
		Text:          q,
		Choices:       choices,
		AllowedUserID: t.Member.TelegramID,
		Timeout:       t.timeout(),
		RetiredNote:   t.cur.retired,
	})
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
func FinishInterrupted(ctx context.Context, a Asker, ps PersonaStore, household string, askPrivate bool, log *slog.Logger) error {
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
		sent := true
		for _, out := range Explanation(p.ChatID, lang.For(cmp.Or(p.Language, household)), askPrivate) {
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
