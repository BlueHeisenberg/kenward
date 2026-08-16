package transport

import (
	"context"
	"sync"
)

// Mux fans one bot's update stream out to per-scope Transport views.
//
// It exists for simple mode, where the whole household shares a single bot
// token: Telegram delivers each update exactly once, so the one stream has to be
// split before the per-member units can each read their own. In isolated mode
// every pod owns its own bot and there is nothing to fan out — the Mux is not
// used, and no code below it can tell the difference.
//
// A view is a Transport: its Updates carries only the messages its match function
// accepts, and its Send and Ask go straight to the underlying transport. Routing
// inbound is the Mux's whole job; it has no opinion about outbound.
//
// The Mux does not own the underlying transport. Closing the Mux stops dispatch
// and closes the views; closing the bot is the job of whoever opened it.
type Mux struct {
	t        Transport
	queueCap int

	mu      sync.Mutex
	views   []*muxView
	started bool
	closed  bool
	dropped int

	done chan struct{}
	wg   sync.WaitGroup
}

// NewMux returns a Mux over t. Nothing is read from t until Start is called.
func NewMux(t Transport) *Mux {
	return &Mux{
		t:        t,
		queueCap: defaultQueueCap,
		done:     make(chan struct{}),
	}
}

// View returns a Transport that sees only the inbound messages match accepts.
//
// Matches are expected to be disjoint — one scope per member, one for the group —
// and a message is handed to the first view that accepts it, so an overlapping
// match cannot cause the same message to be handled twice.
//
// Views may be created before or after Start; one created later starts receiving
// from that moment. A nil match accepts everything.
func (m *Mux) View(match func(Inbound) bool) Transport {
	if match == nil {
		match = func(Inbound) bool { return true }
	}
	v := &muxView{
		mux:   m,
		match: match,
		queue: newQueue(m.queueCap),
		done:  make(chan struct{}),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		_ = v.Close()
		return v
	}
	m.views = append(m.views, v)
	return v
}

// Start begins reading the underlying transport and dispatching to views. It
// returns whatever the underlying Updates returns; it may be called once.
func (m *Mux) Start(ctx context.Context) error {
	// The lock is held across the underlying Updates call and the dispatcher's
	// registration so that Close can never land in between: it either turns
	// Start away before the stream exists, or waits for the dispatcher it saw
	// registered. Released in the middle, a winning Close would strand a started
	// stream that nobody consumes and a started flag that nobody can retry.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.started {
		return ErrUpdatesActive
	}

	up, err := m.t.Updates(ctx)
	if err != nil {
		return err
	}
	m.started = true
	m.wg.Add(1)
	go m.dispatch(ctx, up)
	return nil
}

// Close stops dispatch and closes every view. It is idempotent and returns only
// once the dispatch goroutine and every view pump have stopped. The underlying
// transport is left open.
func (m *Mux) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.done)
	m.mu.Unlock()

	m.closeViews()
	m.wg.Wait()
	return nil
}

// Dropped reports how many inbound messages were lost: either no view matched
// them, or the matching view's backlog overflowed and its oldest message was
// discarded. A number climbing here means a chat nobody is scoped to or a
// consumer that has stopped reading — worth surfacing in doctor output, not
// worth an error.
func (m *Mux) Dropped() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped
}

func (m *Mux) dispatch(ctx context.Context, up <-chan Inbound) {
	defer m.wg.Done()
	defer m.closeViews()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		case in, ok := <-up:
			if !ok {
				return
			}
			m.route(in)
		}
	}
}

func (m *Mux) route(in Inbound) {
	m.mu.Lock()
	views := make([]*muxView, len(m.views))
	copy(views, m.views)
	m.mu.Unlock()

	for _, v := range views {
		if v.accepts(in) {
			if v.queue.push(in) {
				// The view's backlog overflowed and its oldest message is gone.
				// Counted, because a silent drop is invisible to doctor output.
				m.mu.Lock()
				m.dropped++
				m.mu.Unlock()
			}
			return
		}
	}

	m.mu.Lock()
	m.dropped++
	m.mu.Unlock()
}

func (m *Mux) closeViews() {
	m.mu.Lock()
	views := make([]*muxView, len(m.views))
	copy(views, m.views)
	m.mu.Unlock()

	for _, v := range views {
		v.Close()
	}
}

// muxView is one scope's window onto the shared bot.
type muxView struct {
	mux   *Mux
	match func(Inbound) bool
	queue *queue

	mu      sync.Mutex
	started bool
	closed  bool
	done    chan struct{}
}

func (v *muxView) accepts(in Inbound) bool {
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return false
	}
	return v.match(in)
}

// Updates returns this view's slice of the stream. The channel closes when ctx is
// done, the view is closed, or the Mux stops dispatching. It may be called once.
func (v *muxView) Updates(ctx context.Context) (<-chan Inbound, error) {
	// The Mux lock is held across the pump's registration so that a concurrent
	// Close either sees the pump or is seen by it, and never waits on a goroutine
	// that starts after it stopped looking.
	v.mux.mu.Lock()
	defer v.mux.mu.Unlock()
	if v.mux.closed {
		return nil, ErrClosed
	}

	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil, ErrClosed
	}
	if v.started {
		v.mu.Unlock()
		return nil, ErrUpdatesActive
	}
	v.started = true
	v.mu.Unlock()

	out := make(chan Inbound, defaultUpdateBuffer)
	v.mux.wg.Add(1)
	go func() {
		defer v.mux.wg.Done()
		defer close(out)
		for {
			in, ok := v.queue.pop()
			if !ok {
				return
			}
			select {
			case out <- in:
			case <-ctx.Done():
				return
			case <-v.done:
				return
			case <-v.mux.done:
				return
			}
		}
	}()
	return out, nil
}

// Send passes straight through to the shared bot.
func (v *muxView) Send(ctx context.Context, o Outbound) error {
	if err := v.state(); err != nil {
		return err
	}
	return v.mux.t.Send(ctx, o)
}

// Ask passes straight through to the shared bot. The AllowedUserID filter lives
// in the transport, not here: a view is a routing convenience, never an
// authorization boundary.
func (v *muxView) Ask(ctx context.Context, q Question) (Answer, error) {
	if err := v.state(); err != nil {
		return Answer{}, err
	}
	return v.mux.t.Ask(ctx, q)
}

// SendTyping passes straight through to the shared bot. The indicator belongs to a
// chat, and a view is a slice of one bot's stream rather than a separate connection,
// so there is nothing here to route it by.
func (v *muxView) SendTyping(ctx context.Context, chatID int64) error {
	if err := v.state(); err != nil {
		return err
	}
	return v.mux.t.SendTyping(ctx, chatID)
}

// RetireKeyboard passes straight through to the shared bot, if it can do it at all.
//
// A transport that cannot is not an error: retiring a keyboard an earlier process
// left behind is tidying up, and a caller must not fail an onboarding over it.
func (v *muxView) RetireKeyboard(ctx context.Context, chatID int64, messageID int) error {
	if err := v.state(); err != nil {
		return err
	}
	r, ok := v.mux.t.(interface {
		RetireKeyboard(context.Context, int64, int) error
	})
	if !ok {
		return nil
	}
	return r.RetireKeyboard(ctx, chatID, messageID)
}

// Close detaches this view. It never closes the shared bot, so one member's unit
// shutting down cannot take the household off the air. It is idempotent.
func (v *muxView) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closeLocked()
	return nil
}

func (v *muxView) closeLocked() {
	if v.closed {
		return
	}
	v.closed = true
	close(v.done)
	v.queue.close()
}

func (v *muxView) state() error {
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return v.mux.state()
}

func (m *Mux) state() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	return nil
}

// muxView implements Transport.
var _ Transport = (*muxView)(nil)
