package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Key files are PEM-wrapped PKCS#8 (private) and PKIX (public) DER, under
// kenward-specific PEM types. Standard DER means the key material is
// recoverable with ordinary tooling if this program ever goes missing; the
// distinct PEM type means nobody mistakes a release signing key for a TLS
// key and hands it to a web server.
const (
	privatePEMType = "KENWARD RELEASE PRIVATE KEY"
	publicPEMType  = "KENWARD RELEASE PUBLIC KEY"

	// keyIDHeader records the key id inside the file so that a renamed file
	// still signs under the id the manifest will carry. When it is absent
	// the id is derived from the key material instead, which yields the
	// same value keygen would have.
	keyIDHeader = "key-id"
)

// keyIDFor derives a short, stable identifier from a public key: the first
// four bytes of its SHA-256, in hex. It is stable because it is a function of
// the key alone, and short because a human types it and reads it in a
// manifest. It is advisory — keel's verifier tries every trusted key
// regardless — so a collision cannot cause a wrong accept.
func keyIDFor(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:4])
}

// validKeyID rejects ids that would produce a surprising filename or an
// awkward manifest entry.
func validKeyID(id string) error {
	if id == "" {
		return fmt.Errorf("key id is empty")
	}
	if len(id) > 32 {
		return fmt.Errorf("key id %q is longer than 32 characters", id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("key id %q contains %q; use letters, digits, '-', '_' or '.'", id, r)
		}
	}
	return nil
}

func privateKeyPath(dir, id string) string { return filepath.Join(dir, "release-"+id+".key") }
func publicKeyPath(dir, id string) string  { return filepath.Join(dir, "release-"+id+".pub") }

// writePrivateKey writes priv to path at mode 0600, and refuses — always,
// with no flag to override — to touch a file that already exists. O_EXCL
// makes that refusal a property of the syscall rather than of a check that
// could race. A signing key silently replaced is a signing key lost, and a
// lost signing key means no existing installation can ever be updated again.
func writePrivateKey(path, id string, priv ed25519.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		// Deliberately does not wrap err: x509 marshal errors have no
		// business being near key material.
		return fmt.Errorf("encoding the private key failed")
	}
	block := &pem.Block{Type: privatePEMType, Headers: map[string]string{keyIDHeader: id}, Bytes: der}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; refusing to overwrite a signing key", path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// O_CREATE honours the mode only when the file is new, and umask can
	// still have taken bits away; make the mode explicit.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set mode 0600 on %s: %w", path, err)
	}
	return nil
}

func writePublicKey(path, id string, pub ed25519.PublicKey) error {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return fmt.Errorf("encode public key: %w", err)
	}
	block := &pem.Block{Type: publicPEMType, Headers: map[string]string{keyIDHeader: id}, Bytes: der}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; refusing to overwrite it", path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Close()
}

// readPrivateKey loads a signing key. Every error it returns describes the
// file, never its contents: an error message is the one place key material
// most easily escapes, because errors get pasted into issues and chat.
func readPrivateKey(path string, warn io.Writer) (ed25519.PrivateKey, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read signing key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, "", fmt.Errorf("%s is not a PEM file", path)
	}
	if block.Type != privatePEMType {
		return nil, "", fmt.Errorf("%s contains a %q block, want %q", path, block.Type, privatePEMType)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("%s does not contain a usable private key", path)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, "", fmt.Errorf("%s holds a %T, but release manifests are signed with Ed25519", path, parsed)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, "", fmt.Errorf("%s holds a malformed Ed25519 private key", path)
	}

	id := strings.TrimSpace(block.Headers[keyIDHeader])
	derived := keyIDFor(priv.Public().(ed25519.PublicKey))
	if id == "" {
		id = derived
	}
	if err := validKeyID(id); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if warn != nil {
		warnIfPermissive(path, warn)
	}
	return priv, id, nil
}

// readPublicKey loads a verification key. It also accepts a bare base64
// Ed25519 public key on a single line, because that is the form pasted into
// the source of the binary that trusts it, and being handed it back is a
// plausible mistake at the end of a release.
func readPublicKey(path string) (ed25519.PublicKey, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read public key: %w", err)
	}
	if block, _ := pem.Decode(data); block != nil {
		if block.Type != publicPEMType {
			return nil, "", fmt.Errorf("%s contains a %q block, want %q", path, block.Type, publicPEMType)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, "", fmt.Errorf("%s does not contain a usable public key: %w", path, err)
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, "", fmt.Errorf("%s holds a %T, but release manifests are signed with Ed25519", path, parsed)
		}
		id := strings.TrimSpace(block.Headers[keyIDHeader])
		if id == "" {
			id = keyIDFor(pub)
		}
		return pub, id, nil
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, "", fmt.Errorf("%s is neither a %q PEM block nor a base64 Ed25519 public key", path, publicPEMType)
	}
	pub := ed25519.PublicKey(raw)
	return pub, keyIDFor(pub), nil
}

// warnIfPermissive says something when a signing key is readable by more than
// its owner. It is a warning, not a refusal: the operator may have a reason,
// and refusing here would strand a release at the worst possible moment.
func warnIfPermissive(path string, warn io.Writer) {
	if runtime.GOOS == "windows" {
		return // Go reports synthetic modes here; the check would only lie.
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		fmt.Fprintf(warn, "warning: %s is mode %04o — a signing key should be 0600\n", path, perm)
	}
}
