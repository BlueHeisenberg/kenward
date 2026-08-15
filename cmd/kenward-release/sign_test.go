package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/keel/update"
)

// signed signs manifestPath with each key in turn, adding a signature every
// time, and returns the path of the signed envelope.
func signed(t *testing.T, manifestPath string, keys ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "signed.json")
	in := manifestPath
	for _, key := range keys {
		mustExec(t, "sign", "--key", key, "--in", in, "--out", out)
		in = out
	}
	return out
}

func readEnvelope(t *testing.T, path string) update.Envelope {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env, err := update.ParseEnvelope(data)
	if err != nil {
		t.Fatalf("%s is not an envelope: %v", path, err)
	}
	return env
}

func TestSignThenVerifyRoundTrips(t *testing.T) {
	priv, pub := newKey(t, "")
	manifestPath, digests := buildManifest(t)
	sig := signed(t, manifestPath, priv)

	code, out, errb := exec(t, "verify", "--in", sig, "--pub", pub)
	if code != 0 {
		t.Fatalf("verify failed: exit %d\n%s%s", code, out, errb)
	}
	for _, want := range []string{
		"VERIFIED", "v0.2.0", "channel edge", "channel stable",
		"security-sensitive   no", "cloud provider", "linux/amd64", "windows/amd64",
		digests["linux/amd64"], digests["windows/amd64"],
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verify output is missing %q:\n%s", want, out)
		}
	}
	if errb != "" {
		t.Errorf("verify wrote to stderr on success: %q", errb)
	}
}

func TestVerifyShoutsAboutSecuritySensitive(t *testing.T) {
	priv, pub := newKey(t, "")
	manifestPath, _ := buildManifest(t, "--security-sensitive")
	out, _ := mustExec(t, "verify", "--in", signed(t, manifestPath, priv), "--pub", pub)
	if !strings.Contains(out, "security-sensitive   YES") {
		t.Errorf("a security-sensitive release is not obvious in the output:\n%s", out)
	}
}

func TestASecondSignaturePreservesTheFirst(t *testing.T) {
	oldPriv, oldPub := newKey(t, "old")
	newPriv, newPub := newKey(t, "new")
	manifestPath, _ := buildManifest(t)

	once := signed(t, manifestPath, oldPriv)
	first := readEnvelope(t, once)

	twice := signed(t, manifestPath, oldPriv, newPriv)
	both := readEnvelope(t, twice)

	if len(both.Signatures) != 2 {
		t.Fatalf("signatures = %d, want 2", len(both.Signatures))
	}
	if string(both.Payload) != string(first.Payload) {
		t.Error("adding a signature changed the payload, which would invalidate the first signature")
	}
	if string(both.Signatures[0].Sig) != string(first.Signatures[0].Sig) {
		t.Error("the original signature was not preserved byte for byte")
	}
	if both.Signatures[0].KeyID != "old" || both.Signatures[1].KeyID != "new" {
		t.Errorf("key ids = %q, %q; want old then new", both.Signatures[0].KeyID, both.Signatures[1].KeyID)
	}

	// Both keys verify it on their own — that is what lets a release be
	// trusted by installations that know only one of them.
	for _, pub := range []string{oldPub, newPub} {
		if code, _, errb := exec(t, "verify", "--in", twice, "--pub", pub); code != 0 {
			t.Errorf("verify with %s failed: %s", pub, errb)
		}
	}
	out, _ := mustExec(t, "verify", "--in", twice, "--pub", oldPub, "--pub", newPub)
	if !strings.Contains(out, "2 of 2 keys signed it") {
		t.Errorf("verify does not report both keys:\n%s", out)
	}
}

func TestSignRefusesToSignTwiceWithOneKey(t *testing.T) {
	priv, _ := newKey(t, "")
	manifestPath, _ := buildManifest(t)
	once := signed(t, manifestPath, priv)

	code, _, errb := exec(t, "sign", "--key", priv, "--in", once, "--out", once)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb, "already signed") {
		t.Errorf("stderr does not explain:\n%s", errb)
	}
	if n := len(readEnvelope(t, once).Signatures); n != 1 {
		t.Errorf("signatures = %d, want the file left alone with 1", n)
	}
}

func TestSignRefusesRubbish(t *testing.T) {
	priv, _ := newKey(t, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.json")

	notJSON := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notJSON, []byte("release notes, not a manifest"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyManifest := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyManifest, []byte(`{"schema":1,"channels":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	noDigest := filepath.Join(dir, "nodigest.json")
	if err := os.WriteFile(noDigest, []byte(
		`{"schema":1,"channels":{"edge":{"version":"v1.0.0","artifacts":{"linux/amd64":{"url":"https://x/y"}}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wrongSchema := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(wrongSchema, []byte(
		`{"schema":7,"channels":{"edge":{"version":"v1.0.0","artifacts":{"linux/amd64":{"url":"https://x/y","sha256":"`+
			strings.Repeat("a", 64)+`"}}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, in := range []string{notJSON, emptyManifest, noDigest, wrongSchema} {
		if code, _, _ := exec(t, "sign", "--key", priv, "--in", in, "--out", out); code == 0 {
			t.Errorf("%s: signed something that should have been refused", filepath.Base(in))
		}
	}
}

// A payload this tooling did not format still takes a second signature, and
// the signature already on it stays valid: the payload bytes are signed and
// re-encoded verbatim, never re-serialised from the decoded manifest.
func TestASignatureCanBeAddedToAForeignPayload(t *testing.T) {
	firstPriv, firstPub := newKey(t, "first")
	secondPriv, secondPub := newKey(t, "second")

	// An envelope somebody else produced: same manifest, indented payload.
	manifestPath, _ := buildManifest(t)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m update.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	payload, err := json.MarshalIndent(m, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	key, id, err := readPrivateKey(firstPriv, nil)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := update.SignPayload(payload, key, id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := update.Envelope{Payload: payload, Signatures: []update.Signature{sig}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign.json")
	if err := os.WriteFile(foreign, data, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "countersigned.json")
	mustExec(t, "sign", "--key", secondPriv, "--in", foreign, "--out", out)

	env := readEnvelope(t, out)
	if string(env.Payload) != string(payload) {
		t.Error("the payload was re-encoded; the first signature would no longer verify")
	}
	for _, pub := range []string{firstPub, secondPub} {
		if code, _, errb := exec(t, "verify", "--in", out, "--pub", pub); code != 0 {
			t.Errorf("verify with %s failed: %s", pub, errb)
		}
	}
}

// keel's ParseEnvelope refuses an envelope carrying a structurally malformed
// signature, and this tool inherits that refusal rather than repairing it:
// the remedy is to sign the payload again.
func TestSignDoesNotRepairAMalformedSignature(t *testing.T) {
	priv, _ := newKey(t, "one")
	manifestPath, _ := buildManifest(t)
	sig := signed(t, manifestPath, priv)

	env := readEnvelope(t, sig)
	env.Signatures[0].Sig = env.Signatures[0].Sig[:16] // truncated
	data, err := json.Marshal(env)                     // deliberately bypassing Encode's care
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, data, 0o644); err != nil {
		t.Fatal(err)
	}

	other, _ := newKey(t, "two")
	if code, _, _ := exec(t, "sign", "--key", other, "--in", broken, "--out", filepath.Join(t.TempDir(), "o.json")); code == 0 {
		t.Fatal("added a signature to an envelope carrying a malformed one")
	}
}

// The rotation procedure in docs/RELEASING.md, end to end at the CLI: two
// keys, one manifest, signed by each in turn, and every installation trusting
// either key alone accepts the result. Without this, a rotation strands
// whichever half of the fleet knows only one of the keys.
func TestKeyRotationFlow(t *testing.T) {
	oldPriv, oldPub := newKey(t, "2026a")
	newPriv, newPub := newKey(t, "2026b")

	manifestPath, _ := buildManifest(t)
	release := filepath.Join(t.TempDir(), "signed.json")
	mustExec(t, "sign", "--key", oldPriv, "--in", manifestPath, "--out", release)
	mustExec(t, "sign", "--key", newPriv, "--in", release, "--out", release)

	for _, tc := range []struct{ name, pub, id string }{
		{"an installation that only knows the old key", oldPub, "2026a"},
		{"an installation that only knows the new key", newPub, "2026b"},
	} {
		code, out, errb := exec(t, "verify", "--in", release, "--pub", tc.pub)
		if code != 0 {
			t.Errorf("%s: exit %d\n%s", tc.name, code, errb)
			continue
		}
		if !strings.Contains(out, "1 of 1 keys signed it, 2 signatures in the envelope") {
			t.Errorf("%s: verify does not report one of two signatures:\n%s", tc.name, out)
		}
		if !strings.Contains(out, "✓ "+tc.id) {
			t.Errorf("%s: verify does not attribute the signature to %s:\n%s", tc.name, tc.id, out)
		}
	}
}

func TestSignRejectsAKeyFileThatIsNotOne(t *testing.T) {
	dir := t.TempDir()
	notAKey := filepath.Join(dir, "release-x.key")
	if err := os.WriteFile(notAKey, []byte("-----BEGIN NONSENSE-----\nzzzz\n-----END NONSENSE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath, _ := buildManifest(t)
	code, _, _ := exec(t, "sign", "--key", notAKey, "--in", manifestPath, "--out", filepath.Join(dir, "o.json"))
	if code == 0 {
		t.Fatal("signed with a file that is not a key")
	}
}
