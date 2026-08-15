package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/keel/vault"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/session"
)

// TestReadPassphrasePrecedence.
//
// A systemd credential is a file only the service can read; an environment variable
// is visible in /proc and is inherited by the `lore` subprocess kenward starts. The
// mechanism with the smaller blast radius is tried first, and neither is ever asked
// for over Telegram.
func TestReadPassphrasePrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	credPath := filepath.Join(dir, credentialName)
	if err := os.WriteFile(credPath, []byte("from-the-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("credential beats the environment variable", func(t *testing.T) {
		t.Parallel()
		e := &env{lookupEnv: lookup(map[string]string{
			envCredentialsDirectory: dir,
			envPassphrase:           "from-the-environment",
		})}
		p, err := readPassphrase(e)
		if err != nil {
			t.Fatal(err)
		}
		if p.reveal() != "from-the-credential" {
			t.Errorf("passphrase came from %s, want the credential file", p.source)
		}
		// The trailing newline a file inevitably has is not part of the secret.
		if strings.HasSuffix(p.reveal(), "\n") {
			t.Error("the credential's trailing newline was kept")
		}
	})

	t.Run("environment variable when there is no credential", func(t *testing.T) {
		t.Parallel()
		e := &env{lookupEnv: lookup(map[string]string{envPassphrase: "from-the-environment"})}
		p, err := readPassphrase(e)
		if err != nil {
			t.Fatal(err)
		}
		if p.reveal() != "from-the-environment" || p.source != envPassphrase {
			t.Errorf("passphrase = %q from %q", "…", p.source)
		}
	})

	t.Run("a credentials directory without the file falls through", func(t *testing.T) {
		t.Parallel()
		e := &env{lookupEnv: lookup(map[string]string{
			envCredentialsDirectory: t.TempDir(),
			envPassphrase:           "from-the-environment",
		})}
		p, err := readPassphrase(e)
		if err != nil {
			t.Fatal(err)
		}
		if p.source != envPassphrase {
			t.Errorf("source = %q, want the environment variable", p.source)
		}
	})

	t.Run("nothing available, and nobody at a terminal", func(t *testing.T) {
		t.Parallel()
		e := &env{
			stdin:     strings.NewReader("would-be-typed\n"),
			lookupEnv: lookup(nil),
		}
		if _, err := readPassphrase(e); !errors.Is(err, errNoPassphrase) {
			t.Fatalf("err = %v, want errNoPassphrase: a pipe is not somebody standing there", err)
		}
	})
}

// TestPassphraseZeroed: the buffer this process read is overwritten once it has been
// used.
func TestPassphraseZeroed(t *testing.T) {
	t.Parallel()
	p := &passphrase{b: []byte("hunter2"), source: "a test"}
	buf := p.b
	p.zero()
	for i, c := range buf {
		if c != 0 {
			t.Fatalf("byte %d of the buffer was not zeroed", i)
		}
	}
	if !p.empty() {
		t.Error("the passphrase still reports a value after zeroing")
	}
}

// TestPassphraseNeverPrinted: not in a log line, not in an error, not anywhere.
func TestPassphraseNeverPrinted(t *testing.T) {
	t.Parallel()
	const secret = "correct-horse-battery-staple"
	vars := fullEnvironment()
	vars[envPassphrase] = secret

	h := newHarness(t, simpleYAML, vars)
	// Every command, including the ones that read the passphrase.
	for _, args := range [][]string{
		{"doctor"}, {"invite", "--name", "David"}, {"revoke", "david"}, {"version"},
	} {
		h.run(args...)
	}
	if strings.Contains(h.both(), secret) {
		t.Fatal("the passphrase appeared in a command's output")
	}
}

// TestUnlockSessionsProvisionsThenUnlocks.
//
// Provision deliberately leaves nothing unlocked, so a first run has to do both. A
// node that provisioned and stopped there would hold no key and answer every private
// message with the locked notice — which is the whole reason this runs at startup.
func TestUnlockSessionsProvisionsThenUnlocks(t *testing.T) {
	t.Parallel()
	store := session.NewMemStore()
	mgr := newFastManager(t, session.ModeSimple, store)
	members := []domain.Member{
		{ID: "david", Name: "David", TelegramID: 1, Tiers: []string{"local"}},
		{ID: "jordan", Name: "Jordan", TelegramID: 2, Tiers: []string{"local"}},
		{ID: "sam", Name: "Sam"}, // not enrolled: no unit, no key, nothing to do
	}
	pass := &passphrase{b: []byte("a-node-passphrase"), source: "a test"}

	rep, err := unlockSessions(context.Background(), mgr, store, members, pass)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(rep.Provisioned) != 2 || len(rep.Unlocked) != 2 {
		t.Fatalf("provisioned %v, unlocked %v; want both members", rep.Provisioned, rep.Unlocked)
	}
	for _, id := range []domain.MemberID{"david", "jordan"} {
		if _, ok := mgr.Key(id); !ok {
			t.Errorf("%s has no unwrapped key after startup", id)
		}
	}
	if _, ok := mgr.Key("sam"); ok {
		t.Error("sam is not enrolled and must have no key")
	}

	// A second start unlocks what is already there and provisions nothing.
	mgr2 := newFastManager(t, session.ModeSimple, store)
	rep2, err := unlockSessions(context.Background(), mgr2, store, members, pass)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(rep2.Provisioned) != 0 {
		t.Errorf("second start provisioned %v; the keys already existed", rep2.Provisioned)
	}
	if len(rep2.Unlocked) != 2 {
		t.Errorf("second start unlocked %v, want both members", rep2.Unlocked)
	}
}

// TestUnlockSessionsRefusesAWrongPassphrase. Starting anyway would mean a node that
// runs and answers nothing.
func TestUnlockSessionsRefusesAWrongPassphrase(t *testing.T) {
	t.Parallel()
	store := session.NewMemStore()
	members := []domain.Member{{ID: "david", Name: "David", TelegramID: 1}}

	first := newFastManager(t, session.ModeSimple, store)
	if _, err := unlockSessions(context.Background(), first, store, members, &passphrase{b: []byte("right")}); err != nil {
		t.Fatal(err)
	}

	second := newFastManager(t, session.ModeSimple, store)
	_, err := unlockSessions(context.Background(), second, store, members, &passphrase{b: []byte("wrong")})
	if err == nil {
		t.Fatal("a wrong passphrase was accepted")
	}
	if !errors.Is(err, session.ErrBadPassphrase) {
		t.Errorf("err = %v, want session.ErrBadPassphrase", err)
	}
	if strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), "right") {
		t.Errorf("the error quotes a passphrase: %v", err)
	}
}

// TestRunRefusesToStartWithoutAPassphrase.
//
// This is the judgement the whole check rests on. A node that started anyway would
// hold no unwrapped key and would answer every direct message with the locked notice
// — indefinitely, and only in private chats, so the household group would look
// perfectly healthy and nobody would know where to look. A node that refuses to
// start gets fixed in five minutes; a node that silently answers nothing gets blamed
// on the model.
func TestRunRefusesToStartWithoutAPassphrase(t *testing.T) {
	t.Parallel()
	// The real supervisor factory, so this exercises the path a container takes.
	h := newHarness(t, simpleYAML, fullEnvironment())

	if code := h.run("run"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	out := h.stderr()
	for _, want := range []string{
		"no session passphrase available",
		"will not start",
		"locked notice",
		envCredentialsDirectory,
		envPassphrase,
		"never asked for over Telegram",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
}

// TestRunStartsWithAPassphrase: the same path, with the variable set, gets past the
// session gate. It stops at the transport, which needs a bot Telegram will accept —
// that is a different check and a different exit code.
func TestRunStartsPastTheSessionGateWithAPassphrase(t *testing.T) {
	t.Parallel()
	vars := fullEnvironment()
	vars[envPassphrase] = "a-node-passphrase"
	h := newHarness(t, simpleYAML, vars)

	code := h.run("run")
	if strings.Contains(h.stderr(), "no session passphrase") {
		t.Fatalf("the session gate refused a node that has a passphrase:\n%s", h.stderr())
	}
	if code == exitOK {
		t.Fatalf("run returned 0 without a reachable Telegram; something is not being checked")
	}
	// It got far enough to provision and unlock, which is the point.
	if !strings.Contains(h.stderr(), "event=sessions") {
		t.Errorf("no session summary was logged:\n%s", h.stderr())
	}
	h.assertNoSecrets(t)
}

// TestDoctorReportsMembersWithNoKey. The operator has to be able to see this without
// sending a message and waiting for a non-answer.
func TestDoctorReportsMembersWithNoKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.probes = noKeysProvisioned(healthyProbes())

	// It is a warning, not a failure: the container's HEALTHCHECK runs doctor and a
	// household mid-enrolment is not unhealthy.
	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	out := h.stdout()
	for _, want := range []string{"Sessions", "david is enrolled but has no wrapped key", "refused as locked"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor does not report %q:\n%s", want, out)
		}
	}
}

// newFastManager builds a Manager whose key derivation is cheap enough for a test.
// The production cost is the point and is not lowered anywhere but here.
func newFastManager(t *testing.T, mode session.Mode, store session.Store) *session.Manager {
	t.Helper()
	m, err := session.NewManager(mode, store, session.WithKDFParams(fastKDF()))
	if err != nil {
		t.Fatalf("building a session manager: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// fastKDF is the one place in this binary that lowers the key-derivation cost, and
// it is test-only. Production takes the package default: the cost is what makes an
// offline guessing campaign against a stolen wrapped key expensive, so there is no
// configuration knob for it and this deliberately is not one.
func fastKDF() vault.KDFParams {
	return vault.KDFParams{Time: 1, MemoryKiB: 8, Threads: 1}
}
