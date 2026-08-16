package setup

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

// The copy in this file is the product. Setup is the first thing a person touches,
// and most of what kenward is for evaporates if this reads like a configuration
// tool. Two rules hold everywhere below.
//
// Plain second person, no product voice. The reader is someone who was told this
// would take ten minutes, standing at a machine in their own house. They are not an
// operator, an admin or a user.
//
// Never a claim the code does not keep. Every sentence about privacy here is checked
// against ARCHITECTURE.md's key-custody section, and the simple-mode statement says
// the uncomfortable thing first rather than last. A wizard that oversells the mode
// somebody just chose is worse than no wizard, because it is believed.

// Banner opens the wizard.
const Banner = `kenward setup

A handful of questions, then one file. Nothing is sent anywhere while this runs,
and nothing you type here leaves the machine.`

// TrustQuestion is the question the mode is chosen with, verbatim from
// docs/CLI.md.
//
// It asks about trust rather than topology because a non-technical installer can
// answer the first and cannot answer the second. "Do you want per-member pods?" is
// unanswerable by the person who will actually be standing here; "does everyone
// trust whoever runs this machine?" is a question about their own household, and
// they are the only one who knows.
const TrustQuestion = `Does everyone in this household trust whoever runs this machine to be able to
read their private conversations?`

// The two answers to TrustQuestion, with the trailing alignment they are printed
// with. They are constants rather than a formatted table because the block they
// render into is golden-tested against docs/CLI.md, and a layout helper that drifts
// by one space would fail that test for no reason anybody could act on.
const (
	trustAnswerSimple   = "Yes — it is our own family machine.      (Simple)"
	trustAnswerIsolated = "No, or I would rather it were sealed.    (Isolated — needs Linux with\n" +
		"                                                Podman or Docker)"
)

// isolatedNeedsLinux explains, on a machine that cannot run isolated mode, why it
// cannot — and does not offer a degraded version of it under the same name.
//
// The temptation here is to say "isolated mode is not available on Windows yet" and
// carry on. That would leave someone believing their household is sealed when it is
// not, which is the single worst outcome this product has. So the explanation says
// what isolated mode actually does, why this machine cannot do it, and what to do
// instead, and then offers the mode that does work under its own honest name.
func isolatedNeedsLinux(goos string) string {
	return fmt.Sprintf(`Isolated mode is Linux only, and this is %s.

  Isolated mode works by running each member in their own container, with their
  own bot token, their own memory and their own key. That is what stops the
  person running the machine reading somebody else's private conversations, and
  it needs Podman or Docker. On %s there is no way to give you the same
  guarantee, and calling something isolated when it is not is the one thing this
  program must never do.

  Simple mode runs here and runs well. Separation between members is real in
  both modes. Sealing against whoever runs the machine is the part that only
  isolated mode gives you.

  If sealing is what you need, install kenward on a Linux machine — a spare box,
  a small server, a virtual machine on this one — and run setup there.`,
		osName(goos), osName(goos))
}

const (
	isolatedFallbackSimple = "Carry on in simple mode here."
	isolatedFallbackStop   = "Stop. I will set it up on Linux instead."
)

// osName renders a GOOS value the way a person refers to their own computer.
func osName(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	default:
		return goos
	}
}

// Questions asked in the household and member steps. Each carries its own reason
// for existing, because a question whose point is invisible gets a shrug for an
// answer.
const (
	householdIntro = `The household

Two names. Neither is shown to anyone outside the house.`

	questionHouseholdName = "What is this household called?"
	questionSharedSpace   = "Which space is the household's shared memory?"

	sharedSpaceNote = `  The shared memory is what the group chat reads and writes. Everyone in the
  household can see it. Each person also gets their own private memory, which
  nobody else in the household can read.

  Both are lore spaces, and kenward never creates one — a space is yours, and
  who is in it is your decision. So it asks which of the ones you already have
  to use.`

	membersIntro = `Who lives here

Names only, and first names are fine — a name is what kenward calls someone.

Telegram accounts are not asked for. Each person claims their own later with
` + "`kenward invite`" + `, which takes them ten seconds. Asking you to find somebody
else's numeric Telegram id would be a miserable way to spend the next hour.

One name per line. Press Enter on an empty line when everyone is in.`

	questionMemberName = "Name"
)

// The Telegram step. The walkthrough is short and numbered because it is the only
// part of setup that happens in another application, and the reader has to be able
// to look up from the screen and back down again without losing their place.
const (
	botFatherWalkthrough = `  1. Open Telegram and start a chat with @BotFather.
  2. Send /newbot.
  3. Give it a name — anything — and a username ending in "bot".
  4. BotFather replies with a token that looks like this:

         123456789:AAF-3jkkQ_pP8vd2X9j1kZq7wRsTuVwXyZ0

  5. Paste it below. It is not shown as you type.`

	telegramIntroSimple = `Telegram

kenward reaches you through a Telegram bot. You need to make one. It takes about
a minute, it is free, and it does not need a phone number of its own.`

	telegramIntroIsolated = `Telegram

In isolated mode every member gets their own bot. That is not ceremony: whoever
holds a bot's token can read every message sent to it, so a single household bot
would hand the operator every private conversation no matter how well the memory
is sealed on disk. Each member makes their own when they enrol, one each, and
you will not have to touch them.

What is needed now is the household's own bot — the one for the group chat.`

	questionBotToken = "Bot token"

	tokenLooksWrong = `  That does not look like a bot token. They are a number, a colon, and about
  thirty-five more characters — BotFather's message has it on a line of its own.`

	questionUseTokenAnyway  = "Use it anyway?"
	questionLeaveTokenUnset = "Leave it unset for now?"
)

// tokenNotStored is printed after a token is taken, every time. It is the moment to
// say where secrets live, because it is the moment the reader has just handed one
// over and is wondering.
func tokenNotStored(envVar string) string {
	return fmt.Sprintf(`  The token is not written into kenward.yaml. Nothing kenward writes ever
  holds a secret. The file names an environment variable, and kenward reads the
  value out of the environment when it starts:

      %s`, envVar)
}

const (
	questionWriteEnvFile = "Write the token to a .env file next to the configuration?"

	envFileNote = `  The file is created readable only by you, and .gitignore already excludes it.
  If you would rather keep the token in a password manager or a systemd unit,
  say no and export the variable yourself.`
)

// The endpoints step.
const (
	endpointsIntro = `Endpoints

The machines that actually run the model. At least one is needed.

Each is tried as you enter it, so a mistyped address shows up now rather than
the first time somebody asks a question. A machine that is simply switched off
is fine — that is the normal state of a desktop GPU, and kenward is built for
it.`

	questionEndpointName    = "A short label for it"
	questionEndpointBaseURL = "Base URL"
	questionEndpointModel   = "Model, as the server names it"
	questionEndpointKey     = "Does it need an API key?"
	questionEndpointKeyEnv  = "Environment variable to read the key from"
	questionEndpointKeyVal  = "Paste the key, or press Enter to set the variable yourself"
	questionEndpointTiers   = "Which tiers does it answer for?"
	questionAnotherEndpoint = "Add another endpoint?"

	endpointTiersNote = `  A tier is a name you route by. The convention is "local" for machines in the
  house and "cloud" for a provider; anything else is yours to invent. Separate
  several with commas.`
)

// The tier-chain step. This is where the privacy policy is actually decided, so the
// prompts spell out the consequence in the sentence that asks for the answer rather
// than in a paragraph above it, which is not read.
const (
	tiersIntro = `Where each conversation may go

A tier chain is the list of tiers a conversation may use, in order. It is the
privacy policy, and it is the one thing in the file worth reading twice: kenward
never widens a chain. If nothing in it answers, it says so and stops. It does
not quietly reach further.`

	tiersNoLocalWarning = `  Every endpoint you have configured is outside the house, so there is no
  local-only chain to fall back on. Private conversations will reach a provider,
  or they will not be answered at all. If that is not what you want, stop here,
  add a machine on your own network, and run setup again.`
)

// privateDefaultNote states what the local-only default means, for one member, in
// the words the member would use.
func privateDefaultNote(name string, chain []string) string {
	return fmt.Sprintf(`  %s's private conversations will use %s and nothing else.
  Nothing said in there leaves the house: if no local machine answers, kenward
  refuses rather than sending it to a provider.`, name, formatChain(chain))
}

// cloudConsequence is the sentence somebody reads while reaching for the y key. It
// says where the messages would actually go, by name, because "enable cloud
// fallback" is a phrase that hides the whole decision.
func cloudConsequence(what string, tiers []string, hosts []string) string {
	return fmt.Sprintf(`  Adding %s means %s can be sent to
  %s whenever no local machine answers.
  That is a deliberate choice, and it is not the default.`,
		formatChain(tiers), what, formatList(hosts))
}

// cloudOptIn is the question that widens a private chain.
func cloudOptIn(name string, tiers []string) string {
	return fmt.Sprintf("Allow %s for %s?", formatChain(tiers), name)
}

// groupDefaultNote is the same statement for the household's shared conversations.
func groupDefaultNote(chain []string) string {
	return fmt.Sprintf(`  The group chat will use %s and nothing else. It is shared memory:
  everyone in the household can read it. But shared inside the house is not the
  same as sent out of it, so it defaults local too.`, formatChain(chain))
}

// groupCloudOptIn widens the household chain.
func groupCloudOptIn(tiers []string) string {
	return fmt.Sprintf("Allow %s for the group chat?", formatChain(tiers))
}

// systemdNote is printed on Linux, where the documented install uses the unit in
// deploy/kenward.service — and that unit supplies secrets with LoadCredential=
// rather than an environment file.
//
// Without this, an operator who has just been told to set KENWARD_BOT_TOKEN opens
// the unit, finds no EnvironmentFile= anywhere in it, and concludes they have
// misread one of the two. Naming the difference costs four lines and answers the
// question before it is asked.
//
// It tells them what to change rather than only that something differs, because the
// change now works end to end: the runtime resolves every secret through config's
// accessors, so a credential-sourced token runs. An earlier version of this note
// stopped at "read the unit's comments", which was the honest thing to print while
// only the environment form was actually read.
//
// The last sentence is the one that makes the advice safe to follow. Editing the
// file that supplies your bot token, on a machine you may be sitting at over ssh,
// is the kind of change people put off because verifying it seems to mean sending a
// message and waiting to see whether a reply comes back. It does not: `kenward
// doctor` names the source each secret was read from.
//
// The credential name is the constant config uses to look one up, not a copy of it,
// so a change to the naming convention cannot leave this paragraph telling somebody
// to create a credential nothing will read.
const systemdNote = `If you are going to run this under the unit in deploy/kenward.service, it
supplies secrets with LoadCredential= rather than an environment file. Under
that unit, delete the bot_token_env line from kenward.yaml and let the
systemd credential ` + config.CredentialBotToken + ` supply the value instead — kenward reads a
secret from a variable, from a file, or from a credential, but from exactly
one of them, and naming two is an error rather than an order of preference.

kenward doctor prints where each secret was read from, and flags a token file
other people on the machine can read, so you can check the change took effect
without sending a message and waiting to see what happens.`

// personalSpacesSkipped explains a space missing from the list, before somebody
// goes looking for it.
const personalSpacesSkipped = `  Your personal lore space is not in this list. A personal space belongs to one
  account and can never cross accounts, so kenward cannot be the second member
  of it — and being the second member is what lets it answer you at all.`

// noSpaceFor is the end of the road when there is no space to use. It stops rather
// than inventing a name, because a name kenward writes here is a configuration that
// starts, accepts messages, saves memory, and then finds nothing the first time
// somebody asks it to remember — which is the failure this whole step exists to
// remove, not one to replace with another.
//
// It does not print a command to create a space. lore's own interface is lore's to
// document, and a wizard that guesses at somebody else's verb sends the operator
// off to debug an invented instruction.
func noSpaceFor(use string, all []memory.Space) string {
	var b strings.Builder
	fmt.Fprintf(&b, "There is no lore space this household can use for %s.\n\n", use)
	if len(all) == 0 {
		b.WriteString("  This lore home holds no spaces at all.\n")
	} else {
		b.WriteString("  What it holds:\n\n")
		for _, s := range all {
			fmt.Fprintf(&b, "    %s   %s   %s\n", s.Name, shortID(s.ID), s.Kind)
		}
	}
	b.WriteString(`
  Nothing has been written. Create a shared space in lore — one for the
  household, one for each person, each with that person and this machine in it
  — check it appears in ` + "`lore spaces`" + `, and run setup again.`)
	return b.String()
}

// loreUnreachable is printed when the listing cannot be fetched. The first line is
// the one that matters: an operator whose lore home has never been initialised
// needs to be told that, not handed the error underneath it.
//
// It used to distinguish "lore is not installed" from "lore ran and failed",
// because the listing came from a `lore mcp` subprocess and a missing binary was
// the common case. kenward opens the store itself now, so the binary's presence
// says nothing and the common case is a home with no account in it.
func loreUnreachable(cause error) string {
	first := "lore's store could not be opened, so setup cannot show you which spaces you have."
	if loreNotInitialised(cause) {
		first = "lore has not been set up on this machine yet — its home holds no account."
	}
	return fmt.Sprintf(`%s

  Looked in: %s

  %v

  Run `+"`lore init`"+` in another terminal if you have not, then create the spaces
  this household needs.

  Setup can carry on, but it needs the space ids rather than their names. Run
  `+"`lore spaces`"+` and copy the id column. A display name will not work here:
  lore does not make names unique, so kenward identifies a space by id, and a
  name configured here fails the first time somebody asks the assistant to
  remember something.`,
		first, loreHomeForMessage(), cause)
}

// loreHomeForMessage names the home the listing was attempted against, so an
// operator with more than one knows which. An unknown home says so rather than
// naming a directory it guessed at.
func loreHomeForMessage() string {
	if h := memory.DefaultLoreHome(); h != "" {
		return h
	}
	return "the default lore home (LORE_HOME is unset and this user has no home directory)"
}

// stoppedForLinux is printed when somebody chooses to go and do this properly.
const stoppedForLinux = `Nothing has been written. Copy kenward to a Linux machine and run setup there;
the questions will be the same ones.`

// stoppedNoLocal is printed when the only endpoints configured are outside the
// house and the operator would rather not send private conversations to them.
const stoppedNoLocal = `Nothing has been written. Add a machine on your own network — a desktop with a
GPU, a small server, anything that speaks the OpenAI API — and run setup again.`

// memberSummary shows what each name turned into, because the id and the space
// names appear in the file, in log lines and on the `kenward run --member` command
// line, and somebody should see them before they are written rather than after.
func memberSummary(members []config.MemberConfig) string {
	width := 0
	for _, m := range members {
		if n := utf8.RuneCountInString(m.Name); n > width {
			width = n
		}
	}
	var b strings.Builder
	for _, m := range members {
		fmt.Fprintf(&b, "  %s  id %s, private memory %s\n", pad(m.Name, width), m.ID, m.PrivateSpace)
	}
	b.WriteString("\n  Nobody else in the household can read a private memory, and kenward will\n")
	b.WriteString("  not bring one up in the group chat.")
	return b.String()
}

// memberTokenNote explains, in isolated mode, the variables that do not exist yet.
//
// It says plainly that kenward will not start until they do. That is not an
// inconvenience to apologise for: a household that is missing a member's token and
// starts anyway would be a household quietly running one bot for everybody, which is
// simple mode wearing isolated mode's name.
func memberTokenNote(members []config.MemberConfig) string {
	var b strings.Builder
	b.WriteString("  Each member also needs a bot of their own. They make it themselves when\n")
	b.WriteString("  they enrol, and kenward reads each token from its own variable — and a\n")
	b.WriteString("  passphrase of their own, which wraps their key and nobody else's:\n\n")
	for _, m := range members {
		fmt.Fprintf(&b, "      %s\n", m.BotTokenEnv)
		fmt.Fprintf(&b, "      %s\n", m.PassphraseEnv)
	}
	b.WriteString("\n  kenward will not start until those exist. That is the point of the mode: an\n")
	b.WriteString("  isolated household missing a token does not quietly become a shared one,\n")
	b.WriteString("  and one passphrase across two members is one wrapping secret for both.")
	return b.String()
}

// privacyBlock is what setup prints once the mode is settled.
//
// The statement itself belongs to internal/privacy and is printed verbatim. It is
// the most important output this product produces — the point at which a claim
// becomes checkable — and it is written once so that the wizard and `kenward
// doctor` cannot drift apart, because the way a promise like this decays is one
// copy being softened while the other is not.
//
// What belongs to the wizard is the framing around it: the heading, and the line
// telling somebody where they will see this text again. Softening the statement is
// not this package's to do, and the golden tests in internal/privacy make sure it
// is nobody's to do by accident.
func privacyBlock(mode config.Mode) string {
	heading := "Privacy, in simple mode"
	if mode == config.ModeIsolated {
		heading = "Privacy, in isolated mode"
	}
	return heading + "\n\n" + privacy.Statement(privacyMode(mode)) + "\n\n" + privacyTrailer
}

// privacyTrailer tells the reader this paragraph is not a one-off reassurance at the
// end of an installer. It is the same text `kenward doctor` prints, which is what
// makes it worth reading rather than skipping.
const privacyTrailer = `kenward doctor prints this same statement, in the same words, every time it
runs. If the two ever differ, one of them is wrong.`

// privacyMode maps the configured mode onto the privacy package's own.
//
// The two enumerations are kept separate deliberately: internal/privacy must not
// depend on the shape of a configuration file to state what a topology protects.
func privacyMode(mode config.Mode) privacy.Mode {
	if mode == config.ModeIsolated {
		return privacy.ModeIsolated
	}
	return privacy.ModeSimple
}

// pad right-pads a string to a width counted in runes rather than bytes.
//
// Go's %-*s counts bytes, so a household with a María in it would come out of an
// aligned column one space short of everybody else. It is a small thing, and it is
// the kind of small thing that tells somebody whether this program was written with
// their family in mind.
func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// formatChain renders a tier chain the way the configuration file writes it.
func formatChain(tiers []string) string {
	return "[" + strings.Join(tiers, ", ") + "]"
}

// formatList renders a list of things in a sentence: "a", "a and b", "a, b and c".
func formatList(items []string) string {
	switch len(items) {
	case 0:
		return "a provider"
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
