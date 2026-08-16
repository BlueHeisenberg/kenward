package main

import (
	"log/slog"
	"os"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// TestHostSupervisorGivesEveryPodTheHouseholdConfiguration.
//
// D-022: isolated mode has two deployment paths and both are supported, and keel's
// sandbox gained a command vector and create-time file provisioning precisely so the
// supervisor path could express what the compose path already does. The library half
// landed — supervisor.IsolatedOptions.ConfigFile provisions the household file at
// PodConfigPath and starts each pod with the compose-identical argv — and
// internal/supervisor's own tests cover it.
//
// This binary never set the field. `cmdRun` resolved the path into
// runOptions.configPath and nothing read it, so every supervisor-started pod fell back
// to the image's CMD (`run --config /etc/kenward/kenward.yaml --data-dir
// /var/lib/kenward`, see the Dockerfile) against a path this deployment path never
// provisions: the pod exited on a missing configuration and was restarted forever, and
// the decision held nowhere but in a library test. That gap is here, at the wiring,
// which is why this test is here too.
func TestHostSupervisorGivesEveryPodTheHouseholdConfiguration(t *testing.T) {
	t.Parallel()
	h := newHarness(t, isolatedYAML, fullEnvironment())

	// The path is taken from the command rather than assumed: the field the host
	// supervisor needs is the one cmdRun already fills in, and a test that built its
	// own would not notice if it stopped agreeing with the one `run` resolves.
	var got runOptions
	h.e.supervisors = func(_ *env, _ *config.Config, opts runOptions, _ *slog.Logger) (supervisor.Supervisor, error) {
		got = opts
		return stubSupervisor{}, nil
	}
	if code := h.run("run"); code != exitOK {
		t.Fatalf("run exited %d, want 0\n%s", code, h.both())
	}
	if got.configPath != h.config {
		t.Fatalf("run resolved configPath = %q, want %q", got.configPath, h.config)
	}

	got.image = "ghcr.io/blueheisenberg/kenward:test"
	iso, err := isolatedOptions(h.e, got, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("wiring the host supervisor: %v", err)
	}
	if iso.ConfigFile == "" {
		t.Fatalf("IsolatedOptions.ConfigFile is empty, so no pod is given the household\n" +
			"configuration and none is started with the compose-identical argv. Every pod\n" +
			"runs the image's own CMD against /etc/kenward/kenward.yaml, which nothing on\n" +
			"this path provisions, and crash-loops on a missing configuration.")
	}
	if iso.ConfigFile != got.configPath {
		t.Fatalf("IsolatedOptions.ConfigFile = %q, want the configuration run is serving, %q",
			iso.ConfigFile, got.configPath)
	}

	// And it is that household's file, not merely a path shaped like one: what pods
	// receive at supervisor.PodConfigPath is these bytes.
	b, err := os.ReadFile(iso.ConfigFile)
	if err != nil {
		t.Fatalf("reading what the pods would be given: %v", err)
	}
	if string(b) != isolatedYAML {
		t.Fatalf("the file pods would be given is not the household configuration:\n%s", b)
	}
	h.assertNoSecrets(t)
}

// TestHostSupervisorPassesTheConfigPathThroughUnchanged.
//
// Unlike bot_token_file, which internal/supervisor refuses unless it is absolute
// because a relative path names nothing determinate inside a pod, this path is only
// ever read on the host — pods are always started against supervisor.PodConfigPath. So
// whatever resolveConfigPath returned is handed over verbatim, and a relative
// ./kenward.yaml resolves against the same working directory loadConfig just read it
// from. Absolutising it here would be a second answer to a question already settled.
func TestHostSupervisorPassesTheConfigPathThroughUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, isolatedYAML, fullEnvironment())

	iso, err := isolatedOptions(h.e, runOptions{
		configPath: "kenward.yaml",
		image:      "ghcr.io/blueheisenberg/kenward:test",
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("wiring the host supervisor: %v", err)
	}
	if iso.ConfigFile != "kenward.yaml" {
		t.Errorf("ConfigFile = %q, want the resolved path unchanged, %q", iso.ConfigFile, "kenward.yaml")
	}
}
