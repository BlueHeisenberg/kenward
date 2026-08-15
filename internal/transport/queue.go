package transport

import "sync"

// defaultQueueCap bounds the inbound backlog held for a consumer that is not
// reading. It is generous by household standards: a family does not send a
// thousand messages while one question is outstanding.
const defaultQueueCap = 1024

// queue is an ordered handoff between a producer that must never block and a
// consumer that may be slow.
//
// It exists because of one hard constraint: a member's unit blocks inside Ask
// waiting for a callback query, and that callback arrives on the same update
// stream as ordinary messages. If a message could block the goroutine reading
// from Telegram, a question asked while another message is in flight would never
// see its answer — every capture confirmation would deadlock until it timed out.
// So the producer side pushes and returns, always.
//
// The backlog is bounded rather than unbounded: past cap the oldest message is
// dropped and counted, which is visible in a log line and in dropped(), instead
// of growing until the process dies.
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []Inbound
	cap    int
	closed bool
	drops  int
}

// newQueue returns an empty queue holding at most cap messages.
func newQueue(cap int) *queue {
	if cap <= 0 {
		cap = defaultQueueCap
	}
	q := &queue{cap: cap}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push appends a message. It never blocks. It reports whether an older message
// had to be dropped to make room; a push to a closed queue is discarded and
// reported as a drop.
func (q *queue) push(in Inbound) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		q.drops++
		return true
	}

	var lost bool
	if len(q.items) >= q.cap {
		q.items = q.items[1:]
		q.drops++
		lost = true
	}
	q.items = append(q.items, in)
	q.cond.Signal()
	return lost
}

// pop returns the oldest message, blocking until one arrives. It reports false
// once the queue is closed and drained, which is the signal for a consumer to
// stop.
func (q *queue) pop() (Inbound, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return Inbound{}, false
	}
	in := q.items[0]
	q.items = q.items[1:]
	return in, true
}

// close stops the queue. Messages already queued are still delivered; every
// blocked pop wakes. It is idempotent.
func (q *queue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

// dropped returns how many messages were discarded for want of room.
func (q *queue) dropped() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.drops
}
