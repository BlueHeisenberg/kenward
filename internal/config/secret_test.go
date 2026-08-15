package config_test

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// The value every test below proves never reaches a message, a log line or a formatted
// configuration. It is distinctive on purpose: a substring search for it is the whole
// assertion.
const secretValue = "123456:AA-do-not-print-me"

// credsDir stands in for $CREDENTIALS_DIRECTORY. No test reads the real one — it is
// global state belonging to whatever unit happens to have started the test binary, and a
// test that depended on it would pass or fail for reasons outside the test.
const credsDir = "/run/credentials/kenward.service"

// fakeFile is one file in fakeFS: contents and the mode the filesystem reports for it.
type fakeFile struct {
	data string
	mode fs.FileMode
}

// fakeFS is an in-memory SecretFS. It exists because the two cases that matter most —
// a file at mode 0600 and the same file at 0644 — cannot both be created portably on the
// machines this suite runs on, and a permission check that is only tested on one platform
// is a permission check nobody has tested.
type fakeFS map[string]fakeFile

func (f fakeFS) ReadSecretFile(path string) ([]byte, fs.FileMode, error) {
	file, ok := f[filepath.ToSlash(path)]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	return []byte(file.data), file.mode, nil
}

// secrets builds a resolver over a fake filesystem and a fake environment, with the mode
// check forced on so that the refusal is exercised on every platform, Windows included.
func secrets(files fakeFS, vars map[string]string, dir string) *config.Secrets {
	return config.NewSecrets(config.SecretOptions{
		LookupEnv:      env(vars),
		FS:             files,
		CredentialsDir: dir,
		FileMode:       config.FileModeEnforce,
	})
}

func TestResolveFromFile(t *testing.T) {
	s := secrets(fakeFS{"/etc/kenward/token": {data: secretValue, mode: 0o600}}, nil, "")

	got, err := s.Resolve(config.SecretRef{Where: "telegram.bot_token", File: "/etc/kenward/token"})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got.Value() != secretValue {
		t.Error("Resolve() returned the wrong value")
	}
	if want := "file /etc/kenward/token"; got.Source() != want {
		t.Errorf("Source() = %q, want %q", got.Source(), want)
	}
}

func TestResolveFromEnv(t *testing.T) {
	s := secrets(nil, map[string]string{"KENWARD_BOT_TOKEN": secretValue}, "")

	got, err := s.Resolve(config.SecretRef{Where: "telegram.bot_token", Env: "KENWARD_BOT_TOKEN"})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got.Value() != secretValue {
		t.Error("Resolve() returned the wrong value")
	}
	if !strings.Contains(got.Source(), "KENWARD_BOT_TOKEN") {
		t.Errorf("Source() = %q, want it to name the variable", got.Source())
	}
}

// TestResolveFromSystemdCredential covers the source that needs no configuration at all:
// the unit file names the credential, the directory appears, and kenward finds it.
func TestResolveFromSystemdCredential(t *testing.T) {
	s := secrets(fakeFS{
		credsDir + "/bot_token":          {data: secretValue, mode: 0o400},
		credsDir + "/bot_token.david":    {data: "member-" + secretValue, mode: 0o400},
		credsDir + "/api_key.openrouter": {data: "key-" + secretValue, mode: 0o400},
	}, nil, credsDir)

	cfg := &config.Config{
		Telegram:  config.TelegramConfig{},
		Members:   []config.MemberConfig{{ID: "david"}},
		Endpoints: []config.EndpointConfig{{Name: "openrouter"}},
	}

	household, err := cfg.BotToken(s)
	if err != nil {
		t.Fatalf("BotToken() error: %v", err)
	}
	if household.Value() != secretValue {
		t.Error("the household token did not come from the credentials directory")
	}
	if want := "systemd credential bot_token"; household.Source() != want {
		t.Errorf("Source() = %q, want %q", household.Source(), want)
	}

	member, err := cfg.Members[0].BotToken(s)
	if err != nil {
		t.Fatalf("member BotToken() error: %v", err)
	}
	if member.Value() != "member-"+secretValue {
		t.Error("the member's token did not come from bot_token.david")
	}

	key, err := cfg.Endpoints[0].APIKey(s)
	if err != nil {
		t.Fatalf("APIKey() error: %v", err)
	}
	if key.Value() != "key-"+secretValue {
		t.Error("the endpoint key did not come from api_key.openrouter")
	}
}

// TestCredentialNames pins the convention the unit file has to match, and the refusal to
// turn an unusable id into a filename.
func TestCredentialNames(t *testing.T) {
	if got := config.MemberBotTokenCredential("david"); got != "bot_token.david" {
		t.Errorf("MemberBotTokenCredential(david) = %q", got)
	}
	if got := config.EndpointAPIKeyCredential("openrouter"); got != "api_key.openrouter" {
		t.Errorf("EndpointAPIKeyCredential(openrouter) = %q", got)
	}
	for _, bad := range []string{"../../etc/shadow", "with/slash", "with space", ".."} {
		if got := config.MemberBotTokenCredential(bad); got != "" {
			t.Errorf("MemberBotTokenCredential(%q) = %q, want no credential name at all", bad, got)
		}
	}
}

// TestNoCredentialsDirectoryIsNotAFault: absence of the directory is the ordinary case
// for every deployment that supplies secrets another way.
func TestNoCredentialsDirectoryIsNotAFault(t *testing.T) {
	s := secrets(fakeFS{credsDir + "/bot_token": {data: secretValue, mode: 0o400}}, nil, "")

	_, err := (&config.Config{}).BotToken(s)
	var se *config.SecretError
	if !errors.As(err, &se) || !se.NotFound {
		t.Fatalf("BotToken() error = %v, want a NotFound *config.SecretError", err)
	}
}

// TestTwoSourcesIsAnError is the rule that a second source is a contradiction rather than
// a precedence: two sources means someone believes something false about where the value
// comes from.
func TestTwoSourcesIsAnError(t *testing.T) {
	s := secrets(
		fakeFS{"/etc/kenward/token": {data: secretValue, mode: 0o600}},
		map[string]string{"KENWARD_BOT_TOKEN": secretValue},
		"",
	)

	_, err := s.Resolve(config.SecretRef{
		Where: "telegram.bot_token",
		File:  "/etc/kenward/token",
		Env:   "KENWARD_BOT_TOKEN",
	})
	if err == nil {
		t.Fatal("Resolve() error = nil, want a refusal to choose between two sources")
	}
	for _, want := range []string{"telegram.bot_token", "bot_token_file", "bot_token_env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Error("the error exposed a secret value")
	}
}

func TestMissingFileIsNamed(t *testing.T) {
	s := secrets(fakeFS{}, nil, "")

	_, err := s.Resolve(config.SecretRef{Where: "telegram.bot_token", File: "/etc/kenward/absent"})
	if err == nil {
		t.Fatal("Resolve() error = nil, want a missing-file error")
	}
	for _, want := range []string{"telegram.bot_token_file", "/etc/kenward/absent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestPermissiveModeIsRefused: a 0644 token file is a finding. The mode goes in the
// message because the operator who created it learns no other way.
func TestPermissiveModeIsRefused(t *testing.T) {
	for _, mode := range []fs.FileMode{0o644, 0o640, 0o604, 0o666} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			s := secrets(fakeFS{"/etc/kenward/token": {data: secretValue, mode: mode}}, nil, "")

			_, err := s.Resolve(config.SecretRef{Where: "telegram.bot_token", File: "/etc/kenward/token"})
			if err == nil {
				t.Fatalf("Resolve() error = nil, want mode %04o refused", mode)
			}
			if want := fmt.Sprintf("%04o", mode); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not state the mode %s", err, want)
			}
			if !strings.Contains(err.Error(), "/etc/kenward/token") {
				t.Errorf("error %q does not name the file", err)
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Error("the error exposed a secret value")
			}
		})
	}
}

func TestOwnerOnlyModesAreAccepted(t *testing.T) {
	for _, mode := range []fs.FileMode{0o400, 0o600, 0o700} {
		s := secrets(fakeFS{"/etc/kenward/token": {data: secretValue, mode: mode}}, nil, "")
		if _, err := s.Resolve(config.SecretRef{Where: "telegram.bot_token", File: "/etc/kenward/token"}); err != nil {
			t.Errorf("mode %04o: Resolve() error: %v", mode, err)
		}
	}
}

// TestFileModePolicy covers the two deliberate escapes from the check, including the
// platform default: on Windows the mode bits os.Stat reports are synthesised from the
// read-only attribute and say nothing about the ACL that governs access, so the check is
// skipped there rather than refusing every file on the platform for no information.
func TestFileModePolicy(t *testing.T) {
	files := fakeFS{"/etc/kenward/token": {data: secretValue, mode: 0o644}}
	ref := config.SecretRef{Where: "telegram.bot_token", File: "/etc/kenward/token"}

	skipping := config.NewSecrets(config.SecretOptions{FS: files, LookupEnv: env(nil), FileMode: config.FileModeSkip})
	if _, err := skipping.Resolve(ref); err != nil {
		t.Errorf("FileModeSkip still refused a permissive file: %v", err)
	}

	def := config.NewSecrets(config.SecretOptions{FS: files, LookupEnv: env(nil)})
	_, err := def.Resolve(ref)
	if runtime.GOOS == "windows" && err != nil {
		t.Errorf("FileModeDefault on Windows refused a file whose mode bits mean nothing: %v", err)
	}
	if runtime.GOOS != "windows" && err == nil {
		t.Error("FileModeDefault on Unix accepted a group-readable secret file")
	}
}

// TestTrailingNewlineIsTrimmed: every tool that writes a credential file adds one, and a
// token carrying "\n" fails in a way nobody enjoys diagnosing.
func TestTrailingNewlineIsTrimmed(t *testing.T) {
	for _, written := range []string{
		secretValue,
		secretValue + "\n",
		secretValue + "\r\n",
		secretValue + "\n\n",
	} {
		s := secrets(fakeFS{"/etc/kenward/token": {data: written, mode: 0o600}}, nil, "")
		got, err := s.Resolve(config.SecretRef{Where: "telegram.bot_token", File: "/etc/kenward/token"})
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if got.Value() != secretValue {
			t.Errorf("file %q resolved to a value that is not the trimmed token", written)
		}
	}
}

func TestEmptySourcesAreFaults(t *testing.T) {
	s := secrets(
		fakeFS{"/etc/kenward/blank": {data: "\n", mode: 0o600}},
		map[string]string{"BLANK": "   "},
		"",
	)
	if _, err := s.Resolve(config.SecretRef{Where: "telegram.bot_token", File: "/etc/kenward/blank"}); err == nil {
		t.Error("an empty file resolved to a token")
	}
	if _, err := s.Resolve(config.SecretRef{Where: "telegram.bot_token", Env: "BLANK"}); err == nil {
		t.Error("a blank variable resolved to a token")
	}
}

// secretYAML exercises all three sources at once: the household reads a file, one member
// reads a variable, the other takes the systemd credential, and one endpoint reads a key
// file.
const secretYAML = `
mode: isolated
household: {shared_space: household, tiers: [local]}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_file: /etc/kenward/david.token}
  - {id: maria, private_space: mp, tiers: [local], bot_token_env: T_MARIA}
  - {id: jordan, private_space: jp, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
  - {name: openrouter, base_url: https://o:1/v1, model: m, api_key_file: /etc/kenward/or.key, tags: [local]}
`

func secretConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Decode(strings.NewReader(secretYAML))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	return cfg
}

func fullySuppliedSecrets() *config.Secrets {
	return secrets(fakeFS{
		"/etc/kenward/david.token":     {data: "david-" + secretValue, mode: 0o600},
		"/etc/kenward/or.key":          {data: "key-" + secretValue + "\n", mode: 0o600},
		credsDir + "/bot_token.jordan": {data: "jordan-" + secretValue, mode: 0o400},
	}, map[string]string{"T_MARIA": "maria-" + secretValue}, credsDir)
}

// TestValidateAcceptsEverySource is the whole feature seen from the operator's side: a
// household whose three members are supplied three different ways validates clean.
func TestValidateAcceptsEverySource(t *testing.T) {
	if err := secretConfig(t).ValidateWithSecrets(fullySuppliedSecrets()); err != nil {
		t.Fatalf("ValidateWithSecrets() error: %v", err)
	}
}

// TestValidateReportsEverySecretAtOnce keeps the package's standing promise: one pass,
// one list, one edit — and the full set of missing secrets named.
func TestValidateReportsEverySecretAtOnce(t *testing.T) {
	cfg := secretConfig(t)
	// Nothing supplied anywhere: no files, no variables, no credentials directory.
	err := cfg.ValidateWithSecrets(secrets(fakeFS{}, nil, ""))

	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("ValidateWithSecrets() error = %v, want *config.ValidationError", err)
	}
	// Every unmet secret, in file order: the two whose stated source is not there,
	// the one with no source at all, and the endpoint key file that is also absent.
	// The local endpoint, which states nothing and needs nothing, is not on the list.
	want := []string{"members[0].bot_token", "members[1].bot_token", "members[2].bot_token", "endpoints[1].api_key"}
	if got := ve.MissingSecrets; !equalStrings(got, want) {
		t.Errorf("MissingSecrets = %v, want %v", got, want)
	}
	// The variable-named one is still on the export list `kenward doctor` prints; the
	// file and credential ones have no variable to export and are not invented.
	if got := ve.MissingEnv; !equalStrings(got, []string{"T_MARIA"}) {
		t.Errorf("MissingEnv = %v, want [T_MARIA]", got)
	}
	// members[2] states no source at all, so the message has to teach all three.
	joined := err.Error()
	for _, want := range []string{
		"members[2].bot_token_env: required in isolated mode",
		"bot_token_file",
		`"bot_token.jordan"`,
		"/etc/kenward/david.token",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("error does not mention %q:\n%v", want, joined)
		}
	}
}

// TestValidateRejectsTwoSourcesForOneSecret: stating both is a fault of the file, found
// at load time rather than resolved by precedence at runtime.
func TestValidateRejectsTwoSourcesForOneSecret(t *testing.T) {
	const doc = `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T, bot_token_file: /etc/kenward/token}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`
	cfg, err := config.Decode(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	s := secrets(fakeFS{"/etc/kenward/token": {data: secretValue, mode: 0o600}},
		map[string]string{"T": secretValue}, "")

	err = cfg.ValidateWithSecrets(s)
	if err == nil {
		t.Fatal("ValidateWithSecrets() error = nil, want a two-sources error")
	}
	if !strings.Contains(err.Error(), "both set") {
		t.Errorf("error does not say both sources are set: %v", err)
	}
}

// TestIsolatedModeRefusesASharedTokenFile is the file-form twin of the shared-variable
// rule: two pods on one bot is not isolation, whichever form the token arrives in.
func TestIsolatedModeRefusesASharedTokenFile(t *testing.T) {
	const doc = `
mode: isolated
household: {shared_space: household, tiers: [local]}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_file: /etc/kenward/shared.token}
  - {id: maria, private_space: mp, tiers: [local], bot_token_file: /etc/kenward/shared.token}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`
	cfg, err := config.Decode(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	s := secrets(fakeFS{"/etc/kenward/shared.token": {data: secretValue, mode: 0o600}}, nil, "")

	err = cfg.ValidateWithSecrets(s)
	if err == nil {
		t.Fatal("ValidateWithSecrets() error = nil, want a shared-token-file error")
	}
	if !strings.Contains(err.Error(), "members[1].bot_token_file") {
		t.Errorf("error does not name the second member's file: %v", err)
	}
}

// TestSecretsAreNeverFormatted is the rule this whole file exists to protect. The
// resolved value is behind a closure and no Config field holds it, so neither %+v nor
// %#v — the two verbs a debugging session reaches for first — can print one.
func TestSecretsAreNeverFormatted(t *testing.T) {
	cfg := secretConfig(t)
	s := fullySuppliedSecrets()
	if err := cfg.ValidateWithSecrets(s); err != nil {
		t.Fatalf("ValidateWithSecrets() error: %v", err)
	}

	resolved := []config.Secret{}
	for _, m := range cfg.Members {
		sec, err := m.BotToken(s)
		if err != nil {
			t.Fatalf("member %s BotToken() error: %v", m.ID, err)
		}
		resolved = append(resolved, sec)
	}
	for _, e := range cfg.Endpoints {
		sec, err := e.APIKey(s)
		if err != nil {
			t.Fatalf("endpoint %s APIKey() error: %v", e.Name, err)
		}
		resolved = append(resolved, sec)
	}

	// Every value really was resolved, or the rest of this test proves nothing.
	if got := resolved[0].Value(); got != "david-"+secretValue {
		t.Fatalf("members[0] token = %q, want the file's contents", got)
	}
	if got := resolved[1].Value(); got != "maria-"+secretValue {
		t.Fatalf("members[1] token = %q, want the variable's value", got)
	}
	if got := resolved[2].Value(); got != "jordan-"+secretValue {
		t.Fatalf("members[2] token = %q, want the credential's contents", got)
	}

	// The configuration itself, whole, both verbs.
	rendered := fmt.Sprintf("%+v %#v", cfg, cfg)
	if strings.Contains(rendered, secretValue) {
		t.Error("formatting the whole config exposed a secret value")
	}
	// The paths and names may appear — that is what makes a fault fixable.
	if !strings.Contains(rendered, "/etc/kenward/david.token") || !strings.Contains(rendered, "T_MARIA") {
		t.Error("formatting the config hid the names and paths, which are meant to be visible")
	}

	// A Secret on its own, in an exported field where GoString is reachable, and in an
	// unexported one where it is not and fmt falls back to reflection.
	type exported struct{ Token config.Secret }
	type unexported struct{ token config.Secret }
	for _, v := range []any{
		resolved[0],
		exported{resolved[0]},
		unexported{resolved[0]},
		[]config.Secret{resolved[1]},
		map[string]config.Secret{"maria": resolved[1]},
	} {
		if out := fmt.Sprintf("%v %+v %#v %s", v, v, v, fmt.Sprint(v)); strings.Contains(out, secretValue) {
			t.Errorf("formatting %T exposed a secret value: %s", v, out)
		}
	}

	// And the zero Secret says so rather than looking like an empty token.
	var zero config.Secret
	if zero.IsSet() || zero.Value() != "" {
		t.Error("the zero Secret is not empty")
	}
	if !strings.Contains(zero.String(), "unset") {
		t.Errorf("zero Secret String() = %q", zero.String())
	}
}

// TestMissingSecretNames covers the list `kenward doctor` needs now that a missing secret
// need not be a missing variable.
func TestMissingSecretNames(t *testing.T) {
	cfg := secretConfig(t)
	s := secrets(fakeFS{"/etc/kenward/david.token": {data: secretValue, mode: 0o600}}, nil, credsDir)

	// Sorted, and mixing the forms: a variable nobody exported, a credential the unit
	// does not carry, and a key file that is not there.
	want := []string{"endpoints[1].api_key", "members[1].bot_token", "members[2].bot_token"}
	if got := cfg.MissingSecretNames(s); !equalStrings(got, want) {
		t.Errorf("MissingSecretNames() = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
