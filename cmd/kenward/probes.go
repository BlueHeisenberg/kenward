package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// probes are doctor's seams onto everything outside this process.
//
// They exist as fields rather than as direct calls so that doctor's rendering — the
// most important output the product produces — can be golden-tested against a lore
// that answers, a lore that does not, a Telegram that refuses a token and a household
// where every machine is switched off. None of those cases can be arranged with a
// real network, and the last one is the case that must not fail.
type probes struct {
	lore     func(ctx context.Context, cfg *config.Config, scope config.UnitScope) loreResult
	telegram func(ctx context.Context, token string) telegramResult
	endpoint func(ctx context.Context, ep routing.Endpoint) endpointResult
	sessions func(ctx context.Context, cfg *config.Config) sessionsResult
}

func (p probes) sessionsProbe() func(context.Context, *config.Config) sessionsResult {
	if p.sessions != nil {
		return p.sessions
	}
	return probeSessions
}

func (p probes) loreProbe() func(context.Context, *config.Config, config.UnitScope) loreResult {
	if p.lore != nil {
		return p.lore
	}
	return probeLore
}

func (p probes) telegramProbe() func(context.Context, string) telegramResult {
	if p.telegram != nil {
		return p.telegram
	}
	return probeTelegram
}

func (p probes) endpointProbe() func(context.Context, routing.Endpoint) endpointResult {
	if p.endpoint != nil {
		return p.endpoint
	}
	return probeEndpoint
}

// loreResult is what the memory check found.
type loreResult struct {
	// Err is non-nil when lore itself did not answer. That is a failure: without
	// memory there is no retrieval, no capture and no enrolment history.
	Err error
	// Spaces is one entry per configured space, in configuration order.
	Spaces []spaceResult
}

type spaceResult struct {
	Space domain.SpaceID
	// Err is nil when the space answered a search. ErrUnknownSpace means lore does
	// not hold a space with this id — usually a display name configured where an id
	// belongs — and no read will ever succeed against it.
	Err error
}

// probeLore starts `lore mcp` and asks each configured space one bounded question.
//
// The search text is a fixed marker and the limit is one; nothing retrieved is kept,
// counted or printed. `doctor` may confirm that a space answers, and may not report
// anything about what is in it — the contents of anyone's memory are not this
// command's to show.
func probeLore(ctx context.Context, cfg *config.Config, scope config.UnitScope) loreResult {
	cmd := cfg.Memory.LoreCommand
	if len(cmd) == 0 {
		return loreResult{Err: errors.New("memory.lore_command is empty")}
	}
	client, err := memory.NewClient(memory.Config{Command: cmd[0], Args: cmd[1:]})
	if err != nil {
		return loreResult{Err: err}
	}
	defer client.Close()

	var out loreResult
	for i, space := range configuredSpaces(cfg, scope) {
		_, err := client.Search(ctx, memory.SearchQuery{
			Text:   "kenward doctor",
			Spaces: []domain.SpaceID{space},
			Limit:  1,
		})
		// The first call is what starts the subprocess and completes the MCP
		// handshake, so a failure there is lore not answering rather than a
		// problem with that particular space.
		if i == 0 && err != nil && !isSpaceError(err) {
			return loreResult{Err: err}
		}
		out.Spaces = append(out.Spaces, spaceResult{Space: space, Err: err})
	}
	return out
}

// isSpaceError reports whether an error is about one space rather than about lore.
func isSpaceError(err error) bool {
	return errors.Is(err, memory.ErrUnknownSpace) || errors.Is(err, memory.ErrNotWriter)
}

// configuredSpaces lists the lore spaces this process uses, shared space first.
//
// It is scoped for the same reason the bot tokens are, and the scoping here matters
// more. A pod has its own lore instance over its own LORE_HOME, so a member's pod
// legitimately holds only the shared space and that member's own private one — probing
// a sibling's would fail on a perfectly healthy pod, and on a household that had put
// every space in one store it would *succeed*, which is doctor searching a member's
// private memory from another member's container. Neither is acceptable.
//
// A member's pod does need the shared space: its capture engine writes household
// knowledge there (internal/supervisor's buildUnit). The group's pod needs only that.
func configuredSpaces(cfg *config.Config, scope config.UnitScope) []domain.SpaceID {
	var spaces []domain.SpaceID
	if cfg.Household.SharedSpace != "" {
		spaces = append(spaces, domain.SpaceID(cfg.Household.SharedSpace))
	}
	for _, m := range cfg.Members {
		if m.PrivateSpace != "" && scope.Serves(m.ID) {
			spaces = append(spaces, domain.SpaceID(m.PrivateSpace))
		}
	}
	return spaces
}

// telegramResult is what the transport check found.
type telegramResult struct {
	// Username is the bot the token belongs to, without the leading @. It is the
	// one thing that tells an operator they are pointed at the bot they meant.
	Username string
	Err      error
}

// telegramAPIBase is the Bot API root. It is a variable so a test can point the
// production probe at a local server; nothing else changes it.
var telegramAPIBase = "https://api.telegram.org"

// probeTelegram authorises the bot token and reports which bot it is.
//
// It calls getMe directly rather than through internal/transport because the
// transport does not expose the bot's identity — it verifies the token at
// construction and discards the answer — and `✓ Telegram authorises as @name` is the
// line that catches a household pointed at last month's test bot. The token is put in
// the URL path, which is where the Bot API wants it, and is scrubbed out of every
// error before it can reach a terminal or a log.
func probeTelegram(ctx context.Context, token string) telegramResult {
	if token == "" {
		return telegramResult{Err: errors.New("the bot token variable is empty")}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	endpoint := telegramAPIBase + "/bot" + url.PathEscape(token) + "/getMe"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return telegramResult{Err: scrubToken(err, token)}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return telegramResult{Err: scrubToken(err, token)}
	}
	defer resp.Body.Close()

	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return telegramResult{Err: fmt.Errorf("telegram answered %s with something that is not a getMe response", resp.Status)}
	}
	if !body.OK {
		if body.Description != "" {
			return telegramResult{Err: fmt.Errorf("telegram refused the token: %s", body.Description)}
		}
		return telegramResult{Err: fmt.Errorf("telegram refused the token (%s)", resp.Status)}
	}
	return telegramResult{Username: body.Result.Username}
}

// scrubToken removes a bot token from an error before it is shown to anyone. net/url
// and net/http both quote the full URL in their errors, and that URL carries the
// token.
func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), token, "<bot token>")
	msg = strings.ReplaceAll(msg, url.PathEscape(token), "<bot token>")
	return errors.New(msg)
}

// endpointResult is what one endpoint probe found.
//
// None of its states is an error. A household's inference machine is switched off
// most of the time, and the container's HEALTHCHECK runs `doctor`: treating a
// sleeping GPU box as unhealthy would restart a working household in a loop.
type endpointResult struct {
	Name    string
	Tiers   []string
	Reached bool
	Elapsed time.Duration
	// Detail says what happened, in the operator's terms, for an endpoint that did
	// not answer.
	Detail string
}

// probeEndpoint uses the setup wizard's own prober: a plain TCP connect and nothing
// more. It is the same question setup asks while somebody is typing an address, and
// answering it the same way in both places means an endpoint that looked fine during
// setup does not mysteriously look different here.
func probeEndpoint(ctx context.Context, ep routing.Endpoint) endpointResult {
	res := setup.DefaultProbe(ctx, ep.BaseURL)
	out := endpointResult{Name: ep.Name, Tiers: ep.Tags, Elapsed: res.Elapsed}
	switch res.State {
	case setup.Answered:
		out.Reached = true
	case setup.NoAnswer:
		out.Detail = "no answer — switched off, asleep, or behind a firewall"
	case setup.Refused:
		out.Detail = "connection refused — the host is up, nothing is listening on that port"
	case setup.Unresolved:
		out.Detail = "name does not resolve"
	case setup.BadURL:
		out.Detail = "base_url is not an address kenward can dial"
	default:
		out.Detail = "did not answer"
	}
	return out
}
