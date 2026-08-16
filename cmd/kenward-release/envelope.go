package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BlueHeisenberg/keel/update"
)

// isHex64 reports whether s is exactly 64 hexadecimal characters — the shape
// of a SHA-256 digest. Length alone is not enough: keel compares the digest it
// computed against this string, so 64 characters that are not hex can never
// match anything, and the update simply never applies. That failure is silent
// on every installation at once, which is exactly the class of mistake this
// function exists to catch before a manifest is signed.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// inputKind distinguishes the two things a manifest file on disk can be: the
// unsigned payload the manifest command writes, or the signed envelope that
// gets published. sign accepts either.
type inputKind int

const (
	kindUnknown inputKind = iota
	kindManifest
	kindEnvelope
)

func classify(data []byte) (inputKind, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return kindUnknown, fmt.Errorf("this is not JSON: %w", err)
	}
	_, hasPayload := probe["payload"]
	_, hasSigs := probe["signatures"]
	if hasPayload || hasSigs {
		return kindEnvelope, nil
	}
	if _, ok := probe["channels"]; ok {
		return kindManifest, nil
	}
	return kindUnknown, fmt.Errorf("this is neither an unsigned manifest nor a signed envelope")
}

// checkManifest refuses payloads that would verify but be useless: an
// installation that fetches one of these gets no update and no explanation.
// It is much cheaper to fail here than after publishing.
func checkManifest(m update.Manifest) error {
	if m.Schema != update.ManifestSchema {
		return fmt.Errorf("manifest schema is %d; kenward only understands %d and refuses the rest", m.Schema, update.ManifestSchema)
	}
	if len(m.Channels) == 0 {
		return fmt.Errorf("manifest has no channels")
	}
	for name, rel := range m.Channels {
		if rel.Version == "" {
			return fmt.Errorf("channel %q has no version", name)
		}
		if _, err := update.ParseVersion(rel.Version); err != nil {
			return fmt.Errorf("channel %q: %w", name, err)
		}
		if len(rel.Artifacts) == 0 {
			return fmt.Errorf("channel %q has no artifacts", name)
		}
		for platform, art := range rel.Artifacts {
			if art.URL == "" {
				return fmt.Errorf("channel %q, %s: artifact has no URL", name, platform)
			}
			// A signature makes the digest authoritative, so a foreign host
			// cannot substitute content — but it can still watch. Over plain
			// HTTP the exact version every household downloads is readable by
			// anyone on the path, and a mistyped --base-url is the likeliest
			// way to get there. Signing is the last moment this is cheap to
			// catch: once published, every installation acts on it.
			if !strings.HasPrefix(art.URL, "https://") {
				return fmt.Errorf("channel %q, %s: artifact URL %q is not https — "+
					"a release manifest may only name https artifact URLs", name, platform, art.URL)
			}
			if !isHex64(art.SHA256) {
				return fmt.Errorf("channel %q, %s: sha256 %q is not a 64-character hex digest", name, platform, art.SHA256)
			}
		}
	}
	return nil
}
