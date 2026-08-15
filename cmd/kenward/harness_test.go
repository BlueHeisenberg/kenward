package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/version"
)

// The two configurations every test works from. They are the shapes docs/CLI.md and
// docs/IMPLEMENTATION.md §4 describe, with one local-only member and one member whose
// chain reaches a provider, so that the privacy notes have both cases to render.
const simpleYAML = `mode: simple
household:
  name: Casa
  shared_space: household
  group_chat_id: -1001234567890
  tiers: [local, cloud]
telegram:
  bot_token_env: KENWARD_BOT_TOKEN
members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: david-private
    tiers: [local]
  - id: jordan
    name: Jordan
    telegram_id: 87654321
    private_space: jordan-private
    tiers: [local, cloud]
endpoints:
  - name: monster
    base_url: http://monster.tail:8000/v1
    model: qwen3.6-27b-awq
    tags: [local]
    timeout: 120s
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-sonnet-5
    api_key_env: OPENROUTER_API_KEY
    tags: [cloud]
memory:
  lore_command: [lore, mcp]
`

const isolatedYAML = `mode: isolated
household:
  name: Casa
  shared_space: household
  group_chat_id: -1001234567890
  tiers: [local]
telegram:
  bot_token_env: KENWARD_BOT_TOKEN_HOUSEHOLD
members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: david-private
    tiers: [local]
    bot_token_env: KENWARD_BOT_TOKEN_DAVID
  - id: jordan
    name: Jordan
    telegram_id: 87654321
    private_space: jordan-private
    tiers: [local, cloud]
    bot_token_env: KENWARD_BOT_TOKEN_JORDAN
endpoints:
  - name: monster
    base_url: http://monster.tail:8000/v1
    model: qwen3.6-27b-awq
    tags: [local]
    timeout: 120s
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-sonnet-5
    api_key_env: OPENROUTER_API_KEY
    tags: [cloud]
memory:
  lore_command: [lore, mcp]
`

// Secrets the fake environment holds. Every test that renders anything asserts these
// never appear in the output; docs/CLI.md's conventions forbid a command printing a
// bot token or an API key, and the only way to keep that true is to check it.
const (
	fakeBotToken     = "1234567890:AAH-thisIsNotARealBotTokenXXXXXXXXXXXXX"
	fakeAPIKey       = "sk-or-v1-thisIsNotARealApiKeyYYYYYYYYYYYYYYYYYY"
	fakeDavidToken   = "1111111111:AAH-davidsOwnBotTokenZZZZZZZZZZZZZZZZZZ"
	fakeJordanToken  = "2222222222:AAH-jordansOwnBotTokenWWWWWWWWWWWWWWWW"
	fakeGroupToken   = "3333333333:AAH-householdGroupBotTokenVVVVVVVVVVVV"
	versionPlacehold = "<VERSION>"
)

func fullEnvironment() map[string]string {
	return map[string]string{
		"KENWARD_BOT_TOKEN":           fakeBotToken,
		"OPENROUTER_API_KEY":          fakeAPIKey,
		"KENWARD_BOT_TOKEN_DAVID":     fakeDavidToken,
		"KENWARD_BOT_TOKEN_JORDAN":    fakeJordanToken,
		"KENWARD_BOT_TOKEN_HOUSEHOLD": fakeGroupToken,
	}
}

// allSecrets is every value a command must never print.
func allSecrets() []string {
	return []string{fakeBotToken, fakeAPIKey, fakeDavidToken, fakeJordanToken, fakeGroupToken}
}

func lookup(vars map[string]string) config.LookupEnvFunc {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// testSecrets builds a resolver over a fake environment and nothing else, which is
// what most tests want: no filesystem, no credentials directory.
func testSecrets(vars map[string]string) *config.Secrets {
	return config.NewSecrets(config.SecretOptions{LookupEnv: lookup(vars)})
}

// fakeSecretFS is a filesystem of secret files, each with the mode it would have on
// disk. It exists because a developer's machine cannot portably hold a 0644 secret
// for a test to object to — on Windows the bits mean something else entirely.
type fakeSecretFS map[string]fakeSecretFile

type fakeSecretFile struct {
	data string
	mode fs.FileMode
}

func (f fakeSecretFS) ReadSecretFile(path string) ([]byte, fs.FileMode, error) {
	// Keys are written with forward slashes because that is how the paths read in
	// a unit file and in a test; internal/config joins the credentials directory
	// with filepath.Join, which uses backslashes on Windows. Normalising here keeps
	// the same test meaningful on both, rather than silently matching nothing and
	// asserting the not-found path by accident.
	file, ok := f[filepath.ToSlash(path)]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	return []byte(file.data), file.mode, nil
}

// harness is one command invocation's world.
type harness struct {
	e      *env
	out    *bytes.Buffer
	err    *bytes.Buffer
	dir    string
	config string
}

func newHarness(t *testing.T, yaml string, vars map[string]string) *harness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing configuration: %v", err)
	}
	if vars == nil {
		vars = map[string]string{}
	}
	// The data directory is where the state and invite files go; keeping it inside
	// the temp directory keeps a test off the developer's real state.
	vars[envDataDir] = filepath.Join(dir, "data")

	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	h := &harness{
		out:    out,
		err:    errBuf,
		dir:    dir,
		config: path,
	}
	h.e = &env{
		stdin:     strings.NewReader(""),
		stdout:    out,
		stderr:    errBuf,
		lookupEnv: lookup(vars),
		now:       func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
		goos:      "linux",
		ctx:       context.Background(),
		probes:    healthyProbes(),
	}
	return h
}

// run invokes the dispatcher with --config already pointed at this harness's file.
//
// The flag goes immediately after the subcommand, which is where the flag package
// wants it; parseWithPositionals makes the other order work too, and
// TestFlagsAfterAPositionalArgument covers that on its own.
func (h *harness) run(args ...string) int {
	if len(args) == 0 {
		return dispatch(h.e, nil)
	}
	full := append([]string{args[0], "--config", h.config}, args[1:]...)
	return dispatch(h.e, full)
}

func (h *harness) stdout() string { return h.out.String() }
func (h *harness) stderr() string { return h.err.String() }
func (h *harness) both() string   { return h.out.String() + h.err.String() }

// assertNoSecrets is the check docs/CLI.md's conventions demand: no command ever
// prints a bot token or an API key.
func (h *harness) assertNoSecrets(t *testing.T) {
	t.Helper()
	both := h.both()
	for _, secret := range allSecrets() {
		if strings.Contains(both, secret) {
			t.Errorf("output contains a secret from the environment (%.12s…)", secret)
		}
	}
}

// healthyProbes is a world where lore answers, Telegram authorises and every machine
// is awake.
func healthyProbes() probes {
	return probes{
		lore: func(_ context.Context, cfg *config.Config) loreResult {
			var res loreResult
			for _, s := range configuredSpaces(cfg) {
				res.Spaces = append(res.Spaces, spaceResult{Space: s})
			}
			return res
		},
		telegram: func(_ context.Context, token string) telegramResult {
			switch token {
			case fakeDavidToken:
				return telegramResult{Username: "david_kenward_bot"}
			case fakeJordanToken:
				return telegramResult{Username: "jordan_kenward_bot"}
			case fakeGroupToken:
				return telegramResult{Username: "casa_household_bot"}
			default:
				return telegramResult{Username: "our_household_bot"}
			}
		},
		endpoint: func(_ context.Context, ep routing.Endpoint) endpointResult {
			return endpointResult{
				Name:    ep.Name,
				Tiers:   ep.Tags,
				Reached: true,
				Elapsed: 412 * time.Millisecond,
			}
		},
		sessions: func(_ context.Context, cfg *config.Config) sessionsResult {
			var res sessionsResult
			for _, m := range cfg.DomainMembers() {
				if m.Enrolled() {
					res.Provisioned = append(res.Provisioned, m.ID)
				}
			}
			res.Custody = session.CustodyReport{
				Mode:    sessionMode(cfg.Mode),
				Members: res.Provisioned,
			}.String()
			return res
		},
	}
}

// noKeysProvisioned is the failure the operator cannot otherwise see: members are
// enrolled, the group chat works, and every private message comes back locked.
func noKeysProvisioned(base probes) probes {
	base.sessions = func(_ context.Context, cfg *config.Config) sessionsResult {
		res := sessionsResult{Custody: session.CustodyReport{Mode: sessionMode(cfg.Mode)}.String()}
		for _, m := range cfg.DomainMembers() {
			if m.Enrolled() {
				res.MissingKey = append(res.MissingKey, m.ID)
			}
		}
		return res
	}
	return base
}

// everythingAsleep is the case that must not fail: every household machine switched
// off. The container's HEALTHCHECK runs `doctor`, so this has to exit 0.
func everythingAsleep(base probes) probes {
	base.endpoint = func(_ context.Context, ep routing.Endpoint) endpointResult {
		return endpointResult{
			Name:   ep.Name,
			Tiers:  ep.Tags,
			Detail: "no answer — switched off, asleep, or behind a firewall",
		}
	}
	return base
}

func loreDown(base probes) probes {
	base.lore = func(context.Context, *config.Config) loreResult {
		return loreResult{Err: errors.New("lore mcp: exec: \"lore\": executable file not found in $PATH")}
	}
	return base
}

func telegramRefuses(base probes) probes {
	base.telegram = func(context.Context, string) telegramResult {
		return telegramResult{Err: errors.New("telegram refused the token: Unauthorized")}
	}
	return base
}

// normalize replaces the build version so golden files survive a build with
// -ldflags, and normalises line endings so they survive a checkout on Windows.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if v := version.Short(); v != "" {
		s = strings.ReplaceAll(s, "kenward "+v+" ", "kenward "+versionPlacehold+" ")
	}
	return s
}

func mustMember(t *testing.T, cfg *config.Config, id string) domain.Member {
	t.Helper()
	m, ok := cfg.MemberByID(domain.MemberID(id))
	if !ok {
		t.Fatalf("member %q not in configuration", id)
	}
	return m
}
