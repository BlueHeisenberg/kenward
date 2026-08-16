package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The helper-process trick: the test binary re-executes itself as the thing the
// supervisor supervises. It is the only way to check the restart loop and the
// graceful stop against a real process, and on Windows the graceful stop is the
// riskiest code in this package — a console control event aimed at a process group,
// from a program that had to allocate itself a console to be allowed to send one. A
// unit test that faked the process would have proved none of it.
const helperEnv = "KENWARD_DESKTOP_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "die":
		// Fails the way `kenward run` fails on a bad configuration: at once.
		if path := os.Getenv("KENWARD_DESKTOP_TEST_TALLY"); path != "" {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, _ = f.WriteString("x")
				_ = f.Close()
			}
		}
		os.Exit(2)
	case "drain":
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		notifyTerm(c)
		// Only now is it safe to be signalled: on Windows a console control event
		// arriving before the handler is installed runs the default one, which
		// terminates the process. The real daemon has the same window on the way
		// up and it is not worth engineering away — nobody clicks Quit inside the
		// first millisecond — but a test that raced it would be flaky rather than
		// informative.
		_ = os.WriteFile(os.Getenv("KENWARD_DESKTOP_TEST_TALLY"), []byte("ready"), 0o600)
		<-c
		// Proof that the stop was a request and not a kill: a killed process
		// never gets here.
		_ = os.WriteFile(os.Getenv("KENWARD_DESKTOP_TEST_TALLY"), []byte("drained"), 0o600)
		os.Exit(0)
	}
}

func TestDaemonStopsGracefully(t *testing.T) {
	tally := filepath.Join(t.TempDir(), "tally")
	t.Setenv(helperEnv, "drain")
	t.Setenv("KENWARD_DESKTOP_TEST_TALLY", tally)

	d := newDaemon(os.Args[0], "kenward.yaml")
	d.start()
	waitFor(t, d, stateRunning)
	waitForFile(t, tally, "ready")

	done := make(chan struct{})
	go func() { d.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("stop did not return; the child was never asked to drain")
	}

	if st, _ := d.snapshot(); st != stateStopped {
		t.Fatalf("state after stop = %v, want stopped", st)
	}
	got, err := os.ReadFile(tally)
	if err != nil || string(got) != "drained" {
		t.Fatalf("the child was killed rather than asked to drain (tally %q, err %v)", got, err)
	}
}

func TestDaemonRestartsWhatDies(t *testing.T) {
	tally := filepath.Join(t.TempDir(), "tally")
	t.Setenv(helperEnv, "die")
	t.Setenv("KENWARD_DESKTOP_TEST_TALLY", tally)

	d := newDaemon(os.Args[0], "kenward.yaml")
	d.start()
	defer d.stop()
	waitFor(t, d, stateFailed)

	// The first backoff is a second, so a second attempt is due well inside this.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(tally); len(b) >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(tally)
	t.Fatalf("a child that exits immediately was started %d times; the supervisor is not retrying", len(b))
}

func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && string(b) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never contained %q", path, want)
}

func waitFor(t *testing.T, d *daemon, want daemonState) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := d.snapshot(); st == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, detail := d.snapshot()
	t.Fatalf("state = %v (%s), want %v", st, detail, want)
}

func TestDashboardURL(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"no dashboard block", "mode: simple\n", ""},
		{"explicitly off", "dashboard:\n  enabled: false\n  addr: 127.0.0.1:8823\n", ""},
		{"addr", "dashboard:\n  addr: 127.0.0.1:8823\n", "http://127.0.0.1:8823"},
		{"enabled with addr", "dashboard:\n  enabled: true\n  addr: 127.0.0.1:8823\n", "http://127.0.0.1:8823"},
		{"port alone", "dashboard:\n  port: 8823\n", "http://127.0.0.1:8823"},
		// A browser cannot open http://0.0.0.0. Listening everywhere still means
		// reachable here.
		{"wildcard address", "dashboard:\n  addr: 0.0.0.0:8823\n", "http://127.0.0.1:8823"},
		{"bare colon port", "dashboard:\n  addr: \":8823\"\n", "http://127.0.0.1:8823"},
		{"named host is left alone", "dashboard:\n  addr: kenward.local:8823\n", "http://kenward.local:8823"},
		{"enabled but nowhere to go", "dashboard:\n  enabled: true\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kenward.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := dashboardURL(path); got != tc.want {
				t.Errorf("dashboardURL = %q, want %q", got, tc.want)
			}
		})
	}

	// A household with no configuration file at all is the first-run case, and it
	// must be a greyed-out menu item rather than a crash.
	if got := dashboardURL(filepath.Join(t.TempDir(), "absent.yaml")); got != "" {
		t.Errorf("dashboardURL of a missing file = %q, want empty", got)
	}
}

func TestStatusReportReadsDoctorJSON(t *testing.T) {
	const doctorJSON = `{
	  "version": "v0.1.0",
	  "mode": "simple",
	  "configuration": [{"status":"ok","text":"kenward.yaml parses and validates"}],
	  "memory": [{"status":"warn","text":"this lore store does not sync on its own"}],
	  "transport": [{"status":"fail","text":"david: Telegram did not authorise the token"}],
	  "endpoints": [{"name":"monster","reached":false,"detail":"connection refused"}],
	  "exit_code": 1
	}`
	var rep statusReport
	if err := json.Unmarshal([]byte(doctorJSON), &rep); err != nil {
		t.Fatal(err)
	}
	rep.checkedAt = time.Date(2026, 8, 16, 14, 3, 0, 0, time.UTC)

	if rep.healthy() {
		t.Error("a report with exit_code 1 must not be healthy; the icon would stay green")
	}
	joined := strings.Join(rep.lines(), "\n")
	for _, want := range []string{
		"kenward v0.1.0 — mode: simple",
		"✗ david: Telegram did not authorise the token",
		"! monster: connection refused",
		"doctor exit 1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Status menu is missing %q; it reads:\n%s", want, joined)
		}
	}
}

func TestStatusReportSaysWhenDoctorCouldNotRun(t *testing.T) {
	rep := runDoctor(filepath.Join(t.TempDir(), "no-such-kenward"), "kenward.yaml")
	if rep.healthy() {
		t.Error("a doctor that could not be run must not report health")
	}
	if !strings.Contains(rep.lines()[0], "could not run") {
		t.Errorf("lines()[0] = %q, want it to say doctor could not be run", rep.lines()[0])
	}
}

func TestIconsAreDistinct(t *testing.T) {
	seen := map[string]daemonState{}
	for _, st := range []daemonState{stateRunning, stateStopped, stateFailed} {
		b := iconFor(st)
		if len(b) == 0 {
			t.Fatalf("no icon for state %v", st)
		}
		if prev, dup := seen[string(b)]; dup {
			t.Errorf("states %v and %v draw the same icon; the icon is decoration", prev, st)
		}
		seen[string(b)] = st
	}
}

func TestWindowsIconIsAnICO(t *testing.T) {
	if !isWindows {
		t.Skip("the ICO container is only built on Windows")
	}
	b := iconFor(stateRunning)
	if binary.LittleEndian.Uint16(b[2:]) != 1 || binary.LittleEndian.Uint16(b[4:]) != 1 {
		t.Fatalf("not an ICONDIR holding one image: % x", b[:6])
	}
	off := binary.LittleEndian.Uint32(b[18:])
	size := binary.LittleEndian.Uint32(b[14:])
	if int(off)+int(size) != len(b) {
		t.Fatalf("directory entry points at %d..%d of a %d-byte file", off, off+size, len(b))
	}
	if string(b[off+1:off+4]) != "PNG" {
		t.Fatalf("the entry does not point at the PNG: % x", b[off:off+8])
	}
}
