package transport

import "errors"

// Errors an implementation may return in addition to ErrClosed.
//
// They describe programming mistakes at the seam — an empty message, a question
// with nothing to tap, a second reader on a single update stream — rather than
// anything the member did. Nothing here is ever shown to a member.
var (
	// ErrUpdatesActive is returned by Updates when a stream is already running.
	//
	// One bot has one update stream: Telegram delivers each update exactly once,
	// so a second reader would silently steal messages from the first. Fan-out to
	// several consumers is the Mux's job, not the transport's.
	ErrUpdatesActive = errors.New("transport: updates already started")

	// ErrEmptyText is returned when a message or question carries no text.
	// Telegram rejects empty messages; catching it here keeps the error local.
	ErrEmptyText = errors.New("transport: empty text")

	// ErrNoChoices is returned by Ask when a Question has no Choices. A question
	// with no buttons can never be answered, only time out.
	ErrNoChoices = errors.New("transport: question has no choices")

	// ErrTextTooLong is returned by Ask when a question does not fit in a single
	// Telegram message. Questions are not split: the buttons belong to one message,
	// and a question the member has to scroll to read is a question badly asked.
	ErrTextTooLong = errors.New("transport: question text too long for one message")
)
