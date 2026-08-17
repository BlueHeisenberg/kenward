package supervisor

import (
	"context"
	"testing"
	"time"
)

// A Stop that lands while Start is still launching units must still drain.
//
// start walks its units one at a time and sets `launched` only once it has them
// all. A Stop arriving in the middle sets `stopping`, the next launch refuses with
// it, and start gives up and shuts down — having never reached that line. The pumps
// it did manage to start then exit on the drain signal and the active count reaches
// zero, and until workerDone counted `stopping` nobody closed allDone: the drain
// waited out its entire deadline and reported itself unclean for pumps that had all
// gone. In a pod that is a SIGTERM answered twenty seconds later with an error.
//
// It is driven at the runner rather than through Start because the real thing is a
// race between two goroutines and a test that waits for it to land is a test that
// fails on a slow machine. The state below is the state that race produces, exactly:
// started, one pump running, `launched` never set.
func TestDrainEndsWhenStopRacesAnUnfinishedStart(t *testing.T) {
	h := newSingleHarness(t, singleTestConfig(), nil)
	r := h.sup.run

	r.mu.Lock()
	r.started = true
	r.active = 1
	r.mu.Unlock()

	// One pump, behaving exactly as runUnit does: it returns when intake is
	// drained, and retires itself on the way out.
	go func() {
		<-r.draining
		r.workerDone()
	}()

	// Generously longer than a healthy drain and far shorter than the deadline a
	// hung one would burn, so a regression fails as a timeout here rather than as
	// a slow test somebody learns to ignore.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.stop(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop reported an unclean drain for a pump that exited: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return: the last pump exited and nothing closed allDone")
	}
}
