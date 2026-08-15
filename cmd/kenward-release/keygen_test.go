package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestKeygenProducesAUsablePair(t *testing.T) {
	dir := t.TempDir()
	out, _ := mustExec(t, "keygen", "--out", dir)

	privPath, pubPath := findPaths(t, dir)
	priv, privID, err := readPrivateKey(privPath, nil)
	if err != nil {
		t.Fatalf("cannot read back the key we just wrote: %v", err)
	}
	pub, pubID, err := readPublicKey(pubPath)
	if err != nil {
		t.Fatalf("cannot read back the public key: %v", err)
	}
	if privID != pubID {
		t.Errorf("key ids disagree: %q vs %q", privID, pubID)
	}
	if !strings.Contains(filepath.Base(privPath), privID) {
		t.Errorf("filename %q does not carry the key id %q", filepath.Base(privPath), privID)
	}
	if !pub.Equal(priv.Public()) {
		t.Error("the .pub file is not the public half of the .key file")
	}

	// The halves actually work together.
	msg := []byte("release manifest")
	if !ed25519.Verify(pub, msg, ed25519.Sign(priv, msg)) {
		t.Error("signature made with the private half does not verify with the public half")
	}

	// The base64 form printed for embedding must be the real key.
	b64 := base64.StdEncoding.EncodeToString(pub)
	if !strings.Contains(out, b64) {
		t.Errorf("keygen did not print the public key for embedding:\n%s", out)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(privPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("private key mode = %04o, want 0600", perm)
		}
	}
}

func TestKeygenNeverOverwritesAKey(t *testing.T) {
	dir := t.TempDir()
	mustExec(t, "keygen", "--out", dir, "--id", "1")
	privPath := privateKeyPath(dir, "1")
	before, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}

	code, _, errb := exec(t, "keygen", "--out", dir, "--id", "1")
	if code == 0 {
		t.Fatal("keygen overwrote an existing key")
	}
	if !strings.Contains(errb, "refusing to overwrite") {
		t.Errorf("stderr does not say why it refused:\n%s", errb)
	}
	after, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the existing signing key was modified")
	}
}

// A stale .pub with no matching .key must not cause a private key to be
// written and then abandoned.
func TestKeygenRefusesWhenOnlyThePublicHalfExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(publicKeyPath(dir, "1"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, _ := exec(t, "keygen", "--out", dir, "--id", "1")
	if code == 0 {
		t.Fatal("keygen proceeded over an existing .pub")
	}
	if _, err := os.Stat(privateKeyPath(dir, "1")); err == nil {
		t.Error("a private key was written on a path that then failed")
	}
}

func TestKeygenRejectsASillyKeyID(t *testing.T) {
	code, _, _ := exec(t, "keygen", "--out", t.TempDir(), "--id", "../escape")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestKeyIDIsStableAndShort(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	id := keyIDFor(pub)
	if id != keyIDFor(pub) {
		t.Error("key id is not stable for one key")
	}
	if len(id) != 8 {
		t.Errorf("key id %q is %d characters; it goes in a filename and a manifest", id, len(id))
	}
	if err := validKeyID(id); err != nil {
		t.Errorf("derived id is not a valid id: %v", err)
	}
}
