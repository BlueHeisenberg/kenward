package transport

import (
	"testing"
	"time"
)

func TestQueueKeepsOrder(t *testing.T) {
	q := newQueue(10)
	for i := 0; i < 5; i++ {
		q.push(Inbound{MessageID: i})
	}
	q.close()

	for i := 0; i < 5; i++ {
		in, ok := q.pop()
		if !ok || in.MessageID != i {
			t.Fatalf("pop %d = %+v, %v", i, in, ok)
		}
	}
	if _, ok := q.pop(); ok {
		t.Fatal("pop should report false once closed and drained")
	}
}

// The producer is the goroutine reading Telegram; it must never wait for a
// consumer, or a question asked mid-conversation would never see its answer.
func TestQueuePushNeverBlocks(t *testing.T) {
	q := newQueue(4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			q.push(Inbound{MessageID: i})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("push blocked")
	}
	if q.dropped() == 0 {
		t.Fatal("expected the overflow to be counted")
	}

	// What survives is the most recent, not the oldest.
	in, ok := q.pop()
	if !ok || in.MessageID != 996 {
		t.Fatalf("oldest surviving message = %+v, %v; want 996", in, ok)
	}
}

func TestQueueCloseWakesAWaitingConsumer(t *testing.T) {
	q := newQueue(4)
	done := make(chan bool, 1)
	go func() {
		_, ok := q.pop()
		done <- ok
	}()

	time.Sleep(20 * time.Millisecond)
	q.close()
	q.close() // idempotent

	select {
	case ok := <-done:
		if ok {
			t.Fatal("pop returned a message from an empty closed queue")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wake the consumer")
	}
}

func TestQueuePushAfterCloseIsDropped(t *testing.T) {
	q := newQueue(4)
	q.close()
	if lost := q.push(Inbound{MessageID: 1}); !lost {
		t.Fatal("a push to a closed queue should report the drop")
	}
	if _, ok := q.pop(); ok {
		t.Fatal("a closed queue should stay empty")
	}
}
