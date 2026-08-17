package transport

import (
	"fmt"
	"testing"

	"github.com/go-telegram/bot/models"
)

// The bot the telegramtest server's getMe answers for.
const testBotUsername = "kenward_bot"

// msg builds the Message part of an update, so a case can say what it is about and
// nothing else. The fields the transport reads are the only ones set.
func msg(text string, entities []models.MessageEntity, replyFrom *models.User) *models.Message {
	m := &models.Message{
		ID:       1,
		Date:     1700000000,
		From:     &models.User{ID: 7, FirstName: "M"},
		Chat:     models.Chat{ID: -200, Type: models.ChatTypeSupergroup},
		Text:     text,
		Entities: entities,
	}
	if replyFrom != nil {
		m.ReplyToMessage = &models.Message{ID: 99, From: replyFrom, Chat: m.Chat, Text: "earlier"}
	}
	return m
}

func mention(offset, length int) models.MessageEntity {
	return models.MessageEntity{Type: models.MessageEntityTypeMention, Offset: offset, Length: length}
}

func command(offset, length int) models.MessageEntity {
	return models.MessageEntity{Type: models.MessageEntityTypeBotCommand, Offset: offset, Length: length}
}

var (
	kenward  = &models.User{ID: 42, IsBot: true, FirstName: "kenward", Username: testBotUsername}
	otherBot = &models.User{ID: 43, IsBot: true, FirstName: "dice", Username: "dice_bot"}
	member   = &models.User{ID: 8, FirstName: "Mei"}
)

// The full addressed set, read off the update shape Telegram actually sends.
func TestAddressedTo(t *testing.T) {
	tests := []struct {
		name string
		m    *models.Message
		want bool
	}{
		{
			"an ordinary sentence between two members",
			msg("are you picking Mei up or am I?", nil, nil),
			false,
		},
		{
			"a mention at the start",
			msg("@kenward_bot what did we decide about the boiler?", []models.MessageEntity{mention(0, 12)}, nil),
			true,
		},
		{
			// Offsets are UTF-16 code units. The emoji is one rune, four bytes and
			// two units, so a transport that counted either of the other two would
			// slice the wrong window and miss the mention.
			"a mention in the middle of a sentence, after an emoji",
			msg("🔥 so ask @kenward_bot about it", []models.MessageEntity{mention(10, 12)}, nil),
			true,
		},
		{
			"a mention of somebody else",
			msg("@mei can you get the door", []models.MessageEntity{mention(0, 4)}, nil),
			false,
		},
		{
			"a reply to one of kenward's own messages",
			msg("no, the other one", nil, kenward),
			true,
		},
		{
			"a reply to a member's message",
			msg("no, the other one", nil, member),
			false,
		},
		{
			// A reply is a routing decision about the message being replied to, so
			// this is addressed on the mention alone. It is the shape a family
			// actually produces: quoting what somebody said and asking kenward
			// about it.
			"a reply to a member's message that also mentions the bot",
			msg("@kenward_bot is that right?", []models.MessageEntity{mention(0, 12)}, member),
			true,
		},
		{
			"a reply to another bot's message",
			msg("thanks", nil, otherBot),
			false,
		},
		{
			"a command aimed at this bot",
			msg("/reset@kenward_bot", []models.MessageEntity{command(0, 18)}, nil),
			true,
		},
		{
			// Telegram delivers a bare command to every bot in a group even with
			// privacy mode on, which is Telegram saying a command is never a
			// message between members.
			"a bare command",
			msg("/reset", []models.MessageEntity{command(0, 6)}, nil),
			true,
		},
		{
			"a command aimed at another bot",
			msg("/roll@dice_bot", []models.MessageEntity{command(0, 14)}, nil),
			false,
		},
		{
			// Telegram only produces text_mention for a user with no username, and
			// every bot has one, so this is unreachable in practice. It is matched
			// on the user id anyway because the id is free and a rule that holds
			// for reasons outside this file is a rule that stops holding quietly.
			"a text mention carrying the bot's user record",
			msg("ask him", []models.MessageEntity{{
				Type: models.MessageEntityTypeTextMention, Offset: 4, Length: 3, User: kenward,
			}}, nil),
			true,
		},
		{
			"a text mention of somebody else",
			msg("ask her", []models.MessageEntity{{
				Type: models.MessageEntityTypeTextMention, Offset: 4, Length: 3, User: member,
			}}, nil),
			false,
		},
		{
			// The name in a code span is not a mention entity, so it is not a
			// mention, whatever the characters say.
			"the bot's name quoted as code",
			msg("the handle is @kenward_bot", []models.MessageEntity{{
				Type: models.MessageEntityTypeCode, Offset: 14, Length: 12,
			}}, nil),
			false,
		},
		{
			"a mention in the wrong case",
			msg("@KenWard_Bot are you there", []models.MessageEntity{mention(0, 12)}, nil),
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := addressedTo(tc.m, testBotUsername, 42); got != tc.want {
				t.Fatalf("addressedTo(%q) = %v, want %v", tc.m.Text, got, tc.want)
			}
		})
	}
}

// A transport that never learned its own username must answer rather than fall
// silent. Going silent is the exact failure the bot-privacy fix was for, and it
// leaves no trace anywhere; answering too much is visible in the chat within one
// message.
func TestUnknownUsernameFailsOpen(t *testing.T) {
	if !addressedTo(msg("are you picking Mei up?", nil, nil), "", 42) {
		t.Fatal("a transport with no username of its own gated a group message; it must fail open")
	}
}

// Real Telegram does not send an entity pointing outside the text. A broken or
// hostile API server can, and slicing on it would panic the poll goroutine.
func TestEntityOffsetsOutOfRangeAreIgnored(t *testing.T) {
	for _, e := range []models.MessageEntity{
		mention(0, 999),
		mention(-1, 4),
		mention(400, 2),
	} {
		t.Run(fmt.Sprintf("%d+%d", e.Offset, e.Length), func(t *testing.T) {
			if addressedTo(msg("@kenward_bot hi", []models.MessageEntity{e}, nil), testBotUsername, 42) {
				t.Fatal("an entity pointing outside the text was read as a mention")
			}
		})
	}
}
