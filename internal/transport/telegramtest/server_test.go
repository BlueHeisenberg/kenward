package telegramtest

import (
	"net/http"
	"testing"
	"time"
)

// heldCallWindow is how long the test below watches for a call that is still
// being served. It is one-sided: a server that records only what it has answered
// can never record during it, because the hold is never released, so the test
// cannot flake. A server that records on arrival records in microseconds.
const heldCallWindow = 200 * time.Millisecond

// TestACallIsInvisibleUntilItHasBeenAnswered pins the timing that two CI
// failures came out of.
//
// CallsFor and WaitCall are how a test says "the client has made this call", and
// everything it does next is built on that: it cancels a context, it reads the
// MessageID the server handed back, it pushes a callback_query at the keyboard
// that send drew. All of that is only sound if the call is finished. A server
// that recorded a request on arrival would unblock WaitCall while the client was
// still waiting on the response — and then the cancellation would race the send
// it had just waited for, and the MessageID would be the zero the field is
// filled in from rather than the id the message got.
func TestACallIsInvisibleUntilItHasBeenAnswered(t *testing.T) {
	s := New(t, "tok")
	release := s.Hold("sendMessage")
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Post(s.URL()+"/bottok/sendMessage", "text/plain", nil)
		if err == nil {
			resp.Body.Close()
		}
	}()

	time.Sleep(heldCallWindow)
	if n := s.CountFor("sendMessage"); n != 0 {
		t.Fatalf("%d sendMessage call(s) visible while still being served, want 0: "+
			"a test that waited on this call would be racing it", n)
	}

	release()
	<-done

	call := s.WaitCall(t, "sendMessage", 1)
	if call.MessageID == 0 {
		t.Error("recorded call has no MessageID; it must carry the id the server gave the message")
	}
}
