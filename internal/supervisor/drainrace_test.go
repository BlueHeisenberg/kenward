package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/transport"
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

// A Stop that lands while Start is still launching must leave Start reporting a
// clean stop, not a startup failure.
//
// This is the other half of the bug above, and it outlived the fix for it. start
// walks its units one at a time; a Stop arriving in the middle sets `stopping`, and
// everything start has left to do then refuses — launch, launchEnrol and
// launchBackstop all return "supervisor: stopping", mux.Start returns ErrClosed once
// the drain has closed the mux, and a view's Updates does too. start handed whichever
// one it got straight back to its caller, so the same Stop was a clean shutdown or a
// startup failure depending on the microsecond it landed in: a moment later and start
// is parked in the select below, where stoppedCh returns nil.
//
// That is what has been failing TestSingleHealthBeforeStartAndAfterStop on macOS with
// `Start returned: supervisor: stopping`. Health reports a unit Ready as soon as its
// pump starts, which is before start has finished launching, so a caller that waits
// for Ready and then stops — which is what the harness does, and what a tray or a
// SIGTERM does — is inside the window by construction.
//
// Driven through the real start, with the Stop placed at the one point a test can put
// it deterministically: a unit whose update stream opens by stopping the supervisor.
// launch calls Updates before it takes the lock that refuses it, so start meets
// exactly the state the race produces — one pump already running, stopping already
// set, and start about to find out. Racing two goroutines and waiting for the window
// would be a test that passes on every fast machine, which is how this survived.
func TestStartReportsACleanStopWhenStopLandsMidLaunch(t *testing.T) {
	h := newSingleHarness(t, singleTestConfig(), nil)
	r := h.sup.run

	stopper := transport.NewFake()
	t.Cleanup(func() { _ = stopper.Close() })
	r.pending = append(r.pending, pendingUnit{
		key:  unitKey{member: "stopper"},
		view: stopOnUpdates{Transport: stopper, stop: func() { _ = r.stop(context.Background()) }},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.start(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v for a supervisor that was only stopped; a Stop is not a startup failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after the Stop it raced")
	}
}

// stopOnUpdates is a unit's view whose update stream opens by stopping the
// supervisor, which is where the race above puts the Stop.
type stopOnUpdates struct {
	transport.Transport
	stop func()
}

func (s stopOnUpdates) Updates(ctx context.Context) (<-chan transport.Inbound, error) {
	s.stop()
	return s.Transport.Updates(ctx)
}
