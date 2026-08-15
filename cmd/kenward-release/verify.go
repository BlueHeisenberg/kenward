package main

import (
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BlueHeisenberg/keel/update"
)

const verifyUsage = `kenward-release verify --in FILE --pub FILE [--pub FILE ...]

Checks a signed manifest with the same code the updater runs, then prints
everything it says: which of the keys you gave it signed the manifest, and for
each channel the version, the publication time, whether it is flagged as
security-sensitive, the notes, and every artifact with its digest.

Run this on the file you are about to publish and read the output. Everything
below the signature line is what every household will act on.

A key that did not sign is reported, not an error: during a key rotation you
are expected to pass both the old and the new one. Exit is non-zero only if
NO key signed the manifest.

Flags:
  --in FILE    The signed envelope to check.
  --pub FILE   A public key to check against; repeat for more than one.
`

func cmdVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	in := fs.String("in", "", "signed envelope to check")
	var pubs stringList
	fs.Var(&pubs, "pub", "public key to check against (repeatable)")
	if err := parseFlags(fs, verifyUsage, args, stdout, stderr); err != nil {
		return err
	}
	if err := requireFlag("in", *in); err != nil {
		return err
	}
	if len(pubs) == 0 {
		return usagef("--pub is required: verifying against no key verifies nothing")
	}

	data, err := os.ReadFile(*in)
	if err != nil {
		return fmt.Errorf("read --in: %w", err)
	}

	type keyResult struct {
		path   string
		id     string
		signed bool
	}
	results := make([]keyResult, 0, len(pubs))
	trusted := make([]ed25519.PublicKey, 0, len(pubs))
	for _, path := range pubs {
		pub, id, err := readPublicKey(path)
		if err != nil {
			return &usageError{err: err}
		}
		trusted = append(trusted, pub)
		// keel's verifier reports only "some trusted key signed this", so
		// ask it once per key to find out which ones did.
		_, err = update.VerifyManifest(data, []ed25519.PublicKey{pub})
		results = append(results, keyResult{path: path, id: id, signed: err == nil})
	}

	m, err := update.VerifyManifest(data, trusted)
	if err != nil {
		return err
	}

	// Key ids as declared in the envelope. They are advisory — keel tries
	// every trusted key regardless — but a mislabelled one is worth seeing.
	var declared []string
	var env envelope
	if err := json.Unmarshal(data, &env); err == nil {
		for _, s := range env.Signatures {
			if s.KeyID != "" {
				declared = append(declared, s.KeyID)
			}
		}
	}

	signedCount := 0
	for _, r := range results {
		if r.signed {
			signedCount++
		}
	}

	fmt.Fprintf(stdout, "VERIFIED  %s  (%d of %d keys signed it, %d %s in the envelope)\n",
		*in, signedCount, len(results), len(env.Signatures), plural(len(env.Signatures), "signature", "signatures"))
	fmt.Fprintf(stdout, "  schema      %d\n", m.Schema)
	if !m.GeneratedAt.IsZero() {
		fmt.Fprintf(stdout, "  generated   %s\n", m.GeneratedAt.UTC().Format(time.RFC3339))
	}
	if len(declared) > 0 {
		fmt.Fprintf(stdout, "  key ids     %s\n", strings.Join(declared, ", "))
	}
	fmt.Fprintln(stdout)

	fmt.Fprintln(stdout, "keys")
	for _, r := range results {
		mark, note := "x", "  did not sign this manifest"
		if r.signed {
			mark, note = "*", ""
		}
		fmt.Fprintf(stdout, "  %s %-10s %s%s\n", mark, r.id, r.path, note)
	}

	channels := make([]string, 0, len(m.Channels))
	for name := range m.Channels {
		channels = append(channels, name)
	}
	sort.Strings(channels)
	for _, name := range channels {
		printRelease(stdout, name, m.Channels[name])
	}
	return nil
}

func printRelease(w io.Writer, channel string, rel update.Release) {
	fmt.Fprintf(w, "\nchannel %s\n", channel)
	fmt.Fprintf(w, "  version              %s\n", rel.Version)
	if !rel.PublishedAt.IsZero() {
		fmt.Fprintf(w, "  published            %s\n", rel.PublishedAt.UTC().Format(time.RFC3339))
	}
	if rel.SecuritySensitive {
		fmt.Fprintf(w, "  security-sensitive   YES — no household applies this without being asked first\n")
	} else {
		fmt.Fprintf(w, "  security-sensitive   no — this applies on its own, without asking\n")
	}
	notes := strings.TrimRight(rel.Notes, "\n")
	if notes == "" {
		fmt.Fprintf(w, "  notes                (none)\n")
	} else {
		for i, line := range strings.Split(notes, "\n") {
			label := "  notes               "
			if i > 0 {
				label = "                      "
			}
			fmt.Fprintf(w, "%s %s\n", label, line)
		}
	}

	platforms := make([]string, 0, len(rel.Artifacts))
	for p := range rel.Artifacts {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	fmt.Fprintf(w, "  artifacts (%d)\n", len(platforms))
	for _, p := range platforms {
		art := rel.Artifacts[p]
		fmt.Fprintf(w, "    %-14s %s  %9d  %s\n", p, art.SHA256, art.Size, art.URL)
	}
}
