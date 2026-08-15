//go:build integration

package memory

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// This file is the only test that needs a real lore. Everything else runs
// against a scripted fake, so `go test ./...` never requires lore to be
// installed. Run it with:
//
//	KENWARD_LORE_BIN=/path/to/lore \
//	KENWARD_LORE_HOME=/path/to/an/initialised/lore/home \
//	KENWARD_LORE_SPACE=<space_id> \
//	go test -tags integration ./internal/memory/...
//
// It exists to catch the two things a fake cannot: that lore's actual output
// still matches the golden corpus, and that the MCP handshake this SDK performs
// is one lore's much older server library answers. The SDK tries the stateless
// server/discover request before falling back to the legacy initialize
// handshake; lore is built on mark3labs/mcp-go v0.20.1, which predates it.
func TestIntegrationRoundTrip(t *testing.T) {
	bin := os.Getenv("KENWARD_LORE_BIN")
	home := os.Getenv("KENWARD_LORE_HOME")
	space := domain.SpaceID(os.Getenv("KENWARD_LORE_SPACE"))
	if bin == "" || home == "" || space == "" {
		t.Skip("set KENWARD_LORE_BIN, KENWARD_LORE_HOME and KENWARD_LORE_SPACE")
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
