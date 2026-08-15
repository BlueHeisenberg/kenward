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
// `doctor` judging a configuration against an environment other than its own.
//
// s may be nil, in which case the process environment and the real filesystem are used.
func (c *Config) ValidateWithSecrets(s *Secrets) error {
	s = s.orDefault()
	p := &problems{}

	c.validateMode(p)
	tags := c.validateEndpoints(p)
	c.validateHousehold(p, tags)
	c.validateMembers(p, tags)
	c.validateMemory(p)
	c.validateLimits(p)
	c.validateUpdate(p)
	c.validateSecrets(p, s)

	if len(p.list) == 0 {
		return nil
	}
	return &ValidationError{Problems: p.list, MissingEnv: p.missingEnv, MissingSecrets: p.missingSecrets}
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

func (c *Config) validateMembers(p *problems, tags map[string]bool) {
	ids := make(map[string]int)
	spaces := make(map[string]int)
	telegrams := make(map[int64]int)
	tokens := make(map[string]int)
	tokenFiles := make(map[string]int)

	for i, m := range c.Members {
		where := fmt.Sprintf("members[%d]", i)

		switch {
		case strings.TrimSpace(m.ID) == "":
			p.addf("%s.id: required", where)
		default:
			if first, dup := ids[m.ID]; dup {
				p.addf("%s.id: duplicate member id %q, already used by members[%d]", where, m.ID, first)
			} else {
				ids[m.ID] = i
			}
		}

		switch {
		case strings.TrimSpace(m.PrivateSpace) == "":
			p.addf("%s.private_space: required", where)
		default:
			if first, dup := spaces[m.PrivateSpace]; dup {
				p.addf("%s.private_space: %q is already members[%d]'s private space; two members sharing a private space is not a private space", where, m.PrivateSpace, first)
			} else {
				spaces[m.PrivateSpace] = i
			}
			if c.Household.SharedSpace != "" && m.PrivateSpace == c.Household.SharedSpace {
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
		}

		c.validateTiers(p, where+".tiers", m.Tiers, tags)

		// Whether each member has a token at all is settled in validateSecrets,
		// which knows about files and systemd credentials as well as variables.
		// What is settled here is that no two members share one: two pods on one
		// bot is not isolation, whichever form the token arrives in.
		if c.Mode == ModeIsolated {
			if env := strings.TrimSpace(m.BotTokenEnv); env != "" {
				if first, dup := tokens[env]; dup {
					p.addf("%s.bot_token_env: %s is already members[%d]'s token variable; sharing a bot defeats isolated mode", where, env, first)
				} else {
					tokens[env] = i
				}
			}
			if file := strings.TrimSpace(m.BotTokenFile); file != "" {
				if first, dup := tokenFiles[file]; dup {
					p.addf("%s.bot_token_file: %s is already members[%d]'s token file; sharing a bot defeats isolated mode", where, file, first)
				} else {
					tokenFiles[file] = i
				}
			}
		}
	}
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
func (c *Config) secretRefs() []secretRef {
	var refs []secretRef
	seenEnv := make(map[string]bool)
	seenFile := make(map[string]bool)
	add := func(ref SecretRef, required string) {
		// A variable or a file named twice is one thing to check and one thing to
		// say. Whether naming it twice is itself wrong is isolated mode's question,
		// answered in validateMembers.
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
	}
	if c.Mode == ModeIsolated && c.Household.GroupChatID != 0 {
		// Per-member bots cover the members; the household group conversation still
		// runs on the household bot. Only when there is a group to serve — isolated
		// mode without one is a legitimate configuration and must not be made to
		// invent a token it will never use.
		add(c.BotTokenRef(), "required in isolated mode when household.group_chat_id is set; the group pod runs on the household bot")
	}
	if c.Mode == ModeIsolated {
		for i, m := range c.Members {
			ref := m.BotTokenRef()
			// Indexed rather than named: a member with no id at all still has a
			// row in the file, and the row is what the operator has to edit.
			ref.Where = fmt.Sprintf("members[%d].bot_token", i)
			add(ref, "required in isolated mode; each member's pod holds its own bot token")
		}
	}
	for i, e := range c.Endpoints {
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
func (c *Config) validateSecrets(p *problems, s *Secrets) {
	s = s.orDefault()
	for _, ref := range c.secretRefs() {
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
	c.validateSecrets(p, NewSecrets(SecretOptions{LookupEnv: lookupEnv}))
	sort.Strings(p.missingEnv)
	return p.missingEnv
}

// MissingSecretNames returns the configuration paths of every secret that could not be
// resolved, sorted — the whole set an operator has to fix, whatever form each one was
// meant to arrive in.
func (c *Config) MissingSecretNames(s *Secrets) []string {
	p := &problems{}
	c.validateSecrets(p, s.orDefault())
	sort.Strings(p.missingSecrets)
	return p.missingSecrets
}
