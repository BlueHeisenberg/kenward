package dashboard

import (
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// The dashboard's half of the identity question. The two things worth testing here are
// the two that would otherwise go wrong quietly: that the advanced step's answers reach
// the file at all, and that a settings page which never asks about a member's own
// persona does not silently delete one when it rewrites the file.

// TestWizardWritesIdentityAndPersona runs the whole wizard with the identity step
// answered, and checks the file rather than the form.
func TestWizardWritesIdentityAndPersona(t *testing.T) {
	h := newHarness(t)
	runPersonaWizard(t, h, "simple", url.Values{
		"persona_language":  {"Spanish"},
		"persona_tone":      {"warm"},
		"persona_character": {"Knows the house well."},
		"update_channel":    {"stable"},
		"idle_timeout":      {"0s"},
	})

	cfg := readWrittenConfig(t, h.configPath)
	if cfg.Household.Agents != config.AgentsShared {
		t.Errorf("household.agents = %q, want shared", cfg.Household.Agents)
	}
	want := config.PersonaConfig{Language: "Spanish", Tone: "warm", Character: "Knows the house well."}
	if cfg.Household.Persona != want {
		t.Errorf("household.persona = %+v, want %+v", cfg.Household.Persona, want)
	}
	// The wizard has no business writing anybody's personal persona: it is written by
	// the member, in Telegram, and an admin form that filled one in would be choosing
	// somebody's assistant for them.
	for _, m := range cfg.Members {
		if !m.Persona.IsZero() {
			t.Errorf("the wizard wrote a persona for %s: %+v", m.ID, m.Persona)
		}
	}
}

// TestWizardDefaultsToOneAssistant: an advanced step submitted without touching the
// identity question writes today's behaviour. Somebody who presses through the wizard
// without reading must get the household kenward has always had.
func TestWizardDefaultsToOneAssistant(t *testing.T) {
	h := newHarness(t)
	runPersonaWizard(t, h, "simple", url.Values{"update_channel": {"stable"}, "idle_timeout": {"0s"}})
	cfg := readWrittenConfig(t, h.configPath)
	if cfg.Household.Agents != config.AgentsShared {
		t.Errorf("household.agents = %q, want shared from an untouched question", cfg.Household.Agents)
	}
	if !cfg.Household.Persona.IsZero() {
		t.Errorf("household.persona = %+v, want empty", cfg.Household.Persona)
	}
}

// TestSettingsEditsKenwardsPersonaAndKeepsMembers is the one that matters after the
// first day. The settings page is where a household changes its mind about the language
// kenward answers in — an answer available only during setup is one people get wrong —
// and the page must not be able to overwrite what a member wrote about their own agent,
// because it never asked them.
func TestSettingsEditsKenwardsPersonaAndKeepsMembers(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.signIn()
	cfg := h.writeConfig()

	// A member with a persona of their own, as their Telegram tutorial would have
	// left it. The settings form below never mentions it.
	cfg.Household.Persona = config.PersonaConfig{Language: "English"}
	cfg.Members = []config.MemberConfig{{
		ID: "david", Name: "David",
		PrivateSpace: "00000000-0000-4000-8000-000000000001",
		Tiers:        []string{"local"},
		Persona:      config.PersonaConfig{AgentName: "Alfred", Tone: "very terse"},
	}}
	if err := setup.WriteConfig(h.configPath, cfg, true); err != nil {
		t.Fatal(err)
	}

	form := settingsForm(t, h)
	form.Set("persona_language", "Spanish")
	form.Set("persona_tone", "warm")
	form.Set("persona_character", "Knows the house well.")
	resp := h.postCSRF("/settings", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /settings: status = %d\n%s", resp.StatusCode, body(t, resp))
	}

	after := readWrittenConfig(t, h.configPath)
	if after.Household.Persona.Language != "Spanish" {
		t.Errorf("household.persona.language = %q, want Spanish", after.Household.Persona.Language)
	}
	want := config.PersonaConfig{AgentName: "Alfred", Tone: "very terse"}
	if after.Members[0].Persona != want {
		t.Errorf("the settings page overwrote %s's own persona: %+v, want %+v",
			after.Members[0].ID, after.Members[0].Persona, want)
	}
}

// TestSettingsRefusesAnOverlongPersona, in the words of somebody looking at the box
// rather than as a validation field path.
func TestSettingsRefusesAnOverlongPersona(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.signIn()
	h.writeConfig()

	form := settingsForm(t, h)
	form.Set("persona_character", strings.Repeat("x", config.MaxPersonaCharacter+1))
	resp := h.postCSRF("/settings", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if page := body(t, resp); !strings.Contains(page, "never trimmed to fit") {
		t.Errorf("the page refused the persona without saying why the limit exists:\n%s", page)
	}
}

// runPersonaWizard walks the whole first-run flow, with the advanced step's answers
// supplied by the caller. It is the shortest path to a written configuration, which is
// what these tests assert against — the form is not the product, the file is.
func runPersonaWizard(t *testing.T, h *harness, mode string, advanced url.Values) {
	t.Helper()
	token := h.issueToken()
	step := func(path string, form url.Values) {
		t.Helper()
		resp := h.postCSRF(path, form)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d, want 303\n%s", path, resp.StatusCode, body(t, resp))
		}
	}
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()

	step("/setup/install", url.Values{
		"install":        {"host"},
		"household_name": {"Casa"},
		"members":        {"David\n"},
	})
	step("/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}})
	step("/setup/telegram", url.Values{
		"bot_token":     {"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		"bot_token_env": {"KENWARD_BOT_TOKEN"},
	})
	step("/setup/endpoints", url.Values{
		"endpoint.0.name":     {"monster"},
		"endpoint.0.base_url": {"http://monster.tail:8000/v1"},
		"endpoint.0.model":    {"qwen3"},
		"endpoint.0.tiers":    {"local"},
	})
	step("/setup/trust", url.Values{"mode": {mode}})
	step("/setup/advanced", advanced)
	step("/setup/review", nil)
}

// settingsForm is the minimum a settings submission needs to be accepted, read off the
// configuration on disk. The page is a view of the file, and a form missing a member's
// tier chain is refused on purpose.
func settingsForm(t *testing.T, h *harness) url.Values {
	t.Helper()
	cfg := readWrittenConfig(t, h.configPath)
	form := url.Values{
		"household_name":   {cfg.Household.Name},
		"shared_space":     {cfg.Household.SharedSpace},
		"household_tiers":  {strings.Join(cfg.Household.Tiers, ", ")},
		"bot_token_env":    {cfg.Telegram.BotTokenEnv},
		"search_limit":     {"8"},
		"max_proposals":    {"1"},
		"history_reset":    {"0s"},
		"idle_timeout":     {"0s"},
		"update_channel":   {string(cfg.Update.Channel)},
		"persona_language": {cfg.Household.Persona.Language},
	}
	for i, m := range cfg.Members {
		form.Set("member."+itoa(i)+".name", m.Name)
		form.Set("member."+itoa(i)+".private_space", m.PrivateSpace)
		form.Set("member."+itoa(i)+".tiers", strings.Join(m.Tiers, ", "))
	}
	for i, e := range cfg.Endpoints {
		form.Set("endpoint."+itoa(i)+".name", e.Name)
		form.Set("endpoint."+itoa(i)+".base_url", e.BaseURL)
		form.Set("endpoint."+itoa(i)+".model", e.Model)
		form.Set("endpoint."+itoa(i)+".tags", strings.Join(e.Tags, ", "))
	}
	return form
}

func itoa(i int) string { return string(rune('0' + i)) }

func readWrittenConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the written configuration: %v", err)
	}
	defer f.Close()
	cfg, err := config.Decode(f)
	if err != nil {
		t.Fatalf("the written configuration does not parse: %v", err)
	}
	return cfg
}

// TestWizardRefusesOneEachInSimpleMode and TestSettingsRefusesOneEachInSimpleMode are
// the two places an admin can ask for an arrangement this mode cannot deliver.
//
// Refused rather than downgraded, in both, and refused where the clicking happened
// rather than by config.Validate three screens later. The failure a downgrade would
// produce is the silent kind: every member's private chat resolving to kenward's, and
// a household that asked for their own assistants getting kenward under several names
// with no way to tell.
func TestWizardRefusesOneEachInSimpleMode(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()
	post := func(path string, form url.Values) *http.Response {
		t.Helper()
		return h.postCSRF(path, form)
	}
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()
	for _, st := range []struct {
		path string
		form url.Values
	}{
		{"/setup/install", url.Values{"install": {"host"}, "household_name": {"Casa"}, "members": {"David\n"}}},
		{"/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}}},
		{"/setup/telegram", url.Values{"bot_token": {"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"}, "bot_token_env": {"KENWARD_BOT_TOKEN"}}},
		{"/setup/endpoints", url.Values{"endpoint.0.name": {"monster"}, "endpoint.0.base_url": {"http://monster.tail:8000/v1"}, "endpoint.0.model": {"qwen3"}, "endpoint.0.tiers": {"local"}}},
		{"/setup/trust", url.Values{"mode": {"simple"}}},
	} {
		r := post(st.path, st.form)
		r.Body.Close()
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d", st.path, r.StatusCode)
		}
	}
	r := post("/setup/advanced", url.Values{
		"agents": {"per_member"}, "update_channel": {"stable"}, "idle_timeout": {"0s"},
	})
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("the wizard accepted one agent each in simple mode: status = %d", r.StatusCode)
	}
	if page := body(t, r); !strings.Contains(page, "two agents behind one contact are one agent") {
		t.Errorf("the wizard refused without the reason:\n%s", page)
	}
}

func TestSettingsRefusesOneEachInSimpleMode(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.signIn()
	h.writeConfig()

	form := settingsForm(t, h)
	form.Set("agents", "per_member")
	resp := h.postCSRF("/settings", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the settings page accepted one agent each in simple mode: status = %d", resp.StatusCode)
	}
	if page := body(t, resp); !strings.Contains(page, "two agents behind one contact are one agent") {
		t.Errorf("the settings page refused without the reason:\n%s", page)
	}
	if after := readWrittenConfig(t, h.configPath); after.Household.Agents == config.AgentsPerMember {
		t.Error("the refused value was written anyway")
	}
}

// TestWizardWritesOneEach is the identity question's other answer reaching the file.
//
// Linux only, and not because the arrangement is: the dashboard's wizard refuses
// isolated mode anywhere it cannot be delivered, and one agent each needs isolated
// mode for its per-member bots. There is no other combination in which this value can
// be written, so there is no version of this test that runs on Windows.
func TestWizardWritesOneEach(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("isolated mode, and therefore one agent each, needs Linux with Podman or Docker")
	}
	h := newHarness(t)
	runPersonaWizard(t, h, "isolated", url.Values{
		"agents":         {"per_member"},
		"update_channel": {"stable"},
		"idle_timeout":   {"0s"},
	})
	cfg := readWrittenConfig(t, h.configPath)
	if cfg.Household.Agents != config.AgentsPerMember {
		t.Errorf("household.agents = %q, want per_member", cfg.Household.Agents)
	}
	for _, m := range cfg.Members {
		if !m.Persona.IsZero() {
			t.Errorf("the wizard wrote a persona for %s: %+v", m.ID, m.Persona)
		}
	}
}
