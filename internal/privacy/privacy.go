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
All members' keys live in one process here, so the person operating this
computer can read every member's private memory. For most households that is
fine — it is your own family machine, and you already trust whoever set it up.

If that is not the arrangement you want, isolated mode gives each member their
own sealed process, their own key and their own bot. It needs Linux with Podman
or Docker.`

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

The honest limit: kenward has to see your words in plain text to answer them.
Someone with root access to this machine, while your assistant is running,
could reach your key. There is no way around that for any assistant that runs
on a server and answers questions — what changes is that reaching it means
deliberately attacking your own household, rather than opening a file.`

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
