package transport

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestFakeDeliversInjectedMessagesInOrder(t *testing.T) {
	f := NewFake()
	t.Cleanup(func() { _ = f.Close() })

	f.InjectText(100, 7, "first", false)
	f.InjectText(-200, 8, "second", true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up, err := f.Updates(ctx)
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}

	first := recv(t, up)
	if first.Text != "first" || first.IsGroup {
		t.Fatalf("first = %+v", first)
	}
	second := recv(t, up)
	if second.Text != "second" || !second.IsGroup || second.ChatID != -200 {
		t.Fatalf("second = %+v", second)
	}
	if first.At.IsZero() || second.At.IsZero() {
		t.Fatal("injected messages should be stamped")
	}
}

func TestFakeCapturesOutbound(t *testing.T) {
	f := NewFake()
	t.Cleanup(func() { _ = f.Close() })

	if err := f.Send(context.Background(), Outbound{ChatID: 100, Text: "one"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := f.Send(context.Background(), Outbound{ChatID: 100, Text: "two", ReplyTo: 5}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	sent := f.Sent()
	if len(sent) != 2 || sent[0].Text != "one" || sent[1].ReplyTo != 5 {
		t.Fatalf("captured %+v", sent)
	}
	last, ok := f.LastSent()
	if !ok || last.Text != "two" {
		t.Fatalf("LastSent = %+v, %v", last, ok)
	}

	// The snapshot is a copy: mutating it must not corrupt the record.
	sent[0].Text = "tampered"
	if again := f.Sent(); again[0].Text != "one" {
		t.Fatal("Sent() handed out its own slice")
	}
}

func TestFakeDefaultsToDeclining(t *testing.T) {
	f := NewFake()
	t.Cleanup(func() { _ = f.Close() })

	answer, err := f.Ask(context.Background(), Question{
		ChatID:        100,
		Text:          "Save this?",
		Choices:       []Choice{{ID: "yes", Label: "Save"}},
		AllowedUserID: 7,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !answer.TimedOut {
		t.Fatalf("answer = %+v, want a timeout by default", answer)
	}
	if len(f.Asked()) != 1 {
		t.Fatalf("Asked() = %d questions, want 1", len(f.Asked()))
	}
}

func TestFakeAnswerModes(t *testing.T) {
	q := Question{
		ChatID:        -200,
		Text:          "Where should this go?",
		Choices:       []Choice{{ID: "shared", Label: "Household"}, {ID: "no", Label: "Don't save"}},
		AllowedUserID: 7,
	}

	t.Run("choice", func(t *testing.T) {
		f := NewFake()
		f.AnswerWithChoice("shared")
		got, err := f.Ask(context.Background(), q)
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if got.ChoiceID != "shared" || got.UserID != 7 || got.TimedOut {
			t.Fatalf("answer = %+v", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		f := NewFake()
		f.AnswerWithTimeout()
		got, err := f.Ask(context.Background(), q)
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if !got.TimedOut {
			t.Fatalf("answer = %+v, want a timeout", got)
		}
	})

	// The case the real transport exists to prevent: another member taps.
	t.Run("wrong user", func(t *testing.T) {
		f := NewFake()
		f.AnswerFromUser(8, "shared")
		got, err := f.Ask(context.Background(), q)
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if !got.TimedOut || got.ChoiceID != "" {
			t.Fatalf("answer = %+v, want the tap ignored and the question timed out", got)
		}
		if f.IgnoredTaps() != 1 {
			t.Fatalf("IgnoredTaps() = %d, want 1", f.IgnoredTaps())
		}
	})

	t.Run("queued in order", func(t *testing.T) {
		f := NewFake()
		f.QueueAnswers(
			Answer{ChoiceID: "shared"},
			Answer{TimedOut: true},
			Answer{ChoiceID: "no", UserID: 8},
		)
		first, _ := f.Ask(context.Background(), q)
		if first.ChoiceID != "shared" || first.UserID != 7 {
			t.Fatalf("first = %+v", first)
		}
		second, _ := f.Ask(context.Background(), q)
		if !second.TimedOut {
			t.Fatalf("second = %+v", second)
		}
		third, _ := f.Ask(context.Background(), q)
		if !third.TimedOut {
			t.Fatalf("third = %+v, want the wrong user's tap ignored", third)
		}
		fourth, _ := f.Ask(context.Background(), q)
		if !fourth.TimedOut {
			t.Fatalf("fourth = %+v, want the default once the script runs out", fourth)
		}
	})
}

func TestFakeAskRespectsContext(t *testing.T) {
	f := NewFake()
	t.Cleanup(func() { _ = f.Close() })
	f.AnswerWithChoice("yes")
	f.SetAskDelay(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := f.Ask(ctx, Question{
		ChatID:        100,
		Text:          "Save this?",
		Choices:       []Choice{{ID: "yes", Label: "Save"}},
		AllowedUserID: 7,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask error = %v, want context.Canceled", err)
	}
}

func TestFakeErrorsAreProgrammable(t *testing.T) {
	f := NewFake()
	t.Cleanup(func() { _ = f.Close() })

	boom := errors.New("no network")
	f.SetSendError(boom)
	if err := f.Send(context.Background(), Outbound{ChatID: 1, Text: "x"}); !errors.Is(err, boom) {
		t.Fatalf("Send error = %v, want the scripted one", err)
	}
	f.SetSendError(nil)
	if err := f.Send(context.Background(), Outbound{ChatID: 1, Text: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	f.SetAskError(boom)
	if _, err := f.Ask(context.Background(), Question{ChatID: 1, Text: "x", Choices: []Choice{{ID: "a", Label: "A"}}}); !errors.Is(err, boom) {
		t.Fatalf("Ask error = %v, want the scripted one", err)
	}
}

func TestFakeCloseIsIdempotentAndFinal(t *testing.T) {
	base := runtime.NumGoroutine()

	f := NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up, err := f.Updates(ctx)
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	select {
	case _, ok := <-up:
		if ok {
			t.Fatal("a closed Fake still delivered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Updates channel was not closed")
	}

	if err := f.Send(context.Background(), Outbound{ChatID: 1, Text: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close = %v, want ErrClosed", err)
	}
	if _, err := f.Ask(context.Background(), Question{ChatID: 1, Text: "x", Choices: []Choice{{ID: "a", Label: "A"}}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ask after Close = %v, want ErrClosed", err)
	}
	if _, err := f.Updates(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Updates after Close = %v, want ErrClosed", err)
	}
	assertNoGoroutineLeak(t, base)
}

func TestFakeReset(t *testing.T) {
	f := NewFake()
	t.Cleanup(func() { _ = f.Close() })
	f.AnswerWithChoice("yes")

	_ = f.Send(context.Background(), Outbound{ChatID: 1, Text: "x"})
	_, _ = f.Ask(context.Background(), Question{ChatID: 1, Text: "x", Choices: []Choice{{ID: "yes", Label: "Y"}}, AllowedUserID: 7})
	f.Reset()

	if len(f.Sent()) != 0 || len(f.Asked()) != 0 || f.IgnoredTaps() != 0 {
		t.Fatal("Reset did not clear the record")
	}
	got, err := f.Ask(context.Background(), Question{ChatID: 1, Text: "x", Choices: []Choice{{ID: "yes", Label: "Y"}}, AllowedUserID: 7})
	if err != nil || got.ChoiceID != "yes" {
		t.Fatalf("Reset should leave the script alone: %+v, %v", got, err)
	}
}
