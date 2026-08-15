package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
)

// The root of trust for self-update.
//
// keel/update accepts a manifest signed by any of the public keys compiled into the
// consuming binary, and a household's copy has no way to be told about a new one
// except by being replaced. Several keys may be trusted at once so that rotation
// works: ship a build trusting the old and the new key, start signing with the new
// one, drop the old key a release later. See docs/RELEASING.md.
//
// releaseTrustedKeys is empty in the repository on purpose. A key pasted here is a
// key that has been generated, and generating one is a decision with custody
// consequences (docs/RELEASING.md: the signing key never goes on a build machine or
// into CI). Until one exists, `kenward update` refuses rather than fetching anything:
// an updater with no trusted key cannot verify a signature, and an unverified update
// is remote code execution with extra steps.
//
// Both are overridable at link time so a release build can carry its own without this
// file being edited:
//
//	-X main.releaseManifestURL=https://... \
//	-X main.releaseTrustedKeys=BASE64KEY1,BASE64KEY2
var (
	releaseManifestURL = "https://github.com/BlueHeisenberg/kenward/releases/latest/download/manifest.json"
	releaseTrustedKeys = ""
)

// trustedKeys decodes the compiled-in release public keys.
func trustedKeys() ([]ed25519.PublicKey, error) {
	return parseTrustedKeys(releaseTrustedKeys)
}

// parseTrustedKeys decodes a comma-separated list of base64 Ed25519 public keys,
// which is the form `kenward-release keygen` prints for pasting into the source of
// the binary that trusts it.
//
// It takes the list as an argument rather than reading the package variable so that
// it can be tested without writing to a global. That is not tidiness: the variable is
// read by every parallel test that touches the update path, and a test that assigned
// to it was a real data race the race detector caught.
func parseTrustedKeys(list string) ([]ed25519.PublicKey, error) {
	raw := strings.TrimSpace(list)
	if raw == "" {
		return nil, nil
	}
	var keys []ed25519.PublicKey
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(field)
		if err != nil {
			return nil, fmt.Errorf("a compiled-in release key is not valid base64")
		}
		if len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("a compiled-in release key is %d bytes, not an Ed25519 public key", len(b))
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	return keys, nil
}
