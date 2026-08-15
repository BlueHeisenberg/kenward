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
func Resolve(cfg *config.Config, in transport.Inbound) (domain.Scope, error) {
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
