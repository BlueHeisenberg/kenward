package assistant

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files with current output")

// golden compares got against testdata/name byte for byte. Refusal strings and
// rendered prompts are product surface; changing one is a deliberate edit to the
// fixture, made with -update, never an incidental diff.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s (run with -update to create it): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("output does not match golden %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
