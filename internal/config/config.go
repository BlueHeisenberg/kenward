// Package config loads, defaults and validates kenward.yaml.
//
// The file is the whole of kenward's operator-facing surface, so the rules here are
// deliberately unforgiving: unknown fields are an error rather than a warning, every
// referenced environment variable must already be set, and validation reports every
// problem it can find in one pass. An operator editing a household's configuration
// should be able to fix it in a single sitting rather than discovering one fault per
// restart.
//
// Configuration never holds a secret. Bot tokens and API keys are named — by
// environment variable, by file path, or by nothing at all when systemd supplies them as
// a credential — and resolved on demand through the accessors in secret.go; nothing in
// this package stores, logs or formats a key value.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	// The IANA timezone database, embedded. reminders.timezone names a zone and the
	// node has to resolve it, but Windows ships no zoneinfo and a minimal Linux
	// container often does not either — so without this a household that wrote
	// "Europe/Madrid" would be refused on exactly the hosts kenward is meant to run
	// on. It is stdlib and costs a few hundred kilobytes of binary, which is the
	// cheapest correct answer available.
	_ "time/tzdata"

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

// Agents selects how many assistants a household has: one for everybody, or one per
// member alongside the household's own.
//
// It is not Mode and must never be described as though it were. Mode is a security
// question — can the operator read a member's private plaintext — and it is answered by
// topology. This is a presentation question, about who the household is talking to.
// Conflating them is the one mistake this pair of settings exists to prevent: a
// member's own bot is a separate contact, not a separate secret, and internal/privacy
// says so in those words.
//
// The two meet in exactly one place, and it is arithmetic rather than privacy. An
// agent is a Telegram contact; simple mode runs one bot for the whole household; two
// agents behind one contact are one agent. So AgentsPerMember needs ModeIsolated,
// where every member already has a bot of their own, and validateHousehold refuses the
// other pair with that reason rather than downgrading it. AgentPerMember is the one
// predicate that answers the question, and nothing compares this field to decide
// anything.
type Agents string

const (
	// AgentsShared is today's behaviour and the default: there is kenward and
	// nothing else. Everyone talks to it, in private chats and in the group, and the
	// household persona is therefore everyone's persona.
	AgentsShared Agents = "shared"
	// AgentsPerMember gives every member their own named agent, and kenward remains
	// the family agent in the group chat.
	AgentsPerMember Agents = "per_member"
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
	DefaultSearchLimit = 8
	// DefaultIdleTimeout is zero: idle expiry is off unless a household turns it on.
	//
	// It mirrors session.DefaultIdleTimeout and must keep mirroring it, because this is
	// the value the run path actually hands the session manager. A passphrase never
	// travels over Telegram (D-019), so a member whose key is expired has no way back
	// in — the only re-unlock is somebody at the machine. A timeout therefore does not
	// degrade an idle member, it silently stops their assistant answering; and the
	// claim that survives either way is the one that matters, that nothing is readable
	// from a disk, a backup, or a process nobody has unlocked. Zero is not "unset" here
	// and is not rewritten: see ApplyDefaults.
	DefaultIdleTimeout = time.Duration(0)
	// DefaultHistoryReset is zero: a conversation's recent turns are cleared when the
	// node restarts and at no other time, unless a household asks for a schedule.
	//
	// Off rather than a sensible daily reset because turning it on changes what the
	// assistant appears to know mid-week, and no household that has not asked for
	// that should discover it. It is also the value every configuration written
	// before this key existed means.
	DefaultHistoryReset = time.Duration(0)
	// MaxHistoryReset is the longest interval history.reset_every may state.
	//
	// Boundaries are anchored to local midnight (assistant.historyBoundary), so an
	// interval longer than a day cannot be honoured: it would fall back to one reset
	// per midnight while the file claimed something else. Refusing it is better than
	// quietly meaning a different thing, and a household wanting less than daily is
	// asking for off.
	MaxHistoryReset            = 24 * time.Hour
	DefaultMaxProposalsPerTurn = 1
	// DefaultPrivateWrites is the decided behaviour: a note to a member's own space
	// is written, then shown to them with an undo button. A household wanting the
	// old question back sets capture.private_writes: ask.
	DefaultPrivateWrites = PrivateWriteSave
	// DefaultAgents is one assistant for the whole household, which is what kenward
	// has always done and what every configuration written before household.agents
	// existed means.
	DefaultAgents        = AgentsShared
	DefaultUpdateChannel = UpdateStable
	DefaultCheckInterval = 6 * time.Hour
	// DefaultRemindersMaxPerDay is how many unprompted messages one conversation may
	// receive in a day. Six is a handful: enough for a morning routine and a few
	// one-offs, few enough that nobody reaches for the mute button.
	//
	// Zero is "unset" here and is rewritten to this, unlike session.idle_timeout —
	// the off value is a negative number precisely so that the absence of the key and
	// a household's deliberate silence stay distinguishable.
	DefaultRemindersMaxPerDay = 6
	// DefaultRemindersCatchUp is how late a repeating occurrence may be delivered
	// after the node has been off. Six hours carries a morning reminder across a node
	// that booted late and drops one from the day before, which is the line most
	// households would draw. It does not bound a one-off; see internal/remind.
	DefaultRemindersCatchUp = 6 * time.Hour
	// DefaultRemindersMaxStored caps how many reminders one conversation may hold.
	DefaultRemindersMaxStored = 20
	DefaultEndpointTimeout    = 120 * time.Second
	// DefaultContextWindow is the context window assumed for an endpoint that does
	// not state one, in tokens as the assistant estimates them.
	//
	// 16384 rather than a number matching the largest machines, because this is the
	// figure used for an endpoint nobody described, and the two ways of being wrong
	// are not symmetrical. Too small only wastes a window somebody paid for; too
	// large overflows the server mid-conversation and turns into a provider error in
	// front of a member. So the default is the floor of what is still in ordinary
	// service — Phi-4 holds 16k, Qwen2.5 32k, Llama 3.x and Gemma 3 128k, and the
	// hosted providers far more — and an operator running something smaller, or
	// anything larger and wanting the rest of it, says so on the endpoint.
	DefaultContextWindow = 16384
	// DefaultMaxCompletionTokens caps a reply from an endpoint that does not state
	// its own cap, and is reserved out of the context window.
	//
	// 4096 rather than something tighter because a reasoning model spends this
	// budget on hidden tokens before it emits any content. Measured against a local
	// Qwen3 endpoint with an ordinary household question — work out the yearly
	// running cost of three appliances — the old 1024 cap returned a complete
	// reasoning trace, no content at all, and finish_reason "length"; the same
	// request at 4096 answered in full, having spent 1379 tokens. An empty
	// completion is not read as a short answer: the router treats it as an endpoint
	// that failed, cools a perfectly healthy machine, and refuses the turn naming
	// the tier. 4096 covers a short trace and a real answer after it, while still
	// leaving three quarters of the default window for the prompt.
	DefaultMaxCompletionTokens = 4096
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
	// DataDir is where kenward keeps the mutable state it writes about itself: the
	// enrolment bindings and personas in state.json, outstanding invites and
	// revocations, the dashboard's admin record and its private key, and sessions.json,
	// which holds every member's wrapped key. A backup of this directory is a backup of
	// the household's key material and must be treated as one. Empty means
	// DefaultDataDir.
	DataDir   string           `yaml:"data_dir"`
	Household HouseholdConfig  `yaml:"household"`
	Telegram  TelegramConfig   `yaml:"telegram"`
	Members   []MemberConfig   `yaml:"members"`
	Endpoints []EndpointConfig `yaml:"endpoints"`
	Memory    MemoryConfig     `yaml:"memory"`
	History   HistoryConfig    `yaml:"history"`
	Session   SessionConfig    `yaml:"session"`
	Capture   CaptureConfig    `yaml:"capture"`
	Update    UpdateConfig     `yaml:"update"`
	// Reminders bounds proactive messages: the timezone they are stated in, how many
	// a conversation may receive in a day, and how late a missed one may be.
	Reminders RemindersConfig `yaml:"reminders"`
	// Dashboard configures the admin dashboard's HTTP server. Its zero value is off,
	// which is what every configuration written before it existed means. See
	// dashboard.go.
	Dashboard DashboardConfig `yaml:"dashboard"`
}

// AgentName is what the household's own assistant calls itself, everywhere and
// always. It is not configurable: kenward is the product, a household that renames it
// has renamed the thing its documentation, its logs and its `kenward doctor` output all
// talk about, and the name a member actually wants to change is their own agent's —
// which is members[].persona.agent_name and exists for exactly that.
const AgentName = "kenward"

// Persona field limits. Member-written text reaches a system prompt, so its size is a
// trust boundary rather than a matter of taste: the budget loop trims retrieved entries
// and never trims the persona, so an unbounded character would push the scope
// disclosure and the capture rules out of a small endpoint's window — which is the one
// way persona text could countermand them without the model ever being asked to.
//
// Counted in runes, so a household writing in Greek or Japanese gets the same room as
// one writing in English.
const (
	// MaxPersonaLine bounds agent_name, language and tone, each of which is one
	// short phrase.
	MaxPersonaLine = 80
	// MaxPersonaCharacter bounds character, which is prose.
	MaxPersonaCharacter = 1000
)

// PersonaConfig is how one agent writes: the four settings a household or a member
// chooses. Every field's zero value reproduces kenward's behaviour before this section
// existed — English, the flat register, no character, and the name kenward — so a
// configuration that says nothing about persona is unchanged by it.
//
// The same shape serves both layers on purpose. A member's onboarding writes the fields
// under members[].persona and the admin's wizard writes them under household.persona,
// and the two must not drift into different vocabularies for the same four questions.
type PersonaConfig struct {
	// AgentName is what this member's agent calls itself. Empty means AgentName.
	//
	// It is meaningful only under household.agents: per_member, and is otherwise
	// carried and ignored rather than refused — a household that tries one-each and
	// goes back should not lose the names its members chose, and should not have to
	// delete them by hand before the file will load.
	//
	// It is not the Telegram bot's display name, which is global and set in
	// BotFather. This is what the agent calls itself in conversation, which is why
	// per-member names work however many bots exist.
	AgentName string `yaml:"agent_name,omitempty" json:"agent_name,omitempty"`
	// Language is the language this agent writes in, named the way a person names one
	// — "Spanish", "español", "Brazilian Portuguese". It is free text and not a code,
	// because it is passed to the model rather than looked up in a table, and a
	// household is entitled to ask for a register of a language kenward has never
	// heard of.
	//
	// Empty means English. It reaches two places and they are honestly different.
	//
	// It is passed to the model, verbatim, which is why it is free text: a household
	// is entitled to ask for a register of a language kenward has never heard of and
	// the model will do its best. And it selects the node's own strings — the capture
	// announcements, the refusals, the retrieval line, the reminder notices, the
	// locked notice, the memory model explained at enrolment — from internal/lang,
	// which is a closed list of languages somebody has written and read. A language
	// outside that list still reaches the model and leaves the node's own strings in
	// English; lang.Spoken is the predicate for which of the two a value gets.
	//
	// The four setup questions in the enrolment tutorial are a third, shorter list
	// again (internal/enrol), and say so to the member's face when they cannot hold
	// the conversation. See docs/PROMPT.md.
	Language string `yaml:"language,omitempty" json:"language,omitempty"`
	// Tone is the register, in a phrase: "warm", "very terse", "formal usted".
	// Empty means the flat register docs/PROMPT.md specifies, which is the default
	// and remains the default.
	Tone string `yaml:"tone,omitempty" json:"tone,omitempty"`
	// Character is free prose about who this agent is. Empty — the default — means
	// there is no character, and the prompt then says so in the words it always did.
	//
	// It is the one field of the four that is really an invitation to write anything,
	// so it is the one that carries the risk: it enters a system prompt, and text
	// arriving unmarked in a system prompt reads as instruction. It is rendered
	// delimited and indented, exactly as a retrieved entry's body is, and the prompt
	// states that it governs wording and cannot countermand anything else.
	Character string `yaml:"character,omitempty" json:"character,omitempty"`
}

// IsZero reports whether this persona asks for nothing, which is the default and is
// today's behaviour exactly.
func (p PersonaConfig) IsZero() bool {
	return p.AgentName == "" && p.Language == "" && p.Tone == "" && p.Character == ""
}

// HouseholdPersona is kenward's own persona, as the group conversation gets it: the
// household's three fields, and the name kenward, which is not a setting.
func (c *Config) HouseholdPersona() PersonaConfig {
	p := c.Household.Persona
	p.AgentName = AgentName
	return p
}

// PersonaFor resolves the persona a member's private conversation is served with.
//
// Under AgentsShared the voice is the household's — one assistant means one name, one
// register and one character, in the group chat and in every private chat alike — and
// the language is the member's own, falling back to the household's. That split is
// deliberate and it is the one exception to "there is no personal layer".
//
// The reason is that persona bundles two unlike things. Name, tone and character are
// identity: an assistant that is a flat clerk to one member and a wry ship's captain to
// another is two assistants wearing one name, which is the distinction AgentsPerMember
// exists to sell. Language is not identity. It is a property of a conversation, the same
// person answers in whichever language they are addressed in, and the prompt already
// concedes as much — a persona with no language leaves the model mirroring whatever the
// member writes, which is what English-by-saying-nothing has always meant.
//
// What a shared persona actually discarded was never the model's language. It was the
// chrome: Options.Language reaches internal/capture, and internal/capture decides the
// button labels, the write announcement, the undo hint, the alias line folded into a
// stored body, and whether the English-gloss line renders at all. A Spanish member of a
// default household got model prose in Spanish wrapped in English buttons, with the
// gloss and the aliases suppressed outright because Engine.english was true. That is not
// one assistant, it is one assistant half of whose interface the member cannot read —
// and the tutorial asked them their language before doing it.
//
// Structurally it is free: a unit is built per member in every mode, each with its own
// prompt and its own capture engine, so nothing here needs a topology that simple mode
// cannot deliver. Contrast AgentName, which does: see internal/enrol's step list.
//
// Under AgentsPerMember the member's own fields win, one field at a time, and an empty
// field falls back to the household's. Per field rather than all-or-nothing because the
// alternative makes a member who wrote a character lose the household's language, which
// is a thing nobody would ask for and nobody would notice they had done.
//
// An unknown member id resolves to the household persona rather than to nothing: a
// caller with an id no member owns is asking about a conversation that should not be
// served at all, and answering with kenward's own voice is the reading that cannot
// leak somebody else's.
//
// The question is asked through AgentPerMember and never by comparing the field here.
// There is one predicate for "does every member have an agent of their own", and it
// answers false in simple mode whatever the file says; a second reading of the field
// would give a member a persona for an agent that mode cannot deliver.
func (c *Config) PersonaFor(memberID string) PersonaConfig {
	household := c.HouseholdPersona()
	for _, m := range c.Members {
		if m.ID != memberID {
			continue
		}
		if !c.AgentPerMember() {
			// The language, and nothing else. Written onto the household's persona
			// rather than assembled fresh so that a field added to PersonaConfig
			// later joins the household's voice by default, which is the answer
			// that cannot accidentally split the assistant in two.
			household.Language = orElse(m.Persona.Language, household.Language)
			return household
		}
		return PersonaConfig{
			AgentName: orElse(m.Persona.AgentName, household.AgentName),
			Language:  orElse(m.Persona.Language, household.Language),
			Tone:      orElse(m.Persona.Tone, household.Tone),
			Character: orElse(m.Persona.Character, household.Character),
		}
	}
	return household
}

func orElse(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// HouseholdConfig describes the group itself.
type HouseholdConfig struct {
	Name string `yaml:"name"`
	// Agents is the identity question: one assistant for the household, or one each.
	// Empty means AgentsShared, which is what every configuration written before this
	// key existed means.
	Agents Agents `yaml:"agents"`
	// Persona is kenward's own: the language, tone and character it writes the group
	// chat in. It is household configuration rather than a private choice because
	// kenward is visible to everyone who uses the group chat.
	//
	// Under AgentsShared it is also every member's persona, because there is no
	// personal layer to override it. Both wizards say so at the point of asking.
	Persona PersonaConfig `yaml:"persona"`
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
	// BotTokenEnv names the environment variable holding the token. One of this,
	// BotTokenFile or the systemd credential named by CredentialBotToken is required
	// in simple mode, where one bot serves the whole household, and in isolated mode
	// whenever household.group_chat_id is set: the members get their own bots there,
	// but the group conversation still runs on the household bot. Isolated mode with
	// no group chat needs no household token at all.
	BotTokenEnv string `yaml:"bot_token_env"`
	// BotTokenFile names a file holding the token, for the deployments where an
	// environment variable is the wrong place for one. See secret.go. Stating it as
	// well as BotTokenEnv is an error, not a precedence.
	BotTokenFile string `yaml:"bot_token_file"`
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
	// Persona is this member's own: their agent's name, and the language, tone and
	// character it writes their private conversation in. It is written by the member
	// in the Telegram tutorial rather than by the admin in the wizard, which is the
	// same split every other per-member setting already follows.
	//
	// A field left empty falls back to the household's, per field, rather than to a
	// constant: a member who chose a character and said nothing about language keeps
	// the household's language. See PersonaFor.
	Persona PersonaConfig `yaml:"persona"`
	// Tiers is the ordered tier chain this member's private conversations may use. A
	// chain naming only local tiers is what makes "this never leaves the house" true:
	// when nothing in it is reachable, kenward refuses instead of widening.
	Tiers []string `yaml:"tiers"`
	// BotTokenEnv names this member's own bot token variable. Isolated mode only,
	// where the token is required and must differ from every other member's.
	BotTokenEnv string `yaml:"bot_token_env"`
	// BotTokenFile names a file holding this member's token — the form a pod wants,
	// where an environment value is readable by every process in the container. One
	// source only: stating this and BotTokenEnv is an error. With neither, the
	// credential named by MemberBotTokenCredential(ID) is used if systemd supplied
	// one. See secret.go.
	BotTokenFile string `yaml:"bot_token_file"`
	// PassphraseEnv names the variable holding the passphrase that wraps this
	// member's session key. Isolated mode only, and required there: each member's
	// key is wrapped under their own passphrase (session.ModeIsolated), so a pod
	// that is handed no passphrase can unwrap nothing and answers every private
	// message with the locked notice. It must differ from every other member's for
	// the same reason their bot tokens must — one passphrase across two members is
	// one wrapping secret across two members.
	PassphraseEnv string `yaml:"passphrase_env"`
	// PassphraseFile names a file holding it — the form a pod wants, where an
	// environment value is readable by every process in the container and is
	// inherited by the `lore` subprocess. One source only: stating this and
	// PassphraseEnv is an error. With neither, the credential named by
	// MemberPassphraseCredential(ID) is used if systemd supplied one. See secret.go.
	PassphraseFile string `yaml:"passphrase_file"`
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
	// APIKeyFile names a file holding the key instead. One source only: stating this
	// and APIKeyEnv is an error. With neither, the credential named by
	// EndpointAPIKeyCredential(Name) is used if systemd supplied one. See secret.go.
	APIKeyFile string `yaml:"api_key_file"`
	// Tags are the tier names this endpoint answers for.
	Tags []string `yaml:"tags"`
	// Timeout bounds one completion. Defaults to DefaultEndpointTimeout.
	Timeout Duration `yaml:"timeout"`
	// ContextWindow is how many tokens this endpoint can hold in one request,
	// prompt and completion together. Defaults to DefaultContextWindow.
	//
	// It is stated per endpoint because it is a fact about the machine and the
	// model it serves, not a household policy: the number is whatever the server
	// was started with — vLLM's --max-model-len, llama.cpp's -c, ollama's num_ctx
	// — which is frequently smaller than the window the model advertises. A
	// conversation's budget is derived from it as the smallest window across the
	// endpoints its tier chain reaches; see ChainLimits.
	ContextWindow int `yaml:"context_window"`
	// MaxCompletionTokens caps one reply and is reserved out of ContextWindow.
	// Defaults to DefaultMaxCompletionTokens.
	//
	// Per endpoint for the same reason the window is, and it is not merely a
	// question of taste in reply length: a reasoning model spends this budget on
	// hidden tokens before it writes a word the member will see, so a cap sized
	// for a plain instruct model makes a reasoning model stop having thought and
	// said nothing. Raise it on the endpoints that think.
	MaxCompletionTokens int `yaml:"max_completion_tokens"`
}

// ChainLimits reports what a conversation on this tier chain has to fit inside: the
// smallest context window, and the smallest completion cap, of any endpoint the chain
// reaches.
//
// Both are minima because the prompt is assembled before the router picks an endpoint,
// so the turn may land on any endpoint in the chain and must fit the smallest of them.
// A tier chain that reaches nothing returns zeroes, and the caller takes its own
// defaults; validation refuses such a chain long before this matters.
//
// The two minima cannot contradict each other. Validation requires every endpoint's cap
// to be smaller than its own window, and the endpoint holding the smallest window
// contributes a cap no larger than its own — so maxTokens is always below
// contextWindow, and the assistant's construction check can never fire on numbers
// derived here.
func (c *Config) ChainLimits(chain []string) (contextWindow, maxTokens int) {
	for _, e := range c.Endpoints {
		if !chainReaches(chain, e.Tags) {
			continue
		}
		if e.ContextWindow > 0 && (contextWindow == 0 || e.ContextWindow < contextWindow) {
			contextWindow = e.ContextWindow
		}
		if e.MaxCompletionTokens > 0 && (maxTokens == 0 || e.MaxCompletionTokens < maxTokens) {
			maxTokens = e.MaxCompletionTokens
		}
	}
	return contextWindow, maxTokens
}

// MemoryConfig configures the lore client.
//
// There is no lore_command here, and there is no mode in which there is one. The key
// located a binary for kenward to execute, and kenward executes none: the store is
// opened in this process through lore's Go API, and since a1cbd0a the cross-pod sync
// daemon — the last subprocess, and the last reason the key survived removal from
// simple mode — runs in this process too, on the store already open. See
// cmd/kenward's startSyncDaemon and internal/memory/sync.go.
//
// The published image still carries the lore CLI, for the `space invite` / `join`
// handshake an operator runs by hand inside a pod. That is a program a person types,
// not one kenward looks up, and it needs no key in a file kenward validates.
type MemoryConfig struct {
	// SearchLimit is the per-space retrieval budget for one turn.
	SearchLimit int `yaml:"search_limit"`
	// AnnounceReads says whether each reply is prefixed with a line naming the
	// memories that were searched and how many entries reached the answer.
	//
	// It is a pointer because the default is true and a bare bool cannot tell "not
	// stated" from "stated false". Unset means on.
	//
	// This one is a setting where the write announcement is not, and the asymmetry
	// is deliberate: a read changes nothing, so a household that finds the line
	// noisy loses nothing but the line by turning it off. Use AnnouncesReads to
	// read it.
	AnnounceReads *bool `yaml:"announce_reads"`
}

// AnnouncesReads reports whether replies carry the retrieval line. Unset is on.
func (m MemoryConfig) AnnouncesReads() bool { return m.AnnounceReads == nil || *m.AnnounceReads }

// HistoryConfig governs the short-term conversation history: the handful of recent
// turns that ride in the prompt so the assistant can follow a thread.
//
// It is a section of its own, next to `memory` rather than inside it, because the two
// are opposites and being told them apart is the whole point. `memory` is lore — what
// the household decided to keep, permanent by design, written only through the capture
// flow. This is the transcript of the last few minutes, in RAM, never written to lore,
// already lost on every restart.
type HistoryConfig struct {
	// ResetEvery is how often a conversation's recent turns are dropped, anchored to
	// local midnight: "6h" clears at 00:00, 06:00, 12:00 and 18:00, and "24h" at
	// midnight. Zero — the default — never drops them on a schedule.
	//
	// The reset happens on the first message after a boundary passes, not on a timer,
	// and the member is told when it drops anything. Nothing in lore is touched.
	//
	// It is not session.idle_timeout, though both are durations in this file and both
	// are off by default. That one expires a member's *key*, after which their
	// assistant refuses every message until somebody unlocks it at the machine. This
	// one costs the thread of a conversation, once, with a line saying so.
	ResetEvery Duration `yaml:"reset_every"`
}

// SessionConfig configures key lifetime.
type SessionConfig struct {
	// IdleTimeout is how long an unlocked key stays in memory without use. Zero —
	// the default — means it stays until the process stops or the member locks it.
	// Setting it is choosing the trade knowingly: the key leaves memory sooner, and
	// the member's assistant stops answering until somebody unlocks it at the machine.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// PassphraseEnv names the variable holding the node passphrase: simple mode's one
	// secret, which wraps every member's session key. Optional — the passphrase may
	// still arrive as a systemd credential, as KENWARD_PASSPHRASE, or typed at a
	// terminal, and all three still work — but naming it here is what makes its
	// absence a validation error with the variable named, the way
	// members[].passphrase_env already is in isolated mode. Ignored in isolated mode,
	// where each member's key is wrapped under their own.
	PassphraseEnv string `yaml:"passphrase_env"`
	// PassphraseFile names a file holding it — the better form in a container, where
	// an environment variable is readable by every process and is inherited by the
	// `lore` subprocess. One source only: stating this and PassphraseEnv is an error.
	PassphraseFile string `yaml:"passphrase_file"`
}

// CaptureConfig governs how memory writes reach the member.
type CaptureConfig struct {
	// MaxProposalsPerTurn caps capture proposals per turn. The default of one exists
	// because an assistant that interrupts repeatedly trains members to ignore it.
	MaxProposalsPerTurn int `yaml:"max_proposals_per_turn"`
	// PrivateWrites says what a proposal for a member's own private memory does:
	// "save" writes it and shows the member what was written, with an undo button;
	// "ask" puts it as a question first and writes nothing until they tap. Empty
	// means "save".
	//
	// It is the only part of this that a household configures. There is no setting
	// for the household's shared memory, which is always asked about first —
	// publishing to everyone is the one act here that cannot be taken back — and
	// none for the announcement, because a write nobody is told about would make the
	// product's central claim false.
	PrivateWrites PrivateWrites `yaml:"private_writes"`
}

// PrivateWrites is capture.private_writes.
type PrivateWrites string

const (
	// PrivateWriteSave writes the entry and announces it, with an undo button.
	PrivateWriteSave PrivateWrites = "save"
	// PrivateWriteAsk asks first and writes nothing until the member taps.
	PrivateWriteAsk PrivateWrites = "ask"
)

// RemindersConfig bounds the one thing kenward does without being asked.
//
// Every other message this node sends answers one a member sent. A reminder does not,
// and that makes these three numbers the household's control over a capability it
// cannot otherwise refuse: a household that finds the assistant chatty mutes it, and a
// muted assistant is a dead one.
type RemindersConfig struct {
	// Timezone is the IANA name of the household's clock — "Europe/Madrid". Members
	// state reminders in wall-clock time and mean their own clock, not the node's.
	// Empty means the machine's local zone, which is right for the ordinary case of a
	// node sitting in the house it serves.
	Timezone string `yaml:"timezone"`
	// MaxPerDay caps how many unprompted messages one conversation may receive in a
	// day, counted in the household's own timezone.
	//
	// Zero means unset and takes DefaultRemindersMaxPerDay. A negative number turns
	// proactive messages off entirely: reminders can still be set and are still
	// listed, they are simply never delivered. That is deliberately expressible,
	// because a household that wants the assistant silent should be able to say so
	// without every member having to cancel their own reminders.
	MaxPerDay int `yaml:"max_per_day"`
	// CatchUpWindow is how late a *repeating* occurrence may be and still be
	// delivered after the node has been off. It does not bound a one-off, which is a
	// promise to a person and is delivered however late — see internal/remind.
	CatchUpWindow Duration `yaml:"catch_up_window"`
	// MaxStored caps how many reminders one conversation may hold at once. It is what
	// stops a model that has learned it can call the tool from filling a member's
	// list with them.
	MaxStored int `yaml:"max_stored"`
}

// Location resolves the household's timezone, defaulting to the machine's own.
//
// An unloadable name is an error rather than a silent fall back to UTC: a household
// that wrote "Europe/Madrid" and got reminders an hour or two out would have no way to
// tell that from a bug, and validation reports it before anything runs.
func (r RemindersConfig) Location() (*time.Location, error) {
	if r.Timezone == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		return nil, err
	}
	return loc, nil
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
		joined.MissingSecrets = append(joined.MissingSecrets, ve.MissingSecrets...)
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
		// A rejected key that is a real key in another block is told which one.
		// See yamlhint.go: the strict decoder names a Go type, and nobody has one
		// of those open in their editor.
		return nil, fmt.Errorf("config: parsing yaml: %w", hintMisplacedFields(err))
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
	// session.idle_timeout is deliberately not defaulted. Zero is the default and zero
	// is also what "idle_timeout: 0s" means, so there is nothing to rewrite: normalising
	// an unset value to a duration here is what kept a 30-minute expiry on the run path
	// after the session package had turned it off.
	//
	// history.reset_every is not defaulted either, for exactly the same reason: zero
	// is off, off is the default, and "reset_every: 0s" says so on purpose.
	if c.Capture.MaxProposalsPerTurn == 0 {
		c.Capture.MaxProposalsPerTurn = DefaultMaxProposalsPerTurn
	}
	if c.Capture.PrivateWrites == "" {
		c.Capture.PrivateWrites = DefaultPrivateWrites
	}
	// memory.announce_reads is deliberately not defaulted here, for the same reason
	// session.idle_timeout is not: its default is true and its zero value is false,
	// so the absence has to survive as an absence. MemoryConfig.AnnouncesReads reads
	// it; rewriting nil to a pointer-to-true here would only move the branch.
	if c.Household.Agents == "" {
		c.Household.Agents = DefaultAgents
	}
	// household.persona and members[].persona are deliberately not defaulted. Every
	// field's empty value is a meaning — English, the flat register, no character,
	// the name kenward — and writing those meanings in as strings would put "English"
	// in the prompt of a household that never chose it, and freeze the flat register
	// as text rather than as the absence of an instruction.
	if c.Update.Channel == "" {
		c.Update.Channel = DefaultUpdateChannel
	}
	if c.Update.CheckInterval == 0 {
		c.Update.CheckInterval = Duration(DefaultCheckInterval)
	}
	// reminders.timezone is deliberately not defaulted to a name: empty means the
	// machine's own zone, and writing one in here would freeze a household's clock to
	// whatever the node believed on the day it first started.
	if c.Reminders.MaxPerDay == 0 {
		c.Reminders.MaxPerDay = DefaultRemindersMaxPerDay
	}
	if c.Reminders.CatchUpWindow == 0 {
		c.Reminders.CatchUpWindow = Duration(DefaultRemindersCatchUp)
	}
	if c.Reminders.MaxStored == 0 {
		c.Reminders.MaxStored = DefaultRemindersMaxStored
	}
	for i := range c.Endpoints {
		if c.Endpoints[i].Timeout == 0 {
			c.Endpoints[i].Timeout = Duration(DefaultEndpointTimeout)
		}
		if c.Endpoints[i].ContextWindow == 0 {
			c.Endpoints[i].ContextWindow = DefaultContextWindow
		}
		if c.Endpoints[i].MaxCompletionTokens == 0 {
			c.Endpoints[i].MaxCompletionTokens = DefaultMaxCompletionTokens
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
