package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/dashboard"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/version"
)

// checkStatus is how one check came out.
type checkStatus string

const (
	// statusOK is a check that passed.
	statusOK checkStatus = "ok"
	// statusWarn is something worth knowing that is not a failure.
	statusWarn checkStatus = "warn"
	// statusFail is a check that failed.
	statusFail checkStatus = "fail"
)

func (s checkStatus) symbol() string {
	switch s {
	case statusOK:
		return "✓"
	case statusWarn:
		return "!"
	default:
		return "✗"
	}
}

// check is one line of doctor's report, with any detail that belongs under it.
type check struct {
	Status checkStatus `json:"status"`
	Text   string      `json:"text"`
	Detail []string    `json:"detail,omitempty"`
}

// doctorReport is everything `kenward doctor` found. It is assembled first and
// rendered second so that the text and the --json forms cannot disagree, and so that
// the rendering can be golden-tested without any of the world being present.
type doctorReport struct {
	Version    string `json:"version"`
	Mode       string `json:"mode"`
	ConfigPath string `json:"config_path"`
	// Unit names the single unit this report is about, empty for a household-wide
	// one. It is the difference between "no bot token for jordan" being a fault and
	// being the mode working, so it is on the report rather than left to be inferred.
	Unit string `json:"unit,omitempty"`

	Configuration []check `json:"configuration"`
	// Access is where the admin dashboard is listening, and how far that reaches. It
	// is a section of its own rather than a line under Configuration because it is
	// the one fact on this report that is about who can reach this machine rather
	// than about what this machine can reach — and because "a port is open" belongs
	// where somebody scanning the output will see it.
	Access    []check          `json:"access"`
	Memory    []check          `json:"memory"`
	Sessions  []check          `json:"sessions"`
	Transport []check          `json:"transport"`
	Endpoints []endpointReport `json:"endpoints"`

	// Statement is internal/privacy's statement for this mode, verbatim. It is not
	// composed here and must never be: two copies of this claim is how one of them
	// drifts into promising more than the mode delivers.
	Statement string `json:"privacy_statement"`
	// Exposure is internal/privacy's paragraph about the admin dashboard's listener,
	// verbatim, and it is printed with the statement rather than beside it.
	//
	// "Whoever runs the machine" stopped being the whole truth the moment a port
	// could be open. The statement above cannot know a household's configuration and
	// must not vary with it; this can and does, which is why it is a second string
	// from the same package rather than a paragraph spliced into the first.
	Exposure string `json:"privacy_exposure"`
	// TierNotes are privacy.TierNote's lines, one per conversation.
	TierNotes []string `json:"tier_notes"`

	Exit int `json:"exit_code"`
}

type endpointReport struct {
	Name    string   `json:"name"`
	Tiers   []string `json:"tiers"`
	Reached bool     `json:"reached"`
	Detail  string   `json:"detail,omitempty"`
	Millis  int64    `json:"millis,omitempty"`
}

func cmdDoctor(e *env, args []string) int {
	fs := newFlagSet(e, "doctor", "kenward doctor [--config PATH] [--data-dir PATH] [--member ID | --group] [--json]")
	configPath := fs.String("config", "", "path to kenward.yaml")
	dataDir := fs.String("data-dir", "", "override the data directory")
	member := fs.String("member", "", "isolated mode only: report on this member's pod and nothing else")
	group := fs.Bool("group", false, "isolated mode only: report on the household group's pod and nothing else")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		e.errorf("doctor takes no positional arguments; got %q", fs.Arg(0))
		return exitUsage
	}

	// The same resolution `run` uses, flags then environment, and for the same reason
	// it matters here: the container's HEALTHCHECK is a second process with no
	// arguments of its own, so the only way it can know which unit its container is
	// serving is the environment KENWARD_MEMBER/KENWARD_GROUP that the pod already
	// carries. Without that a member's pod would be reported unhealthy for every
	// sibling token it correctly does not hold, and restarted in a loop.
	sel, err := resolveUnitSelection(*member, *group, e.env())
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}

	rep := runDoctor(e, resolveConfigPath(e, *configPath), resolveDataDir(e, *dataDir), sel)
	if *asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			e.errorf("%v", err)
			return exitFailure
		}
		return rep.Exit
	}
	fmt.Fprint(e.stdout, renderDoctor(rep))
	return rep.Exit
}

// runDoctor runs every check and reports all results.
//
// Nothing stops at the first failure. A household's configuration is edited rarely
// and by hand; surfacing one problem per run turns a five-minute fix into an evening.
// sel is which unit this process is reporting on. The zero selection is the whole
// household, which is what a simple-mode node and an operator at a terminal both are.
func runDoctor(e *env, path, dataDir string, sel unitSelection) doctorReport {
	ctx := e.context()
	scope := sel.scope()
	rep := doctorReport{
		Version:    version.Short(),
		ConfigPath: path,
		Mode:       "unknown",
		Exit:       exitOK,
	}
	if sel.single() {
		rep.Unit = sel.describe()
	}

	secrets := e.secrets()
	cfg, cfgErr := loadConfigForUnit(path, dataDir, secrets, scope)
	var ve *config.ValidationError
	switch {
	case cfgErr != nil && !errors.As(cfgErr, &ve):
		// The file could not be opened or parsed. There is no configuration to
		// check anything else against, so this is the whole report.
		rep.Configuration = append(rep.Configuration, check{
			Status: statusFail,
			Text:   fmt.Sprintf("%s could not be read", path),
			Detail: []string{cfgErr.Error()},
		})
		rep.Exit = exitUsage
		return rep
	case cfgErr != nil:
		rep.Configuration = append(rep.Configuration, check{
			Status: statusFail,
			Text:   fmt.Sprintf("%s parses, but describes a household that cannot be served", filepath.Base(path)),
			Detail: append([]string{path}, ve.Problems...),
		})
		rep.Exit = exitUsage
	default:
		rep.Configuration = append(rep.Configuration, check{
			Status: statusOK,
			Text:   fmt.Sprintf("%s parses and validates", filepath.Base(path)),
			Detail: []string{path},
		})
	}

	rep.Mode = string(cfg.Mode)

	// Secrets get their own check, listed by the configuration path an operator has
	// to fix rather than by variable name — a household may supply a token as a
	// file or a systemd credential, and "KENWARD_BOT_TOKEN is unset" would be a
	// confusing thing to tell somebody who never intended to use a variable.
	// Paths only: nothing here ever prints a value. Scoped: in a member's pod the
	// secrets that count are that member's, and a sibling's absence is D-007 working.
	if missing := cfg.MissingSecretNamesForUnit(secrets, scope); len(missing) > 0 {
		rep.Configuration = append(rep.Configuration, check{
			Status: statusFail,
			Text:   fmt.Sprintf("%s no readable value", plural(len(missing), "1 secret has", fmt.Sprintf("%d secrets have", len(missing)))),
			Detail: missing,
		})
		rep.Exit = exitUsage
	} else {
		// Worded for the scope it was judged at. "Every secret the configuration
		// names" would be a false claim in a pod, which holds one unit's secrets on
		// purpose and reads none of the others.
		text := "every secret the configuration names can be read"
		if rep.Unit != "" {
			text = "every secret this unit needs can be read"
		}
		rep.Configuration = append(rep.Configuration, check{Status: statusOK, Text: text})
	}

	rep.Access = doctorAccess(e, cfg)
	rep.Memory = doctorMemory(ctx, e, cfg, scope, &rep)
	rep.Sessions = doctorSessions(ctx, e, cfg, scope)
	rep.Transport = doctorTransport(ctx, e, cfg, secrets, scope, &rep)
	rep.Transport = append(rep.Transport, doctorEndpointKeys(cfg, secrets, scope, &rep)...)
	rep.Endpoints = doctorEndpoints(ctx, e, cfg)
	rep.Statement = privacy.Statement(privacyModeFor(cfg.Mode))
	rep.Exposure = privacy.DashboardNote(dashboard.ReachFor(cfg.Dashboard), dashboard.URLFor(cfg.Dashboard), cfg.Dashboard.TLS())
	rep.TierNotes = tierNotes(cfg, scope)
	return rep
}

// doctorAccess reports the dashboard's bind address and exposure, as a first-class line.
//
// It is first-class because it is the only thing in this report that says who can reach
// this machine. Everything else answers "does this node work"; this answers "and who
// else can get at it", which is the question a household running a box full of everyone's
// private memory has the strongest interest in and the least visibility of.
//
// Nothing here exits non-zero. docs/CLI.md names three things doctor fails for and this
// is not one of them, and it must not become one: the container's HEALTHCHECK runs this
// command, and a household that has deliberately put its dashboard on a tailnet is not
// unhealthy. A configuration that is actually unsafe — LAN without TLS, an exposure that
// contradicts its own bind — is refused by config.validateDashboard long before here,
// and shows up in the Configuration section as the parse failure it is.
func doctorAccess(e *env, cfg *config.Config) []check {
	d := cfg.Dashboard
	if !d.Enabled {
		return []check{{
			Status: statusOK,
			Text:   d.DashboardSummary(),
			Detail: []string{"nothing listens; kenward is configured and operated from this machine's own shell"},
		}}
	}

	c := check{Status: statusOK, Text: d.DashboardSummary()}
	switch d.ExposureOrDefault() {
	case config.ExposureLoopback:
		c.Detail = []string{"this machine only: nothing on your network can reach the admin login"}
	case config.ExposureTailnet:
		c.Detail = []string{
			"anyone already on that tailnet or VPN can reach the admin login, and the " +
				"admin password is the only thing between them and every setting in this household",
		}
	case config.ExposureLAN:
		// A warning, and it stays one however correct the configuration is. Every
		// device on a household's wifi can reach this login page, which is a fact
		// worth reading once a month rather than deciding once and forgetting.
		c.Status = statusWarn
		c.Detail = []string{
			"every device on your own network can reach the admin login",
			"the certificate is self-signed; check its fingerprint against the browser's once",
			"a tailnet or VPN is the better way in from another machine",
		}
	}

	dir := cfg.DataDir
	if !dashboard.NewAdminStore(dir).Exists() {
		c2 := check{
			Status: statusWarn,
			Text:   "the admin dashboard has no account yet",
			Detail: []string{"run `kenward setup-token` and open the dashboard; setup happens on loopback"},
		}
		if !d.Loopback() {
			// A listener off this machine with no account on it is a wizard
			// anybody who can reach the port can complete.
			c2.Status = statusFail
			c2.Detail = append(c2.Detail,
				"this listener is not loopback and has no account behind it: anyone who can reach "+
					"it and holds the setup token can configure this household")
		}
		return []check{c, c2}
	}
	if dashboard.NewSetupTokenStore(dir).Outstanding(e.clock()) {
		return []check{c, {
			Status: statusWarn,
			Text:   "a setup token is outstanding and there is already an admin account",
			Detail: []string{"it cannot be used — the setup pages do not exist once an account does — but it should not be there; it is removed on the next start"},
		}}
	}
	return []check{c}
}

// doctorMemory checks that lore answers and that every configured space is there.
//
// lore not answering is a failure: without it there is no retrieval, no capture and
// no enrolment history, and the node cannot do its job.
//
// A space lore does not hold is a failure too, and this check exists to make it one.
// The probe asks through internal/memory, so it resolves the configured value exactly
// the way every turn will — a space id against lore's own listing — and a value that
// does not resolve is a value no read will ever succeed with. kenward never creates a
// lore space, so there is nothing for such a household to wait for; this once said the
// space would appear when the member claimed their invite, which was a green light for
// a node that could not read its own memory.
//
// The common cause is a display name configured where a space id belongs, and it hides
// well: lore's own arguments are lenient enough that writes keep working, so a
// household can put memory away for weeks and find on the first retrieval that nothing
// comes back. That is why this is worth catching at doctor time rather than at the
// first Get. The explanation comes from internal/memory's own error rather than a
// second copy of it written here.
func doctorMemory(ctx context.Context, e *env, cfg *config.Config, scope config.UnitScope, rep *doctorReport) []check {
	res := e.probes.loreProbe()(ctx, cfg, scope)
	if res.Err != nil {
		if rep.Exit == exitOK {
			rep.Exit = exitFailure
		}
		return []check{{
			Status: statusFail,
			Text:   "lore mcp did not respond",
			Detail: []string{
				res.Err.Error(),
				"kenward spawns `lore mcp` (memory.lore_command). Without it there is " +
					"no retrieval, no capture and no enrolment history.",
			},
		}}
	}

	checks := []check{{Status: statusOK, Text: "lore mcp responds"}}
	for _, s := range res.Spaces {
		switch {
		case s.Err == nil:
			checks = append(checks, check{
				Status: statusOK,
				Text:   fmt.Sprintf("space %q reachable", s.Space),
			})
		case errors.Is(s.Err, memory.ErrUnknownSpace):
			// A configuration fault rather than a runtime one: the file names a
			// space this lore store does not hold, and only an edit fixes it.
			if rep.Exit == exitOK {
				rep.Exit = exitUsage
			}
			detail := []string{
				s.Err.Error(),
				"run `lore spaces` and configure the id column: kenward keys spaces on " +
					"ids because lore does not enforce unique display names",
				"a name here fails only on reads — writes keep appearing to work — so " +
					"this is worth fixing before anything is written",
			}
			// In a pod the likelier cause is not a mistyped id at all. Each pod has
			// its own LORE_HOME and therefore its own lore account, so the household's
			// shared space exists in whichever store created it and in no other until
			// that store invites this one into it. Sending an operator to check the id
			// column for that would be sending them to the wrong place.
			if isPod(scope) && string(s.Space) == cfg.Household.SharedSpace {
				detail = append(detail,
					"this pod holds its own lore store, so the household's shared space is "+
						"here only once this store has been invited into it: `lore space "+
						"invite <space> --lan --yes` in the pod that created it, then `lore "+
						"join <code> --yes` in this one (docs/IMPLEMENTATION.md §8)")
			}
			checks = append(checks, check{
				Status: statusFail,
				Text:   fmt.Sprintf("space %q is not a space this lore store holds", s.Space),
				Detail: detail,
			})
		default:
			if rep.Exit == exitOK {
				rep.Exit = exitFailure
			}
			checks = append(checks, check{
				Status: statusFail,
				Text:   fmt.Sprintf("space %q did not answer", s.Space),
				Detail: []string{s.Err.Error()},
			})
		}
	}

	return append(checks, doctorSharedMemory(ctx, e, cfg, scope, sharedSpaceHeld(cfg, res))...)
}

// sharedSpaceHeld reports whether this store actually holds the household's shared
// space, from the probe that just asked.
func sharedSpaceHeld(cfg *config.Config, res loreResult) bool {
	for _, s := range res.Spaces {
		if string(s.Space) == cfg.Household.SharedSpace {
			return s.Err == nil
		}
	}
	return false
}

// isPod reports whether this process is one isolated unit rather than the whole
// household — a member's pod or the group's, as opposed to a simple-mode node or the
// host supervisor. It is the same distinction `run` draws before it starts a sync
// daemon, and it is drawn from the scope rather than from the selection because
// doctorMemory is given the scope.
func isPod(scope config.UnitScope) bool {
	return scope.Group || strings.TrimSpace(scope.Member) != ""
}

// doctorSharedMemory reports whether the household's shared memory is wired and
// moving, which the check above cannot say on its own.
//
// The space check says the space is in this store. That is necessary and it is not
// sufficient: in isolated mode every pod has its own LORE_HOME and therefore its own
// lore account, so the household's shared space is one space held by several accounts
// and something has to carry entries between them. `lore mcp` never does — it reads
// and writes the local store — and until `run` started one, no `lore serve` existed in
// any pod, so a household could pass every check above with a shared space that
// reached nobody. That is the state this section exists to name.
//
// It reports peers and rounds and nothing else. What is in the household's memory is
// not something a health check may show, and this one runs as the image's HEALTHCHECK.
//
// Nothing here fails the report. A pod whose sync daemon is down still serves, still
// answers, and still has that member's private memory, which is the property the mode
// exists for; failing would restart a working pod over a degraded feature. The
// warnings are the point.
func doctorSharedMemory(ctx context.Context, e *env, cfg *config.Config, scope config.UnitScope, sharedHeld bool) []check {
	if !isPod(scope) {
		// A simple-mode node has one lore home holding every space, so there is
		// nothing to converge with and no daemon is run. The host supervisor holds no
		// lore home at all. Both still deserve the standing hint, because a household
		// syncing to a laptop needs the same daemon and nothing else would say so.
		return []check{{
			Status: statusWarn,
			Text:   "`lore mcp` does not sync on its own",
			Detail: []string{"run `lore serve` on the same LORE_HOME if this store should reach another machine"},
		}}
	}
	if cfg.Household.SharedSpace == "" {
		// No shared space is configured, so there is no shared memory to be wired.
		return nil
	}
	if !sharedHeld {
		// The check above already reported the space missing, with the remedy. A
		// second line here saying the daemon is syncing would be true of the daemon
		// and false of the household: there is nothing in this store for it to carry.
		return nil
	}

	res := e.probes.syncProbe()(ctx)
	switch {
	case errors.Is(res.Err, memory.ErrNoSyncDaemon):
		return []check{{
			Status: statusWarn,
			Text:   "shared memory is not syncing: no lore sync daemon on this pod's store",
			Detail: []string{
				res.Err.Error(),
				"this pod's own lore store is " + res.Home,
				"kenward runs `lore serve` for the life of the unit, so this means it " +
					"stopped or never started; the household group's memory is not " +
					"reaching this pod and anything written here is not leaving it",
				"private memory is unaffected: it lives in this store and needs no sync",
			},
		}}
	case res.Err != nil:
		return []check{{
			Status: statusWarn,
			Text:   "could not ask this pod's lore sync daemon how it is doing",
			Detail: []string{res.Err.Error(), "this pod's own lore store is " + res.Home},
		}}
	}

	st := res.Status
	if st.Peers == 0 {
		return []check{{
			Status: statusWarn,
			Text:   "shared memory is syncing with nobody: the daemon is running and has found no other lore instance",
			Detail: []string{
				"a household's pods find each other on the pod network; one pod alone " +
					"is the expected answer while the rest are still starting",
				"if it persists, the pods cannot see one another and the shared space " +
					"is a separate copy in each of them",
			},
		}}
	}

	detail := []string{
		fmt.Sprintf("last sync round %s", lastSyncText(st.LastSync)),
		// Said because it is the limit of what can be observed from here. The daemon
		// reports the instances it has verified, not which of them hold this
		// household's space, and two lore homes exchange a space only when both
		// already hold its key — so a reachable instance is not a reader.
		"the daemon reports instances it can reach, not which of them are in the " +
			"household's space; what is in that memory is not this command's to report",
	}
	// Some peers unreachable is the normal state of a household, not a fault, and
	// this is the difference between a report an operator reads and one they learn to
	// ignore. lore's daemon remembers every instance it has ever discovered and never
	// forgets one, so each rolling update leaves the previous pods' addresses behind
	// as rows that will never answer again — found by rolling this household on real
	// podman, where every pod's report carried a column of "no route to host" for
	// containers that had been correctly replaced. What actually says shared memory
	// has stopped is every peer failing at once.
	status := statusOK
	if len(st.Errors) > 0 && len(st.Errors) >= st.Peers {
		status = statusWarn
		detail = append(detail, "no peer answered the last round; shared memory is not moving")
	}
	detail = append(detail, firstFew(st.Errors, 3)...)
	return []check{{
		Status: status,
		Text: fmt.Sprintf("shared memory is syncing: this store holds the household's space and reaches %s",
			plural(st.Peers, "1 other lore instance", fmt.Sprintf("%d other lore instances", st.Peers))),
		Detail: detail,
	}}
}

// firstFew returns at most n of in, with a final line counting what was left out.
// The list it trims is the per-peer sync errors, which grow by one per replaced pod
// for as long as the household lives — a report nobody can read is a report nobody
// reads.
func firstFew(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return append(append([]string(nil), in[:n]...),
		fmt.Sprintf("and %d more, most likely pods this household has already replaced", len(in)-n))
}

// lastSyncText renders when the last sync round finished, for an operator reading a
// report that may be minutes old.
func lastSyncText(t time.Time) string {
	if t.IsZero() {
		return "has not run yet"
	}
	return "finished " + t.UTC().Format(time.RFC3339)
}

// doctorSessions reports key custody: who has a wrapped key, and who is enrolled
// without one.
//
// An operator needs to be able to see this without sending a message and waiting for
// a non-answer. A member who is enrolled but has no wrapped key gets the locked
// notice for every private message they ever send, while the household group works
// perfectly — so it is worth saying out loud, at length, on the one screen somebody
// looks at when something is wrong.
//
// It is a warning rather than a failure. docs/CLI.md names three things doctor exits
// non-zero for, and this is not one of them; the container's HEALTHCHECK runs this
// command, and a household mid-enrolment is not unhealthy. Whether a key is unwrapped
// *right now* is not knowable from here at all: that lives in the running node's
// memory and this is a different process. Saying so is better than implying otherwise.
func doctorSessions(ctx context.Context, e *env, cfg *config.Config, scope config.UnitScope) []check {
	res := e.probes.sessionsProbe()(ctx, cfg)
	if res.Err != nil {
		return []check{{
			Status: statusWarn,
			Text:   "could not read the wrapped-key store",
			Detail: []string{res.Err.Error()},
		}}
	}

	var checks []check
	if res.Custody != "" {
		checks = append(checks, check{
			Status: statusOK,
			Text:   res.Custody,
			Detail: []string{
				"whether a key is unwrapped right now is not visible from here: unwrapped " +
					"keys live in the running node's memory and this is a different process",
			},
		})
	}
	checks = append(checks, check{
		Status: statusOK,
		Text: fmt.Sprintf("%s a wrapped key on disk",
			plural(len(res.Provisioned), "1 member has", fmt.Sprintf("%d members have", len(res.Provisioned)))),
	})
	checks = append(checks, idleExpiryCheck(cfg.Session.IdleTimeout.Duration()))
	for _, id := range res.MissingKey {
		// A member's pod holds one member's key and no other, so a sibling with no
		// key here is not a finding — it is the mode. Reporting it would tell an
		// operator to go and fix something that must never be fixed in this pod.
		if !scope.Serves(string(id)) {
			continue
		}
		checks = append(checks, check{
			Status: statusWarn,
			Text:   fmt.Sprintf("%s is enrolled but has no wrapped key", id),
			Detail: []string{
				"every private message to them is refused as locked until the node is " +
					"started once with a passphrase; the household group is unaffected, " +
					"which is why this is easy to miss",
			},
		})
	}
	return checks
}

// idleExpiryCheck says which way this household has session.idle_timeout set.
//
// The privacy statement names the knob and is true either way, deliberately: it cannot
// know a household's configuration. doctor can, and this is the screen somebody reads to
// find out which case is theirs.
//
// A configured timeout is a warning rather than an ok line because there is no in-band
// way back. A passphrase never travels over Telegram (D-019), so a key zeroed after a
// quiet afternoon is only replaced by somebody at the machine — until then the assistant
// simply stops answering, which reads as broken rather than locked. It is still not a
// failure: it is a household's own knowing choice, and doctor exits non-zero for three
// things and this is not one of them.
func idleExpiryCheck(d time.Duration) check {
	if d <= 0 {
		return check{
			Status: statusOK,
			Text:   "unlocked keys do not expire on idle (session.idle_timeout is off)",
		}
	}
	return check{
		Status: statusWarn,
		Text:   fmt.Sprintf("unlocked keys expire after %s of quiet (session.idle_timeout)", d),
		Detail: []string{
			"after that long without a message the member's key is zeroed and their " +
				"assistant stops answering, which looks like a broken assistant rather " +
				"than a locked one",
			"there is no way back from a chat: a passphrase never travels over Telegram, " +
				"so someone has to start the process again at the machine",
		},
	}
}

// doctorTransport authorises every bot token this configuration names.
//
// A token Telegram refuses is a failure: the node has no way to receive or send
// anything, which is not a household machine being asleep but a deployment that
// cannot work.
//
// Every bot token *this unit* names, that is. In a member's pod the household's own
// token and every sibling's are deliberately absent (D-007), and asking Telegram about
// a token this container was correctly never given would fail the pod's health check
// for doing the one thing isolated mode is for.
func doctorTransport(ctx context.Context, e *env, cfg *config.Config, secrets *config.Secrets, scope config.UnitScope, rep *doctorReport) []check {
	probe := e.probes.telegramProbe()
	var checks []check

	// add resolves one bot token and authorises with it. Where the value came from
	// is reported and the value itself never is: Secret.Source() is written for
	// exactly this, and "token from systemd credential bot_token" versus "token
	// from environment variable KENWARD_BOT_TOKEN" is the line that settles an
	// argument about why a node will not start.
	add := func(label string, ref config.SecretRef, resolve func() (config.Secret, error)) {
		sec, err := resolve()
		switch {
		case err != nil:
			// Stated but unreadable: a token file at 0644, a credential that is
			// not there, or two sources named at once. The refusal carries the
			// mode and the path, and it is a finding rather than a crash.
			if rep.Exit == exitOK {
				rep.Exit = exitFailure
			}
			checks = append(checks, check{
				Status: statusFail,
				Text:   fmt.Sprintf("%s: the bot token could not be read", label),
				Detail: []string{err.Error()},
			})
			return
		case !sec.IsSet():
			if rep.Exit == exitOK {
				rep.Exit = exitFailure
			}
			checks = append(checks, check{
				Status: statusFail,
				Text:   fmt.Sprintf("%s: no bot token is configured", label),
				Detail: []string{fmt.Sprintf("set %s_file or %s_env, or supply the %s systemd credential",
					ref.Where, ref.Where, ref.Credential)},
			})
			return
		}

		res := probe(ctx, sec.Value())
		if res.Err != nil {
			if rep.Exit == exitOK {
				rep.Exit = exitFailure
			}
			checks = append(checks, check{
				Status: statusFail,
				Text:   fmt.Sprintf("%s: Telegram did not authorise the token from %s", label, sec.Source()),
				Detail: []string{res.Err.Error()},
			})
			return
		}
		name := res.Username
		if name == "" {
			name = "an unnamed bot"
		} else {
			name = "@" + name
		}
		checks = append(checks, check{
			Status: statusOK,
			Text:   fmt.Sprintf("%s: Telegram authorises as %s", label, name),
			Detail: []string{"token from " + sec.Source()},
		})
	}

	if cfg.Mode == config.ModeIsolated {
		for _, m := range cfg.Members {
			if !scope.Serves(m.ID) {
				continue
			}
			add(m.Name, m.BotTokenRef(), func() (config.Secret, error) { return m.BotToken(secrets) })
		}
		// The group's bot, for whoever runs the group conversation. A member's pod
		// does not and never holds that token.
		if scope.ServesGroup() {
			add("household group", cfg.BotTokenRef(), func() (config.Secret, error) { return cfg.BotToken(secrets) })
		}
		return checks
	}
	add("household", cfg.BotTokenRef(), func() (config.Secret, error) { return cfg.BotToken(secrets) })
	return checks
}

// doctorEndpointKeys reports on the API keys the endpoints name.
//
// An endpoint on the household's own network usually needs none, and absence is
// therefore not a fault. What is worth surfacing is a key that was stated and cannot
// be read — a file with the wrong mode, a credential that is not there — because that
// endpoint will refuse every request while looking configured, and the tier chain
// will fall through it silently.
//
// The endpoints this unit's own chain can reach, and no others. A pod is given only
// those keys on purpose — a key in an environment is a key that can be used whatever
// routing intended — so an absent key for an endpoint this unit may not route to is the
// configuration working.
func doctorEndpointKeys(cfg *config.Config, secrets *config.Secrets, scope config.UnitScope, rep *doctorReport) []check {
	var checks []check
	for _, ep := range cfg.EndpointsForUnit(scope) {
		sec, err := ep.APIKey(secrets)
		switch {
		case err != nil:
			if rep.Exit == exitOK {
				rep.Exit = exitUsage
			}
			checks = append(checks, check{
				Status: statusFail,
				Text:   fmt.Sprintf("%s: the API key could not be read", ep.Name),
				Detail: []string{err.Error()},
			})
		case sec.IsSet():
			checks = append(checks, check{
				Status: statusOK,
				Text:   fmt.Sprintf("%s: key from %s", ep.Name, sec.Source()),
			})
		}
	}
	return checks
}

// doctorEndpoints probes every endpoint and never fails over any of them.
//
// This is load-bearing rather than tidy. The container's HEALTHCHECK runs `doctor`,
// so an endpoint that does not answer must not change the exit code: a household's
// GPU box is switched off most of the time, and treating that as unhealthy would put
// a perfectly good household into a restart loop.
func doctorEndpoints(ctx context.Context, e *env, cfg *config.Config) []endpointReport {
	probe := e.probes.endpointProbe()
	var out []endpointReport
	for _, ep := range cfg.RoutingEndpoints() {
		res := probe(ctx, ep)
		out = append(out, endpointReport{
			Name:    res.Name,
			Tiers:   res.Tiers,
			Reached: res.Reached,
			Detail:  res.Detail,
			Millis:  res.Elapsed.Milliseconds(),
		})
	}
	return out
}

// tierNotes renders, for each conversation this process serves, what its tier chain
// means in practice. Every line comes from internal/privacy; the only decision made here
// is which tiers count as local, which privacy.TierNote asks its caller for because that
// is configuration rather than something the privacy package can know.
//
// Scoped, because "where each conversation may go" is a claim about the conversations
// this process actually holds. A member's pod listing every other member's chain would
// be answering for pods it has no part in.
func tierNotes(cfg *config.Config, scope config.UnitScope) []string {
	local := localTiers(cfg)
	var notes []string
	for _, m := range cfg.DomainMembers() {
		if !scope.Serves(string(m.ID)) {
			continue
		}
		notes = append(notes, privacy.MemberNote(m, staysHome(local, m.Tiers)))
	}
	label := cfg.Household.Name
	if label == "" {
		label = cfg.Household.SharedSpace
	}
	if label != "" && scope.ServesGroup() {
		notes = append(notes, privacy.TierNote(label, cfg.Household.Tiers, staysHome(local, cfg.Household.Tiers)))
	}
	return notes
}

// renderDoctor draws the report.
func renderDoctor(r doctorReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kenward %s — mode: %s", r.Version, r.Mode)
	if r.Unit != "" {
		// Named on the first line so that nobody reads a pod's report as the
		// household's. Half the checks below are about this unit alone.
		fmt.Fprintf(&b, " — this pod runs only %s", r.Unit)
	}
	b.WriteString("\n")

	writeSection(&b, "Configuration", r.Configuration)
	writeSection(&b, "Access", r.Access)
	writeSection(&b, "Memory", r.Memory)
	writeSection(&b, "Sessions", r.Sessions)
	writeSection(&b, "Transport", r.Transport)

	if len(r.Endpoints) > 0 {
		b.WriteString("\nEndpoints\n")
		nameWidth, tierWidth := 0, 0
		for _, ep := range r.Endpoints {
			nameWidth = max(nameWidth, len([]rune(ep.Name)))
			tierWidth = max(tierWidth, len([]rune(strings.Join(ep.Tiers, ","))))
		}
		anyDown := false
		for _, ep := range r.Endpoints {
			symbol, detail := statusOK.symbol(), fmt.Sprintf("answered in %dms", ep.Millis)
			if !ep.Reached {
				anyDown = true
				symbol, detail = statusFail.symbol(), ep.Detail
			}
			fmt.Fprintf(&b, "  %s %s  %s  %s\n",
				symbol,
				padRunes(ep.Name, nameWidth),
				padRunes(strings.Join(ep.Tiers, ","), tierWidth),
				detail)
		}
		if anyDown {
			b.WriteString("\n  An endpoint that does not answer is reported here, not failed. Household\n")
			b.WriteString("  machines are legitimately switched off, and a conversation whose chain names\n")
			b.WriteString("  only unreachable machines is refused rather than sent somewhere else.\n")
		}
	}

	if r.Statement != "" {
		b.WriteString("\nPrivacy\n\n")
		// Verbatim, unindented. internal/setup tells every operator that `kenward
		// doctor` prints this same statement in the same words, and that promise is
		// only worth anything if the two strings are identical.
		b.WriteString(r.Statement)
		b.WriteString("\n")
		if r.Exposure != "" {
			b.WriteString("\n")
			b.WriteString(r.Exposure)
			b.WriteString("\n")
		}
	}
	if len(r.TierNotes) > 0 {
		b.WriteString("\nWhere each conversation may go\n\n")
		for _, note := range r.TierNotes {
			fmt.Fprintf(&b, "  %s\n", note)
		}
	}
	return b.String()
}

func writeSection(b *strings.Builder, name string, checks []check) {
	if len(checks) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", name)
	for _, c := range checks {
		for i, line := range wrap(c.Text, reportWidth-4) {
			if i == 0 {
				fmt.Fprintf(b, "  %s %s\n", c.Status.symbol(), line)
				continue
			}
			fmt.Fprintf(b, "    %s\n", line)
		}
		for _, d := range c.Detail {
			for _, line := range wrap(d, reportWidth-6) {
				fmt.Fprintf(b, "      %s\n", line)
			}
		}
	}
}

// reportWidth is how wide doctor's own prose is allowed to be. It is narrower than
// eighty so that the indented forms still fit, and it applies only to text this
// package composes — never to the privacy statement, which arrives already wrapped
// and is printed exactly as internal/privacy wrote it.
const reportWidth = 80

// wrap breaks a line on spaces. session.CustodyReport in particular is one long
// sentence, and a terminal's own wrapping would break the alignment of everything
// under it.
func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len([]rune(line))+1+len([]rune(w)) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	return append(lines, line)
}

// padRunes right-pads counted in runes rather than bytes, so a household with a
// María in it comes out aligned with everybody else.
func padRunes(s string, width int) string {
	n := width - len([]rune(s))
	if n <= 0 {
		return s
	}
	return s + strings.Repeat(" ", n)
}
