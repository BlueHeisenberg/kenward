package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

// realToken is a token of the shape BotFather hands out. It is a fixture, not a
// credential, and the tests below assert that it never reaches the YAML or the
// terminal.
//
// It is deliberately not the example token printed in the walkthrough: a fixture
// that matched the illustration would make "no secret was printed" pass for the
// wrong reason.
const realToken = "987654321:BBH-9zzqW_kK1mn5R2t8vXcYbNmQwErTyU3"

// noEnv is an environment with nothing in it, so that a test never depends on what
// happens to be exported in the process running it.
func noEnv(string) (string, bool) { return "", false }

// runWizard runs a scripted flow in a temporary directory and returns the wizard,
// its configuration and the transcript.
func runWizard(t *testing.T, goos string, opts Options, answers ...string) (*Wizard, *config.Config, *ScriptIO, error) {
	t.Helper()
	io := NewScriptIO(answers...)
	if opts.ConfigPath == "" {
		opts.ConfigPath = filepath.Join(t.TempDir(), DefaultConfigFileName)
	}
	if opts.GOOS == "" {
		opts.GOOS = goos
	}
	if opts.Probe == nil {
		opts.Probe = fixedProbe(Answered)
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = noEnv
	}
	w := New(io, opts)
	cfg, err := w.Run(context.Background())
	return w, cfg, io, err
}

// simpleAnswers is a complete run through simple mode: one local endpoint, one
// provider, two members, and no to every widening.
func simpleAnswers() []string {
	return []string{
		"1",         // trust question: our own family machine
		"Casa",      // household name
		"household", // shared space
		realToken,   // bot token
		"y",         // write .env
		"David",     // member
		"María",     // member
		"",          // no more members
		"monster",   // endpoint name
		"http://monster.tail:8000/v1",
		"qwen3.6-27b-awq",
		"n",     // no api key
		"local", // tiers
		"y",     // another endpoint
		"openrouter",
		"https://openrouter.ai/api/v1",
		"anthropic/claude-sonnet-5",
		"y",                  // needs an api key
		"OPENROUTER_API_KEY", // variable
		"sk-secret",          // value
		"cloud",              // tiers
		"n",                  // no more endpoints
		"n",                  // David: no cloud
		"n",                  // María: no cloud
		"n",                  // group: no cloud
	}
}

func TestSimpleModeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFileName)
	w, cfg, io, err := runWizard(t, "windows", Options{ConfigPath: path}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if io.Remaining() != 0 {
		t.Errorf("%d scripted answers were never used, so the flow is not the one under test", io.Remaining())
	}

	if cfg.Mode != config.ModeSimple {
		t.Errorf("mode = %q, want simple", cfg.Mode)
	}
	if cfg.Household.Name != "Casa" || cfg.Household.SharedSpace != "household" {
		t.Errorf("household = %+v", cfg.Household)
	}
	if cfg.Telegram.BotTokenEnv != DefaultBotTokenEnv {
		t.Errorf("telegram.bot_token_env = %q, want %q", cfg.Telegram.BotTokenEnv, DefaultBotTokenEnv)
	}
	if len(cfg.Members) != 2 {
		t.Fatalf("got %d members, want 2", len(cfg.Members))
	}
	if cfg.Members[0].ID != "david" || cfg.Members[1].ID != "maria" {
		t.Errorf("member ids = %q, %q", cfg.Members[0].ID, cfg.Members[1].ID)
	}
	for _, m := range cfg.Members {
		// Nobody was asked for a Telegram id, and none may be invented.
		if m.TelegramID != 0 {
			t.Errorf("member %s has telegram_id %d before enrolling", m.ID, m.TelegramID)
		}
		if m.BotTokenEnv != "" {
			t.Errorf("member %s has a per-member token in simple mode", m.ID)
		}
		if strings.Join(m.Tiers, ",") != "local" {
			t.Errorf("member %s tiers = %v, want the local-only default", m.ID, m.Tiers)
		}
	}
	if strings.Join(cfg.Household.Tiers, ",") != "local" {
		t.Errorf("household tiers = %v, want the local-only default", cfg.Household.Tiers)
	}
	if w.ConfigPath() != path {
		t.Errorf("ConfigPath() = %q, want %q", w.ConfigPath(), path)
	}
	if w.EnvFilePath() != filepath.Join(dir, EnvFileName) {
		t.Errorf("EnvFilePath() = %q", w.EnvFilePath())
	}

	assertLoadable(t, path, w)
	assertNoSecrets(t, path, realToken, "sk-secret")

	// The statement for the mode that was chosen, and only that one.
	if !strings.Contains(io.Transcript(), privacy.Statement(privacy.ModeSimple)) {
		t.Error("the simple-mode privacy statement was not printed")
	}
	if strings.Contains(io.Transcript(), "Privacy, in isolated mode") {
		t.Error("the isolated-mode privacy statement was printed for a simple-mode household")
	}
}

func TestIsolatedModeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFileName)
	answers := []string{
		"2",         // trust question: no, seal it
		"Casa",      // household name
		"household", // shared space
		realToken,   // the group chat's bot
		"y",         // write .env
		"David", "María", "",
		"monster", "http://monster.tail:8000/v1", "qwen3.6-27b-awq", "n", "local",
		"n", // no more endpoints
	}
	w, cfg, io, err := runWizard(t, "linux", Options{ConfigPath: path}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Mode != config.ModeIsolated {
		t.Fatalf("mode = %q, want isolated", cfg.Mode)
	}

	// With no endpoint outside the house there is no cloud tier, so no opt-in is
	// offered and no scripted answer is left over.
	if io.Remaining() != 0 {
		t.Errorf("%d answers unused", io.Remaining())
	}

	seen := map[string]bool{}
	for _, m := range cfg.Members {
		if m.BotTokenEnv == "" {
			t.Errorf("member %s has no bot token variable, which isolated mode requires", m.ID)
		}
		if seen[m.BotTokenEnv] {
			t.Errorf("member %s shares a bot token variable, which defeats the mode", m.ID)
		}
		seen[m.BotTokenEnv] = true
	}
	if cfg.Members[0].BotTokenEnv != "KENWARD_BOT_TOKEN_DAVID" {
		t.Errorf("David's token variable = %q", cfg.Members[0].BotTokenEnv)
	}

	// The variables that do not exist yet are named for the operator rather than
	// left to be discovered when kenward refuses to start.
	transcript := io.Transcript()
	for _, name := range []string{"KENWARD_BOT_TOKEN_DAVID", "KENWARD_BOT_TOKEN_MARIA"} {
		if !strings.Contains(transcript, name) {
			t.Errorf("the transcript never names %s", name)
		}
	}
	if !strings.Contains(transcript, "kenward will not start until") {
		t.Error("the transcript does not say that the missing variables block startup")
	}
	if !strings.Contains(transcript, privacy.Statement(privacy.ModeIsolated)) {
		t.Error("the isolated-mode privacy statement was not printed")
	}

	assertLoadable(t, path, w)
	assertNoSecrets(t, path, realToken)
}

// TestIsolatedOnNonLinuxExplainsAndOffersSimple covers the case somebody actually
// hits: they want their household sealed, and they are standing at a Mac.
func TestIsolatedOnNonLinuxExplainsAndOffersSimple(t *testing.T) {
	for _, goos := range []string{"windows", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			answers := append([]string{"2", "1"}, simpleAnswers()[1:]...)
			_, cfg, io, err := runWizard(t, goos, Options{}, answers...)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, io.Transcript())
			}
			transcript := io.Transcript()

			if !strings.Contains(transcript, "Isolated mode is Linux only") {
				t.Error("the wizard did not say that isolated mode is Linux only")
			}
			if !strings.Contains(transcript, osName(goos)) {
				t.Errorf("the explanation does not name the platform (%s)", osName(goos))
			}
			if !strings.Contains(transcript, isolatedFallbackSimple) {
				t.Error("simple mode was not offered as the alternative")
			}
			if !strings.Contains(transcript, isolatedFallbackStop) {
				t.Error("stopping and using Linux was not offered")
			}
			// It must not pretend: no configuration claiming isolation is written,
			// and the statement printed is simple mode's.
			if cfg.Mode != config.ModeSimple {
				t.Errorf("mode = %q, want simple after the fallback", cfg.Mode)
			}
			if !strings.Contains(transcript, privacy.Statement(privacy.ModeSimple)) {
				t.Error("the simple-mode privacy statement was not printed after falling back")
			}
		})
	}
}

// TestIsolatedOnNonLinuxCanStop asserts that choosing to go and do it on Linux
// writes nothing at all.
func TestIsolatedOnNonLinuxCanStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFileName)
	_, cfg, io, err := runWizard(t, "windows", Options{ConfigPath: path}, "2", "2")
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
	if cfg != nil {
		t.Error("a configuration was returned after stopping")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file was written after the operator chose to stop")
	}
	if !strings.Contains(io.Transcript(), "Nothing has been written") {
		t.Error("the wizard did not say that nothing was written")
	}
}

// TestTheDefaultOnNonLinuxIsToStop asserts that pressing Enter at the fallback
// question does not silently downgrade the mode somebody just asked for.
func TestTheDefaultOnNonLinuxIsToStop(t *testing.T) {
	_, _, _, err := runWizard(t, "windows", Options{}, "2", "")
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped: an empty answer must not choose simple mode", err)
	}
}

// TestEveryPathProducesConfigTheLoaderAccepts is the assertion the whole package
// exists to keep. A wizard that can write a file kenward will not load has failed at
// its only job, so several different journeys through it are run and each one's
// output is put through the real loader.
func TestEveryPathProducesConfigTheLoaderAccepts(t *testing.T) {
	paths := map[string][]string{
		"simple, everything local": {
			"1", "Home", "household", realToken, "n",
			"David", "",
			"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
			"n",
		},
		"simple, cloud allowed everywhere": append(simpleAnswers()[:len(simpleAnswers())-3], "y", "y", "y"),
		"simple, one member takes cloud and the other does not": append(
			simpleAnswers()[:len(simpleAnswers())-3], "y", "n", "n"),
		"simple, several tiers on one endpoint": {
			"1", "Home", "household", realToken, "n",
			"David", "",
			"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local, local-slow",
			"n",
		},
		"simple, no token given at all": {
			"1", "Home", "household", "", "y",
			"David", "",
			"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
			"n",
		},
		"simple, names that collide": {
			"1", "Home", "household", realToken, "n",
			"David", "David", "",
			"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
			"n",
		},
		"simple, a name that is not Latin at all": {
			"1", "Home", "household", realToken, "n",
			"あかり", "",
			"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
			"n",
		},
		"simple, only a provider, opted into deliberately": {
			"1", "Home", "household", realToken, "n",
			"David", "",
			"openrouter", "https://openrouter.ai/api/v1", "sonnet", "y", "OPENROUTER_API_KEY", "sk-x", "cloud",
			"n",
			"y", // yes, use the provider for private conversations
		},
		"simple, endpoint that did not answer": {
			"1", "Home", "household", realToken, "n",
			"David", "",
			"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
			"n",
		},
		"simple, a shared space that had to be slugified": {
			"1", "Home", "Our House!", realToken, "n",
			"David", "",
			"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
			"n",
		},
	}

	for name, answers := range paths {
		t.Run(name, func(t *testing.T) {
			probe := fixedProbe(Answered)
			if strings.Contains(name, "did not answer") {
				probe = fixedProbe(NoAnswer)
			}
			path := filepath.Join(t.TempDir(), DefaultConfigFileName)
			w, cfg, io, err := runWizard(t, "linux", Options{ConfigPath: path, Probe: probe}, answers...)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, io.Transcript())
			}
			if err := cfg.Validate(w.validationEnv()); err != nil {
				t.Fatalf("the returned configuration does not validate: %v", err)
			}
			assertLoadable(t, path, w)
		})
	}
}

// TestIsolatedPathsAlsoLoad runs the same assertion through isolated mode, where the
// per-member token variables cannot exist yet.
func TestIsolatedPathsAlsoLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	answers := []string{
		"2", "Home", "household", realToken, "n",
		"David", "María", "Ana", "",
		"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
		"n",
	}
	w, cfg, io, err := runWizard(t, "linux", Options{ConfigPath: path}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if len(cfg.Members) != 3 {
		t.Fatalf("got %d members", len(cfg.Members))
	}
	assertLoadable(t, path, w)
}

func TestRefusesToOverwriteAnExistingConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFileName)
	const existing = "mode: simple  # written by a person, not by us\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := runWizard(t, "linux", Options{ConfigPath: path}, simpleAnswers()...)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing {
		t.Error("the existing configuration was modified")
	}
}

func TestForceReplacesAnExistingConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFileName)
	if err := os.WriteFile(path, []byte("mode: simple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _, io, err := runWizard(t, "linux", Options{ConfigPath: path, Force: true}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	assertLoadable(t, path, w)
}

func TestEnvFileIsWrittenPrivately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFileName)
	_, _, io, err := runWizard(t, "linux", Options{ConfigPath: path}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}

	envPath := filepath.Join(dir, EnvFileName)
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("no .env was written: %v", err)
	}
	if runtime.GOOS == "windows" {
		// Windows has no mode bits worth asserting on: os.Chmod there sets only
		// the read-only attribute. What can still be checked is that the wizard
		// asked for 0600 and that the file exists, and the mode is enforced by
		// this assertion on every platform where it means something.
		if envFileMode != 0o600 {
			t.Errorf("envFileMode = %O, want 0600", envFileMode)
		}
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env mode = %O, want 0600", perm)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{
		"KENWARD_BOT_TOKEN=" + realToken,
		"OPENROUTER_API_KEY=sk-secret",
	} {
		if !strings.Contains(body, want) {
			t.Errorf(".env does not contain %q", want)
		}
	}
	if !strings.Contains(body, "This file holds secrets") {
		t.Error(".env does not say what it is")
	}
}

// TestExistingEnvFileIsLeftAlone: an existing .env belongs to somebody else and may
// hold secrets for something entirely different.
func TestExistingEnvFileIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, EnvFileName)
	const existing = "SOMETHING_ELSE=keepme\n"
	if err := os.WriteFile(envPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	w, _, io, err := runWizard(t, "linux",
		Options{ConfigPath: filepath.Join(dir, DefaultConfigFileName)}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing {
		t.Errorf("the existing .env was rewritten:\n%s", got)
	}
	if w.EnvFilePath() != "" {
		t.Errorf("EnvFilePath() = %q, but nothing was written", w.EnvFilePath())
	}

	transcript := io.Transcript()
	if !strings.Contains(transcript, "already exists") {
		t.Error("the operator was not told the .env was left alone")
	}
	if !strings.Contains(transcript, "KENWARD_BOT_TOKEN") {
		t.Error("the operator was not told which variables to add")
	}
	// Being told which variable to add must never mean being shown the value.
	if strings.Contains(transcript, realToken) {
		t.Error("the token was printed to the terminal")
	}
}

// TestNoSecretIsEverPrinted covers the transcript as a whole, including the paths
// where the wizard repeats a question.
func TestNoSecretIsEverPrinted(t *testing.T) {
	_, _, io, err := runWizard(t, "linux", Options{},
		append([]string{}, simpleAnswers()...)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{realToken, "sk-secret"} {
		if strings.Contains(io.Transcript(), secret) {
			t.Errorf("a secret reached the terminal: %q", secret)
		}
	}
}

func TestTokenShapeIsQueriedNotEnforced(t *testing.T) {
	// A pasted username instead of a token: the wizard says so and asks again.
	answers := []string{
		"1", "Home", "household",
		"@our_household_bot", // not a token
		"n",                  // no, do not use it anyway
		realToken,            // the real one
		"n",                  // do not write .env
		"David", "",
		"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
		"n",
	}
	_, cfg, io, err := runWizard(t, "linux", Options{}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if !strings.Contains(io.Transcript(), "does not look like a bot token") {
		t.Error("the wizard accepted a bot username without comment")
	}
	if cfg.Telegram.BotTokenEnv != DefaultBotTokenEnv {
		t.Errorf("bot_token_env = %q", cfg.Telegram.BotTokenEnv)
	}

	// And insisting is allowed, because Telegram's format is theirs to change.
	insist := []string{
		"1", "Home", "household",
		"some-new-shape", "y", // use it anyway
		"n",
		"David", "",
		"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
		"n",
	}
	if _, _, io, err := runWizard(t, "linux", Options{}, insist...); err != nil {
		t.Fatalf("insisting on an odd token failed: %v\n%s", err, io.Transcript())
	}
}

// TestCloudIsNeverTheDefault is the routing half of the privacy claim, checked at
// the only place a person can get it wrong.
func TestCloudIsNeverTheDefault(t *testing.T) {
	// Every tier question answered by pressing Enter.
	answers := []string{
		"1", "Home", "household", realToken, "n",
		"David", "María", "",
		"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local", "y",
		"openrouter", "https://openrouter.ai/api/v1", "sonnet", "y", "OPENROUTER_API_KEY", "sk-x", "cloud", "n",
		"", "", "", // Enter at all three tier questions
	}
	_, cfg, io, err := runWizard(t, "linux", Options{}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	for _, m := range cfg.Members {
		if len(m.Tiers) != 1 || m.Tiers[0] != LocalTier {
			t.Errorf("member %s got %v by pressing Enter; the default must be local-only", m.ID, m.Tiers)
		}
	}
	if len(cfg.Household.Tiers) != 1 || cfg.Household.Tiers[0] != LocalTier {
		t.Errorf("household got %v by pressing Enter", cfg.Household.Tiers)
	}
	// The consequence has to be on screen next to the question, not in a preamble.
	if !strings.Contains(io.Transcript(), "openrouter.ai whenever no local machine answers") {
		t.Error("the cloud opt-in did not name where the messages would go")
	}
}

func TestCloudOptInWidensOnlyWhoAskedForIt(t *testing.T) {
	answers := append(simpleAnswers()[:len(simpleAnswers())-3], "y", "n", "n")
	_, cfg, io, err := runWizard(t, "linux", Options{}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if strings.Join(cfg.Members[0].Tiers, ",") != "local,cloud" {
		t.Errorf("David opted in but got %v", cfg.Members[0].Tiers)
	}
	if strings.Join(cfg.Members[1].Tiers, ",") != "local" {
		t.Errorf("María did not opt in but got %v", cfg.Members[1].Tiers)
	}
	if strings.Join(cfg.Household.Tiers, ",") != "local" {
		t.Errorf("the group did not opt in but got %v", cfg.Household.Tiers)
	}
}

// TestNoLocalEndpointsIsAnExplicitDecision covers the household whose only endpoint
// is a provider: there is no private default available, so the wizard says so and
// makes them answer rather than quietly defaulting to the wide chain.
func TestNoLocalEndpointsIsAnExplicitDecision(t *testing.T) {
	base := []string{
		"1", "Home", "household", realToken, "n",
		"David", "",
		"openrouter", "https://openrouter.ai/api/v1", "sonnet", "y", "OPENROUTER_API_KEY", "sk-x", "cloud",
		"n",
	}

	t.Run("saying no writes nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), DefaultConfigFileName)
		_, _, io, err := runWizard(t, "linux", Options{ConfigPath: path}, append(base, "n")...)
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("err = %v, want ErrStopped", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Error("a configuration was written after the operator declined")
		}
		if !strings.Contains(io.Transcript(), "local-only chain") {
			t.Error("the operator was not told why there was no private default")
		}
	})

	t.Run("saying yes is recorded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), DefaultConfigFileName)
		w, cfg, io, err := runWizard(t, "linux", Options{ConfigPath: path}, append(base, "y")...)
		if err != nil {
			t.Fatalf("run: %v\n%s", err, io.Transcript())
		}
		if strings.Join(cfg.Members[0].Tiers, ",") != "cloud" {
			t.Errorf("tiers = %v", cfg.Members[0].Tiers)
		}
		assertLoadable(t, path, w)
	})
}

func TestInputEndingMidwayStopsWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	_, _, _, err := runWizard(t, "linux", Options{ConfigPath: path}, "1", "Home")
	if !errors.Is(err, ErrInputClosed) {
		t.Fatalf("err = %v, want ErrInputClosed", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a configuration was written from a half-answered wizard")
	}
}

func TestAtLeastOneMemberAndOneEndpoint(t *testing.T) {
	// An empty first name is refused and the question comes round again.
	answers := []string{
		"1", "Home", "household", realToken, "n",
		"", "David", "",
		"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
		"n",
	}
	_, cfg, io, err := runWizard(t, "linux", Options{}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if len(cfg.Members) != 1 {
		t.Fatalf("got %d members", len(cfg.Members))
	}
	if !strings.Contains(io.Transcript(), "At least one person") {
		t.Error("the wizard did not explain why an empty answer was refused")
	}
}

// TestMistypedURLIsCaughtDuringTheQuestion is the whole point of probing as the
// answer is given.
func TestMistypedURLIsCaughtDuringTheQuestion(t *testing.T) {
	answers := []string{
		"1", "Home", "household", realToken, "n",
		"David", "",
		"monster",
		"monster.tail:8000", // no scheme: cannot be dialled at all
		"http://monster.tail:8000/v1",
		"qwen3", "n", "local",
		"n",
	}
	_, cfg, io, err := runWizard(t, "linux", Options{}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Endpoints[0].BaseURL != "http://monster.tail:8000/v1" {
		t.Errorf("base_url = %q", cfg.Endpoints[0].BaseURL)
	}
	if !strings.Contains(io.Transcript(), "http:// or https://") {
		t.Error("the wizard did not say what was wrong with the address")
	}
}

// TestAMachineThatIsOffIsRecordedAnyway is the normal case for this product.
func TestAMachineThatIsOffIsRecordedAnyway(t *testing.T) {
	for _, tc := range []struct {
		state Reachability
		want  string
	}{
		{NoAnswer, "switched off"},
		{Refused, "nothing is listening"},
		{Unresolved, "could not be looked up"},
	} {
		answers := []string{
			"1", "Home", "household", realToken, "n",
			"David", "",
			"monster", "http://monster.tail:8000/v1", "qwen3", "n", "local",
			"n",
		}
		path := filepath.Join(t.TempDir(), DefaultConfigFileName)
		w, cfg, io, err := runWizard(t, "linux",
			Options{ConfigPath: path, Probe: fixedProbe(tc.state)}, answers...)
		if err != nil {
			t.Fatalf("state %v: %v\n%s", tc.state, err, io.Transcript())
		}
		if len(cfg.Endpoints) != 1 {
			t.Fatalf("state %v: the endpoint was not recorded", tc.state)
		}
		if !strings.Contains(io.Transcript(), tc.want) {
			t.Errorf("state %v: the transcript does not contain %q", tc.state, tc.want)
		}
		assertLoadable(t, path, w)
	}
}

func TestNonInteractiveSimple(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFileName)
	io := NewScriptIO() // no answers at all: nothing may be asked
	w := New(io, Options{
		ConfigPath: path,
		GOOS:       "windows",
		Probe:      fixedProbe(Answered),
		LookupEnv:  noEnv,
		Answers: &Answers{
			HouseholdName: "Casa",
			BotToken:      realToken,
			MemberNames:   []string{"David", "María"},
			Endpoints: []EndpointAnswer{
				{Name: "monster", BaseURL: "http://monster.tail:8000/v1", Model: "qwen3"},
				{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "sonnet",
					APIKeyEnv: "OPENROUTER_API_KEY", APIKey: "sk-x"},
			},
			WriteEnvFile: true,
		},
	})
	cfg, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Mode != config.ModeSimple {
		t.Errorf("mode = %q", cfg.Mode)
	}
	if cfg.Household.SharedSpace != DefaultSharedSpace {
		t.Errorf("shared_space = %q, want the default", cfg.Household.SharedSpace)
	}
	// Tiers were derived, and derived privately: a script that says nothing about a
	// member's chain does not widen it.
	for _, m := range cfg.Members {
		if strings.Join(m.Tiers, ",") != "local" {
			t.Errorf("member %s = %v, want the local-only default", m.ID, m.Tiers)
		}
	}
	if got := cfg.Endpoints[1].Tags; strings.Join(got, ",") != "cloud" {
		t.Errorf("the provider was tagged %v, want cloud", got)
	}
	assertLoadable(t, path, w)
	assertNoSecrets(t, path, realToken, "sk-x")

	// The privacy statement is printed for a scripted install too. Somebody reads
	// that output eventually, and it is where the claim is made.
	if !strings.Contains(io.Transcript(), privacy.Statement(privacy.ModeSimple)) {
		t.Error("a scripted install did not print the privacy statement")
	}
}

func TestNonInteractiveIsolatedRefusesOnNonLinux(t *testing.T) {
	w := New(NewScriptIO(), Options{
		ConfigPath: filepath.Join(t.TempDir(), DefaultConfigFileName),
		GOOS:       "darwin",
		Probe:      fixedProbe(Answered),
		LookupEnv:  noEnv,
		Answers: &Answers{
			Mode:        config.ModeIsolated,
			MemberNames: []string{"David"},
			Endpoints:   []EndpointAnswer{{Name: "m", BaseURL: "http://m.local:8000/v1", Model: "q"}},
		},
	})
	_, err := w.Run(context.Background())
	if !errors.Is(err, ErrNotLinux) {
		t.Fatalf("err = %v, want ErrNotLinux: a script that asked for isolated mode must never be given simple mode", err)
	}
}

func TestNonInteractiveIsolatedOnLinux(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	io := NewScriptIO()
	w := New(io, Options{
		ConfigPath: path,
		GOOS:       "linux",
		Probe:      fixedProbe(NoAnswer),
		LookupEnv:  noEnv,
		Answers: &Answers{
			Mode:        config.ModeIsolated,
			MemberNames: []string{"David", "Ana"},
			Endpoints:   []EndpointAnswer{{Name: "monster", BaseURL: "http://monster.tail:8000/v1", Model: "q"}},
			MemberTiers: map[string][]string{"ana": {"local"}},
		},
	})
	cfg, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Members[0].BotTokenEnv != "KENWARD_BOT_TOKEN_DAVID" {
		t.Errorf("David's token variable = %q", cfg.Members[0].BotTokenEnv)
	}
	assertLoadable(t, path, w)
}

func TestNonInteractiveNeedsAMemberAndAnEndpoint(t *testing.T) {
	for name, answers := range map[string]*Answers{
		"no members":   {Endpoints: []EndpointAnswer{{Name: "m", BaseURL: "http://m.local/v1", Model: "q"}}},
		"no endpoints": {MemberNames: []string{"David"}},
	} {
		w := New(NewScriptIO(), Options{
			ConfigPath: filepath.Join(t.TempDir(), DefaultConfigFileName),
			GOOS:       "linux",
			Probe:      fixedProbe(Answered),
			LookupEnv:  noEnv,
			Answers:    answers,
		})
		if _, err := w.Run(context.Background()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDataDirIsWrittenOnlyWhenAskedFor(t *testing.T) {
	withDir := filepath.Join(t.TempDir(), DefaultConfigFileName)
	_, _, _, err := runWizard(t, "linux", Options{ConfigPath: withDir, DataDir: "/var/lib/kenward"}, simpleAnswers()...)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(withDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "data_dir: /var/lib/kenward") {
		t.Error("data_dir was asked for but not written")
	}

	without := filepath.Join(t.TempDir(), DefaultConfigFileName)
	if _, _, _, err := runWizard(t, "linux", Options{ConfigPath: without}, simpleAnswers()...); err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadFile(without)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "data_dir:") {
		t.Error("data_dir was written into a file nobody asked to pin down")
	}
}

// TestEnvVarsAreListedWithoutValues checks the accessor a command would use.
func TestEnvVarsAreListedWithoutValues(t *testing.T) {
	w, _, _, err := runWizard(t, "linux", Options{}, simpleAnswers()...)
	if err != nil {
		t.Fatal(err)
	}
	vars := w.EnvVars()
	if len(vars) != 2 {
		t.Fatalf("got %d variables, want 2", len(vars))
	}
	for _, v := range vars {
		if v.value != "" {
			t.Errorf("EnvVars() handed out the value of %s", v.Name)
		}
		if v.Where == "" || v.Note == "" {
			t.Errorf("%s has no field or note attached", v.Name)
		}
	}
}

// TestSystemdNoteAppearsWhereTheUnitDoes covers the operator who finishes setup and
// immediately opens deploy/kenward.service, which supplies secrets with
// LoadCredential= and no environment file at all.
func TestSystemdNoteAppearsWhereTheUnitDoes(t *testing.T) {
	for _, goos := range []string{"linux", "windows", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			_, _, io, err := runWizard(t, goos, Options{}, simpleAnswers()...)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, io.Transcript())
			}
			mentioned := strings.Contains(io.Transcript(), "LoadCredential=")
			if goos == "linux" && !mentioned {
				t.Error("nothing warned a Linux operator that the shipped unit supplies secrets differently")
			}
			if goos != "linux" && mentioned {
				t.Errorf("a systemd note was printed on %s", goos)
			}
		})
	}
}

// TestSystemdNoteDoesNotTellAnybodyToBreakTheirNode: removing the *_env line in
// favour of a credential is the right shape under that unit, but nothing in the run
// path resolves credentials yet. The note may point at the unit's comments; it may
// not instruct a change that would stop the node starting today.
func TestSystemdNoteDoesNotTellAnybodyToBreakTheirNode(t *testing.T) {
	for _, instruction := range []string{"remove the", "delete the", "instead of bot_token_env"} {
		if strings.Contains(strings.ToLower(systemdNote), instruction) {
			t.Errorf("the systemd note says %q, but the run path still reads only the *_env form", instruction)
		}
	}
	if !strings.Contains(systemdNote, "deploy/kenward.service") {
		t.Error("the note does not say where the authoritative explanation is")
	}
}

// TestTierSummaryReadsThePolicyBack checks the closing summary uses the same
// rendering `kenward doctor` uses, so that the claim made at setup and the claim
// checked afterwards are the same sentence.
func TestTierSummaryReadsThePolicyBack(t *testing.T) {
	answers := append(simpleAnswers()[:len(simpleAnswers())-3], "y", "n", "n")
	_, cfg, io, err := runWizard(t, "linux", Options{}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	transcript := io.Transcript()

	// David opted into a provider; María did not. Both lines are rendered by
	// internal/privacy, and the difference between them is the whole policy.
	if !strings.Contains(transcript, privacy.MemberNote(cfg.Members[0].Domain(), false)) {
		t.Errorf("the summary does not say that David may use a provider:\n%s", transcript)
	}
	if !strings.Contains(transcript, privacy.MemberNote(cfg.Members[1].Domain(), true)) {
		t.Errorf("the summary does not say that María will refuse rather than use one")
	}
	if !strings.Contains(transcript, privacy.TierNote("Casa", cfg.Household.Tiers, true)) {
		t.Error("the summary does not cover the group chat")
	}
}

// assertLoadable puts the written file through the loader that will have to read it
// in production, unknown-field checking and all.
func assertLoadable(t *testing.T, path string, w *Wizard) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the written configuration: %v", err)
	}
	defer f.Close()
	if _, err := config.ParseWithEnv(f, w.validationEnv()); err != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("the written configuration does not load: %v\n\n%s", err, data)
	}
}

// assertNoSecrets checks that nothing setup was told in confidence reached the file
// it wrote.
func assertNoSecrets(t *testing.T, path string, secrets ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(string(data), secret) {
			t.Errorf("a secret was written into %s", filepath.Base(path))
		}
	}
}
