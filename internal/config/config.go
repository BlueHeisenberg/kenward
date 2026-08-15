// Package config loads, defaults and validates kenward.yaml.
//
// The file is the whole of kenward's operator-facing surface, so the rules here are
// deliberately unforgiving: unknown fields are an error rather than a warning, every
// referenced environment variable must already be set, and validation reports every
// problem it can find in one pass. An operator editing a household's configuration
// should be able to fix it in a single sitting rather than discovering one fault per
// restart.
//
// Configuration never holds a secret. Bot tokens and API keys are named by environment
// variable only; nothing in this package stores, logs or formats a key value.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode selects how the household's units are run.
type Mode string

const (
	// ModeSimple runs every member's unit as a goroutine in one process behind a
	// single bot token. Simple to operate; the isolation is logical, not enforced.
	ModeSimple Mode = "simple"
	// ModeIsolated runs one pod per member, each with its own bot token, lore
	// instance and key.
	ModeIsolated Mode = "isolated"
)

// UpdateChannel selects which releases the node will apply, if any.
type UpdateChannel string

const (
	// UpdateStable is the default: released versions only.
	UpdateStable UpdateChannel = "stable"
	// UpdateEdge takes pre-releases.
	UpdateEdge UpdateChannel = "edge"
	// UpdateOff never updates. It is a fully supported way to run kenward forever.
	UpdateOff UpdateChannel = "off"
)

// Default values applied before validation. They are exported so `kenward doctor` and
// the setup wizard can show an operator what they will get if they say nothing.
const (
	DefaultSearchLimit         = 8
	DefaultIdleTimeout         = 30 * time.Minute
	DefaultMaxProposalsPerTurn = 1
	DefaultUpdateChannel       = UpdateStable
	DefaultCheckInterval       = 6 * time.Hour
	DefaultEndpointTimeout     = 120 * time.Second
)

// LookupEnvFunc reads an environment variable, with the same shape as os.LookupEnv.
//
// It is injectable so that validation can be tested exhaustively without a test
// process mutating the real environment, which is global state shared with every other
// test running in the same binary.
type LookupEnvFunc func(string) (string, bool)

// Config is the parsed kenward.yaml, one file per household.
type Config struct {
	Mode Mode `yaml:"mode"`
	// DataDir is where kenward keeps the mutable state it writes about itself, which
	// today is the enrolment bindings in state.json and nothing else. Empty means
	// DefaultDataDir.
	DataDir   string           `yaml:"data_dir"`
	Household HouseholdConfig  `yaml:"household"`
	Telegram  TelegramConfig   `yaml:"telegram"`
	Members   []MemberConfig   `yaml:"members"`
	Endpoints []EndpointConfig `yaml:"endpoints"`
	Memory    MemoryConfig     `yaml:"memory"`
	Session   SessionConfig    `yaml:"session"`
	Capture   CaptureConfig    `yaml:"capture"`
	Update    UpdateConfig     `yaml:"update"`
}

// HouseholdConfig describes the group itself.
type HouseholdConfig struct {
	Name string `yaml:"name"`
	// SharedSpace is the lore space every member can read and the group chat writes
	// to. It may never coincide with a member's private space.
	SharedSpace string `yaml:"shared_space"`
	// GroupChatID is the one Telegram group mapped to the shared space. Zero means no
	// group is configured yet; in that case no chat resolves to a group scope.
	GroupChatID int64 `yaml:"group_chat_id"`
	// Tiers is the ordered endpoint tier chain for group conversations.
	Tiers []string `yaml:"tiers"`
}

// TelegramConfig holds the household-wide bot binding used in simple mode.
type TelegramConfig struct {
	// BotTokenEnv names the environment variable holding the token. Required in
	// simple mode, where one bot serves the whole household; unused in isolated mode,
	// where each member's pod carries its own token.
	BotTokenEnv string `yaml:"bot_token_env"`
}

// MemberConfig is one human in the household.
type MemberConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	// TelegramID is zero until the member has claimed an invite. Several members may
	// sit at zero at once, which is the normal state of a freshly created household.
	TelegramID int64 `yaml:"telegram_id"`
	// PrivateSpace is the member's two-member lore space: them and the node.
	PrivateSpace string `yaml:"private_space"`
	// Tiers is the ordered tier chain this member's private conversations may use. A
	// chain naming only local tiers is what makes "this never leaves the house" true:
	// when nothing in it is reachable, kenward refuses instead of widening.
	Tiers []string `yaml:"tiers"`
	// BotTokenEnv names this member's own bot token variable. Isolated mode only,
	// where it is required and must differ from every other member's.
	BotTokenEnv string `yaml:"bot_token_env"`
	// EnrolledAt is filled in from the state file by MergeState and is deliberately
	// not readable from the YAML: when a member claimed their invite is something that
	// happened, not something an operator declares. Writing enrolled_at in the file is
	// an unknown-field error, which is the right answer to someone trying.
	EnrolledAt time.Time `yaml:"-"`
}

// EndpointConfig is one OpenAI-compatible backend and the tiers it belongs to.
type EndpointConfig struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	// APIKeyEnv names the environment variable holding the key. The key itself never
	// appears in configuration. Empty for endpoints that need no authentication,
	// which is the usual case for a machine on the household's own network.
	APIKeyEnv string `yaml:"api_key_env"`
	// Tags are the tier names this endpoint answers for.
	Tags []string `yaml:"tags"`
	// Timeout bounds one completion. Defaults to DefaultEndpointTimeout.
	Timeout Duration `yaml:"timeout"`
}

// MemoryConfig configures the lore client.
type MemoryConfig struct {
	// LoreCommand is the argv used to start lore's MCP server.
	LoreCommand []string `yaml:"lore_command"`
	// SearchLimit is the per-space retrieval budget for one turn.
	SearchLimit int `yaml:"search_limit"`
}

// SessionConfig configures key lifetime.
type SessionConfig struct {
	// IdleTimeout is how long an unlocked key stays in memory without use.
	IdleTimeout Duration `yaml:"idle_timeout"`
}

// CaptureConfig bounds how often the assistant may ask to remember something.
type CaptureConfig struct {
	// MaxProposalsPerTurn caps capture questions per turn. The default of one exists
	// because an assistant that asks repeatedly trains members to dismiss it.
	MaxProposalsPerTurn int `yaml:"max_proposals_per_turn"`
}

// UpdateConfig configures self-update.
type UpdateConfig struct {
	Channel UpdateChannel `yaml:"channel"`
	// CheckInterval is how often the node looks for a new release. Ignored when the
	// channel is off.
	CheckInterval Duration `yaml:"check_interval"`
}

// Load reads a kenward.yaml from disk, folds in the recorded enrolment state, and
// validates the result against the process environment.
func Load(path string) (*Config, error) {
	return LoadWithEnv(path, os.LookupEnv)
}

// LoadWithEnv is Load with an injected environment lookup, for tests and for `doctor`
// checking a configuration against an environment other than its own.
//
// The order is load, merge, then validate: validation judges the configuration kenward
// will actually run with, bindings included, rather than the one written in the file.
func LoadWithEnv(path string, lookupEnv LookupEnvFunc) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: opening %s: %w", path, err)
	}
	defer f.Close()

	cfg, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	st, err := LoadState(cfg.StatePath())
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	// Merge problems and validation problems are reported together, for the same
	// reason every other problem is: one pass, one list, one edit.
	if err := joinValidation(cfg.MergeState(st), cfg.Validate(lookupEnv)); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// joinValidation folds several validation results into one, so a caller doing more than
// one check still hands the operator a single list.
func joinValidation(errs ...error) error {
	joined := &ValidationError{}
	for _, err := range errs {
		if err == nil {
			continue
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			// Not a validation problem at all: return it unchanged rather than
			// flattening it into a list of strings and losing its type.
			return err
		}
		joined.Problems = append(joined.Problems, ve.Problems...)
		joined.MissingEnv = append(joined.MissingEnv, ve.MissingEnv...)
	}
	if len(joined.Problems) == 0 {
		return nil
	}
	return joined
}

// Parse reads and validates a configuration from r against the process environment.
func Parse(r io.Reader) (*Config, error) {
	return ParseWithEnv(r, os.LookupEnv)
}

// ParseWithEnv is Parse with an injected environment lookup.
//
// It reads no state file: it judges the document alone, which is what the setup wizard
// and tests want. Anything about to serve a household calls Load, which merges the
// recorded enrolments first.
//
// It returns a *ValidationError when the document is well-formed YAML but describes a
// household that cannot be served. Syntax and unknown-field problems are reported
// separately, because they are faults in the file rather than in the configuration it
// describes.
func ParseWithEnv(r io.Reader, lookupEnv LookupEnvFunc) (*Config, error) {
	cfg, err := Decode(r)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(lookupEnv); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Decode parses a configuration and applies defaults without validating it.
//
// It exists for the setup wizard, which builds a configuration interactively and needs
// to hold an incomplete one, and for tests. Anything that is about to serve a household
// calls Load or Parse instead.
func Decode(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	// An unknown field is nearly always a typo in a field that matters — a misspelled
	// private_space would silently give a member no space at all. Refusing to start is
	// the only safe reading.
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty document is not a syntax error; it is a configuration with
			// nothing in it, and validation will say so in the operator's terms.
			cfg.ApplyDefaults()
			return &cfg, nil
		}
		return nil, fmt.Errorf("config: parsing yaml: %w", err)
	}
	cfg.ApplyDefaults()
	return &cfg, nil
}

// ApplyDefaults fills in every value that has one. It is idempotent, and always runs
// before validation so that validation judges the configuration kenward will actually
// use rather than the one that was written down.
func (c *Config) ApplyDefaults() {
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir()
	}
	if c.Memory.SearchLimit == 0 {
		c.Memory.SearchLimit = DefaultSearchLimit
	}
	if c.Session.IdleTimeout == 0 {
		c.Session.IdleTimeout = Duration(DefaultIdleTimeout)
	}
	if c.Capture.MaxProposalsPerTurn == 0 {
		c.Capture.MaxProposalsPerTurn = DefaultMaxProposalsPerTurn
	}
	if c.Update.Channel == "" {
		c.Update.Channel = DefaultUpdateChannel
	}
	if c.Update.CheckInterval == 0 {
		c.Update.CheckInterval = Duration(DefaultCheckInterval)
	}
	for i := range c.Endpoints {
		if c.Endpoints[i].Timeout == 0 {
			c.Endpoints[i].Timeout = Duration(DefaultEndpointTimeout)
		}
	}
}

// Duration is a time.Duration written in YAML the way an operator writes one: "120s",
// "30m", "6h". YAML has no duration scalar, and decoding into a bare time.Duration
// would silently read "30" as thirty nanoseconds.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the duration in the same form it is written in the file.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string, rejecting anything Go's parser will not take
// and anything negative.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a quoted-or-bare string such as \"120s\", \"30m\" or \"6h\"", node.Line)
	}
	if strings.TrimSpace(s) == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration; use a form such as \"120s\", \"30m\" or \"6h\"", node.Line, s)
	}
	if v < 0 {
		return fmt.Errorf("line %d: duration %q is negative", node.Line, s)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes the duration back in string form, so a configuration written by
// the setup wizard reads like one written by hand.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
