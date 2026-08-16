package main

import (
	"context"
	"strings"
	"testing"
)

// TestDoctorReportsABotThatCannotHearTheGroup.
//
// A household adds the bot to their family group long after setup ran, and Telegram's
// privacy mode — on by default for every new bot — means it receives nothing sent there:
// not plain messages, not a reply to it, not an @mention. Nothing arrives, so nothing is
// logged, and the only symptom is an assistant that ignores the group. `doctor` is the
// command somebody runs when something is wrong and they cannot say what, so it is the
// command that has to name this.
func TestDoctorReportsABotThatCannotHearTheGroup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.probes.telegram = func(context.Context, string) telegramResult {
		return telegramResult{Username: "our_household_bot"} // privacy mode on
	}
	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d, want 0: this is a warning, not a failure — the container's HEALTHCHECK runs this\n%s",
			code, h.stderr())
	}
	out := strings.Join(strings.Fields(h.stdout()), " ")
	for _, want := range []string{
		"cannot see messages in a group chat",
		"/setprivacy",
		// The ordering caveat: Telegram applies the change only to groups the bot
		// joins afterwards, so somebody who flips it and tests in the group the bot
		// is already in will think it did nothing.
		"add it again",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor does not mention %q:\n%s", want, h.stdout())
		}
	}
}

// TestDoctorSaysNothingAboutPrivacyModeWhenItIsOff, or the report grows a line nobody
// can act on for every healthy household.
func TestDoctorSaysNothingAboutPrivacyModeWhenItIsOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d\n%s", code, h.stderr())
	}
	if strings.Contains(h.stdout(), "privacy mode") {
		t.Errorf("doctor warns about privacy mode on a bot that has it off:\n%s", h.stdout())
	}
}

// TestAMembersOwnBotIsNotWarnedAboutTheGroup.
//
// In isolated mode a member's bot serves that member's private chat and never speaks in
// the group — the group's pod runs on the household bot. Privacy mode is irrelevant
// there, and a warning about it would appear on every member's pod, every health check,
// about nothing.
func TestAMembersOwnBotIsNotWarnedAboutTheGroup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, isolatedYAML, fullEnvironment())
	h.e.probes.telegram = func(context.Context, string) telegramResult {
		return telegramResult{Username: "david_kenward_bot"} // privacy mode on, everywhere
	}
	if code := h.run("doctor", "--member", "david"); code != exitOK {
		t.Fatalf("exit = %d\n%s", code, h.stderr())
	}
	if strings.Contains(h.stdout(), "privacy mode") {
		t.Errorf("a member's own pod is warned about a group it never speaks in:\n%s", h.stdout())
	}
}
