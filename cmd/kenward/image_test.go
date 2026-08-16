package main

import (
	"os"
	"strings"
	"testing"
)

// Two things the image's own comments have to say, because nothing else in this module
// will ever say them and both failures are silent.
//
// A Dockerfile is not compiled, so these are checked the way
// TestComposeIsolatedCommandsAreSomethingThisBinaryRuns checks the compose file: by
// reading it. The subject is the prose rather than the instructions, and deliberately so
// — in both cases the instruction is right and the note beside it is what leaves an
// operator debugging an error that names no cause.

func readDockerfile(t *testing.T) string {
	t.Helper()
	const path = "../../Dockerfile"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestDockerfileLoreNoteSaysStaticallyLinked.
//
// The note tells operators to supply a `lore` binary matching the image's platform. That
// is necessary and not sufficient: the final stage is gcr.io/distroless/static-debian12,
// which carries no dynamic loader, so a stock `go build -o lore ./cmd/lore` produces a
// binary that matches the platform, is executable, and dies as
//
//	exec /usr/local/bin/lore: no such file or directory
//
// naming a file that is plainly there. Measured both ways: default build → dynamically
// linked → that error; CGO_ENABLED=0 → static → works. The compose files explain the
// SELinux `z` flag at length for exactly this reason — a failure that names no cause is
// the one worth a line of prose — and this is the same class of trap.
func TestDockerfileLoreNoteSaysStaticallyLinked(t *testing.T) {
	doc := readDockerfile(t)
	note, _, ok := strings.Cut(doc, "# ---- builder ----")
	if !ok {
		t.Fatal("the Dockerfile no longer has a header block before the builder stage")
	}
	if !strings.Contains(note, "lore") {
		t.Fatal("the header block no longer mentions lore; this test has stopped checking anything")
	}
	if !strings.Contains(note, "CGO_ENABLED=0") {
		t.Errorf("the lore note does not say the binary has to be statically linked, so a "+
			"correct-platform build still dies as `exec /usr/local/bin/lore: no such file or "+
			"directory`:\n%s", note)
	}
}

// TestDockerfileIsHonestAboutHEALTHCHECKOnPodman.
//
// `podman build` defaults to the OCI image format, which has nowhere to put a
// HEALTHCHECK, and says so three times during a build of this file:
//
//	HEALTHCHECK is not supported for OCI image format and will be ignored
//
// Podman is one of the two runtimes isolated mode supports, so an instruction that
// silently does nothing on half the supported surface has to say so — and say what to do
// instead, which is `--format docker` at build time or the `healthcheck:` the compose
// files now declare themselves.
func TestDockerfileIsHonestAboutHEALTHCHECKOnPodman(t *testing.T) {
	doc := readDockerfile(t)
	// The instruction, not the several mentions of it in the prose above.
	i := strings.Index(doc, "\nHEALTHCHECK ")
	if i < 0 {
		t.Fatal("the Dockerfile no longer declares a HEALTHCHECK")
	}
	// The prose above the instruction, which is where the caveat belongs: somebody
	// reading the instruction has already scrolled past it.
	note := doc[:i]
	if !strings.Contains(note, "podman") && !strings.Contains(note, "PODMAN") {
		t.Errorf("nothing beside HEALTHCHECK says it is dropped on podman, which is half of "+
			"the runtimes isolated mode supports:\n%s", tail(note))
	}
	if !strings.Contains(note, "--format docker") {
		t.Errorf("the HEALTHCHECK note names no way to keep it on podman:\n%s", tail(note))
	}
}

// TestComposeFilesDeclareTheirOwnHealthcheck: the other half of the same fix. A compose
// deployment on podman gets no healthcheck from the image, so the file that starts the
// container declares one.
func TestComposeFilesDeclareTheirOwnHealthcheck(t *testing.T) {
	for _, path := range []string{"../../deploy/compose.simple.yml", "../../deploy/compose.isolated.yml"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		text := string(b)
		services := strings.Count(text, "image: ghcr.io/blueheisenberg/kenward:")
		if services == 0 {
			t.Fatalf("%s names no kenward service; this test has stopped checking anything", path)
		}
		if got := strings.Count(text, "healthcheck:"); got != services {
			t.Errorf("%s: %d services, %d healthchecks; podman drops the image's own, so every "+
				"service needs one here", path, services, got)
		}
	}
}

// tail is the last few lines of a block, for an error message that would otherwise carry
// the whole file.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return strings.Join(lines, "\n")
}
