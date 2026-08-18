package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/lore"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// writeDaemonJSON puts a daemon.json in home pointing at addr's port, the way
// `lore serve` does when it starts.
func writeDaemonJSON(t *testing.T, home, addr, token string) {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("addr %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	b, err := json.Marshal(map[string]any{"port": port, "token": token, "sync_port": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "daemon.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadSyncStatusReportsTheDaemonsOwnAnswer(t *testing.T) {
	const token = "s3cret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"device_id":"dev-1","last_sync":"2026-08-16T10:50:25Z",
			"sync_errors":["peer beef: connection refused"],
			"peers":[{"device_id":"a"},{"device_id":"b"}]}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	writeDaemonJSON(t, home, srv.Listener.Addr().String(), token)

	st, err := ReadSyncStatus(context.Background(), home)
	if err != nil {
		t.Fatalf("ReadSyncStatus: %v", err)
	}
	if !st.Running || st.DeviceID != "dev-1" || st.Peers != 2 {
		t.Fatalf("got %+v", st)
	}
	if want := time.Date(2026, 8, 16, 10, 50, 25, 0, time.UTC); !st.LastSync.Equal(want) {
		t.Fatalf("last sync %v, want %v", st.LastSync, want)
	}
	if len(st.Errors) != 1 {
		t.Fatalf("errors %v", st.Errors)
	}
}

func TestReadSyncStatusWithoutADaemon(t *testing.T) {
	// No daemon.json at all: the normal state of a home nobody is serving.
	if _, err := ReadSyncStatus(context.Background(), t.TempDir()); !errors.Is(err, ErrNoSyncDaemon) {
		t.Fatalf("missing daemon.json: %v, want ErrNoSyncDaemon", err)
	}

	// A daemon.json left behind by a daemon that was killed rather than stopped.
	// Believing it would tell an operator shared memory is syncing when it is not.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.Listener.Addr().String()
	srv.Close()
	home := t.TempDir()
	writeDaemonJSON(t, home, addr, "t")
	if _, err := ReadSyncStatus(context.Background(), home); !errors.Is(err, ErrNoSyncDaemon) {
		t.Fatalf("stale daemon.json: %v, want ErrNoSyncDaemon", err)
	}
}

// initHome makes a lore home and opens a kenward client on it. Both halves are
// library calls: lore.Init is what a pod runs on its own empty volume, and NewClient
// is the store every read and write in this process goes through.
func initHome(t *testing.T, name string) *Client {
	t.Helper()
	home := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := lore.Init(home, name); err != nil {
		t.Fatalf("lore.Init %s: %v", home, err)
	}
	c, err := NewClient(Config{LoreHome: home})
	if err != nil {
		t.Fatalf("NewClient %s: %v", home, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// serving runs c.Serve in a goroutine and blocks until the daemon has published
// itself, which is exactly what `kenward doctor` looks for. It returns the function
// that cancels and waits, and fails the test if Serve reported a startup error.
func serving(t *testing.T, c *Client, interval time.Duration) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx, interval) }()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := ReadSyncStatus(context.Background(), c.cfg.LoreHome); err == nil {
			break
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("Serve returned before the daemon was up: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the sync daemon never published a daemon.json")
		}
		time.Sleep(50 * time.Millisecond)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve returned %v; a clean shutdown is nil", err)
				}
			case <-time.After(30 * time.Second):
				t.Error("Serve did not return within 30s of cancellation")
			}
		})
	}
}

// TestServeRunsAndStopsInProcess is the lifecycle half of the in-process daemon:
// it comes up on the store this Client already has open, it is visible to the same
// admin API `kenward doctor` reads, and cancelling the context takes it down —
// cleanly, completely, and without leaving a goroutine behind.
//
// The goroutine count is the assertion that matters most here. The thing this
// replaced was a supervised subprocess whose exit was observable from the outside;
// an in-process daemon that does not stop is a leak nothing reports, and kenward's
// supervisor has spent this week finding out what those cost.
func TestServeRunsAndStopsInProcess(t *testing.T) {
	c := initHome(t, "node")
	home := c.cfg.LoreHome

	// After the store is open, so sqlite's own workers are in the baseline and only
	// the daemon's goroutines are being counted.
	base := runtime.NumGoroutine()

	stop := serving(t, c, 200*time.Millisecond)

	st, err := ReadSyncStatus(context.Background(), home)
	if err != nil {
		t.Fatalf("ReadSyncStatus while serving: %v", err)
	}
	if !st.Running || st.DeviceID == "" {
		t.Fatalf("the daemon is up but reports %+v", st)
	}
	if st.DeviceID != c.store.DeviceID() {
		t.Fatalf("the daemon reports device %s, the store this Client opened is %s — "+
			"they must be one store", st.DeviceID, c.store.DeviceID())
	}

	stop()

	// A clean stop removes daemon.json, which is what keeps `doctor` from reporting
	// a daemon that is not there.
	if _, err := ReadSyncStatus(context.Background(), home); !errors.Is(err, ErrNoSyncDaemon) {
		t.Errorf("after shutdown ReadSyncStatus = %v, want ErrNoSyncDaemon", err)
	}
	// Serve neither opened nor closed the store, so it is still the caller's.
	if _, err := c.Spaces(context.Background()); err != nil {
		t.Errorf("the store is unusable after Serve returned: %v", err)
	}

	// Listeners and their goroutines unwind after the return rather than during it,
	// so this settles rather than snapping. base is the count with the store open and
	// no daemon; mdnsResidual is what the count is allowed to exceed it by.
	deadline := time.Now().Add(10 * time.Second)
	for {
		live, residual := goroutinesAfterServe()
		n := runtime.NumGoroutine()
		if len(live) == 0 && n <= base+residual {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after the daemon stopped, %d before it started, %d of them "+
				"the known mDNS residual; still running:\n%s", n, base, residual, strings.Join(live, "\n\n"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// goroutinesAfterServe splits the live goroutines into the ones the daemon should have
// taken with it and the ones it may leave.
//
// The residual is one goroutine per Serve and it is not kenward's: agentmesh's
// discovery.Browse starts a reader over a `merged` channel that nothing ever closes, so
// it stays parked on a receive after its context is cancelled and everything else in the
// daemon has stopped. It is bounded — one per daemon start, and kenward starts one
// daemon per process — and it holds nothing but an empty channel. Measured here rather
// than assumed; it belongs in lore's dependency, not in this repository, and this test
// is where it will be noticed if it ever stops being the only one.
func goroutinesAfterServe() (live []string, residual int) {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		switch {
		case strings.Contains(g, "agentmesh/pkg/discovery.Browse"):
			residual++
		case strings.Contains(g, "lore/internal/daemon"),
			strings.Contains(g, "memory.(*Client).Serve"),
			strings.Contains(g, "lore.(*Store).Serve"):
			live = append(live, g)
		}
	}
	return live, residual
}

// TestServeConvergesTwoStoresInProcess is the premise of the whole change: two
// kenward memory clients, two lore homes, two daemons, one process — and an entry
// written through Client.Put on one turns up through Client.Get on the other. No
// `lore serve` is started, no lore binary is executed, and nothing between the two
// stores is a pipe.
//
// # The one subprocess, and why it is not kenward's
//
// Two homes exchange a space only when both already hold its id and its key, and the
// only thing that grants that is the invite handshake — `lore space invite` on one
// side, `lore join` on the other. lore's Go API exposes neither, deliberately: a
// membership is a person's decision about who may read a household's memory, and
// kenward is not the one to take it. So the fixture below runs the two commands an
// operator runs, once, before anything under test starts. That is the same
// out-of-band step deploy/compose.isolated.yml documents, and it is the only reason
// `lore` appears in this file at all. Everything the test actually asserts — the
// stores, the daemons, the write and the read — is a library call in this process.
//
// It skips where lore is not installed, because there is nothing it could provision.
func TestServeConvergesTwoStoresInProcess(t *testing.T) {
	ctx := context.Background()
	a := initHome(t, "a")
	b := initHome(t, "b")

	shared, err := a.CreateSpace(ctx, "household")
	if err != nil {
		t.Fatal(err)
	}
	joinSpace(t, a.cfg.LoreHome, b.cfg.LoreHome, shared.ID)

	// A short interval so the test does not sit through lore's default thirty
	// seconds waiting for the round that follows discovery. The write itself pokes
	// the daemon — NewClient opens the store with NotifyOnWrite — but a poke before
	// the peer has been found over mDNS has nobody to push to.
	defer serving(t, a, 200*time.Millisecond)()
	defer serving(t, b, 200*time.Millisecond)()

	space := domain.SpaceID(shared.ID)
	written, err := a.Put(ctx, space, Draft{
		Domain: "household/routine",
		Title:  "Bins",
		Body:   "the green bin goes out on Tuesday",
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		got, err := b.Get(ctx, space, written.ID)
		if err == nil {
			if got.Body != written.Body {
				t.Fatalf("the entry arrived with body %q, want %q", got.Body, written.Body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the entry never reached the second store: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// joinSpace gives the home at to membership of the space at from, over lore's LAN
// invite handshake on loopback. It is the operator's step; see the test above.
func joinSpace(t *testing.T, from, to, spaceID string) {
	t.Helper()
	exe, err := exec.LookPath("lore")
	if err != nil {
		t.Skipf("no `lore` on PATH to provision space membership with, and lore's Go API "+
			"exposes no invite or join: %v", err)
	}

	invite := exec.Command(exe, "space", "invite", spaceID, "--lan", "--yes", "--no-mdns", "--timeout", "90s")
	invite.Env = append(os.Environ(), "LORE_HOME="+from)
	out, err := invite.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	invite.Stderr = os.Stderr
	if err := invite.Start(); err != nil {
		t.Fatalf("lore space invite: %v", err)
	}
	defer invite.Wait()

	// "(if mDNS discovery fails: lore join <code> --addr <this-host>:<port>)" — the
	// one line that carries both halves. --no-mdns above is what makes it the only
	// route, so the joiner cannot succeed by accident through discovery.
	var code, port string
	scan := bufio.NewScanner(out)
	re := regexp.MustCompile(`lore join (\S+) --addr <this-host>:(\d+)`)
	for scan.Scan() {
		if m := re.FindStringSubmatch(scan.Text()); m != nil {
			code, port = m[1], m[2]
			break
		}
	}
	if code == "" {
		invite.Process.Kill()
		t.Fatalf("lore space invite printed no join line: %v", scan.Err())
	}

	join := exec.Command(exe, "join", code, "--addr", "127.0.0.1:"+port, "--yes")
	join.Env = append(os.Environ(), "LORE_HOME="+to)
	if b, err := join.CombinedOutput(); err != nil {
		invite.Process.Kill()
		t.Fatalf("lore join: %v\n%s", err, b)
	}
	io.Copy(io.Discard, out)
}

// TestNothingKenwardRunsIsALoreBinary is the guard on the claim this change exists to
// make: kenward needs no `lore` binary in any mode.
//
// What it actually asserts, precisely, because the honest scope matters more than the
// headline: across every non-test Go file in this module, no call to exec.Command,
// exec.CommandContext or exec.LookPath names lore — and no package under internal/
// imports os/exec at all, which is where every route to memory now lives. It reads the
// source rather than the binary, so what it cannot see is a program name computed at
// run time out of parts none of which say "lore". Nothing in this module does that; the
// end-to-end answer is the container measurement, where an isolated household syncs its
// shared space with no lore binary present at all.
//
// cmd/kenward-desktop is the one place os/exec survives, and what it starts is kenward
// itself — the rule above still applies to it.
func TestNothingKenwardRunsIsALoreBinary(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			if name := d.Name(); name == ".git" || name == ".claude" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel := filepath.ToSlash(path)
		for _, imp := range f.Imports {
			if imp.Path.Value == `"os/exec"` && strings.Contains(rel, "/internal/") {
				t.Errorf("%s imports os/exec; nothing under internal/ starts a program any more", rel)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			switch sel.Sel.Name {
			case "Command", "LookPath":
			case "CommandContext":
				call.Args = call.Args[1:]
			default:
				return true
			}
			var b strings.Builder
			if err := printer.Fprint(&b, fset, call.Args[0]); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(b.String()), "lore") {
				t.Errorf("%s:%d runs %s — kenward does not execute lore",
					rel, fset.Position(call.Pos()).Line, b.String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
