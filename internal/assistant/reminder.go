// The reminder tools — remind and unremind — their specifications on the routing
// seam, and the defensive parsing of what the model does with them.
//
// The schemas are docs/PROMPT.md's, verbatim, and the parsing rules are remember.go's:
// a malformed call is dropped with a log line, never a crashed turn. What differs is
// what happens after a good one. A remember proposal becomes a question or an
// announcement; a reminder is simply set, and the member is told in the same reply.
// There is no button, and that is a deliberate difference from capture rather than an
// oversight — see applyReminders.

package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// The reminder tool names.
const (
	remindToolName   = "remind"
	unremindToolName = "unremind"
)

// remindSchema is the input schema from docs/PROMPT.md, verbatim.
const remindSchema = `{
  "type": "object",
  "required": ["text", "at"],
  "properties": {
    "text":  {"type": "string", "description": "The message to send when the time comes, written as it will be read. Nothing is generated at that moment — this exact text is what arrives."},
    "at":    {"type": "string", "description": "The time of day on a 24-hour clock, as HH:MM."},
    "every": {"type": "string", "enum": ["once", "daily", "weekly"], "description": "How often it repeats. Defaults to once."},
    "on":    {"type": "string", "description": "For once, the date as YYYY-MM-DD; for weekly, the name of the weekday. Omit it for daily, or for a one-off at the next time that clock reading comes round."}
  }
}`

// unremindSchema is the input schema from docs/PROMPT.md, verbatim.
const unremindSchema = `{
  "type": "object",
  "required": ["id"],
  "properties": {
    "id": {"type": "string", "description": "The code shown beside the reminder in the list of reminders above."}
  }
}`

// The notices the unit appends to a reply after a reminder tool call live in the
// catalogue (internal/lang, section REM).
//
// They are bracketed because this is the node accounting for what it did rather than
// the assistant talking. The comment here used to say "bracketed like the retrieval
// line"; the retrieval line stopped being bracketed when it gained italics and a
// glyph, so the brackets are now this convention alone.
//
// Every one of them is appended to the model's own answer inside the same Telegram
// message, which is why each is wrapped by Catalogue.Notice on its way out: in a
// right-to-left language a Latin-initial answer sets the paragraph base to LTR and
// the fragments of an unpinned notice lay out backwards relative to each other.

// remindSpecs are the reminder tools, offered in every scope.
//
// Unlike publish, these are not gated on the scope. A household reminder is a real
// thing a group conversation should be able to set — bin day is nobody's private
// business — and it lands in the group chat, which is the only chat the group unit can
// reach.
func remindSpecs() []routing.ToolSpec {
	return []routing.ToolSpec{
		{
			Name:        remindToolName,
			Description: "Set a reminder. At the time asked for, this conversation is sent the text given, exactly as written. Only set one when asked for one.",
			Schema:      json.RawMessage(remindSchema),
		},
		{
			Name:        unremindToolName,
			Description: "Cancel a reminder that is already set, by the code shown beside it. Cancel only the one that was named.",
			Schema:      json.RawMessage(unremindSchema),
		},
	}
}

// remindCall mirrors the tool schema. Unknown fields are tolerated: models decorate,
// and a decoration is not a malformation.
type remindCall struct {
	Text  string `json:"text"`
	At    string `json:"at"`
	Every string `json:"every"`
	On    string `json:"on"`
}

// applyReminders acts on this turn's reminder tool calls and returns the notice to
// append to the reply, plus a warning for the log.
//
// A reminder is set without asking, and that is not the confirmation posture capture
// has. It is the right difference: capture asks because the model *volunteered* a
// write to a memory the household will read for years, while a reminder is something
// the member asked for out loud, is reversible with one word, and writes nothing to
// lore at all. So the member is told rather than asked — which is the half of the
// capture rule that was always load-bearing.
//
// Only the first call to each tool is honoured, exactly as remember and publish do:
// one action per turn reaches the member, so a model that emits five reminder calls
// sets one and has four dropped into the log.
func (u *Unit) applyReminders(sc domain.Scope, calls []routing.ToolCall) (notice, warn string) {
	set, cancel, unusable, warn := extractReminders(calls)

	var notices []string
	if cancel != "" {
		notices = append(notices, u.cat.Notice(u.cancelReminder(cancel)))
	}
	if set != nil {
		n, w := u.setReminder(sc, *set)
		notices = append(notices, u.cat.Notice(n))
		warn = joinWarn(warn, w)
	}
	// A remind call the node could not even read is still a member who asked to be
	// reminded. Saying nothing is the one failure this feature cannot afford — they
	// find out by missing the thing it was for. setReminder says so about every
	// failure it can see; a call that never reached it needs the same.
	if set == nil && unusable {
		notices = append(notices, u.cat.Notice(u.cat.RemindFailed))
	}
	return strings.Join(notices, "\n"), warn
}

// extractReminders reads the completion's tool calls for the two reminder tools.
//
// unusable reports that a remind call was made and could not be read at all, which
// the caller must still tell the member about.
func extractReminders(calls []routing.ToolCall) (set *remindCall, cancelID string, unusable bool, warn string) {
	for _, c := range calls {
		switch c.Name {
		case remindToolName:
			if set != nil {
				warn = joinWarn(warn, "model made more than one remind call; using the first")
				continue
			}
			var call remindCall
			if err := json.Unmarshal(c.Arguments, &call); err != nil {
				warn = joinWarn(warn, fmt.Sprintf("remind arguments are not valid JSON: %v", err))
				unusable = true
				continue
			}
			set = &call
		case unremindToolName:
			if cancelID != "" {
				warn = joinWarn(warn, "model made more than one unremind call; using the first")
				continue
			}
			var call struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(c.Arguments, &call); err != nil {
				warn = joinWarn(warn, fmt.Sprintf("unremind arguments are not valid JSON: %v", err))
				continue
			}
			if cancelID = strings.TrimSpace(call.ID); cancelID == "" {
				warn = joinWarn(warn, "unremind call has no id")
			}
		}
	}
	return set, cancelID, unusable, warn
}

// setReminder stores one reminder and returns what to tell the member.
//
// Every failure produces a notice. A member who asked to be reminded and is told
// nothing will believe they were, and a reminder nobody set is the one failure this
// feature cannot afford — it is discovered by missing the thing it was for.
func (u *Unit) setReminder(sc domain.Scope, call remindCall) (notice, warn string) {
	hour, minute, err := parseClock(call.At)
	if err != nil {
		return u.cat.RemindFailed, err.Error()
	}
	every, err := remind.ParseEvery(call.Every)
	if err != nil {
		return u.cat.RemindFailed, err.Error()
	}

	weekday := time.Sunday
	date := ""
	switch every {
	case remind.EveryWeekly:
		if weekday, err = parseWeekday(call.On); err != nil {
			return u.cat.RemindFailed, err.Error()
		}
	case remind.EveryOnce:
		date = strings.TrimSpace(call.On)
	}

	loc := u.deps.Reminders.Location()
	// The chat comes from the resolved scope, which is the authorization decision for
	// this turn. It is stored on the reminder because there is no inbound message when
	// a timer fires and therefore no scope to resolve then: a reminder can only ever be
	// delivered to the conversation that asked for it.
	r, err := remind.New(call.Text, every, hour, minute, weekday, date, sc.ChatID, u.opts.Now(), loc)
	if err != nil {
		switch {
		case errors.Is(err, remind.ErrPast):
			return u.cat.RemindPast, err.Error()
		default:
			return u.cat.RemindFailed, err.Error()
		}
	}

	stored, err := u.deps.Reminders.Add(r)
	if err != nil {
		if errors.Is(err, remind.ErrFull) {
			return u.cat.RemindFull, err.Error()
		}
		return u.cat.RemindFailed, err.Error()
	}
	// The member's reading of the schedule, not remind.Reminder.When's. That method
	// serves three audiences from one place — this one, the model's system prompt
	// and the operator's CLI — and only this one is translated. See
	// TestReminderWhenStaysEnglishInThePrompt.
	//
	// stored.Text is the member's own words and is escaped by the catalogue entry:
	// this notice rides on a message sent with a parse mode, so a reminder titled
	// "<b>" must arrive as "<b>" rather than as markup somebody else chose.
	return u.cat.ReminderSet(u.cat.When(stored, loc), stored.Text, stored.ID), ""
}

// cancelReminder removes one reminder and returns what to tell the member.
func (u *Unit) cancelReminder(id string) string {
	r, err := u.deps.Reminders.Cancel(id)
	switch {
	case errors.Is(err, remind.ErrNoSuchReminder):
		return u.cat.UnremindNone
	case err != nil:
		return u.cat.UnremindFails
	}
	return u.cat.ReminderCancelled(r.Text)
}

// parseClock reads "HH:MM". It is deliberately strict: a time this function had to
// guess at is a reminder that arrives at the wrong hour, which is worse than a notice
// saying it could not be set.
func parseClock(s string) (hour, minute int, err error) {
	t := strings.TrimSpace(s)
	h, m, ok := strings.Cut(t, ":")
	if !ok {
		return 0, 0, fmt.Errorf("remind call has %q as a time; it must be HH:MM", s)
	}
	if hour, err = strconv.Atoi(strings.TrimSpace(h)); err != nil {
		return 0, 0, fmt.Errorf("remind call has %q as a time; it must be HH:MM", s)
	}
	if minute, err = strconv.Atoi(strings.TrimSpace(m)); err != nil {
		return 0, 0, fmt.Errorf("remind call has %q as a time; it must be HH:MM", s)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("remind call has %q as a time, which is not a time of day", s)
	}
	return hour, minute, nil
}

// parseWeekday reads a weekday name, in full or abbreviated to three letters.
func parseWeekday(s string) (time.Weekday, error) {
	want := strings.ToLower(strings.TrimSpace(s))
	for d := time.Sunday; d <= time.Saturday; d++ {
		name := strings.ToLower(d.String())
		if want == name || (len(want) == 3 && want == name[:3]) {
			return d, nil
		}
	}
	return time.Sunday, fmt.Errorf("remind call has %q as a weekday", s)
}
