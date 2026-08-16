// Package remind gives one unit a clock.
//
// It is the whole of kenward's proactive capability, and it is deliberately the
// smallest thing that could be one: a reminder is a piece of text and a time to send
// it. When the time comes the text is sent, verbatim. Nothing is generated, nothing is
// retrieved, and no model is consulted.
//
// That is not a simplification of a richer design, it is the design. A scheduled job
// that invokes the model is a job that spends tokens with nobody asking, all night,
// against whatever tier its chain reaches — and the household's guarantee that a
// local-only chain "never reaches a provider" (internal/privacy) would then depend on
// a timer's configuration rather than on the shape of the code. So this package is
// given no Router and no Memory. It cannot reach a provider because it has nothing to
// reach one with, and that is checkable by reading its imports.
//
// A Store belongs to exactly one unit and lives in one file of its own. Nothing here
// is keyed by member: a member's reminders are in their unit's store, the household's
// are in the group unit's, and in isolated mode a member's pod holds the only copy of
// theirs. The same code runs as a goroutine beside its siblings and alone in a pod,
// which is the property the whole product rests on.
package remind

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Every says how often a reminder repeats.
type Every int

const (
	// EveryOnce fires a single time and is then gone. Zero value.
	EveryOnce Every = iota
	// EveryDaily fires at the same wall-clock time every day.
	EveryDaily
	// EveryWeekly fires at the same wall-clock time on one weekday.
	EveryWeekly
)

func (e Every) String() string {
	switch e {
	case EveryDaily:
		return "daily"
	case EveryWeekly:
		return "weekly"
	default:
		return "once"
	}
}

// ParseEvery reads the tool's `every` argument. An unrecognised value is an error
// rather than a default: guessing between "once" and "daily" is the difference between
// one message and one every morning forever.
func ParseEvery(s string) (Every, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "once":
		return EveryOnce, nil
	case "daily":
		return EveryDaily, nil
	case "weekly":
		return EveryWeekly, nil
	default:
		return EveryOnce, fmt.Errorf("remind: unknown repeat %q (want once, daily or weekly)", s)
	}
}

// Reminder is one scheduled message.
//
// It carries the chat it must be delivered to rather than deriving one at fire time.
// A Scope is built from an inbound message and there is no inbound message when a
// timer fires, so the chat id is captured from the scope that created the reminder and
// stored with it. That also makes the record self-contained across a restart, which is
// the only way a reminder survives one.
type Reminder struct {
	// ID is short and member-visible: it is how they cancel one.
	ID string `json:"id"`
	// Text is sent verbatim when the reminder fires. It is the message, not a
	// prompt — nothing expands it and nothing answers it.
	Text string `json:"text"`
	// Every is how often it repeats.
	Every Every `json:"every"`
	// Hour and Minute are the wall-clock time in the store's location. They are the
	// authority for recomputing Next, so a repeating reminder keeps its 07:30 across
	// a daylight-saving change instead of drifting to 06:30.
	Hour   int `json:"hour"`
	Minute int `json:"minute"`
	// Weekday applies to EveryWeekly only.
	Weekday time.Weekday `json:"weekday"`
	// Next is when it is next due, in UTC. It is what the clock compares against.
	Next time.Time `json:"next"`
	// ChatID is where it is delivered.
	ChatID int64 `json:"chat_id"`
	// Created is when the member asked for it.
	Created time.Time `json:"created"`
}

// Every reminder's Every value decides its missed-occurrence policy, and the two
// answers are deliberately different — see Clock.due.

// maxSteps bounds the walk forward to the next future occurrence. A daily reminder
// left behind by a node that was off for a year needs 365 steps; anything past this
// bound means a corrupt record, and spinning on one is worse than dropping it.
const maxSteps = 4000

// step advances one period in wall-clock terms.
//
// AddDate re-derives the date from the wall clock in the same location, so a 07:30
// reminder stays at 07:30 across a daylight-saving transition rather than sliding an
// hour. A wall-clock time that does not exist on a spring-forward day — 02:30 where
// the clocks jump from 02:00 to 03:00 — normalises forward, which is the standard
// behaviour and the only one that fires at all.
func (r Reminder) step(t time.Time, loc *time.Location) time.Time {
	if r.Every == EveryWeekly {
		return t.In(loc).AddDate(0, 0, 7)
	}
	return t.In(loc).AddDate(0, 0, 1)
}

// advance walks Next forward past now and returns both the new next occurrence and the
// most recent occurrence at or before now.
//
// Walking rather than firing per occurrence is the coalescing rule: a node that was off
// for eight hours owes its household one message, not thirty-two.
//
// Returning `last` is what makes the catch-up window mean the right thing. Judging
// lateness against the *stored* Next measures from the oldest occurrence that was
// missed, so a daily reminder that a fortnight's outage left behind would look a
// fortnight late even when this morning's is minutes old — and would be skipped on the
// very morning somebody wanted it. The question worth asking is whether the most recent
// occurrence is still worth delivering, and that is what `last` answers.
func (r Reminder) advance(now time.Time, loc *time.Location) (next, last time.Time) {
	next, last = r.Next, r.Next
	for n := 0; n < maxSteps && !next.After(now); n++ {
		last = next
		next = r.step(next, loc)
	}
	return next.UTC(), last.UTC()
}

// ErrPast is returned when a one-off reminder names a time that has already gone. It
// is a refusal rather than an immediate fire: a member who asked for Thursday and is
// pinged instantly has been told their assistant misread them, which is more useful
// than a reminder they did not want now.
var ErrPast = errors.New("remind: that time has already passed")

// ErrFull is returned when the store already holds Options.MaxStored reminders.
var ErrFull = errors.New("remind: too many reminders already set")

// ErrNoSuchReminder is returned by Cancel for an id the store does not hold.
var ErrNoSuchReminder = errors.New("remind: no reminder with that code")

// New computes a reminder's first occurrence.
//
// date is optional and applies to EveryOnce: "2006-01-02". Without one, a one-off
// behaves like a daily reminder's first fire — the next time that clock reading comes
// round — which is what "remind me at 8" means when nobody said which day.
func New(text string, every Every, hour, minute int, weekday time.Weekday, date string, chatID int64, now time.Time, loc *time.Location) (Reminder, error) {
	if strings.TrimSpace(text) == "" {
		return Reminder{}, errors.New("remind: a reminder needs something to say")
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return Reminder{}, fmt.Errorf("remind: %02d:%02d is not a time of day", hour, minute)
	}
	r := Reminder{
		Text:    oneLine(text),
		Every:   every,
		Hour:    hour,
		Minute:  minute,
		Weekday: weekday,
		ChatID:  chatID,
		Created: now.UTC(),
	}

	w := now.In(loc)
	switch {
	case every == EveryOnce && date != "":
		d, err := time.ParseInLocation("2006-01-02", date, loc)
		if err != nil {
			return Reminder{}, fmt.Errorf("remind: %q is not a date (want YYYY-MM-DD)", date)
		}
		next := time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, loc)
		if !next.After(now) {
			return Reminder{}, ErrPast
		}
		r.Next = next.UTC()
	case every == EveryWeekly:
		next := time.Date(w.Year(), w.Month(), w.Day(), hour, minute, 0, 0, loc)
		// Walk to the wanted weekday, then past now if today's has already gone.
		for next.Weekday() != weekday {
			next = next.AddDate(0, 0, 1)
		}
		if !next.After(now) {
			next = next.AddDate(0, 0, 7)
		}
		r.Next = next.UTC()
	default:
		next := time.Date(w.Year(), w.Month(), w.Day(), hour, minute, 0, 0, loc)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		r.Next = next.UTC()
	}
	return r, nil
}

// When renders a reminder's schedule in English, and it stays English.
//
// It serves three audiences from one function and only one of them is a member
// reading their own language:
//
//	assistant/prompt.go  the model's system prompt      English, pinned
//	cmd/kenward/doctor   the operator's CLI             English
//	the member, over Telegram                           lang.Catalogue.When
//
// The prompt is the one that matters. docs/PROMPT.md is checked against the rendered
// prompt verbatim, and the model is told the member's language by the persona rather
// than by having its instructions translated — a translated prompt changes what the
// model is asked to do, not what a member is told. So the member's reading of a
// schedule is a separate function in internal/lang, with the weekday and month tables
// Go's time package does not have, and this one is asserted English by
// TestReminderWhenStaysEnglishWhateverTheMemberSpeaks.
//
// It is product surface for the operator and is golden-tested.
func (r Reminder) When(loc *time.Location) string {
	hhmm := fmt.Sprintf("%02d:%02d", r.Hour, r.Minute)
	switch r.Every {
	case EveryDaily:
		return "every day at " + hhmm
	case EveryWeekly:
		return "every " + r.Weekday.String() + " at " + hhmm
	default:
		return r.Next.In(loc).Format("Monday 2 January") + " at " + hhmm
	}
}

// newID returns a short member-visible code. Four hex characters is enough for a store
// bounded at a couple of dozen entries, and short enough that a member can type it
// back without resenting it.
func newID(taken func(string) bool) (string, error) {
	var b [2]byte
	for range 8 {
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("remind: generating an id: %w", err)
		}
		id := hex.EncodeToString(b[:])
		if !taken(id) {
			return id, nil
		}
	}
	return "", errors.New("remind: could not find an unused id")
}

// oneLine flattens text that will be rendered into a prompt behind a bullet.
//
// Reminder text is written by the model out of member text, and the pending list is
// shown to the model on every later turn — so it is the same class of content as a
// retrieved entry's title, and it gets the same defence. A reminder carrying a line
// break could otherwise put member-supplied text at column zero, where a forged
// section heading is indistinguishable from one of the prompt's own.
func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " "))
}
