package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ValidationError reports every problem found in one configuration, together.
//
// Validation deliberately does not stop at the first fault. A household's
// configuration is edited rarely and by hand; surfacing one problem per restart turns a
// five-minute edit into a half-hour guessing game.
type ValidationError struct {
	// Problems are human-readable, in file order, each naming the field it is about.
	Problems []string
	// MissingEnv lists the environment variables that were referenced but not set (or
	// set to an empty value). It is separate from Problems so `kenward doctor` can
	// print an actionable export list; the same faults also appear in Problems.
	MissingEnv []string
	// MissingSecrets names the secrets no source supplied, by configuration path —
	// "telegram.bot_token", "members[2].bot_token". A secret may now arrive as a file
	// or a systemd credential as well as a variable, so an export list no longer
	// describes the whole set; this does. The same faults also appear in Problems.
	MissingSecrets []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	if len(e.Problems) == 1 {
		b.WriteString("config: 1 problem:")
	} else {
		fmt.Fprintf(&b, "config: %d problems:", len(e.Problems))
	}
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// problems accumulates validation faults in the order they are found.
type problems struct {
	list           []string
	missingEnv     []string
	missingSecrets []string
}

func (p *problems) addf(format string, args ...any) {
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

// UnitScope names the one unit a process was asked to be.
//
// Isolated mode runs one pod per unit, and each pod holds only that unit's bot token —
// that is D-007, and it is the whole point of the mode. Every pod is nevertheless given
// the household's whole configuration, because a pod has to know the members it may not
// serve in order to refuse them. Unscoped, validation therefore demanded every token
// named in the file from every pod, and the only way to satisfy that is to put every
// member's token in every member's container: exactly the failure the mode exists to
// prevent.
//
// The zero value is the whole household — a simple-mode node, or the isolated host
// supervisor that starts the pods — so a household-wide process validates exactly as it
// always did.
//
// It scopes secret *resolution* and nothing else. Every structural rule in this file is
// a property of the file rather than of the pod reading it, and stays household-wide: a
// pod handed a configuration with two members on one private space, a tier no endpoint
// serves, or two members on one bot token must refuse it, whichever unit the pod is.
type UnitScope struct {
	// Member is the id of the member this pod serves. Empty means it serves none.
	Member string
	// Group marks the household group's pod.
	Group bool
	// NoSecrets asks for the file to be checked and none of its secrets resolved.
	//
	// It is for the commands that read no secret at all. `kenward invite` mints a
	// digest and `kenward revoke` clears a binding; neither opens a bot, unwraps a key
	// or calls a provider, and demanding the household's tokens of them made both
	// unrunnable in isolated mode — where, by design, no container holds a sibling's
	// secrets, so there was nowhere left to run a documented operator command.
	//
	// It drops resolution and nothing else. Every structural rule still applies,
	// including the one rule about secrets that is a property of the file rather than
	// of the environment: naming both a _file and an _env source for one secret is
	// still reported, because it is wrong whoever reads it. `run` and `doctor` never
	// set it — they are about to serve, and a node that starts half-configured is the
	// failure the resolution exists to prevent.
	NoSecrets bool
}

// ServesGroup reports whether this scope covers the household group's conversation, and
// so the household bot it runs on. A member's pod never speaks on that bot; the group's
// pod and a household-wide process both do.
func (u UnitScope) ServesGroup() bool { return strings.TrimSpace(u.Member) == "" }

// Serves reports whether this scope covers the member with this id — their bot, their
// key, their private space.
func (u UnitScope) Serves(memberID string) bool {
	if u.Group {
		return false
	}
	member := strings.TrimSpace(u.Member)
	return member == "" || member == strings.TrimSpace(memberID)
}

// EndpointsForUnit lists the endpoints this unit's conversations can actually route to:
// the ones tagged with a tier in its own chain.
//
// A pod is given only the provider keys its chain can reach, and deliberately so — a key
// present in an environment is a key that can be used whatever the routing policy
// intended, so david's local-only pod holds none. deploy/compose.isolated.yml says
// exactly that, in those words. Demanding every endpoint's key of every pod would have
// made that file unstartable for a second reason after the bot tokens, so the same
// scoping applies here.
//
// The zero scope returns every endpoint, unchanged: a household-wide process may route
// any conversation, and an endpoint no chain names is a configuration fault worth
// surfacing rather than quietly skipping.
func (c *Config) EndpointsForUnit(scope UnitScope) []EndpointConfig {
	chain, scoped := c.unitTiers(scope)
	if !scoped {
		return c.Endpoints
	}
	var out []EndpointConfig
	for _, e := range c.Endpoints {
		if chainReaches(chain, e.Tags) {
			out = append(out, e)
		}
	}
	return out
}

// chainReaches reports whether a tier chain names any of an endpoint's tags. It is the
// same question the router asks at routing time, asked here about credentials.
func chainReaches(chain, tags []string) bool {
	for _, t := range chain {
		for _, tag := range tags {
			if strings.TrimSpace(t) == strings.TrimSpace(tag) {
				return true
			}
		}
	}
	return false
}

// unitTiers returns the tier chain this scope's conversations use. scoped is false for a
// household-wide process, which uses every chain in the file and so reaches everything.
//
// A member the file does not name yields an empty chain rather than every chain: that
// pod reaches nothing, which is the safe reading, and validateScope refuses it anyway.
func (c *Config) unitTiers(scope UnitScope) (chain []string, scoped bool) {
	if scope.Group {
		return c.Household.Tiers, true
	}
	if member := strings.TrimSpace(scope.Member); member != "" {
		for _, m := range c.Members {
			if strings.TrimSpace(m.ID) == member {
				return m.Tiers, true
			}
		}
		return nil, true
	}
	return nil, false
}

// Validate checks the configuration against the rules in the implementation contract
// and against the environment it will run in.
//
// lookupEnv may be nil, in which case the real process environment is used. Secrets are
// resolved against that environment, the real filesystem and whatever credentials
// directory it names; ValidateWithSecrets judges against injected ones.
func (c *Config) Validate(lookupEnv LookupEnvFunc) error {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return c.ValidateWithSecrets(NewSecrets(SecretOptions{LookupEnv: lookupEnv}))
}

// ValidateWithSecrets is Validate with the secret sources injected, for tests and for
// `doctor` judging a configuration against an environment other than its own. It judges
// the whole household; ValidateForUnit judges one pod.
//
// s may be nil, in which case the process environment and the real filesystem are used.
func (c *Config) ValidateWithSecrets(s *Secrets) error {
	return c.ValidateForUnit(s, UnitScope{})
}

// ValidateForUnit is ValidateWithSecrets for a process that runs one unit: the secrets
// it demands are the ones that unit reads, and nothing else changes. See UnitScope.
func (c *Config) ValidateForUnit(s *Secrets, scope UnitScope) error {
	s = s.orDefault()
	p := &problems{}

	c.validateMode(p)
	tags := c.validateEndpoints(p)
	c.validateHousehold(p, tags)
	c.validateMembers(p, tags)
	c.validateMemory(p)
	c.validateLimits(p)
	c.validateUpdate(p)
	c.validateReminders(p)
	c.validateDashboard(p)
	c.validateScope(p, scope)
	c.validateSecrets(p, s, scope)

	if len(p.list) == 0 {
		return nil
	}
	return &ValidationError{Problems: p.list, MissingEnv: p.missingEnv, MissingSecrets: p.missingSecrets}
}

// validateScope checks the unit this process was asked to be against the household it
// was handed.
//
// Without it a pod told to serve a member the file does not name would validate clean —
// scoped secret resolution finds no such member, so it demands no token at all — and
// then refuse to start. That is a green health check on a pod that cannot work, which is
// worse than the fault it hides.
//
// A selector in simple mode is not checked here. There is nothing to select in simple
// mode, and the refusal for that belongs where the selector was given, with the reason
// it is wrong there; it is not a fault in the file.
func (c *Config) validateScope(p *problems, scope UnitScope) {
	member := strings.TrimSpace(scope.Member)
	if member == "" || c.Mode != ModeIsolated {
		return
	}
	for _, m := range c.Members {
		if strings.TrimSpace(m.ID) == member {
			return
		}
	}
	p.addf("members: this process was asked to serve member %q, and this configuration names no member with that id", member)
}

func (c *Config) validateMode(p *problems) {
	switch c.Mode {
	case ModeSimple, ModeIsolated:
	case "":
		p.addf("mode: required; set it to %q or %q", ModeSimple, ModeIsolated)
	default:
		p.addf("mode: %q is not a mode; use %q or %q", c.Mode, ModeSimple, ModeIsolated)
	}
}

// validateEndpoints checks each endpoint and returns the set of tier tags that at least
// one endpoint answers for. Tier chains are validated against that set: a chain naming
// a tier no machine serves is a chain that can only ever refuse.
func (c *Config) validateEndpoints(p *problems) map[string]bool {
	tags := make(map[string]bool)
	seen := make(map[string]int)

	for i, e := range c.Endpoints {
		where := fmt.Sprintf("endpoints[%d]", i)

		switch {
		case strings.TrimSpace(e.Name) == "":
			p.addf("%s.name: required; endpoints are named so refusals and cooldowns can name them", where)
		default:
			if first, dup := seen[e.Name]; dup {
				p.addf("%s.name: duplicate endpoint name %q, already used by endpoints[%d]", where, e.Name, first)
			} else {
				seen[e.Name] = i
			}
		}

		if strings.TrimSpace(e.Model) == "" {
			p.addf("%s.model: required", where)
		}

		switch {
		case strings.TrimSpace(e.BaseURL) == "":
			p.addf("%s.base_url: required", where)
		default:
			if err := validateBaseURL(e.BaseURL); err != nil {
				p.addf("%s.base_url: %v", where, err)
			}
		}

		validateEndpointBudget(p, where, e)

		for _, t := range e.Tags {
			if strings.TrimSpace(t) == "" {
				p.addf("%s.tags: contains an empty tier name", where)
				continue
			}
			tags[t] = true
		}
	}
	return tags
}

// validateEndpointBudget checks one endpoint's context window against its completion
// cap.
//
// The completion is reserved out of the window, so a cap that meets or exceeds it
// leaves nothing for the prompt — the assistant refuses to construct a unit on those
// numbers, and it refuses at startup with no endpoint named, because by then the two
// figures are a single derived pair. Caught here it is a line in a file with a name
// beside it. Both fields are defaulted before validation runs, so an endpoint that
// states only a small window is checked against the default cap, which is exactly the
// pairing it will run with.
func validateEndpointBudget(p *problems, where string, e EndpointConfig) {
	name := strings.TrimSpace(e.Name)
	if name == "" {
		name = "this endpoint"
	} else {
		name = fmt.Sprintf("%q", name)
	}
	if e.ContextWindow < 0 {
		p.addf("%s.context_window: %d is negative; %s has no window of that size", where, e.ContextWindow, name)
	}
	if e.MaxCompletionTokens < 0 {
		p.addf("%s.max_completion_tokens: %d is negative; %s cannot be asked for fewer than no tokens", where, e.MaxCompletionTokens, name)
	}
	if e.ContextWindow > 0 && e.MaxCompletionTokens > 0 && e.MaxCompletionTokens >= e.ContextWindow {
		p.addf("%s.max_completion_tokens: %d must be smaller than %s's context_window (%d); the completion is\n"+
			"reserved out of the window, so a cap this large leaves no room for the prompt at all",
			where, e.MaxCompletionTokens, name, e.ContextWindow)
	}
}

// validateBaseURL requires an absolute http or https URL. A relative URL, or one with a
// scheme kenward cannot speak, would fail only at the moment a member was waiting for
// an answer.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %v", raw, err)
	}
	if !u.IsAbs() {
		return fmt.Errorf("%q is not absolute; it needs an http:// or https:// scheme", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not supported; use http or https", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}

func (c *Config) validateHousehold(p *problems, tags map[string]bool) {
	if strings.TrimSpace(c.Household.SharedSpace) == "" {
		p.addf("household.shared_space: required; the group conversation has nowhere to read or write without it")
	}
	c.validateTiers(p, "household.tiers", c.Household.Tiers, tags)
}

// validateTiers checks that a tier chain is stated and that every tier in it is served
// by at least one endpoint.
//
// An omitted chain is an error rather than a default. Defaulting it to the household's
// chain would widen a member's privacy policy without anyone saying so, which is the
// worst shape of bug this product can have; leaving it empty would be a configuration
// that parses, starts, and then refuses every turn for a reason nobody can see. Saying
// it out loud is also what makes the guarantee checkable: "this member only ever
// reaches local machines" is a claim you can read off the file.
func (c *Config) validateTiers(p *problems, where string, chain []string, tags map[string]bool) {
	if len(chain) == 0 {
		p.addf("%s: required; a tier chain must be stated explicitly, because it is the privacy policy for these conversations and an unstated one cannot be checked", where)
		return
	}
	for _, t := range chain {
		if strings.TrimSpace(t) == "" {
			p.addf("%s: contains an empty tier name", where)
			continue
		}
		if !tags[t] {
			p.addf("%s: tier %q is not a tag on any endpoint", where, t)
		}
	}
}

// householdOwner is the owner index used for the household's own bot token in the token
// maps below: not a member, so not a members[N] index.
const householdOwner = -1

// sanitizeMemberID mirrors podName in internal/supervisor, which collapses everything
// outside [A-Za-z0-9_-] to a hyphen when it builds a pod name from a member id. Two ids
// that differ only in characters it collapses become one pod name, and a member served
// by another member's pod holds that member's lore volume and bot token. The supervisor
// refuses the collision as well, but a configuration that cannot be run is worth
// refusing at the file rather than at the third pod.
func sanitizeMemberID(id string) string {
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func (c *Config) validateMembers(p *problems, tags map[string]bool) {
	ids := make(map[string]int)
	sanitized := make(map[string]string)
	spaces := make(map[string]int)
	telegrams := make(map[int64]int)
	tokens := make(map[string]int)
	tokenFiles := make(map[string]int)
	passphrases := make(map[string]int)
	passphraseFiles := make(map[string]int)

	// The household's own bot token joins the member-vs-member check rather than being
	// checked only against itself. In isolated mode it is the group pod's token, so a
	// member naming the same source puts the group pod and that member's pod on one
	// bot — the isolation loss the mode exists to prevent, and one that member-vs-member
	// uniqueness alone never sees. It is seeded whether or not household.group_chat_id
	// is set: a group added later must not turn a file that validated into one that
	// leaks.
	if c.Mode == ModeIsolated {
		if env := strings.TrimSpace(c.Telegram.BotTokenEnv); env != "" {
			tokens[env] = householdOwner
		}
		if file := strings.TrimSpace(c.Telegram.BotTokenFile); file != "" {
			tokenFiles[file] = householdOwner
		}
	}

	for i, m := range c.Members {
		where := fmt.Sprintf("members[%d]", i)

		// Every uniqueness map below is keyed on the trimmed value, because the
		// emptiness checks trim too: keying on the raw one would let " david" and
		// "david" both through a check whose whole purpose is that they cannot.
		id := strings.TrimSpace(m.ID)
		switch {
		case id == "":
			p.addf("%s.id: required", where)
		default:
			if first, dup := ids[id]; dup {
				p.addf("%s.id: duplicate member id %q, already used by members[%d]", where, m.ID, first)
			} else {
				ids[id] = i
			}
			if key := sanitizeMemberID(id); sanitized[key] != "" && sanitized[key] != id {
				p.addf("%s.id: %q and %q are different ids but the same pod name %q, so one member would be served by the other's pod; change one of them", where, id, sanitized[key], key)
			} else {
				sanitized[key] = id
			}
		}

		space := strings.TrimSpace(m.PrivateSpace)
		shared := strings.TrimSpace(c.Household.SharedSpace)
		switch {
		case space == "":
			p.addf("%s.private_space: required", where)
		default:
			if first, dup := spaces[space]; dup {
				p.addf("%s.private_space: %q is already members[%d]'s private space; two members sharing a private space is not a private space", where, m.PrivateSpace, first)
			} else {
				spaces[space] = i
			}
			if shared != "" && space == shared {
				p.addf("%s.private_space: %q is also household.shared_space; a private space that is the shared space publishes everything the member says", where, m.PrivateSpace)
			}
		}

		// Zero means "not yet claimed" and many members may sit there at once, so
		// only non-zero ids are checked for collisions.
		if m.TelegramID != 0 {
			if first, dup := telegrams[m.TelegramID]; dup {
				p.addf("%s.telegram_id: %d is already members[%d]'s telegram id; one Telegram account cannot be two members", where, m.TelegramID, first)
			} else {
				telegrams[m.TelegramID] = i
			}
			// scope.Resolve matches the group on chat id before it looks at the
			// sender, so a group_chat_id that is also a member's telegram id would
			// route that member's direct messages into the group scope. Resolve is
			// fail-closed about it and answers neither, which is the safe reading and
			// also a member who is silently served nothing.
			if c.Household.GroupChatID != 0 && m.TelegramID == c.Household.GroupChatID {
				p.addf("%s.telegram_id: %d is also household.group_chat_id; a member's own chat cannot also be the household group, and their direct messages would resolve to neither", where, m.TelegramID)
			}
		}

		c.validateTiers(p, where+".tiers", m.Tiers, tags)

		// Whether each member has a token at all is settled in validateSecrets,
		// which knows about files and systemd credentials as well as variables.
		// What is settled here is that no two pods share one: two pods on one bot
		// is not isolation, whichever form the token arrives in and whether the
		// other pod is a member's or the household group's.
		if c.Mode == ModeIsolated {
			c.checkTokenSource(p, where, "bot_token_env", "variable", strings.TrimSpace(m.BotTokenEnv), tokens, i)
			c.checkTokenSource(p, where, "bot_token_file", "file", strings.TrimSpace(m.BotTokenFile), tokenFiles, i)
			// And the same rule for the passphrase, which is the other half of what
			// makes a pod isolated. Two members on one passphrase is one wrapping
			// secret for both keys — simple mode's custody model wearing isolated
			// mode's name — and nothing downstream would ever notice: both pods
			// unwrap, both serve, and the property the mode was chosen for is gone.
			c.checkPassphraseSource(p, where, "passphrase_env", "variable", strings.TrimSpace(m.PassphraseEnv), passphrases, i)
			c.checkPassphraseSource(p, where, "passphrase_file", "file", strings.TrimSpace(m.PassphraseFile), passphraseFiles, i)
		}
	}
}

// checkTokenSource records one member's token source and reports it if something else
// already claimed it. seen maps the source to its first claimant: a members[] index, or
// household for telegram.bot_token.
func (c *Config) checkTokenSource(p *problems, where, field, kind, value string, seen map[string]int, i int) {
	if value == "" {
		return
	}
	first, dup := seen[value]
	switch {
	case !dup:
		seen[value] = i
	case first == householdOwner:
		p.addf("%s.%s: %s is also telegram.%s, the household bot's; in isolated mode the group conversation runs on that bot, so this member's pod and the group's would be one bot and one message stream", where, field, value, field)
	default:
		p.addf("%s.%s: %s is already members[%d]'s token %s; sharing a bot defeats isolated mode", where, field, value, first, kind)
	}
}

// checkPassphraseSource is checkTokenSource for the passphrase that wraps a member's
// key. It is a second function rather than a second call because the household seeds
// nothing here — there is no household passphrase in the file — and because the loss
// it reports is a different one, worth saying in its own words.
func (c *Config) checkPassphraseSource(p *problems, where, field, kind, value string, seen map[string]int, i int) {
	if value == "" {
		return
	}
	if first, dup := seen[value]; dup {
		p.addf("%s.%s: %s is already members[%d]'s passphrase %s; one passphrase across two members wraps both their keys, which is simple mode's custody with isolated mode's name on it", where, field, value, first, kind)
		return
	}
	seen[value] = i
}

// validateMemory checks the lore command is something that could be executed.
//
// After ApplyDefaults an omitted command has already become DefaultLoreCommand, so the
// empty case here is not reachable from a YAML file — it catches configurations built in
// Go that never went through ApplyDefaults, which the setup wizard and other packages
// do. That is worth a check rather than a comment: the failure it replaces is a spawn of
// nothing at startup, reported far from the file that caused it.
//
// What it deliberately does not do is look for the program on PATH or stat it. Whether
// lore is installed is a property of the machine, not of the configuration, and a
// validation that fails on one host and passes on another would make `doctor` useless
// for checking a file before shipping it. That check belongs to `doctor`, on the machine
// that will run it.
func (c *Config) validateMemory(p *problems) {
	if len(c.Memory.LoreCommand) == 0 {
		p.addf("memory.lore_command: required; it is the argv kenward runs to start lore's MCP server, and without it there is no memory to read or write")
		return
	}
	if strings.TrimSpace(c.Memory.LoreCommand[0]) == "" {
		p.addf("memory.lore_command: the first element is the program to run and it is empty; write it as %s", yamlFlowSeq(DefaultLoreCommand()))
	}
}

// yamlFlowSeq renders a string slice as the YAML an operator should type. Go's own %v
// prints [lore mcp], which is not a flow sequence: copying it into kenward.yaml turns a
// message meant to unstick someone into a second error.
func yamlFlowSeq(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func (c *Config) validateLimits(p *problems) {
	if c.Memory.SearchLimit < 0 {
		p.addf("memory.search_limit: %d is negative", c.Memory.SearchLimit)
	}
	if c.Capture.MaxProposalsPerTurn < 0 {
		p.addf("capture.max_proposals_per_turn: %d is negative", c.Capture.MaxProposalsPerTurn)
	}
	switch c.Capture.PrivateWrites {
	case "", PrivateWriteSave, PrivateWriteAsk:
	default:
		// Named rather than ignored. An unrecognised value here would silently fall
		// back to writing without asking, which is the more surprising of the two
		// behaviours to get by accident and the one a household typing this field at
		// all is most likely trying to turn off.
		p.addf("capture.private_writes: %q is not a policy; use %q or %q",
			c.Capture.PrivateWrites, PrivateWriteSave, PrivateWriteAsk)
	}
	if c.Session.IdleTimeout < 0 {
		p.addf("session.idle_timeout: %s is negative", c.Session.IdleTimeout)
	}
}

func (c *Config) validateUpdate(p *problems) {
	switch c.Update.Channel {
	case UpdateStable, UpdateEdge, UpdateOff:
	default:
		p.addf("update.channel: %q is not a channel; use %q, %q or %q",
			c.Update.Channel, UpdateStable, UpdateEdge, UpdateOff)
	}
	if c.Update.CheckInterval < 0 {
		p.addf("update.check_interval: %s is negative", c.Update.CheckInterval)
	}
}

func (c *Config) validateReminders(p *problems) {
	if _, err := c.Reminders.Location(); err != nil {
		// Not defaulted to UTC: a household whose reminders arrived an hour out
		// would have no way to tell that from a bug, so the name has to be right
		// before anything runs.
		p.addf("reminders.timezone: %q is not a timezone this node can load (%v); use an IANA name such as \"Europe/Madrid\", or leave it empty for the machine's own clock",
			c.Reminders.Timezone, err)
	}
	if c.Reminders.CatchUpWindow < 0 {
		p.addf("reminders.catch_up_window: %s is negative", c.Reminders.CatchUpWindow)
	}
	if c.Reminders.MaxStored < 0 {
		p.addf("reminders.max_stored: %d is negative; use 0 for the default", c.Reminders.MaxStored)
	}
	// reminders.max_per_day is deliberately unchecked below zero: a negative number
	// is how a household says no proactive message is ever to be delivered.
}

// secretRef is one secret this configuration depends on, together with the reason it
// must exist. An empty reason means the secret is optional — an endpoint on the
// household's own network needs no key, and demanding one would be demanding a secret
// nothing reads.
type secretRef struct {
	SecretRef
	required string
}

// secretRefs lists the secrets this configuration actually depends on, in file order and
// without repeating a variable or a path. Only the secrets the selected mode uses are
// listed: a leftover per-member token in a simple-mode file is inert.
//
// Every reference is included even when the file states no source for it, because the
// systemd credential is the source in that case and the only way to find out is to look.
//
// scope narrows the bot tokens to the ones this process speaks on — see UnitScope. The
// three cases are the three kinds of process there are:
//
//   - the household node (the zero scope): every member's token, plus the household's.
//   - the household group's pod: the household's token and no member's. It serves the
//     shared space over the household bot and never opens a private conversation.
//   - a member's pod: that member's token and nothing else, not even the household's.
//     The group conversation is another pod's work.
//
// Endpoint API keys are scoped too, by the unit's tier chain — see EndpointsForUnit. A
// key that is named and unset is a hard fault rather than an absent optional secret, so
// leaving these unscoped would refuse a local-only member's pod for not holding the
// provider key its own configuration forbids it to use.
func (c *Config) secretRefs(scope UnitScope) []secretRef {
	if scope.NoSecrets {
		return nil
	}
	var refs []secretRef
	seenEnv := make(map[string]bool)
	seenFile := make(map[string]bool)
	add := func(ref SecretRef, required string) {
		// A variable or a file named twice is one thing to *read* and one thing to
		// say about reading it, so the second reference is dropped here. That is
		// only safe because whether naming it twice is wrong at all is answered
		// elsewhere and in full: validateMembers now checks every member's token
		// against every other member's and against the household's, so no sharing
		// reaches this dedup unreported. Two endpoints on one API key variable is
		// the case that remains legitimate, and it is one check.
		if (ref.Env != "" && seenEnv[ref.Env]) || (ref.File != "" && seenFile[ref.File]) {
			return
		}
		if ref.Env != "" {
			seenEnv[ref.Env] = true
		}
		if ref.File != "" {
			seenFile[ref.File] = true
		}
		refs = append(refs, secretRef{SecretRef: ref, required: required})
	}

	if c.Mode == ModeSimple {
		add(c.BotTokenRef(), "required in simple mode; the household shares one bot")
		// Optional, and deliberately: the node passphrase has three sources this file
		// cannot see — a systemd credential, KENWARD_PASSPHRASE, and a person typing at
		// a terminal — and every one of them is a legitimate way to run simple mode. So
		// a file that names no source is not a fault. A file that *names* one is held to
		// the same standard as every other secret: an unset variable or an unreadable
		// path is reported here, by name, at load, which is what a member's
		// passphrase_env already got and this one did not. See SessionPassphraseRef.
		add(c.SessionPassphraseRef(), "")
	}
	if c.Mode == ModeIsolated && c.Household.GroupChatID != 0 && scope.ServesGroup() {
		// Per-member bots cover the members; the household group conversation still
		// runs on the household bot. Only when there is a group to serve — isolated
		// mode without one is a legitimate configuration and must not be made to
		// invent a token it will never use — and not in a member's pod, which never
		// speaks on that bot and must never be handed it.
		add(c.BotTokenRef(), "required in isolated mode when household.group_chat_id is set; the group pod runs on the household bot")
	}
	if c.Mode == ModeIsolated {
		for i, m := range c.Members {
			if !scope.Serves(m.ID) {
				continue
			}
			ref := m.BotTokenRef()
			// Indexed rather than named: a member with no id at all still has a
			// row in the file, and the row is what the operator has to edit.
			ref.Where = fmt.Sprintf("members[%d].bot_token", i)
			add(ref, "required in isolated mode; each member's pod holds its own bot token")

			pass := m.PassphraseRef()
			pass.Where = fmt.Sprintf("members[%d].passphrase", i)
			// Required for a member who has not claimed yet as much as for one who
			// has: D-023 puts their bot up before the claim, and the claim is what
			// provisions their key — under this passphrase, in their own pod, with
			// nobody standing at a terminal to be asked for one.
			add(pass, "required in isolated mode; each member's key is wrapped under their own passphrase, and a pod given none can unwrap nothing")
		}
	}
	// Walked over the whole list, and skipped rather than filtered, so that the index
	// in a message is the row in the file whoever is reading it.
	chain, scoped := c.unitTiers(scope)
	for i, e := range c.Endpoints {
		if scoped && !chainReaches(chain, e.Tags) {
			continue
		}
		ref := e.APIKeyRef()
		ref.Where = fmt.Sprintf("endpoints[%d].api_key", i)
		add(ref, "")
	}
	return refs
}

// validateSecrets requires every secret this configuration depends on to be readable at
// load time, from whichever of its three sources supplies it.
//
// kenward must never start half-configured. A missing provider key discovered mid-turn
// is a refusal a member cannot act on; a missing key discovered at startup is a line in
// the operator's terminal. Values read here are proved and dropped: nothing is kept, and
// no fault message quotes one.
//
// scope decides which secrets that is; validateSecretSources, below, is unscoped,
// because naming two sources for one secret is wrong in the file whoever reads it.
func (c *Config) validateSecrets(p *problems, s *Secrets, scope UnitScope) {
	s = s.orDefault()
	c.validateSecretSources(p)
	for _, ref := range c.secretRefs(scope) {
		if ref.File != "" && ref.Env != "" {
			// Already reported by validateSecretSources, which sees the secrets
			// this mode does not use as well. Resolving would say it twice.
			continue
		}
		_, err := s.Resolve(ref.SecretRef)
		if err == nil {
			continue
		}

		var se *SecretError
		if !errors.As(err, &se) {
			p.addf("%s: %v", ref.Where, err)
			p.missingSecrets = append(p.missingSecrets, ref.Where)
			continue
		}
		if se.NotFound {
			if ref.required == "" {
				continue
			}
			fileField, envField := ref.fields()
			msg := fmt.Sprintf("%s_env: %s. Supply it as %s, as %s", ref.Where, ref.required, envField, fileField)
			if ref.Credential != "" {
				msg += fmt.Sprintf(", or as the systemd credential %q", ref.Credential)
			}
			p.list = append(p.list, msg)
			p.missingSecrets = append(p.missingSecrets, ref.Where)
			continue
		}
		p.addf("%s: %s", se.Where, se.Detail)
		p.missingSecrets = append(p.missingSecrets, ref.Where)
		if se.MissingEnv != "" {
			p.missingEnv = append(p.missingEnv, se.MissingEnv)
		}
	}
}

// validateSecretSources enforces the one rule that is a property of the file rather than
// of the mode: a secret names exactly one source.
//
// It walks every secret in the document, including the ones the selected mode never
// reads — a per-member token in a simple-mode file, the household token in an isolated
// file with no group. Those are inert today, which is exactly why the contradiction has
// to be reported now: left to Resolve, which validation calls only for the secrets in
// use, the file validates clean and fails the day somebody changes one line at the top
// of it.
func (c *Config) validateSecretSources(p *problems) {
	refs := []SecretRef{c.BotTokenRef(), c.SessionPassphraseRef()}
	for i, m := range c.Members {
		ref := m.BotTokenRef()
		ref.Where = fmt.Sprintf("members[%d].bot_token", i)
		refs = append(refs, ref)

		pass := m.PassphraseRef()
		pass.Where = fmt.Sprintf("members[%d].passphrase", i)
		refs = append(refs, pass)
	}
	for i, e := range c.Endpoints {
		ref := e.APIKeyRef()
		ref.Where = fmt.Sprintf("endpoints[%d].api_key", i)
		refs = append(refs, ref)
	}
	for _, ref := range refs {
		if ref.File == "" || ref.Env == "" {
			continue
		}
		fileField, envField := ref.fields()
		p.addf("%s: %s", ref.Where, bothSourcesDetail(fileField, envField))
	}
}

// MissingEnvNames returns the referenced environment variables that are not set or are
// empty, sorted, without validating anything else. `kenward doctor` uses it to report
// on a running node's environment.
//
// It says nothing about secrets supplied as files or as systemd credentials, which have
// no variable to export; MissingSecretNames covers those.
func (c *Config) MissingEnvNames(lookupEnv LookupEnvFunc) []string {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	p := &problems{}
	c.validateSecrets(p, NewSecrets(SecretOptions{LookupEnv: lookupEnv}), UnitScope{})
	sort.Strings(p.missingEnv)
	return p.missingEnv
}

// MissingSecretNames returns the configuration paths of every secret the household needs
// and could not resolve, sorted — the whole set an operator has to fix, whatever form
// each one was meant to arrive in. MissingSecretNamesForUnit answers for one pod.
func (c *Config) MissingSecretNames(s *Secrets) []string {
	return c.MissingSecretNamesForUnit(s, UnitScope{})
}

// MissingSecretNamesForUnit is MissingSecretNames for a process that runs one unit. A
// pod's health check asks this one: a sibling's token being absent from this container
// is the mode working, not a fault.
func (c *Config) MissingSecretNamesForUnit(s *Secrets, scope UnitScope) []string {
	p := &problems{}
	c.validateSecrets(p, s.orDefault(), scope)
	sort.Strings(p.missingSecrets)
	return p.missingSecrets
}
