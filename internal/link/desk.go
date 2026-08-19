package link

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/BlueHeisenberg/agentmesh/pkg/discovery"
)

// Serve runs the group pod's link desk until ctx is cancelled.
//
// It listens for grant requests and advertises itself over mDNS so a member's pod
// can find it without an address being written down anywhere. It blocks; run it in
// a goroutine, cancel ctx to stop it, and by the time it returns the listener is
// closed and the advertisement is withdrawn.
//
// It runs for the life of the pod rather than until every member has been
// admitted, because "every member" is not a set this pod knows the end of: a
// member added to a running household brings up a pod that has to find a desk, and
// so does a member whose volume was replaced.
//
// A non-nil return is a failure to start — a listener that will not bind, an mDNS
// registration that will not take. It is not fatal to the household and callers do
// not treat it as such: the group's own memory is unaffected, and what a broken
// desk costs is new members not being admitted, which `kenward doctor` reports.
func Serve(ctx context.Context, opts Options) error {
	if err := opts.check(); err != nil {
		return err
	}
	log := opts.log()

	host := "127.0.0.1"
	if !opts.LoopbackOnly {
		host = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, opts.Port))
	if err != nil {
		return fmt.Errorf("link: the household link desk could not bind: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Path, func(w http.ResponseWriter, r *http.Request) {
		opts.handleGrant(w, r)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	stopAdv := func() {}
	if !opts.NoDiscovery {
		ifaces, ips := ifacesAndIPs(!opts.LoopbackOnly)
		// The advertisement's peer id has to be at least sixteen characters —
		// agentmesh slices it for the mDNS instance name — and has to be stable
		// for this pod, so a recreated desk replaces its own record rather than
		// adding a second. The space id is both, and it is not a secret: it is in
		// kenward.yaml on every host that runs this household.
		stop, err := discovery.Advertise(Service, "kenward-link", peerID(string(opts.Space)), port, ifaces, ips)
		if err != nil {
			ln.Close()
			return fmt.Errorf("link: the household link desk could not advertise itself: %w", err)
		}
		stopAdv = stop
	}

	log.Info("kenward", "event", "link",
		"detail", "the household link desk is listening; a member's pod is admitted to the household's shared space when it asks",
		"port", port, "space", string(opts.Space))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("kenward", "event", "link", "detail", "the household link desk stopped", "err", err.Error())
		}
	}()

	<-ctx.Done()
	stopAdv()
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	<-done
	return nil
}

// handleGrant is the desk. It answers one question — "may this store into the
// household's shared space?" — and the answer is yes exactly when the asker proves
// it holds the household link key and names the space this desk owns.
//
// It deliberately says very little in its refusals. A caller that got the key
// wrong and one that got the space wrong are the same 403 to anybody probing the
// bridge; the reasons go to this pod's log, where the operator is.
func (o Options) handleGrant(w http.ResponseWriter, r *http.Request) {
	log := o.log()
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	deny := func(detail string) {
		log.Warn("kenward", "event", "link",
			"detail", "a request to join the household's shared space was refused: "+detail)
		http.Error(w, "refused", http.StatusForbidden)
	}
	// The space first, and by equality rather than by "a space I hold": this desk
	// admits members to the household's shared space and to nothing else it may
	// happen to own.
	if req.Space != string(o.Space) {
		deny("it named a space this unit does not serve")
		return
	}
	if !equalMAC(req.MAC, req.expectedMAC(o.Key)) {
		deny("it did not prove it holds the household link key")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	grant, err := o.Memory.Grant(ctx, o.Space, memoryIdentity(req))
	if err != nil {
		// Not a 403: the asker did everything right and this pod could not do its
		// half. The usual cause is this pod not owning the space, which is a
		// configuration fault an operator has to see.
		log.Error("kenward", "event", "link",
			"detail", "a member's pod asked to join the household's shared space and this unit could not admit it",
			"account", short(req.AccountID), "err", err.Error())
		http.Error(w, "cannot grant", http.StatusInternalServerError)
		return
	}
	blob := base64.StdEncoding.EncodeToString(grant)
	log.Info("kenward", "event", "link",
		"detail", "admitted a store to the household's shared space",
		"account", short(req.AccountID), "space", string(o.Space))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{
		Grant: blob,
		MAC:   grantMAC(o.Key, req.Space, req.AccountID, req.Nonce, blob),
	})
}
