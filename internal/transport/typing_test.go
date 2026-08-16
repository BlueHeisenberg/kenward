package transport

import (
	"context"
	"testing"
	"time"
)

// TestKeepTypingRepeatsAndStops.
//
// Telegram's typing action expires after about five seconds and cannot be extended or
// cancelled; the only lever a caller has is whether it sends another one. So the two
// properties that matter are that the indicator is repeated for as long as the member
// is waiting — a turn against a real model measured fifteen to twenty seconds, which is
// three or four actions — and that it stops when the wait does.
//
// The interval is a parameter for exactly this test. A package-level variable would be
// a data race the moment two tests ran together, and a hardcoded four seconds would
// make the repetition either untestable or slow enough that somebody skips it.
func TestKeepTypingRepeatsAndStops(t *testing.T) {
	f := NewFake()
	const chat = int64(42)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		KeepTyping(ctx, f, chat, time.Millisecond)
	}()

	// Three actions is the shape of a fifteen-second wait at the real interval. The
	// deadline is generous because this asserts repetition, not latency.
	deadline := time.Now().Add(5 * time.Second)
	for f.TypingCount(chat) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d typing actions in five seconds; the indicator is not being repeated, so it expires part-way through a real wait", f.TypingCount(chat))
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	<-done
	// KeepTyping has returned, so nothing can send another action: the count is
	// settled rather than merely quiet, which is what makes "it stops when the reply
	// lands" an assertion instead of a hope.
	settled := f.TypingCount(chat)
	time.Sleep(20 * time.Millisecond)
	if got := f.TypingCount(chat); got != settled {
		t.Errorf("typing count went from %d to %d after KeepTyping returned; the indicator outlived the wait it was covering", settled, got)
	}
	if f.TypingCount(chat+1) != 0 {
		t.Errorf("a chat nobody was waiting in got %d typing actions", f.TypingCount(chat+1))
	}
}

// TestKeepTypingSendsImmediately: the first action is what the whole thing is for. A
// member who sees nothing for the first four seconds has already decided the assistant
// is broken, and an indicator that waits for its first tick would deliver exactly that.
func TestKeepTypingSendsImmediately(t *testing.T) {
	f := NewFake()
	// An hour between ticks, so anything that arrives arrived before the first one.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		KeepTyping(ctx, f, 7, time.Hour)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for f.TypingCount(7) == 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("no typing action before the first tick; the member watches an empty chat for the whole interval")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if got := f.TypingCount(7); got != 1 {
		t.Errorf("typing actions = %d, want exactly the one sent before the first tick", got)
	}
}
