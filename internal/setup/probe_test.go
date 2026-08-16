package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDialAddress(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "http://monster.tail:8000/v1", want: "monster.tail:8000"},
		{in: "http://monster.tail/v1", want: "monster.tail:80"},
		{in: "https://openrouter.ai/api/v1", want: "openrouter.ai:443"},
		{in: "http://192.168.1.10:11434/v1", want: "192.168.1.10:11434"},
		{in: "http://[::1]:8000/v1", want: "[::1]:8000"},
		{in: "monster.tail:8000", wantErr: "http:// or https://"},
		{in: "ftp://monster.tail/v1", wantErr: "http:// or https://"},
		{in: "http:///v1", wantErr: "no host"},
	} {
		got, err := dialAddress(tc.in)
		switch {
		case tc.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("dialAddress(%q) error = %v, want it to mention %q", tc.in, err, tc.wantErr)
			}
		case err != nil:
			t.Errorf("dialAddress(%q) = %v", tc.in, err)
		case got != tc.want:
			t.Errorf("dialAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProbeAnswers(t *testing.T) {
	p := &Prober{Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "monster.tail:8000" {
			t.Errorf("dialled %q", address)
		}
		client, server := net.Pipe()
		server.Close()
		return client, nil
	}}
	got := p.Probe(context.Background(), "http://monster.tail:8000/v1")
	if got.State != Answered {
		t.Fatalf("state = %v, want Answered", got.State)
	}
	if !strings.Contains(got.describe(), "answered in") {
		t.Errorf("describe() = %q", got.describe())
	}
}

// TestAFastAnswerIsNotReportedAsNoTimeAtAll.
//
// A machine on the household's own network accepts a connection in a few hundred
// microseconds, which is the common case rather than the exception, and "answered in
// 0ms" reads as though the wizard did not actually try — the opposite of what just
// happened, on the one line that exists to reassure somebody the address is right.
func TestAFastAnswerIsNotReportedAsNoTimeAtAll(t *testing.T) {
	got := ProbeResult{State: Answered, Elapsed: 200 * time.Microsecond, Addr: "monster.tail:8000"}
	if strings.Contains(got.describe(), "0ms") {
		t.Errorf("a sub-millisecond answer reads as no attempt at all: %q", got.describe())
	}
	if !strings.Contains(got.describe(), "answered") {
		t.Errorf("describe() = %q, want it to say the endpoint answered", got.describe())
	}
}

func TestProbeRefused(t *testing.T) {
	p := &Prober{Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	}}
	got := p.Probe(context.Background(), "http://monster.tail:8000/v1")
	if got.State != Refused {
		t.Fatalf("state = %v, want Refused", got.State)
	}
	// A machine with the wrong port is still recorded, and the wording has to say
	// so or somebody will go and switch a working machine off and on again.
	if !strings.Contains(got.describe(), "Recorded anyway") {
		t.Errorf("describe() = %q", got.describe())
	}
}

func TestProbeTimesOut(t *testing.T) {
	p := &Prober{
		Timeout: 20 * time.Millisecond,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	start := time.Now()
	got := p.Probe(context.Background(), "http://monster.tail:8000/v1")
	if got.State != NoAnswer {
		t.Fatalf("state = %v, want NoAnswer", got.State)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the probe waited %s, so the timeout is not being applied", elapsed)
	}
	// The normal case in this product: a desktop that is switched off.
	if !strings.Contains(got.describe(), "switched off") {
		t.Errorf("describe() = %q", got.describe())
	}
	if !strings.Contains(got.describe(), "recorded") {
		t.Errorf("a switched-off machine must still be recorded: %q", got.describe())
	}
}

func TestProbeUnresolvedName(t *testing.T) {
	p := &Prober{Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "monster.tial"}}
	}}
	got := p.Probe(context.Background(), "http://monster.tial:8000/v1")
	if got.State != Unresolved {
		t.Fatalf("state = %v, want Unresolved", got.State)
	}
	if !strings.Contains(got.describe(), "typo") {
		t.Errorf("describe() = %q, want it to suggest a typo", got.describe())
	}
}

func TestProbeBadURL(t *testing.T) {
	p := &Prober{Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		t.Error("a URL that cannot be dialled must not be dialled")
		return nil, nil
	}}
	got := p.Probe(context.Background(), "monster.tail:8000")
	if got.State != BadURL {
		t.Fatalf("state = %v, want BadURL", got.State)
	}
}

// TestProbeAgainstARealSocket exercises the default dialler once, on the loopback
// interface. It is not a network test: nothing leaves the machine, and the listener
// is created and destroyed by the test itself.
func TestProbeAgainstARealSocket(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}
	addr := listener.Addr().String()
	baseURL := fmt.Sprintf("http://%s/v1", addr)

	got := DefaultProbe(context.Background(), baseURL)
	if got.State != Answered {
		t.Errorf("a listening socket reported %v: %v", got.State, got.Err)
	}

	listener.Close()
	if got := (&Prober{Timeout: time.Second}).Probe(context.Background(), baseURL); got.State == Answered {
		t.Error("a closed socket reported as answering")
	}
}

func TestIsLocal(t *testing.T) {
	for url, want := range map[string]bool{
		"http://localhost:8000/v1":          true,
		"http://127.0.0.1:8000/v1":          true,
		"http://[::1]:8000/v1":              true,
		"http://192.168.1.10:11434/v1":      true,
		"http://10.0.0.5:8000/v1":           true,
		"http://monster:8000/v1":            true,
		"http://monster.local:8000/v1":      true,
		"http://monster.lan:8000/v1":        true,
		"http://monster.tail:8000/v1":       true,
		"http://box.tailnet.ts.net:8000/v1": true,
		"https://openrouter.ai/api/v1":      false,
		"https://api.openai.com/v1":         false,
		"https://8.8.8.8/v1":                false,
		"":                                  false,
	} {
		if got := isLocal(url); got != want {
			t.Errorf("isLocal(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestHostOf(t *testing.T) {
	if got := hostOf("https://openrouter.ai/api/v1"); got != "openrouter.ai" {
		t.Errorf("hostOf = %q", got)
	}
}
