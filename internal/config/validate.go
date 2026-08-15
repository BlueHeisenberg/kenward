package config

import (
	"fmt"
	"net/url"
	"os"
	"sort"
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
	list       []string
	missingEnv []string
}

func (p *problems) addf(format string, args ...any) {
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

// Validate checks the configuration against the rules in the implementation contract
// and against the environment it will run in.
//
// lookupEnv may be nil, in which case the real process environment is used.
func (c *Config) Validate(lookupEnv LookupEnvFunc) error {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	p := &problems{}

	c.validateMode(p)
	tags := c.validateEndpoints(p)
	c.validateHousehold(p, tags)
	c.validateMembers(p, tags)
	c.validateTelegram(p)
	c.validateLimits(p)
	c.validateUpdate(p)
	c.validateEnv(p, lookupEnv)

	if len(p.list) == 0 {
		return nil
	}
	return &ValidationError{Problems: p.list, MissingEnv: p.missingEnv}
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

		if c.Mode == ModeIsolated {
			switch {
			case strings.TrimSpace(m.BotTokenEnv) == "":
				p.addf("%s.bot_token_env: required in isolated mode; each member's pod holds its own bot token", where)
			default:
				if first, dup := tokens[m.BotTokenEnv]; dup {
					p.addf("%s.bot_token_env: %s is already members[%d]'s token variable; sharing a bot defeats isolated mode", where, m.BotTokenEnv, first)
				} else {
					tokens[m.BotTokenEnv] = i
				}
			}
		}
	}
}

func (c *Config) validateTelegram(p *problems) {
	if c.Mode == ModeSimple && strings.TrimSpace(c.Telegram.BotTokenEnv) == "" {
		p.addf("telegram.bot_token_env: required in simple mode; the household shares one bot")
	}
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

// envRef is one place the configuration names an environment variable.
type envRef struct {
	name  string
	where string
}

// envRefs lists the variables this configuration actually depends on, in file order and
// without duplicates. Only variables the selected mode uses are listed: a leftover
// per-member token in a simple-mode file is inert, and demanding it be exported would
// be demanding a secret that nothing reads.
func (c *Config) envRefs() []envRef {
	var refs []envRef
	seen := make(map[string]bool)
	add := func(name, where string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		refs = append(refs, envRef{name: name, where: where})
	}

	if c.Mode == ModeSimple {
		add(c.Telegram.BotTokenEnv, "telegram.bot_token_env")
	}
	if c.Mode == ModeIsolated {
		for i, m := range c.Members {
			add(m.BotTokenEnv, fmt.Sprintf("members[%d].bot_token_env", i))
		}
	}
	for i, e := range c.Endpoints {
		add(e.APIKeyEnv, fmt.Sprintf("endpoints[%d].api_key_env", i))
	}
	return refs
}

// validateEnv requires every referenced variable to be set and non-empty at load time.
//
// kenward must never start half-configured. A missing provider key discovered mid-turn
// is a refusal a member cannot act on; a missing key discovered at startup is a line in
// the operator's terminal.
func (c *Config) validateEnv(p *problems, lookupEnv LookupEnvFunc) {
	for _, ref := range c.envRefs() {
		v, ok := lookupEnv(ref.name)
		switch {
		case !ok:
			p.addf("%s: environment variable %s is not set", ref.where, ref.name)
			p.missingEnv = append(p.missingEnv, ref.name)
		case strings.TrimSpace(v) == "":
			p.addf("%s: environment variable %s is set but empty", ref.where, ref.name)
			p.missingEnv = append(p.missingEnv, ref.name)
		}
	}
}

// MissingEnvNames returns the referenced environment variables that are not set or are
// empty, sorted, without validating anything else. `kenward doctor` uses it to report
// on a running node's environment.
func (c *Config) MissingEnvNames(lookupEnv LookupEnvFunc) []string {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	p := &problems{}
	c.validateEnv(p, lookupEnv)
	sort.Strings(p.missingEnv)
	return p.missingEnv
}
