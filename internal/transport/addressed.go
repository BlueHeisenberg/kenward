package transport

import (
	"strings"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"
)

// addressedTo reports whether a message speaks to this bot rather than to the room.
//
// It rebuilds, deliberately, the gate Telegram used to provide for free. Bot privacy
// mode is on by default and with it on a group delivers a bot only what names it;
// kenward's wizard and doctor now require it off, because with it on the bot receives
// nothing at all and ignores the household in silence. Turning it off means every
// sentence a family says to each other now arrives here, so the set below is
// Telegram's own privacy-mode set, restated where kenward can see it:
//
//   - an @mention of this bot, anywhere in the text
//   - a reply to one of this bot's own messages
//   - a bot command, unless it is aimed at some other bot by name
//   - a text_mention entity carrying this bot's user record
//
// It is a fact about the update, not a decision about the turn. Which conversations
// require it is internal/assistant's to say, and it says so on the scope; a private
// chat carries the flag exactly as a group does and nothing reads it there.
//
// An empty username fails open — every message is addressed. A transport that never
// learned its own name would otherwise gate the whole household into silence, which
// is the failure this function exists to prevent and the one nothing in the chat
// would show; answering too much is wrong in a way a household can see and report.
func addressedTo(m *models.Message, username string, botID int64) bool {
	if username == "" {
		return true
	}
	handle := "@" + username

	// A reply is the member pointing at a message, and pointing at one of ours is
	// addressing us. Bots always carry a username in `from`, so this compares the
	// same field the mention path does.
	if r := m.ReplyToMessage; r != nil && r.From != nil && strings.EqualFold(r.From.Username, username) {
		return true
	}

	// Entity offsets are counted in UTF-16 code units, which is neither bytes nor
	// runes: one emoji ahead of a mention moves a byte offset by four and a rune
	// offset by one. Encoding once here costs one pass over a chat message.
	units := utf16.Encode([]rune(m.Text))
	for _, e := range m.Entities {
		switch e.Type {
		case models.MessageEntityTypeMention:
			if strings.EqualFold(entityText(units, e), handle) {
				return true
			}
		case models.MessageEntityTypeBotCommand:
			// A command is never one member talking to another, so a bare
			// `/reset` counts — that is Telegram's own rule, and it is why a
			// bare command is the one thing privacy mode still delivers. A
			// command that names a bot names exactly one, so `/roll@dice_bot`
			// is not ours.
			_, named, ok := strings.Cut(entityText(units, e), "@")
			if !ok || strings.EqualFold(named, username) {
				return true
			}
		case models.MessageEntityTypeTextMention:
			// Telegram produces this only for a user with no username, and every
			// bot has one, so it is unreachable in practice. Matched on the id
			// anyway: the id is free, and a rule that holds for a reason living
			// outside this file is a rule that stops holding without a sound.
			if e.User != nil && e.User.ID == botID {
				return true
			}
		}
	}
	return false
}

// entityText returns the text one entity covers, or "" if it points outside the
// message. Telegram does not send those; a hostile or broken API server can.
func entityText(units []uint16, e models.MessageEntity) string {
	if e.Offset < 0 || e.Length < 0 || e.Offset+e.Length > len(units) {
		return ""
	}
	return string(utf16.Decode(units[e.Offset : e.Offset+e.Length]))
}
