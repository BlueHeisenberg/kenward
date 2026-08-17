package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// The dashboard's half of the same four defects: the identity answer decides how many
// Telegram bots a household has to make, and until now it could be given, refused,
// forgotten and never restated, on a page with no way back.

// wizardTo walks the flow as far as the trust step, with the mode chosen, and leaves the
// caller on the identity step. It stops there because that is where all four of these
// tests happen.
func wizardTo(t *testing.T, h *harness, mode string) {
	t.Helper()
	token := h.issueToken()
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()
	for _, st := range []struct {
		path string
		form url.Values
	}{
		{"/setup/install", url.Values{"install": {"host"}, "household_name": {"Casa"}, "members": {"David\nMaría\n"}}},
		{"/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}}},
		{"/setup/telegram", url.Values{"bot_token": {"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"}, "bot_token_env": {"KENWARD_BOT_TOKEN"}}},
		{"/setup/endpoints", url.Values{"endpoint.0.name": {"monster"}, "endpoint.0.base_url": {"http://monster.tail:8000/v1"}, "endpoint.0.model": {"qwen3"}, "endpoint.0.tiers": {"local"}}},
		{"/setup/trust", url.Values{"mode": {mode}}},
	} {
		r := h.postCSRF(st.path, st.form)
		r.Body.Close()
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d, want 303", st.path, r.StatusCode)
		}
	}
}

// TestWizardRefusesOneEachWithNoGroupChat: the same refusal the terminal wizard makes,
// where the clicking happened.
//
// Under one assistant each kenward lives in the group chat and nowhere else — every
// private chat belongs to somebody's own assistant — and the supervisor creates the
// group's pod only when household.group_chat_id is set. So a per_member configuration
// with no group chat id is a household that cannot be reached at all, and this wizard
// used never to ask. `kenward doctor` warns about it afterwards; nothing here told
// anybody to run doctor.
func TestWizardRefusesOneEachWithNoGroupChat(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "isolated")

	resp := h.postCSRF("/setup/advanced", url.Values{
		"agents":         {"per_member"},
		"update_channel": {"stable"},
		"idle_timeout":   {"0s"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the wizard accepted one assistant each with no group chat id: status = %d", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "no kenward in it") {
		t.Errorf("the refusal does not say what it costs:\n%s", page)
	}
	if !strings.Contains(page, "getUpdates") {
		t.Errorf("the refusal does not say how to find the id:\n%s", page)
	}
}

// TestWizardTakesTheGroupChatAndCarriesOn: with the id given, the step passes.
func TestWizardTakesTheGroupChatAndCarriesOn(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "isolated")

	resp := h.postCSRF("/setup/advanced", url.Values{
		"agents":         {"per_member"},
		"group_chat_id":  {"-1001234567890"},
		"update_channel": {"stable"},
		"idle_timeout":   {"0s"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", resp.StatusCode, body(t, resp))
	}
}

// TestARefusedIdentityAnswerSurvivesTheRerender.
//
// The telegram step already does this deliberately — it assigns the rejected token back
// onto the state before rendering the error, so a token that was refused is still in the
// box. The identity answer was not, so the refusal re-rendered the radio as "shared" and
// the operator's choice was gone: the page said "you cannot have that here" and then
// quietly recorded that they had not asked for it.
func TestARefusedIdentityAnswerSurvivesTheRerender(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "simple")

	resp := h.postCSRF("/setup/advanced", url.Values{
		"agents":           {"per_member"},
		"persona_language": {"Spanish"},
		"update_channel":   {"stable"},
		"idle_timeout":     {"0s"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, `value="per_member" checked`) {
		t.Errorf("the refused identity answer was discarded from the form:\n%s", page)
	}
	// And so is everything else that was typed on the same screen, for the same
	// reason: a form that empties itself on a refusal is a form filled in twice.
	if !strings.Contains(page, `value="Spanish"`) {
		t.Errorf("the persona language was discarded from the form:\n%s", page)
	}
}

// TestTheRefusalPointsAtSomethingThatExists: the wizard's refusal names Back, and the
// page has one.
func TestTheRefusalPointsAtSomethingThatExists(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "simple")

	resp := h.postCSRF("/setup/advanced", url.Values{
		"agents":         {"per_member"},
		"update_channel": {"stable"},
		"idle_timeout":   {"0s"},
	})
	defer resp.Body.Close()
	page := body(t, resp)
	if !strings.Contains(page, "two agents behind one contact are one agent") {
		t.Errorf("the wizard refused without the reason:\n%s", page)
	}
	if !strings.Contains(page, `href="/setup/trust"`) {
		t.Errorf("the refusal tells the operator to go back and there is no way to:\n%s", page)
	}
}

// TestEveryWizardStepAfterTheFirstHasABack.
//
// There was no back control anywhere: no link, no button, and the progress list is
// inert. The only way to change an earlier answer was to edit the URL, which is not
// something the person this wizard is written for is going to do — and two refusals in
// this flow tell them to go back and choose differently.
func TestEveryWizardStepAfterTheFirstHasABack(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "isolated")

	for _, tc := range []struct{ step, back string }{
		// The account step is stepped over rather than offered: it cannot be
		// re-entered, there is no reset flow, and re-submitting it is a 409.
		{"telegram", "/setup/install"},
		{"endpoints", "/setup/telegram"},
		{"trust", "/setup/endpoints"},
		// This household is isolated, so it has the per-member bots step; a simple
		// one does not, and TestSimpleModeNeverSeesTheBotsStep asserts Back steps
		// over it there rather than bouncing off it.
		{"bots", "/setup/trust"},
		{"advanced", "/setup/bots"},
		{"review", "/setup/advanced"},
	} {
		resp := h.get("/setup/" + tc.step)
		page := body(t, resp)
		resp.Body.Close()
		if !strings.Contains(page, `href="`+tc.back+`"`) {
			t.Errorf("/setup/%s has no way back to %s:\n%s", tc.step, tc.back, page)
		}
	}

	resp := h.get("/setup/install")
	page := body(t, resp)
	resp.Body.Close()
	if strings.Contains(page, `href="/setup/`) {
		t.Errorf("the first step offers a way back to something before it:\n%s", page)
	}
}

// TestReviewRestatesTheIdentityAnswerAndThePersona.
//
// "Check, then write it" listed the household, the people, the mode, the endpoints, the
// bot token and whether a provider is reachable — and neither the identity answer nor
// the persona. A choice that changes how many Telegram bots the household has to create,
// and the words the assistant everybody reads will speak in, were committed without
// being restated.
func TestReviewRestatesTheIdentityAnswerAndThePersona(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "isolated")

	resp := h.postCSRF("/setup/advanced", url.Values{
		"agents":            {"per_member"},
		"group_chat_id":     {"-1001234567890"},
		"persona_language":  {"Spanish"},
		"persona_tone":      {"warm"},
		"persona_character": {"Knows the house well."},
		"update_channel":    {"stable"},
		"idle_timeout":      {"0s"},
	})
	resp.Body.Close()

	review := h.get("/setup/review")
	page := body(t, review)
	review.Body.Close()

	for _, want := range []string{"per_member", "-1001234567890", "Spanish", "warm", "Knows the house well."} {
		if !strings.Contains(page, want) {
			t.Errorf("the review step does not restate %q before writing it:\n%s", want, page)
		}
	}
}

// TestReviewSaysWhenNothingAboutThePersonaWasChanged: three empty boxes are an answer,
// and the review says which one rather than showing three blanks.
func TestReviewSaysWhenNothingAboutThePersonaWasChanged(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "simple")

	resp := h.postCSRF("/setup/advanced", url.Values{"update_channel": {"stable"}, "idle_timeout": {"0s"}})
	resp.Body.Close()

	review := h.get("/setup/review")
	page := body(t, review)
	review.Body.Close()
	if !strings.Contains(page, "as it always has") {
		t.Errorf("the review step does not say what an untouched persona means:\n%s", page)
	}
	if !strings.Contains(page, "shared") {
		t.Errorf("the review step does not restate the identity answer:\n%s", page)
	}
}

// TestSettingsRefusesOneEachWithAdviceItCanFollow.
//
// The settings page carried the wizard's refusal verbatim — "or seal the household in
// isolated mode" — on a page with no mode field, so the advice could never be acted on
// from where it was given. Mode is not editable here on purpose: it decides where every
// member's key lives. Saying that plainly is the fix; repeating advice the page cannot
// support is not.
func TestSettingsRefusesOneEachWithAdviceItCanFollow(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.signIn()
	h.writeConfig()

	form := settingsForm(t, h)
	form.Set("agents", "per_member")
	resp := h.postCSRF("/settings", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "not editable here") {
		t.Errorf("the settings refusal gives advice this page cannot support:\n%s", page)
	}
	if strings.Contains(page, "seal the household in isolated mode") {
		t.Errorf("the settings refusal still tells the reader to do something no control here does:\n%s", page)
	}
}

// TestSettingsRefusesOneEachWithNoGroupChat: the third door onto the same household. The
// group chat id is a field on this page already, so the refusal names something the
// reader is looking at.
func TestSettingsRefusesOneEachWithNoGroupChat(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.signIn()
	cfg := h.writeConfig()
	// Isolated, because one assistant each is refused outright in simple mode and this
	// test is about the other refusal.
	cfg.Mode = config.ModeIsolated
	if err := setup.WriteConfig(h.configPath, cfg, true); err != nil {
		t.Fatal(err)
	}

	form := settingsForm(t, h)
	form.Set("agents", "per_member")
	form.Set("group_chat_id", "")
	resp := h.postCSRF("/settings", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the settings page accepted one assistant each with no group chat id: status = %d\n%s",
			resp.StatusCode, body(t, resp))
	}
	if page := body(t, resp); !strings.Contains(page, "no kenward in it") {
		t.Errorf("the refusal does not say what it costs:\n%s", page)
	}
	if after := readWrittenConfig(t, h.configPath); after.Household.Agents == "per_member" {
		t.Error("the refused value was written anyway")
	}
}

// TestWizardRefusesABotThatCannotHearTheGroup.
//
// Telegram turns bot privacy mode on for every new bot, and with it on the bot receives
// nothing sent in a group chat. Nothing arrives, so nothing is logged and the household
// is simply ignored — the worst failure shape there is for somebody who is not going to
// read a log. The dashboard has the token in hand at exactly the moment BotFather is
// still open in another tab, so it asks.
func TestWizardRefusesABotThatCannotHearTheGroup(t *testing.T) {
	h := newHarness(t)
	h.bot = &setup.BotInfo{Username: "casa_household_bot"} // privacy mode on
	token := h.issueToken()
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()
	for _, st := range []struct {
		path string
		form url.Values
	}{
		{"/setup/install", url.Values{"install": {"host"}, "household_name": {"Casa"}, "members": {"David\n"}}},
		{"/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}}},
	} {
		r := h.postCSRF(st.path, st.form)
		r.Body.Close()
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d", st.path, r.StatusCode)
		}
	}

	form := url.Values{
		"bot_token":     {"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		"bot_token_env": {"KENWARD_BOT_TOKEN"},
	}
	r := h.postCSRF("/setup/telegram", form)
	page := body(t, r)
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("the wizard accepted a bot that cannot see the group: status = %d\n%s", r.StatusCode, page)
	}
	for _, want := range []string{"privacy mode", "/setprivacy", "add it again"} {
		if !strings.Contains(page, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, page)
		}
	}
	// And the token is still in the box, like every other refusal on this page.
	if !strings.Contains(page, "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw") {
		t.Errorf("the refused token was discarded from the form:\n%s", page)
	}

	// A household that will never use a group chat says so and carries on. Refusing
	// them outright would be the wizard deciding something that is not its to decide.
	form.Set("no_group", "1")
	r2 := h.postCSRF("/setup/telegram", form)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusSeeOther {
		t.Fatalf("a household with no group chat could not get past the check: status = %d\n%s",
			r2.StatusCode, body(t, r2))
	}
}
