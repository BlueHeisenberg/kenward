//go:build integration && linux

package supervisor

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// TestIsolatedRealPodman exercises the isolated supervisor against a real Podman.
// It needs KENWARD_TEST_IMAGE set to an image whose entrypoint tolerates the
// KENWARD_MEMBER / KENWARD_GROUP environment (any long-running image will do for
// lifecycle purposes) and a Podman socket the current user can reach.
func TestIsolatedRealPodman(t *testing.T) {
	image := os.Getenv("KENWARD_TEST_IMAGE")
	if image == "" {
		t.Skip("KENWARD_TEST_IMAGE not set; skipping real-Podman test")
	}

	cfg := &config.Config{
		Mode: config.ModeIsolated,
		Household: config.HouseholdConfig{
			Name: "IT", SharedSpace: "household",
		},
		Members: []config.MemberConfig{
			{ID: "it-member", Name: "IT", TelegramID: 1, PrivateSpace: "p", Tiers: []string{"local"}, BotTokenEnv: "KENWARD_IT_TOKEN"},
		},
	}
	sup, err := NewIsolated(cfg, IsolatedOptions{
		Image:        image,
		NamePrefix:   "kenward-it",
		PollInterval: time.Second,
		LookupEnv: func(string) (string, bool) {
			return "not-a-real-token", true
		},
	})
	if err != nil {
		t.Fatalf("NewIsolated: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- sup.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		hs, herr := sup.Health(context.Background())
		if herr != nil {
			t.Fatalf("Health: %v", herr)
		}
		if len(hs) == 1 && hs[0].State == StateReady {
			break
		}
		time.Sleep(time.Second)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Minute)
	defer stopCancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
}
