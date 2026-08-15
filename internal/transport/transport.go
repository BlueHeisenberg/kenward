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
}

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
	Close() error
}
