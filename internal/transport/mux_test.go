package transport

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func byUser(id int64) func(Inbound) bool {
	return func(in Inbound) bool { return in.UserID == id }
}

func recv(t *testing.T, ch <-chan Inbound) Inbound {
	t.Helper()
	select {
	case in, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while waiting for a message")
		}
		return in
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a message")
		return Inbound{}
	}
}

func expectQuiet(t *testing.T, ch <-chan Inbound) {
	t.Helper()
	select {
	case in := <-ch:
		t.Fatalf("unexpected message %+v", in)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestMuxRoutesToTheMatchingView(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	david := m.View(byUser(7))
	ana := m.View(byUser(8))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	davidUp, err := david.Updates(ctx)
	if err != nil {
		t.Fatalf("david Updates: %v", err)
	}
	anaUp, err := ana.Updates(ctx)
	if err != nil {
		t.Fatalf("ana Updates: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	f.InjectText(100, 7, "mine", false)
	f.InjectText(200, 8, "hers", false)

	if got := recv(t, davidUp); got.Text != "mine" || got.UserID != 7 {
		t.Fatalf("david received %+v", got)
	}
	if got := recv(t, anaUp); got.Text != "hers" || got.UserID != 8 {
		t.Fatalf("ana received %+v", got)
	}
	expectQuiet(t, davidUp)
	expectQuiet(t, anaUp)
}

func TestMuxDropsWhatNoViewClaims(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	view := m.View(byUser(7))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up, err := view.Updates(ctx)
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	f.InjectText(999, 404, "a stranger writes", false)
	f.InjectText(100, 7, "mine", false)

	if got := recv(t, up); got.Text != "mine" {
		t.Fatalf("received %+v, want only the matching message", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if m.Dropped() != 1 {
		t.Fatalf("Dropped() = %d, want 1", m.Dropped())
	}
}

func TestMuxFirstMatchWins(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	first := m.View(func(Inbound) bool { return true })
	second := m.View(func(Inbound) bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstUp, _ := first.Updates(ctx)
	secondUp, _ := second.Updates(ctx)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	f.InjectText(100, 7, "once only", false)
	if got := recv(t, firstUp); got.Text != "once only" {
		t.Fatalf("first view received %+v", got)
	}
	expectQuiet(t, secondUp)
}

func TestMuxViewPassesSendAndAskThrough(t *testing.T) {
	f := NewFake()
	f.AnswerWithChoice("shared")
	m := NewMux(f)
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	view := m.View(byUser(7))
	if err := view.Send(context.Background(), Outbound{ChatID: 100, Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	answer, err := view.Ask(context.Background(), Question{
		ChatID:        100,
		Text:          "Where?",
		Choices:       []Choice{{ID: "shared", Label: "Household"}},
		AllowedUserID: 7,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if answer.ChoiceID != "shared" {
		t.Fatalf("answer = %+v", answer)
	}
	if len(f.Sent()) != 1 || len(f.Asked()) != 1 {
		t.Fatalf("underlying transport saw %d sends and %d questions", len(f.Sent()), len(f.Asked()))
	}
}

func TestMuxViewCloseDoesNotCloseTheBot(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	david := m.View(byUser(7))
	ana := m.View(byUser(8))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	davidUp, _ := david.Updates(ctx)
	anaUp, _ := ana.Updates(ctx)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := david.Close(); err != nil {
		t.Fatalf("view Close: %v", err)
	}
	if err := david.Close(); err != nil {
		t.Fatalf("second view Close: %v", err)
	}

	select {
	case _, ok := <-davidUp:
		if ok {
			t.Fatal("a closed view still delivered a message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a closed view did not close its channel")
	}
	if err := david.Send(context.Background(), Outbound{ChatID: 1, Text: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send on a closed view = %v, want ErrClosed", err)
	}

	// The household is still on the air.
	f.InjectText(200, 8, "still here", false)
	if got := recv(t, anaUp); got.Text != "still here" {
		t.Fatalf("ana received %+v", got)
	}
	if err := ana.Send(context.Background(), Outbound{ChatID: 200, Text: "reply"}); err != nil {
		t.Fatalf("Send on a live view: %v", err)
	}
}

func TestMuxCloseStopsEverythingWithoutLeaking(t *testing.T) {
	f := NewFake()
	base := runtime.NumGoroutine()

	m := NewMux(f)
	views := []Transport{m.View(byUser(7)), m.View(byUser(8)), m.View(func(in Inbound) bool { return in.IsGroup })}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var streams []<-chan Inbound
	for _, v := range views {
		up, err := v.Updates(ctx)
		if err != nil {
			t.Fatalf("Updates: %v", err)
		}
		streams = append(streams, up)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	f.InjectText(100, 7, "one", false)
	recv(t, streams[0])

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	for i, s := range streams {
		select {
		case _, ok := <-s:
			if ok {
				for range s {
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("view %d channel was not closed", i)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("fake Close: %v", err)
	}
	assertNoGoroutineLeak(t, base)
}

// A view whose consumer is blocked must not stop the others: units block inside
// Ask all the time, and the household keeps talking meanwhile.
func TestMuxBlockedViewDoesNotStallTheRest(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	busy := m.View(byUser(7))
	other := m.View(byUser(8))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// busy never reads its stream.
	if _, err := busy.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}
	otherUp, err := other.Updates(ctx)
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < 200; i++ {
		f.InjectText(100, 7, "backlog", false)
	}
	f.InjectText(200, 8, "still delivered", false)

	if got := recv(t, otherUp); got.Text != "still delivered" {
		t.Fatalf("received %+v", got)
	}
}

func TestMuxStartIsSingleShot(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Start(ctx); !errors.Is(err, ErrUpdatesActive) {
		t.Fatalf("second Start = %v, want ErrUpdatesActive", err)
	}
}

func TestMuxAfterCloseRefuses(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	t.Cleanup(func() { _ = f.Close() })

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Start(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}

	v := m.View(nil)
	if _, err := v.Updates(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("view Updates after Close = %v, want ErrClosed", err)
	}
	if err := v.Send(context.Background(), Outbound{ChatID: 1, Text: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("view Send after Close = %v, want ErrClosed", err)
	}
}

// gatedTransport blocks inside Updates until released, so a test can hold Start
// at the exact point a concurrent Close used to win the race.
type gatedTransport struct {
	entered chan struct{}
	release chan struct{}
	ch      chan Inbound
}

func (g *gatedTransport) Updates(context.Context) (<-chan Inbound, error) {
	close(g.entered)
	<-g.release
	return g.ch, nil
}
func (g *gatedTransport) Send(context.Context, Outbound) error          { return nil }
func (g *gatedTransport) Ask(context.Context, Question) (Answer, error) { return Answer{}, nil }
func (g *gatedTransport) Close() error                                  { return nil }

// A Close racing Start must not strand the stream: once Start has consumed the
// underlying Updates, its dispatcher must run. The gate parks Start inside
// Updates; under the old code Close ran to completion in that window and Start
// then returned ErrClosed with a started, unconsumed stream behind it.
func TestMuxStartWinsOverConcurrentClose(t *testing.T) {
	g := &gatedTransport{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		ch:      make(chan Inbound),
	}
	m := NewMux(g)

	startErr := make(chan error, 1)
	go func() { startErr <- m.Start(context.Background()) }()
	<-g.entered

	closeDone := make(chan struct{})
	go func() { _ = m.Close(); close(closeDone) }()

	// Give Close every chance to finish while Start is inside Updates. With the
	// fix it is parked on the lock and cannot; under the old code it completed
	// here, deterministically.
	select {
	case <-closeDone:
	case <-time.After(100 * time.Millisecond):
	}

	close(g.release)
	if err := <-startErr; err != nil {
		t.Fatalf("Start = %v; a Start that consumed the update stream must run its dispatcher", err)
	}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close never finished")
	}
}

// A view whose backlog overflows loses its oldest message; that loss must be
// visible in Dropped(), not silent.
func TestMuxCountsViewBacklogOverflow(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	m.queueCap = 2
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	// The view exists but never reads its stream, like a unit stuck in Ask.
	m.View(byUser(7))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < 5; i++ {
		f.InjectText(100, 7, "backlog", false)
	}

	deadline := time.Now().Add(2 * time.Second)
	for m.Dropped() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := m.Dropped(); got != 3 {
		t.Fatalf("Dropped() = %d, want 3 overflow drops to be counted", got)
	}
}

func TestMuxViewUpdatesIsSingleReader(t *testing.T) {
	f := NewFake()
	m := NewMux(f)
	t.Cleanup(func() { _ = m.Close(); _ = f.Close() })

	v := m.View(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := v.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}
	if _, err := v.Updates(ctx); !errors.Is(err, ErrUpdatesActive) {
		t.Fatalf("second Updates = %v, want ErrUpdatesActive", err)
	}
}
