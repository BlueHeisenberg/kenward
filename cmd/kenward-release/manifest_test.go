package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/keel/update"
)

func readManifest(t *testing.T, path string) update.Manifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m update.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest is not decodable as update.Manifest: %v", err)
	}
	return m
}

func TestManifestDescribesEveryBuild(t *testing.T) {
	path, digests := buildManifest(t)
	m := readManifest(t, path)

	if m.Schema != update.ManifestSchema {
		t.Errorf("schema = %d, want %d", m.Schema, update.ManifestSchema)
	}
	if len(m.Channels) != 2 {
		t.Fatalf("channels = %v, want edge and stable", m.Channels)
	}
	edge, stable := m.Channels["edge"], m.Channels["stable"]
	if edge.Version != "v0.2.0" || stable.Version != "v0.2.0" {
		t.Errorf("versions = %q / %q", edge.Version, stable.Version)
	}
	if edge.PublishedAt.IsZero() {
		t.Error("no publication timestamp: the stable delay is enforced against it")
	}
	if edge.SecuritySensitive {
		t.Error("securitySensitive set without being asked for")
	}

	for platform, wantDigest := range digests {
		art, ok := edge.Artifacts[platform]
		if !ok {
			t.Errorf("no artifact for %s", platform)
			continue
		}
		if art.SHA256 != wantDigest {
			t.Errorf("%s: sha256 = %s, want %s", platform, art.SHA256, wantDigest)
		}
		if art.Size == 0 {
			t.Errorf("%s: size is zero", platform)
		}
		if !strings.Contains(art.URL, "v0.2.0") || !strings.HasPrefix(art.URL, "https://") {
			t.Errorf("%s: url = %q", platform, art.URL)
		}
	}
	if got := edge.Artifacts["windows/amd64"]; !strings.HasSuffix(got.URL, ".exe") {
		t.Errorf("windows artifact url = %q, want the .exe", got.URL)
	}
}

func TestManifestRefusesToStrandAPlatform(t *testing.T) {
	dist, _ := writeDist(t, "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64")
	out := filepath.Join(t.TempDir(), "manifest.json")
	code, _, errb := exec(t, "manifest",
		"--version", "v0.2.0", "--channel", "stable", "--dist", dist,
		"--notes", "x", "--out", out)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb, "windows/amd64") {
		t.Errorf("stderr does not name the missing platform:\n%s", errb)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a manifest was written despite the missing platform")
	}
}

func TestManifestSecuritySensitiveIsCarriedAndAnnounced(t *testing.T) {
	path, _ := buildManifest(t, "--security-sensitive")
	m := readManifest(t, path)
	for name, rel := range m.Channels {
		if !rel.SecuritySensitive {
			t.Errorf("channel %s: securitySensitive not set", name)
		}
	}
}

func TestManifestRejectsBadInput(t *testing.T) {
	dist, _ := writeDist(t)
	out := filepath.Join(t.TempDir(), "m.json")
	base := []string{"manifest", "--dist", dist, "--notes", "x", "--out", out}
	cases := map[string][]string{
		"bad version":      {"--version", "point two", "--channel", "edge"},
		"unknown channel":  {"--version", "v0.2.0", "--channel", "nightly"},
		"channel off":      {"--version", "v0.2.0", "--channel", "off"},
		"bad published-at": {"--version", "v0.2.0", "--channel", "edge", "--published-at", "yesterday"},
	}
	for name, extra := range cases {
		code, _, _ := exec(t, append(append([]string{}, base...), extra...)...)
		if code != 2 {
			t.Errorf("%s: exit = %d, want 2", name, code)
		}
	}
}

func TestManifestIgnoresFilesThatAreNotBuilds(t *testing.T) {
	dist, _ := writeDist(t)
	for _, junk := range []string{"checksums.txt", "kenward_linux_amd64.sha256", "README"} {
		if err := os.WriteFile(filepath.Join(dist, junk), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "m.json")
	mustExec(t, "manifest", "--version", "v0.2.0", "--channel", "edge",
		"--dist", dist, "--notes", "x", "--out", out)
	m := readManifest(t, out)
	if got := len(m.Channels["edge"].Artifacts); got != len(defaultPlatforms) {
		t.Errorf("artifacts = %d, want %d", got, len(defaultPlatforms))
	}
}

func TestPlatformFromFilename(t *testing.T) {
	cases := map[string]string{
		"kenward_linux_amd64":       "linux/amd64",
		"kenward_darwin_arm64":      "darwin/arm64",
		"kenward_windows_amd64.exe": "windows/amd64",
		"kenward-release_linux_386": "linux/386",

		"kenward_windows_amd64":        "", // windows build without .exe
		"kenward_linux_amd64.exe":      "", // unix build with one
		"kenward_linux_amd64.sha256":   "",
		"kenward_solaris_amd64":        "",
		"kenward_linux_pentium":        "",
		"kenward":                      "",
		"checksums.txt":                "",
		"kenward_linux_amd64.tar.gz":   "",
		"kenward_v0.2.0_linux_amd64":   "linux/amd64",
		"kenward_v020_windows_386.exe": "windows/386",
	}
	for name, want := range cases {
		got, ok := platformFromFilename(name)
		if want == "" && ok {
			t.Errorf("%s: recognised as %s, want ignored", name, got)
		}
		if want != "" && got != want {
			t.Errorf("%s: got %q (%v), want %q", name, got, ok, want)
		}
	}
}

func TestArtifactURLTemplating(t *testing.T) {
	if got := artifactURL(defaultBaseURL, "v0.2.0", "kenward_linux_amd64"); !strings.HasSuffix(got, "/v0.2.0/kenward_linux_amd64") {
		t.Errorf("templated url = %q", got)
	}
	if got := artifactURL("https://example.test/dl/", "v0.2.0", "kenward_linux_amd64"); got != "https://example.test/dl/kenward_linux_amd64" {
		t.Errorf("prefix url = %q", got)
	}
}
