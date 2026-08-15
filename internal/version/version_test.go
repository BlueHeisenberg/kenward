package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestFull(t *testing.T) {
	orig := Version
	origCommit := Commit
	defer func() {
		Version = orig
		Commit = origCommit
	}()

	Version = "v0.1.0"
	Commit = "abc1234"

	full := Full()

	for _, want := range []string{
		Version,
		Commit,
		runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(full, want) {
			t.Errorf("Full() = %q, want it to contain %q", full, want)
		}
	}
}

func TestShort(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	Version = "v1.2.3"
	if got := Short(); got != "v1.2.3" {
		t.Errorf("Short() = %q, want %q", got, "v1.2.3")
	}
}
