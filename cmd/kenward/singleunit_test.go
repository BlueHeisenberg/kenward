package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// TestPodForAnUnclaimedMemberServesTheClaimAndThenTheMember.
//
// D-023: in isolated mode a member's bot exists before they claim, and the claim
// happens in a conversation with that bot rather than the household's. The pod
// therefore comes up in a claim-only state, waiting for the code, and starts serving
// the moment it binds.
//
// internal/supervisor implements that and TestSingleClaimOnlyPodServesAfterClaim
// covers it — by setting SingleOptions.Enrol itself. This binary never set it, so
// every real pod for a member who had not claimed refused to start with
// ErrNotEnrolled, and the decision held nowhere but in a library test. That gap is
// here, at the wiring, which is why this test is here too.
//
// Only the three edges are faked, each where kenward stops and another process
// begins: Telegram, lore, the endpoints. The claim code is minted by the real
// `invite` command, the claimer, the binder, the invite store and the session
// manager are the ones the pod builds for itself, and the key derivation is the real
// one — a hook that provisioned nothing would pass a cheaper test.
func TestPodForAnUnclaimedMemberServesTheClaimAndThenTheMember(t *testing.T) {
	t.Parallel()
	// Jordan is declared, has their own bot token, and has not claimed.
	yaml := strings.Replace(isolatedYAML,
		"  - id: jordan\n    name: Jordan\n    telegram_id: 87654321\n",
		"  - id: jordan\n    name: Jordan\n", 1)
	vars := fullEnvironment()
	vars[envPassphrase] = "jordans-own-passphrase"
	h := newHarness(t, yaml, vars)

	// The operator mints the code and hands it over: D-023's extra manual step.
	if code := h.run("invite", "--name", "Jordan"); code != exitOK {
		t.Fatalf("invite exited %d\n%s", code, h.both())
	}
	claimCode, _ := splitClaimCode(t, h.stdout())

	cfg, err := loadConfig(h.config, resolveDataDir(h.e, ""), h.e.secrets())
	if err != nil {
		t.Fatalf("loading the pod's configuration: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(h.e.stderr, nil))
	opts, err := singleUnitOptions(h.e, cfg, runOptions{
		selection: unitSelection{member: "jordan"},
	}, logger)
	if err != nil {
		t.Fatalf("wiring jordan's pod: %v", err)
	}
	fake := transport.NewFake()
	opts.Transport = fake
	opts.Memory = &stubMemory{}
	opts.Router = stubRouter{}

	sup, err := supervisor.NewSingle(cfg, opts)
	if err != nil {
		t.Fatalf("a pod for a member who has not claimed refuses to start: %v\n"+
			"D-023 says their bot exists before they claim and the claim happens on it, "+
			"so this pod has to come up claim-only and wait for the code", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- sup.Start(ctx) }()

	// The claim-only window: up, awaiting enrolment, serving nothing — and silent
	// to a sender with no code, which is what protects the fact the bot is live.
	podWaitFor(t, "the claim-only state", func() bool {
		return podHealth(t, sup)["jordan"] == supervisor.StateNotEnrolled
	})
	const jordanTelegramID = int64(87654321)
	fake.InjectText(jordanTelegramID, jordanTelegramID, "hello?", false)
	time.Sleep(30 * time.Millisecond)
	if n := len(fake.Sent()); n != 0 {
		t.Fatalf("the claim-only pod answered a codeless sender %d times, want silence", n)
	}

	// The code binds, and the pod serves in place.
	fake.InjectText(jordanTelegramID, jordanTelegramID, claimCode, false)
	podWaitFor(t, "the onboarding message", func() bool { return len(fake.Sent()) >= 1 })
	podWaitFor(t, "jordan's unit", func() bool {
		return podHealth(t, sup)["jordan"] == supervisor.StateReady
	})

	before := len(fake.Sent())
	fake.InjectText(jordanTelegramID, jordanTelegramID, "hi", false)
	podWaitFor(t, "jordan's first turn", func() bool { return len(fake.Sent()) > before })
	got, _ := fake.LastSent()
	if replyBody(got.Text) != "via:local,cloud" || got.ChatID != jordanTelegramID {
		t.Fatalf("jordan's first message after claiming was answered with %+v; "+
			"the pod reported itself serving, so it must serve — over jordan's own chain, "+
			"and with a key the claim unlocked", got)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-startErr; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
	_ = fake.Close()
	h.assertNoSecrets(t)
}

func podWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// podHealth indexes the supervisor's health report by member id.
func podHealth(t *testing.T, sup supervisor.Supervisor) map[string]supervisor.State {
	t.Helper()
	hs, err := sup.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	out := make(map[string]supervisor.State, len(hs))
	for _, u := range hs {
		out[string(u.Member)] = u.State
	}
	return out
}

// replyBody strips the retrieval line the assistant prefixes to a reply. These tests
// are about which pod answered over which chain; what that pod read is
// internal/assistant's business and has its own assertions there.
func replyBody(text string) string {
	if _, rest, ok := strings.Cut(text, "</i>\n\n"); ok && strings.HasPrefix(text, "<i>🔍 searched ") {
		return rest
	}
	return text
}

// --- the three faked edges ---------------------------------------------------

// stubMemory stands in for lore: it answers, and remembers nothing.
type stubMemory struct{}

func (*stubMemory) Search(context.Context, memory.SearchQuery) ([]memory.Entry, error) {
	return nil, nil
}

func (*stubMemory) Get(context.Context, domain.SpaceID, string) (memory.Entry, error) {
	return memory.Entry{}, memory.ErrNotFound
}

func (*stubMemory) Put(_ context.Context, space domain.SpaceID, d memory.Draft) (memory.Entry, error) {
	return memory.Entry{ID: "e1", Space: space, Title: d.Title, Body: d.Body}, nil
}

func (*stubMemory) Share(_ context.Context, _, to domain.SpaceID, id string) (memory.Entry, error) {
	return memory.Entry{ID: id, Space: to}, nil
}

func (*stubMemory) Delete(context.Context, domain.SpaceID, string) error { return nil }

func (*stubMemory) Close() error { return nil }

// stubRouter answers with the tier chain it was walked over, so the reply names the
// conversation's own policy rather than a fixed string. It holds no state, so it
// needs no lock for the concurrent turns a Unit dispatches.
type stubRouter struct{}

func (stubRouter) Complete(_ context.Context, chain []string, _ routing.Request) (routing.Completion, error) {
	return routing.Completion{
		Text:         "via:" + strings.Join(chain, ","),
		FinishReason: routing.FinishStop,
	}, nil
}
