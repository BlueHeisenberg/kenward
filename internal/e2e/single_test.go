package e2e

import (
	"context"
	"errors"
	"fmt"
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

	samMemberID = domain.MemberID("sam")
	samSpace    = domain.SpaceID("sam-private")
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
	// No telegram_id: sam has been given a bot and a code and has not used it.
	fmt.Fprintf(&b, "  - id: %s\n    name: Sam\n    private_space: %s\n    tiers: [local]\n    bot_token_env: %s\n",
		samMemberID, samSpace, samBotTokenEnv)
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
	path := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(path, []byte(isolatedConfigYAML(dir, local.baseURL())), 0o600); err != nil {
		t.Fatalf("writing configuration: %v", err)
	}
	cfg, err := config.LoadWithEnv(path, lookupEnv)
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	return cfg, local, dir
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

// TestPodReadsOnlyItsOwnMembersBotToken is the credential-minimality assertion. A
// pod holding a second member's bot token would be able to read and write that
// member's private conversation, which is the entire property isolated mode sells;
// and a token a process never reads is a token a compromise of that process cannot
// disclose.
//
// The environment is injectable, so what a pod asked for is directly observable.
func TestPodReadsOnlyItsOwnMembersBotToken(t *testing.T) {
	// The configuration is loaded through a separate lookup: loading validates the
	// whole household's file and therefore reads every member's token variable by
	// design. What is under test is what the supervisor reads afterwards.
	cfg, _, _ := loadIsolatedConfig(t, fakeEnv(podEnvironment()))

	t.Run("building its own bot", func(t *testing.T) {
		// Every token variable resolves to a single space. It is non-empty, so the
		// supervisor accepts it and goes on to build the transport, which rejects a
		// blank token locally — before it opens a connection to anything. That is
		// what lets this test observe the real credential path without a network:
		// construction gets exactly as far as the environment lookup and stops.
		blank := podEnvironment()
		for _, name := range []string{botTokenEnv, davidBotTokenEnv, meiBotTokenEnv, samBotTokenEnv} {
			blank[name] = " "
		}
		env := newRecordingEnv(blank)

		_, err := supervisor.NewSingle(cfg, supervisor.SingleOptions{
			Member:    "david",
			Memory:    newFakeMemory(),
			Sessions:  newPodSessions(t, "david"),
			LookupEnv: env.lookup,
		})
		if err == nil {
			t.Fatal("NewSingle built a transport from a blank token; this test can no longer see the credential path")
		}
		if !env.asked(davidBotTokenEnv) {
			t.Fatalf("david's pod never read %s; it read %v", davidBotTokenEnv, env.names())
		}
		// The two that matter. Another member's token in this process would undo the
		// mode outright; the household token would mean the member's conversation
		// went through the operator's bot, which D-023 forbids by construction.
		if env.asked(meiBotTokenEnv) {
			t.Errorf("david's pod read %s; a pod must never resolve another member's bot token (asked: %v)",
				meiBotTokenEnv, env.names())
		}
		if env.asked(samBotTokenEnv) {
			t.Errorf("david's pod read %s; a pod must never resolve another member's bot token (asked: %v)",
				samBotTokenEnv, env.names())
		}
		if env.asked(botTokenEnv) {
			t.Errorf("david's pod read the household token %s; no member's conversation may traverse the household bot (asked: %v)",
				botTokenEnv, env.names())
		}
	})

	t.Run("given a bot", func(t *testing.T) {
		// The production path a pod actually takes still resolves one token. This
		// case is the other half: when the transport is supplied, no bot token is
		// resolved at all, so nothing is read that is not used.
		env := newRecordingEnv(podEnvironment())
		tr := transport.NewFake()
		t.Cleanup(func() { _ = tr.Close() })

		sup, err := supervisor.NewSingle(cfg, supervisor.SingleOptions{
			Member:    "david",
			Transport: tr,
			Memory:    newFakeMemory(),
			Sessions:  newPodSessions(t, "david"),
			LookupEnv: env.lookup,
		})
		if err != nil {
			t.Fatalf("NewSingle: %v", err)
		}
		t.Cleanup(func() { _ = sup.Stop(context.Background()) })

		for _, name := range []string{botTokenEnv, davidBotTokenEnv, meiBotTokenEnv, samBotTokenEnv} {
			if env.asked(name) {
				t.Errorf("the supervisor read %s although it was given its transport; it asked for %v", name, env.names())
			}
		}
	})
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
