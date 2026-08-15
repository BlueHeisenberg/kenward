package main

import (
	"strings"
	"testing"
)

// TestRunListsEveryValidationProblemAtOnce.
//
// docs/CLI.md: refuses to start on any validation error, printing all of them at
// once. A household's configuration is edited rarely and by hand; one problem per
// restart turns a five-minute fix into an evening of guessing.
func TestRunListsEveryValidationProblemAtOnce(t *testing.T) {
	t.Parallel()
	// Three faults at once: a tier no endpoint carries, two members sharing a
	// private space, and two members with the same Telegram id.
	broken := `mode: simple
household:
  name: Casa
  shared_space: household
  group_chat_id: -1001234567890
  tiers: [local]
telegram:
  bot_token_env: KENWARD_BOT_TOKEN
members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: shared-by-mistake
    tiers: [gpu]
  - id: jordan
    name: Jordan
    telegram_id: 12345678
    private_space: shared-by-mistake
    tiers: [local]
endpoints:
  - name: monster
    base_url: http://monster.tail:8000/v1
    model: qwen3.6-27b-awq
    tags: [local]
`
	h := newHarness(t, broken, fullEnvironment())
	if code := h.run("run"); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	out := h.stderr()

	// Every problem, in one report, each naming the field it is about.
	if n := strings.Count(out, "\n  - "); n < 3 {
		t.Errorf("only %d problems listed; validation must report every one it can find:\n%s", n, out)
	}
	for _, want := range []string{"gpu", "shared-by-mistake", "12345678"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
}

// TestRunListsEveryMissingEnvironmentVariableByName.
//
// docs/CLI.md: refuses to start with a missing environment variable, listing every
// one. Never starts half-configured — a node missing one member's token would serve
// everyone else and silently drop that member.
func TestRunListsEveryMissingEnvironmentVariableByName(t *testing.T) {
	t.Parallel()
	// An isolated household with nothing at all in the environment: four variables
	// are named by the configuration and none of them is set.
	h := newHarness(t, isolatedYAML, map[string]string{})
	if code := h.run("run"); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	out := h.stderr()
	for _, name := range []string{
		"KENWARD_BOT_TOKEN_DAVID",
		"KENWARD_BOT_TOKEN_JORDAN",
		"OPENROUTER_API_KEY",
	} {
		if !strings.Contains(out, name) {
			t.Errorf("the report does not name %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "half-configured") {
		t.Errorf("the report does not say why it refuses:\n%s", out)
	}

	// Note for whoever reads this next: telegram.bot_token_env is deliberately not
	// among them. internal/config treats that field as unused in isolated mode, so
	// validation does not require it — but supervisor.NewIsolated does require it
	// when a group chat is configured, and refuses to start without it. The
	// household group's missing token therefore surfaces as a runtime failure
	// (exit 1) rather than a configuration one (exit 2). That is a gap in
	// internal/config's validation rules, not something cmd/kenward should paper
	// over by inventing a check the configuration package disagrees with.
	if strings.Contains(out, "KENWARD_BOT_TOKEN_HOUSEHOLD") {
		t.Log("internal/config now validates the group token too; this test's note is stale")
	}
}

// TestRunNamesAMissingLoreCommand.
//
// internal/config neither defaults nor validates memory.lore_command, so a
// hand-written file that omits the memory: block validates cleanly and then fails
// deep in the wiring with whatever the client says about spawning nothing. The right
// place for this is validation, which is internal/config's to add; until then the
// failure has to name the key and the file rather than the symptom.
func TestRunNamesAMissingLoreCommand(t *testing.T) {
	t.Parallel()
	without := strings.Replace(simpleYAML, "memory:\n  lore_command: [lore, mcp]\n", "", 1)
	if strings.Contains(without, "lore_command") {
		t.Fatal("the fixture still has a lore_command; this test is not testing anything")
	}
	h := newHarness(t, without, fullEnvironment())

	if code := h.run("run"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	out := h.stderr()
	for _, want := range []string{h.config, "memory.lore_command", "lore_command: [lore, mcp]"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not name %q:\n%s", want, out)
		}
	}
}

// TestDataDirOverrideIsAppliedBeforeStateIsRead.
//
// The state file lives under the data directory. Reading it from the configured
// location and then applying --data-dir would merge one household's bindings into
// another's configuration.
func TestDataDirOverrideIsAppliedBeforeStateIsRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	elsewhere := t.TempDir()

	cfg, err := loadConfig(h.config, elsewhere, testSecrets(fullEnvironment()))
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	if cfg.DataDir != elsewhere {
		t.Errorf("data dir = %q, want %q", cfg.DataDir, elsewhere)
	}
	if !strings.HasPrefix(cfg.StatePath(), elsewhere) {
		t.Errorf("state path %q is not under the overridden data directory", cfg.StatePath())
	}
}

// TestConfigPathPrecedence: the flag, then the environment variable the Dockerfile
// sets, then the working directory.
func TestConfigPathPrecedence(t *testing.T) {
	t.Parallel()
	e := &env{lookupEnv: lookup(map[string]string{envConfigPath: "/etc/kenward/kenward.yaml"})}

	if got := resolveConfigPath(e, "/flag/kenward.yaml"); got != "/flag/kenward.yaml" {
		t.Errorf("the flag did not win: %q", got)
	}
	if got := resolveConfigPath(e, ""); got != "/etc/kenward/kenward.yaml" {
		t.Errorf("%s was not honoured: %q", envConfigPath, got)
	}

	bare := &env{lookupEnv: lookup(nil)}
	if got := resolveConfigPath(bare, ""); got == "" {
		t.Error("no default config path at all")
	}
}

// TestDataDirPrecedence: the same, for the other variable the Dockerfile sets.
func TestDataDirPrecedence(t *testing.T) {
	t.Parallel()
	e := &env{lookupEnv: lookup(map[string]string{envDataDir: "/var/lib/kenward"})}

	if got := resolveDataDir(e, "/flag/data"); got != "/flag/data" {
		t.Errorf("the flag did not win: %q", got)
	}
	if got := resolveDataDir(e, ""); got != "/var/lib/kenward" {
		t.Errorf("%s was not honoured: %q", envDataDir, got)
	}
	// Empty means "whatever the configuration says", which config.ApplyDefaults
	// already resolves. Answering it a second time here would put two answers in
	// the binary.
	bare := &env{lookupEnv: lookup(nil)}
	if got := resolveDataDir(bare, ""); got != "" {
		t.Errorf("data dir = %q, want empty", got)
	}
}

// TestLocalTiersReadsWhereEndpointsActuallyAre.
//
// The privacy note is only worth anything if "local" means the machine is in the
// house, not that the tier happens to be spelled "local".
func TestLocalTiersReadsWhereEndpointsActuallyAre(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, simpleYAML)
	local := localTiers(cfg)

	if !local["local"] {
		t.Error("monster.tail is a machine in the house; its tier should be local")
	}
	if local["cloud"] {
		t.Error("openrouter.ai is not in the house")
	}
	if !staysHome(local, []string{"local"}) {
		t.Error("a [local] chain should stay home")
	}
	if staysHome(local, []string{"local", "cloud"}) {
		t.Error("a chain naming cloud does not stay home")
	}
	if staysHome(local, nil) {
		t.Error("an empty chain is not local; it refuses everything")
	}
}

// TestATierIsOnlyLocalIfEveryEndpointInItIs. Routing may pick any endpoint in a
// tier, so one provider carrying the tag makes the whole tier a way out.
func TestATierIsOnlyLocalIfEveryEndpointInItIs(t *testing.T) {
	t.Parallel()
	mixed := strings.Replace(simpleYAML,
		"    tags: [cloud]\n",
		"    tags: [cloud, local]\n", 1)
	cfg := mustLoad(t, mixed)
	if localTiers(cfg)["local"] {
		t.Error("a tier carrying one provider endpoint is not local: routing may pick it")
	}
}

// TestHostIsLocal covers the address shapes a household actually uses.
func TestHostIsLocal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"http://localhost:8000/v1", true},
		{"http://127.0.0.1:8000/v1", true},
		{"http://192.168.1.20:8000/v1", true},
		{"http://10.0.0.5:8000/v1", true},
		{"http://monster:8000/v1", true},
		{"http://monster.local:8000/v1", true},
		{"http://monster.tail:8000/v1", true},
		{"http://box.ts.net:8000/v1", true},
		{"https://openrouter.ai/api/v1", false},
		{"https://api.openai.com/v1", false},
		{"http://8.8.8.8:8000/v1", false},
		{"not a url at all", false},
		{"", false},
	} {
		if got := hostIsLocal(tc.url); got != tc.want {
			t.Errorf("hostIsLocal(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
