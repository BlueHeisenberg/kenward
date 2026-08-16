package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	{Slug: "advanced", Title: "Everything else", Short: "Details"},
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

	// Advanced, all with defaults that are what the file would have said anyway.
	SearchLimit   int
	MaxProposals  int
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
		IdleTimeout:   config.Duration(config.DefaultIdleTimeout).String(),
		UpdateChannel: string(config.DefaultUpdateChannel),
		Mode:          config.ModeSimple,
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
	a := &setup.Answers{
		Mode:          st.Mode,
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
