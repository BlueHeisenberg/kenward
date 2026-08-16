package setup

import (
	"errors"
	"strings"
	"testing"
)

// Telegram's bot privacy mode, which is on for every new bot and which nothing in this
// product used to mention outside one aside in docs/TESTING.md.
//
// With it on, a bot in a group receives nothing: not plain messages, not a reply to it,
// not an @mention. Only `/start@thebot` is delivered. So a household adds the bot to
// their family group and it ignores everyone, with no error, no warning and no log line
// — nothing arrives, so there is nothing to log. It is the worst failure shape this
// product has: silent, total, and indistinguishable from the machine being off.

// privacyOffAnswers is a run whose only extra answer is the one the privacy-mode
// refusal asks: "Check again?", answered no.
func privacyOffAnswers() []string {
	answers := simpleAnswers()
	for i, a := range answers {
		// The bot token, immediately after which the check is made.
		if a == realToken {
			out := append([]string(nil), answers[:i+1]...)
			out = append(out, "n")
			return append(out, answers[i+1:]...)
		}
	}
	panic("the answer script no longer contains the bot token")
}

// TestPrivacyModeIsCaughtWhereItCanStillBeFixedCheaply.
//
// At this point in the wizard the token is in hand and the bot is not in any group yet,
// which is the only moment the fix costs nothing: Telegram applies a privacy-mode change
// only to groups the bot joins afterwards.
func TestPrivacyModeIsCaughtWhereItCanStillBeFixedCheaply(t *testing.T) {
	private := fixedBot(BotInfo{Username: "casa_household_bot", ReadsGroupMessages: false})
	_, _, io, err := runWizard(t, "linux", Options{Telegram: private}, privacyOffAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	transcript := io.Transcript()
	for _, want := range []string{
		// The consequence, which is the invisible part.
		"cannot see messages in a group chat",
		// The remedy, exactly as it is spelled in BotFather.
		"/setprivacy",
		"Disable",
		// And the ordering caveat. Without it, an operator flips the setting, sends
		// a message to the group the bot is already in, sees nothing happen and
		// concludes the fix did not work.
		"remove it and add it again",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the wizard did not say %q about a bot that cannot hear the group:\n%s", want, transcript)
		}
	}
}

// TestPrivacyModeCheckDoesNotBlock: it is offered again and then let go.
//
// A household that will never use a group chat is a real household, and refusing to
// finish setup over a setting that only affects groups would be the wizard deciding
// something that is not its to decide.
func TestPrivacyModeCheckOffersToCheckAgainAndThenLetsGo(t *testing.T) {
	private := fixedBot(BotInfo{Username: "casa_household_bot", ReadsGroupMessages: false})
	_, cfg, io, err := runWizard(t, "linux", Options{Telegram: private}, privacyOffAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg == nil {
		t.Fatal("nothing was written")
	}
	if io.Remaining() != 0 {
		t.Errorf("%d scripted answers were never used: the flow is not the one under test", io.Remaining())
	}
	if !strings.Contains(io.Transcript(), "Check again?") {
		t.Errorf("the wizard did not offer to check again:\n%s", io.Transcript())
	}
}

// TestAHealthyBotIsNamedAndNotWarnedAbout. The check has to be quiet when there is
// nothing wrong, or it is one more paragraph nobody reads.
func TestAHealthyBotIsNamedAndNotWarnedAbout(t *testing.T) {
	_, _, io, err := runWizard(t, "linux", Options{}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	transcript := io.Transcript()
	if !strings.Contains(transcript, "@casa_household_bot") {
		t.Errorf("the wizard never named the bot the token belongs to:\n%s", transcript)
	}
	if strings.Contains(transcript, "cannot see messages in a group chat") {
		t.Errorf("a bot with privacy mode already off was warned about anyway:\n%s", transcript)
	}
}

// TestTelegramUnreachableDoesNotStopSetup. The token may be perfectly good and the
// household's connection merely down, and a wizard that refused to finish over a check
// it could not make would strand somebody setting up on a train.
func TestTelegramUnreachableDoesNotStopSetup(t *testing.T) {
	down := unreachableTelegram(errors.New("dial tcp: no route to host"))
	_, cfg, io, err := runWizard(t, "linux", Options{Telegram: down}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg == nil {
		t.Fatal("nothing was written")
	}
	if !strings.Contains(io.Transcript(), "could not ask Telegram") {
		t.Errorf("the wizard did not say which check it could not make:\n%s", io.Transcript())
	}
}

// TestBotFatherWalkthroughSetsPrivacyBeforeTheBotJoinsAnything.
//
// The check above catches a bot that already has privacy mode on. This is the half that
// stops it happening: the walkthrough puts /setprivacy in the ceremony of making the
// bot, before it is in any group, which is the only place the fix costs one message
// rather than removing the bot from the group and adding it again.
func TestBotFatherWalkthroughSetsPrivacyBeforeTheBotJoinsAnything(t *testing.T) {
	if !strings.Contains(botFatherWalkthrough, "/setprivacy") {
		t.Errorf("the BotFather walkthrough never mentions privacy mode:\n%s", botFatherWalkthrough)
	}
	if i, j := strings.Index(botFatherWalkthrough, "/setprivacy"), strings.Index(botFatherWalkthrough, "Paste the token"); i > j {
		t.Errorf("privacy mode is dealt with after the token is pasted; it belongs in the making of the bot:\n%s",
			botFatherWalkthrough)
	}
}
