package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
		p, err := readPassphrase(e, nil)
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
		p, err := readPassphrase(e, nil)
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
		p, err := readPassphrase(e, nil)
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
		if _, err := readPassphrase(e, nil); !errors.Is(err, errNoPassphrase) {
			t.Fatalf("err = %v, want errNoPassphrase: a pipe is not somebody standing there", err)
		}
	})
}

// TestNoOperatorAtACharacterDevice is a regression test from the container, and it has
// two halves because it was fixed twice.
//
// `docker run` without -i hands the process /dev/null on standard input, and /dev/null
// is a character device — so the first "is anyone there" check passed, kenward
// prompted, the read hit end-of-input immediately, and the operator got the setup
// wizard's "input ended" and exit 1 instead of the refusal that lists the ways to
// supply a passphrase. Treating an immediate end-of-input as nobody being there fixed
// the exit code and left the prompt: every non-interactive container logged
//
//	Passphrase for this node's member keys:
//
// and then refused, one line apart, which reads as a node waiting for an answer that
// nobody was ever going to be asked for. The prompt is now offered only where the
// terminal ioctl says somebody could answer it — which is also the only place it could
// be typed without being echoed into that same log.
func TestNoOperatorAtACharacterDevice(t *testing.T) {
	t.Parallel()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("no %s on this platform: %v", os.DevNull, err)
	}
	t.Cleanup(func() { devNull.Close() })

	// The shape that fooled the first check: a character device that is not a
	// terminal. If a platform's null device is not one, the case cannot arise there.
	if info, err := devNull.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		t.Skip("this platform's null device is not a character device, so the case cannot arise")
	}
	if isTerminal(devNull) {
		t.Fatalf("%s is reported as a terminal; the prompt would be written to a log nobody can answer", os.DevNull)
	}

	stderr := &bytes.Buffer{}
	e := &env{
		stdin:     devNull,
		stderr:    stderr,
		lookupEnv: lookup(nil),
	}
	if _, err := readPassphrase(e, nil); !errors.Is(err, errNoPassphrase) {
		t.Fatalf("err = %v, want errNoPassphrase: nothing was there to answer the prompt", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a prompt was written where nobody can answer it: %q", stderr.String())
	}
}

// TestSimpleModeCanNameItsNodePassphrase is the simple-mode half of the defect isolated
// mode had first.
//
// A member's passphrase is a configuration field, so an isolated pod handed none is
// refused at load with the variable named. Simple mode's node passphrase had no field,
// so the same mistake — the one deploy/compose.simple.yml shipped with — reached the
// session manager instead and the container restart-looped there forever, with nothing
// upstream able to say which variable was missing.
//
// The three sources that were already there keep working, because an existing
// deployment states none of this and must not break.
func TestSimpleModeCanNameItsNodePassphrase(t *testing.T) {
	t.Parallel()
	const named = "mode: simple\nsession:\n  passphrase_env: KENWARD_NODE_PASSPHRASE\n"
	namedYAML := strings.Replace(simpleYAML, "mode: simple\n", named, 1)

	t.Run("a named variable that is not set is refused at load, by name", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, namedYAML, fullEnvironment())
		if code := h.run("run"); code != exitUsage {
			t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
		}
		if !strings.Contains(h.stderr(), "KENWARD_NODE_PASSPHRASE") {
			t.Errorf("stderr does not name the variable, which is the whole point of naming it:\n%s", h.stderr())
		}
	})

	t.Run("a named variable that is set is what unwraps the keys", func(t *testing.T) {
		t.Parallel()
		vars := fullEnvironment()
		vars["KENWARD_NODE_PASSPHRASE"] = "from-the-configured-variable"
		// Present as well, and must lose: a file that names a source means it.
		vars[envPassphrase] = "from-KENWARD_PASSPHRASE"
		cfg := mustLoadWith(t, namedYAML, testSecrets(vars))

		e := &env{lookupEnv: lookup(vars)}
		p, err := readPassphrase(e, passphraseRefFor(cfg, cfg.DomainMembers()))
		if err != nil {
			t.Fatalf("readPassphrase: %v", err)
		}
		if p.reveal() != "from-the-configured-variable" {
			t.Errorf("the node unwrapped with the passphrase from %s, want the configured variable", p.source)
		}
	})

	t.Run("naming nothing leaves the three older sources alone", func(t *testing.T) {
		t.Parallel()
		// simpleYAML has no session.passphrase_* at all — the shape of every simple
		// household that already works.
		cfg := mustLoad(t, simpleYAML)
		vars := map[string]string{envPassphrase: "from-KENWARD_PASSPHRASE"}
		e := &env{lookupEnv: lookup(vars)}
		p, err := readPassphrase(e, passphraseRefFor(cfg, cfg.DomainMembers()))
		if err != nil {
			t.Fatalf("readPassphrase: %v", err)
		}
		if p.source != envPassphrase {
			t.Errorf("source = %q, want %s; an existing deployment names no source and must keep working", p.source, envPassphrase)
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
	const pass = "a-node-passphrase"

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
	if _, err := unlockSessions(context.Background(), first, store, members, "right"); err != nil {
		t.Fatal(err)
	}

	second := newFastManager(t, session.ModeSimple, store)
	_, err := unlockSessions(context.Background(), second, store, members, "wrong")
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
	// fullEnvironment holds the node passphrase — `run` refuses without one, so a
	// "full" environment that lacked it was not full — and this test is the one that
	// takes it away again.
	vars := fullEnvironment()
	delete(vars, envPassphrase)
	h := newHarness(t, simpleYAML, vars)

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

// TestDoctorAndRunAgreeAboutAMissingPassphrase.
//
// The defect: on this exact configuration `doctor` printed a clean bill — including
// "every secret the configuration names can be read" — and `kenward run` then exited 2
// three seconds later. Both lines were true. The passphrase is not *named* in the file,
// so a check that reads what the file names cannot see that there is none, and nothing
// else was looking.
//
// True and useless. doctorMemory already writes the rule down for the case where a
// space is missing — unhealthy means "this node would refuse to start on this" — and
// this is that rule seen from the side where nothing is named. It is asserted as an
// agreement between the two commands rather than as a string in a report, for the same
// reason TestStartupAndHealthAgreeAboutMissingSpaces is: what matters is that the two
// cannot drift, not what either of them says today.
func TestDoctorAndRunAgreeAboutAMissingPassphrase(t *testing.T) {
	t.Parallel()
	vars := fullEnvironment()
	delete(vars, envPassphrase)

	hRun := newHarness(t, simpleYAML, vars)
	runCode := hRun.run("run")

	hDoc := newHarness(t, simpleYAML, vars)
	docCode := hDoc.run("doctor")

	if runCode == exitOK {
		t.Fatalf("run started without a passphrase; this test is checking the wrong thing\n%s", hRun.both())
	}
	if docCode == exitOK {
		t.Errorf("doctor exited 0 on a configuration run refuses with %d. A health check that passes a node that cannot start is worse than no health check:\n%s",
			runCode, hDoc.stdout())
	}
	out := hDoc.stdout()
	for _, want := range []string{
		"no session passphrase is available",
		"refuses to start",
		"locked notice",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	hDoc.assertNoSecrets(t)
}

// TestDoctorSaysWhereThePassphraseCameFromAndNotWhatItIs. The source is the whole
// diagnostic value of the line — an operator debugging a pod needs to know whether it
// read the credential or fell back to the environment — and the value is the one thing
// that must never be printed.
func TestDoctorSaysWhereThePassphraseCameFromAndNotWhatItIs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	if !strings.Contains(h.stdout(), "from "+envPassphrase) {
		t.Errorf("doctor does not name the passphrase source:\n%s", h.stdout())
	}
	h.assertNoSecrets(t)
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

// TestAPodProvisionsOnlyItsOwnMembersKey.
//
// This is the isolation the mode exists for, at the point it would be easiest to lose.
// unlockSessions provisions and unlocks whoever it is handed, so passing a pod the
// household's member list — the obvious convenience, and what the simple path
// legitimately does — would have david's pod generate and wrap jordan's key under
// david's passphrase. The pod would then hold a key it must never have, and the mode
// would be a lie while every test about bot tokens still passed.
func TestAPodProvisionsOnlyItsOwnMembersKey(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, isolatedYAML)
	david := mustMember(t, cfg, "david")

	store := session.NewMemStore()
	mgr := newFastManager(t, session.ModeIsolated, store)
	const pass = "davids-own-passphrase"

	rep, err := unlockSessions(context.Background(), mgr, store, []domain.Member{david}, pass)
	if err != nil {
		t.Fatalf("unlocking david's pod: %v", err)
	}
	if len(rep.Provisioned) != 1 || rep.Provisioned[0] != "david" {
		t.Fatalf("provisioned %v, want only david", rep.Provisioned)
	}

	held, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0] != "david" {
		t.Fatalf("the pod's key store holds %v; a pod must hold exactly its own member's key", held)
	}
	if _, ok := mgr.Key("jordan"); ok {
		t.Fatal("david's pod has jordan's key unwrapped in memory")
	}
}

// TestMemberPodUnlocksBeforeStarting.
//
// The TODO left when the single-unit constructor did not exist: a pod that never
// unlocks answers every message its one member sends with the locked notice. It is
// the same bug the end-to-end test caught in simple mode and strictly harder to spot
// here, because there is no group chat alongside it working fine to contrast against
// — the pod simply goes quiet.
func TestMemberPodUnlocksBeforeStarting(t *testing.T) {
	t.Parallel()
	// Every secret this pod needs except the one under test.
	vars := fullEnvironment()
	delete(vars, "KENWARD_PASSPHRASE_DAVID")
	h := newHarness(t, isolatedYAML, vars)
	cfg := mustLoad(t, isolatedYAML)
	logger := slog.New(slog.NewTextHandler(h.e.stderr, nil))

	_, err := newSingleUnitSupervisor(h.e, cfg, runOptions{
		selection: unitSelection{member: "david"},
	}, logger)
	if err == nil {
		t.Fatal("david's pod started with no passphrase; it would answer every private message with the locked notice")
	}
	// And it says which variable, rather than the three-mechanism sermon: the
	// configuration named a source, so the fault is that source and nothing else.
	if !strings.Contains(err.Error(), "KENWARD_PASSPHRASE_DAVID") {
		t.Fatalf("err = %v, want it to name david's own passphrase variable", err)
	}
}

// TestMemberPodTakesItsOwnMembersPassphrase is the defect isolated mode shipped with,
// found the first time the mode was run against a real container runtime.
//
// Nothing could hand a supervisor-started pod a passphrase. `readPassphrase` knew a
// systemd credential, KENWARD_PASSPHRASE and a terminal; a pod has no terminal, the
// host provisioned neither of the other two, and kenward.yaml had nowhere to name one.
// Every member's pod died at startup with "no session passphrase available".
//
// The fix is a passphrase named per member, so the two halves asserted here are the
// whole of it: a pod resolves *its own member's* passphrase, and having one it gets
// past the gate that used to stop it.
func TestMemberPodTakesItsOwnMembersPassphrase(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, isolatedYAML)
	david := mustMember(t, cfg, "david")

	t.Run("the member's own passphrase beats the node's", func(t *testing.T) {
		t.Parallel()
		// Both are present. A pod that took the node-wide one would provision or
		// unwrap under a passphrase every other pod also holds, which is simple
		// mode's key custody wearing isolated mode's name.
		vars := fullEnvironment()
		vars[envPassphrase] = "the-node-wide-passphrase"
		e := &env{lookupEnv: lookup(vars)}

		ref := passphraseRefFor(cfg, []domain.Member{david})
		if ref == nil {
			t.Fatal("a member's pod in isolated mode resolved no member passphrase reference")
		}
		p, err := readPassphrase(e, ref)
		if err != nil {
			t.Fatalf("readPassphrase: %v", err)
		}
		if p.reveal() != fakeDavidPassphrase {
			t.Errorf("the pod unwrapped with the passphrase from %s, want david's own", p.source)
		}
		if !strings.Contains(p.source, "KENWARD_PASSPHRASE_DAVID") {
			t.Errorf("source = %q, want it to name david's own variable", p.source)
		}
	})

	t.Run("simple mode and the group pod use the node passphrase", func(t *testing.T) {
		t.Parallel()
		// Only a pod serving exactly one member in isolated mode has a member
		// passphrase to take. The group pod has no key at all, so it takes nothing.
		if ref := passphraseRefFor(cfg, nil); ref != nil {
			t.Errorf("the group pod resolved a passphrase reference: %+v", ref)
		}
		if ref := passphraseRefFor(cfg, cfg.DomainMembers()); ref != nil {
			t.Errorf("a whole-household isolated process resolved one member's passphrase: %+v", ref)
		}
		// Simple mode takes the node passphrase, which may now be named in the file
		// as session.passphrase_* — never a member's, whichever member it is serving.
		simple := mustLoad(t, simpleYAML)
		ref := passphraseRefFor(simple, []domain.Member{mustMember(t, simple, "david")})
		if ref == nil || ref.Where != "session.passphrase" {
			t.Errorf("simple mode resolved %+v, want the node's session.passphrase reference", ref)
		}
	})

	t.Run("the pod gets past the session gate", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, isolatedYAML, fullEnvironment())
		logger := slog.New(slog.NewTextHandler(h.e.stderr, nil))

		_, err := newSingleUnitSupervisor(h.e, cfg, runOptions{
			selection: unitSelection{member: "david"},
		}, logger)
		// It still fails: the next thing a pod does is call getMe with a bot token
		// that is not a real one. What must not happen is failing for want of a
		// passphrase, which is where it stopped before.
		if errors.Is(err, errNoPassphrase) {
			t.Fatal("david's pod was refused for want of a passphrase its own configuration names")
		}
	})
}

// TestEnrolMidRunProvisionsAndUnlocks.
//
// The hook startSessions hands the supervisor is what makes a claim complete while
// the node runs. It has to work after the passphrase buffer has been zeroed, which
// happens the moment startSessions returns — capturing the *passphrase rather than
// the string it revealed would leave a hook that provisions nothing and fails
// silently in the one place nobody is watching. So this calls it exactly where
// production does: after startup, for a member startup never saw.
//
// The real derivation cost is paid here deliberately. The fast-KDF managers the other
// tests build cannot exercise this path, because the hook builds its manager itself.
func TestEnrolMidRunProvisionsAndUnlocks(t *testing.T) {
	t.Parallel()
	vars := fullEnvironment()
	vars[envPassphrase] = "a-node-passphrase"
	h := newHarness(t, simpleYAML, vars)
	cfg := mustLoad(t, simpleYAML)
	logger := slog.New(slog.NewTextHandler(h.e.stderr, nil))
	david := mustMember(t, cfg, "david")

	sessions, onEnrol, err := startSessions(h.e, cfg, logger, []domain.Member{david})
	if err != nil {
		t.Fatalf("startSessions: %v", err)
	}
	if _, ok := sessions.Key("david"); !ok {
		t.Fatal("startup left david without a key")
	}

	// Jordan claims now. Startup never provisioned anything for them.
	jordan := mustMember(t, cfg, "jordan")
	if _, ok := sessions.Key(jordan.ID); ok {
		t.Fatal("jordan has a key before claiming; the test proves nothing")
	}
	if err := onEnrol(context.Background(), jordan); err != nil {
		t.Fatalf("the enrolment hook: %v", err)
	}
	if _, ok := sessions.Key(jordan.ID); !ok {
		t.Fatal("a member who claimed while the node was running has no unwrapped key, " +
			"so their first private message gets the locked notice and the remedy is an operator restart")
	}

	// Custody is checked for free: in simple mode Provision verifies the offered
	// passphrase against a member already provisioned, so a hook that had captured
	// an emptied buffer would have failed above rather than written a record under
	// a second passphrase.
	h.assertNoSecrets(t)
}

// TestGroupPodNeedsNoPassphrase.
//
// The household group's unit serves the shared space and holds no member key, so
// demanding a passphrase for it would be asking for a secret that unwraps nothing —
// and would stop the group pod starting for no reason.
//
// The configuration here has no group chat, so construction stops immediately on that
// instead: reaching that error at all proves the session gate did not refuse first.
func TestGroupPodNeedsNoPassphrase(t *testing.T) {
	t.Parallel()
	noGroup := strings.Replace(isolatedYAML, "  group_chat_id: -1001234567890\n", "", 1)
	h := newHarness(t, noGroup, fullEnvironment())
	cfg := mustLoad(t, noGroup)
	logger := slog.New(slog.NewTextHandler(h.e.stderr, nil))

	_, err := newSingleUnitSupervisor(h.e, cfg, runOptions{
		selection: unitSelection{group: true},
	}, logger)
	if errors.Is(err, errNoPassphrase) {
		t.Fatal("the group pod was refused for want of a passphrase it has no use for")
	}
	if err == nil || !strings.Contains(err.Error(), "group chat") {
		t.Fatalf("err = %v, want the missing group chat", err)
	}
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
