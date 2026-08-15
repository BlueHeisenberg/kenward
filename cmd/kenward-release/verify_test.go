package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tamper rewrites the manifest inside a signed envelope, leaving the
// signature untouched — exactly what a hostile update host would do.
func tamper(t *testing.T, sigPath string, edit func(payload []byte) []byte) string {
	t.Helper()
	env := readEnvelope(t, sigPath)
	env.Payload = edit(env.Payload)
	data, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "tampered.json")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerifyRefusesATamperedPayload(t *testing.T) {
	priv, pub := newKey(t, "")
	manifestPath, _ := buildManifest(t)
	sig := signed(t, manifestPath, priv)

	bad := tamper(t, sig, func(p []byte) []byte {
		return []byte(strings.Replace(string(p), `"version":"v0.2.0"`, `"version":"v9.9.9"`, 1))
	})
	code, out, errb := exec(t, "verify", "--in", bad, "--pub", pub)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if strings.Contains(out, "VERIFIED") {
		t.Errorf("stdout claims a tampered manifest verified:\n%s", out)
	}
	if !strings.Contains(errb, "signature") {
		t.Errorf("stderr does not blame the signature:\n%s", errb)
	}
}

func TestVerifyRefusesATamperedDigest(t *testing.T) {
	priv, pub := newKey(t, "")
	manifestPath, digests := buildManifest(t)
	sig := signed(t, manifestPath, priv)

	want := digests["linux/amd64"]
	evil := strings.Repeat("0", 64)
	bad := tamper(t, sig, func(p []byte) []byte {
		return []byte(strings.Replace(string(p), want, evil, 1))
	})
	if code, _, _ := exec(t, "verify", "--in", bad, "--pub", pub); code != 1 {
		t.Errorf("exit = %d, want 1: a swapped artifact digest must not verify", code)
	}
}

func TestVerifyRefusesAnUnknownKey(t *testing.T) {
	priv, _ := newKey(t, "signer")
	_, strangerPub := newKey(t, "stranger")
	manifestPath, _ := buildManifest(t)
	sig := signed(t, manifestPath, priv)

	code, out, _ := exec(t, "verify", "--in", sig, "--pub", strangerPub)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if strings.Contains(out, "VERIFIED") {
		t.Errorf("stdout claims verification against a key that did not sign:\n%s", out)
	}
}

// During a rotation both keys are passed; the one that did not sign is
// reported, not treated as a failure.
func TestVerifyReportsAKeyThatDidNotSign(t *testing.T) {
	priv, pub := newKey(t, "signer")
	_, otherPub := newKey(t, "future")
	manifestPath, _ := buildManifest(t)
	sig := signed(t, manifestPath, priv)

	out, _ := mustExec(t, "verify", "--in", sig, "--pub", pub, "--pub", otherPub)
	if !strings.Contains(out, "1 of 2 keys signed it") {
		t.Errorf("verify does not report the split:\n%s", out)
	}
	if !strings.Contains(out, "did not sign this manifest") {
		t.Errorf("verify does not name the key that did not sign:\n%s", out)
	}
}

func TestVerifyRefusesAnUnsignedManifest(t *testing.T) {
	_, pub := newKey(t, "")
	manifestPath, _ := buildManifest(t)
	if code, _, _ := exec(t, "verify", "--in", manifestPath, "--pub", pub); code != 1 {
		t.Errorf("exit = %d, want 1: an unsigned manifest is not publishable", code)
	}
}

func TestVerifyRejectsAPublicKeyFileThatIsNotOne(t *testing.T) {
	priv, _ := newKey(t, "")
	manifestPath, _ := buildManifest(t)
	sig := signed(t, manifestPath, priv)

	notAKey := filepath.Join(t.TempDir(), "not.pub")
	if err := os.WriteFile(notAKey, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := exec(t, "verify", "--in", sig, "--pub", notAKey); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

// The public key is also handed around as the base64 line keygen prints for
// embedding in the binary; verify accepts that form too.
func TestVerifyAcceptsABase64PublicKey(t *testing.T) {
	priv, pub := newKey(t, "")
	manifestPath, _ := buildManifest(t)
	sig := signed(t, manifestPath, priv)

	key, _, err := readPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	b64 := filepath.Join(t.TempDir(), "key.b64")
	if err := os.WriteFile(b64, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := exec(t, "verify", "--in", sig, "--pub", b64); code != 0 {
		t.Errorf("exit = %d, want 0: %s", code, errb)
	}
}
