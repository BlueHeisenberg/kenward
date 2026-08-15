// Package setup implements `kenward setup`, the first-run wizard.
//
// It is the first thing a person touches, and it is where most of what kenward is
// for can be lost. A household assistant that promises privacy is believed on the
// strength of a paragraph somebody reads once, at the end of setup, on a machine in
// their kitchen. So this package is written to two rules that outrank tidiness.
//
// The questions are about the household, not the topology. Nothing here asks for a
// numeric Telegram id, a container runtime or a pod layout. It asks who lives here
// and whether they trust whoever runs the machine, and derives the rest.
//
// Nothing it prints may be more reassuring than what the code does. The privacy
// statement for each mode is golden-tested, the simple-mode one says the operator
// can read every member's private memory in those words, and a tier chain that
// leaves the house is never the default and never chosen on somebody's behalf.
//
// The wizard writes a configuration and nothing else. It validates what it built
// with config.Validate before writing it, because a wizard that can emit a file its
// own loader rejects has failed at the only thing it was for.
package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// Defaults offered by the wizard. They are exported so `cmd/kenward` can document
// its non-interactive flags with the same values the questions suggest.
const (
	// DefaultConfigFileName is what setup writes when no path is given, in the
	// working directory, which is where `kenward run` looks first.
	DefaultConfigFileName = "kenward.yaml"
	// DefaultHouseholdName is a placeholder nobody outside the house ever sees.
	DefaultHouseholdName = "Home"
	// DefaultSharedSpace is the lore space the group chat reads and writes.
	DefaultSharedSpace = "household"
	// DefaultBotTokenEnv holds the household bot's token: the whole household's
	// bot in simple mode, the group chat's bot in isolated mode.
	DefaultBotTokenEnv = "KENWARD_BOT_TOKEN"
	// MemberBotTokenPrefix is the prefix for a member's own token variable in
	// isolated mode; the member's id is appended.
	MemberBotTokenPrefix = "KENWARD_BOT_TOKEN"
)

// Tier names the wizard suggests. They are only a convention — a tier is any name
// an endpoint is tagged with and a chain names — but suggesting the same two
// everywhere means one household's configuration can be read by somebody who has
// only seen another's.
const (
	// LocalTier is for machines the household owns.
	LocalTier = "local"
	// CloudTier is for a provider.
	CloudTier = "cloud"
)

// ErrStopped means the operator chose to stop rather than continue.
//
// It is a sentinel so `cmd/kenward` can exit quietly instead of reporting a failure
// at somebody who simply decided to set this up on a different machine. Nothing has
// been written when it is returned.
var ErrStopped = errors.New("setup: stopped at the operator's request")

// ErrNotLinux is returned when isolated mode is asked for non-interactively on a
// platform that cannot provide it.
//
// A script that asked for isolated mode is never quietly given simple mode. That
// substitution is the exact failure this whole product is built to avoid: somebody
// believing their household is sealed when it is not.
var ErrNotLinux = errors.New("setup: isolated mode needs Linux with Podman or Docker")

// pendingEnvValue stands in, during validation only, for a variable the operator has
// been told to set and has not set yet. It is never written or printed.
const pendingEnvValue = "(set before kenward starts)"

// EnvVar is one environment variable the written configuration depends on.
//
// Setup collects these as it goes and prints them at the end, because "kenward will
// not start until these exist" is the single most common way a first run fails, and
// a list is a great deal kinder than a validation error.
type EnvVar struct {
	// Name is the variable itself.
	Name string
	// Where names the field in kenward.yaml that refers to it.
	Where string
	// Note tells the operator where its value is going to come from.
	Note string
	// InEnvFile is true when setup wrote the value into the .env file.
	InEnvFile bool
	// value is the secret, held only long enough to write the .env file and to
	// validate the configuration against the environment the operator is being
	// told to create. It is unexported on purpose: nothing outside this package
	// can read a secret back out of a wizard.
	value string
}

// Options configures a Wizard.
type Options struct {
	// ConfigPath is where kenward.yaml is written. Empty means
	// DefaultConfigFileName in the working directory.
	ConfigPath string
	// DataDir, when set, is written into the file as data_dir. Left empty the file
	// says nothing and kenward uses the platform's own state location, which is
	// the right answer on whatever machine the file ends up on. Containers set it,
	// because a container's home directory is not where anyone expects state.
	DataDir string
	// Force allows setup to replace an existing configuration.
	Force bool
	// GOOS overrides runtime.GOOS. It is what lets the Windows and macOS paths
	// through isolated mode be tested anywhere, and what lets a scripted install
	// generate a configuration for the Linux box it will be copied to.
	GOOS string
	// Probe checks that an endpoint answers. Nil means DefaultProbe.
	Probe Probe
	// LookupEnv reads the environment the configuration is validated against. Nil
	// means the process environment.
	LookupEnv config.LookupEnvFunc
	// Answers, when set, supplies every answer and no question is asked. This is
	// `--non-interactive`, for scripted installs.
	Answers *Answers
}

// Answers is every answer the wizard would have asked for, for scripted installs.
//
// A field left empty takes the same default the interactive question offers, with
// two exceptions that have no safe default and are errors when missing: at least one
// member, and at least one endpoint.
type Answers struct {
	// Mode is simple or isolated. Empty means simple.
	Mode config.Mode
	// HouseholdName and SharedSpace name the group.
	HouseholdName string
	SharedSpace   string
	// BotToken is the household bot's token. It is written to the .env file when
	// WriteEnvFile is set and is never written to the configuration. Empty means
	// the operator will export the variable themselves.
	BotToken string
	// BotTokenEnv names the variable the token is read from. Empty means
	// DefaultBotTokenEnv.
	BotTokenEnv string
	// MemberNames are the people in the household, in the order they will appear
	// in the file. Ids are derived from them.
	MemberNames []string
	// Endpoints are the machines that run the model.
	Endpoints []EndpointAnswer
	// MemberTiers overrides a member's tier chain, keyed by the id derived from
	// their name. A member not named here gets the local-only default, which is
	// the point of the default: a scripted install does not widen anybody's
	// privacy policy by omission.
	MemberTiers map[string][]string
	// GroupTiers overrides the household chain. Empty means the local-only
	// default.
	GroupTiers []string
	// WriteEnvFile asks setup to write the collected secrets to a .env file beside
	// the configuration.
	WriteEnvFile bool
}

// EndpointAnswer is one endpoint, for a scripted install.
type EndpointAnswer struct {
	Name    string
	BaseURL string
	Model   string
	// APIKeyEnv names the variable holding this endpoint's key, if it needs one.
	APIKeyEnv string
	// APIKey is the key itself, written only to the .env file. Empty means the
	// operator will export it themselves.
	APIKey string
	// Tiers are the tier names this endpoint answers for. Empty means LocalTier
	// for a machine on the household's own network and CloudTier for anything
	// else.
	Tiers []string
}

// Wizard runs the first-run flow.
type Wizard struct {
	io    IO
	opts  Options
	goos  string
	probe Probe

	mode      config.Mode
	household config.HouseholdConfig
	telegram  config.TelegramConfig
	members   []config.MemberConfig
	endpoints []config.EndpointConfig
	env       []EnvVar
	// wantEnvFile records that the operator asked for the collected secrets to be
	// written beside the configuration.
	wantEnvFile bool

	configPath  string
	envFilePath string
}

// New returns a Wizard that will ask its questions through io.
func New(io IO, opts Options) *Wizard {
	w := &Wizard{io: io, opts: opts, goos: opts.GOOS, probe: opts.Probe}
	if w.goos == "" {
		w.goos = runtime.GOOS
	}
	if w.probe == nil {
		w.probe = DefaultProbe
	}
	w.configPath = opts.ConfigPath
	if w.configPath == "" {
		w.configPath = DefaultConfigFileName
	}
	return w
}

// ConfigPath is where the configuration was, or will be, written.
func (w *Wizard) ConfigPath() string { return w.configPath }

// EnvFilePath is where the .env file was written, or empty if none was.
func (w *Wizard) EnvFilePath() string { return w.envFilePath }

// EnvVars lists the environment variables the written configuration depends on,
// with a note about where each one's value comes from. The values themselves are
// not exposed.
func (w *Wizard) EnvVars() []EnvVar {
	out := make([]EnvVar, len(w.env))
	for i, v := range w.env {
		v.value = ""
		out[i] = v
	}
	return out
}

// Run asks the questions, writes the files, and returns the configuration it wrote.
//
// The context bounds the endpoint probes and is checked between steps. It cannot
// interrupt a question: a blocking read on a terminal does not cancel, and
// pretending otherwise would mean returning from Run while the operator is still
// typing into a wizard that is no longer listening.
func (w *Wizard) Run(ctx context.Context) (*config.Config, error) {
	if w.opts.Answers != nil {
		if err := w.collectScripted(ctx, w.opts.Answers); err != nil {
			return nil, err
		}
		return w.finish()
	}

	w.io.Print(Banner)
	steps := []func(context.Context) error{
		w.askMode,
		w.askHousehold,
		w.askTelegram,
		w.askMembers,
		w.askEndpoints,
		w.askTiers,
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := step(ctx); err != nil {
			return nil, err
		}
	}
	return w.finish()
}

// build assembles the configuration from what has been collected.
func (w *Wizard) build() *config.Config {
	cfg := &config.Config{
		Mode:      w.mode,
		DataDir:   w.opts.DataDir,
		Household: w.household,
		Telegram:  w.telegram,
		Members:   w.members,
		Endpoints: w.endpoints,
		Memory:    config.MemoryConfig{LoreCommand: DefaultLoreCommand},
	}
	// Everything with a default is written out explicitly rather than left to the
	// loader. The generated file is the only documentation of these values most
	// households will ever read, and a value you can see is a value you can change.
	cfg.Memory.SearchLimit = config.DefaultSearchLimit
	cfg.Session.IdleTimeout = config.Duration(config.DefaultIdleTimeout)
	cfg.Capture.MaxProposalsPerTurn = config.DefaultMaxProposalsPerTurn
	cfg.Update.Channel = config.DefaultUpdateChannel
	cfg.Update.CheckInterval = config.Duration(config.DefaultCheckInterval)
	cfg.ApplyDefaults()
	return cfg
}

// finish validates what was collected, writes it, and says what happens next.
func (w *Wizard) finish() (*config.Config, error) {
	cfg := w.build()

	// The configuration is judged by the package that will have to load it, against
	// the environment the operator is being told to create. If this ever fails it
	// is a defect in the wizard rather than a mistake by the person answering: no
	// answer to any question above is supposed to be able to produce a file kenward
	// will not load, and the tests assert exactly that over every path through the
	// flow.
	if err := cfg.Validate(w.validationEnv()); err != nil {
		return nil, fmt.Errorf("setup: the answers produced a configuration kenward would refuse, which is a bug in setup rather than in your answers: %w", err)
	}

	data, err := marshalDocument(documentFor(cfg, w.opts.DataDir != ""))
	if err != nil {
		return nil, err
	}
	if err := writeFile(w.configPath, data, configFileMode, w.opts.Force); err != nil {
		return nil, err
	}
	if err := w.writeEnvFile(); err != nil {
		return nil, err
	}

	w.blank()
	w.io.Print(PrivacyStatement(cfg.Mode))
	w.blank()
	w.io.Print(w.closing())
	return cfg, nil
}

// writeEnvFile writes the collected secrets beside the configuration, if that is
// what was asked for and there is anything to write.
func (w *Wizard) writeEnvFile() error {
	if !w.wantEnvFile {
		return nil
	}
	var withValues []EnvVar
	for _, v := range w.env {
		if v.value != "" {
			withValues = append(withValues, v)
		}
	}
	if len(withValues) == 0 {
		return nil
	}

	path := filepath.Join(filepath.Dir(w.configPath), EnvFileName)
	err := writeFile(path, renderEnvFile(withValues), envFileMode, w.opts.Force)
	switch {
	case errors.Is(err, ErrExists):
		// An existing .env is somebody else's file and may hold secrets for
		// something entirely different. Appending to it blind, or replacing it,
		// are both worse than saying so — and the token is not printed here, or
		// anywhere, so the operator is told what to add rather than shown it.
		w.blank()
		w.io.Print(fmt.Sprintf(`  %s already exists, so it has been left alone. Add these variables to it
  yourself, or delete it and run setup again:

%s`, path, indentNames(withValues)))
		return nil
	case err != nil:
		return err
	}

	w.envFilePath = path
	for i := range w.env {
		if w.env[i].value != "" {
			w.env[i].InEnvFile = true
			w.env[i].Note = "in the .env file just written"
		}
	}
	return nil
}

func indentNames(vars []EnvVar) string {
	var b strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&b, "      %s\n", v.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// closing is the last thing setup prints: what it wrote, what still has to exist
// before kenward will start, and the two or three commands to run next.
func (w *Wizard) closing() string {
	var b strings.Builder
	b.WriteString("Written\n\n")
	fmt.Fprintf(&b, "  %s\n", w.configPath)
	if w.envFilePath != "" {
		fmt.Fprintf(&b, "  %s   holds your secrets, readable only by you, already in .gitignore\n", w.envFilePath)
	}

	if len(w.env) > 0 {
		b.WriteString("\nkenward will not start until these are set\n\n")
		width := 0
		for _, v := range w.env {
			if len(v.Name) > width {
				width = len(v.Name)
			}
		}
		for _, v := range w.env {
			fmt.Fprintf(&b, "  %-*s   %s\n", width, v.Name, v.Note)
		}
	}

	b.WriteString("\nNext\n\n")
	fmt.Fprintf(&b, "  kenward doctor%s   checks every part of this and says what is not working\n", strings.Repeat(" ", inviteWidth(w.members)-len("kenward doctor")))
	for _, m := range w.members {
		cmd := "kenward invite --name " + m.Name
		fmt.Fprintf(&b, "  %s%s   gives %s a code to claim their account\n",
			cmd, strings.Repeat(" ", inviteWidth(w.members)-len(cmd)), m.Name)
	}
	fmt.Fprintf(&b, "  kenward run%s   starts the node\n", strings.Repeat(" ", inviteWidth(w.members)-len("kenward run")))
	return strings.TrimRight(b.String(), "\n")
}

// inviteWidth is the column the descriptions in the closing block line up at.
func inviteWidth(members []config.MemberConfig) int {
	width := len("kenward doctor")
	for _, m := range members {
		if n := len("kenward invite --name " + m.Name); n > width {
			width = n
		}
	}
	return width
}

// validationEnv is the environment the finished configuration is judged against.
//
// It is the process environment overlaid with every variable setup has just told
// the operator to create. That is deliberate, and it is the honest reading of what
// is being validated: the wizard is checking the document it is about to write, not
// the shell it happens to be running in. In isolated mode it could not be otherwise
// — each member's bot token is created during that member's own enrolment, days
// after setup runs, and refusing to write the file until they exist would make
// isolated mode impossible to set up at all.
//
// What the overlay cannot hide is a structural fault: a tier no endpoint serves, two
// members sharing a private space, a missing shared space. Those are the faults a
// wizard can actually cause, and they are caught here before anything is written.
// The variables themselves are listed for the operator in the closing block, and
// `kenward doctor` checks them against the real environment afterwards.
func (w *Wizard) validationEnv() config.LookupEnvFunc {
	base := w.opts.LookupEnv
	if base == nil {
		base = os.LookupEnv
	}
	declared := make(map[string]string, len(w.env))
	for _, v := range w.env {
		if v.value != "" {
			declared[v.Name] = v.value
		} else {
			declared[v.Name] = pendingEnvValue
		}
	}
	return func(name string) (string, bool) {
		if v, ok := declared[name]; ok {
			return v, true
		}
		return base(name)
	}
}

// recordEnv notes a variable the configuration will depend on.
func (w *Wizard) recordEnv(name, where, note, value string) {
	for i := range w.env {
		if w.env[i].Name == name {
			if value != "" {
				w.env[i].value = value
				w.env[i].Note = note
			}
			return
		}
	}
	w.env = append(w.env, EnvVar{Name: name, Where: where, Note: note, value: value})
}

// blank prints an empty line.
func (w *Wizard) blank() { w.io.Print("") }
