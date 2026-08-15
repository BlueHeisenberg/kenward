package assistant

import (
	"context"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// The rendered system prompt is a product surface specified in docs/PROMPT.md.
// These goldens pin it for both scope kinds; changing the prompt is a deliberate
// fixture edit made with -update.

func TestRenderedPromptDirectGolden(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["david-private"] = []memory.Entry{
		entry("david-private", "Coffee order", "David drinks oat-milk flat whites.", "validated"),
		entry("david-private", "Dentist", "Appointment on the first Monday of October.", "provisional", "PROVISIONAL"),
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		entry("household", "Bin day", "Bins go out Thursday night.\nRecycling alternates weekly.", "hardened", "UPDATED", "CONTEXT"),
	}

	if err := rig.unit.Handle(context.Background(), directInbound("when is my dentist appointment?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("router never called")
	}
	golden(t, "prompt_direct.golden", req.Messages[0].Content)

	// The member's message is the final message, after history.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || last.Content != "when is my dentist appointment?" {
		t.Errorf("final message %+v, want the member's message", last)
	}
}

func TestRenderedPromptGroupGolden(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		entry("household", "Bin day", "Bins go out Thursday night.", "hardened"),
	}

	if err := rig.unit.Handle(context.Background(), groupInbound("bins?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("router never called")
	}
	golden(t, "prompt_group.golden", req.Messages[0].Content)
}

func TestHistoryRendersOldestFirstAndIsBounded(t *testing.T) {
	opts := testOptions()
	opts.HistoryLimit = 2
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"one", "two", "three"} {
		if err := rig.unit.Handle(context.Background(), directInbound(msg)); err != nil {
			t.Fatalf("Handle(%s): %v", msg, err)
		}
	}
	req, _ := rig.router.lastRequest()
	var users []string
	for _, m := range req.Messages[1:] {
		if m.Role == "user" {
			users = append(users, m.Content)
		}
	}
	// The third turn saw the first two turns of history (ring bound 2), oldest
	// first, then its own message.
	want := []string{"one", "two", "three"}
	if strings.Join(users, "|") != strings.Join(want, "|") {
		t.Errorf("user messages %v, want %v", users, want)
	}
	if got := len(rig.unit.history.snapshot()); got != 2 {
		t.Errorf("ring holds %d turns, want the bound of 2", got)
	}
}

func TestRetrievalErrorRendersUnreadableNotEmpty(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.errFor["household"] = context.DeadlineExceeded

	if err := rig.unit.Handle(context.Background(), directInbound("bins?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	req, _ := rig.router.lastRequest()
	sys := req.Messages[0].Content
	if !strings.Contains(sys, unreadableGroupText) {
		t.Error("failed retrieval not disclosed as unreadable")
	}
	// The failure must not masquerade as an honest empty result for that space.
	shared := sys[strings.Index(sys, "## From the household's shared memory"):]
	if strings.Contains(strings.Split(shared, "\n\n")[0], emptyGroupText) {
		t.Error("failed retrieval rendered as (nothing relevant found)")
	}
}
