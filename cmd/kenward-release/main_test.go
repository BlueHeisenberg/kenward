package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exec runs the tool exactly as main would, capturing both streams and the
// exit code, without starting a process.
func exec(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func mustExec(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	code, out, errb := exec(t, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d\nstdout:\n%s\nstderr:\n%s", args, code, out, errb)
	}
	return out, errb
}

// writeDist creates a dist directory holding one fake build per platform,
// each with distinct contents, and returns it with the digests it should
// produce.
func writeDist(t *testing.T, platforms ...string) (dir string, digests map[string]string) {
	t.Helper()
	if len(platforms) == 0 {
		platforms = defaultPlatforms
	}
	dir = t.TempDir()
	digests = map[string]string{}
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		name := "kenward_" + goos + "_" + goarch
		if goos == "windows" {
			name += ".exe"
		}
		content := []byte("fake build for " + p + "\n")
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		digests[p] = hex.EncodeToString(sum[:])
	}
	return dir, digests
}

// newKey generates a keypair in a temp dir and returns the two paths.
func newKey(t *testing.T, id string) (privPath, pubPath string) {
	t.Helper()
	dir := t.TempDir()
	args := []string{"keygen", "--out", dir}
	if id != "" {
		args = append(args, "--id", id)
	}
	out, _ := mustExec(t, args...)
	priv, pub := findPaths(t, dir)
	if !strings.Contains(out, priv) {
		t.Errorf("keygen stdout does not name the private key path\n%s", out)
	}
	return priv, pub
}

func findPaths(t *testing.T, dir string) (privPath, pubPath string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case ".key":
			privPath = filepath.Join(dir, e.Name())
		case ".pub":
			pubPath = filepath.Join(dir, e.Name())
		}
	}
	if privPath == "" || pubPath == "" {
		t.Fatalf("keygen left %v in %s, want a .key and a .pub", entries, dir)
	}
	return privPath, pubPath
}

// buildManifest writes a full manifest for a complete dist directory.
func buildManifest(t *testing.T, extra ...string) (manifestPath string, digests map[string]string) {
	t.Helper()
	dist, digests := writeDist(t)
	manifestPath = filepath.Join(t.TempDir(), "manifest.json")
	args := []string{
		"manifest",
		"--version", "v0.2.0",
		"--channel", "edge,stable",
		"--dist", dist,
		"--notes", "kenward now asks before it uses a cloud provider.",
		"--out", manifestPath,
		"--published-at", "2026-08-15T09:00:00Z",
	}
	mustExec(t, append(args, extra...)...)
	return manifestPath, digests
}

func TestNoCommandIsAUsageError(t *testing.T) {
	code, _, errb := exec(t)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb, "Usage:") {
		t.Errorf("stderr does not show usage:\n%s", errb)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	code, _, _ := exec(t, "publish")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestEveryCommandExplainsItself(t *testing.T) {
	for _, cmd := range []string{"keygen", "manifest", "sign", "verify", "version"} {
		code, out, errb := exec(t, cmd, "-h")
		if code != 0 {
			t.Errorf("%s -h: exit = %d, want 0 (stderr: %s)", cmd, code, errb)
		}
		if !strings.Contains(out, "kenward-release "+cmd) {
			t.Errorf("%s -h: help does not start with the command line:\n%s", cmd, out)
		}
		if len(out) < 120 {
			t.Errorf("%s -h: help is too thin to be useful at the end of a release:\n%s", cmd, out)
		}
	}
}

func TestVersionGoesToStdout(t *testing.T) {
	out, errb := mustExec(t, "version")
	if !strings.HasPrefix(out, "kenward-release ") {
		t.Errorf("version stdout = %q", out)
	}
	if errb != "" {
		t.Errorf("version wrote to stderr: %q", errb)
	}
}

func TestMissingRequiredFlagIsExitTwo(t *testing.T) {
	cases := [][]string{
		{"keygen"},
		{"manifest", "--version", "v1.0.0"},
		{"sign", "--in", "x.json"},
		{"verify", "--in", "x.json"},
		{"verify", "--pub", "x.pub"},
	}
	for _, args := range cases {
		code, _, _ := exec(t, args...)
		if code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
	}
}
