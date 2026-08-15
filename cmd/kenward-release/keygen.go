package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
)

const keygenUsage = `kenward-release keygen --out DIR [--id ID]

Creates a new Ed25519 release signing keypair in DIR: release-<id>.key, which
is the private half and is written at mode 0600, and release-<id>.pub, which
is the public half you compile into the binary. The id is derived from the key
itself and travels in every manifest, so a verifier knows which key to check.

An existing key file is never overwritten. There is no flag to force it.

Keep the private half offline — removable media or a hardware token — and back
it up somewhere that will not burn down with the machine you sign on. Anyone
holding it can execute code on every household that updates. If you lose it,
existing installations keep working but you can never update them again.

Flags:
  --out DIR   Directory to write the keypair into. Created if missing.
  --id ID     Override the derived key id (letters, digits, '-', '_', '.').
`

func cmdKeygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "directory to write the keypair into")
	id := fs.String("id", "", "override the derived key id")
	if err := parseFlags(fs, keygenUsage, args, stdout, stderr); err != nil {
		return err
	}
	if err := requireFlag("out", *out); err != nil {
		return err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	keyID := *id
	if keyID == "" {
		keyID = keyIDFor(pub)
	}
	if err := validKeyID(keyID); err != nil {
		return &usageError{err: err}
	}

	if err := os.MkdirAll(*out, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", *out, err)
	}
	privPath := privateKeyPath(*out, keyID)
	pubPath := publicKeyPath(*out, keyID)

	// Check the public path first: if it is taken we want to fail before
	// creating a private key file that would then have to be cleaned up,
	// and cleaning up a key file is exactly what this command must never do.
	if _, err := os.Stat(pubPath); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite it", pubPath)
	}
	if err := writePrivateKey(privPath, keyID, priv); err != nil {
		return err
	}
	if err := writePublicKey(pubPath, keyID, pub); err != nil {
		return fmt.Errorf("%w (the private key was written to %s and has been left in place)", err, privPath)
	}

	fmt.Fprintf(stdout, "key id      %s\n", keyID)
	fmt.Fprintf(stdout, "private key %s\n", privPath)
	fmt.Fprintf(stdout, "public key  %s\n", pubPath)
	fmt.Fprintf(stdout, "public key (base64, for the trusted set compiled into kenward):\n  %s\n",
		base64.StdEncoding.EncodeToString(pub))

	fmt.Fprint(stderr, "\nThe private key is the root of trust for every installation.\n"+
		"Move it offline, back it up outside this machine's failure domain, and\n"+
		"never put it on a build machine or in CI. See docs/RELEASING.md.\n")
	return nil
}
