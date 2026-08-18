package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
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
// What kenward supplies here is the running daemon and nothing else. The space itself
// is kenward's now — both wizards call lore.CreateSpace, and a pod initialises its own
// home with lore.Init — but *membership* across accounts is not: carrying one space
// into a second pod's store is `lore space invite` there and `lore join` here, and
// lore's embeddable API exposes neither. So an isolated household of pods still has
// one out-of-band step, and it is the last one. See docs/IMPLEMENTATION.md §8 for the
// recipe.
//
// The isolation boundary is lore's, not kenward's, and it does not depend on kenward
// getting this right. A sync exchange begins with a blinded space-id intersection —
// HMAC(space_key, "lore-blind"||space_id) — so two homes exchange a space only when
// both already hold its id AND its key, which only the invite handshake grants. A
// member's private space is a space of its own with a key of its own, generated in
// that member's pod; a sibling pod cannot compute its blinded id, cannot name it, and
// is refused if it asks. Running a daemon in every pod therefore adds one reachable
// space — the household's — and no others.

// SyncCommandArgs is the argv `lore serve` is started with, after the lore command
// itself.
//
// --lan is not optional here and is not a widening. `lore serve` binds loopback only
// by default and advertises on loopback only, and a pod's siblings are not on its
// loopback: they are separate network namespaces on the container runtime's bridge.
// Without it every pod runs a daemon that can never see another one. What --lan means
// inside a pod is the pod network, which is precisely the set of pods this household
// is made of.
//
// mDNS is left on, and that is what makes the two deployment paths need no addresses.
// A pod's sibling address is not knowable when its spec is built — the supervisor
// assigns no IPs and a recreated pod gets a new one — so a static peer list would be
// stale by the first rolling update. Discovery costs nothing in trust: a discovered
// peer is verified over mTLS before it is recorded, and being recorded grants nothing
// on its own, because the blinded intersection still decides what may be exchanged.
var SyncCommandArgs = []string{"serve", "--lan"}

// syncRestartFloor and syncRestartCeiling bound the restart schedule of the daemon.
const (
	syncRestartFloor   = time.Second
	syncRestartCeiling = 30 * time.Second
	// syncHealthyFor is how long the daemon must stay up before its backoff resets.
	syncHealthyFor = time.Minute
)

// RunSyncDaemon runs `lore serve` on cfg's LORE_HOME until ctx is cancelled,
// restarting it on an exponential backoff whenever it exits.
//
// It blocks, so callers run it in a goroutine, and it never returns an error: a
// household whose sync daemon will not start still has a working assistant with
// working private memory, and refusing to serve over it would trade a partial outage
// for a total one. Failures are logged and retried. What reports the state to an
// operator is ReadSyncStatus, through `kenward doctor`.
//
// stderr receives the daemon's own diagnostics — its per-peer sync errors, which are
// the only place a wrong address or an unreachable sibling is named. Nil discards them.
func RunSyncDaemon(ctx context.Context, cfg Config, stderr io.Writer, logger *slog.Logger) {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = cfg.Logger
	}
	if stderr == nil {
		stderr = io.Discard
	}
	delay := syncRestartFloor
	for ctx.Err() == nil {
		started := time.Now()
		err := runSyncOnce(ctx, cfg, stderr)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= syncHealthyFor {
			delay = syncRestartFloor
		}
		logger.Warn("lore: the sync daemon exited; shared memory stops moving until it is back",
			"after", time.Since(started).Round(time.Second), "retry_in", delay, "err", errText(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < syncRestartCeiling {
			delay *= 2
		}
	}
}

// runSyncOnce runs one `lore serve` to completion.
func runSyncOnce(ctx context.Context, cfg Config, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, cfg.Command, SyncCommandArgs...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.childEnv()
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	// Interrupt rather than kill, so the daemon closes its store and removes its
	// daemon.json: a stale daemon.json is a file that tells doctor a daemon is
	// running when none is. WaitDelay is the fallback for a daemon that ignores it,
	// and for Windows, where Signal is not supported at all — isolated mode is
	// Linux-only, but this package builds everywhere.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

func errText(err error) string {
	if err == nil {
		return "exited cleanly"
	}
	return err.Error()
}

// DefaultLoreHome is the lore home a lore subprocess started with this environment
// will use: $LORE_HOME, then ~/.lore. It mirrors lore's own rule, because kenward has
// to look at the same directory the subprocess it starts is looking at, and lore does
// not report the path anywhere a caller can read it.
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
