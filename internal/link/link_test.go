package link

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/lore"
	"github.com/google/uuid"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// Two real lore homes, because the thing under test is whether a space actually
// crosses from one account to another. A fake memory would prove the HTTP shape and
// nothing that matters.
func newPod(t *testing.T) *memory.Client {
	t.Helper()
	home := t.TempDir()
	if _, err := lore.Init(home, "linktest"); err != nil {
		t.Fatalf("lore.Init: %v", err)
	}
	c, err := memory.NewClient(memory.Config{LoreHome: home})
	if err != nil {
		t.Fatalf("memory.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// deskFor starts a link desk on loopback for the group's store and returns its
// address. mDNS is off: two halves in one process are already on each other's
// loopback, and every test here hands the address over directly.
func deskFor(t *testing.T, group *memory.Client, space domain.SpaceID, key []byte) string {
	t.Helper()
	ln := listener(t)
	addr := ln.Addr
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Serve(ctx, Options{
			Space: space, Key: key, Memory: group,
			LoopbackOnly: true, Port: ln.Port, NoDiscovery: true,
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitForDesk(t, addr)
	return addr
}

// listener picks a free loopback port and releases it, so Serve can bind it. A
// race in principle; in a test binary that has just asked the kernel for a port
// nobody else is competing for one.
type freePort struct {
	Addr string
	Port int
}

func listener(t *testing.T) freePort {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	var port int
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		for _, r := range addr[i+1:] {
			port = port*10 + int(r-'0')
		}
	}
	return freePort{Addr: addr, Port: port}
}

func waitForDesk(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Post("http://"+addr+Path, "application/json", strings.NewReader("{}"))
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the link desk at %s never came up", addr)
}

func newSpaceID(t *testing.T) domain.SpaceID {
	t.Helper()
	return domain.SpaceID(uuid.NewString())
}

var key = []byte("a-household-link-key-of-real-length")

// TestAMemberPodIsAdmittedWithNobodyRunningAnything is the whole point: the
// group's pod owns the space, the member's pod does not hold it, and after the two
// halves have run the member's own store holds it and can write to it.
func TestAMemberPodIsAdmittedWithNobodyRunningAnything(t *testing.T) {
	group, member := newPod(t), newPod(t)
	space := newSpaceID(t)
	if _, err := group.CreateSpaceWithID(t.Context(), string(space), "household"); err != nil {
		t.Fatal(err)
	}

	// The failing state, asserted rather than assumed.
	if held, err := member.HasSpace(t.Context(), space); err != nil || held {
		t.Fatalf("the member's pod already holds the household space: held=%v err=%v", held, err)
	}

	addr := deskFor(t, group, space, key)
	if err := Join(t.Context(), Options{
		Space: space, Key: key, Memory: member, LoopbackOnly: true, Addrs: []string{addr},
	}); err != nil {
		t.Fatalf("Join: %v", err)
	}

	held, err := member.HasSpace(t.Context(), space)
	if err != nil || !held {
		t.Fatalf("the member's pod does not hold the household space: held=%v err=%v", held, err)
	}
	if _, err := member.Put(t.Context(), space, memory.Draft{
		Title: "bin day", Body: "tuesday", Domain: "house/chores",
	}); err != nil {
		t.Fatalf("the member's pod cannot write to the household space: %v", err)
	}
}

// TestJoinIsANoOpOnceHeld covers every boot after the first, and every rolling
// update: the pod checks before it asks, and a desk that is not even there costs
// nothing.
func TestJoinIsANoOpOnceHeld(t *testing.T) {
	member := newPod(t)
	space := newSpaceID(t)
	if _, err := member.CreateSpaceWithID(t.Context(), string(space), "household"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := Join(ctx, Options{
		Space: space, Key: key, Memory: member, LoopbackOnly: true,
		Addrs: []string{"127.0.0.1:1"}, // would refuse the connection if it were tried
	}); err != nil {
		t.Fatalf("Join on a pod that already holds the space: %v", err)
	}
}

// TestAdmittingTwiceIsOneMembership is the re-provisioning case: a pod recreated
// against its own volume asks again, and the household's member list does not grow
// a duplicate.
func TestAdmittingTwiceIsOneMembership(t *testing.T) {
	group, member := newPod(t), newPod(t)
	space := newSpaceID(t)
	if _, err := group.CreateSpaceWithID(t.Context(), string(space), "household"); err != nil {
		t.Fatal(err)
	}
	addr := deskFor(t, group, space, key)
	id, err := member.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Space: space, Key: key, Memory: member, LoopbackOnly: true, Addrs: []string{addr}}
	for i := 0; i < 3; i++ {
		if err := o.ask(t.Context(), addr, id); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if held, _ := member.HasSpace(t.Context(), space); !held {
		t.Fatal("the member's pod does not hold the space after three admissions")
	}
	// That three admissions are one membership is lore's promise and lore's test
	// (TestGrantMembershipIsIdempotent); what this asserts is that repeating the
	// exchange is not an error and does not leave the member worse off.
}

// TestTheDeskRefusesWhatItShould is the authority boundary on kenward's side.
func TestTheDeskRefusesWhatItShould(t *testing.T) {
	group := newPod(t)
	space := newSpaceID(t)
	if _, err := group.CreateSpaceWithID(t.Context(), string(space), "household"); err != nil {
		t.Fatal(err)
	}
	addr := deskFor(t, group, space, key)

	t.Run("a stranger without the household link key", func(t *testing.T) {
		stranger := newPod(t)
		// Join never gives up, deliberately, so the bound here is time. What is
		// asserted is the store afterwards: whatever it did, it was not admitted.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_ = Join(ctx, Options{
			Space: space, Key: []byte("some-other-key-entirely-long-enough"),
			Memory: stranger, LoopbackOnly: true, Addrs: []string{addr},
			Retry: 50 * time.Millisecond,
		})
		if held, _ := stranger.HasSpace(t.Context(), space); held {
			t.Fatal("a store without the household link key was admitted")
		}
	})

	t.Run("a member of a different household", func(t *testing.T) {
		other := newPod(t)
		id, err := other.Identity(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// The right key, the wrong space: the desk serves one space and admits to
		// no other, even for a caller that can MAC its request.
		wrong := newSpaceID(t)
		req := request{Space: string(wrong), AccountID: id.AccountID, EncPub: id.EncPub,
			EncPubSig: id.EncPubSig, Nonce: randomHex(16)}
		req.MAC = req.expectedMAC(key)
		if code := post(t, addr, req); code != http.StatusForbidden {
			t.Fatalf("the desk answered %d for a space it does not serve, want 403", code)
		}
	})

	t.Run("a forged MAC", func(t *testing.T) {
		other := newPod(t)
		id, _ := other.Identity(t.Context())
		req := request{Space: string(space), AccountID: id.AccountID, EncPub: id.EncPub,
			EncPubSig: id.EncPubSig, Nonce: randomHex(16), MAC: randomHex(32)}
		if code := post(t, addr, req); code != http.StatusForbidden {
			t.Fatalf("the desk answered %d for a forged MAC, want 403", code)
		}
	})

	t.Run("an identity whose encryption key is not its own", func(t *testing.T) {
		a, b := newPod(t), newPod(t)
		ida, _ := a.Identity(t.Context())
		idb, _ := b.Identity(t.Context())
		// A correctly MACed request — the caller does hold the link key — pairing
		// one account with another's encryption key. lore refuses it, which is
		// where that check belongs, and the desk reports the failure rather than
		// signing a member list nobody can use.
		req := request{Space: string(space), AccountID: ida.AccountID, EncPub: idb.EncPub,
			EncPubSig: ida.EncPubSig, Nonce: randomHex(16)}
		req.MAC = req.expectedMAC(key)
		if code := post(t, addr, req); code != http.StatusInternalServerError {
			t.Fatalf("the desk answered %d for an unbound encryption key, want 500", code)
		}
	})
}

// TestAMemberRefusesAnImpostorDesk is the other direction, and the one that would
// hurt: a desk on the same bridge that owns a space of its own at the household's
// configured id. AcceptGrant would overwrite the real space with it, so the member
// must refuse before applying anything.
func TestAMemberRefusesAnImpostorDesk(t *testing.T) {
	impostor, member := newPod(t), newPod(t)
	space := newSpaceID(t)
	// The impostor really does own a space at that id — CreateSpaceWithID accepts
	// any id, which is exactly why the id alone is not authority.
	if _, err := impostor.CreateSpaceWithID(t.Context(), string(space), "not the household"); err != nil {
		t.Fatal(err)
	}
	addr := deskFor(t, impostor, space, []byte("the-impostors-own-key-long-enough!!"))

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_ = Join(ctx, Options{Space: space, Key: key, Memory: member,
		LoopbackOnly: true, Addrs: []string{addr}, Retry: 10 * time.Millisecond})

	if held, _ := member.HasSpace(t.Context(), space); held {
		t.Fatal("the member's pod adopted an impostor's space at the household's id")
	}
}

// TestAMemberRefusesAnUnsignedAnswer is the same attack with the impostor holding
// a real grant it is entitled to: it strips the MAC and hands the grant over. The
// member must not apply it.
func TestAMemberRefusesAnUnsignedAnswer(t *testing.T) {
	impostor, member := newPod(t), newPod(t)
	space := newSpaceID(t)
	if _, err := impostor.CreateSpaceWithID(t.Context(), string(space), "not the household"); err != nil {
		t.Fatal(err)
	}
	id, err := member.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	grant, err := impostor.Grant(t.Context(), space, id)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(response{
			Grant: base64.StdEncoding.EncodeToString(grant),
			MAC:   randomHex(32),
		})
	}))
	defer srv.Close()

	o := Options{Space: space, Key: key, Memory: member, LoopbackOnly: true}
	err = o.ask(t.Context(), strings.TrimPrefix(srv.URL, "http://"), id)
	if err == nil {
		t.Fatal("a grant with an unsigned answer was applied")
	}
	if !strings.Contains(err.Error(), "household link key") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if held, _ := member.HasSpace(t.Context(), space); held {
		t.Fatal("the member's pod holds a space it should have refused")
	}
}

// TestOptionsRefuseAnEmptyKey: a household whose link key variable is set to
// nothing must not get a desk whose gate opens for the empty string.
func TestOptionsRefuseAnEmptyKey(t *testing.T) {
	m := newPod(t)
	for _, k := range [][]byte{nil, []byte(""), []byte("short")} {
		o := Options{Space: newSpaceID(t), Key: k, Memory: m}
		if err := Serve(t.Context(), o); err == nil {
			t.Fatalf("Serve accepted a %d-byte link key", len(k))
		}
		if err := Join(t.Context(), o); err == nil {
			t.Fatalf("Join accepted a %d-byte link key", len(k))
		}
	}
}

func post(t *testing.T, addr string, req request) int {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+addr+Path, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("posting to the desk: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
