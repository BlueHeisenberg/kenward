package main

import (
	"os"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// TestPodCommandIsSomethingThisBinaryRuns puts the command line an isolated pod
// is started with through the real dispatcher.
//
// Both isolated deployment paths hand the container a command, and the image's
// ENTRYPOINT is the bare binary with `run` in its CMD (see the Dockerfile), so a
// command that does not name a subcommand replaces `run` instead of following
// it. Neither path had one. Against a real Podman on a real Linux host every
// supervisor-started pod did this, on every restart, forever:
//
//	$ podman logs sbx-kenward-member-david
//	kenward: unknown command "--config=/etc/kenward/kenward.yaml"
//	(exit 2)
//
// The library layer could not see it: internal/supervisor asserts the argv it
// builds against its own constant, and a fake container backend runs no binary,
// so an argv this binary rejects passed every test in the module. This is the
// layer that decides, so the check lives here.
func TestPodCommandIsSomethingThisBinaryRuns(t *testing.T) {
	for _, unitFlag := range []string{"--member=david", "--group"} {
		assertDispatchable(t, "supervisor pod "+unitFlag, supervisor.PodCommand(unitFlag))
	}
}

// TestComposeIsolatedCommandsAreSomethingThisBinaryRuns does the same for
// deploy/compose.isolated.yml, which writes the same command out by hand for
// every service and was broken in exactly the same way. A compose file is not
// compiled, so nothing else in this module would ever notice.
func TestComposeIsolatedCommandsAreSomethingThisBinaryRuns(t *testing.T) {
	const path = "../../deploy/compose.isolated.yml"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	cmds := composeCommands(string(b))
	if len(cmds) == 0 {
		t.Fatalf("%s: found no `command:` block; this test has stopped checking anything", path)
	}
	for _, cmd := range cmds {
		assertDispatchable(t, path+" "+strings.Join(cmd, " "), cmd)
	}
}

// assertDispatchable requires that dispatch recognises args as a command.
//
// It asserts what this test is about and nothing more: the command NAME is one
// this binary has. The run that follows fails — the configuration path is a
// container's, not this machine's — and that failure is the proof the argument
// reached `run` at all rather than being rejected as a command name.
func assertDispatchable(t *testing.T, what string, args []string) {
	t.Helper()
	h := newHarness(t, isolatedYAML, nil)
	code := dispatch(h.e, args)
	if strings.Contains(h.both(), "unknown command") {
		t.Errorf("%s: kenward does not recognise this as a command (exit %d)\n%s", what, code, h.both())
	}
}

// composeCommands extracts every `command:` list from a compose file as its
// argument vector. A three-line YAML reader beats a dependency for a file whose
// shape is fixed and checked in beside it.
func composeCommands(yaml string) [][]string {
	var out [][]string
	var cur []string
	inCommand := false
	for _, line := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "command:":
			inCommand = true
			cur = nil
		case inCommand && strings.HasPrefix(trimmed, "- "):
			cur = append(cur, strings.Trim(strings.TrimPrefix(trimmed, "- "), `"`))
		case inCommand && !strings.HasPrefix(trimmed, "#") && trimmed != "":
			out = append(out, cur)
			inCommand = false
		}
	}
	if inCommand && cur != nil {
		out = append(out, cur)
	}
	return out
}
