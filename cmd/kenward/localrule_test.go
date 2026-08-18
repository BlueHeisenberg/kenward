package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// TestLocalHostRuleAgreesWithTheWizard is a drift test between two implementations
// of one claim.
//
// privacy.TierNote asks its caller to decide which tiers count as local, because that
// is configuration rather than something the privacy package can know. Two callers
// answer it: internal/setup, when it tells an operator at the end of the wizard where
// each conversation may go, and cmd/kenward, when `doctor` prints the same lines
// later. internal/setup keeps its rule unexported, so this package has its own copy
// in tiers.go.
//
// If the two ever disagree, the wizard and `doctor` make different claims about the
// same kenward.yaml — one telling somebody their private conversations stay in the
// house while the other says they may reach a provider. That is exactly the drift the
// single-source-of-truth arrangement around internal/privacy exists to prevent, and a
// shared constant would not have caught it: the divergence would be in the rule, not
// in the string.
//
// So this runs both. The wizard writes a real configuration and prints its own
// verdict; this package's rule is then run over the file the wizard just wrote, and
// the two verdicts must match. The inputs are the address shapes a household actually
// has, including the ones the two rules are most likely to disagree about.
func TestLocalHostRuleAgreesWithTheWizard(t *testing.T) {
	t.Parallel()
	for _, baseURL := range []string{
		"http://localhost:8000/v1",
		"http://127.0.0.1:8000/v1",
		"http://192.168.1.20:8000/v1",
		"http://10.0.0.5:8000/v1",
		"http://172.16.4.9:8000/v1",
		"http://169.254.10.2:8000/v1",
		"http://host.docker.internal:8000/v1",
		"http://monster:8000/v1",
		"http://monster.local:8000/v1",
		"http://monster.lan:8000/v1",
		"http://monster.home:8000/v1",
		"http://monster.internal:8000/v1",
		"http://monster.home.arpa:8000/v1",
		"http://monster.tail:8000/v1",
		"http://box.ts.net:8000/v1",
		"https://openrouter.ai/api/v1",
		"https://api.openai.com/v1",
		"https://generativelanguage.googleapis.com/v1beta/openai",
		"http://8.8.8.8:8000/v1",
		"http://203.0.113.7:8000/v1",
		"https://monster.example.com/v1",
	} {
		t.Run(baseURL, func(t *testing.T) {
			t.Parallel()
			wizardSaysLocal, configPath := runWizard(t, baseURL)

			// This package's rule, over the very file the wizard wrote.
			cfg, err := loadConfig(configPath, t.TempDir(), testSecrets(map[string]string{
				"KENWARD_BOT_TOKEN": fakeBotToken,
			}))
			if err != nil {
				t.Fatalf("the wizard wrote a configuration this package cannot load: %v", err)
			}
			ourVerdict := staysHome(localTiers(cfg), []string{probeTier})

			if ourVerdict != wizardSaysLocal {
				t.Fatalf("the wizard and cmd/kenward disagree about %s:\n"+
					"  the wizard told the operator it %s\n"+
					"  doctor would tell them it %s\n"+
					"One of them is lying to somebody about where their private conversations go. "+
					"Reconcile hostIsLocal in cmd/kenward/tiers.go with isLocal in internal/setup/probe.go.",
					baseURL, verdictWords(wizardSaysLocal), verdictWords(ourVerdict))
			}
		})
	}
}

// probeTier is the single tier name both sides are asked about, so the comparison is
// about the address and nothing else.
const probeTier = "t"

// runWizard drives the setup wizard non-interactively over one endpoint and reports
// whether it told the operator the member's conversations stay in the house.
//
// The verdict is read out of the transcript rather than computed, because the
// transcript is what the operator was actually told — which is the thing that has to
// match.
func runWizard(t *testing.T, baseURL string) (local bool, configPath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = dir + "/kenward.yaml"

	io := setup.NewScriptIO()
	w := setup.New(io, setup.Options{
		ConfigPath: configPath,
		GOOS:       "linux",
		// No network: the probe's answer is shown to the operator and then
		// accepted either way, and it has nothing to do with the local rule.
		Probe: func(context.Context, string) setup.ProbeResult {
			return setup.ProbeResult{State: setup.NoAnswer, Elapsed: time.Millisecond}
		},
		LookupEnv: lookup(map[string]string{"KENWARD_BOT_TOKEN": fakeBotToken}),
		// The wizard makes the spaces itself, and this test must not let it make
		// them in whatever lore home the machine running it has. Naming ids
		// instead would not do either: the ids would have to exist in that same
		// real store, which is how this test used to pass on one developer's
		// machine and fail in a clean container.
		CreateSpace: func(_ context.Context, name string) (memory.Space, error) {
			return memory.Space{ID: uuid.NewString(), Name: name, Kind: "shared"}, nil
		},
		Answers: &setup.Answers{
			HouseholdName: "Casa",
			MemberNames:   []string{"David"},
			Endpoints: []setup.EndpointAnswer{{
				Name:    "machine",
				BaseURL: baseURL,
				Model:   "qwen3.6-27b-awq",
				Tiers:   []string{probeTier},
			}},
			MemberTiers: map[string][]string{"david": {probeTier}},
			GroupTiers:  []string{probeTier},
		},
	})
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatalf("the wizard failed on %s: %v", baseURL, err)
	}

	transcript := io.Transcript()
	// privacy.TierNote's two endings. Exactly one must appear for the member line.
	const (
		refuses = "David: [" + probeTier + "] — will refuse rather than use a provider"
		mayUse  = "David: [" + probeTier + "] — may use a provider"
	)
	saysRefuses := strings.Contains(transcript, refuses)
	saysMayUse := strings.Contains(transcript, mayUse)
	switch {
	case saysRefuses == saysMayUse:
		t.Fatalf("could not read the wizard's verdict for %s out of its transcript; "+
			"privacy.TierNote's wording may have changed:\n%s", baseURL, transcript)
	}
	return saysRefuses, configPath
}

func verdictWords(local bool) string {
	if local {
		return "stays in the house"
	}
	return "may reach a provider"
}
