package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/BlueHeisenberg/keel/update"
)

const signUsage = `kenward-release sign --key FILE --in FILE --out FILE

Signs a manifest with a release signing key and writes the signed envelope.
That envelope is what you publish; kenward refuses anything else.

--in takes either an unsigned manifest from "kenward-release manifest", or an
already-signed envelope, in which case this ADDS a signature instead of
replacing the one that is there. That is what makes key rotation work: ship a
release trusting both keys, signed with the old one, wait for it to propagate,
then start signing with the new one.

Sign on the machine that holds the key. Never in CI: a CI system that can sign
is a CI system that can push code to every household, which is the exact
property the signature exists to deny.

Flags:
  --key FILE   The private half of a release keypair (release-<id>.key).
  --in FILE    Manifest to sign, or signed envelope to add a signature to.
  --out FILE   Where to write the signed envelope; "-" for stdout.
`

func cmdSign(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "private half of a release keypair")
	in := fs.String("in", "", "manifest or signed envelope to sign")
	out := fs.String("out", "", `where to write the signed envelope, or "-" for stdout`)
	if err := parseFlags(fs, signUsage, args, stdout, stderr); err != nil {
		return err
	}
	for _, required := range []struct{ name, value string }{{"key", *keyPath}, {"in", *in}, {"out", *out}} {
		if err := requireFlag(required.name, required.value); err != nil {
			return err
		}
	}

	priv, keyID, err := readPrivateKey(*keyPath, stderr)
	if err != nil {
		return err
	}
	signer := update.Signer{KeyID: keyID, Key: priv}

	data, err := os.ReadFile(*in)
	if err != nil {
		return fmt.Errorf("read --in: %w", err)
	}
	kind, err := classify(data)
	if err != nil {
		return fmt.Errorf("%s: %w", *in, err)
	}

	var signed []byte
	var total int
	switch kind {
	case kindManifest:
		var m update.Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("%s: %w", *in, err)
		}
		if err := checkManifest(m); err != nil {
			return fmt.Errorf("%s: %w", *in, err)
		}
		if signed, err = update.SignManifest(m, signer); err != nil {
			return err
		}
		total = 1
	case kindEnvelope:
		if signed, total, err = addSignature(data, signer); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s: unrecognised file", *in)
	}

	if err := writeOut(*out, append(signed, '\n'), stdout); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "signed with key %s (%d %s on this manifest)\n",
		keyID, total, plural(total, "signature", "signatures"))
	return nil
}

// addSignature adds signer's signature to an already-signed envelope, leaving
// every existing signature intact and byte-identical.
//
// The signature covers exact payload bytes, not a canonical form of the
// manifest, so the existing signatures stay valid only if the payload is
// preserved verbatim. keel does not expose a way to sign given bytes, so the
// new signature is obtained by re-signing the decoded manifest and then
// checking that keel produced the very same payload; if it did not, the file
// was not written by this tooling and re-wrapping it would invalidate the
// signature already on it. Refusing is the only safe answer there.
func addSignature(data []byte, signer update.Signer) (out []byte, total int, err error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, 0, fmt.Errorf("parse signed envelope: %w", err)
	}
	if len(env.Payload) == 0 {
		return nil, 0, fmt.Errorf("signed envelope has an empty payload")
	}
	var m update.Manifest
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		return nil, 0, fmt.Errorf("decode manifest inside the envelope: %w", err)
	}
	if err := checkManifest(m); err != nil {
		return nil, 0, err
	}

	fresh, err := update.SignManifest(m, signer)
	if err != nil {
		return nil, 0, err
	}
	var freshEnv envelope
	if err := json.Unmarshal(fresh, &freshEnv); err != nil {
		return nil, 0, fmt.Errorf("decode freshly signed envelope: %w", err)
	}
	if !bytes.Equal(freshEnv.Payload, env.Payload) {
		return nil, 0, fmt.Errorf("cannot add a signature: the payload in this envelope is not " +
			"byte-identical to the one this tooling would produce, so adding a signature would " +
			"invalidate the signature already on it. Rebuild the manifest and sign it with each key in turn")
	}
	if len(freshEnv.Signatures) != 1 {
		return nil, 0, fmt.Errorf("expected exactly one new signature, got %d", len(freshEnv.Signatures))
	}
	add := freshEnv.Signatures[0]

	for _, existing := range env.Signatures {
		// Ed25519 is deterministic, so an identical signature means this
		// key has already signed this exact payload.
		if bytes.Equal(existing.Sig, add.Sig) {
			return nil, 0, usagef("this manifest is already signed by key %s", signer.KeyID)
		}
		if existing.KeyID != "" && existing.KeyID == add.KeyID {
			return nil, 0, usagef("this manifest already carries a different signature labelled %s", add.KeyID)
		}
	}
	env.Signatures = append(env.Signatures, add)

	out, err = json.Marshal(env)
	if err != nil {
		return nil, 0, fmt.Errorf("encode signed envelope: %w", err)
	}
	return out, len(env.Signatures), nil
}
