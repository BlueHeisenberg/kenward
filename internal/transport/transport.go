// Package transport carries conversations to and from household members.
//
// Telegram is the first and only implementation, but the interface is deliberately
// free of it: the transport is a thin adapter, and swapping it must not touch memory
// or routing.
package transport

import (
	"context"
	"errors"
	"time"
)

// Inbound is a message received from a member.
type Inbound struct {
	ChatID    int64
	UserID    int64
	Text      string
	MessageID int
	IsGroup   bool
	At        time.Time
}

// Outbound is a reply.
type Outbound struct {
	ChatID  int64
	Text    string
	ReplyTo int // 0 for none
}

// Choice is one button on a question.
type Choice struct {
	// ID is stable and machine-readable; it is what comes back in an Answer.
	ID string
	// Label is what the member sees.
	Label string
}

// Question asks the member to choose, with buttons, and blocks until they do.
type Question struct {
	ChatID  int64
	Text    string
	Choices []Choice
	// AllowedUserID is the only user whose taps are accepted.
	//
	// This is load-bearing. In a group chat every member can see and tap an inline
	// keyboard; without this filter another member could decide where someone else's
	// memory is stored. Taps from anyone else are ignored silently — not answered,
	// not acknowledged.
	AllowedUserID int64
	// Timeout bounds the wait. Expiry is reported as TimedOut and must be treated as
	// a decline by every caller.
	Timeout time.Duration
	// RetiredNote replaces the outcome line appended to the message when the
	// question ends without a tap — it expired, or it was withdrawn because the
	// context was cancelled or the transport shut down.
	//
	// The default wording says the question was declined or withdrawn, which is
	// right for a question and wrong for a message that reports something already
	// done and offers to take it back. "Saved X … — no answer, treated as declined"
	// reads as though the save had been called off, which is the one thing it must
	// never say. A caller in that shape sets this to what is true once the button
	// stops working; both endings share it, because from the member's side they are
	// the same fact about a button that no longer does anything.
	//
	// Empty keeps the defaults. It is a note, not a whole text: it is appended
	// after the question exactly as a chosen label is.
	RetiredNote string
	// Posted, if set, is called with the message id of the question as soon as it is
	// on screen and before the wait begins.
	//
	// It exists for a caller that has to be able to retire this keyboard from a
	// later process. Ask retires its own message on every ending it can see, but the
	// id is only knowable from inside Ask, and a node killed while the question is up
	// never sees Ask return — so the caller that wants to clean up on the next start
	// has to be handed the id while the question is still live. It runs on the
	// goroutine that called Ask, before any answer can arrive.
	Posted func(messageID int)
	// Notes is what this conversation's language calls a question nobody answered.
	//
	// The words travel on the question rather than being looked up here, because
	// this package cannot see the catalogue: the catalogue calls format.go's markup
	// helpers, so a dependency in the other direction would be a cycle. The zero
	// value is English, which is what every caller that has no language gets.
	//
	// It is not decoration. retireReserve sizes the message against Telegram's
	// 4096-unit budget from these exact strings, so a language whose outcome line
	// is longer than English's reserves more room by construction rather than by a
	// margin somebody guessed.
	Notes OutcomeNotes
}

// OutcomeNotes is the wording appended to a question that ended without a tap.
//
// Dash is separate from the two phrases because the separator is language-dependent
// in its own right: Chinese uses 破折号 and takes no following space, and Arabic
// needs a leading RLM so the dash does not detach from the phrase it introduces when
// the question above it was written in Latin script.
type OutcomeNotes struct {
	Dash      string
	Declined  string
	Withdrawn string
}

// orDefault fills any empty field from the English defaults, per field rather than
// per struct: a caller that translated one line and not the other should get the
// line they translated.
func (n OutcomeNotes) orDefault() OutcomeNotes {
	if n.Dash == "" {
		n.Dash = defaultDash
	}
	if n.Declined == "" {
		n.Declined = defaultDeclined
	}
	if n.Withdrawn == "" {
		n.Withdrawn = defaultWithdrawn
	}
	return n
}

// The English outcome lines. They are the fallback and they are what every golden
// file asserts.
const (
	defaultDash      = "— "
	defaultDeclined  = "no answer, treated as declined"
	defaultWithdrawn = "question withdrawn"
)

// Answer is the member's choice.
type Answer struct {
	ChoiceID string
	UserID   int64
	TimedOut bool
}

// ErrClosed is returned once the transport has been shut down.
var ErrClosed = errors.New("transport: closed")

// Transport is a bidirectional channel to one or more chats.
type Transport interface {
	// Updates returns a channel of inbound messages. It is closed when ctx is done
	// or the transport is closed.
	Updates(ctx context.Context) (<-chan Inbound, error)
	Send(ctx context.Context, o Outbound) error
	// Ask sends a question with buttons and blocks until the allowed user answers or
	// the question times out.
	Ask(ctx context.Context, q Question) (Answer, error)
	// SendTyping shows that a reply is being worked on. It expires on its own after
	// a few seconds and must be repeated to cover a longer wait; KeepTyping does
	// that. A failure is not worth failing a turn over — the member loses an
	// indicator, not an answer — so callers log it and carry on.
	SendTyping(ctx context.Context, chatID int64) error
	Close() error
}

// TypingInterval is how often the indicator is refreshed.
//
// Telegram's typing action lasts five seconds and there is no way to extend or cancel
// one; the only control a caller has is whether it sends another. Four seconds leaves
// a second of slack for the round trip, so the indicator never blinks out mid-wait,
// and it costs one cheap API call every four seconds for as long as a member is
// waiting — which, against a local 27B, is fifteen to twenty seconds a turn.
const TypingInterval = 4 * time.Second

// KeepTyping shows the typing indicator in chat until ctx is done.
//
// It sends the first action immediately, because the whole point is the first second
// of the wait: a member who sees nothing for fifteen seconds has already decided the
// assistant is broken. It returns when ctx is done, so a caller that waits for it has
// a guarantee the indicator has stopped rather than a hope — which is what makes "it
// stops when the reply lands" testable rather than a matter of timing.
//
// Failures are dropped. A transport that cannot show an indicator still has a reply to
// deliver, and a turn that failed because a decoration failed would be a worse product
// than one that quietly goes without it.
func KeepTyping(ctx context.Context, t Transport, chatID int64, every time.Duration) {
	if every <= 0 {
		every = TypingInterval
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		_ = t.SendTyping(ctx, chatID)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
