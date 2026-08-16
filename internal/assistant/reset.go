// The scheduled history reset: dropping a conversation's recent turns on a boundary
// the household chose, and telling the member it happened.
//
// This has nothing to do with memory. lore holds what the household decided to keep,
// and nothing in this file touches it; the ring is the last few turns that ride in the
// prompt, so clearing it costs the thread of a conversation and nothing else. The
// neighbouring setting it must not be confused with is session.idle_timeout, which
// locks a member's *key* after quiet and stops the assistant answering at all. They
// share no code, no configuration key and no default.
//
// Nothing here compresses or summarises the turns it drops. A summary would be a new
// piece of text about the household, written by the model, kept without anyone
// agreeing to it, and read into every later prompt — which is a memory write in all
// but name, arriving down the one path the capture engine exists to keep supervised.
// The household already has a way to keep something from a conversation, and it is
// the remember tool.

package assistant

import "time"

// resetNoticeText is sent when a scheduled reset drops a conversation's recent turns.
//
// It is a message of its own rather than a line prefixed to the reply, unlike the
// retrieval notice. A reset can land on a turn whose only output is a tool call, and a
// notice riding on reply text would then be dropped — leaving the one turn in this
// design where the assistant quietly forgets an hour of conversation. The retrieval
// line was weighed against arriving on every single turn; this one arrives once per
// interval, which is not the same trade.
//
// The second sentence is the load-bearing one. The failure this text exists to prevent
// is a member reading the first and believing something was dropped from their memory.
const resetNoticeText = "Starting fresh — I've cleared the earlier part of this conversation. Nothing in your memory changed; this is the scheduled reset."

// historyBoundary returns the most recent scheduled reset at or before now.
//
// Boundaries are anchored to local midnight rather than to when the process started,
// so a household's reset falls at the same times every day and survives a restart:
// with every set to 6h they are 00:00, 06:00, 12:00 and 18:00, and with 24h it is
// midnight. Anchoring to start-up would instead put the boundary wherever the last
// power cut left it, which is not a schedule anybody can predict — and being able to
// predict what the assistant saw is the rule the whole prompt is assembled under.
//
// every must be positive and no more than 24h. Configuration refuses anything longer,
// because the anchor resets at midnight and a longer interval would silently collapse
// to a daily reset here rather than doing what it says.
func historyBoundary(now time.Time, every time.Duration) time.Time {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	n := int64(now.Sub(midnight) / every)
	return midnight.Add(time.Duration(n) * every)
}

// maybeReset drops the history ring if a boundary has passed since the last reset, and
// reports whether the member has to be told.
//
// It is called on the turn that crosses the boundary rather than fired by a timer, and
// that is the whole scheduling mechanism. A timer would clear a conversation nobody is
// having — which is invisible and therefore pointless — and would also be the only way
// to clear one somebody *is* having between their message and the answer to it. Checked
// here, the turn that crosses a boundary starts clean and says so, and a household
// asleep at 04:00 finds the reset already done at breakfast.
func (u *Unit) maybeReset(now time.Time) bool {
	if u.opts.HistoryReset <= 0 {
		return false
	}
	return u.history.resetIfDue(historyBoundary(now, u.opts.HistoryReset))
}
