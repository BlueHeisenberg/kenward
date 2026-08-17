package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	keelupdate "github.com/BlueHeisenberg/keel/update"
)

// Both compose files ship `read_only: true`, correctly, and every household running
// them opened its log with:
//
//	WARN ... event=update detail="could not finish a pending update"
//	  err="update: create lock file: open /usr/local/bin/kenward.update.lock: read-only file system"
//
// immediately followed by "updater: updates are off; kenward will never fetch, check or
// replace anything". Resume is right to run with the channel off — an update in flight
// when updates were turned off still deserves an answer — but it reached for the
// cross-process lock before looking at whether there was anything to resume, so a
// deployment that can never have staged an update warned about failing to finish one.

// resumeFixture builds a Scheduler whose install path is target.
func resumeFixture(t *testing.T, target string) *Scheduler {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s, err := New(Options{
		Channel:        keelupdate.ChannelOff,
		CheckInterval:  time.Hour,
		ManifestURL:    manifestURL,
		Keys:           []ed25519.PublicKey{pub},
		CurrentVersion: "v1.0.0",
		targetPath:     target,
		skipPreflight:  true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestResumeTouchesNothingWhenThereIsNoUpdateToFinish.
//
// The install path here cannot be written at all — its directory does not exist, which
// is the portable stand-in for the read-only filesystem every shipped container has.
// With no update state on disk there is nothing to resume, so nothing should be
// attempted and nothing should be reported.
func TestResumeTouchesNothingWhenThereIsNoUpdateToFinish(t *testing.T) {
	target := filepath.Join(t.TempDir(), "no-such-dir", "kenward")

	rep, err := resumeFixture(t, target).Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume warned about an update that was never in flight: %v", err)
	}
	if rep.Outcome != keelupdate.OutcomeNone {
		t.Errorf("outcome = %v, want OutcomeNone", rep.Outcome)
	}
}

// TestResumeStillFinishesAnUpdateThatIsInFlight: the skip is conditional on there being
// nothing to do, and not on the channel, the filesystem or anything else. A journal on
// disk is an update in flight and it is still resumed — here it is deliberately corrupt,
// which is the cheapest way to prove keel was reached at all.
func TestResumeStillFinishesAnUpdateThatIsInFlight(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "kenward")
	if err := os.WriteFile(target, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".update.json", []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := resumeFixture(t, target).Resume(context.Background())
	if err == nil {
		t.Fatal("Resume skipped an update that was in flight")
	}
	if !strings.Contains(err.Error(), "journal") {
		t.Errorf("err = %v, want the corrupt journal keel found", err)
	}
}
