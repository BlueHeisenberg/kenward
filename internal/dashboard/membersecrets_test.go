package dashboard

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// The dashboard wizard's isolated path used to name four secrets it had never asked
// for. `per_member` + `isolated` completed, wrote a members list carrying a
// bot_token_env and a passphrase_env each, wrote a .env holding only the household
// token, and landed on Overview saying "Restart kenward for it to take effect, then
// invite the household." — an instruction that cannot succeed, because
// config.secretRefs demands every member's token and passphrase of an unscoped
// isolated node whether or not that member has claimed an invite yet.
//
// `kenward doctor` caught it. Nothing in the wizard told anybody to run doctor, which
// is the same gap household.group_chat_id was closed against: ask, and refuse a blank.

// wizardToBots walks the flow as far as the per-member secrets step, in isolated mode,
// with one assistant each on the way.
func wizardToBots(t *testing.T, h *harness) {
	t.Helper()
	token := h.issueToken()
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()
	for _, st := range []struct {
		path string
		form url.Values
	}{
		{"/setup/install", url.Values{"install": {"host"}, "household_name": {"Casa"}, "members": {"David\nRobin\n"}}},
		{"/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}}},
		{"/setup/telegram", url.Values{
			"bot_token":      {"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
			"bot_token_env":  {"KENWARD_BOT_TOKEN"},
			"write_env_file": {"1"},
		}},
		{"/setup/endpoints", url.Values{
			"endpoint.0.name": {"monster"}, "endpoint.0.base_url": {"http://monster.tail:8000/v1"},
			"endpoint.0.model": {"qwen3"}, "endpoint.0.tiers": {"local"},
		}},
		{"/setup/trust", url.Values{"mode": {"isolated"}}},
	} {
		r := h.postCSRF(st.path, st.form)
		r.Body.Close()
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d, want 303", st.path, r.StatusCode)
		}
	}
}

// TestIsolatedWizardAsksForEachMembersOwnSecrets: the step exists, and it names both
// people and both of their variables.
func TestIsolatedWizardAsksForEachMembersOwnSecrets(t *testing.T) {
	h := newHarness(t)
	wizardToBots(t, h)

	resp := h.get("/setup/bots")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /setup/bots: status = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	for _, want := range []string{
		"KENWARD_BOT_TOKEN_DAVID", "KENWARD_PASSPHRASE_DAVID",
		"KENWARD_BOT_TOKEN_ROBIN", "KENWARD_PASSPHRASE_ROBIN",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the step does not name %s:\n%s", want, page)
		}
	}
}

// TestIsolatedWizardRefusesAMemberWithNoSecrets: a blank is refused, by name, the way
// a blank group chat id already is.
//
// Not a warning: doctor already warns, and the operator who follows the wizard's own
// closing sentence never runs doctor.
func TestIsolatedWizardRefusesAMemberWithNoSecrets(t *testing.T) {
	h := newHarness(t)
	wizardToBots(t, h)

	resp := h.postCSRF("/setup/bots", url.Values{
		"member.david.bot_token":  {"111111111:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		"member.david.passphrase": {"david's own passphrase"},
		// Robin's two boxes left empty, which is what the wizard used to do to all
		// four of them without asking.
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the wizard accepted an isolated household missing a member's own secrets: status = %d", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "Robin") {
		t.Errorf("the refusal does not say who is missing:\n%s", page)
	}
	// And what was typed for David survives, the way a refused token already does.
	if !strings.Contains(page, "111111111:") {
		t.Errorf("the refusal discarded the answers that were given:\n%s", page)
	}
}

// TestIsolatedWizardRefusesSecretsItCannotDeliver.
//
// The .env beside the configuration is the only place this wizard can put a secret —
// kenward.yaml never holds one — so with that box unticked, four typed secrets are four
// secrets thrown away and a household exactly as unstartable as before. Refused where
// the typing would have happened, with a remedy the Back control can reach.
func TestIsolatedWizardRefusesSecretsItCannotDeliver(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()
	for _, st := range []struct {
		path string
		form url.Values
	}{
		{"/setup/install", url.Values{"install": {"host"}, "household_name": {"Casa"}, "members": {"David\n"}}},
		{"/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}}},
		// No write_env_file: the operator turned it off.
		{"/setup/telegram", url.Values{
			"bot_token": {"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"}, "bot_token_env": {"KENWARD_BOT_TOKEN"},
		}},
		{"/setup/endpoints", url.Values{
			"endpoint.0.name": {"monster"}, "endpoint.0.base_url": {"http://monster.tail:8000/v1"},
			"endpoint.0.model": {"qwen3"}, "endpoint.0.tiers": {"local"},
		}},
		{"/setup/trust", url.Values{"mode": {"isolated"}}},
	} {
		r := h.postCSRF(st.path, st.form)
		r.Body.Close()
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d, want 303", st.path, r.StatusCode)
		}
	}

	bad := h.postCSRF("/setup/bots", url.Values{
		"member.david.bot_token":  {"111111111:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		"member.david.passphrase": {"david-passphrase-one"},
	})
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("the wizard took secrets it has nowhere to write: status = %d", bad.StatusCode)
	}
	page := body(t, bad)
	if !strings.Contains(page, "nowhere to go") {
		t.Errorf("the refusal does not say why:\n%s", page)
	}
	if !strings.Contains(page, `href="/setup/telegram"`) && !strings.Contains(page, "The Telegram bot") {
		t.Errorf("the refusal names no remedy this page can reach:\n%s", page)
	}
}

// TestSimpleModeNeverSeesTheBotsStep: one bot for the household, so there is nothing
// to ask, and the step is not in the flow at all.
func TestSimpleModeNeverSeesTheBotsStep(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "simple")

	resp := h.get("/setup/bots")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /setup/bots in simple mode: status = %d, want 303 onwards", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/setup/advanced" {
		t.Errorf("Location = %q, want /setup/advanced", got)
	}
	// And Back from the identity step does not bounce off it forever.
	page := h.get("/setup/advanced")
	defer page.Body.Close()
	if b := body(t, page); strings.Contains(b, `href="/setup/bots"`) {
		t.Errorf("simple mode offers Back to a step it never had:\n%s", b)
	}
}

// TestIsolatedWizardWritesAStartableHousehold is the one that matters.
//
// It walks the flagship privacy mode's happy path to the end, then judges the file the
// wizard wrote against the environment the wizard itself created — the .env beside it,
// and nothing else. That is exactly what the operator has after following the closing
// sentence, and it either starts kenward or it does not.
func TestIsolatedWizardWritesAStartableHousehold(t *testing.T) {
	h := newHarness(t)
	wizardToBots(t, h)

	step := func(path string, form url.Values) {
		t.Helper()
		resp := h.postCSRF(path, form)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d, want 303\n%s", path, resp.StatusCode, body(t, resp))
		}
	}
	step("/setup/bots", url.Values{
		"member.david.bot_token":  {"111111111:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		"member.david.passphrase": {"david-passphrase-one"},
		"member.robin.bot_token":  {"222222222:BBHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		"member.robin.passphrase": {"robin-passphrase-two"},
	})
	step("/setup/advanced", url.Values{
		"agents":         {"per_member"},
		"group_chat_id":  {"-1001234567890"},
		"update_channel": {"off"},
		"idle_timeout":   {"0s"},
	})
	// Review restates them before they are committed, the way it restates the identity
	// answer: four variables the household has to keep alive are four things to see
	// once on a screen headed "Check, then write it".
	review := h.get("/setup/review")
	page := body(t, review)
	review.Body.Close()
	for _, want := range []string{"KENWARD_BOT_TOKEN_DAVID", "KENWARD_PASSPHRASE_ROBIN"} {
		if !strings.Contains(page, want) {
			t.Errorf("review does not restate %s:\n%s", want, page)
		}
	}

	// The closing sentence, which is the one the operator follows. It used to say
	// "Restart kenward for it to take effect, then invite the household." with four
	// secrets missing and no mention of them anywhere.
	last := h.postCSRF("/setup/review", nil)
	location := last.Header.Get("Location")
	last.Body.Close()
	if last.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /setup/review: status = %d, want 303", last.StatusCode)
	}
	closing, err := url.QueryUnescape(strings.TrimPrefix(location, "/overview?ok="))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(closing, ".env") {
		t.Errorf("the closing sentence does not say where the secrets went, or that they have to be loaded: %q", closing)
	}

	f, err := os.Open(h.configPath)
	if err != nil {
		t.Fatalf("the wizard reported success and wrote nothing: %v", err)
	}
	defer f.Close()
	cfg, err := config.Decode(f)
	if err != nil {
		t.Fatalf("the wizard wrote a file that does not parse: %v", err)
	}

	env := envFileVars(t, h)
	if err := cfg.Validate(func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}); err != nil {
		t.Fatalf("the wizard wrote a household that will not start from the .env it wrote beside it:\n%v", err)
	}

	// Every member's own two variables are named in the file and present in the .env,
	// and no two members share one. One passphrase across two members is simple
	// mode's key custody wearing isolated mode's name.
	seen := map[string]bool{}
	for _, m := range cfg.Members {
		for _, name := range []string{m.BotTokenEnv, m.PassphraseEnv} {
			if name == "" {
				t.Fatalf("member %s has no own secret variable in an isolated household", m.ID)
			}
			if env[name] == "" {
				t.Errorf("%s is named by the configuration and has no value in the .env beside it", name)
			}
			if seen[env[name]] {
				t.Errorf("%s repeats a value another member already has", name)
			}
			seen[env[name]] = true
		}
	}

	// And none of it reached the configuration.
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"david-passphrase-one", "robin-passphrase-two", "111111111:", "222222222:"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("%q was written into kenward.yaml", secret)
		}
	}
}

// envFileVars reads the .env the wizard wrote beside the configuration.
func envFileVars(t *testing.T, h *harness) map[string]string {
	t.Helper()
	path := strings.TrimSuffix(h.configPath, "kenward.yaml") + ".env"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the wizard collected secrets and wrote no .env: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) > 1 && strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
			value = strings.ReplaceAll(value[1:len(value)-1], `'\''`, "'")
		}
		out[name] = value
	}
	return out
}
