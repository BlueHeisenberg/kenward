package main

import (
	"encoding/json"
	"fmt"

	"github.com/BlueHeisenberg/keel/update"
)

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
			if len(art.SHA256) != 64 {
				return fmt.Errorf("channel %q, %s: sha256 is not a 64-character hex digest", name, platform)
			}
		}
	}
	return nil
}
