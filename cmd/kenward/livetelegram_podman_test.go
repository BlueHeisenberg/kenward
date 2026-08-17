//go:build integration && linux

package main

// One real bot, one real pod, real api.telegram.org.
//
// thirdscope_podman_test.go drives three bots into three pods and asserts the whole
// authorization boundary, but every one of those bots is a stand-in. This file is the
// other half of the honesty: it takes the household's single real Telegram token, gives
// it to the group's pod, and lets that pod dial the actual api.telegram.org.
//
// # What it can assert, and what it cannot
//
// It can assert that a per_member pod authorises a real bot against the real service and
// goes on serving. That is not nothing: every previous run of TestIsolatedPodman ended
// with every pod dying at
//
//	kenward: supervisor: building telegram transport: transport: telegram: error call getMe, unauthorized
//
// and built its assertions around the fact. A pod that gets past getMe against the real
// service has done everything those runs did *and* proved its egress, its CA bundle, its
// token resolution and the Bot API contract against the thing itself.
//
// It cannot assert anything about a conversation. Driving one would mean sending a
// message *to* the bot as a Telegram user, and a bot token cannot do that — only a human
// with a Telegram client, or a user-account API session, can. So there is no live turn
// here and there is no live third scope here; what there is, is the transport half of it
// proven against the real service, with the conversation half proven in the sibling file
// against a stand-in. Neither file pretends to be the other.
//
// The other members' pods hold deliberately invalid tokens and are expected to fail. That
// is the one-token limit, made visible rather than described: a household of three bots
// cannot be stood up live on a developer's single token, and this file says so by
// asserting the failure it causes.
//
// # The token
//
// Read from the file named by KENWARD_E2E_REAL_BOT_TOKEN_FILE, never from a literal and
// never from a checked-in fixture. Nothing in this file logs it: every message that could
// carry it goes through redact() first, and the assertions on other pods report the
// *label* of a leaked secret rather than its value.
//
//	KENWARD_E2E_REAL_BOT_TOKEN_FILE=/path/to/bot.token \
//	  go test -tags integration -run TestPerMemberLiveTelegram -timeout 20m -v ./cmd/kenward/

import (
	"os"
	"strings"
	"testing"
	"time"
)

// realTokenEnv names the file holding the household's real bot token.
const realTokenEnv = "KENWARD_E2E_REAL_BOT_TOKEN_FILE"

// realToken reads the token, or skips. It is never returned to anywhere that logs.
func realToken(t *testing.T) string {
	t.Helper()
	path := os.Getenv(realTokenEnv)
	if path == "" {
		t.Skipf("%s is not set; this test needs a real Telegram bot token to dial the real service", realTokenEnv)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read the bot token from %s: %v", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		t.Skipf("%s names an empty file", path)
	}
	return tok
}

// redact removes the token from anything about to be logged. Every t.Log and t.Error in
// this file goes through it, because a pod's log is the natural thing to print on failure
// and a token in a CI transcript is a token that has to be rotated.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<REAL BOT TOKEN REDACTED>")
}

// TestPerMemberLiveTelegram brings up a per_member household in which the group's pod
// holds the household's real bot token and talks to the real api.telegram.org.
func TestPerMemberLiveTelegram(t *testing.T) {
	token := realToken(t)

	// No redirectTelegramToHost here, deliberately and load-bearingly: this pod must
	// resolve api.telegram.org to Telegram. The CA bundle is the image's own, untouched.
	r := newRig(t)

	hh := newHousehold(t, r, perMemberConfig(liveModelURL(),
		placeholderShared, placeholderDavid, placeholderJordan), liveTokenEnv(token))

	david, jordan, group := hh.memberPod("david"), hh.memberPod("jordan"), hh.groupPod()

	// The dwell has to happen inside the wait, not after it: hh.start stops the
	// supervisor the moment its condition holds, and a pod inspected after that has been
	// drained on purpose. Checking "is it still up?" afterwards asks whether Stop worked.
	var stillRunning, sawRunning bool
	sup, _ := hh.supervisorFor(r.image)
	hh.start(sup, func() bool {
		// The group's pod is the only one that can succeed. Wait for it to say so, or
		// for it to have died trying.
		if !hh.containerExists(group) {
			return false
		}
		log := hh.logs(group)
		switch {
		case strings.Contains(log, "supervisor: started"):
		case strings.Contains(log, "getMe"):
			return true // it failed; stop waiting and let the assertions report it
		default:
			return false
		}
		sawRunning = true
		// It kept serving rather than authorising once and falling over. A long poll
		// that dies immediately would look identical to success above.
		time.Sleep(20 * time.Second)
		v, err := hh.tryInspect(group)
		stillRunning = err == nil && v.State.Running
		return true
	})

	// --- the assertion this file exists for ---

	groupLog := hh.logs(group)
	switch {
	case sawRunning:
		t.Logf("the group's pod authorised the household's real bot against the real "+
			"api.telegram.org and went on serving: per_member, isolated mode, inside a "+
			"container. Still running twenty seconds later: %v", stillRunning)
	case strings.Contains(groupLog, "getMe"):
		t.Errorf("the group's pod could not authorise the real bot token against the real "+
			"api.telegram.org:\n%s", redact(lastLine(groupLog), token))
	default:
		t.Errorf("the group's pod neither started nor reached Telegram:\n%s",
			redact(lastLine(groupLog), token))
	}

	if sawRunning && !stillRunning {
		t.Errorf("the group's pod authorised the real bot and then stopped within twenty "+
			"seconds; its log ends:\n%s", redact(lastLine(hh.logs(group)), token))
	}

	// --- the one-token limit, asserted rather than described ---
	//
	// david's and jordan's pods hold tokens no Telegram bot has, so they cannot come up.
	// Recording it here is what keeps "we tested per_member live" from being read as more
	// than it is.
	for _, pod := range []string{david, jordan} {
		if !hh.containerExists(pod) {
			continue
		}
		if log := hh.logs(pod); strings.Contains(log, "supervisor: started") {
			t.Errorf("pod %s came up on a token that is not a real bot; the stand-in must be "+
				"leaking into this test", pod)
		}
	}
	t.Log("david's and jordan's pods are expected to fail here: there is one real token in " +
		"this household and it belongs to kenward. Their conversations are covered in " +
		"TestThirdScopeAgainstPods, against a stand-in.")

	// --- and the real token is where it belongs and nowhere else ---

	assertPodEnv(t, hh, group, map[string]string{
		"KENWARD_BOT_TOKEN_HOUSEHOLD": token,
	}, map[string]string{
		"david's passphrase":  e2eDavidPass,
		"jordan's passphrase": e2eJordanPass,
	})
	for _, pod := range []string{david, jordan} {
		assertPodEnv(t, hh, pod, nil, map[string]string{
			"the household's REAL bot token": token,
		})
	}
}

// liveTokenEnv is perMemberEnv with the household's real token in place of the stand-in's.
func liveTokenEnv(token string) map[string]string {
	env := perMemberEnv()
	env["KENWARD_BOT_TOKEN_HOUSEHOLD"] = token
	return env
}
