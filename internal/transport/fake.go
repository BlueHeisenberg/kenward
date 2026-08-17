package transport

import (
	"context"
	"sync"
	"time"
)

// Fake is an in-memory Transport for tests in other packages.
//
// It is the seam that keeps capture, enrol and assistant tests off the network:
// script what the member says, read back what was sent, and decide in advance how
// they answer a question.
//
// A typical capture test reads:
//
//	f := transport.NewFake()
//	f.AnswerWithChoice("personal")
//	// ... run the turn ...
//	if got := f.Sent(); len(got) != 1 { ... }
//
// The default, if nothing is scripted, is that questions time out — a decline.
// That is deliberate: a test that forgets to say how the member answered must not
// accidentally assert that a memory was written.
//
// Fake is safe for concurrent use. Every accessor returns a copy.
type Fake struct {
	mu       sync.Mutex
	closed   bool
	closedCh chan struct{}
	started  bool

	queue *queue

	sent    []Outbound
	asked   []Question
	ignored int
	// retired is every keyboard RetireKeyboard was asked to strip, as chat and
	// message id. A test asserting that a restart cleaned up after itself reads it.
	retired []Retired
	// edits is every message EditText was asked to rewrite. A test asserting that a
	// message stopped saying something false reads it.
	edits []Edit
	// typing counts the indicators sent, per chat.
	typing map[int64]int

	scripted  []Answer
	answerFn  func(Question) Answer
	sendErr   error
	askErr    error
	editErr   error
	askDelay  time.Duration
	updateBuf int
}

// NewFake returns an empty Fake. Nothing blocks until it is used.
func NewFake() *Fake {
	return &Fake{
		closedCh:  make(chan struct{}),
		queue:     newQueue(defaultQueueCap),
		updateBuf: defaultUpdateBuffer,
		typing:    map[int64]int{},
	}
}

// --- scripting inbound -----------------------------------------------------

// Inject queues an inbound message. Messages injected before Updates is called
// are held and delivered in order once it is.
func (f *Fake) Inject(in ...Inbound) {
	for _, m := range in {
		if m.At.IsZero() {
			m.At = time.Now().UTC()
		}
		f.queue.push(m)
	}
}

// InjectText is Inject for the common case: one text message from one member.
// Set group to true to have it look like it came from the household chat.
//
// The message is Addressed: a caller reaching for the one-line helper means the
// member spoke to the assistant, and in the household group only an addressed
// message is answered at all. An overheard one — two members talking to each other
// — is a different thing to say, so it is said with Inject and the field left off.
func (f *Fake) InjectText(chatID, userID int64, text string, group bool) {
	f.Inject(Inbound{
		ChatID:    chatID,
		UserID:    userID,
		Text:      text,
		IsGroup:   group,
		Addressed: true,
		At:        time.Now().UTC(),
	})
}

// --- scripting answers -----------------------------------------------------

// AnswerWithChoice makes every question answered with the given choice id, by the
// member the question was addressed to.
func (f *Fake) AnswerWithChoice(choiceID string) {
	f.SetAnswerFunc(func(q Question) Answer {
		return Answer{ChoiceID: choiceID, UserID: q.AllowedUserID}
	})
}

// AnswerWithTimeout makes every question time out, which callers must treat as a
// decline. This is the default.
func (f *Fake) AnswerWithTimeout() {
	f.SetAnswerFunc(func(Question) Answer { return Answer{TimedOut: true} })
}

// AnswerFromUser models somebody else tapping the buttons.
//
// If userID is not the question's AllowedUserID the tap is ignored exactly as the
// real transport ignores it — no answer, no acknowledgement — and the question
// goes on to time out. This is the case worth testing: in a group chat everyone
// can see the keyboard, and one member must not be able to route another's
// memory. IgnoredTaps counts how often it happened.
func (f *Fake) AnswerFromUser(userID int64, choiceID string) {
	f.SetAnswerFunc(func(Question) Answer {
		return Answer{ChoiceID: choiceID, UserID: userID}
	})
}

// QueueAnswers scripts answers for the next questions in order. Once the queue is
// exhausted the answer function takes over again. A queued Answer whose UserID is
// zero is taken to be the addressed member.
func (f *Fake) QueueAnswers(answers ...Answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripted = append(f.scripted, answers...)
}

// SetAnswerFunc installs a function that decides each answer. It is called
// outside the Fake's lock, so it may inspect the question freely.
func (f *Fake) SetAnswerFunc(fn func(Question) Answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answerFn = fn
}

// SetAskDelay makes Ask block for d before answering, so a test can cancel its
// context mid-question.
func (f *Fake) SetAskDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.askDelay = d
}

// SetSendError makes every subsequent Send fail with err. Pass nil to clear.
func (f *Fake) SetSendError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

// SetAskError makes every subsequent Ask fail with err. Pass nil to clear.
func (f *Fake) SetAskError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.askErr = err
}

// SetEditError makes every subsequent EditText fail with err. Pass nil to clear.
//
// It is how a test reaches the case Telegram reaches on its own: a message too old to
// edit, a node that restarted since, a network that dropped the call. A caller
// correcting something it already said has to have somewhere else to say it.
func (f *Fake) SetEditError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editErr = err
}

// --- captured outbound -----------------------------------------------------

// Sent returns every message sent so far, in order.
func (f *Fake) Sent() []Outbound {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Outbound, len(f.sent))
	copy(out, f.sent)
	return out
}

// LastSent returns the most recent message, reporting false if nothing was sent.
func (f *Fake) LastSent() (Outbound, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return Outbound{}, false
	}
	return f.sent[len(f.sent)-1], true
}

// Asked returns every question asked so far, in order.
func (f *Fake) Asked() []Question {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Question, len(f.asked))
	copy(out, f.asked)
	return out
}

// LastAsked returns the most recent question, reporting false if none was asked.
func (f *Fake) LastAsked() (Question, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.asked) == 0 {
		return Question{}, false
	}
	return f.asked[len(f.asked)-1], true
}

// IgnoredTaps counts answers discarded because they came from somebody other than
// the addressed member.
func (f *Fake) IgnoredTaps() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ignored
}

// Reset clears captured traffic. Scripted answers and errors are left alone.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = nil
	f.asked = nil
	f.ignored = 0
	f.retired = nil
	f.edits = nil
	f.typing = map[int64]int{}
}

// --- Transport -------------------------------------------------------------

// Updates returns the injected messages. The channel closes when ctx is done or
// the Fake is closed. It may be called once.
func (f *Fake) Updates(ctx context.Context) (<-chan Inbound, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, ErrClosed
	}
	if f.started {
		f.mu.Unlock()
		return nil, ErrUpdatesActive
	}
	f.started = true
	buf := f.updateBuf
	f.mu.Unlock()

	out := make(chan Inbound, buf)
	go func() {
		defer close(out)
		for {
			in, ok := f.queue.pop()
			if !ok {
				return
			}
			select {
			case out <- in:
			case <-ctx.Done():
				return
			case <-f.closedCh:
				return
			}
		}
	}()
	return out, nil
}

// Send records the message.
func (f *Fake) Send(ctx context.Context, o Outbound) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrClosed
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, o)
	return nil
}

// Ask records the question and answers it from the script.
//
// A scripted answer from the wrong user is dropped and the question times out,
// mirroring the real transport, where a tap from anyone but AllowedUserID is
// ignored outright.
func (f *Fake) Ask(ctx context.Context, q Question) (Answer, error) {
	if err := ctx.Err(); err != nil {
		return Answer{}, err
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return Answer{}, ErrClosed
	}
	if f.askErr != nil {
		err := f.askErr
		f.mu.Unlock()
		return Answer{}, err
	}
	f.asked = append(f.asked, q)
	if q.Posted != nil {
		// Message ids are one-based and in the order the questions were asked, which
		// is enough for a test to tell one question's keyboard from another's.
		posted, n := q.Posted, len(f.asked)
		f.mu.Unlock()
		posted(n)
		f.mu.Lock()
	}

	var ans Answer
	switch {
	case len(f.scripted) > 0:
		ans = f.scripted[0]
		f.scripted = f.scripted[1:]
		if ans.UserID == 0 && !ans.TimedOut {
			ans.UserID = q.AllowedUserID
		}
	case f.answerFn != nil:
		fn := f.answerFn
		f.mu.Unlock()
		ans = fn(q)
		f.mu.Lock()
	default:
		ans = Answer{TimedOut: true}
	}

	if !ans.TimedOut && ans.UserID != q.AllowedUserID {
		f.ignored++
		ans = Answer{TimedOut: true}
	}
	delay := f.askDelay
	f.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		case <-f.closedCh:
			return Answer{}, ErrClosed
		}
	}
	return ans, nil
}

// SendTyping records the indicator. It is counted rather than stored as a list of
// one repeated value, because what a test asserts about it is that it happened, that
// it happened in the right chat, and that it stopped.
func (f *Fake) SendTyping(ctx context.Context, chatID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrClosed
	}
	f.typing[chatID]++
	return nil
}

// TypingCount returns how many typing indicators were sent to one chat.
func (f *Fake) TypingCount(chatID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.typing[chatID]
}

// Retired is one keyboard RetireKeyboard was asked to strip.
type Retired struct {
	ChatID    int64
	MessageID int
}

// RetireKeyboard records the request. See Telegram.RetireKeyboard for what it does
// against the real thing.
func (f *Fake) RetireKeyboard(ctx context.Context, chatID int64, messageID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrClosed
	}
	f.retired = append(f.retired, Retired{ChatID: chatID, MessageID: messageID})
	return nil
}

// RetiredKeyboards returns every keyboard this Fake was asked to strip, in order.
func (f *Fake) RetiredKeyboards() []Retired {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Retired(nil), f.retired...)
}

// Edit is one message EditText was asked to rewrite, with the words it was rewritten
// to. The text is kept because the point of an edit is what the message now says.
type Edit struct {
	ChatID    int64
	MessageID int
	Text      string
}

// EditText records the rewrite. See Telegram.EditText for what it does against the
// real thing.
func (f *Fake) EditText(ctx context.Context, chatID int64, messageID int, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrClosed
	}
	if f.editErr != nil {
		return f.editErr
	}
	f.edits = append(f.edits, Edit{ChatID: chatID, MessageID: messageID, Text: text})
	return nil
}

// Edits returns every message this Fake was asked to rewrite, in order.
func (f *Fake) Edits() []Edit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Edit(nil), f.edits...)
}

// Close releases the Fake. It is idempotent; afterwards every call returns
// ErrClosed and the Updates channel closes.
func (f *Fake) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	close(f.closedCh)
	f.mu.Unlock()

	f.queue.close()
	return nil
}

// Fake implements Transport.
var _ Transport = (*Fake)(nil)
