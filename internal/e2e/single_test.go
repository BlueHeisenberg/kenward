package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/BlueHeisenberg/keel/vault"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// -----------------------------------------------------------------------------
// one pod of an isolated household
// -----------------------------------------------------------------------------

// The isolated household's members. Isolated mode gives every member their own
// bot, so every member has their own token variable — and the pod that serves one
// of them must never touch another's. samMemberID is declared, invited and has not
// claimed: the state D-023 says a member is in while their pod already exists.
const (
	davidBotTokenEnv = "KENWARD_BOT_TOKEN_DAVID"
	meiBotTokenEnv   = "KENWARD_BOT_TOKEN_MEI"
	samBotTokenEnv   = "KENWARD_BOT_TOKEN_SAM"
	patBotTokenEnv   = "KENWARD_BOT_TOKEN_PAT"

	samMemberID         = domain.MemberID("sam")
	samSpace            = domain.SpaceID("sam-private")
	samTelegramID int64 = 1003
	samChatID     int64 = 5003

	// Pat is a second member who has not claimed, so a code for one of them can
	// be presented to the other's bot.
	patMemberID         = domain.MemberID("pat")
	patSpace            = domain.SpaceID("pat-private")
	patTelegramID int64 = 1004
	patChatID     int64 = 5004
)

// podEnvironment is the environment an isolated household's configuration is
// loaded against: every member's token variable, because loading validates the
// whole household's file even inside one member's pod.
func podEnvironment() map[string]string {
	return map[string]string{
		botTokenEnv:      "123456:household-token",
		davidBotTokenEnv: "123456:david-token",
		meiBotTokenEnv:   "123456:mei-token",
		samBotTokenEnv:   "123456:sam-token",
		patBotTokenEnv:   "123456:pat-token",
	}
}

// isolatedConfigYAML renders the household in isolated mode. It is the same
// household the simple-mode harness describes, with the two things the mode adds:
// a per-member bot token variable and a member who has not claimed yet.
func isolatedConfigYAML(dataDir, localURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode: isolated\n")
	fmt.Fprintf(&b, "data_dir: '%s'\n", dataDir)
	fmt.Fprintf(&b, "household:\n")
	fmt.Fprintf(&b, "  name: Ashfield\n")
	fmt.Fprintf(&b, "  shared_space: %s\n", sharedSpace)
	fmt.Fprintf(&b, "  group_chat_id: %d\n", groupChatID)
	fmt.Fprintf(&b, "  tiers: [local]\n")
	fmt.Fprintf(&b, "telegram:\n")
	fmt.Fprintf(&b, "  bot_token_env: %s\n", botTokenEnv)
	fmt.Fprintf(&b, "members:\n")
	fmt.Fprintf(&b, "  - id: david\n    name: David\n    telegram_id: %d\n    private_space: %s\n    tiers: [local]\n    bot_token_env: %s\n",
		davidTelegramID, davidSpace, davidBotTokenEnv)
	fmt.Fprintf(&b, "  - id: mei\n    name: Mei\n    telegram_id: %d\n    private_space: %s\n    tiers: [local]\n    bot_token_env: %s\n",
		meiTelegramID, meiSpace, meiBotTokenEnv)
	// No telegram_id: each has been given a bot and a code and has not used it.
	fmt.Fprintf(&b, "  - id: %s\n    name: Sam\n    private_space: %s\n    tiers: [local]\n    bot_token_env: %s\n",
		samMemberID, samSpace, samBotTokenEnv)
	fmt.Fprintf(&b, "  - id: %s\n    name: Pat\n    private_space: %s\n    tiers: [local]\n    bot_token_env: %s\n",
		patMemberID, patSpace, patBotTokenEnv)
	fmt.Fprintf(&b, "endpoints:\n")
	fmt.Fprintf(&b, "  - name: attic\n    base_url: '%s'\n    model: test-model\n    tags: [local]\n    timeout: 30s\n", localURL)
	fmt.Fprintf(&b, "memory:\n  lore_command: [lore, mcp]\n  search_limit: 8\n")
	fmt.Fprintf(&b, "session:\n  idle_timeout: 30m\n")
	fmt.Fprintf(&b, "capture:\n  max_proposals_per_turn: 1\n")
	fmt.Fprintf(&b, "update:\n  channel: stable\n  check_interval: 6h\n")
	return b.String()
}

// loadIsolatedConfig writes and loads a real isolated-mode kenward.yaml, returning
// it with the endpoint it points at and the data directory it was given.
func loadIsolatedConfig(t *testing.T, lookupEnv config.LookupEnvFunc) (*config.Config, *fakeProvider, string) {
	t.Helper()
	dir := t.TempDir()
	local := newFakeProvider(t, "attic")
	return loadConfigYAML(t, dir, isolatedConfigYAML(dir, local.baseURL()), lookupEnv), local, dir
}

// loadConfigYAML writes one configuration document into dir and loads it for real.
func loadConfigYAML(t *testing.T, dir, doc string, lookupEnv config.LookupEnvFunc) *config.Config {
	t.Helper()
	path := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("writing configuration: %v", err)
	}
	cfg, err := config.LoadWithEnv(path, lookupEnv)
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	return cfg
}

// podOptions selects the one unit a pod runs.
type podOptions struct {
	member domain.MemberID
	group  bool
}

// newPod builds one pod of the isolated household: a real supervisor.Single over a
// real isolated-mode configuration, with the same three edges faked as everywhere
// else in this package. It returns the package's harness so that every helper —
// start, stop, sentTo, waitForReply — means the same thing here as in simple mode.
func newPod(t *testing.T, opts podOptions) *harness {
	t.Helper()
	lookupEnv := fakeEnv(podEnvironment())
	cfg, local, dir := loadIsolatedConfig(t, lookupEnv)

	// A pod's session manager is in isolated custody and holds one key: the member
	// this pod serves. The group's pod holds none — a group turn has no session,
	// because the household chat is nobody's private conversation.
	sessions, err := session.NewManager(session.ModeIsolated, session.NewMemStore(),
		session.WithKDFParams(vault.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}))
	if err != nil {
		t.Fatalf("building session manager: %v", err)
	}
	t.Cleanup(sessions.Close)
	if !opts.group {
		ctx := context.Background()
		if err := sessions.Provision(ctx, opts.member, passphrase); err != nil {
			t.Fatalf("provisioning %s: %v", opts.member, err)
		}
		if err := sessions.Unlock(ctx, opts.member, passphrase); err != nil {
			t.Fatalf("unlocking %s: %v", opts.member, err)
		}
	}

	tr := transport.NewFake()
	mem := newFakeMemory()

	// Router left nil on purpose, as in simple mode: the supervisor builds the real
	// pool over the real HTTP completer.
	sup, err := supervisor.NewSingle(cfg, supervisor.SingleOptions{
		Member:    opts.member,
		Group:     opts.group,
		Transport: tr,
		Memory:    mem,
		Sessions:  sessions,
		LookupEnv: lookupEnv,
	})
	if err != nil {
		t.Fatalf("building single-unit supervisor: %v", err)
	}

	h := &harness{
		t:        t,
		dir:      dir,
		cfg:      cfg,
		tr:       tr,
		mem:      mem,
		local:    local,
		sessions: sessions,
		sup:      sup,
		startErr: make(chan error, 1),
	}
	t.Cleanup(func() {
		h.stop()
		_ = tr.Close()
	})
	return h
}

// recordingEnv is a config.LookupEnvFunc that remembers every name it was asked
// for. It is how "this process never even looked at that credential" becomes an
// assertion rather than a claim.
type recordingEnv struct {
	vars map[string]string
	mu   sync.Mutex
	seen []string
}

func newRecordingEnv(vars map[string]string) *recordingEnv {
	return &recordingEnv{vars: vars}
}

func (e *recordingEnv) lookup(name string) (string, bool) {
	e.mu.Lock()
	e.seen = append(e.seen, name)
	e.mu.Unlock()
	v, ok := e.vars[name]
	return v, ok
}

func (e *recordingEnv) asked(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range e.seen {
		if n == name {
			return true
		}
	}
	return false
}

func (e *recordingEnv) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]string(nil), e.seen...)
	sort.Strings(out)
	return out
}

// recordingFS is a config.SecretFS that remembers every path it was asked to read
// and returns a single space for all of them. It is the filesystem half of the same
// observation: a secret may reach kenward as a file or as a systemd credential, and
// watching only the environment would leave both of those doors unwatched.
type recordingFS struct {
	mu    sync.Mutex
	paths []string
}

func (f *recordingFS) ReadSecretFile(path string) ([]byte, fs.FileMode, error) {
	f.mu.Lock()
	f.paths = append(f.paths, path)
	f.mu.Unlock()
	return []byte(" "), 0o600, nil
}

func (f *recordingFS) read(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if p == path {
			return true
		}
	}
	return false
}

func (f *recordingFS) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.paths...)
	sort.Strings(out)
	return out
}

var _ config.SecretFS = (*recordingFS)(nil)

// -----------------------------------------------------------------------------
// the scenarios
// -----------------------------------------------------------------------------

// TestPodForOneMemberServesThatMemberNormally checks the mode-blindness the design
// rests on: a unit inside a pod is the same unit simple mode runs as a goroutine,
// so a member's whole turn — scope, retrieval over their two spaces, the prompt,
// the pool, the reply — has to work here exactly as it does there.
func TestPodForOneMemberServesThatMemberNormally(t *testing.T) {
	h := newPod(t, podOptions{member: "david"})
	h.mem.seed(davidSpace, entry("p1", "Bin day", "Recycling goes out on Tuesday night."))
	h.mem.seed(sharedSpace, entry("s1", "Side gate", "The side gate code is 4417."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "Tuesday night.", FinishReason: "stop"}
	})
	h.start()

	h.tr.InjectText(davidChatID, davidTelegramID, "when do the bins go out?", false)
	sent := h.waitForReply(davidChatID, 1)

	if sent[0].Text != "Tuesday night." {
		t.Errorf("reply = %q, want the model's text", sent[0].Text)
	}
	health, err := h.sup.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(health) != 1 || health[0].Member != "david" || health[0].Group {
		t.Fatalf("health reported %+v, want exactly david's unit; a pod runs one unit and no more", health)
	}
	searched := h.mem.searchedSpaces()
	if len(searched) != 2 || !containsSpace(searched, davidSpace) || !containsSpace(searched, sharedSpace) {
		t.Errorf("searched %v, want exactly david's private space and the household's", searched)
	}
	if !strings.Contains(h.local.last(t).System(), "Recycling goes out on Tuesday night.") {
		t.Error("system prompt does not carry what retrieval found")
	}
}

// TestPodForOneMemberDoesNotServeAnotherMember is the property the mode exists for.
// Mei is an enrolled member of the same household and her pod is elsewhere; this
// process must answer her nothing at all. A pod that served a second member would
// put two members' conversations in one address space, which is precisely the thing
// simple mode is honest about and isolated mode claims not to do.
func TestPodForOneMemberDoesNotServeAnotherMember(t *testing.T) {
	h := newPod(t, podOptions{member: "david"})
	h.mem.seed(meiSpace, entry("m1", "Mei's cardiologist", "Appointment on the 3rd."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "Noted.", FinishReason: "stop"}
	})
	h.start()

	h.tr.InjectText(meiChatID, meiTelegramID, "when is my appointment?", false)
	// David's message behind hers on the same stream, so once his reply is out hers
	// has been dispatched and this is an assertion about absence, not about timing.
	h.tr.InjectText(davidChatID, davidTelegramID, "morning", false)
	h.waitForReply(davidChatID, 1)
	// Draining as well: a message still queued when the drain begins is dropped
	// rather than answered, so after this nothing more can be sent to anyone.
	if err := h.stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := h.sentTo(meiChatID); len(got) != 0 {
		t.Errorf("mei received %d message(s) from david's pod: %+v; her pod is a different process", len(got), got)
	}
	if n := h.local.count(); n != 1 {
		t.Errorf("provider saw %d requests, want 1 (david's); another member's words must never reach this pod's model", n)
	}
	if containsSpace(h.mem.touchedSpaces(), meiSpace) {
		t.Errorf("david's pod reached %s; one pod must never touch another member's space", meiSpace)
	}
}

// TestGroupPodServesTheGroupChatAndSearchesOnlyTheSharedSpace covers the household's
// own pod. The check is on which spaces were reached rather than on what was said: a
// group turn that searched a private space and happened to find nothing has already
// broken the invariant.
func TestGroupPodServesTheGroupChatAndSearchesOnlyTheSharedSpace(t *testing.T) {
	h := newPod(t, podOptions{group: true})
	h.mem.seed(sharedSpace, entry("s1", "Side gate", "The side gate code is 4417."))
	h.mem.seed(davidSpace, entry("p1", "David's PIN", "The card PIN is 9931."))
	h.mem.seed(meiSpace, entry("m1", "Mei's cardiologist", "Appointment on the 3rd."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "The side gate code is 4417.", FinishReason: "stop"}
	})
	h.start()

	h.tr.InjectText(groupChatID, davidTelegramID, "what's the gate code?", true)
	sent := h.waitForReply(groupChatID, 1)

	if sent[0].Text != "The side gate code is 4417." {
		t.Errorf("reply = %q, want the model's text", sent[0].Text)
	}
	health, err := h.sup.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(health) != 1 || !health[0].Group {
		t.Fatalf("health reported %+v, want exactly the group's unit", health)
	}
	// Every call of any kind: a write into a private space from the group pod would
	// be as bad as a read out of one.
	for _, sp := range h.mem.touchedSpaces() {
		if sp != sharedSpace {
			t.Errorf("the group pod touched %s; it may only ever reach %s", sp, sharedSpace)
		}
	}
	system := h.local.last(t).System()
	if strings.Contains(system, "9931") || strings.Contains(system, "Appointment on the 3rd.") {
		t.Error("the group pod's prompt carries a private entry")
	}
}

// TestPodForAnUnenrolledMemberIsRefusedAtConstruction covers the failure that would
// otherwise be invisible. A pod that started cleanly for a member who has not
// claimed would serve nobody while reporting itself healthy, and the member would
// simply never be answered — with the supervisor, the operator and the health check
// all agreeing that everything was fine.
func TestPodForAnUnenrolledMemberIsRefusedAtConstruction(t *testing.T) {
	lookupEnv := fakeEnv(podEnvironment())
	cfg, _, _ := loadIsolatedConfig(t, lookupEnv)
	tr := transport.NewFake()
	t.Cleanup(func() { _ = tr.Close() })

	base := supervisor.SingleOptions{
		Transport: tr,
		Memory:    newFakeMemory(),
		LookupEnv: lookupEnv,
	}

	t.Run("declared but not claimed", func(t *testing.T) {
		opts := base
		opts.Member = samMemberID
		sup, err := supervisor.NewSingle(cfg, opts)
		if !errors.Is(err, supervisor.ErrNotEnrolled) {
			t.Fatalf("NewSingle for an unenrolled member = %v, want %v", err, supervisor.ErrNotEnrolled)
		}
		if sup != nil {
			t.Error("NewSingle returned a supervisor as well as an error")
		}
	})

	t.Run("not in the household at all", func(t *testing.T) {
		opts := base
		opts.Member = "nobody"
		if _, err := supervisor.NewSingle(cfg, opts); err == nil {
			t.Fatal("NewSingle for a member the configuration does not declare returned no error")
		}
	})

	t.Run("neither member nor group", func(t *testing.T) {
		if _, err := supervisor.NewSingle(cfg, base); err == nil {
			t.Fatal("NewSingle selecting no unit returned no error")
		}
	})
}

// TestPodReadsOnlyItsOwnUnitsBotToken is the credential-minimality assertion. A pod
// holding a second member's bot token could read and write that member's private
// conversation, which is the whole of what isolated mode sells; and a credential a
// process never reads is one that a compromise of that process cannot disclose.
//
// A token may arrive by three doors — a 0600 file, an environment variable, or a
// systemd credential under $CREDENTIALS_DIRECTORY — and config.Secrets opens all
// three. The household below therefore puts one unit behind each door, and both the
// environment and the filesystem the supervisor resolves through are recorded, so
// "never read" means never by any route rather than merely never through the
// environment.
func TestPodReadsOnlyItsOwnUnitsBotToken(t *testing.T) {
	dir := t.TempDir()
	local := newFakeProvider(t, "attic")
	credsDir := filepath.Join(dir, "credentials")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatalf("making the credentials directory: %v", err)
	}
	meiTokenPath := filepath.Join(dir, "mei-bot-token")
	samCredential := config.MemberBotTokenCredential(string(samMemberID))
	if samCredential == "" {
		t.Fatalf("member id %q has no systemd credential name", samMemberID)
	}
	samCredentialPath := filepath.Join(credsDir, samCredential)
	for _, f := range []struct{ path, content string }{
		{meiTokenPath, "123456:mei-token\n"},
		{samCredentialPath, "123456:sam-token\n"},
	} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", f.path, err)
		}
	}

	// Loading goes through the real filesystem and a plain environment: config
	// validates the whole household's file and so reads every member's token by
	// design, whichever door it comes through. What is under test is what the
	// supervisor reads afterwards.
	loadEnv := podEnvironment()
	loadEnv[config.EnvCredentialsDirectory] = credsDir
	cfg := loadConfigYAML(t, dir,
		threeDoorConfigYAML(dir, local.baseURL(), meiTokenPath),
		fakeEnv(loadEnv))

	// Every door the supervisor opens hands back a single space: a stated source
	// that really was read, and not a usable token. Construction therefore gets
	// exactly as far as resolving one credential and stops, which is what lets this
	// test watch the real path without a network round trip to Telegram.
	newRecorders := func() (*recordingEnv, *recordingFS, *config.Secrets) {
		blank := podEnvironment()
		for _, name := range []string{botTokenEnv, davidBotTokenEnv, meiBotTokenEnv, samBotTokenEnv} {
			blank[name] = " "
		}
		blank[config.EnvCredentialsDirectory] = credsDir
		env := newRecordingEnv(blank)
		rfs := &recordingFS{}
		return env, rfs, config.NewSecrets(config.SecretOptions{
			LookupEnv:      env.lookup,
			FS:             rfs,
			CredentialsDir: credsDir,
			FileMode:       config.FileModeSkip,
		})
	}

	// One row per unit: the door its own token comes through. Every other row's
	// door must stay shut for the whole of that unit's construction.
	type door struct {
		unit    string // named in the failure message
		select_ func(*supervisor.SingleOptions)
		env     string // the variable this unit's token comes from, if any
		file    string // the path this unit's token comes from, if any
	}
	doors := []door{
		{unit: "david's pod", select_: func(o *supervisor.SingleOptions) { o.Member = "david" }, env: davidBotTokenEnv},
		{unit: "mei's pod", select_: func(o *supervisor.SingleOptions) { o.Member = "mei" }, file: meiTokenPath},
		{unit: "sam's pod", select_: func(o *supervisor.SingleOptions) { o.Member = samMemberID }, file: samCredentialPath},
		{unit: "the group's pod", select_: func(o *supervisor.SingleOptions) { o.Group = true }, env: botTokenEnv},
	}

	for _, d := range doors {
		t.Run(d.unit, func(t *testing.T) {
			env, rfs, secrets := newRecorders()
			opts := supervisor.SingleOptions{
				Memory:    newFakeMemory(),
				Secrets:   secrets,
				LookupEnv: env.lookup,
			}
			d.select_(&opts)
			if opts.Member != "" {
				opts.Sessions = newPodSessions(t, opts.Member)
			}

			if _, err := supervisor.NewSingle(cfg, opts); err == nil {
				t.Fatal("NewSingle accepted a blank token; this test can no longer see the credential path")
			}

			// The unit's own door was opened...
			if d.env != "" && !env.asked(d.env) {
				t.Errorf("%s never read %s; it read env %v and files %v", d.unit, d.env, env.names(), rfs.names())
			}
			if d.file != "" && !rfs.read(d.file) {
				t.Errorf("%s never read %s; it read env %v and files %v", d.unit, d.file, env.names(), rfs.names())
			}
			// ...and every other unit's stayed shut. Another unit's token resolved in
			// this process would undo the mode outright, and for a member the
			// household token would mean their conversation had passed through the
			// operator's bot, which D-023 forbids by construction.
			for _, other := range doors {
				if other.unit == d.unit {
					continue
				}
				if other.env != "" && env.asked(other.env) {
					t.Errorf("%s read %s, which belongs to %s; a pod must resolve no credential but its own (read env %v)",
						d.unit, other.env, other.unit, env.names())
				}
				if other.file != "" && rfs.read(other.file) {
					t.Errorf("%s read %s, which belongs to %s; a pod must resolve no credential but its own (read files %v)",
						d.unit, other.file, other.unit, rfs.names())
				}
			}
		})
	}

	t.Run("given a bot, no credential is resolved at all", func(t *testing.T) {
		// The other half of minimality: a unit handed its transport resolves
		// nothing, so nothing is read that is not used.
		env, rfs, secrets := newRecorders()
		tr := transport.NewFake()
		t.Cleanup(func() { _ = tr.Close() })

		sup, err := supervisor.NewSingle(cfg, supervisor.SingleOptions{
			Member:    "david",
			Transport: tr,
			Memory:    newFakeMemory(),
			Sessions:  newPodSessions(t, "david"),
			Secrets:   secrets,
			LookupEnv: env.lookup,
		})
		if err != nil {
			t.Fatalf("NewSingle: %v", err)
		}
		t.Cleanup(func() { _ = sup.Stop(context.Background()) })

		for _, name := range []string{botTokenEnv, davidBotTokenEnv, meiBotTokenEnv, samBotTokenEnv} {
			if env.asked(name) {
				t.Errorf("the supervisor read %s although it was given its transport; it read %v", name, env.names())
			}
		}
		if got := rfs.names(); len(got) != 0 {
			t.Errorf("the supervisor read credential files %v although it was given its transport", got)
		}
	})
}

// threeDoorConfigYAML is the isolated household with each unit's bot token arriving
// by a different one of the three sources config supports: David's from an
// environment variable, Mei's from a file, Sam's from a systemd credential he states
// nowhere — which is that door's whole point, since the unit file already names it.
// All three have claimed, because an unenrolled member's pod is refused before any
// credential is resolved.
func threeDoorConfigYAML(dataDir, localURL, meiTokenPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode: isolated\n")
	fmt.Fprintf(&b, "data_dir: '%s'\n", dataDir)
	fmt.Fprintf(&b, "household:\n")
	fmt.Fprintf(&b, "  name: Ashfield\n")
	fmt.Fprintf(&b, "  shared_space: %s\n", sharedSpace)
	fmt.Fprintf(&b, "  group_chat_id: %d\n", groupChatID)
	fmt.Fprintf(&b, "  tiers: [local]\n")
	fmt.Fprintf(&b, "telegram:\n")
	fmt.Fprintf(&b, "  bot_token_env: %s\n", botTokenEnv)
	fmt.Fprintf(&b, "members:\n")
	fmt.Fprintf(&b, "  - id: david\n    name: David\n    telegram_id: %d\n    private_space: %s\n    tiers: [local]\n    bot_token_env: %s\n",
		davidTelegramID, davidSpace, davidBotTokenEnv)
	fmt.Fprintf(&b, "  - id: mei\n    name: Mei\n    telegram_id: %d\n    private_space: %s\n    tiers: [local]\n    bot_token_file: '%s'\n",
		meiTelegramID, meiSpace, meiTokenPath)
	fmt.Fprintf(&b, "  - id: %s\n    name: Sam\n    telegram_id: %d\n    private_space: %s\n    tiers: [local]\n",
		samMemberID, samTelegramID, samSpace)
	fmt.Fprintf(&b, "endpoints:\n")
	fmt.Fprintf(&b, "  - name: attic\n    base_url: '%s'\n    model: test-model\n    tags: [local]\n    timeout: 30s\n", localURL)
	fmt.Fprintf(&b, "memory:\n  lore_command: [lore, mcp]\n  search_limit: 8\n")
	fmt.Fprintf(&b, "session:\n  idle_timeout: 30m\n")
	fmt.Fprintf(&b, "capture:\n  max_proposals_per_turn: 1\n")
	fmt.Fprintf(&b, "update:\n  channel: stable\n  check_interval: 6h\n")
	return b.String()
}

// newPodSessions is one pod's session manager: isolated custody, one member's key.
func newPodSessions(t *testing.T, member domain.MemberID) *session.Manager {
	t.Helper()
	m, err := session.NewManager(session.ModeIsolated, session.NewMemStore(),
		session.WithKDFParams(vault.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}))
	if err != nil {
		t.Fatalf("building session manager: %v", err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()
	if err := m.Provision(ctx, member, passphrase); err != nil {
		t.Fatalf("provisioning %s: %v", member, err)
	}
	if err := m.Unlock(ctx, member, passphrase); err != nil {
		t.Fatalf("unlocking %s: %v", member, err)
	}
	return m
}
