// The unit-local history ring. History is conversational context, nothing more: it
// lives in memory, is bounded, and is never written to lore — lore holds distilled
// knowledge, not transcripts. A restart legitimately forgets the conversation.

package assistant

import "sync"

// turnRecord is one delivered turn: what the member said and what they were shown.
type turnRecord struct {
	user      string
	assistant string
}

// historyRing keeps the last max turns. Turns are serialised by the Unit's slot, so
// the ring sees one writer at a time; it carries its own lock anyway, so a snapshot
// is consistent no matter what a future caller does.
type historyRing struct {
	mu  sync.Mutex
	buf []turnRecord
	max int
}

func newHistoryRing(max int) *historyRing {
	return &historyRing{max: max}
}

// add records one turn, evicting the oldest when full.
func (h *historyRing) add(user, assistant string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf = append(h.buf, turnRecord{user: user, assistant: assistant})
	if len(h.buf) > h.max {
		h.buf = append(h.buf[:0], h.buf[len(h.buf)-h.max:]...)
	}
}

// snapshot returns the turns oldest first. The slice is a copy: the budget loop
// trims it freely without touching the ring.
func (h *historyRing) snapshot() []turnRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]turnRecord, len(h.buf))
	copy(out, h.buf)
	return out
}
