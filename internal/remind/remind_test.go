package remind

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// madrid is a real zone with a real daylight-saving transition, used so the wall-clock
// tests are not quietly passing on UTC.
func madrid(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatalf("loading Europe/Madrid (is time/tzdata imported?): %v", err)
	}
	return loc
}

func openTemp(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reminders.json")
	s, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

// add stores one reminder or fails the test.
func add(t *testing.T, s *Store, text string, every Every, hour, minute int, weekday time.Weekday, date string, now time.Time) Reminder {
	t.Helper()
	r, err := New(text, every, hour, minute, weekday, date, 42, now, s.Location())
	if err != nil {
		t.Fatalf("New(%q): %v", text, err)
	}
	stored, err := s.Add(r)
	if err != nil {
		t.Fatalf("Add(%q): %v", text, err)
	}
	return stored
}

// TestMissedOccurrenceWhileTheNodeWasDown is the test the whole design exists for.
//
// The household's machines are usually asleep, so firing against a powered-off node is
// the normal case. The two kinds get deliberately different answers, and this asserts
// both in the same run: a repeating reminder that was missed sixteen times overnight
// produces exactly ONE message and then realigns, and a one-off produces its one
// message however late it is.
func TestMissedOccurrenceWhileTheNodeWasDown(t *testing.T) {
	loc := madrid(t)
	s, _ := openTemp(t, Options{Location: loc, CatchUp: 6 * time.Hour, MaxPerDay: 50})

	// Monday evening: the household sets a reminder for every morning, and a one-off
	// for Tuesday lunchtime. Then the node goes off.
	monEvening := time.Date(2026, time.August, 10, 22, 0, 0, 0, loc)
	daily := add(t, s, "bins", EveryDaily, 7, 30, time.Sunday, "", monEvening)
	once := add(t, s, "dentist", EveryOnce, 13, 0, time.Sunday, "2026-08-11", monEvening)

	// The node comes back on Thursday morning. The daily reminder has been due three
	// times (Tue, Wed, Thu) and the one-off once, three days ago.
	thuMorning := time.Date(2026, time.August, 13, 9, 15, 0, 0, loc)
	fires, skip := s.Due(thuMorning)

	if len(fires) != 2 {
		t.Fatalf("delivered %d messages, want exactly 2 — one per reminder, never one per missed occurrence: %+v", len(fires), fires)
	}

	byID := map[string]Fire{}
	for _, f := range fires {
		byID[f.Reminder.ID] = f
	}

	// The one-off is delivered three days late, because a one-off is a promise to a
	// person and there is only ever one of it.
	d, ok := byID[once.ID]
	if !ok {
		t.Fatal("the one-off was not delivered; a promise dropped silently is the one outcome this must never produce")
	}
	if !d.Late {
		t.Error("the one-off was delivered without being marked late")
	}
	if got := d.Message(loc); !strings.Contains(got, "This was due at 13:00 on Tuesday 11 August") {
		t.Errorf("late message %q does not say when it was due", got)
	}

	// The daily one is delivered once — this morning's occurrence, which is inside the
	// six-hour catch-up window — and Tuesday's and Wednesday's are gone for good.
	b, ok := byID[daily.ID]
	if !ok {
		t.Fatal("this morning's repeating occurrence was dropped although it is inside the catch-up window")
	}
	if want := time.Date(2026, time.August, 13, 7, 30, 0, 0, loc); !b.Due.Equal(want) {
		t.Errorf("delivered occurrence %s, want today's %s — the missed ones must be skipped, not queued", b.Due, want)
	}
	if skip.Missed != 0 || skip.Capped != 0 {
		t.Errorf("skipped %+v, want nothing skipped on this pass", skip)
	}

	// The one-off is gone; the daily one is realigned to tomorrow morning, not to a
	// backlog of yesterdays.
	left := s.List()
	if len(left) != 1 || left[0].ID != daily.ID {
		t.Fatalf("store holds %+v, want only the repeating reminder", left)
	}
	if want := time.Date(2026, time.August, 14, 7, 30, 0, 0, loc); !left[0].Next.Equal(want.UTC()) {
		t.Errorf("next fire %s, want %s", left[0].Next, want.UTC())
	}

	// And a second pass immediately afterwards sends nothing at all.
	if again, _ := s.Due(thuMorning.Add(time.Minute)); len(again) != 0 {
		t.Fatalf("a second pass re-delivered %+v", again)
	}
}

// TestStaleRepeatingOccurrenceIsSkippedNotDelivered. Beyond the catch-up window a
// routine is worthless: last night's bin reminder at lunchtime is noise.
func TestStaleRepeatingOccurrenceIsSkippedNotDelivered(t *testing.T) {
	loc := madrid(t)
	s, _ := openTemp(t, Options{Location: loc, CatchUp: 6 * time.Hour, MaxPerDay: 50})

	mon := time.Date(2026, time.August, 10, 12, 0, 0, 0, loc)
	add(t, s, "bins", EveryDaily, 19, 30, time.Sunday, "", mon)

	// Nineteen hours after it was due — well past the six-hour window.
	late := time.Date(2026, time.August, 11, 14, 30, 0, 0, loc)
	fires, skip := s.Due(late)
	if len(fires) != 0 {
		t.Fatalf("delivered %+v, want nothing: the occurrence is older than the catch-up window", fires)
	}
	if skip.Missed != 1 {
		t.Errorf("skip = %+v, want one missed occurrence recorded", skip)
	}
	if left := s.List(); len(left) != 1 {
		t.Fatalf("store holds %d, want the reminder still scheduled", len(left))
	} else if want := time.Date(2026, time.August, 11, 19, 30, 0, 0, loc); !left[0].Next.Equal(want.UTC()) {
		t.Errorf("next fire %s, want tonight's %s", left[0].Next, want.UTC())
	}
}

// TestCapStopsUnpromptedMessages is the second test the brief asks for by name.
//
// It also asserts the asymmetry the cap inherits from the missed-occurrence policy: a
// capped repeating occurrence is skipped and advanced, and a capped one-off is held so
// it is delivered tomorrow rather than lost.
func TestCapStopsUnpromptedMessages(t *testing.T) {
	loc := time.UTC
	s, _ := openTemp(t, Options{Location: loc, MaxPerDay: 2, CatchUp: 6 * time.Hour})

	base := time.Date(2026, time.August, 10, 6, 0, 0, 0, loc)
	// Three dailies and a one-off, all due at 07:00; the cap is two.
	for _, name := range []string{"first", "second", "third"} {
		add(t, s, name, EveryDaily, 7, 0, time.Sunday, "", base)
	}
	held := add(t, s, "promise", EveryOnce, 7, 0, time.Sunday, "2026-08-10", base)

	at7 := time.Date(2026, time.August, 10, 7, 0, 30, 0, loc)
	fires, skip := s.Due(at7)
	if len(fires) != 2 {
		t.Fatalf("delivered %d messages, want the cap of 2: %+v", len(fires), fires)
	}
	if skip.Capped != 2 {
		t.Errorf("skip = %+v, want 2 held back by the cap", skip)
	}
	if sent, cap := s.SentToday(at7); sent != 2 || cap != 2 {
		t.Errorf("SentToday = %d/%d, want 2/2", sent, cap)
	}

	// Nothing more today, however many passes run.
	for range 3 {
		if more, _ := s.Due(at7.Add(time.Hour)); len(more) != 0 {
			t.Fatalf("the cap let %+v through after it was spent", more)
		}
	}

	// The one-off was held rather than dropped: its next fire is still in the past,
	// so the first pass tomorrow delivers it.
	var promise Reminder
	for _, r := range s.List() {
		if r.ID == held.ID {
			promise = r
		}
	}
	if promise.ID == "" {
		t.Fatal("the capped one-off was dropped; a promise held back by the cap must survive to tomorrow")
	}
	if !promise.Next.Before(at7) {
		t.Errorf("the capped one-off was advanced to %s; it must stay due so tomorrow delivers it", promise.Next)
	}

	// Tomorrow the allowance resets and the held promise goes out.
	tomorrow := time.Date(2026, time.August, 11, 7, 0, 30, 0, loc)
	fires, _ = s.Due(tomorrow)
	var delivered bool
	for _, f := range fires {
		if f.Reminder.ID == held.ID {
			delivered = true
		}
	}
	if !delivered {
		t.Errorf("the held one-off was not delivered the next day; got %+v", fires)
	}
}

// TestCapOfZeroOrLessSilencesEverything. A household turning proactive messages off is
// expressible, and reminders survive being turned back on.
func TestCapNegativeSilencesEverything(t *testing.T) {
	s, _ := openTemp(t, Options{Location: time.UTC, MaxPerDay: -1})
	base := time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC)
	add(t, s, "quiet", EveryDaily, 7, 0, time.Sunday, "", base)

	fires, skip := s.Due(base.Add(2 * time.Hour))
	if len(fires) != 0 {
		t.Fatalf("delivered %+v with proactive messages turned off", fires)
	}
	if skip.Capped != 1 {
		t.Errorf("skip = %+v, want the delivery recorded as capped", skip)
	}
	if len(s.List()) != 1 {
		t.Error("the reminder was dropped; turning delivery off must not delete anything")
	}
}

// TestSurvivesARestart. A schedule that forgets on reboot is not a schedule.
func TestSurvivesARestart(t *testing.T) {
	loc := time.UTC
	opts := Options{Location: loc, MaxPerDay: 1, CatchUp: 6 * time.Hour}
	dir := t.TempDir()
	path := filepath.Join(dir, "reminders.json")

	first, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.August, 10, 6, 0, 0, 0, loc)
	kept := add(t, first, "bins go out tonight", EveryDaily, 19, 30, time.Sunday, "", base)
	// Spend the day's single allowance so the ledger has something to persist too.
	add(t, first, "spend it", EveryOnce, 7, 0, time.Sunday, "2026-08-10", base)
	if fires, _ := first.Due(time.Date(2026, time.August, 10, 7, 0, 30, 0, loc)); len(fires) != 1 {
		t.Fatalf("setup: delivered %d, want 1", len(fires))
	}

	// A new process opens the same file.
	second, err := Open(path, opts)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	list := second.List()
	if len(list) != 1 || list[0].ID != kept.ID {
		t.Fatalf("after restart the store holds %+v, want the daily reminder", list)
	}
	if list[0].Text != "bins go out tonight" || list[0].ChatID != 42 {
		t.Errorf("reloaded reminder lost fields: %+v", list[0])
	}
	// The cap ledger survived too: a unit that reset its allowance on every boot
	// would spend it forever in a crash loop.
	if sent, _ := second.SentToday(time.Date(2026, time.August, 10, 8, 0, 0, 0, loc)); sent != 1 {
		t.Errorf("SentToday = %d after restart, want the spent allowance to persist", sent)
	}
}

// TestCorruptFileIsAnErrorNotAnEmptyStore. Starting fresh would silently drop every
// promise the household is owed.
func TestCorruptFileIsAnErrorNotAnEmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("Open accepted a corrupt file; it must refuse rather than start empty")
	}
}

// TestWallClockSurvivesDaylightSaving. A 07:30 reminder stays at 07:30 across the
// change rather than sliding an hour, which is what makes storing hour and minute
// worth doing instead of just adding twenty-four hours.
func TestWallClockSurvivesDaylightSaving(t *testing.T) {
	loc := madrid(t)
	s, _ := openTemp(t, Options{Location: loc, MaxPerDay: 50, CatchUp: 6 * time.Hour})

	// Spain moves its clocks on the last Sunday of October; 2026's is the 25th.
	before := time.Date(2026, time.October, 24, 6, 0, 0, 0, loc)
	add(t, s, "bins", EveryDaily, 7, 30, time.Sunday, "", before)

	for _, day := range []int{24, 25, 26} {
		at := time.Date(2026, time.October, day, 7, 30, 30, 0, loc)
		fires, _ := s.Due(at)
		if len(fires) != 1 {
			t.Fatalf("%d October: delivered %d, want 1", day, len(fires))
		}
		if got := fires[0].Due.In(loc).Format("15:04"); got != "07:30" {
			t.Errorf("%d October: fired at %s, want 07:30 — the wall clock must not drift across the transition", day, got)
		}
		if fires[0].Late {
			t.Errorf("%d October: fire marked late", day)
		}
	}
}

// TestMaxStoredIsEnforced.
func TestMaxStoredIsEnforced(t *testing.T) {
	s, _ := openTemp(t, Options{Location: time.UTC, MaxStored: 2})
	base := time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC)
	add(t, s, "one", EveryDaily, 7, 0, time.Sunday, "", base)
	add(t, s, "two", EveryDaily, 8, 0, time.Sunday, "", base)

	r, err := New("three", EveryDaily, 9, 0, time.Sunday, "", 42, base, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(r); !errors.Is(err, ErrFull) {
		t.Fatalf("Add returned %v, want ErrFull", err)
	}
}

// TestOneOffInThePastIsRefused.
func TestOneOffInThePastIsRefused(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	if _, err := New("gone", EveryOnce, 9, 0, time.Sunday, "2026-08-09", 42, now, time.UTC); !errors.Is(err, ErrPast) {
		t.Fatalf("New returned %v, want ErrPast", err)
	}
}

// TestWeeklyLandsOnItsWeekday.
func TestWeeklyLandsOnItsWeekday(t *testing.T) {
	loc := time.UTC
	// A Monday.
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, loc)
	r, err := New("bins", EveryWeekly, 19, 0, time.Wednesday, "", 42, now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Next.In(loc); got.Weekday() != time.Wednesday || got.Day() != 12 {
		t.Errorf("first fire %s, want Wednesday 12 August", got)
	}
	if got := r.When(loc); got != "every Wednesday at 19:00" {
		t.Errorf("When = %q", got)
	}
}

// TestClockDeliversAndReportsFailures exercises the Clock over a fake sender.
func TestClockDeliversAndReportsFailures(t *testing.T) {
	loc := time.UTC
	s, _ := openTemp(t, Options{Location: loc, MaxPerDay: 5})
	base := time.Date(2026, time.August, 10, 6, 0, 0, 0, loc)
	add(t, s, "bins", EveryDaily, 7, 0, time.Sunday, "", base)

	var mu sync.Mutex
	var got []string
	send := func(_ context.Context, chatID int64, text string) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, text)
		if chatID != 42 {
			t.Errorf("sent to chat %d, want 42", chatID)
		}
		return nil
	}
	now := time.Date(2026, time.August, 10, 7, 0, 10, 0, loc)
	c := NewClock(s, send, func() time.Time { return now }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.pass(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "bins" {
		t.Fatalf("delivered %q, want the stored text verbatim and nothing appended", got)
	}
}

// TestClockRunStopsOnContextCancel. The drain cancels the clock context, and Run must
// return rather than leak a goroutine.
func TestClockRunStopsOnContextCancel(t *testing.T) {
	s, _ := openTemp(t, Options{Location: time.UTC})
	c := NewClock(s, func(context.Context, int64, string) error { return nil }, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Clock.Run did not return after its context was cancelled")
	}
}
