package main

import (
	"strings"
	"testing"

	keelupdate "github.com/BlueHeisenberg/keel/update"
)

func keelVersion(major, minor, patch int) keelupdate.Version {
	return keelupdate.Version{Major: major, Minor: minor, Patch: patch}
}

// TestUpdateSaysSoWhenTheChannelIsOff.
//
// docs/CLI.md: prints the channel in use, and says so plainly when it is off.
// `channel: off` is a fully supported way to run kenward forever, not a degraded
// state waiting to be fixed, and the wording has to read that way.
func TestUpdateSaysSoWhenTheChannelIsOff(t *testing.T) {
	t.Parallel()
	off := simpleYAML + "update:\n  channel: off\n"
	h := newHarness(t, off, fullEnvironment())

	if code := h.run("update"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	out := h.stdout()
	if !strings.Contains(out, "Update channel: off") {
		t.Errorf("update does not print the channel:\n%s", out)
	}
	for _, want := range []string{"never fetch anything", "works indefinitely"} {
		if !strings.Contains(out, want) {
			t.Errorf("update does not say off is supported (%q):\n%s", want, out)
		}
	}
}

// TestUpdateCheckPrintsTheChannel: --check reports and changes nothing.
//
// This build has no release signing keys compiled in, so it refuses before touching
// the network — which is the correct behaviour and is what this asserts. An updater
// that cannot verify a signature must refuse rather than fetch: it is a remote code
// execution channel into somebody's house.
func TestUpdateRefusesWithoutTrustedKeys(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(releaseTrustedKeys) != "" {
		t.Skip("this build has release keys compiled in")
	}
	h := newHarness(t, simpleYAML, fullEnvironment())

	if code := h.run("update", "--check"); code != exitFailure {
		t.Fatalf("exit = %d, want %d\n%s", code, exitFailure, h.both())
	}
	if !strings.Contains(h.stdout(), "Update channel: stable") {
		t.Errorf("update did not print the channel before refusing:\n%s", h.stdout())
	}
	for _, want := range []string{"no release signing keys", "cannot verify", "Refusing"} {
		if !strings.Contains(h.stderr(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, h.stderr())
		}
	}
}

// TestTrustedKeysRejectsRubbish. A key that is not a key is a build fault, not
// something to fetch a manifest with anyway.
func TestTrustedKeysRejectsRubbish(t *testing.T) {
	t.Parallel()
	original := releaseTrustedKeys
	t.Cleanup(func() { releaseTrustedKeys = original })

	releaseTrustedKeys = "not base64 at all!!"
	if _, err := trustedKeys(); err == nil {
		t.Error("a non-base64 key was accepted")
	}

	releaseTrustedKeys = "dG9vIHNob3J0" // valid base64, wrong length
	if _, err := trustedKeys(); err == nil {
		t.Error("a key of the wrong length was accepted")
	}

	releaseTrustedKeys = ""
	keys, err := trustedKeys()
	if err != nil || len(keys) != 0 {
		t.Errorf("an empty key list should be no keys and no error; got %v, %v", len(keys), err)
	}
}

// TestConsentDeclinesWhenNobodyIsThere.
//
// A major version bump, or a release flagged as changing security-relevant defaults,
// needs a human to agree. When there is nobody at the terminal the answer is no: a
// release that may move routing or privacy defaults must not slip through because
// nothing was listening.
func TestConsentDeclinesWhenNobodyIsThere(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stdin string
		want  bool
	}{
		{"empty input", "", false},
		{"no", "n\n", false},
		{"anything else", "maybe\n", false},
		{"yes", "y\n", true},
		{"yes spelled out", "YES\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, simpleYAML, fullEnvironment())
			h.e.stdin = strings.NewReader(tc.stdin)
			ok, err := consentPrompt(h.e)(h.e.context(), keelVersion(1, 0, 0), keelVersion(2, 0, 0), "notes")
			if err != nil {
				t.Fatalf("consent returned an error: %v", err)
			}
			if ok != tc.want {
				t.Errorf("consent = %v, want %v", ok, tc.want)
			}
			if !strings.Contains(h.stdout(), "needs your agreement") {
				t.Errorf("the prompt does not explain itself:\n%s", h.stdout())
			}
		})
	}
}
