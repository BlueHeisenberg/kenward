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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

// Defaults offered by the wizard. They are exported so `cmd/kenward` can document
// its non-interactive flags with the same values the questions suggest.
const (
	// DefaultConfigFileName is what setup writes when no path is given, in the
	// working directory, which is where `kenward run` looks first.
	DefaultConfigFileName = "kenward.yaml"
	// DefaultHouseholdName is a placeholder nobody outside the house ever sees.
	DefaultHouseholdName = "Home"
	// DefaultSharedSpace was the name the wizard used to suggest for the shared
	// space. It is no longer a usable answer and nothing here defaults to it: a
	// space is identified by its lore id, and there is no id anybody can guess.
	// It survives only so that a caller's flag definition still compiles.
	//
	// Deprecated: ask for a space id. See Answers.SharedSpace.
	DefaultSharedSpace = "household"
	// DefaultBotTokenEnv holds the household bot's token: the whole household's
	// bot in simple mode, the group chat's bot in isolated mode.
	DefaultBotTokenEnv = "KENWARD_BOT_TOKEN"
	// MemberBotTokenPrefix is the prefix for a member's own token variable in
	// isolated mode; the member's id is appended.
	MemberBotTokenPrefix = "KENWARD_BOT_TOKEN"
	// DefaultLinkKeyEnv holds the household link key: the one secret every unit of
	// an isolated household shares, and what tells the group's pod that a store
	// asking to be admitted to the household's shared space is one of its own. See
	// internal/link.
	//
	// Setup generates the value rather than asking for one. There is no decision in
	// it — it is thirty-two random bytes whose only requirement is being the same in
	// every pod, which one variable already guarantees — and the alternative is a
	// household that has to invent a secret before its members can share memory.
	DefaultLinkKeyEnv = "KENWARD_LINK_KEY"
	// MemberPassphrasePrefix is the prefix for the variable holding the passphrase
	// that wraps a member's key in isolated mode; the member's id is appended. One
	// per member, never one for the household: in that mode each key is wrapped
	// under its own passphrase, and a shared one would be simple mode's custody
	// under isolated mode's name.
	MemberPassphrasePrefix = "KENWARD_PASSPHRASE"
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
	// Telegram asks Telegram which bot a token belongs to, and whether it can hear a
	// group chat. Nil means DefaultTelegramProbe.
	Telegram TelegramProbe
	// Spaces lists the lore spaces this machine's lore home holds. Nil means
	// asking the real lore. It is only consulted to check ids a scripted install
	// supplied, and to find a space again when setup is re-run.
	Spaces SpaceLister
	// CreateSpace makes one shared lore space. Nil means asking the real lore.
	CreateSpace SpaceMaker
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
//
// There is deliberately no file or credential form here, though the runtime reads
// both. A path field would be the wrong shape for the deployment that wants it: on a
// systemd host the best configuration names no source at all, because the credential
// lookup is automatic and making the operator repeat the name in kenward.yaml only
// creates a second place for the unit and the configuration to disagree. So the
// answer that would earn its place is not "here is a path" but "state no source, the
// unit supplies credentials" — and that one cannot be validated at setup time
// without inventing a credentials directory on a machine that has none, which is a
// larger claim than this package should make on somebody's behalf. Whoever adds it
// should add that answer, print the LoadCredential= lines the household needs, and
// not a BotTokenFile string.
type Answers struct {
	// Mode is simple or isolated. Empty means simple.
	Mode config.Mode
	// HouseholdName is the group's display name.
	HouseholdName string
	// SharedSpace is the id of the shared lore space the group chat reads and
	// writes. Empty means make one — which is the ordinary case, and what lets a
	// scripted install be one command with no lore in it. It used to be required,
	// which meant an installer had to create the space out of band first.
	//
	// Supplied, it must be an id rather than a display name: lore does not make
	// names unique, so a name here fails on the first retrieval rather than at
	// startup. The dashboard's wizard supplies ids, of spaces it made itself.
	SharedSpace string
	// BotToken is the household bot's token. It is written to the .env file when
	// WriteEnvFile is set and is never written to the configuration. Empty means
	// the operator will export the variable themselves.
	BotToken string
	// BotTokenEnv names the variable the token is read from. Empty means
	// DefaultBotTokenEnv.
	BotTokenEnv string
	// Agents is the identity question: one assistant for the household, or one each.
	// Empty means config.DefaultAgents, which is one — today's behaviour, and the
	// answer a scripted install that has not thought about it should get.
	Agents config.Agents
	// GroupChatID is the Telegram id of the household's own group. Required under
	// Agents == config.AgentsPerMember and optional otherwise: with one assistant
	// each, kenward speaks only in the group, so a configuration without one has no
	// kenward in it and is refused rather than written.
	GroupChatID int64
	// Persona is kenward's own: the language, tone and character the household's
	// assistant writes in. Every field's zero value is what kenward has always done,
	// so a scripted install that says nothing gets English, the flat register and no
	// character.
	//
	// There is deliberately no per-member persona here. A member's persona is theirs
	// to write in the Telegram tutorial, and an install script filling one in on
	// somebody's behalf would be the one part of this design that is nobody else's
	// to decide.
	Persona config.PersonaConfig
	// MemberNames are the people in the household, in the order they will appear
	// in the file. Member ids are derived from them.
	MemberNames []string
	// MemberSpaces is the id of each member's private lore space, keyed by the id
	// derived from their name. One is required per member: a private space holds
	// that person and this machine, so it cannot be derived or created here.
	MemberSpaces map[string]string
	// Endpoints are the machines that run the model.
	Endpoints []EndpointAnswer
	// MemberBotTokens and MemberPassphrases are each member's own two secrets in
	// isolated mode, keyed by the id derived from their name: the token of the bot
	// only they speak on, and the passphrase that wraps their key and nobody else's.
	// Both are written to the .env file when WriteEnvFile is set and neither is ever
	// written to the configuration, which names the variables and holds no values.
	//
	// A member left out of either map is one whose value the caller is not supplying,
	// which is a legitimate answer — the terminal wizard never supplies one, because at
	// a terminal these arrive later: the member makes their bot when they enrol and
	// whoever runs their pod chooses the passphrase. What is not legitimate is a
	// wizard that names four variables and mentions none of them, which is what the
	// dashboard used to do; that refusal lives where the asking happens, in the form,
	// not here. This package's job is only to carry a value it was given.
	MemberBotTokens   map[string]string
	MemberPassphrases map[string]string
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
	// ContextWindow is the window this endpoint actually has, where the caller
	// knows it. Zero means it does not, and config.DefaultContextWindow applies.
	//
	// The terminal wizard never fills this in — it probes whether an address
	// answers, not what the server behind it was started with. The dashboard
	// wizard does: it reads /v1/models, and the number vLLM publishes there is
	// the one that binds. Dropping it silently gave a 262144-token machine a
	// 16384-token configuration, with nothing on any screen saying so.
	ContextWindow int
}

// Wizard runs the first-run flow.
type Wizard struct {
	io       IO
	opts     Options
	goos     string
	probe    Probe
	botProbe TelegramProbe
	lister   SpaceLister
	maker    SpaceMaker

	// The lore listing, fetched once. Spaces are chosen by id, so every question
	// about one is answered from the same snapshot.
	spaceList    []memory.Space
	spaceErr     error
	spacesLoaded bool
	takenSpaces  map[string]bool
	// spaceLabels are the display names already used for a space in this run, so
	// that two people with one name do not get one space.
	spaceLabels map[string]bool

	mode      config.Mode
	household config.HouseholdConfig
	// agents and persona are the identity step's answers: how many assistants this
	// household has, and what kenward's own writing is like. Both are household
	// configuration; a member's own persona is written by the member in Telegram and
	// this wizard never touches one.
	agents    config.Agents
	persona   config.PersonaConfig
	telegram  config.TelegramConfig
	members   []config.MemberConfig
	endpoints []config.EndpointConfig
	// historyReset is history.reset_every: how often a conversation's recent turns
	// are dropped. Zero is off and is what pressing Enter gives.
	historyReset config.Duration
	env          []EnvVar
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
	w.botProbe = opts.Telegram
	if w.botProbe == nil {
		w.botProbe = DefaultTelegramProbe
	}
	w.lister = opts.Spaces
	if w.lister == nil {
		w.lister = defaultSpaceLister
	}
	w.maker = opts.CreateSpace
	if w.maker == nil {
		w.maker = defaultSpaceMaker
	}
	w.takenSpaces = map[string]bool{}
	w.spaceLabels = map[string]bool{}
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
		w.askIdentity,
		w.askEndpoints,
		w.askTiers,
		w.askHistory,
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
	household := w.household
	household.Agents = w.agents
	household.Persona = w.persona
	cfg := &config.Config{
		Mode:      w.mode,
		DataDir:   w.opts.DataDir,
		Household: household,
		Telegram:  w.telegram,
		Members:   w.members,
		Endpoints: w.endpoints,
	}
	// No memory.lore_command in either mode. Nothing kenward runs looks a lore binary
	// up — see config.MemoryConfig — and the generated file used to carry the key in
	// isolated mode, which taught isolated households they had to install one.
	//
	// Everything with a default is written out explicitly rather than left to the
	// loader. The generated file is the only documentation of these values most
	// households will ever read, and a value you can see is a value you can change.
	cfg.Memory.SearchLimit = config.DefaultSearchLimit
	announceReads := true
	cfg.Memory.AnnounceReads = &announceReads
	cfg.Session.IdleTimeout = config.Duration(config.DefaultIdleTimeout)
	// Whatever askHistory collected, which is zero unless somebody typed otherwise.
	// Written out either way, for the same reason idle_timeout is: an off that is
	// visible in the file is a knob the household knows it has.
	cfg.History.ResetEvery = w.historyReset
	cfg.Capture.MaxProposalsPerTurn = config.DefaultMaxProposalsPerTurn
	cfg.Capture.PrivateWrites = config.DefaultPrivateWrites
	cfg.Update.Channel = config.DefaultUpdateChannel
	cfg.Update.CheckInterval = config.Duration(config.DefaultCheckInterval)
	cfg.ApplyDefaults()
	return cfg
}

// finish validates what was collected, writes it, and says what happens next.
func (w *Wizard) finish() (*config.Config, error) {
	w.addLinkKey()
	cfg := w.build()

	// The configuration is judged by the package that will have to load it, against
	// the environment the operator is being told to create. Reached from the terminal
	// wizard, a failure here is a defect in the wizard rather than a mistake by the
	// person answering: no answer to any question above is supposed to be able to
	// produce a file kenward will not load, and the tests assert exactly that over
	// every path through the flow.
	//
	// Reached from Answers it is the opposite claim, and making it would be a lie: a
	// scripted install can say anything the schema forbids — a persona longer than the
	// limit, a tier no endpoint answers for — and telling that operator they have found
	// a bug in setup sends them to the wrong file.
	if err := cfg.Validate(w.validationEnv()); err != nil {
		if w.opts.Answers != nil {
			return nil, fmt.Errorf("setup: the answers given produce a configuration kenward would refuse: %w", err)
		}
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
	w.io.Print(privacyBlock(cfg.Mode, cfg.Household.Agents))
	w.blank()
	w.io.Print(w.tierSummary())
	w.blank()
	w.io.Print(w.closing())
	return cfg, nil
}

// addLinkKey names and generates the household link key for an isolated household.
//
// It is the one secret setup mints instead of asking for, and the reason is that
// asking would be theatre: its value carries no meaning, it is never shown to
// anybody, and the only property it needs is being identical in every pod — which
// one environment variable gives it for free. What it buys is the last manual step
// in isolated mode: without it a person has to run `lore space invite` and `lore
// join` inside two containers, once per member, before the household's shared
// memory reaches anybody.
//
// Simple mode gets none. There is one lore home there holding every space, so there
// is no second store to admit and nothing for a key to authenticate.
//
// A household that already named a source keeps it: this is a default, not an
// override, and a scripted install pointing at a file is a decision somebody made.
func (w *Wizard) addLinkKey() {
	if w.mode != config.ModeIsolated {
		return
	}
	if w.household.LinkKeyEnv != "" || w.household.LinkKeyFile != "" {
		return
	}
	w.household.LinkKeyEnv = DefaultLinkKeyEnv
	w.recordEnv(DefaultLinkKeyEnv, "household.link_key_env",
		"generated by setup — it lets each member's pod be admitted to the household's shared memory. "+
			"Any value works as long as every pod gets the same one",
		newLinkKey())
}

// newLinkKey is thirty-two bytes of crypto/rand, base64url so it survives a .env
// file, a compose environment block and a shell without quoting.
//
// Entropy that cannot be read is not a case worth a code path — crypto/rand does
// not fail on any platform kenward runs on — and a fallback here would be a
// household whose gate opens for a predictable value.
func newLinkKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("setup: no entropy for the household link key: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// tierSummary reads the privacy policy back off the configuration that was just
// written, one line per space.
//
// It is the same rendering `kenward doctor` uses, from the same package, because
// this is the line somebody checks a claim against: "David will refuse rather than
// use a provider" is either true of the file or it is not, and it should say the
// same thing in both places six months apart.
func (w *Wizard) tierSummary() string {
	plan := w.tierPlan()
	inHouse := make(map[string]bool, len(plan.local))
	for _, tier := range plan.local {
		inHouse[tier] = true
	}
	staysHome := func(chain []string) bool {
		if len(chain) == 0 {
			return false
		}
		for _, tier := range chain {
			if !inHouse[tier] {
				return false
			}
		}
		return true
	}

	var b strings.Builder
	b.WriteString("Where each conversation may go\n\n")
	for _, m := range w.members {
		fmt.Fprintf(&b, "  %s\n", privacy.MemberNote(m.Domain(), staysHome(m.Tiers)))
	}
	label := w.household.Name
	if label == "" {
		label = w.household.SharedSpace
	}
	fmt.Fprintf(&b, "  %s\n", privacy.TierNote(label, w.household.Tiers, staysHome(w.household.Tiers)))
	return strings.TrimRight(b.String(), "\n")
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
	// Force is deliberately not passed on. It means "replace the configuration I
	// wrote", and .env is not that file: `.env` is the name compose, systemd and half
	// the tools on the machine already read, so the one beside the config is quite
	// likely to hold a database password that has nothing to do with kenward.
	// Truncating it would destroy secrets nobody has a copy of, to save the operator
	// pasting three lines. Merging was the alternative and was not taken: it means
	// parsing a format with no specification, and getting quoting or a duplicate key
	// wrong there breaks the file just as thoroughly, only quietly. So the wizard
	// never overwrites an existing .env, which is also what docs/CLI.md promises.
	err := writeFile(path, renderEnvFile(withValues), envFileMode, false)
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
		fmt.Fprintf(&b, "  %s\n", w.envFilePath)
		b.WriteString("      your secrets, readable only by you, already excluded by .gitignore\n")
	}

	if len(w.env) > 0 {
		b.WriteString("\nkenward will not start until these are set\n\n")
		width := 0
		for _, v := range w.env {
			if n := utf8.RuneCountInString(v.Name); n > width {
				width = n
			}
		}
		for _, v := range w.env {
			fmt.Fprintf(&b, "  %s   %s\n", pad(v.Name, width), v.Note)
		}
	}

	// Only on Linux: the unit this refers to is a systemd unit, and a note about
	// systemd on a Windows machine is noise in the one block somebody actually
	// reads to the end.
	if w.goos == "linux" && len(w.env) > 0 {
		b.WriteString("\n")
		b.WriteString(systemdNote)
		b.WriteString("\n")
	}

	b.WriteString("\nNext\n\n")
	commands := [][2]string{
		{"kenward doctor", "checks every part of this and says what is not working"},
	}
	for _, m := range w.members {
		commands = append(commands, [2]string{
			"kenward invite --name " + m.Name,
			fmt.Sprintf("gives %s a code to claim their account", m.Name),
		})
	}
	commands = append(commands, [2]string{"kenward run", "starts the node"})

	width := 0
	for _, c := range commands {
		if n := utf8.RuneCountInString(c[0]); n > width {
			width = n
		}
	}
	for _, c := range commands {
		fmt.Fprintf(&b, "  %s   %s\n", pad(c[0], width), c[1])
	}
	return strings.TrimRight(b.String(), "\n")
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
