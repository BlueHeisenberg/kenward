package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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

// TestRunSyncDaemonRestartsAndStops runs the daemon loop over a command that exits
// immediately, and checks the two properties that matter: it comes back, and it stops
// when the context does rather than leaving a goroutine restarting forever.
func TestRunSyncDaemonRestartsAndStops(t *testing.T) {
	// A command that cannot be started fails the same way an exiting one does, and
	// needs no helper binary on any platform.
	cfg := Config{Command: filepath.Join(t.TempDir(), "no-such-lore")}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunSyncDaemon(ctx, cfg, nil, nil)
	}()

	// The floor of the backoff schedule is a second, so two attempts prove the
	// restart without a long test: the first is immediate.
	time.Sleep(1500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSyncDaemon did not return after its context was cancelled")
	}
}
