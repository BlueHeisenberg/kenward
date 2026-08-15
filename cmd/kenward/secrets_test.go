package main

import (
	"os"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// fileTokenYAML supplies the household bot token as a file rather than a variable.
// This is the shape deploy/kenward.service now ships, so it is the documented Linux
// install and it has to work.
const fileTokenYAML = `mode: simple
household:
  name: Casa
  shared_space: household
  group_chat_id: -1001234567890
  tiers: [local]
telegram:
  bot_token_file: /etc/kenward/bot-token
members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: david-private
    tiers: [local]
endpoints:
  - name: monster
    base_url: http://monster.tail:8000/v1
    model: qwen3.6-27b-awq
    tags: [local]
memory:
  lore_command: [lore, mcp]
`

// TestDoctorReadsATokenFromAFile.
//
// The bug this guards against: validation gained file and credential sources while
// the runtime still read the environment directly, so a household that supplied its
// token only as bot_token_file passed validation and then found no token where it was
// needed. `doctor` said the configuration was fine and the node would not start.
func TestDoctorReadsATokenFromAFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fileTokenYAML, map[string]string{})
	h.e.secretOpts = config.SecretOptions{
		LookupEnv: lookup(nil),
		FS:        fakeSecretFS{"/etc/kenward/bot-token": {data: fakeBotToken + "\n", mode: 0o600}},
		FileMode:  config.FileModeEnforce,
	}

	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d, want 0: the token is supplied as a file\n%s", code, h.both())
	}
	out := h.stdout()
	if !strings.Contains(out, "Telegram authorises as @our_household_bot") {
		t.Errorf("the token from the file was not used:\n%s", out)
	}
	// Provenance is exactly what an operator debugging a failed start needs.
	if !strings.Contains(out, "token from file /etc/kenward/bot-token") {
		t.Errorf("doctor does not say where the token came from:\n%s", out)
	}
	if !strings.Contains(out, "every secret the configuration names can be read") {
		t.Errorf("the configuration section did not accept the file source:\n%s", out)
	}
	h.assertNoSecrets(t)
}

// TestDoctorReportsAnUnreadableTokenFile.
//
// A token file at 0644 is refused with the mode in the message. Surfacing that as a
// finding is the whole point of doctor: the alternative is a node that will not start
// and an operator with no idea why.
func TestDoctorReportsAnUnreadableTokenFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fileTokenYAML, map[string]string{})
	h.e.secretOpts = config.SecretOptions{
		LookupEnv: lookup(nil),
		FS:        fakeSecretFS{"/etc/kenward/bot-token": {data: fakeBotToken, mode: 0o644}},
		FileMode:  config.FileModeEnforce,
	}

	code := h.run("doctor")
	if code == exitOK {
		t.Fatalf("exit = 0; a token that cannot be read is a fault\n%s", h.both())
	}
	out := h.stdout()
	if !strings.Contains(out, "/etc/kenward/bot-token") {
		t.Errorf("the finding does not name the file:\n%s", out)
	}
	if !strings.Contains(out, "644") {
		t.Errorf("the finding does not give the mode, which is the thing to fix:\n%s", out)
	}
	// It is a report, not a crash: every other section still ran.
	for _, section := range []string{"Memory", "Sessions", "Endpoints", "Privacy"} {
		if !strings.Contains(out, section) {
			t.Errorf("doctor stopped early and never printed %s:\n%s", section, out)
		}
	}
	h.assertNoSecrets(t)
}

// TestDoctorReadsATokenFromASystemdCredential.
//
// deploy/kenward.service ships LoadCredential= and no EnvironmentFile=, so this is
// the path the documented Linux install actually takes.
func TestDoctorReadsATokenFromASystemdCredential(t *testing.T) {
	t.Parallel()
	// Neither bot_token_file nor bot_token_env: the credential is found automatically.
	credYAML := strings.Replace(fileTokenYAML,
		"  bot_token_file: /etc/kenward/bot-token\n", "", 1)
	credYAML = strings.Replace(credYAML, "telegram:\n", "telegram: {}\n", 1)

	h := newHarness(t, credYAML, map[string]string{})
	h.e.secretOpts = config.SecretOptions{
		LookupEnv:      lookup(nil),
		CredentialsDir: "/run/credentials/kenward.service",
		FS: fakeSecretFS{
			"/run/credentials/kenward.service/" + config.CredentialBotToken: {data: fakeBotToken, mode: 0o400},
		},
		FileMode: config.FileModeEnforce,
	}

	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d, want 0: the token is supplied as a systemd credential\n%s", code, h.both())
	}
	if !strings.Contains(h.stdout(), "token from systemd credential "+config.CredentialBotToken) {
		t.Errorf("doctor does not report the credential as the source:\n%s", h.stdout())
	}
	h.assertNoSecrets(t)
}

// TestHealthReadsTheTokenTheSameWay.
//
// The update health check was the other call site reading the environment directly.
// A node whose token is a file would have failed its own post-swap health check and
// rolled back a perfectly good update — repeatedly.
func TestHealthReadsTheTokenTheSameWay(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fileTokenYAML, map[string]string{})
	h.e.secretOpts = config.SecretOptions{
		LookupEnv: lookup(nil),
		FS:        fakeSecretFS{"/etc/kenward/bot-token": {data: fakeBotToken, mode: 0o600}},
		FileMode:  config.FileModeEnforce,
	}
	cfg := mustLoadWith(t, fileTokenYAML, h.e.secrets())

	if err := nodeHealthProbes(h.e, cfg, unitSelection{}).Telegram(h.e.context()); err != nil {
		t.Fatalf("health rejected a node whose token is a file: %v", err)
	}
}

// TestHealthErrorNamesTheSourceNotTheValue.
func TestHealthErrorNamesTheSourceNotTheValue(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fileTokenYAML, map[string]string{})
	h.e.secretOpts = config.SecretOptions{
		LookupEnv: lookup(nil),
		FS:        fakeSecretFS{"/etc/kenward/bot-token": {data: fakeBotToken, mode: 0o600}},
		FileMode:  config.FileModeEnforce,
	}
	h.e.probes = telegramRefuses(healthyProbes())
	cfg := mustLoadWith(t, fileTokenYAML, h.e.secrets())

	err := nodeHealthProbes(h.e, cfg, unitSelection{}).Telegram(h.e.context())
	if err == nil {
		t.Fatal("health passed with Telegram refusing the token")
	}
	if !strings.Contains(err.Error(), "/etc/kenward/bot-token") {
		t.Errorf("the error does not say which source the token came from: %v", err)
	}
	if strings.Contains(err.Error(), fakeBotToken) {
		t.Fatalf("the error carries the token itself")
	}
}

// TestNoSecretSourceAtAllIsReported: nothing stated and no credential present.
func TestNoSecretSourceAtAllIsReported(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, map[string]string{})
	h.e.secretOpts = config.SecretOptions{LookupEnv: lookup(nil)}

	if code := h.run("doctor"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	out := h.stdout()
	if !strings.Contains(out, "telegram.bot_token") {
		t.Errorf("the report does not name the secret by its configuration path:\n%s", out)
	}
	// Named by path, not by variable: a household using a file never set a variable
	// and should not be told one is missing.
	if !strings.Contains(out, "no readable value") {
		t.Errorf("the report does not say the secret has no value:\n%s", out)
	}
}

// mustLoadWith loads a configuration against a given resolver.
func mustLoadWith(t *testing.T, yaml string, secrets *config.Secrets) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/kenward.yaml"
	if err := writeFileForTest(path, yaml); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path, dir+"/data", secrets)
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	return cfg
}

func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
