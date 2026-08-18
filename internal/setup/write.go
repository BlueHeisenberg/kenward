package setup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// ErrExists is returned when setup would have to overwrite a file it did not write.
//
// A household's configuration is hand-edited and full of decisions somebody made
// once; running the wizard again by accident must not be a way to lose them. The
// caller can pass Options.Force when overwriting is what was actually meant.
var ErrExists = errors.New("setup: file already exists")

// EnvFileName is the file setup offers to write secrets into, beside the
// configuration.
//
// The name is not arbitrary: `.env` is already in this repository's .gitignore, it
// is what `docker compose --env-file` and systemd's EnvironmentFile= both read, and
// it is the file everybody already knows not to commit.
const EnvFileName = ".env"

// File modes. Both files are readable only by their owner. The configuration holds
// no secrets, but it does describe who lives in the house and which machines they
// talk to, and no other account on a shared machine has any business reading that.
const (
	configFileMode = 0o600
	envFileMode    = 0o600
)

// configHeader is the first thing anybody opening kenward.yaml reads.
const configHeader = `# kenward.yaml — written by ` + "`kenward setup`" + `.
#
# Hand-edit it freely. kenward validates the whole file when it starts and reports
# everything wrong with it at once, so an edit is one sitting rather than one fault
# per restart.
#
# Nothing here is a secret. Bot tokens and API keys are named by environment
# variable and read from the environment; no value ever lands in this file.
#
# Keep it out of version control all the same — it says who lives here.
`

// envFileHeader is the first thing anybody opening the .env file reads.
const envFileHeader = `# Written by ` + "`kenward setup`" + `. This file holds secrets.
#
# It is created readable only by you, and .gitignore already excludes it. Load it
# before starting kenward — "set -a; . ./.env; set +a" in a shell, EnvironmentFile=
# in a systemd unit, or env_file: in a compose file.
`

// document is the YAML shape setup writes.
//
// It carries the *_env half of every secret and never the *_file half. A secret may
// name exactly one source — file, environment variable, or a systemd credential
// found without configuration — and naming two is a validation error rather than an
// order of preference, so this is a choice rather than an omission.
//
// The environment form is written because it is the one form that works in every
// deployment: a shell, a compose file's env_file, a systemd unit with an
// EnvironmentFile. The file form needs a path that already exists with mode 0600,
// which the wizard would have to ask for and could not sensibly create; the
// credential form needs systemd. All three run — the runtime resolves each secret
// through config's accessors at the point of use — but which mechanism delivers a
// secret is not a question a household can answer, and asking it would trade the
// one answer that always works for three that sometimes do. An operator who has a
// better answer is editing this file by hand, and the closing block tells them
// exactly what to change.
//
// It mirrors config.Config rather than being it, for one reason: omitempty. A
// generated file that spells out group_chat_id: 0 and bot_token_env: "" for every
// member is a file that reads as machine output, and the first thing an operator
// does with it is hand-edit it. The mirror is kept honest by a test that decodes
// what was written with config.Decode and compares every field against the
// configuration the wizard built, so a field added to the schema and forgotten here
// fails the build rather than going quietly missing.
type document struct {
	Mode      config.Mode   `yaml:"mode"`
	DataDir   string        `yaml:"data_dir,omitempty"`
	Household householdDoc  `yaml:"household"`
	Telegram  telegramDoc   `yaml:"telegram"`
	Members   []memberDoc   `yaml:"members"`
	Endpoints []endpointDoc `yaml:"endpoints"`
	Memory    memoryDoc     `yaml:"memory"`
	History   historyDoc    `yaml:"history"`
	Session   sessionDoc    `yaml:"session"`
	Capture   captureDoc    `yaml:"capture"`
	Update    updateDoc     `yaml:"update"`
	// Dashboard is a pointer so that the block is omitted entirely from a household
	// that has never opened the admin dashboard. An absent block is off, and a file
	// that spells out "enabled: false" invites somebody to flip it without reading
	// what exposure means. See dashboarddoc.go.
	Dashboard *dashboardDoc `yaml:"dashboard,omitempty"`
}

type householdDoc struct {
	Name        string        `yaml:"name"`
	Agents      config.Agents `yaml:"agents"`
	SharedSpace string        `yaml:"shared_space"`
	GroupChatID int64         `yaml:"group_chat_id,omitempty"`
	Tiers       []string      `yaml:"tiers,flow"`
	// Persona is omitted entirely from a household that chose the default, because
	// every field's empty value is a meaning — English, the flat register, no
	// character — and three empty strings in the file say that less clearly than
	// their absence does. kenward.example.yaml is where the keys are documented.
	Persona personaDoc `yaml:"persona,omitempty"`
}

// personaDoc is the four wording settings. agent_name is written for a member and
// never for the household, which has no name to choose: config.Validate refuses one.
type personaDoc struct {
	AgentName string `yaml:"agent_name,omitempty"`
	Language  string `yaml:"language,omitempty"`
	Tone      string `yaml:"tone,omitempty"`
	Character string `yaml:"character,omitempty"`
}

func personaDocFor(p config.PersonaConfig) personaDoc {
	return personaDoc{
		AgentName: p.AgentName,
		Language:  p.Language,
		Tone:      p.Tone,
		Character: p.Character,
	}
}

type telegramDoc struct {
	BotTokenEnv string `yaml:"bot_token_env,omitempty"`
}

type memberDoc struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	TelegramID   int64    `yaml:"telegram_id,omitempty"`
	PrivateSpace string   `yaml:"private_space"`
	Tiers        []string `yaml:"tiers,flow"`
	BotTokenEnv  string   `yaml:"bot_token_env,omitempty"`
	// Persona is a member's own, and the wizard never sets it: it is written in the
	// Telegram tutorial by the member themselves. It is carried here so that a
	// configuration rewritten from the dashboard's settings page keeps what a member
	// chose, rather than a form that never asked about it dropping it silently.
	Persona personaDoc `yaml:"persona,omitempty"`
	// PassphraseEnv is written beside the token because in isolated mode a pod
	// needs both to serve anybody: the bot nobody else speaks on, and the
	// passphrase that unwraps that member's key and no other member's.
	PassphraseEnv string `yaml:"passphrase_env,omitempty"`
}

type endpointDoc struct {
	Name      string          `yaml:"name"`
	BaseURL   string          `yaml:"base_url"`
	Model     string          `yaml:"model"`
	APIKeyEnv string          `yaml:"api_key_env,omitempty"`
	Tags      []string        `yaml:"tags,flow"`
	Timeout   config.Duration `yaml:"timeout"`
	// ContextWindow and MaxCompletionTokens are always written out, exactly as
	// timeout is, whether they were learned or defaulted. The terminal wizard
	// learns neither — it probes whether an address answers, not what the server
	// behind it was started with — so there they are the defaults, and the point
	// of writing them is to put the keys where the operator will see them. A
	// machine bought for the size of its window is wasted silently otherwise.
	// The dashboard wizard does learn the window, off /v1/models, and it arrives
	// here through EndpointAnswer.ContextWindow.
	ContextWindow       int `yaml:"context_window"`
	MaxCompletionTokens int `yaml:"max_completion_tokens"`
}

type memoryDoc struct {
	SearchLimit   int   `yaml:"search_limit"`
	AnnounceReads *bool `yaml:"announce_reads,omitempty"`
}

// historyDoc is the conversation's own lifetime, written next to memory and
// deliberately not inside it: one is lore and the other is the last few minutes of
// chat, and this file is where that distinction is easiest to lose.
type historyDoc struct {
	ResetEvery config.Duration `yaml:"reset_every"`
}

type sessionDoc struct {
	IdleTimeout config.Duration `yaml:"idle_timeout"`
}

type captureDoc struct {
	MaxProposalsPerTurn int                  `yaml:"max_proposals_per_turn"`
	PrivateWrites       config.PrivateWrites `yaml:"private_writes"`
}

type updateDoc struct {
	Channel       config.UpdateChannel `yaml:"channel"`
	CheckInterval config.Duration      `yaml:"check_interval"`
}

// documentFor projects a validated configuration into the file that will be
// written.
func documentFor(cfg *config.Config, writeDataDir bool) document {
	doc := document{
		Mode: cfg.Mode,
		Household: householdDoc{
			Name:        cfg.Household.Name,
			Agents:      cfg.Household.Agents,
			SharedSpace: cfg.Household.SharedSpace,
			GroupChatID: cfg.Household.GroupChatID,
			Tiers:       cfg.Household.Tiers,
			Persona:     personaDocFor(cfg.Household.Persona),
		},
		Telegram: telegramDoc{BotTokenEnv: cfg.Telegram.BotTokenEnv},
		Memory: memoryDoc{
			SearchLimit:   cfg.Memory.SearchLimit,
			AnnounceReads: cfg.Memory.AnnounceReads,
		},
		History: historyDoc{ResetEvery: cfg.History.ResetEvery},
		Session: sessionDoc{IdleTimeout: cfg.Session.IdleTimeout},
		Capture: captureDoc{
			MaxProposalsPerTurn: cfg.Capture.MaxProposalsPerTurn,
			PrivateWrites:       cfg.Capture.PrivateWrites,
		},
		Update: updateDoc{
			Channel:       cfg.Update.Channel,
			CheckInterval: cfg.Update.CheckInterval,
		},
	}
	doc.Dashboard = dashboardDocFor(cfg.Dashboard)
	// data_dir is written only when somebody asked for one. Left out, kenward uses
	// the platform's own state location, which is the right answer on every machine
	// the file might be copied to; written out, it is one absolute path that is
	// wrong everywhere else.
	if writeDataDir {
		doc.DataDir = cfg.DataDir
	}
	for _, m := range cfg.Members {
		doc.Members = append(doc.Members, memberDoc{
			ID:            m.ID,
			Name:          m.Name,
			TelegramID:    m.TelegramID,
			PrivateSpace:  m.PrivateSpace,
			Tiers:         m.Tiers,
			BotTokenEnv:   m.BotTokenEnv,
			Persona:       personaDocFor(m.Persona),
			PassphraseEnv: m.PassphraseEnv,
		})
	}
	for _, e := range cfg.Endpoints {
		doc.Endpoints = append(doc.Endpoints, endpointDoc{
			Name:      e.Name,
			BaseURL:   e.BaseURL,
			Model:     e.Model,
			APIKeyEnv: e.APIKeyEnv,
			Tags:      e.Tags,
			Timeout:   e.Timeout,

			ContextWindow:       e.ContextWindow,
			MaxCompletionTokens: e.MaxCompletionTokens,
		})
	}
	return doc
}

// sectionNotes are the comments written above each top-level section. They say what
// the section is for in one line, so somebody opening the file six months later can
// find the part they came for without the documentation.
var sectionNotes = [][2]string{
	{"household", "The household itself, and where its shared conversations may go."},
	{"telegram", "Tokens are named here, never written here."},
	{"members", "One entry per person. telegram_id appears once they claim an invite."},
	{"endpoints", "The machines that run the model. tags are the tier names to route by."},
	{"memory", "lore, which owns everything kenward remembers."},
	{"history", "The last few turns of each conversation. Not memory; 0s never drops them."},
	{"session", "How long an unlocked key stays in memory without use."},
	{"capture", "How often the assistant may ask to remember something."},
	{"update", "stable, edge or off. off is a supported way to run kenward forever."},
}

// marshalDocument renders the configuration file, comments and all.
func marshalDocument(doc document) ([]byte, error) {
	var node yaml.Node
	if err := node.Encode(doc); err != nil {
		return nil, fmt.Errorf("setup: encoding configuration: %w", err)
	}
	for _, note := range sectionNotes {
		setHeadComment(&node, note[0], note[1])
	}

	var buf bytes.Buffer
	buf.WriteString(configHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return nil, fmt.Errorf("setup: encoding configuration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("setup: encoding configuration: %w", err)
	}
	return buf.Bytes(), nil
}

// setHeadComment attaches a comment above one key of a mapping node. A missing key
// is not an error: the comment is a courtesy, and a section that was omitted has
// nothing to be courteous about.
func setHeadComment(mapping *yaml.Node, key, comment string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			// The leading newline is what yaml.v3 renders as a blank line before
			// the comment, which is what separates the sections visually.
			mapping.Content[i].HeadComment = "\n" + comment
			return
		}
	}
}

// writeFile writes data to path, refusing to replace a file it did not write unless
// force is set.
func writeFile(path string, data []byte, mode os.FileMode, force bool) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("setup: creating %s: %w", dir, err)
		}
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, path)
		}
		return fmt.Errorf("setup: creating %s: %w", path, err)
	}
	// Chmod as well as passing the mode to OpenFile, because OpenFile's mode is
	// filtered through the umask and an existing file being rewritten under force
	// keeps whatever mode it already had. Windows has no mode bits worth the name,
	// and failing there would make setup unusable on the platform a good share of
	// simple-mode households run.
	if err := f.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return fmt.Errorf("setup: setting permissions on %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("setup: writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("setup: writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("setup: writing %s: %w", path, err)
	}
	return nil
}

// renderEnvFile builds the contents of the .env file from the secrets that were
// actually given. Variables whose value setup was not told stay out of it entirely:
// writing NAME= for them would produce a file that sets a variable to nothing,
// which config.Validate rejects for the same reason kenward would fail on it.
func renderEnvFile(vars []EnvVar) []byte {
	var b bytes.Buffer
	b.WriteString(envFileHeader)
	b.WriteString("\n")
	for _, v := range vars {
		if v.value == "" {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", v.Name, quoteEnvValue(v.value))
	}
	return b.Bytes()
}

// quoteEnvValue quotes a value for the shells and parsers that read a .env file.
//
// Bot tokens and API keys never need it, but a value that did and was written raw
// would produce a file that silently sets the wrong thing, and finding that out
// costs an hour.
func quoteEnvValue(v string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:+=@-"
	if v != "" && strings.IndexFunc(v, func(r rune) bool { return !strings.ContainsRune(safe, r) }) < 0 {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
