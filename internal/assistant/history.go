// The unit-local history ring. History is conversational context, nothing more: it
// lives in memory, is bounded, and is never written to lore — lore holds distilled
// knowledge, not transcripts. A restart legitimately forgets the conversation.

package assistant

import (
	"sync"
	"time"
)

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
	// since is the scheduled boundary this ring has already been reset to. See
	// reset.go. The zero value is before every boundary, so the first turn after a
	// restart adopts the current one without clearing anything and without telling
	// anybody — a restart has already forgotten the conversation, and announcing the
	// reset of an empty ring would be a notice about an event that did not happen.
	since time.Time
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

// resetIfDue drops every turn when boundary is later than the last one this ring was
// reset to, and reports whether anything was actually dropped.
//
// The boundary is adopted either way, and the two halves of that are deliberate. A
// ring that was empty when a boundary passed has nothing to forget, so nothing is
// said; but the boundary is still spent, so a member who says nothing for three days
// is told once, on the turn that crosses the next boundary, rather than being told
// three times about resets of nothing.
func (h *historyRing) resetIfDue(boundary time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.since.Before(boundary) {
		return false
	}
	h.since = boundary
	if len(h.buf) == 0 {
		return false
	}
	h.buf = h.buf[:0]
	return true
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
