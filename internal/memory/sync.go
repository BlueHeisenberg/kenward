package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/BlueHeisenberg/lore"
)

// The sync daemon, and why kenward has to run one.
//
// lore isolates instances by LORE_HOME, not by machine, and `lore mcp` never syncs:
// it reads and writes the SQLite store under that home and fires one best-effort poke
// at a daemon named in LORE_HOME/daemon.json. Carrying an entry from one lore home to
// another is `lore serve`'s job and nothing else's.
//
// In simple mode that costs a household nothing, because there is one home and every
// space in it. In isolated mode it is the whole of the defect this file closes: each
// pod has its own LORE_HOME, therefore its own lore account, therefore its own id
// space, so the single household.shared_space in kenward.yaml could only ever be real
// in one pod. Every other pod's `doctor` reported it missing:
//
//	✗ space "…" is not a space this lore store holds
//
// and the household group conversation had memory in exactly one container. Private
// memory was never affected — that is the property the mode exists for — but shared
// memory did not work at all, in either deployment path, and neither said so.
//
// What kenward supplies here is the running daemon and nothing else. The spaces
// themselves are kenward's now — both wizards call lore.CreateSpace, a pod initialises
// its own home with lore.Init, and a pod creates the spaces it is configured for at
// the configured ids with lore.CreateSpaceWithID — but *membership* across accounts is
// not: carrying one space into a second pod's store is `lore space invite` there and
// `lore join` here, and lore's Go API exposes neither. A household assistant minting
// its own memberships would in any case be kenward taking a decision that is not its
// to take. So an isolated household of pods has one out-of-band step left, and it is
// the last one and the right one: a person runs those two commands inside the pods,
// which is the sole reason the image carries the `lore` binary at all. kenward itself
// executes nothing. See docs/IMPLEMENTATION.md §8 for the recipe.
//
// The isolation boundary is lore's, not kenward's, and it does not depend on kenward
// getting this right. A sync exchange begins with a blinded space-id intersection —
// HMAC(space_key, "lore-blind"||space_id) — so two homes exchange a space only when
// both already hold its id AND its key, which only the invite handshake grants. A
// member's private space is a space of its own with a key of its own, generated in
// that member's pod; a sibling pod cannot compute its blinded id, cannot name it, and
// is refused if it asks. Running a daemon in every pod therefore adds one reachable
// space — the household's — and no others.

// Serve runs lore's sync daemon on this client's own store until ctx is cancelled.
//
// It is lore's daemon, not a reimplementation of it and not a subprocess: the same
// engine `lore serve` runs, in this process, over the store this Client already has
// open. Nothing here executes a `lore` binary, and after this there is no mode in which
// kenward needs one — the store, its account, its spaces and every read and write are
// all Go calls now.
//
// One store, one daemon. It runs on the Client the unit itself uses rather than on a
// second handle of its own, because a home with two daemons on it works but degrades in
// three ways worth avoiding: they bind different ports while advertising one device id,
// so a peer's recorded address flaps; the second overwrites daemon.json, so write pokes
// reach only one of them; and whichever stops first removes daemon.json, after which
// pokes reach nobody.
//
// # LAN, and why it is not a widening
//
// The daemon binds loopback only by default, and a pod's siblings are not on its
// loopback: they are separate network namespaces on the container runtime's bridge.
// Without LAN every pod would run a daemon that can never see another one. What LAN
// means inside a pod is the pod network, which is precisely the set of pods this
// household is made of, and it widens who may open a TLS connection rather than who may
// read anything — the blinded space-id intersection above still decides that.
//
// mDNS is left on, and that is what makes the two deployment paths need no addresses. A
// pod's sibling address is not knowable when its spec is built — the supervisor assigns
// no IPs and a recreated pod gets a new one — so a static peer list would be stale by
// the first rolling update.
//
// LAN is therefore what Serve asks for unless a caller says otherwise, and LoopbackOnly
// is how it says so. Nothing in kenward passes it; the tests in this package do, because
// two stores in one process are on each other's loopback already and a 0.0.0.0 bind buys
// them nothing but a Windows Firewall prompt on every `go test` — a fresh temp binary
// each run, so no allow-rule ever sticks.
//
// # Blocking, stopping, failing
//
// It blocks, so callers run it in a goroutine and cancel ctx to stop it. By the time it
// returns the listeners are closed, daemon.json is gone and the daemon's goroutines are
// finished; the store is untouched and still the caller's to close, which the caller
// must do after this returns and not before.
//
// A nil return means a clean shutdown, so a non-nil one is a real failure — and it can
// only ever be a failure to *start*: a listener that would not bind, an identity that
// would not load, a read-only or closed store. Per-round and per-peer failures never
// come back this way; they go to the logger. There is deliberately no restart loop for
// the startup errors that remain, because none of them is transient — the supervision
// this replaced existed to babysit a subprocess that could exit for a hundred reasons,
// and there is no longer a process to babysit. The caller logs it and carries on
// serving: private memory works without sync, and refusing a household its assistant
// over a daemon that would not bind trades a partial outage for a total one.
//
// interval is how often a sync round runs; zero takes lore's own thirty seconds. A
// round also runs at start and on every local write, because NewClient opens the store
// with NotifyOnWrite.
func (c *Client) Serve(ctx context.Context, interval time.Duration, opts ...ServeOption) error {
	o := serveOptions(interval, opts...)
	// The daemon's running diagnostics — a peer that will not answer, a hello
	// refused, an address forgotten — are the only account of why shared memory
	// is not moving, and lore discards them if nobody takes them. They went to
	// the subprocess's stderr before; now they go where the rest of this
	// process's operational log goes. Warn, because every one of them is
	// something not working.
	o.Logf = func(format string, args ...any) {
		c.cfg.Logger.Warn("lore: sync", "detail", fmt.Sprintf(format, args...))
	}
	o.Ready = func(info lore.ServeInfo) {
		c.cfg.Logger.Info("lore: the sync daemon is listening",
			"device", info.DeviceID, "sync_port", info.SyncPort, "admin_port", info.AdminPort)
	}
	return c.store.Serve(ctx, o)
}

// A ServeOption changes what Serve asks lore's daemon for. There is one, and it exists
// because the answer differs between a pod and a test rather than because anything is
// configurable: see LoopbackOnly.
type ServeOption func(*lore.ServeOptions)

// LoopbackOnly keeps the sync daemon off every interface but loopback.
//
// It is the right answer for two stores in one process and the wrong one for a pod,
// whose siblings are reachable only over the pod network — which is why Serve's default
// is the LAN and this has to be asked for. Discovery still works: lore advertises and
// browses mDNS on loopback whatever LAN is set to, so two daemons on one machine find
// each other and converge exactly as they would over a bridge.
func LoopbackOnly() ServeOption {
	return func(o *lore.ServeOptions) { o.LAN = false }
}

// serveOptions is the LAN decision, split out from Serve so it can be asserted without
// binding anything. Turning the default off here would silently stop every household's
// pods from seeing each other, and TestServeAsksForTheLANUnlessToldOtherwise is what
// notices.
func serveOptions(interval time.Duration, opts ...ServeOption) lore.ServeOptions {
	o := lore.ServeOptions{LAN: true, SyncInterval: interval}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// DefaultLoreHome is the lore home this process uses when none is configured:
// $LORE_HOME, then ~/.lore. It mirrors lore's own rule, because `kenward doctor` runs
// as a separate process and has to find the store and the daemon.json of the node it is
// reporting on.
//
// It returns the empty string when neither can be determined, which callers must treat
// as "unknown" rather than as the current directory.
func DefaultLoreHome() string {
	if h := os.Getenv("LORE_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".lore")
}

// SyncStatus is what a running `lore serve` reports about itself.
//
// It is the only observation of shared memory available from outside the daemon, and
// it is deliberately shallow: peers and rounds, never entries. What is in anyone's
// memory is not something a health check may report.
type SyncStatus struct {
	// Running reports whether a daemon answered on this lore home.
	Running bool
	// DeviceID is the daemon's lore device id. Empty when it did not answer.
	DeviceID string
	// Peers is how many other lore instances this daemon has verified and recorded.
	// Zero with Running true means it is up and has found nobody: in a household of
	// pods that is shared memory not reaching anyone.
	Peers int
	// LastSync is when the last sync round finished, zero if none has.
	LastSync time.Time
	// Errors are the per-peer failures of the last round, as the daemon phrased them.
	Errors []string
}

// daemonJSON is LORE_HOME/daemon.json, which `lore serve` writes at start and removes
// at a clean stop. Only the two fields needed to reach the admin API are read.
type daemonJSON struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// adminStatus is the shape of the daemon's GET /admin/status response. Only the parts
// doctor reports are decoded.
type adminStatus struct {
	DeviceID string   `json:"device_id"`
	LastSync string   `json:"last_sync"`
	SyncErrs []string `json:"sync_errors"`
	Peers    []struct {
		DeviceID string `json:"device_id"`
	} `json:"peers"`
}

// ErrNoSyncDaemon reports that no `lore serve` is running on this lore home.
//
// It is a distinct error because it is the normal state of a simple-mode node and a
// fault in an isolated pod, and only the caller knows which of the two it is looking
// at.
var ErrNoSyncDaemon = errors.New("memory: no lore sync daemon on this lore home")

// ReadSyncStatus asks the sync daemon on loreHome about itself.
//
// The daemon publishes a loopback admin port and a token in LORE_HOME/daemon.json for
// exactly this. No daemon.json, or a daemon.json naming a port nothing answers on,
// means no daemon: the file is removed on a clean stop, and a torn one left by a
// killed daemon fails the request rather than being believed.
func ReadSyncStatus(ctx context.Context, loreHome string) (SyncStatus, error) {
	if loreHome == "" {
		return SyncStatus{}, fmt.Errorf("memory: lore home is unknown: %w", ErrInvalidArgument)
	}
	b, err := os.ReadFile(filepath.Join(loreHome, "daemon.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return SyncStatus{}, ErrNoSyncDaemon
	case err != nil:
		return SyncStatus{}, fmt.Errorf("memory: reading the sync daemon's admin details: %w", err)
	}
	var dj daemonJSON
	if err := json.Unmarshal(b, &dj); err != nil {
		return SyncStatus{}, fmt.Errorf("memory: %s/daemon.json is not readable as JSON: %w", loreHome, err)
	}
	if dj.Port == 0 {
		return SyncStatus{}, fmt.Errorf("memory: %s/daemon.json names no admin port: %w", loreHome, ErrNoSyncDaemon)
	}
	return readSyncStatusAt(ctx, fmt.Sprintf("http://127.0.0.1:%d", dj.Port), dj.Token)
}

// readSyncStatusAt is ReadSyncStatus once the admin endpoint is known. It is separate
// so a test can point it at an httptest server without a lore home on disk.
func readSyncStatusAt(ctx context.Context, base, token string) (SyncStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/admin/status?token="+token, nil)
	if err != nil {
		return SyncStatus{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The file said there is a daemon and nothing answered. That is a dead
		// daemon, not a broken kenward, and it is the same answer as no file.
		return SyncStatus{}, fmt.Errorf("%w: it published %s and did not answer there", ErrNoSyncDaemon, base)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SyncStatus{}, fmt.Errorf("memory: the sync daemon answered %s", resp.Status)
	}
	var as adminStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&as); err != nil {
		return SyncStatus{}, fmt.Errorf("memory: the sync daemon's status is not readable as JSON: %w", err)
	}
	st := SyncStatus{
		Running:  true,
		DeviceID: as.DeviceID,
		Peers:    len(as.Peers),
		Errors:   as.SyncErrs,
	}
	if as.LastSync != "" {
		if t, err := time.Parse(time.RFC3339, as.LastSync); err == nil {
			st.LastSync = t
		}
	}
	return st, nil
}
