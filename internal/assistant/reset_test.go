package assistant

import (
	"context"
	"strings"
	"testing"
	"time"
)

// clockOptions returns options whose clock a test moves, so a turn can be placed on
// either side of a scheduled boundary. Everything else matches testOptions, including
// the date the prompt renders, which only changes when a test moves the clock across
// a day.
func clockOptions(now *time.Time, reset time.Duration) Options {
	o := testOptions()
	o.HistoryReset = reset
	o.Now = func() time.Time { return *now }
	return o
}

// TestHistoryBoundaryIsAnchoredToMidnight pins the schedule itself.
//
// The anchor is the whole reason a household can predict when this happens: 6h means
// midnight, six, noon and six, on every machine, on every day, whenever the process
// was last started. An anchor at start-up would satisfy "every six hours" and be
// unpredictable, which is the property that actually matters.
//
// The exactly-on-the-boundary row is the one worth stating out loud. A turn arriving
// at precisely 06:00:00 is *at* the new boundary, not before it, so it belongs to the
// interval that starts there and the reset happens on it.
func TestHistoryBoundaryIsAnchoredToMidnight(t *testing.T) {
	day := func(h, m, s int) time.Time {
		return time.Date(2026, time.August, 14, h, m, s, 0, time.UTC)
	}
	for _, tc := range []struct {
		name  string
		now   time.Time
		every time.Duration
		want  time.Time
	}{
		{"just after midnight, six-hourly", day(0, 0, 1), 6 * time.Hour, day(0, 0, 0)},
		{"exactly on a boundary", day(6, 0, 0), 6 * time.Hour, day(6, 0, 0)},
		{"one second before one", day(5, 59, 59), 6 * time.Hour, day(0, 0, 0)},
		{"one second after one", day(6, 0, 1), 6 * time.Hour, day(6, 0, 0)},
		{"last interval of the day", day(23, 59, 59), 6 * time.Hour, day(18, 0, 0)},
		{"daily is midnight", day(13, 20, 0), 24 * time.Hour, day(0, 0, 0)},
		{"daily, exactly midnight", day(0, 0, 0), 24 * time.Hour, day(0, 0, 0)},
		{"an interval that does not divide the day", day(23, 0, 0), 7 * time.Hour, day(21, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := historyBoundary(tc.now, tc.every); !got.Equal(tc.want) {
				t.Errorf("historyBoundary(%s, %s) = %s, want %s",
					tc.now.Format(time.RFC3339), tc.every,
					got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestHistoryIsKeptWhenNoBoundaryPasses is the control. Without it every assertion
// below could pass on a Unit that simply never remembers anything.
func TestHistoryIsKeptWhenNoBoundaryPasses(t *testing.T) {
	now := time.Date(2026, time.August, 14, 1, 0, 0, 0, time.UTC)
	rig, err := newTestRig(fixedResolver(testDirectScope()), clockOptions(&now, 6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), directInbound("the boiler is making a noise")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	now = now.Add(time.Hour)
	if err := rig.unit.Handle(context.Background(), directInbound("is it serious?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := historyTurns(t, rig); got != 1 {
		t.Errorf("the second turn saw %d earlier turns, want 1: no boundary was crossed", got)
	}
	assertNoResetNotice(t, rig)
}

// TestScheduledResetDropsTheHistoryAndSaysSo is the feature.
//
// It asserts the three things that together make the reset honest rather than merely
// implemented: the earlier turns are gone from the next prompt, the member is told in
// the same conversation, and the telling happens before the answer rather than after
// it.
func TestScheduledResetDropsTheHistoryAndSaysSo(t *testing.T) {
	now := time.Date(2026, time.August, 14, 5, 0, 0, 0, time.UTC)
	rig, err := newTestRig(fixedResolver(testDirectScope()), clockOptions(&now, 6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), directInbound("the boiler is making a noise")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := historyTurns(t, rig); got != 0 {
		t.Fatalf("the first turn already saw %d earlier turns", got)
	}

	// Across 06:00, so the next turn is the first of a new interval.
	now = now.Add(2 * time.Hour)
	if err := rig.unit.Handle(context.Background(), directInbound("is it serious?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := historyTurns(t, rig); got != 0 {
		t.Errorf("the turn after the boundary saw %d earlier turns, want 0", got)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 3 {
		t.Fatalf("sent %v, want the first reply, the reset notice and the second reply", texts)
	}
	golden(t, "history_reset.golden", texts[1])
	// The notice arrives before the answer it explains. A member who reads the answer
	// first has already been surprised by it.
	if !strings.Contains(texts[2], "the bins go out on Thursday") {
		t.Errorf("the reply did not follow the notice: %v", texts)
	}
	// And it says the thing it exists to say. A member who reads this and believes
	// the household's memory was touched has been misled by the one line that was
	// meant to stop exactly that.
	if !strings.Contains(strings.ToLower(texts[1]), "memory") {
		t.Error("the reset notice never mentions memory, so it cannot say memory was untouched")
	}
}

// TestResetNoticeIsNotFedBackIntoTheNextPrompt keeps the node's own accounting out of
// the assistant's mouth, exactly as the retrieval line is kept out.
//
// A notice recorded as an assistant turn would be read by the model on the next turn
// as something it had said, and a model that has seen itself announce a reset starts
// announcing them.
func TestResetNoticeIsNotFedBackIntoTheNextPrompt(t *testing.T) {
	now := time.Date(2026, time.August, 14, 5, 0, 0, 0, time.UTC)
	rig, err := newTestRig(fixedResolver(testDirectScope()), clockOptions(&now, 6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), directInbound("first")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if err := rig.unit.Handle(context.Background(), directInbound("second")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := rig.unit.Handle(context.Background(), directInbound("third")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("no request reached the router")
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "Starting fresh") {
			t.Errorf("the reset notice was recorded in history and sent back to the model as a %s message", m.Role)
		}
	}
}

// TestOneNoticePerBoundary guards the obvious way this becomes noise: a member who
// keeps talking after a reset must not be told about it on every message.
func TestOneNoticePerBoundary(t *testing.T) {
	now := time.Date(2026, time.August, 14, 5, 0, 0, 0, time.UTC)
	rig, err := newTestRig(fixedResolver(testDirectScope()), clockOptions(&now, 6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), directInbound("first")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	now = now.Add(2 * time.Hour)
	for _, text := range []string{"second", "third", "fourth"} {
		if err := rig.unit.Handle(context.Background(), directInbound(text)); err != nil {
			t.Fatalf("Handle(%s): %v", text, err)
		}
	}
	if got := countNotices(rig); got != 1 {
		t.Errorf("the member was told about the reset %d times, want once", got)
	}
}

// TestNothingIsAnnouncedWhenThereWasNothingToDrop covers the case a household will
// actually meet most mornings: nobody was talking when the boundary passed.
//
// The boundary is still spent — the alternative is being told, at breakfast, about a
// reset of the empty conversation that a restart had already produced.
func TestNothingIsAnnouncedWhenThereWasNothingToDrop(t *testing.T) {
	now := time.Date(2026, time.August, 14, 9, 0, 0, 0, time.UTC)
	rig, err := newTestRig(fixedResolver(testDirectScope()), clockOptions(&now, 6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The first turn of the process, well past several boundaries. A restart has
	// already forgotten everything, so there is nothing to announce.
	if err := rig.unit.Handle(context.Background(), directInbound("morning")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	assertNoResetNotice(t, rig)
	// And the boundary it adopted was the current one, so the next turn inside the
	// same interval is not a reset either.
	now = now.Add(time.Hour)
	if err := rig.unit.Handle(context.Background(), directInbound("still morning")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	assertNoResetNotice(t, rig)
	if got := historyTurns(t, rig); got != 1 {
		t.Errorf("the second turn saw %d earlier turns, want 1", got)
	}
}

// TestHistoryResetIsOffByDefault is the compatibility guarantee. An existing
// household's conversations must behave exactly as they did before this existed, and
// the zero value is what every configuration written before the key means.
func TestHistoryResetIsOffByDefault(t *testing.T) {
	now := time.Date(2026, time.August, 14, 5, 0, 0, 0, time.UTC)
	opts := clockOptions(&now, 0)
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), directInbound("first")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Three days later, which crosses every boundary any schedule could have.
	now = now.Add(72 * time.Hour)
	if err := rig.unit.Handle(context.Background(), directInbound("second")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := historyTurns(t, rig); got != 1 {
		t.Errorf("the second turn saw %d earlier turns, want 1: nothing may be dropped with the schedule off", got)
	}
	assertNoResetNotice(t, rig)
}

// TestTheGroupConversationIsResetToo states the scope decision as an assertion.
//
// How stale a conversation may get is not a privacy question and has nothing
// per-member to say, so the setting is household-wide and the group chat is not an
// exception. Each unit still keeps its own ring and crosses the boundary on its own
// next message.
func TestTheGroupConversationIsResetToo(t *testing.T) {
	now := time.Date(2026, time.August, 14, 5, 0, 0, 0, time.UTC)
	rig, err := newTestRig(fixedResolver(testGroupScope()), clockOptions(&now, 6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), groupInbound("who is cooking")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if err := rig.unit.Handle(context.Background(), groupInbound("and who is shopping")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := historyTurns(t, rig); got != 0 {
		t.Errorf("the group turn after the boundary saw %d earlier turns, want 0", got)
	}
	if got := countNotices(rig); got != 1 {
		t.Errorf("the group was told about the reset %d times, want once", got)
	}
}

// historyTurns counts the earlier turns the last request carried, by counting the
// assistant messages between the system prompt and the member's own message.
func historyTurns(t *testing.T, rig *testRig) int {
	t.Helper()
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("no request reached the router")
	}
	n := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

func countNotices(rig *testRig) int {
	n := 0
	for _, text := range rig.tr.sentTexts() {
		if text == enCat.ResetNotice {
			n++
		}
	}
	return n
}

func assertNoResetNotice(t *testing.T, rig *testRig) {
	t.Helper()
	if n := countNotices(rig); n != 0 {
		t.Errorf("a reset was announced %d times when none was due: %v", n, rig.tr.sentTexts())
	}
}
