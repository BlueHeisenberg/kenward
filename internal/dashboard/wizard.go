package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// wizardStep is one screen of the first run.
type wizardStep struct {
	Slug  string
	Title string
	// Short is what the progress list shows.
	Short string
}

// wizardSteps is the flow, in order.
//
// The order is not arbitrary and is not a matter of taste. The admin account comes
// second, immediately after the machine has been described and before anything about the
// household exists, because every question after it is one an unauthenticated stranger
// must not be able to answer — and the only thing standing in front of them until then
// is a token this process printed on a loopback socket. Exposure is not here at all: it
// is chosen from the dashboard afterwards, never during a flow that runs before there is
// an account to protect.
var wizardSteps = []wizardStep{
	{Slug: "install", Title: "Where this is installed", Short: "Install"},
	{Slug: "admin", Title: "Your admin account", Short: "Account"},
	{Slug: "telegram", Title: "The Telegram bot", Short: "Telegram"},
	{Slug: "endpoints", Title: "The machines that answer", Short: "Endpoints"},
	{Slug: "trust", Title: "Who can read what", Short: "Trust"},
	// Isolated mode only, and asked here because it cannot be asked earlier: the
	// answer that creates these variables is the one on the step above. In simple
	// mode there is one bot and one node passphrase and this step is not in the flow
	// at all — see wizardStep.applies.
	{Slug: "bots", Title: "Each member's own bot and passphrase", Short: "Bots"},
	// Named for the question that leads it rather than for the settings under it. The
	// identity question decides how many Telegram bots this household has to make;
	// filing it under "Everything else", beneath "everything below already has an
	// answer", told exactly the reader it is aimed at to skip it.
	{Slug: "advanced", Title: "Who kenward is, and everything else", Short: "Identity"},
	{Slug: "review", Title: "Check, then write it", Short: "Write"},
}

func stepIndex(slug string) int {
	for i, s := range wizardSteps {
		if s.Slug == slug {
			return i
		}
	}
	return -1
}

// applies reports whether this household is asked this step at all.
//
// Only one step is conditional and it is conditional on a question asked before it: the
// per-member bots exist in isolated mode and nowhere else, so in simple mode the step is
// skipped forwards on the way in, skipped backwards by the Back control, and left out of
// the progress list — a numbered list with a step nobody can reach in it is a wizard that
// looks stuck.
func (s wizardStep) applies(st *wizardState) bool {
	return s.Slug != "bots" || st.Mode == config.ModeIsolated
}

// nextStep and prevStep walk the flow, skipping the steps this household does not have.
//
// prevStep steps over the account step rather than offering it, because that step is not
// re-enterable: the account exists by the time anything after it is on screen, there is
// no reset flow, and re-submitting the form is a 409. Everything else in the wizard is a
// form that can be filled in again.
func nextStep(idx int, st *wizardState) string {
	for i := idx + 1; i < len(wizardSteps); i++ {
		if wizardSteps[i].applies(st) {
			return wizardSteps[i].Slug
		}
	}
	return wizardSteps[len(wizardSteps)-1].Slug
}

func prevStep(idx int, st *wizardState) string {
	for i := idx - 1; i >= 0; i-- {
		if wizardSteps[i].Slug == "admin" || !wizardSteps[i].applies(st) {
			continue
		}
		return wizardSteps[i].Slug
	}
	return ""
}

// wizardEndpoint is one machine as the wizard has it so far.
type wizardEndpoint struct {
	Name      string
	BaseURL   string
	Model     string
	APIKeyEnv string
	APIKey    string
	Tiers     string
	// ContextWindow is what the endpoint published on /v1/models, or what the
	// operator typed. Zero means neither, and kenward's own default applies.
	ContextWindow int
	// Reach is the last probe's prose, for the operator to read next to the address
	// they typed.
	Reach string
	// Learned is what /v1/models said: the model names it serves and the window it
	// published. It is prose because it is advice, not a value: the operator still
	// chooses, and an endpoint that publishes nothing is not a problem.
	Learned string
}

// wizardMemberSecret is one member's own two secrets in isolated mode, with the
// variables the written configuration will name them by.
//
// Both are shown on the form because both have to be found by a person: the token is
// something that member creates in BotFather and hands over, and the passphrase is
// something somebody chooses. Neither is generated here, and the passphrase in
// particular is deliberately not: the isolated-mode statement in internal/privacy says
// that the person who runs this machine cannot read a member's memory, and a wrapping
// secret this wizard invented and wrote into the operator's .env would be that promise
// broken by the screen that made it.
type wizardMemberSecret struct {
	ID   string
	Name string
	// TokenEnv and PassphraseEnv are derived from the id, by the same rule
	// internal/setup uses, and shown so the operator can match them to the compose
	// file or the unit they are about to write.
	TokenEnv      string
	PassphraseEnv string
	Token         string
	Passphrase    string
}

// wizardState is every answer collected so far.
//
// It lives on the session rather than in hidden form fields, for one reason that
// outranks the convenience: the bot token passes through it, and a token in a hidden
// field is a token in the browser's DOM, in its back/forward cache, and in whatever
// autofill decides to keep.
type wizardState struct {
	// Container records that this will run in a container, which is the only thing
	// that decision changes: a container's home directory is not where anybody
	// expects state to persist, so a data_dir is written into the file.
	Container bool
	DataDir   string

	HouseholdName string
	MemberNames   []string

	BotToken    string
	BotTokenEnv string
	WriteEnvile bool

	Endpoints []wizardEndpoint

	Mode config.Mode

	// MemberSecrets is each member's own bot token and passphrase, collected in
	// isolated mode and empty in simple mode. It lives on the session for the reason
	// the household token does, and more so: these are the secrets that make one pod
	// unable to read another's, and one of them in a hidden field would be one of them
	// in the browser's DOM and back/forward cache.
	MemberSecrets []wizardMemberSecret

	// Agents is the identity question — one assistant for the household, or one
	// each — and Persona is kenward's own writing. They live on the advanced step
	// beside the other household choices and deliberately nowhere near the trust
	// step: one is a security question answered by topology and the other is a
	// presentation question that costs nothing, and putting them on one screen is
	// how a household comes to believe a bot of their own sealed their memory.
	Agents config.Agents
	// GroupChatID is the household group's Telegram id, held as the operator typed it
	// so that a rejected answer survives the re-render. Required under one assistant
	// each: kenward speaks only in the group there, so a household without one has no
	// kenward in it.
	GroupChatID string
	// Persona is the household's, never a member's. A member's own is written by the
	// member in Telegram, and there is no form here that could ask on their behalf.
	Persona config.PersonaConfig

	// Advanced, all with defaults that are what the file would have said anyway.
	SearchLimit  int
	MaxProposals int
	// HistoryReset is history.reset_every: how often a conversation's recent turns
	// are dropped. "0s" is off and is the default. It is not IdleTimeout, which is
	// about a member's key, and the two are deliberately in different fieldsets on
	// the page for that reason.
	HistoryReset  string
	IdleTimeout   string
	UpdateChannel string
	// CloudEveryone widens every chain to include the tiers that leave the house.
	// It is off, it is a separate question, and it is the only way a private
	// conversation in a configuration this wizard writes can reach a provider.
	CloudEveryone bool
}

// newWizardState is the wizard's opening position: every default the file would have
// taken anyway, so that somebody who presses through without reading gets the same
// configuration `kenward setup` would have written.
func newWizardState() *wizardState {
	return &wizardState{
		HouseholdName: setup.DefaultHouseholdName,
		BotTokenEnv:   setup.DefaultBotTokenEnv,
		WriteEnvile:   true,
		SearchLimit:   config.DefaultSearchLimit,
		MaxProposals:  config.DefaultMaxProposalsPerTurn,
		HistoryReset:  config.Duration(config.DefaultHistoryReset).String(),
		IdleTimeout:   config.Duration(config.DefaultIdleTimeout).String(),
		UpdateChannel: string(config.DefaultUpdateChannel),
		Mode:          config.ModeSimple,
		Agents:        config.DefaultAgents,
		// One empty row, so the endpoints step opens on a form rather than on a
		// page whose only control is "add another".
		Endpoints: []wizardEndpoint{{}},
	}
}

// parseEndpointRows reads the endpoint table back off the form.
//
// Rows are addressed by index in the order they were rendered, and a row with nothing in
// it at all is dropped rather than validated — that is the empty row the page opens with,
// and refusing it would mean an operator with one endpoint has to delete a box before
// they can continue.
func parseEndpointRows(r *http.Request) []wizardEndpoint {
	var out []wizardEndpoint
	for i := 0; ; i++ {
		p := fmt.Sprintf("endpoint.%d.", i)
		if _, ok := r.PostForm[p+"name"]; !ok {
			break
		}
		e := wizardEndpoint{
			Name:          strings.TrimSpace(r.PostFormValue(p + "name")),
			BaseURL:       strings.TrimSpace(r.PostFormValue(p + "base_url")),
			Model:         strings.TrimSpace(r.PostFormValue(p + "model")),
			APIKeyEnv:     strings.TrimSpace(r.PostFormValue(p + "api_key_env")),
			APIKey:        r.PostFormValue(p + "api_key"),
			Tiers:         strings.TrimSpace(r.PostFormValue(p + "tiers")),
			ContextWindow: atoi(r.PostFormValue(p+"context_window"), 0),
		}
		if e.Name == "" && e.BaseURL == "" && e.Model == "" {
			continue
		}
		if e.Tiers == "" {
			// The same suggestion the terminal wizard makes, from the same rule:
			// a machine in the house is local, anything else is a provider.
			if setup.HostIsLocal(e.BaseURL) {
				e.Tiers = setup.LocalTier
			} else {
				e.Tiers = setup.CloudTier
			}
		}
		out = append(out, e)
	}
	return out
}

// syncMemberSecrets brings the per-member rows into line with the people named on the
// install step, keeping whatever has already been typed for somebody who is still in the
// list. It is called before the step renders and before its form is read, so that going
// Back and adding a person does not leave the form describing the old household.
func (st *wizardState) syncMemberSecrets() {
	was := make(map[string]wizardMemberSecret, len(st.MemberSecrets))
	for _, ms := range st.MemberSecrets {
		was[ms.ID] = ms
	}
	out := make([]wizardMemberSecret, 0, len(st.MemberNames))
	for _, name := range st.MemberNames {
		id := setup.Slugify(name)
		if id == "" {
			continue
		}
		ms := was[id]
		ms.ID, ms.Name = id, name
		ms.TokenEnv = envVarFor(setup.MemberBotTokenPrefix, id)
		ms.PassphraseEnv = envVarFor(setup.MemberPassphrasePrefix, id)
		out = append(out, ms)
	}
	st.MemberSecrets = out
}

// readMemberSecrets reads the per-member boxes back off the form. Values are recorded
// before anything is judged, so a refusal re-renders what was typed rather than an empty
// form — the same rule the telegram step's token and the identity step's answer follow.
func (st *wizardState) readMemberSecrets(r *http.Request) {
	st.syncMemberSecrets()
	for i := range st.MemberSecrets {
		p := "member." + st.MemberSecrets[i].ID + "."
		st.MemberSecrets[i].Token = strings.TrimSpace(r.PostFormValue(p + "bot_token"))
		st.MemberSecrets[i].Passphrase = r.PostFormValue(p + "passphrase")
	}
}

// checkMemberSecrets refuses an isolated household whose members have no secrets of
// their own, by name.
//
// This is the same shape as checkGroupChat and exists for the same reason. The
// configuration this wizard writes names two variables per member — kenward reads both
// at startup and refuses to start without either, for a member who has claimed nothing
// yet as much as for one who has, because D-023 puts their bot up before the claim — and
// the wizard used to name all of them and ask for none. `kenward doctor` reported it
// afterwards, in exactly the words that would have fixed it, to an operator the wizard
// had just told to restart and invite the household.
//
// Blank is refused rather than warned about because a warning is what already existed
// and did not work. There is no "later" answer here on purpose: later is what the file
// says when it names a variable nobody has set, and a household in that state does not
// start at all.
func checkMemberSecrets(st *wizardState) error {
	if st.Mode != config.ModeIsolated {
		return nil
	}
	// The .env beside the configuration is the only way this wizard can deliver a
	// secret. Collecting four of them into a file nobody asked for would be four
	// secrets typed and thrown away, and the household would be exactly as unstartable
	// as it was before anybody typed them.
	if !st.WriteEnvile {
		return errors.New("Each person's own bot token and passphrase have nowhere to go: " +
			"the box that writes them to a .env file beside the configuration is unticked. " +
			"kenward.yaml never holds a secret, so that file is the only thing this wizard can " +
			"put one in. Go Back to \"The Telegram bot\" and tick \"Write it to a .env file for " +
			"me\", or export all of these yourself and set this household up with `kenward setup` " +
			"at a terminal, which prints the list.")
	}
	var missing []string
	for _, ms := range st.MemberSecrets {
		if strings.TrimSpace(ms.Token) == "" {
			missing = append(missing, ms.Name+"'s bot token")
		}
		if strings.TrimSpace(ms.Passphrase) == "" {
			missing = append(missing, ms.Name+"'s passphrase")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("Still needed: %s. In isolated mode each person has a pod of their own, "+
		"and it serves nobody without both of these: the bot only they speak on, and the passphrase "+
		"that unwraps their key and no other member's. The file this wizard is about to write names "+
		"a variable for each of them, and kenward refuses to start while one has no value — before "+
		"anybody has claimed an invite as much as after, because each person's bot has to be up "+
		"for them to claim on. Ask each of them to make a bot with @BotFather and send you the "+
		"token, choose a passphrase of their own with them, and paste both here. Nothing typed on "+
		"this page reaches kenward.yaml; it goes to the .env file beside it.",
		strings.Join(missing, ", "))
}

// answers projects the collected state onto internal/setup's own scripted-install
// answers.
//
// This is the whole reason the wizard's questions map onto that type rather than
// building a configuration here. internal/setup already decides what a member id is,
// what a tier chain defaults to, which endpoints count as being in the house, and what
// the file looks like when it is written — and those decisions are load-bearing privacy
// behaviour, not formatting. A second implementation of them behind a web form would be
// a second answer to "does david's chain reach a provider", which is the one question
// this product exists to answer once.
//
// spaces maps the household and each member id onto the lore space created for them.
func (st *wizardState) answers(spaces map[string]string) *setup.Answers {
	groupChat, _ := parseGroupChatID(st.GroupChatID)
	a := &setup.Answers{
		Mode:          st.Mode,
		Agents:        st.Agents,
		GroupChatID:   groupChat,
		Persona:       st.Persona,
		HouseholdName: st.HouseholdName,
		SharedSpace:   spaces[householdSpaceKey],
		BotToken:      st.BotToken,
		BotTokenEnv:   st.BotTokenEnv,
		MemberNames:   append([]string(nil), st.MemberNames...),
		MemberSpaces:  map[string]string{},
		WriteEnvFile:  st.WriteEnvile,
	}
	for id, space := range spaces {
		if id != householdSpaceKey {
			a.MemberSpaces[id] = space
		}
	}
	// Collected only in isolated mode, and carried through internal/setup so that
	// they land in the same .env, under the same names, as the household's own token.
	if len(st.MemberSecrets) > 0 {
		a.MemberBotTokens = map[string]string{}
		a.MemberPassphrases = map[string]string{}
		for _, ms := range st.MemberSecrets {
			a.MemberBotTokens[ms.ID] = ms.Token
			a.MemberPassphrases[ms.ID] = ms.Passphrase
		}
	}
	for _, e := range st.Endpoints {
		a.Endpoints = append(a.Endpoints, setup.EndpointAnswer{
			Name:      e.Name,
			BaseURL:   e.BaseURL,
			Model:     e.Model,
			APIKeyEnv: e.APIKeyEnv,
			APIKey:    e.APIKey,
			Tiers:     splitList(e.Tiers),
			// What /v1/models published, or what the operator typed over it.
			// Without this the endpoints step reads a 262144-token window off
			// vLLM, shows it, and writes 16384.
			ContextWindow: e.ContextWindow,
		})
	}
	return a
}

// householdSpaceKey is the key the household's own shared space is held under in the
// space map. It cannot collide with a member id: internal/setup slugifies names and a
// slug never contains a space character.
const householdSpaceKey = "household shared"

// applyAdvanced writes the wizard's advanced answers onto a configuration
// internal/setup has already built.
//
// They are applied afterwards rather than through Answers because Answers deliberately
// has no field for any of them: they are settings with safe defaults, and a scripted
// install that says nothing about them should get those defaults rather than a widened
// surface. The wizard collects them because a person looking at a screen can see what
// they are changing.
func (st *wizardState) applyAdvanced(cfg *config.Config) error {
	if st.SearchLimit > 0 {
		cfg.Memory.SearchLimit = st.SearchLimit
	}
	if st.MaxProposals > 0 {
		cfg.Capture.MaxProposalsPerTurn = st.MaxProposals
	}
	if strings.TrimSpace(st.HistoryReset) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(st.HistoryReset))
		if err != nil || d < 0 {
			return fmt.Errorf("%q is not a length of time; write it as 0s, 6h or 24h", st.HistoryReset)
		}
		if d > config.MaxHistoryReset {
			// Said here rather than left to config.Validate, which would answer a
			// person looking at a text box with a field path.
			return fmt.Errorf("%q is longer than %s; resets are counted from midnight, so a longer gap cannot be kept — use 0s for off", st.HistoryReset, config.MaxHistoryReset)
		}
		cfg.History.ResetEvery = config.Duration(d)
	}
	if strings.TrimSpace(st.IdleTimeout) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(st.IdleTimeout))
		if err != nil || d < 0 {
			return fmt.Errorf("%q is not a length of time; write it as 0s, 30m or 6h", st.IdleTimeout)
		}
		cfg.Session.IdleTimeout = config.Duration(d)
	}
	switch config.UpdateChannel(st.UpdateChannel) {
	case config.UpdateStable, config.UpdateEdge, config.UpdateOff:
		cfg.Update.Channel = config.UpdateChannel(st.UpdateChannel)
	case "":
	default:
		return fmt.Errorf("%q is not an update channel; use stable, edge or off", st.UpdateChannel)
	}
	if st.CloudEveryone {
		// Every tier every endpoint answers for, in the order the endpoints were
		// entered. It is the widening the question asked for, applied to the group
		// and to every member, and it is the only path in this wizard that can put
		// a provider in a private chain.
		var chain []string
		seen := map[string]bool{}
		for _, e := range cfg.Endpoints {
			for _, t := range e.Tags {
				if !seen[t] {
					seen[t] = true
					chain = append(chain, t)
				}
			}
		}
		if len(chain) > 0 {
			cfg.Household.Tiers = chain
			for i := range cfg.Members {
				cfg.Members[i].Tiers = append([]string(nil), chain...)
			}
		}
	}
	return nil
}

// probeEndpoints fills in what each endpoint says about itself: whether it answers, and
// what context window it publishes.
//
// The two are separate calls on purpose, and the separation is internal/setup's rather
// than this package's. The reachability probe is a bare TCP connect, made against an
// address somebody may have mistyped; the model list is an HTTP request with a
// credential on it, made only against an address they have confirmed. Collapsing them
// would send an authenticated request to whatever host a typo names.
func (s *Server) probeEndpoints(ctx context.Context, eps []wizardEndpoint) []wizardEndpoint {
	probe, models := s.deps.probe(), s.deps.models()
	out := make([]wizardEndpoint, len(eps))
	copy(out, eps)
	for i := range out {
		if strings.TrimSpace(out[i].BaseURL) == "" {
			continue
		}
		res := probe(ctx, out[i].BaseURL)
		out[i].Reach = strings.TrimSpace(res.Describe())
		if res.State != setup.Answered {
			out[i].Learned = ""
			continue
		}
		list, err := models(ctx, out[i].BaseURL, out[i].APIKey)
		if err != nil {
			out[i].Learned = "it answered, but did not list its models: " + err.Error()
			continue
		}
		out[i].Learned = describeModels(list)
		// The window is read rather than typed wherever the server publishes one.
		// vLLM does, on every entry, and the number it publishes is the one that
		// binds — --max-model-len is routinely far below what the model card says,
		// and it is the server that refuses the request.
		if w := windowFor(list, out[i].Model); w > 0 {
			out[i].ContextWindow = w
		}
		if out[i].Model == "" && len(list) == 1 {
			out[i].Model = list[0].ID
		}
	}
	return out
}

// windowFor picks the published window for the model this endpoint is configured with,
// or the only one on offer when nothing is configured yet.
func windowFor(list []setup.ModelInfo, model string) int {
	model = strings.TrimSpace(model)
	for _, m := range list {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	if model == "" && len(list) == 1 {
		return list[0].ContextWindow
	}
	return 0
}

func describeModels(list []setup.ModelInfo) string {
	if len(list) == 0 {
		return "it answered and listed no models."
	}
	var parts []string
	for _, m := range list {
		if m.ContextWindow > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d tokens)", m.ID, m.ContextWindow))
			continue
		}
		parts = append(parts, m.ID+" (does not publish a window)")
	}
	return "it serves " + strings.Join(parts, ", ") + "."
}

// checkPersonaLengths reports an over-long persona field in the words of somebody
// looking at a text box. config.Validate refuses the same values and would name a field
// path instead; this runs first so the page can answer where the typing happened.
func checkPersonaLengths(p config.PersonaConfig) error {
	for _, f := range []struct {
		label string
		value string
		max   int
	}{
		{"The language", p.Language, config.MaxPersonaLine},
		{"The tone", p.Tone, config.MaxPersonaLine},
		{"The character", p.Character, config.MaxPersonaCharacter},
	} {
		if utf8.RuneCountInString(f.value) > f.max {
			return fmt.Errorf("%s is longer than %d characters. Persona text rides in every prompt and is never trimmed to fit, so a long one costs the memory kenward would otherwise have retrieved to answer with", f.label, f.max)
		}
	}
	return nil
}

// The two remedies for one agent each in simple mode. They differ because the two pages
// offer different controls, and advice that cannot be followed from where it is given is
// worse than none: the wizard has a Back control that reaches the trust step, and the
// settings page has no mode field at all and is not going to grow one — changing a
// household's mode moves every member's key and there is no migration.
const (
	wizardModeRemedy = "go Back to \"Who can read what\" and seal the household in isolated mode, " +
		"where each member already has a bot of their own"
	settingsModeRemedy = "change the mode — which is not editable here, and not by accident: it decides " +
		"where every member's key lives, so it is chosen when the household is set up. Changing " +
		"it afterwards means editing `mode:` in kenward.yaml by hand, restarting, and re-enrolling " +
		"each member with a bot of their own"
)

// checkAgents maps the identity question's answer onto a value.
//
// It refuses per_member in simple mode rather than downgrading it, and it says why in
// the words of somebody who has just clicked a radio button. config.Validate refuses
// the same pair and would answer with a field path; this runs first so the page can
// answer where the choosing happened, and so the wizard never writes a file kenward
// would then refuse to load.
//
// The reason is a counting one and is deliberately not a privacy one: an agent is a
// Telegram contact, simple mode runs one bot for the whole household, and two agents
// behind one contact are one agent. The trust question is asked on its own step and
// this is not it. That sentence leads, in a sentence of its own — it used to be buried
// in the middle of one sixty-word sentence carrying the arithmetic, both remedies and
// the definition of an agent at once.
//
// remedy is the caller's, because only the caller knows what the reader can actually do
// from the page they are looking at.
func checkAgents(raw string, mode config.Mode, remedy string) (config.Agents, error) {
	switch agents := config.Agents(strings.TrimSpace(raw)); agents {
	case "":
		return config.DefaultAgents, nil
	case config.AgentsShared:
		return agents, nil
	case config.AgentsPerMember:
		if mode == config.ModeSimple {
			return "", fmt.Errorf("In simple mode, two agents behind one contact are one agent. "+
				"One assistant each needs a Telegram bot for each person, and simple mode runs one bot "+
				"for the whole household. Choose one assistant for everybody, or %s.", remedy)
		}
		return agents, nil
	default:
		return "", fmt.Errorf("%q is not an answer to how many assistants this household has; it is shared or per_member", agents)
	}
}

// botPrivacyRefusal is what a bot with Telegram's privacy mode still on gets.
//
// The consequence goes first, because it is the part that is invisible. There is no
// error to search for and no log line to find: with privacy mode on the bot receives
// nothing sent in a group — not plain messages, not a reply to it, not an @mention — so
// the household adds it to their family group and it ignores everyone, forever, in
// silence.
//
// The re-add sentence is not a detail. Telegram applies the change only to groups the
// bot joins afterwards, so an operator who flips the setting, goes back to the group they
// already added the bot to and sees nothing happen will conclude the fix did not work.
func botPrivacyRefusal(username string) string {
	name := "this bot"
	if username != "" {
		name = "@" + username
	}
	return fmt.Sprintf("%s cannot see messages in a group chat: Telegram's bot privacy mode is on, "+
		"which is the default for every new bot. With it on the bot receives nothing sent in a group "+
		"— not plain messages, not a reply to it, not an @mention — so the group conversation never "+
		"reaches kenward and nothing anywhere reports an error. Fix it in Telegram: send /setprivacy "+
		"to @BotFather, choose %s, choose Disable, then submit this page again. If the bot is already "+
		"in the group, remove it and add it again afterwards — Telegram applies the change only to "+
		"groups the bot joins after it, so the setting alone will look as though it did nothing.",
		name, name)
}

// checkGroupChat reads the household group's Telegram id, and refuses to leave it out
// under one assistant each.
//
// Under `agents: per_member` every private chat belongs to somebody's own assistant and
// kenward speaks only in the group, so a household with no group chat id has no kenward
// in it at all — the supervisor creates the group's pod only when household.group_chat_id
// is set. `kenward doctor` warns about it after the fact, which is where this used to be
// caught and is not good enough: nothing in either wizard told anybody to run doctor.
//
// Any non-zero whole number is accepted. The shape is a question rather than a rule, for
// the same reason the bot token's is: Telegram's numbering is theirs to change.
func checkGroupChat(agents config.Agents, raw string) (int64, error) {
	id, ok := parseGroupChatID(raw)
	switch {
	case ok:
		return id, nil
	case agents != config.AgentsPerMember:
		// Optional with one shared assistant, which answers every private chat
		// whether or not a group is mapped. Blank is a household that has not made
		// its group yet, and the settings page maps one later.
		if strings.TrimSpace(raw) == "" {
			return 0, nil
		}
		return 0, fmt.Errorf("%q is not a Telegram chat id: it is a whole number, and a group's is negative — like -1001234567890", raw)
	case strings.TrimSpace(raw) == "":
		return 0, errors.New("One assistant each needs the household's own group. kenward itself lives in " +
			"that group chat and nowhere else — every private chat belongs to somebody's own assistant — " +
			"so without its id this household has no kenward in it at all, in the group or anywhere. " +
			"Add the bot to the group, send a message there, and read the id off " +
			"https://api.telegram.org/bot<TOKEN>/getUpdates: it is the negative number after \"chat\":{\"id\":")
	default:
		return 0, fmt.Errorf("%q is not a Telegram chat id: it is a whole number, and a group's is negative — like -1001234567890", raw)
	}
}

// parseGroupChatID reads a chat id. Zero is not one: it is the value that means no group
// is configured, which is exactly what checkGroupChat refuses to write under one each.
func parseGroupChatID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// splitList turns a comma-separated field into a list, dropping blanks and repeats.
func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		v := strings.TrimSpace(part)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// atoi is strconv.Atoi with a fallback, for form fields where an empty box means "the
// default" rather than "zero".
func atoi(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return v
}
