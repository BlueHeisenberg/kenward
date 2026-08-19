package link

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/BlueHeisenberg/agentmesh/pkg/discovery"

	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// Join runs a member pod's half until this store holds the household's shared
// space or ctx is cancelled.
//
// It checks first and asks second, so the ordinary case — a pod that was admitted
// on some earlier boot and kept its volume — costs one local lookup and stops.
// Otherwise it browses for the desk, asks, applies what it is given, and tries
// again on the retry interval until it succeeds. There is no attempt limit,
// because there is no number of attempts after which giving up is the right
// answer: the group's pod may simply not be up yet, and a member whose assistant
// silently stopped trying is the failure this package exists to remove.
//
// It blocks and returns nil when it is done or when ctx is cancelled. Failures are
// logged and retried, never returned: shared memory arriving late is a degraded
// household and refusing to serve over it would be a stopped one.
func Join(ctx context.Context, opts Options) error {
	if err := opts.check(); err != nil {
		return err
	}
	log := opts.log()
	for {
		switch held, err := opts.Memory.HasSpace(ctx, opts.Space); {
		case err != nil:
			log.Warn("kenward", "event", "link",
				"detail", "could not tell whether this pod holds the household's shared space", "err", err.Error())
		case held:
			return nil
		default:
			if err := opts.askOnce(ctx); err != nil {
				log.Warn("kenward", "event", "link",
					"detail", "this pod is not yet in the household's shared space; it will keep asking",
					"space", string(opts.Space), "err", err.Error())
			} else {
				log.Info("kenward", "event", "link",
					"detail", "this pod was admitted to the household's shared space; the household's memory is now readable and writable here",
					"space", string(opts.Space))
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.retry()):
		}
	}
}

// askOnce finds the desk and tries every candidate until one admits this store.
func (o Options) askOnce(ctx context.Context) error {
	id, err := o.Memory.Identity(ctx)
	if err != nil {
		return err
	}
	addrs := o.Addrs
	if len(addrs) == 0 {
		addrs = o.find(ctx)
	}
	if len(addrs) == 0 {
		return errors.New("no household link desk found on this network (the group's pod may not be up yet)")
	}
	var last error = errors.New("no household link desk answered")
	for _, addr := range addrs {
		if err := o.ask(ctx, addr, id); err != nil {
			last = fmt.Errorf("%s: %w", addr, err)
			continue
		}
		return nil
	}
	return last
}

// find browses mDNS for the group pod's desk.
func (o Options) find(ctx context.Context) []string {
	ifaces, _ := ifacesAndIPs(!o.LoopbackOnly)
	// A registry drops the peer whose id matches its own, and this pod's desk — if
	// it had one — would advertise under the space id. A member's pod has no desk,
	// so a self id that matches nothing is right, and a random one is the safest
	// way to say so.
	reg := discovery.NewRegistry(peerID("member-" + randomHex(16)))
	bctx, cancel := context.WithTimeout(ctx, o.browse())
	defer cancel()
	_ = discovery.Browse(bctx, Service, reg, ifaces)
	var out []string
	for _, p := range reg.List() {
		if p.Addr != "" {
			out = append(out, p.Addr)
		}
	}
	return out
}

// ask performs the exchange against one address and applies what comes back.
func (o Options) ask(ctx context.Context, addr string, id memory.Identity) error {
	nonce := randomHex(16)
	req := request{
		Space:     string(o.Space),
		AccountID: id.AccountID,
		EncPub:    id.EncPub,
		EncPubSig: id.EncPubSig,
		Nonce:     nonce,
	}
	req.MAC = req.expectedMAC(o.Key)
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+Path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	post.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(post)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("the desk answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var res response
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&res); err != nil {
		return err
	}

	// The response MAC before anything else. Without it an impostor on the bridge
	// could hand this pod a grant into a space of its own creation at the
	// household's configured id, and AcceptGrant would overwrite the real one —
	// which is this member's assistant quietly reading and writing a stranger's
	// memory in place of the household's.
	if !equalMAC(res.MAC, grantMAC(o.Key, req.Space, req.AccountID, nonce, res.Grant)) {
		return errors.New("the answer was not signed with the household link key")
	}
	grant, err := base64.StdEncoding.DecodeString(res.Grant)
	if err != nil {
		return errors.New("the answer did not carry a readable grant")
	}
	sp, err := o.Memory.AcceptGrant(ctx, grant)
	if err != nil {
		return err
	}
	// Belt and braces on top of the MAC: a desk that answered with a grant into
	// some other space has misconfigured itself, and applying it would have put a
	// space here under the wrong id.
	if sp.ID != string(o.Space) {
		return fmt.Errorf("the desk granted space %s, not the household's %s", sp.ID, o.Space)
	}
	return nil
}

// -----------------------------------------------------------------------------
// shared helpers
// -----------------------------------------------------------------------------

// ifacesAndIPs is lore's rule, spelled here because it is internal there: mDNS on
// loopback always, plus the multicast-capable interfaces when the network beyond
// this process matters — which inside a pod means the container bridge, and that
// is precisely the set of pods this household is made of.
func ifacesAndIPs(lan bool) ([]net.Interface, []string) {
	ifaces := discovery.LoopbackInterfaces()
	ips := []string{"127.0.0.1"}
	if lan {
		ifaces = append(ifaces, discovery.NonLoopbackMulticastInterfaces()...)
		ips = append(ips, discovery.LANIPv4s()...)
	}
	return ifaces, ips
}

// peerID pads a value to the sixteen characters agentmesh's Advertise slices for
// an mDNS instance name. A space id is already longer; the padding is here so a
// short one is a shorter name and never a panic.
func peerID(s string) string {
	for len(s) < 16 {
		s += "0"
	}
	return s
}

// randomHex is n bytes of crypto/rand as hex. A nonce that cannot be generated is
// not a condition worth a code path — crypto/rand does not fail on any platform
// kenward runs on — so it panics rather than returning a predictable value.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("link: no entropy: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// short abbreviates an account id for a log line.
func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// memoryIdentity is the request's identity fields, as internal/memory wants them.
func memoryIdentity(r request) memory.Identity {
	return memory.Identity{AccountID: r.AccountID, EncPub: r.EncPub, EncPubSig: r.EncPubSig}
}
