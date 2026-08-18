package main

import (
	"context"
	"errors"
	"time"

	"github.com/BlueHeisenberg/lore"

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
	loreInit func(ctx context.Context, home, device string) (bool, error)
	sync     func(ctx context.Context) syncResult
	telegram func(ctx context.Context, token string) telegramResult
	endpoint func(ctx context.Context, ep routing.Endpoint) endpointResult
	sessions func(ctx context.Context, cfg *config.Config) sessionsResult
}

func (p probes) syncProbe() func(context.Context) syncResult {
	if p.sync != nil {
		return p.sync
	}
	return probeSync
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

func (p probes) loreInitProbe() func(context.Context, string, string) (bool, error) {
	if p.loreInit != nil {
		return p.loreInit
	}
	return runLoreInit
}

// runLoreInit creates the account, device and personal space in an empty lore home.
//
// It is a library call. It was a subprocess until lore exported Init, because account
// and device generation lived behind internal/keys and the binary was the whole of the
// surface for this job.
//
// # Idempotence is lore's to decide, not this function's
//
// Init refuses a home that already holds an account.json, a device.json or a lore.db,
// and returns lore.ErrAlreadyInitialised having written nothing. That is the check
// kenward used to make for itself by reading the directory, and lore's is the better
// one: the rule kenward wanted was "never let a new account adopt an existing store",
// and lore names the three files that would mean rather than inferring it from a
// directory being non-empty. The caller treats that error as success.
//
// # The recovery code
//
// Init returns one, once, and lore stores it nowhere. This drops it, deliberately, on
// two counts. It is a KDF factor for relay signup and backup — neither of which any
// kenward deployment configures — so nothing here needs it; and the alternative is
// putting a member's account recovery factor into `podman logs`, which is the
// operator's to read, in the one mode whose purpose is that the operator holds nothing
// of a member's. A member who later wants one mints it from inside their own pod with
// `lore recovery new`, which needs no previous code.
//
// The account and device ids it also returns are dropped for the same reason: they are
// not the host's business either. created reports only that a home was made, which is
// the one bit of this an operator watching a fresh household come up needs.
func runLoreInit(ctx context.Context, home, device string) (created bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// The Identity is discarded at the call rather than bound to a variable, so there
	// is nothing here for a later edit to log by reaching for the struct.
	switch _, err := lore.Init(home, device); {
	case err == nil:
		return true, nil
	case errors.Is(err, lore.ErrAlreadyInitialised):
		return false, nil
	default:
		return false, err
	}
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

// probeLore opens this machine's lore home and asks each configured space one
// bounded question.
//
// The search text is a fixed marker and the limit is one; nothing retrieved is kept,
// counted or printed. `doctor` may confirm that a space answers, and may not report
// anything about what is in it — the contents of anyone's memory are not this
// command's to show.
func probeLore(ctx context.Context, cfg *config.Config, scope config.UnitScope) loreResult {
	client, err := memory.NewClient(memory.Config{})
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
		// A failure that is not about this particular space is lore itself not
		// answering — the store went away under us, or it is contended — and it
		// would repeat identically for every remaining space.
		if i == 0 && err != nil && !isSpaceError(err) {
			return loreResult{Err: err}
		}
		out.Spaces = append(out.Spaces, spaceResult{Space: space, Err: err})
	}
	return out
}

// syncResult is what the shared-memory check found.
//
// It answers one question and it is the question the old report could not: is this
// store's copy of the household's memory actually joined to anybody else's. The
// space check above says the space is here; this says whether anything crosses.
type syncResult struct {
	// Home is the lore home asked about, for an operator who needs to know which
	// store answered — a pod's is not the one on the host.
	Home string
	// Status is the daemon's own report. Zero when Err is set.
	Status memory.SyncStatus
	// Err is why the daemon could not be asked. memory.ErrNoSyncDaemon means there
	// is none running, which is the normal state outside an isolated pod.
	Err error
}

// probeSync asks the `lore serve` on this process's LORE_HOME about itself.
//
// It reports peers and rounds and nothing else. What is in the household's memory is
// not a health check's to show, and `doctor` runs as a container HEALTHCHECK.
func probeSync(ctx context.Context) syncResult {
	home := memory.DefaultLoreHome()
	st, err := memory.ReadSyncStatus(ctx, home)
	return syncResult{Home: home, Status: st, Err: err}
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
	// ReadsGroupMessages is getMe's can_read_all_group_messages: false means bot
	// privacy mode is on, and a bot with it on receives nothing at all in a group
	// chat — not plain messages, not even an @mention. It is on by default for every
	// new bot, and its failure has no symptom: nothing arrives, so nothing is logged
	// and nothing anywhere says why the family group is being ignored.
	ReadsGroupMessages bool
	Err                error
}

// probeTelegram authorises the bot token and reports what Telegram says about it.
//
// The call itself is internal/setup's, and deliberately: the wizard asks the same
// question at the moment the token is typed, and two getMe implementations would
// eventually give a household two answers about whether its bot can hear the group.
// This is the adapter onto the shape the doctor report already renders.
func probeTelegram(ctx context.Context, token string) telegramResult {
	info, err := setup.DefaultTelegramProbe(ctx, token)
	if err != nil {
		return telegramResult{Err: err}
	}
	return telegramResult{Username: info.Username, ReadsGroupMessages: info.ReadsGroupMessages}
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
