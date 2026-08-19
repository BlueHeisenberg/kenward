// Package privacy holds the statements kenward makes about what it protects.
//
// This text exists once, here, because it is the most important output the product
// produces. It is where a claim becomes checkable, and it is the thing a
// privacy-minded reader will test first. Two copies of it — one in the setup wizard,
// one in `kenward doctor` — is how one of them quietly drifts into promising more than
// the mode delivers.
//
// The statements are golden-tested. Changing one is a deliberate edit to a fixture,
// which is the point: nobody should be able to soften this wording by accident.
package privacy

import (
	"fmt"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Mode is the deployment topology a household chose.
type Mode int

const (
	// ModeUnknown is the zero value and has no statement. Rendering a privacy claim
	// for an unknown mode is a bug, not a default.
	ModeUnknown Mode = iota
	// ModeSimple runs every unit in one process, with every member's key in one
	// address space.
	ModeSimple
	// ModeIsolated runs one pod per member, each holding only its own key.
	ModeIsolated
)

func (m Mode) String() string {
	switch m {
	case ModeSimple:
		return "simple"
	case ModeIsolated:
		return "isolated"
	default:
		return "unknown"
	}
}

// simpleStatement is deliberately blunt about the operator. Separation between members
// is real in this mode; sealing against whoever runs the machine is not, and saying so
// plainly is what makes the isolated mode's claim believable when it is made.
const simpleStatement = `Every member's memory is separate: what you tell kenward in a private chat is
stored in your own space, and the household group can never read it.

What this mode does NOT do is seal anything against whoever runs this machine.
All members' keys live in one process here, and one bot token carries every
conversation, so the person operating this computer can read every member's
private memory — on the disk, and in flight on its way to and from Telegram.
For most households that is fine — it is your own family machine, and you
already trust whoever set it up.

If that is not the arrangement you want, isolated mode gives each member their
own sealed process, their own key and their own bot. It needs Linux with Podman
or Docker.

` + bothModes

// isolatedStatement states a strong claim and then immediately bounds it. The bound is
// not a disclaimer to be skimmed: a model must see plaintext to answer, so no
// server-side assistant can be end-to-end encrypted with respect to its own server, and
// a reader who discovers that themselves after being told otherwise will not believe
// anything else on the page.
//
// An earlier draft of this text claimed your key was unwrapped "only while you are
// actually talking". That was not true and could not be made true: re-unwrapping after
// an idle period would require you to send your passphrase again, and the only channel
// you have is Telegram — which would hand the passphrase to Telegram, to your chat
// history, and in simple mode to whoever holds the bot token. Refusing to do that is
// the right call, and the honest consequence is that your key stays in your own
// process's memory while that process is running.
//
// The idle paragraph then says both halves of that out loud, because for a while the
// text stated the no-expiry behaviour as fact while internal/session shipped a
// thirty-minute default that zeroed the key anyway. Two user-facing surfaces disagreeing
// about a fact a member is asked to trust is the exact failure this package exists to
// prevent, so the wording now describes the knob rather than one of its settings:
// session.idle_timeout is off by default, and a household that turns it on is buying a
// member who stops answering until somebody walks to the machine. Whichever way a
// household has it, the sentences below are true — which is what lets `kenward doctor`
// print them without knowing the setting.
const isolatedStatement = `Your assistant runs in its own process, with its own key and its own Telegram
bot. Nobody else in the household can read your private memory, and neither can
the person who runs this machine — not from the disk, not from a backup, and
not before your process has been unlocked.

Your passphrase is given to your own process when it starts, and never travels
over Telegram. That is deliberate: sending it in a chat message would hand it
to Telegram's servers and leave it in your message history, which would be
worse than the problem it solved. The consequence is that once your assistant
is unlocked and running, your key stays in that process's memory until it
stops or you lock it.

kenward does not lock it for you after a quiet spell, because there would be
no way to unlock it again from a chat. Your household can switch that on —
session.idle_timeout in kenward.yaml — and it is off unless someone did. Where
it is on, an assistant that has been quiet for that long stops answering until
somebody at the machine starts it again.

The honest limit: kenward has to see your words in plain text to answer them,
and it is the second member of your private space. That is the price of an
assistant that answers when your laptop is shut. Someone with root access to
this machine, while your assistant is running, could reach your key. There is
no way around that for any assistant that runs on a server and answers
questions — what changes is that reaching it means deliberately attacking your
own household, rather than opening a file.

` + bothModes

// bothModes states the two guarantees that do not depend on the topology, and it is
// appended to both statements for that reason.
//
// They belong here rather than in the wizard because they are the same kind of claim as
// everything else in this file: checkable, and worth nothing if the two places that make
// it disagree. The routing sentence in particular is the one an operator can verify
// against their own configuration in ten seconds, which is what makes the rest credible.
//
// The memory paragraph used to say that nothing was written without the member seeing
// the exact words first and saying yes. That stopped being true when a private write
// became something kenward does and then reports, and a statement in this file that has
// stopped being true is worse than no statement: it is the one page a privacy-minded
// reader tests, and the first claim they catch out is the last one they believe. What
// survived the change is the part that was load-bearing — the member finds out, in
// their own words, every time — so that is what it now says, together with the two
// halves that are settings and the two that are not.
const bothModes = `Two things hold whichever mode you are in. A conversation whose tier chain
names only machines in the house never reaches a provider: when none of them
answers, kenward refuses rather than reaching further, and there is no setting
that changes that.

And nothing is written to memory without you being told. A note to your own
private memory is written first and then shown to you in full — the exact words
and the space they went to — with an Undo button that removes it. Anything
going to the household's shared memory is shown to you first and written only
if you say yes, because other people will have read it by the time you regret
it. Your household can turn the private half back into a question, and some do.
It cannot turn off being told, and it cannot turn off the question for the
shared memory.`

// ownBotSimple is the paragraph a household under one-agent-each needs and would
// otherwise have to infer, and inferring it wrong is the one new misunderstanding this
// design can create.
//
// A member gets their own bot for two entirely different reasons, and both can be true
// at once. Isolation needs a per-member token so the operator never holds the key to
// somebody else's plaintext: that is a security property, and it is what isolated mode
// buys. Identity needs a separate contact so each agent is its own conversation: that
// is a presentation property and it is true in either mode. Somebody who has just made
// a bot of their own, in BotFather, with their own name on it, has every reason to
// believe they have bought the first when they have bought the second — the ceremony is
// identical, and nothing on the screen distinguishes them.
//
// So this says it flatly and in the same breath as what a bot does do. Leaving it to
// inference would be the exact failure this package exists to prevent, and it would be
// worse than the failures it already guards against, because this one a member would
// arrive at by reasoning carefully from what they were shown.
const ownBotSimple = `Your household gave everyone their own assistant, so you have a bot of your own
in Telegram. That is a separate contact, not a separate secret.

Your own bot does NOT mean your memory is sealed. This household is in simple
mode: every member's key is still in one process, and the person operating this
computer can still read every member's private memory, yours included. Making a
bot in BotFather changes who your assistant is; it does not change who can read
what you say to it.

Sealing is isolated mode, and it is the other question — the one about whether
everyone here trusts whoever runs this machine. It needs Linux with Podman or
Docker. You can have your own assistant without it, and most households do.`

// ownBotIsolated is the same paragraph where the answer is the good one. It is stated
// rather than omitted because a member who was warned in one household and told nothing
// in another has no way to know which they are in.
const ownBotIsolated = `Your household gave everyone their own assistant, and this household is in
isolated mode, so your bot is doing two jobs at once: it is your assistant's own
contact, and it is the reason no component anybody else controls ever sees your
messages in plain text.

The bot alone would not have bought you the second one. Under simple mode the
same bot would be a separate contact and nothing more. It is the mode that seals
your memory; the bot is what makes your assistant yours.`

// sharedOnlyNote is the paragraph for a household that has a member with no memory of
// their own, and it exists because everything above it is false about that member.
//
// Both statements open by promising a private space. The simple-mode one says "every
// member's memory is separate: what you tell kenward in a private chat is stored in
// your own space"; the isolated one says a member's assistant runs in a process of its
// own with a key of its own. A shared_only member has no space, no process and no key,
// and so has no separation to be told about — what they have is a private conversation
// whose contents go to a shared memory, which is the one arrangement in this product a
// reader could most reasonably mistake for the opposite of what it is.
//
// Not correcting it here would be the same defect as the sealed-memory language that
// used to appear under simple mode: a claim that is true of the household as a whole
// and false about one of the people in it, in the one document written to be checked.
//
// It takes no Mode, and that is the content rather than an omission. Sealing is about
// who can read a member's own memory. This member has none, so there is nothing for
// isolated mode to seal and nothing for simple mode to expose, and the honest answer
// is the same sentence in both.
const sharedOnlyNote = `Not everyone here has a memory of their own. A member marked shared_only in
kenward.yaml — a teenager, a grandparent, a lodger — is in the household and in
the group, and kenward answers them in both places. What they do not have is a
private space, an assistant of their own, or, in isolated mode, a container of
their own.

Everything kenward remembers from them goes to the household's shared memory,
where everyone can read it. That includes what they say in a private chat with
kenward: it is a private conversation, and it is not a private memory. They are
shown the exact words and nothing is written until they say yes — in that chat
exactly as in the group, because it is the same memory either way.

So the paragraphs above about a member's own memory are not about them. There
is nothing of theirs on this disk for either mode to protect and nothing for a
passphrase to wrap, and neither mode changes that. What they were given instead
is being told, in the first message kenward ever sends them, that this is how
it works.`

// SharedOnlyNote states what is and is not true for a member with no memory of their
// own. The caller renders it when the household has one and never otherwise: a
// paragraph about a kind of member nobody here is would be noise in the one block a
// privacy-minded reader actually finishes, which is the same rule OwnBotNote follows.
func SharedOnlyNote() string { return sharedOnlyNote }

// OwnBotNote states what a per-member bot buys, for a household that chose one
// assistant each. It is empty for an unknown mode, exactly as Statement is.
//
// It is separate from Statement rather than folded into it because a household under
// one shared agent has no per-member bots to be confused about, and a paragraph about
// bots nobody has is noise in the one block a privacy-minded reader actually finishes.
// The caller renders it when household.agents is per_member and never otherwise.
func OwnBotNote(m Mode) string {
	switch m {
	case ModeSimple:
		return ownBotSimple
	case ModeIsolated:
		return ownBotIsolated
	default:
		return ""
	}
}

// Statement returns the privacy claim for a mode, as prose, with no leading or trailing
// blank lines.
func Statement(m Mode) string {
	switch m {
	case ModeSimple:
		return simpleStatement
	case ModeIsolated:
		return isolatedStatement
	default:
		return ""
	}
}

// TierNote describes, for one conversation, what its tier chain means in practice. It
// is the line that turns "private conversations never leave my hardware" from a claim
// into something an operator can read off their own configuration.
//
// local reports whether every tier in the chain is one the household controls; the
// caller decides which tier names count, because that is configuration, not something
// this package can know.
func TierNote(label string, tiers []string, local bool) string {
	chain := strings.Join(tiers, ", ")
	if len(tiers) == 0 {
		// An empty chain is rejected by configuration validation, so reaching here
		// means something built a Scope by hand. Say something true rather than
		// something reassuring.
		return fmt.Sprintf("%s: no tiers configured — every request will be refused", label)
	}
	if local {
		return fmt.Sprintf("%s: [%s] — will refuse rather than use a provider", label, chain)
	}
	return fmt.Sprintf("%s: [%s] — may use a provider", label, chain)
}

// MemberNote is TierNote for a member's private conversation.
func MemberNote(m domain.Member, local bool) string {
	return TierNote(m.Name, m.Tiers, local)
}
