package remind

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Tick is how often the clock looks for work. Reminders are minute-granular, so this
// is not a knob: it is short enough that a reminder is never noticeably late and long
// enough that it costs nothing.
const Tick = 30 * time.Second

// lateAfter is how overdue an occurrence must be before the member is told it was
// late. A few minutes is not worth a sentence; two hours is.
const lateAfter = 15 * time.Minute

// Fire is one reminder claimed for delivery.
type Fire struct {
	Reminder Reminder
	// Due is the occurrence being delivered, which is not the same as now when the
	// node has been off.
	Due time.Time
	// Late says the occurrence is old enough that the member should be told.
	Late bool
}

// Message is what the member receives. It is product surface and golden-tested.
//
// The late note states when the occurrence was due and nothing about why it was late.
// The clock does not know: the node may have been off, the machine asleep, or the
// household's daily cap in the way. A note that guessed would be wrong some of the
// time, and a reminder is a bad place to be caught guessing.
func (f Fire) Message(loc *time.Location) string {
	if !f.Late {
		return f.Reminder.Text
	}
	return f.Reminder.Text + "\n\n" +
		fmt.Sprintf("(This was due at %s.)", f.Due.In(loc).Format("15:04 on Monday 2 January"))
}

// Skipped counts what a pass declined to send, for the log and for `kenward doctor`.
type Skipped struct {
	// Missed is repeating occurrences dropped for being older than CatchUp.
	Missed int
	// Capped is deliveries held back by the day's cap.
	Capped int
}

// Any reports whether anything was skipped.
func (s Skipped) Any() bool { return s.Missed > 0 || s.Capped > 0 }

// Due claims every reminder that should be delivered now, applies the
// missed-occurrence policy and the daily cap, persists the result, and returns what to
// send.
//
// # Missed occurrences
//
// The household's machines are usually asleep — that is a premise of the whole
// product, not an edge case — so a schedule that fires against a powered-off node is
// the normal case rather than the exception. There are two right answers and this
// method gives a different one to each kind, because "remind me at 8" and "every
// morning" are not the same promise:
//
//   - A one-off is a promise to a person. It is delivered however late it is. There
//     is exactly one of it, ever, so it cannot storm, and dropping it silently is the
//     one outcome that leaves a member trusting a reminder that never came.
//   - A repeating occurrence is a routine, and a stale one is worthless. However many
//     were missed, at most one is sent, and the one on offer is the *most recent*
//     occurrence rather than the oldest missed one — a node off for a fortnight should
//     deliver this morning's bin reminder, not the one from a fortnight ago, and
//     should not skip it on the grounds that an occurrence it never sent is old. That
//     one is delivered only if it is younger than Options.CatchUp. A node that was off
//     overnight therefore owes its household one bin reminder, not sixteen.
//
// # The cap
//
// Nothing here has been asked for, which is what makes it dangerous: a household that
// finds the assistant chatty mutes it, and a muted assistant is a dead one. So one
// unit may send Options.MaxPerDay unprompted messages a day, counted in the store's
// own file so the allowance survives a restart — a crash-looping unit that reset its
// count on every boot would spend it forever. The cap treats the two kinds
// differently for the same reason the missed-occurrence policy does: a capped
// repeating occurrence is skipped and advanced, and a capped one-off is left where it
// is, so it is delivered tomorrow instead of being lost.
//
// The member is not told the cap was reached. Saying so would itself be an unprompted
// message, sent for the express purpose of reporting that too many unprompted messages
// had been sent. It goes to the log and to `kenward doctor`, which is where the
// operator who can change the number is looking.
//
// # Claiming
//
// A reminder is claimed — advanced or deleted, and counted against the cap — and
// persisted before it is handed back for sending. A crash in the gap therefore loses
// the message rather than repeating it. That is the deliberate direction: the cap has
// to be spent before the send, because a send that fails in a loop would otherwise
// become exactly the flood the cap exists to prevent.
func (s *Store) Due(now time.Time) ([]Fire, Skipped) {
	s.mu.Lock()
	defer s.mu.Unlock()

	loc := s.opts.Location
	// Roll the ledger if the day turned over in the household's own timezone.
	if day := s.day(now); s.data.SentDay != day {
		s.data.SentDay = day
		s.data.SentCount = 0
	}

	var fires []Fire
	var skip Skipped
	changed := false
	kept := s.data.Reminders[:0]

	for _, r := range s.data.Reminders {
		if r.Next.After(now) {
			kept = append(kept, r)
			continue
		}
		once := r.Every == EveryOnce

		// For a repeating reminder the occurrence on offer is the most recent one,
		// not the oldest missed one, and everything between them is discarded. A
		// one-off has exactly one occurrence and it is the one stored.
		due := r.Next
		next := r.Next
		if !once {
			next, due = r.advance(now, loc)
		}
		late := now.Sub(due)

		// A repeating occurrence older than the catch-up window is a stale routine
		// and is not worth delivering. Advance past it and say nothing.
		if !once && late > s.opts.CatchUp {
			r.Next = next
			skip.Missed++
			kept = append(kept, r)
			changed = true
			continue
		}

		if s.data.SentCount >= s.opts.MaxPerDay {
			skip.Capped++
			if once {
				// Held, not dropped: Next stays in the past, so the first pass
				// after midnight delivers it.
				kept = append(kept, r)
				continue
			}
			r.Next = next
			kept = append(kept, r)
			changed = true
			continue
		}

		fires = append(fires, Fire{Reminder: r, Due: due, Late: late > lateAfter})
		s.data.SentCount++
		changed = true
		if once {
			continue // dropped from kept: a one-off is done
		}
		r.Next = next
		kept = append(kept, r)
	}

	s.data.Reminders = kept
	if !changed {
		return nil, skip
	}
	s.sort()
	if err := s.save(); err != nil {
		// The claim could not be persisted, so it has not happened. Returning the
		// fires anyway would send messages the store has no record of sending, and
		// the next pass would send them again — the one failure mode worse than a
		// missed reminder.
		return nil, skip
	}
	return fires, skip
}

// Sender delivers one message to one chat. It is the unit's own transport view, so a
// reminder can only ever reach the chat the unit already serves.
type Sender func(ctx context.Context, chatID int64, text string) error

// Clock delivers a store's reminders as they come due.
//
// Its dependencies are the whole argument for this design: a store, a sender, a clock
// and a log. There is no Router and no Memory here, so a reminder cannot reach a
// model, cannot spend a token, and cannot widen a tier chain — not by configuration,
// but because there is nothing in this struct to do it with.
type Clock struct {
	store  *Store
	send   Sender
	now    func() time.Time
	logger *slog.Logger
}

// NewClock wires a clock over one unit's store. now defaults to time.Now and logger to
// a discarding one.
func NewClock(store *Store, send Sender, now func() time.Time, logger *slog.Logger) *Clock {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Clock{store: store, send: send, now: now, logger: logger}
}

// Run delivers due reminders until ctx is cancelled. It runs one pass immediately, so
// a node that has just booted after being off overnight settles its debts at startup
// rather than up to a tick later.
func (c *Clock) Run(ctx context.Context) {
	t := time.NewTicker(Tick)
	defer t.Stop()
	c.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.pass(ctx)
		}
	}
}

// pass claims and delivers one round.
func (c *Clock) pass(ctx context.Context) {
	fires, skip := c.store.Due(c.now())
	if skip.Any() {
		// One line per pass, not one per reminder: a capped one-off is skipped on
		// every pass until the day turns over, and a log that said so every thirty
		// seconds would be the noise an operator learns to filter out.
		c.logger.Warn("remind: skipped due reminders",
			"missed", skip.Missed, "capped", skip.Capped)
	}
	for _, f := range fires {
		if ctx.Err() != nil {
			return
		}
		if err := c.send(ctx, f.Reminder.ChatID, f.Message(c.store.Location())); err != nil {
			// The reminder is already claimed, so this one is lost. It is logged at
			// error precisely because nothing else will notice: no member is waiting
			// on a reply, and the only sign anything went wrong is this line.
			c.logger.Error("remind: could not deliver a reminder",
				"id", f.Reminder.ID, "error", err)
		}
	}
}
