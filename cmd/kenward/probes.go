package main

import (
	"context"
	"errors"
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
	loreInit func(ctx context.Context, home, device string) (bool, error)
	loreMake func(ctx context.Context, cfg *config.Config, scope config.UnitScope) error
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
	return memory.InitHome
}

func (p probes) loreMakeProbe() func(context.Context, *config.Config, config.UnitScope) error {
	if p.loreMake != nil {
		return p.loreMake
	}
	return makeOwnSpaces
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

// memoryVerdict is what a unit should do about the spaces its store does not hold.
//
// It exists so that `run` and `doctor` cannot disagree. They used to: startup treated
// a missing space as one space's problem and served, while `doctor` exited 2 on it —
// and `doctor` is the image's HEALTHCHECK, so the same condition produced a node that
// ran happily and a container marked unhealthy forever, with a restart that could not
// possibly fix it. One judgement, two callers, and the rule below is the whole of it.
type memoryVerdict struct {
	// Missing is every configured space this store does not hold, in probe order.
	Missing []domain.SpaceID
	// Fatal is whether this unit should refuse to serve. It is also what decides
	// `doctor`'s exit code, which is the point: a health check is only meaningful if
	// unhealthy means "this node would not start on this", because that is the only
	// condition a restart can act on.
	Fatal bool
}

// judgeMemory decides what a store holding some, none or all of its spaces means.
//
// # None of them is a different fault from one of them
//
// A store holding NONE of the spaces this unit is configured for is not this
// household's store at all — a fresh volume, another user's home, a restore from before
// the household existed. Serving on it is the silent amnesia checkLore's own doc comment
// describes: the node starts, authorises its bot, greets a member and remembers nothing.
// So it is fatal.
//
// A store holding some of them is one mistyped id, and one conversation's problem.
// Refusing a whole household its assistant over one member's typo would be a second,
// larger outage than the one being prevented, so it is reported and served through.
//
// # The isolated pod used to be excused this, and no longer needs to be
//
// A pod was excused outright, because every step that provisioned one of its spaces —
// `lore space create`, `lore space invite`, `lore join` — was run with `podman exec`
// INSIDE a running pod, and a pod that refused to start until its spaces existed could
// never be given them. That made the refusal an unprovisionable mode rather than a
// stricter policy. Nothing is run with `podman exec` any more, but the shape of the
// problem survives for the household's shared space: this pod asks to be admitted to it
// while it is running, so a pod that refused to start without it could still never
// ask.
//
// Creation is no longer one of those steps: a pod makes its own spaces at the
// configured ids on the way up (ownSpaces, makeOwnSpaces), and every member has a
// private space — internal/config requires one — so every pod holds at least one space
// of its own by the time this runs. A pod that holds NONE of them is therefore a pod
// whose own creation did not happen, which is a fault and not a stage. The exception is
// gone rather than narrowed, and nothing it protected has been lost: the invite
// handshake that is still somebody's to run happens in a pod that holds its private
// space and is missing only the household's, which is the "some of them" case, which
// serves.
//
// The residual is unchanged and worth naming: a pod whose volume is wiped after
// provisioning comes back looking exactly like a first boot, and nothing inside the pod
// can tell the two apart. It now recreates its own space, empty, rather than reporting
// it missing — the memory went with the volume either way.
func judgeMemory(cfg *config.Config, scope config.UnitScope, res loreResult) memoryVerdict {
	var v memoryVerdict
	for _, s := range res.Spaces {
		if s.Err != nil {
			v.Missing = append(v.Missing, s.Space)
		}
	}
	v.Fatal = len(res.Spaces) > 0 && len(v.Missing) == len(res.Spaces)
	return v
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

// ownSpace is a configured space this unit creates for itself, with the
// display name to create it under.
type ownSpace struct {
	ID   domain.SpaceID
	Name string
}

// ownSpaces lists the spaces this unit makes rather than waits for.
//
// It is empty for everything except an isolated pod, and that asymmetry is the
// whole rule. A simple-mode node shares one store with the wizard that made
// its spaces, so a space missing there means the store is wrong — and a node
// that answered "missing" by creating it would serve happily on somebody
// else's home with an empty memory, which is the silent amnesia checkLore
// exists to refuse. A pod's store is on a volume the wizard could never reach,
// so a space missing there on first boot is not a fault at all; it is what a
// fresh volume looks like.
//
// Which spaces are its own follows from who else needs them. A member's
// private space lives in exactly one store and is shared with nobody, so the
// pod that serves that member is the only place it can be made. The
// household's shared space is made once, by the group's pod, and reaches the
// members through the link handshake (internal/link) — so a member's pod does
// not create it, and must not: a second space at that id, with a key of its
// own, is a space that could never converge with the household's, because
// peers intersect blinded ids computed from the key and two keys are two
// blinded ids.
func ownSpaces(cfg *config.Config, scope config.UnitScope) []ownSpace {
	if cfg.Mode != config.ModeIsolated || !isPod(scope) {
		return nil
	}
	if scope.Group {
		if cfg.Household.SharedSpace == "" {
			return nil
		}
		return []ownSpace{{ID: domain.SpaceID(cfg.Household.SharedSpace), Name: cfg.Household.Name}}
	}
	for _, m := range cfg.Members {
		if scope.Serves(m.ID) && m.PrivateSpace != "" {
			return []ownSpace{{ID: domain.SpaceID(m.PrivateSpace), Name: m.Name}}
		}
	}
	return nil
}

// makeOwnSpaces creates this unit's own spaces at the ids kenward.yaml names,
// and does nothing at all when they are already there.
//
// The name is a local label in one pod's own store — lore does not make names
// unique and never compares them — so it is the plainest one available: the
// member's name in the member's pod, the household's in the group's. The id is
// the only thing that has to match, and it is the one the wizard wrote.
func makeOwnSpaces(ctx context.Context, cfg *config.Config, scope config.UnitScope) error {
	spaces := ownSpaces(cfg, scope)
	if len(spaces) == 0 {
		return nil
	}
	client, err := memory.NewClient(memory.Config{})
	if err != nil {
		return err
	}
	defer client.Close()
	for _, sp := range spaces {
		if _, err := client.CreateSpaceWithID(ctx, string(sp.ID), sp.Name); err != nil {
			return err
		}
	}
	return nil
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
