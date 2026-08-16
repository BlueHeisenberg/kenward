// Package scope turns an inbound message into the authorization decision for the turn
// that follows it.
//
// This is the authorization boundary of the whole product. Everything downstream —
// retrieval, capture, routing — obeys the Scope produced here and re-derives nothing.
// Two properties are load-bearing and are asserted directly by the tests:
//
//   - A group scope never names any member's private space, in Read or in Write. The
//     household chat is not a way to read or write a private memory, and no message
//     arriving in it, from anyone, changes that.
//   - Neither does a household scope, which is the same guarantee for the same reason
//     in the one place it is easier to get wrong: a private chat with kenward carries
//     the member who is speaking and still reads and writes the shared space alone.
//     Knowing who is asking is not a route to what is theirs.
//   - Anything unrecognised resolves to ErrNotEnrolled and nothing else. The caller
//     drops the message in silence, because replying at all — even to refuse — confirms
//     to a stranger that this bot is a kenward node serving a real household.
//
// Resolution is fail-closed throughout: where the input is contradictory or the
// configuration is incomplete, the answer is ErrNotEnrolled, never a guess.
package scope

import (
	"errors"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// ErrNotEnrolled reports that a message belongs to no scope this node serves.
//
// It covers every rejection deliberately, as one error: an unknown Telegram account, a
// member who has not yet claimed their invite, a group the bot was added to that is not
// the household's, and a message whose chat and group flags disagree. The caller must
// treat them identically and answer none of them, so distinguishing them here would
// only invite a caller to leak the difference.
var ErrNotEnrolled = errors.New("scope: sender is not enrolled in this household")

// Resolve decides who is speaking and what the resulting conversation may touch.
//
// A direct message from an enrolled member resolves to a direct scope: it writes to
// their private space and reads their private space first, then the household's. A
// message in the configured group chat, from an enrolled member, resolves to a group
// scope, which reads and writes the shared space only — a member speaking in the group
// is speaking to the household, and their private space is not in the conversation at
// all. Everything else is ErrNotEnrolled, including a message in the household group
// from someone who is not a member of the household.
//
// bot names which of the household's bots the message arrived on: a member's id for
// their own agent's bot, empty for the household's own — kenward's. It is a fact about
// the process, known by whoever opened the token, and it is a parameter rather than a
// field on Inbound because Telegram does not put it on the wire: a private chat's id
// is the member's own account id and is identical across every bot they talk to, so
// which conversation a direct message belongs to cannot be recovered from the update.
//
// It is what separates a member's private chat with their own agent from their private
// chat with kenward, and under one agent each it is the only thing that does.
func Resolve(cfg *config.Config, bot domain.MemberID, in transport.Inbound) (domain.Scope, error) {
	if cfg == nil {
		// A missing configuration is a wiring fault, but the safe reading of "I do not
		// know who this is" is always the same one.
		return domain.Scope{}, ErrNotEnrolled
	}

	household := cfg.DomainHousehold()

	// The group is matched on chat id first, before anything about the sender is
	// considered. This ordering is what makes a member's message in the household chat
	// resolve to the household's scope rather than their own: the chat decides what the
	// conversation may touch, not the person. GroupChatID zero means no group is
	// configured, and must not match the zero-valued ChatID of a malformed update.
	if household.GroupChatID != 0 && in.ChatID == household.GroupChatID {
		// The chat and the flag have to agree. A message carrying the household's
		// chat id but not marked as a group message is contradictory, and the only
		// way to produce one is a group_chat_id that is really some member's own
		// chat: their direct messages would then arrive here and be answered into
		// the shared space, where the whole household reads them. Configuration
		// validation rejects that collision, and this is what stands behind it for
		// the configurations Resolve is handed without having validated. There is no
		// safe guess between "this is the household" and "this is a private chat",
		// so neither is made.
		if !in.IsGroup {
			return domain.Scope{}, ErrNotEnrolled
		}

		// The household's conversation belongs on the household's bot. A member's
		// own agent has no part in it, and a member's bot that has been added to the
		// family group — which any member can do — must not start answering there as
		// though it were kenward.
		if bot != "" {
			return domain.Scope{}, ErrNotEnrolled
		}

		// Being in the group chat is not enrolment. Any member can add anyone to a
		// Telegram group, deliberately or by accident, and the shared space holds
		// household knowledge — door codes, keys, logistics — that being added to a
		// chat must not hand over. So the sender has to be an enrolled member as well.
		//
		// This is an admission gate and nothing more: what a group scope may touch is
		// unchanged by who passed through it. The Scope built below still carries no
		// member identity and still names the shared space alone.
		if _, ok := cfg.MemberByTelegramID(in.UserID); !ok {
			return domain.Scope{}, ErrNotEnrolled
		}
		return groupScope(household, in.ChatID), nil
	}

	// Any other group chat is one the bot has been added to but is not configured to
	// serve. This is the common case of someone adding the bot to an unrelated group,
	// and it is served by silence.
	if in.IsGroup {
		return domain.Scope{}, ErrNotEnrolled
	}

	member, ok := cfg.MemberByTelegramID(in.UserID)
	if !ok || !member.Enrolled() {
		return domain.Scope{}, ErrNotEnrolled
	}

	// A member's own bot is their agent and nobody else's. Another member reaching
	// it is not a mistake to be served politely with their own scope: their assistant
	// lives elsewhere, this process holds this member's key and this member's memory,
	// and the answer a stranger gets is the answer they get. It has been true in
	// practice only because a pod runs no unit for anyone else; making it true here
	// means the boundary says so rather than the wiring happening to.
	if bot != "" {
		if bot != member.ID {
			return domain.Scope{}, ErrNotEnrolled
		}
		return directScope(member, household, in.ChatID), nil
	}

	// The household's own bot. Under one agent it is also everybody's agent, so a
	// private message to it is a member's own conversation and always has been.
	// Under one agent each it is kenward and only kenward: the member has their own
	// bot for their own assistant, and this chat is the one place they can add to the
	// household's memory, or ask what is in it, without doing so in front of everyone.
	if cfg.AgentPerMember() {
		return householdScope(member, household, in.ChatID), nil
	}
	return directScope(member, household, in.ChatID), nil
}

// groupScope builds the household scope. Member is nil and the private spaces of the
// household's members are not reachable from here by construction: this function is
// given no members to read from.
func groupScope(h domain.Household, chatID int64) domain.Scope {
	return domain.Scope{
		Kind:   domain.ScopeGroup,
		Member: nil,
		Write:  h.Shared,
		Read:   []domain.SpaceID{h.Shared},
		Tiers:  copyStrings(h.Tiers),
		ChatID: chatID,
	}
}

// householdScope builds a member's private conversation with kenward: the household's
// shared space, read and written, and the household's tier chain.
//
// It is groupScope with a member attached, and that is the whole of it. The member is
// carried so kenward knows who is asking — to authorise them, to address them, and to
// route the capture question's buttons to the person who spoke — and their private
// space is not reachable from here by construction, for exactly the reason a group
// scope's is not: nothing below reads m.Private, and there is nowhere in the returned
// Scope for it to go.
//
// The tiers are the household's rather than the member's. The subject of this
// conversation is the household's memory, so it travels wherever household
// conversations are allowed to travel; a member's own chain is the policy for their
// own material, and none of that is in this conversation.
func householdScope(m domain.Member, h domain.Household, chatID int64) domain.Scope {
	member := m
	return domain.Scope{
		Kind:   domain.ScopeHousehold,
		Member: &member,
		Write:  h.Shared,
		Read:   []domain.SpaceID{h.Shared},
		Tiers:  copyStrings(h.Tiers),
		ChatID: chatID,
	}
}

// directScope builds a member's private scope: writes land in their own space, reads
// see their space first and the household's second.
func directScope(m domain.Member, h domain.Household, chatID int64) domain.Scope {
	read := []domain.SpaceID{m.Private}
	// Configuration validation forbids a private space equal to the shared one, but
	// Resolve is also called with configurations it did not validate, and listing the
	// same space twice would make retrieval read it twice and rank it against itself.
	if h.Shared != "" && h.Shared != m.Private {
		read = append(read, h.Shared)
	}
	member := m
	return domain.Scope{
		Kind:   domain.ScopeDirect,
		Member: &member,
		Write:  m.Private,
		Read:   read,
		Tiers:  copyStrings(m.Tiers),
		ChatID: chatID,
	}
}

// copyStrings defends the configuration's tier chains from being edited through a
// Scope. The chain is the privacy policy and a later package must not be able to append
// to it by accident.
func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
