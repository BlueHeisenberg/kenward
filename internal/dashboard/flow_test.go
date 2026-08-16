package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// TestFirstRunWritesAHouseholdThatKenwardWillLoad walks the whole wizard and then loads
// what it wrote with the real loader.
//
// The assertion at the end is the one that matters. A wizard that writes a file its own
// loader refuses has failed at the only thing it was for, and no amount of asserting on
// intermediate redirects would catch that — so the last step is config.Decode plus
// Validate, against the environment the wizard told the operator to create.
func TestFirstRunWritesAHouseholdThatKenwardWillLoad(t *testing.T) {
	h := newHarness(t)
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
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("exchanging the token: status = %d", resp.StatusCode)
	}

	step("/setup/install", url.Values{
		"install":        {"host"},
		"household_name": {"Casa"},
		"members":        {"David\nJordan\n"},
	})
	step("/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}})
	step("/setup/telegram", url.Values{
		"bot_token":      {"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		"bot_token_env":  {"KENWARD_BOT_TOKEN"},
		"write_env_file": {"1"},
	})
	step("/setup/endpoints", url.Values{
		"endpoint.0.name":     {"monster"},
		"endpoint.0.base_url": {"http://monster.tail:8000/v1"},
		"endpoint.0.model":    {"qwen3"},
		"endpoint.0.tiers":    {"local"},
		"endpoint.0.api_key":  {""},
	})
	step("/setup/trust", url.Values{"mode": {"simple"}})
	step("/setup/advanced", url.Values{
		"search_limit":   {"8"},
		"max_proposals":  {"1"},
		"history_reset":  {"6h"},
		"idle_timeout":   {"0s"},
		"update_channel": {"stable"},
	})
	step("/setup/review", nil)

	// Three spaces: the household's and one per person, each created for real by the
	// fake and each with a distinct id.
	if len(h.lore.created) != 3 {
		t.Fatalf("lore spaces created = %v, want three (household, David, Jordan)", h.lore.created)
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

	env := func(name string) (string, bool) {
		if name == "KENWARD_BOT_TOKEN" {
			return "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", true
		}
		return "", false
	}
	if err := cfg.Validate(env); err != nil {
		t.Fatalf("the wizard wrote a configuration kenward refuses:\n%v", err)
	}

	if cfg.Household.Name != "Casa" {
		t.Errorf("household = %q, want Casa", cfg.Household.Name)
	}
	if len(cfg.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(cfg.Members))
	}
	// Every space is distinct, and no member shares the household's. Getting this
	// wrong publishes somebody's private memory to the whole house.
	seen := map[string]bool{cfg.Household.SharedSpace: true}
	for _, m := range cfg.Members {
		if m.PrivateSpace == "" || seen[m.PrivateSpace] {
			t.Fatalf("member %s has space %q, which is empty or already taken", m.ID, m.PrivateSpace)
		}
		seen[m.PrivateSpace] = true
	}
	// The default is local-only, and the wizard was not asked to widen it.
	local := setup.LocalTiers(cfg.Endpoints)
	for _, m := range cfg.Members {
		if !setup.StaysHome(local, m.Tiers) {
			t.Errorf("%s's chain %v reaches a provider, and nobody asked for that", m.ID, m.Tiers)
		}
	}

	// The advanced step's conversation reset reached the file. It is the one answer
	// on that page whose effect a member sees in a chat rather than an operator sees
	// in a log, so a form field that quietly went nowhere would be found by the
	// household rather than by anybody here.
	if got, want := cfg.History.ResetEvery.Duration(), 6*time.Hour; got != want {
		t.Errorf("history.reset_every = %v, want %v: the advanced step's answer did not reach the file", got, want)
	}
	// And it did not land on the neighbouring duration, which would stop David's
	// assistant answering rather than shorten his conversation.
	if got := cfg.Session.IdleTimeout.Duration(); got != 0 {
		t.Errorf("session.idle_timeout = %v, want 0: the conversation reset was written to the wrong key", got)
	}

	// The dashboard turned itself on, on loopback, and nothing wider.
	if !cfg.Dashboard.Enabled {
		t.Error("the wizard wrote a configuration in which the dashboard it was run from is off")
	}
	if cfg.Dashboard.ExposureOrDefault() != config.ExposureLoopback || !cfg.Dashboard.Loopback() {
		t.Errorf("dashboard exposure = %q bind = %q; first run must never open anything wider than loopback",
			cfg.Dashboard.ExposureOrDefault(), cfg.Dashboard.BindAddr())
	}
}

// TestTheBotTokenNeverReachesTheConfiguration.
//
// It goes to the .env file, which is the whole rule kenward.yaml is written to. A wizard
// that leaked one into the configuration would leak it into whatever backup or repo the
// household keeps that file in.
func TestTheBotTokenNeverReachesTheConfiguration(t *testing.T) {
	h := newHarness(t)
	const token = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	runFirstRun(t, h, token)

	data, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("the bot token was written into kenward.yaml")
	}
	if !strings.Contains(string(data), "KENWARD_BOT_TOKEN") {
		t.Fatal("the configuration does not name the variable the token is read from")
	}
}

// runFirstRun drives the wizard to completion, for tests that are about what it left
// behind rather than about the flow itself.
func runFirstRun(t *testing.T, h *harness, botToken string) {
	t.Helper()
	token := h.issueToken()
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()

	for _, s := range []struct {
		path string
		form url.Values
	}{
		{"/setup/install", url.Values{"install": {"host"}, "household_name": {"Casa"}, "members": {"David"}}},
		{"/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}}},
		{"/setup/telegram", url.Values{"bot_token": {botToken}, "bot_token_env": {"KENWARD_BOT_TOKEN"}, "write_env_file": {"1"}}},
		{"/setup/endpoints", url.Values{
			"endpoint.0.name": {"monster"}, "endpoint.0.base_url": {"http://monster.tail:8000/v1"},
			"endpoint.0.model": {"qwen3"}, "endpoint.0.tiers": {"local"},
		}},
		{"/setup/trust", url.Values{"mode": {"simple"}}},
		{"/setup/advanced", url.Values{"update_channel": {"stable"}, "idle_timeout": {"0s"}}},
		{"/setup/review", nil},
	} {
		r := h.postCSRF(s.path, s.form)
		code := r.StatusCode
		b := body(t, r)
		r.Body.Close()
		if code != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d\n%s", s.path, code, b)
		}
	}
}

// TestEndpointProbeReadsTheContextWindowRatherThanAskingForIt.
//
// vLLM publishes max_model_len on /v1/models, and it is the number that actually binds —
// far more often than not it is well below what the model card advertises. Reading it is
// the difference between a household that gets its window and one that gets kenward's
// cautious default forever.
func TestEndpointProbeReadsTheContextWindowRatherThanAskingForIt(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()
	step := url.Values{"install": {"host"}, "household_name": {"Casa"}, "members": {"David"}}
	resp = h.postCSRF("/setup/install", step)
	resp.Body.Close()
	resp = h.postCSRF("/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}})
	resp.Body.Close()
	resp = h.postCSRF("/setup/telegram", url.Values{"bot_token_env": {"KENWARD_BOT_TOKEN"}})
	resp.Body.Close()

	resp = h.postCSRF("/setup/endpoints", url.Values{
		"action":              {"probe"},
		"endpoint.0.name":     {"monster"},
		"endpoint.0.base_url": {"http://monster.tail:8000/v1"},
		"endpoint.0.model":    {"test-model"},
		"endpoint.0.tiers":    {"local"},
	})
	defer resp.Body.Close()
	page := body(t, resp)
	if !strings.Contains(page, "262144") {
		t.Fatalf("the probe did not report the window the endpoint published:\n%s", page)
	}
}

// TestAddingAMemberDoesAllThreeThings: creates the space, writes the configuration, mints
// the code — and the configuration it leaves behind is one kenward will load.
//
// The three used to be three commands, and the ordinary failure was a member declared
// against a space that did not exist, found weeks later when their first retrieval came
// back empty. So the space is asserted to be real, in the fake's own listing, and named
// by the configuration.
func TestAddingAMemberDoesAllThreeThings(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	resp := h.postCSRF("/members/add", url.Values{"name": {"Jordan"}, "tiers": {"local"}})
	defer resp.Body.Close()
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d\n%s", resp.StatusCode, page)
	}

	if len(h.minted) != 1 {
		t.Fatalf("claim codes minted = %v, want one", h.minted)
	}
	if !strings.Contains(page, h.minted[0]) {
		t.Fatal("the claim code was minted and not shown; it exists in the clear exactly once")
	}
	if !h.lore.closed {
		t.Error("the lore client was left open")
	}

	f, err := os.Open(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := config.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := cfg.MemberByID("jordan")
	if !ok {
		t.Fatalf("jordan is not in the written configuration: %+v", cfg.Members)
	}
	// The space named is one lore actually holds.
	spaces, _ := h.lore.Spaces(t.Context())
	var found bool
	for _, s := range spaces {
		if s.ID == string(m.Private) {
			found = true
		}
	}
	if !found {
		t.Fatalf("jordan's private_space %q is not a space lore holds", m.Private)
	}
	if err := cfg.ValidateForUnit(nil, config.UnitScope{NoSecrets: true}); err != nil {
		t.Fatalf("adding a member produced a configuration kenward refuses:\n%v", err)
	}
}

// TestAddingAMemberWithNoTierChainIsRefused.
//
// There is no default, anywhere, and this is the surface most likely to grow one: a web
// form with an empty checkbox group is exactly where somebody reaches for "well, use the
// household's". A member's chain is their privacy policy and nothing chooses it for them.
func TestAddingAMemberWithNoTierChainIsRefused(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	resp := h.postCSRF("/members/add", url.Values{"name": {"Jordan"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(h.minted) != 0 {
		t.Fatal("a code was minted for a member who was not added")
	}
	if len(h.lore.created) != 0 {
		t.Fatal("a lore space was created for a member who was not added; lore cannot remove one")
	}
}

// TestAMemberIsNotWrittenWhenLoreCannotMakeTheirSpace.
//
// The order matters and this pins it: the space is created first, so a lore that is not
// there leaves the configuration untouched. The other order leaves a member declared
// against nothing, which validates, starts, and returns empty on every read.
func TestAMemberIsNotWrittenWhenLoreCannotMakeTheirSpace(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	before := h.writeConfig()
	h.signIn()
	h.lore.createErr = errors.New("lore did not answer")

	resp := h.postCSRF("/members/add", url.Values{"name": {"Jordan"}, "tiers": {"local"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	f, _ := os.Open(h.configPath)
	defer f.Close()
	cfg, err := config.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Members) != len(before.Members) {
		t.Fatalf("members = %d, want %d: a member was written without a space",
			len(cfg.Members), len(before.Members))
	}
}

// TestSettingsRefusesAnEditThatWouldBreakTheHousehold, and writes nothing when it does.
func TestSettingsRefusesAnEditThatWouldBreakTheHousehold(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()
	original, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}

	// A tier chain naming a tier no endpoint serves: a chain that can only ever
	// refuse, and one of the faults config.Validate exists to catch.
	resp := h.postCSRF("/settings", url.Values{
		"household_name":  {"Home"},
		"shared_space":    {"00000000-0000-4000-8000-000000000099"},
		"household_tiers": {"nowhere"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", resp.StatusCode, body(t, resp))
	}
	after, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("a refused edit was written to disk anyway")
	}
}

// TestSettingsKeepsFieldsTheFormNeverAsksAbout.
//
// A member's telegram_id is not on the settings page — it is a recorded fact, not a
// setting — and an edit that dropped it would unbind somebody who is happily using
// kenward, silently, because they changed a search limit.
func TestSettingsKeepsFieldsTheFormNeverAsksAbout(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	cfg := h.writeConfig()
	cfg.Members = append(cfg.Members, config.MemberConfig{
		ID: "david", Name: "David", TelegramID: 100200301,
		PrivateSpace: "00000000-0000-4000-8000-000000000001", Tiers: []string{"local"},
	})
	if err := setup.WriteConfig(h.configPath, cfg, true); err != nil {
		t.Fatal(err)
	}
	h.signIn()

	resp := h.postCSRF("/settings", url.Values{
		"household_name":         {"Home"},
		"shared_space":           {"00000000-0000-4000-8000-000000000099"},
		"household_tiers":        {"local"},
		"member.0.name":          {"David"},
		"member.0.private_space": {"00000000-0000-4000-8000-000000000001"},
		"member.0.tiers":         {"local"},
		"search_limit":           {"12"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d\n%s", resp.StatusCode, body(t, resp))
	}

	f, _ := os.Open(h.configPath)
	defer f.Close()
	out, err := config.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if out.Members[0].TelegramID != 100200301 {
		t.Fatalf("telegram_id = %d after an unrelated edit, want 100200301", out.Members[0].TelegramID)
	}
	if out.Memory.SearchLimit != 12 {
		t.Fatalf("search_limit = %d, want 12", out.Memory.SearchLimit)
	}
}

// TestChoosingLANExposureGeneratesACertificateAndShowsItsFingerprint.
//
// LAN requires TLS and validation refuses it without, so this asserts both halves: the
// pair exists on disk and the configuration names it, and the fingerprint is on the page
// — because a self-signed certificate whose fingerprint was never shown is a warning
// clicked through, which is worth nothing.
func TestChoosingLANExposureGeneratesACertificateAndShowsItsFingerprint(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	resp := h.postCSRF("/exposure", url.Values{
		"exposure": {"lan"},
		"address":  {"192.168.1.20"},
		"port":     {"8770"},
	})
	defer resp.Body.Close()
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d\n%s", resp.StatusCode, page)
	}

	f, _ := os.Open(h.configPath)
	defer f.Close()
	cfg, err := config.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Dashboard.TLS() {
		t.Fatal("lan exposure was written without a certificate")
	}
	for _, p := range []string{cfg.Dashboard.TLSCertFile, cfg.Dashboard.TLSKeyFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if err := cfg.ValidateForUnit(nil, config.UnitScope{NoSecrets: true}); err != nil {
		t.Fatalf("the written configuration is one kenward refuses:\n%v", err)
	}
	if !strings.Contains(page, "Check this fingerprint") || !strings.Contains(page, ":") {
		t.Fatalf("the fingerprint was not shown:\n%s", page)
	}

	// The key is not readable by anybody else. Windows has no mode bits worth the
	// name, so this is asserted where it means something — which is the platform
	// every household actually runs a LAN-exposed dashboard on.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(cfg.Dashboard.TLSKeyFile); err == nil && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("the private key is mode %v", info.Mode().Perm())
		}
	}
}

// TestExposureCannotBeSetToSomethingKenwardRefuses.
func TestExposureCannotBeSetToSomethingKenwardRefuses(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	for _, form := range []url.Values{
		{"exposure": {"everywhere"}},
		{"exposure": {"tailnet"}}, // no address chosen
		{"exposure": {"lan"}, "address": {"192.168.1.20"}, "port": {"not-a-port"}},
	} {
		resp := h.postCSRF("/exposure", form)
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400", form, code)
		}
	}

	f, _ := os.Open(h.configPath)
	defer f.Close()
	cfg, err := config.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dashboard.ExposureOrDefault() != config.ExposureLoopback {
		t.Fatalf("exposure = %q after three refused edits", cfg.Dashboard.ExposureOrDefault())
	}
}

// TestTheOverviewPrintsThePrivacyStatementVerbatim.
//
// The same words `kenward doctor` prints, from internal/privacy, because that promise is
// only worth something if the two strings are identical. A dashboard that paraphrased it
// would be the second copy this whole arrangement exists to prevent.
func TestTheOverviewPrintsThePrivacyStatementVerbatim(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	resp := h.get("/overview")
	defer resp.Body.Close()
	page := body(t, resp)
	// html/template escapes the apostrophes, so the comparison is made on a
	// distinctive clause that survives escaping intact.
	const clause = "seal anything against whoever runs this machine"
	if !strings.Contains(page, clause) {
		t.Fatalf("the overview does not carry the simple-mode privacy statement:\n%s", page)
	}
	if !strings.Contains(page, "The admin dashboard is listening on") {
		t.Fatalf("the overview does not say where the dashboard is listening:\n%s", page)
	}
}
