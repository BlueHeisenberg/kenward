package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretForms returns every encoding of a private key that could plausibly
// appear in output: the raw 64-byte key and its 32-byte seed, each as base64,
// hex, and raw bytes, plus the base64 body of the PEM file itself.
func secretForms(t *testing.T, privPath string) []string {
	t.Helper()
	data, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("%s is not PEM", privPath)
	}
	priv, _, err := readPrivateKey(privPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := priv.Seed()

	forms := []string{
		base64.StdEncoding.EncodeToString(priv),
		base64.StdEncoding.EncodeToString(seed),
		base64.RawStdEncoding.EncodeToString(seed),
		base64.URLEncoding.EncodeToString(seed),
		hex.EncodeToString(priv),
		hex.EncodeToString(seed),
		base64.StdEncoding.EncodeToString(block.Bytes),
		string(priv),
		string(seed),
	}
	// The base64 body of the file, line by line: a naive "print the key
	// file we could not parse" bug would show up as one of these.
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); len(line) >= 16 && !strings.HasPrefix(line, "-----") && !strings.Contains(line, ":") {
			forms = append(forms, line)
		}
	}
	return forms
}

func assertNoSecret(t *testing.T, what string, forms []string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if text == "" {
			continue
		}
		for _, secret := range forms {
			if secret == "" {
				continue
			}
			if strings.Contains(text, secret) {
				t.Fatalf("%s leaked private key material", what)
			}
		}
	}
}

// The private key must not appear in stdout, stderr, or an error value of any
// command — including every failure path, which is where key material is most
// likely to escape, because errors get pasted into issues and chat logs.
func TestPrivateKeyNeverAppearsInOutput(t *testing.T) {
	dir := t.TempDir()
	mustExec(t, "keygen", "--out", dir, "--id", "leak")
	privPath, pubPath := findPaths(t, dir)
	forms := secretForms(t, privPath)

	manifestPath, _ := buildManifest(t)
	sigPath := filepath.Join(t.TempDir(), "signed.json")

	// A corrupt key file: the closest analogue here to a wrong passphrase.
	// Nothing it contains may be echoed back.
	corruptDir := t.TempDir()
	corruptPath := filepath.Join(corruptDir, "release-leak.key")
	raw, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	corruptForms := secretForms(t, privPath) // same key material, truncated file

	// A key file that parses but holds the wrong kind of key.
	wrongKind := filepath.Join(corruptDir, "release-rsa.key")
	if err := os.WriteFile(wrongKind, pem.EncodeToMemory(&pem.Block{
		Type: privatePEMType, Bytes: []byte("not DER at all"),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"keygen", []string{"keygen", "--out", t.TempDir()}},
		{"keygen refusing to overwrite", []string{"keygen", "--out", dir, "--id", "leak"}},
		{"sign", []string{"sign", "--key", privPath, "--in", manifestPath, "--out", sigPath}},
		{"sign with a missing input", []string{"sign", "--key", privPath, "--in", filepath.Join(dir, "nope.json"), "--out", sigPath}},
		{"sign with an unwritable output", []string{"sign", "--key", privPath, "--in", manifestPath, "--out", dir}},
		{"sign with a corrupt key", []string{"sign", "--key", corruptPath, "--in", manifestPath, "--out", sigPath}},
		{"sign with a key that is not Ed25519", []string{"sign", "--key", wrongKind, "--in", manifestPath, "--out", sigPath}},
		{"sign with a missing key", []string{"sign", "--key", filepath.Join(dir, "nope.key"), "--in", manifestPath, "--out", sigPath}},
		{"sign the key file itself", []string{"sign", "--key", privPath, "--in", privPath, "--out", sigPath}},
		{"verify the key file itself", []string{"verify", "--in", privPath, "--pub", pubPath}},
		{"verify with the private key as --pub", []string{"verify", "--in", sigPath, "--pub", privPath}},
		{"version", []string{"version"}},
		{"help", []string{"-h"}},
	}
	for _, tc := range cases {
		_, out, errb := exec(t, tc.args...)
		assertNoSecret(t, tc.name+" stdout", forms, out)
		assertNoSecret(t, tc.name+" stderr", forms, errb)
		assertNoSecret(t, tc.name+" stdout", corruptForms, out)
		assertNoSecret(t, tc.name+" stderr", corruptForms, errb)
	}

	// The signed envelope carries a signature and a key id, never the key.
	if data, err := os.ReadFile(sigPath); err == nil {
		assertNoSecret(t, "the signed manifest", forms, string(data))
	}

	// And the error values themselves, not only what was printed.
	if _, _, err := readPrivateKey(corruptPath, nil); err != nil {
		assertNoSecret(t, "readPrivateKey's error", corruptForms, err.Error())
	} else {
		t.Error("a truncated key file parsed")
	}
	if _, _, err := readPrivateKey(wrongKind, nil); err != nil {
		assertNoSecret(t, "readPrivateKey's error", forms, err.Error())
	} else {
		t.Error("a key file holding rubbish parsed")
	}
}

// The public half is not secret, but keygen must actually be printing the key
// it wrote rather than something it invented.
func TestKeygenPrintsThePublicHalfOnly(t *testing.T) {
	dir := t.TempDir()
	out, _ := mustExec(t, "keygen", "--out", dir)
	privPath, pubPath := findPaths(t, dir)
	pub, _, err := readPublicKey(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, base64.StdEncoding.EncodeToString(pub)) {
		t.Error("keygen did not print the public key")
	}
	assertNoSecret(t, "keygen stdout", secretForms(t, privPath), out)
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
}
