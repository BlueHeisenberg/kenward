package main

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/version"
)

// TestDispatchExitCodes covers argument handling for every command.
//
// docs/CLI.md's conventions: 0 success, 1 runtime failure, 2 configuration or usage
// error. Getting these wrong is not cosmetic — the container's HEALTHCHECK and any
// script an operator writes read nothing else.
func TestDispatchExitCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, exitUsage},
		{"unknown command", []string{"summon"}, exitUsage},
		{"help", []string{"help"}, exitOK},
		{"--help", []string{"--help"}, exitOK},
		{"version", []string{"version"}, exitOK},
		{"version --json", []string{"version", "--json"}, exitOK},
		{"version with an argument", []string{"version", "extra"}, exitUsage},
		{"version with an unknown flag", []string{"version", "--verbose"}, exitUsage},
		{"doctor --json on a missing file", []string{"doctor", "--config", "no/such/file.yaml"}, exitUsage},
		{"invite without --name", []string{"invite"}, exitUsage},
		{"invite with a negative ttl", []string{"invite", "--name", "David", "--ttl", "-1h"}, exitUsage},
		{"revoke without a member", []string{"revoke"}, exitUsage},
		{"revoke with two members", []string{"revoke", "david", "jordan"}, exitUsage},
		{"run with a positional argument", []string{"run", "wat"}, exitUsage},
		{"doctor with a positional argument", []string{"doctor", "wat"}, exitUsage},
		{"update with a positional argument", []string{"update", "wat"}, exitUsage},
		{"setup with a positional argument", []string{"setup", "wat"}, exitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, simpleYAML, fullEnvironment())
			if got := dispatch(h.e, tc.args); got != tc.want {
				t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", got, tc.want, h.stdout(), h.stderr())
			}
		})
	}
}

// TestErrorsGoToStderr: docs/CLI.md keeps stdout for anything a script might parse.
func TestErrorsGoToStderr(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	dispatch(h.e, []string{"summon"})
	if h.stdout() != "" {
		t.Errorf("an unknown command wrote to stdout:\n%s", h.stdout())
	}
	if !strings.Contains(h.stderr(), "unknown command") {
		t.Errorf("stderr does not name the problem:\n%s", h.stderr())
	}
}

// TestVersionOutput checks both forms carry the build.
func TestVersionOutput(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	if code := dispatch(h.e, []string{"version"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	line := strings.TrimSpace(h.stdout())
	if line != version.Full() {
		t.Errorf("version printed %q, want internal/version's %q", line, version.Full())
	}
	if strings.Count(line, "\n") != 0 {
		t.Errorf("version is not one line: %q", line)
	}

	h2 := newHarness(t, simpleYAML, fullEnvironment())
	if code := dispatch(h2.e, []string{"version", "--json"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var rep versionReport
	if err := json.Unmarshal(h2.out.Bytes(), &rep); err != nil {
		t.Fatalf("--json did not emit JSON: %v", err)
	}
	if rep.Version != version.Version || rep.Commit != version.Commit {
		t.Errorf("--json does not reuse internal/version: %+v", rep)
	}
	if rep.Go == "" || rep.Platform == "" {
		t.Errorf("--json is missing the runtime: %+v", rep)
	}
}

// TestJSONOnlyOnDoctorAndVersion: docs/CLI.md says --json exists on those two and
// nowhere else, because there is nothing to parse in the rest.
func TestJSONOnlyOnDoctorAndVersion(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{"run", "setup", "invite", "revoke", "update"} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, simpleYAML, fullEnvironment())
			if code := h.run(cmd, "--json"); code != exitUsage {
				t.Errorf("%s --json exited %d; --json is for doctor and version only", cmd, code)
			}
		})
	}
}

// TestNoCommandPrintsASecret is the conventions check, run across the whole surface.
//
// The configuration names a bot token variable and an API key variable, the fake
// environment holds values for both, and no command may put either on a terminal, in
// a log line or in an error.
func TestNoCommandPrintsASecret(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		yaml string
		args []string
	}{
		{"doctor", simpleYAML, []string{"doctor"}},
		{"doctor --json", simpleYAML, []string{"doctor", "--json"}},
		{"doctor isolated", isolatedYAML, []string{"doctor"}},
		{"doctor with everything down", simpleYAML, []string{"doctor"}},
		{"run", simpleYAML, []string{"run"}},
		{"run isolated pod", isolatedYAML, []string{"run", "--member", "david"}},
		{"invite", simpleYAML, []string{"invite", "--name", "David"}},
		{"revoke", simpleYAML, []string{"revoke", "david"}},
		{"update", simpleYAML, []string{"update", "--check"}},
		{"version", simpleYAML, []string{"version"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			h.e.probes = telegramRefuses(loreDown(everythingAsleep(healthyProbes())))
			h.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
				return stubSupervisor{}, nil
			}
			h.run(tc.args...)
			h.assertNoSecrets(t)
		})
	}
}

// TestScrubTokenRemovesItFromAnError guards the one place a bot token most easily
// escapes: net/http quotes the whole URL, and the Bot API puts the token in the path.
func TestScrubTokenRemovesItFromAnError(t *testing.T) {
	t.Parallel()
	err := scrubToken(
		errTest("Get \"https://api.telegram.org/bot"+fakeBotToken+"/getMe\": dial tcp: no route to host"),
		fakeBotToken)
	if strings.Contains(err.Error(), fakeBotToken) {
		t.Fatalf("the token survived scrubbing: %v", err)
	}
	if !strings.Contains(err.Error(), "<bot token>") {
		t.Errorf("the error does not say something was removed: %v", err)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
