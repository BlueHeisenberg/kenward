//go:build integration

package memory

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// This file is the only test in this package that needs a real lore. Everything
// else runs against a scripted fake, so `go test ./...` never requires lore to be
// installed. Run it with:
//
//	KENWARD_LORE_BIN=/path/to/lore \
//	go test -tags integration ./internal/memory/...
//
// The store is not configured. This test used to be pointed at a home and a space
// kept for the purpose, and it wrote an entry into that space on every run and
// removed nothing, because lore has no delete — see newLoreStore in
// internal/e2e/live_test.go for the same defect and the same reasoning. It now
// initialises a home under t.TempDir() and creates its own space there.
//
// It exists to catch the two things a fake cannot: that lore's actual output
// still matches the golden corpus, and that the MCP handshake this SDK performs
// is one lore's much older server library answers. The SDK tries the stateless
// server/discover request before falling back to the legacy initialize
// handshake; lore is built on mark3labs/mcp-go v0.20.1, which predates it.
func TestIntegrationRoundTrip(t *testing.T) {
	bin := os.Getenv("KENWARD_LORE_BIN")
	if bin == "" {
		t.Skip("set KENWARD_LORE_BIN to the lore binary; the store it runs against is created by this test")
	}
	home := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "LORE_HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("lore %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("init", "--yes-i-saved-it", "--name", "kenward-integration")
	var space domain.SpaceID
	for _, f := range strings.Fields(run("space", "create", "kenward-integration")) {
		if len(f) == 36 && strings.Count(f, "-") == 4 {
			space = domain.SpaceID(f)
			break
		}
	}
	if space == "" {
		t.Fatal("`lore space create` printed no space id")
	}

	c, err := NewClient(Config{
		Command:  bin,
		Args:     []string{"mcp"},
		LoreHome: home,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The listing is what a setup wizard picks from, so it has to carry the id
	// to configure and the kind: a personal space cannot serve as a household's
	// shared memory, and lore's own enum is the only thing that says which is
	// which.
	spaces, err := c.Spaces(ctx)
	if err != nil {
		t.Fatalf("Spaces: %v", err)
	}
	var listed, personal int
	for _, s := range spaces {
		if s.ID == "" || s.Name == "" || (s.Kind != "personal" && s.Kind != "shared") {
			t.Errorf("incomplete space row: %+v", s)
		}
		if domain.SpaceID(s.ID) == space {
			listed++
		}
		if s.Kind == "personal" {
			personal++
		}
	}
	if listed != 1 {
		t.Errorf("the space under test appears %d times in the listing", listed)
	}
	if personal == 0 {
		t.Error("a lore home always holds a personal space; none was reported")
	}

	body := "Written by kenward's integration test at " + time.Now().UTC().Format(time.RFC3339Nano) + "."
	put, err := c.Put(ctx, space, Draft{
		Domain:     "kenward/integration",
		Title:      "kenward integration probe",
		Body:       body,
		Confidence: "experimental",
		Markers:    []string{"CONTEXT"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if put.ID == "" || put.Space != space {
		t.Fatalf("unexpected stored entry: %+v", put)
	}
	if put.UpdatedAt.IsZero() {
		t.Errorf("the read-back should carry lore's updated_at: %+v", put)
	}
	if len(put.Markers) != 1 || put.Markers[0] != "[CONTEXT]" {
		t.Errorf("lore should have normalised the marker, got %v", put.Markers)
	}

	got, err := c.Get(ctx, space, put.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != body {
		t.Errorf("body round-trip:\n got %q\nwant %q", got.Body, body)
	}

	found, err := c.SearchExcerpts(ctx, SearchQuery{
		Text:   "kenward integration probe",
		Spaces: []domain.SpaceID{space},
		Limit:  5,
	})
	if err != nil {
		t.Fatalf("SearchExcerpts: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the entry just written was not found by search")
	}
	for _, x := range found {
		if x.Entry.Space != space {
			t.Errorf("search result in the wrong space: %+v", x.Entry)
		}
		if !IsExcerpt(x.Entry) {
			t.Errorf("a real lore search hit must still report as partial: %+v", x.Entry)
		}
	}
}
