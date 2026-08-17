//go:build integration && linux

package main

// The third scope, against real pods.
//
// # What this file exists for
//
// domain.ScopeHousehold — a member's private chat with kenward's own bot, under
// household.agents: per_member — is the authorization boundary the whole third-scope
// design exists to enforce, and until this file it had never run in a container. Two
// things kept it out of reach:
//
//   - per_member needs mode: isolated, which needs Linux and podman. internal/e2e does
//     cover the third scope well — single_test.go and injection_test.go drive it through
//     the real scope resolver and the real capture engine — but in-process, against a
//     fake transport and a fake memory.Memory, in something the package calls a "pod" and
//     which is a goroutine. TestIsolatedPodman is the only test that starts containers,
//     and its householdYAML never sets `agents: per_member`, so every container it has
//     ever started ran the two-scope household. The third scope had never met a real
//     container, a real lore store or a real Bot API.
//   - per_member needs one Telegram bot per member plus one for the household, and a
//     developer has one token. So the scopes could not be told apart on the wire: a
//     private chat's id is the member's own account id and is identical on every bot,
//     and the bot the message arrived on is the *only* thing that separates "david's
//     chat with his own agent" from "david's chat with kenward" (see internal/scope).
//
// The second is the one that made this look impossible, and it is the one that is not.
// The bot a household needs is an endpoint, not an account: transport.WithAPIServer
// exists because go-telegram/bot lets the API root be redirected, and
// internal/transport/telegramtest is already a faithful stand-in for api.telegram.org.
// What could not be done before is pointing a *pod* at it, because nothing in
// kenward.yaml names a Bot API root and nothing should — a household must not be able
// to misconfigure where its bot lives.
//
// So it is done from outside the product, and entirely from outside: podman's
// base_hosts_file resolves api.telegram.org to the host, and keel's sandbox.Spec.Files
// provisions a CA bundle that trusts the stand-in's certificate. The pod's binary is
// unmodified, its configuration is unmodified, and it dials https://api.telegram.org
// exactly as it does in production. Three bots then cost nothing, and the third scope
// becomes reachable.
//
// # What is real here and what is not
//
// Real: podman and the pods, the image built from this repository's Dockerfile, the
// real `lore` binary initialising and serving a real store on each pod's own volume,
// real cross-pod lore sync (D-044) provisioned by the documented operator recipe, the
// real supervisor and the real cmd/kenward wiring, real scope resolution and real
// assistant units inside the containers, and — where a scenario says so — real
// inference against the live model.
//
// Not real: the Bot API endpoint. It is telegramtest, reached over TLS as
// api.telegram.org. Every assertion below about who was answered and what was written
// is therefore a statement about a real pod driven by a stand-in transport, and never
// about a live Telegram account. TestPerMemberLiveTelegram, in the same directory, is
// the one that uses the real token, and it deliberately asserts far less.
//
// Every memory assertion is made by a **separate lore process** run by the host against
// the pod's own volume — never by reading kenward's logs, which are the thing under
// test.
//
// # Running it
//
//	go test -tags integration -run TestThirdScopeAgainstPods -timeout 40m -v ./cmd/kenward/
//
// It needs root (it binds :443 to be api.telegram.org) and a podman whose containers
// can reach the host. It skips, rather than fails, where the host cannot provide that.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/sandbox"

	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/transport/telegramtest"
)

// podCABundlePath is where the distroless base keeps the trust store the pod's Go
// runtime reads. Overwriting it with the system bundle plus one extra CA is the whole
// of what makes the stand-in reachable over TLS from inside a pod.
const podCABundlePath = "/etc/ssl/certs/ca-certificates.crt"

// hostFromPod is the address a container reaches this host on. podman writes it into
// every container's /etc/hosts as host.containers.internal; it is named numerically
// here because base_hosts_file takes an address, not an alias.
//
// It is discovered rather than assumed — see requireHostReachableFromPod, which skips
// the test on a host where it is wrong instead of failing obscurely twenty minutes in.
const hostFromPod = "10.255.255.254"

// -----------------------------------------------------------------------------
// the stand-in api.telegram.org
// -----------------------------------------------------------------------------

// botAPI is one TLS endpoint answering for api.telegram.org on behalf of several bots.
//
// telegramtest.Server answers for exactly one token, which is right: "the transport
// sent the token it was given" is an assertion worth keeping. A per_member household
// has one bot per member plus the household's, so this multiplexes by the token in the
// request path — the same fact the inner server checks again — and hands each request
// to that bot's own server.
type botAPI struct {
	byToken map[string]*telegramtest.Server
	ln      net.Listener
	srv     *http.Server
	caPEM   []byte
}

// newBotAPI starts the stand-in on :443 with a certificate for api.telegram.org, and
// returns it with the CA that signs it. One server is created per token.
func newBotAPI(t *testing.T, tokens map[string]string) *botAPI {
	t.Helper()

	caPEM, certPEM, keyPEM := selfSignedFor(t, "api.telegram.org")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("loading the stand-in's certificate: %v", err)
	}

	api := &botAPI{byToken: map[string]*telegramtest.Server{}, caPEM: caPEM}
	for label, token := range tokens {
		srv := telegramtest.New(t, token)
		api.byToken[token] = srv
		t.Logf("stand-in bot %q -> %s", label, srv.URL())
	}

	ln, err := net.Listen("tcp", ":443")
	if err != nil {
		t.Skipf("cannot bind :443 to stand in for api.telegram.org (%v); "+
			"this test needs it to redirect a pod's Bot API calls without changing the product", err)
	}
	api.ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	api.srv = &http.Server{Handler: http.HandlerFunc(api.route)}

	go func() { _ = api.srv.Serve(api.ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = api.srv.Shutdown(ctx)
	})
	return api
}

// route forwards one call to the server for the bot whose token is in its path. An
// unknown token is answered with Telegram's own 401 rather than a proxy error, because
// that is what a pod holding a token this household never issued must see.
func (a *botAPI) route(w http.ResponseWriter, r *http.Request) {
	prefix, _, ok := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
	token := strings.TrimPrefix(prefix, "bot")
	srv, known := a.byToken[token]
	if !ok || !known {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
		return
	}
	target, err := url.Parse(srv.URL())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// The path carries the token and the method and must survive untouched; only the
	// scheme and host change.
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
	}
	proxy.ServeHTTP(w, r)
}

// bot returns the stand-in server for one token.
func (a *botAPI) bot(t *testing.T, token string) *telegramtest.Server {
	t.Helper()
	srv, ok := a.byToken[token]
	if !ok {
		t.Fatalf("no stand-in bot for that token")
	}
	return srv
}

// selfSignedFor mints a throwaway CA and one leaf certificate for host. Both live for
// the length of the test run and neither is written anywhere outside it.
func selfSignedFor(t *testing.T, host string) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kenward third-scope e2e CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("self-signing the CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing the CA: %v", err)
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("signing the leaf: %v", err)
	}

	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})
	return caPEM, certPEM, keyPEM
}

// -----------------------------------------------------------------------------
// getting the pods to talk to it
// -----------------------------------------------------------------------------

// caInjectingBackend adds one file to every pod: the image's own CA bundle with the
// stand-in's CA appended.
//
// It is a Backend wrapper rather than a supervisor option because there is no
// supervisor option for it and there must not be — a household that could add a
// trusted root to its pods from kenward.yaml is a household that can be talked into
// trusting anything. keel's Spec is the last place the test owns before podman.
type caInjectingBackend struct {
	sandbox.Backend
	bundle []byte
}

func (b *caInjectingBackend) withCA(spec sandbox.Spec) sandbox.Spec {
	spec.Files = append(append([]sandbox.File(nil), spec.Files...), sandbox.File{
		Path: podCABundlePath,
		Data: b.bundle,
		Mode: 0o644,
		UID:  podUID,
		GID:  podGID,
	})
	return spec
}

func (b *caInjectingBackend) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Handle, error) {
	return b.Backend.Create(ctx, b.withCA(spec))
}

func (b *caInjectingBackend) Recreate(ctx context.Context, spec sandbox.Spec) (sandbox.Handle, error) {
	return b.Backend.Recreate(ctx, b.withCA(spec))
}

var _ sandbox.Backend = (*caInjectingBackend)(nil)

// imageCABundle copies the trust store out of the pod image, so the bundle the pods get
// is the real one plus the stand-in's CA rather than the stand-in's CA alone. A pod
// that trusted only the test's root would still reach the stand-in and would quietly
// stop being able to reach anything else, which is a difference worth not introducing.
func imageCABundle(t *testing.T, r *rig) []byte {
	t.Helper()
	name := fmt.Sprintf("kwe2e-ca-%d", time.Now().UnixNano())
	if out, err := r.try(t, "create", "--name", name, r.image, "version"); err != nil {
		t.Fatalf("creating a container to read the image's CA bundle: %v\n%s", err, out)
	}
	defer func() { _, _ = r.try(t, "rm", "-f", name) }()

	dst := filepath.Join(t.TempDir(), "ca-certificates.crt")
	if out, err := r.try(t, "cp", name+":"+podCABundlePath, dst); err != nil {
		t.Fatalf("copying %s out of %s: %v\n%s", podCABundlePath, r.image, err, out)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading the image's CA bundle: %v", err)
	}
	return b
}

// redirectTelegramToHost points every container podman starts from now on at this host
// for api.telegram.org.
//
// It does it through CONTAINERS_CONF — a scratch copy of the machine's own
// configuration with one line added — and never by editing /etc. The variable is set on
// this process, so it reaches podman through the environment keel's backend inherits and
// affects nothing else on the machine and nothing after the test.
func redirectTelegramToHost(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	writeFile(t, hosts, []byte(hostFromPod+" api.telegram.org\n"), 0o644)

	base, err := os.ReadFile("/etc/containers/containers.conf")
	if err != nil {
		// Not fatal: the machine may keep its settings elsewhere, and the only thing
		// this file must carry is the added line.
		t.Logf("no /etc/containers/containers.conf to extend (%v); writing a fresh one", err)
		base = []byte("[containers]\n")
	}
	conf := filepath.Join(dir, "containers.conf")
	writeFile(t, conf, append(append([]byte(nil), base...),
		[]byte(fmt.Sprintf("\nbase_hosts_file = %q\n", hosts))...), 0o644)

	t.Setenv("CONTAINERS_CONF", conf)
}

// requireHostReachableFromPod checks the two host facts this file depends on before any
// of the expensive work: that a container resolves api.telegram.org to this host, and
// that this host answers there. Either being false is a property of the machine, not of
// kenward, so it skips.
func requireHostReachableFromPod(t *testing.T, r *rig) {
	t.Helper()

	// Something must be listening for the check to mean anything; the stand-in is not
	// up yet, so use a throwaway listener on a throwaway port.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skipf("cannot listen on this host at all: %v", err)
	}
	defer ln.Close()
	go func() {
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("reachable"))
		})}
		_ = srv.Serve(ln)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	out, err := r.try(t, "run", "--rm", r.image, "version")
	if err != nil {
		t.Fatalf("the pod image will not run at all: %v\n%s", err, out)
	}

	// busybox rather than the pod image: the pod image is distroless and has no way to
	// make an HTTP request from a shell.
	probe := fmt.Sprintf("wget -q -T 5 -O - http://api.telegram.org:%d/ || echo UNREACHABLE", port)
	got, err := r.try(t, "run", "--rm", "docker.io/library/busybox:latest", "sh", "-c", probe)
	if err != nil {
		t.Skipf("cannot run a probe container: %v\n%s", err, got)
	}
	if !strings.Contains(got, "reachable") {
		t.Skipf("a container on this host does not reach the host as api.telegram.org:%d — "+
			"base_hosts_file or %s is wrong here, so a pod cannot be pointed at the stand-in:\n%s",
			port, hostFromPod, got)
	}
}

// -----------------------------------------------------------------------------
// the per_member household
// -----------------------------------------------------------------------------

// perMemberYAML is householdYAML's shape with the one line that has never been in a
// pod: household.agents: per_member. Two enrolled members, each with their own bot and
// their own agent, plus kenward on the household's own bot.
//
// The endpoint is the live model, named by address: these pods run real inference. The
// space ids are placeholders that provisionSpaces replaces with the ids the pods' own
// lore stores actually mint, because a space id cannot be chosen from outside — see
// docs/IMPLEMENTATION.md §8.
const perMemberYAML = `mode: isolated
household:
  name: Ashfield
  agents: per_member
  shared_space: %SHARED%
  group_chat_id: -1001234567890
  tiers: [local]
telegram:
  bot_token_env: KENWARD_BOT_TOKEN_HOUSEHOLD
members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: %DAVID%
    tiers: [local]
    bot_token_env: KENWARD_BOT_TOKEN_DAVID
    passphrase_env: KENWARD_PASSPHRASE_DAVID
  - id: jordan
    name: Jordan
    telegram_id: 87654321
    private_space: %JORDAN%
    tiers: [local]
    bot_token_env: KENWARD_BOT_TOKEN_JORDAN
    passphrase_env: KENWARD_PASSPHRASE_JORDAN
endpoints:
  - name: monster
    base_url: %MODEL%
    model: %MODELNAME%
    tags: [local]
    timeout: 180s
memory:
  lore_command: [lore, mcp]
`

// The telegram ids perMemberYAML enrols, and one it does not.
const (
	davidTelegramID   = 12345678
	jordanTelegramID  = 87654321
	strangerTelegramI = 99999999
	groupChatID       = -1001234567890
)

// liveModelURL and liveModelName name the model these pods actually call. They are
// overridable so the file is not pinned to one developer's network.
func liveModelURL() string {
	if v := os.Getenv("KENWARD_E2E_MODEL_URL"); v != "" {
		return v
	}
	return "http://192.168.1.20:8000/v1"
}

func liveModelName() string {
	if v := os.Getenv("KENWARD_E2E_MODEL"); v != "" {
		return v
	}
	return "monster"
}

// placeholderSpaces are valid uuids that no lore store holds. They exist only so the
// configuration parses before the pods have minted their real ones.
const (
	placeholderShared = "dac31e70-72e4-4b10-9cef-a6276c4a87b8"
	placeholderDavid  = "7d5047bb-d939-4539-b3db-8b6221a2e245"
	placeholderJordan = "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19"
)

func perMemberConfig(modelURL, shared, david, jordan string) string {
	return strings.NewReplacer(
		"%SHARED%", shared,
		"%DAVID%", david,
		"%JORDAN%", jordan,
		"%MODEL%", modelURL,
		"%MODELNAME%", liveModelName(),
	).Replace(perMemberYAML)
}

// perMemberEnv is householdEnv's subset for this household, with the tokens the
// stand-in will answer for.
func perMemberEnv() map[string]string {
	return map[string]string{
		"KENWARD_BOT_TOKEN_HOUSEHOLD": e2eGroupToken,
		"KENWARD_BOT_TOKEN_DAVID":     e2eDavidToken,
		"KENWARD_BOT_TOKEN_JORDAN":    e2eJordanToken,
		"KENWARD_PASSPHRASE_DAVID":    e2eDavidPass,
		"KENWARD_PASSPHRASE_JORDAN":   e2eJordanPass,
	}
}

// standInTokens is every bot the stand-in answers for, by the label a log line uses.
func standInTokens() map[string]string {
	return map[string]string{
		"kenward (household)": e2eGroupToken,
		"david's own agent":   e2eDavidToken,
		"jordan's own agent":  e2eJordanToken,
	}
}

// -----------------------------------------------------------------------------
// driving and observing
// -----------------------------------------------------------------------------

// podLore runs the real lore binary, on the host, against one pod's own store, and is
// the only witness this file trusts for what a pod's memory holds.
//
// It reads the pod's volume through its mountpoint, which is a thing only the host can
// do and only because this is a test. Nothing kenward logs is evidence here.
func (hh *household) podLore(pod string, args ...string) string {
	hh.t.Helper()
	// Reading the store as root can leave journal files the pod's own uid cannot write,
	// which would break the pod the next time it starts. Hand the volume back.
	defer chownTree(hh.t, hh.mountpoint(pod), podUID, podGID)
	return hh.lore(hh.workFile(pod, "lore"), "", args...)
}

// podSearch asks one pod's store, through a separate lore process, whether it holds
// anything matching query in space.
//
// A failed search is returned rather than fatal, for the reason the sibling file's
// loreSearch gives: "there is no such space here" is one of the answers this file's
// questions have, and the assertion that names what was or was not found is a better
// failure than a helper aborting on an exit code.
func (hh *household) podSearch(pod, space, query string) string {
	hh.t.Helper()
	defer chownTree(hh.t, hh.mountpoint(pod), podUID, podGID)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hh.rig.loreBin, "search", "-space", space, query)
	cmd.Env = append(os.Environ(), "LORE_HOME="+hh.workFile(pod, "lore"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out) + "\n(lore search exited: " + err.Error() + ")"
	}
	return string(out)
}

// podSpaces lists the space ids one pod's store holds, as a separate lore process sees
// them.
func (hh *household) podSpaces(pod string) []string {
	hh.t.Helper()
	return allUUIDs(hh.podLore(pod, "spaces"))
}

// podHolds reports whether a pod's store holds a space at all. It is the pod-boundary
// half of the third scope's guarantee: the group's pod cannot read david's private
// space partly because scope.Resolve never names it, and partly because it is not
// there.
func (hh *household) podHolds(pod, space string) bool {
	for _, s := range hh.podSpaces(pod) {
		if s == space {
			return true
		}
	}
	return false
}

// waitForPod blocks until a pod's log says it is serving, which under the stand-in it
// can actually do — every earlier podman test could only wait for it to die at getMe.
func (hh *household) waitForPod(pod, want string, within time.Duration) {
	t := hh.t
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if hh.containerExists(pod) && strings.Contains(hh.logs(pod), want) {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("pod %s never logged %q within %s; its log ends:\n%s",
		pod, want, within, lastLine(hh.logs(pod)))
}

// exec runs a command inside a running pod. It is how the operator's out-of-band lore
// membership recipe is performed — docs/IMPLEMENTATION.md §8 spells it as exactly this.
func (hh *household) exec(pod string, args ...string) (string, error) {
	hh.t.Helper()
	return hh.rig.try(hh.t, append([]string{"exec", hh.container(pod)}, args...)...)
}

func (hh *household) mustExec(pod string, args ...string) string {
	hh.t.Helper()
	out, err := hh.exec(pod, args...)
	if err != nil {
		hh.t.Fatalf("podman exec %s %s: %v\n%s", pod, strings.Join(args, " "), err, out)
	}
	return out
}

// allUUIDs pulls every uuid out of a lore command's output, in order.
func allUUIDs(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F')
	}) {
		if len(f) == 36 && strings.Count(f, "-") == 4 {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// botWatch is a view of one stand-in bot from a point in time.
//
// The stand-in servers outlive a scenario — one set is built for the whole suite,
// because a telegramtest.Server answers for one token and building three of them per
// subtest would mean rebuilding the TLS front too. So "was this chat answered?" has to
// mean "since this scenario started", and a helper that asked the server for all of its
// calls would read a previous scenario's replies as this one's. It did, and turned a
// working group gate into a reported silence failure.
type botWatch struct {
	srv   *telegramtest.Server
	since int
}

// watch marks the current point in a bot's history. Everything the scenario asserts is
// relative to it.
func watch(srv *telegramtest.Server) botWatch {
	return botWatch{srv: srv, since: srv.CountFor("sendMessage")}
}

// toChat returns the text of every sendMessage to one chat since the mark.
func (w botWatch) toChat(chatID int64) []string {
	var out []string
	want := fmt.Sprint(chatID)
	calls := w.srv.CallsFor("sendMessage")
	if w.since > len(calls) {
		return nil
	}
	for _, c := range calls[w.since:] {
		if c.Form.Get("chat_id") == want {
			out = append(out, c.Form.Get("text"))
		}
	}
	return out
}

// waitFor waits until at least n messages have been sent to one chat since the mark. It
// is a poll rather than telegramtest.WaitCall because the interesting question is per
// chat: a reply to the group is not a reply to david.
func (w botWatch) waitFor(t *testing.T, chatID int64, n int, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if got := w.toChat(chatID); len(got) >= n {
			return got
		}
		time.Sleep(500 * time.Millisecond)
	}
	return w.toChat(chatID)
}

// quiet asserts that a chat received nothing at all for the whole of within. Silence is
// the designed answer on several paths here and it is the one that cannot be observed by
// waiting for a reply.
func (w botWatch) quiet(t *testing.T, chatID int64, within time.Duration, what string) {
	t.Helper()
	time.Sleep(within)
	if got := w.toChat(chatID); len(got) > 0 {
		t.Errorf("%s: chat %d was answered, and the design says silence:\n%q", what, chatID, got)
	}
}

// -----------------------------------------------------------------------------
// the model gateway
// -----------------------------------------------------------------------------

// modelGateway is the endpoint the pods' tier chain points at. It does two jobs that
// have to be done by one thing, because a pod may only be given one base_url:
//
//   - it records every request, which is the only way to ask what a conversation's
//     prompt actually contained. "What david told kenward in private never reaches the
//     group's context" is a statement about a prompt, and no assertion on a reply can
//     substitute for reading it.
//   - it either forwards the turn to the live model — real inference, from inside a
//     real pod — or answers it from a script.
//
// The script exists for one reason. Two scenarios below turn on the assistant emitting
// a `remember` tool call, and whether a 27B model chooses to call a tool is not a
// property of kenward. Scripting those makes the assertion about the capture path
// rather than about the model's mood; every other scenario runs live and says so.
type modelGateway struct {
	upstream string
	port     int

	mu       sync.Mutex
	requests []gatewayRequest
	script   func(gatewayRequest) (string, bool)
}

// gatewayRequest is one chat completion as it left a pod.
type gatewayRequest struct {
	Pod  string // best-effort: which unit's turn this was, by the bot token in play
	Body []byte
}

// System returns the system prompt of the request, which is where a scope's retrieved
// entries and its disclosure end up.
func (g gatewayRequest) System() string { return g.role("system") }

// User returns the concatenated user turns, which is the conversation history a unit
// carried into this call.
func (g gatewayRequest) User() string { return g.role("user") }

// All returns the whole request body, for the assertion that a token appears nowhere in
// it at all — which is the one that matters for context isolation.
func (g gatewayRequest) All() string { return string(g.Body) }

func (g gatewayRequest) role(want string) string {
	var msg struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(g.Body, &msg); err != nil {
		return ""
	}
	var b strings.Builder
	for _, m := range msg.Messages {
		if m.Role == want {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func newModelGateway(t *testing.T, upstream string) *modelGateway {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skipf("cannot listen for the model gateway: %v", err)
	}
	g := &modelGateway{upstream: upstream, port: ln.Addr().(*net.TCPAddr).Port}
	srv := &http.Server{Handler: http.HandlerFunc(g.handle)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return g
}

// baseURL is what goes into endpoints[].base_url, addressed as a pod sees this host.
func (g *modelGateway) baseURL() string {
	return fmt.Sprintf("http://%s:%d/v1", hostFromPod, g.port)
}

// setScript installs a canned answer. The function returns the assistant message
// content — or a tool call, rendered by scriptedToolCall — and false to fall through to
// the live model.
func (g *modelGateway) setScript(fn func(gatewayRequest) (string, bool)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.script = fn
}

func (g *modelGateway) all() []gatewayRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]gatewayRequest, len(g.requests))
	copy(out, g.requests)
	return out
}

func (g *modelGateway) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = nil
}

func (g *modelGateway) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req := gatewayRequest{Body: body}
	g.mu.Lock()
	if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "chat/completions") {
		g.requests = append(g.requests, req)
	}
	script := g.script
	g.mu.Unlock()

	if script != nil && strings.Contains(r.URL.Path, "chat/completions") {
		if canned, ok := script(req); ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(canned))
			return
		}
	}

	// Live: forward to the real model, unchanged.
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method,
		strings.TrimSuffix(g.upstream, "/v1")+r.URL.Path, strings.NewReader(string(body)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	outReq.Header = r.Header.Clone()
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// completion renders a plain assistant reply in the OpenAI wire format.
func completion(text string) string {
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"content": text, "tool_calls": []any{}},
			"finish_reason": "stop",
		}},
	})
	return string(body)
}

// toolCompletion renders an assistant reply that calls `remember`.
func toolCompletion(title, body, target string) string {
	args, _ := json.Marshal(map[string]any{
		"title": title, "body": body,
		"domain": "household/logistics", "confidence": "provisional",
		"target": target,
	})
	out, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"content": "",
				"tool_calls": []map[string]any{{
					"id": "call_0", "type": "function",
					"function": map[string]any{"name": "remember", "arguments": string(args)},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	})
	return string(out)
}

// -----------------------------------------------------------------------------
// the suite
// -----------------------------------------------------------------------------

// perMemberRig is everything the scenarios below share: the stand-in Bot API, the CA
// the pods are given, and the image rig.
type perMemberRig struct {
	r      *rig
	api    *botAPI
	gw     *modelGateway
	bundle []byte
}

func newPerMemberRig(t *testing.T) *perMemberRig {
	t.Helper()
	// Before the rig: the rig captures the environment for its builds, and this is one
	// of the variables it should capture.
	redirectTelegramToHost(t)

	r := newRig(t)
	requireHostReachableFromPod(t, r)

	api := newBotAPI(t, standInTokens())
	gw := newModelGateway(t, liveModelURL())
	bundle := append(append([]byte(nil), imageCABundle(t, r)...), api.caPEM...)
	return &perMemberRig{r: r, api: api, gw: gw, bundle: bundle}
}

// household builds a per_member household whose pods trust the stand-in.
func (p *perMemberRig) household(t *testing.T, yaml string) *household {
	t.Helper()
	hh := newHousehold(t, p.r, yaml, perMemberEnv())
	bundle := p.bundle
	hh.wrapBackend = func(b sandbox.Backend) sandbox.Backend {
		return &caInjectingBackend{Backend: b, bundle: bundle}
	}
	return hh
}

// spaces are the three lore spaces a per_member household needs, as the pods' own
// stores minted them.
type spaces struct {
	shared, david, jordan string
}

// provision performs the operator's out-of-band step and nothing kenward could do for
// itself: it creates the household's shared space in the group's pod and each member's
// private space in that member's own pod, then rewrites the configuration with the ids
// the stores actually chose.
//
// This is docs/IMPLEMENTATION.md §8's recipe, run against the pods keel named. It has to
// be a recipe rather than a fixture because lore cannot create a space at an id somebody
// picked in advance — which is exactly why every earlier podman run could use fixed uuids
// in kenward.yaml and never notice: those runs died before any turn asked for a space.
//
// It returns the ids and leaves the household stopped, with the new configuration on
// disk and every pod's volume holding its own space.
func (p *perMemberRig) provision(t *testing.T, hh *household) spaces {
	t.Helper()

	david, jordan, group := hh.memberPod("david"), hh.memberPod("jordan"), hh.groupPod()
	pods := []string{david, jordan, group}

	sup, _ := hh.supervisorFor(p.r.image)
	var got spaces
	hh.start(sup, func() bool {
		for _, pod := range pods {
			if !hh.containerExists(pod) || !strings.Contains(hh.logs(pod), "supervisor: started") {
				return false
			}
		}
		// The pods are up, so the recipe can be run inside them, which is where it is
		// documented to run. Doing it here rather than after Stop is not incidental:
		// `lore space create` has to be the pod's own account, and the pod's account
		// only exists inside the pod.
		got = spaces{
			shared: createSpace(t, hh, group, "Ashfield household"),
			david:  createSpace(t, hh, david, "David private"),
			jordan: createSpace(t, hh, jordan, "Jordan private"),
		}
		return true
	})

	if got.shared == "" || got.david == "" || got.jordan == "" {
		t.Fatalf("provisioning did not yield three spaces: %+v", got)
	}
	t.Logf("provisioned spaces: shared=%s david=%s jordan=%s", got.shared, got.david, got.jordan)

	writeFile(t, hh.h.config, []byte(p.config(got.shared, got.david, got.jordan)), 0o644)

	// Force the pods to actually read it.
	//
	// This is a workaround for a real defect, not a tidy-up: a pod is given the
	// household's configuration at Create and Recreate time only, and nothing
	// afterwards notices that the file on the host has changed — see
	// TestPodConfigGoesStale, which is that defect on its own with nothing else in the
	// way. Without this the pods below would go on running the placeholder space ids,
	// every memory assertion would fail, and the failure would look like a broken third
	// scope rather than a configuration that never arrived.
	//
	// Removing the container and keeping the volume is the operator's remedy and is
	// also the right thing for the test: the spaces just created live on the volume, so
	// a pod rebuilt this way keeps them and reads the new configuration.
	for _, pod := range pods {
		if out, err := p.r.try(t, "rm", "-f", "-t", "1", hh.container(pod)); err != nil {
			t.Logf("could not remove container for %s (it may not exist): %v\n%s", pod, err, out)
		}
		if !hh.volumeExists(pod) {
			t.Errorf("removing pod %s's container took its work volume with it; the spaces "+
				"just provisioned are gone", pod)
		}
	}
	return got
}

// config renders this rig's configuration: the pods' tier chain points at the gateway,
// which is what makes both real inference and prompt inspection possible.
func (p *perMemberRig) config(shared, david, jordan string) string {
	return perMemberConfig(p.gw.baseURL(), shared, david, jordan)
}

// createSpace runs `lore space create` inside a pod and returns the id lore minted.
func createSpace(t *testing.T, hh *household, pod, name string) string {
	t.Helper()
	out, err := hh.exec(pod, "/usr/local/bin/lore", "space", "create", name)
	if err != nil {
		t.Fatalf("`lore space create %q` in pod %s: %v\n%s", name, pod, err, out)
	}
	ids := allUUIDs(out)
	if len(ids) == 0 {
		t.Fatalf("`lore space create %q` in pod %s printed no space id:\n%s", name, pod, out)
	}
	return ids[0]
}

// runHousehold starts the household, runs body against it, and stops it. Every
// assertion about a pod's store is made after it returns, because the host must not read
// a SQLite store a running pod is writing.
func (p *perMemberRig) runHousehold(t *testing.T, hh *household, ready []string, body func()) {
	t.Helper()
	sup, _ := hh.supervisorFor(p.r.image)
	done := make(chan struct{})
	hh.start(sup, func() bool {
		for _, pod := range ready {
			if !hh.containerExists(pod) || !strings.Contains(hh.logs(pod), "supervisor: started") {
				return false
			}
		}
		select {
		case <-done:
			return true
		default:
		}
		body()
		close(done)
		return true
	})
}

func TestThirdScopeAgainstPods(t *testing.T) {
	p := newPerMemberRig(t)

	t.Run("PerMemberPodsServe", func(t *testing.T) { testPerMemberPodsServe(t, p) })
	t.Run("ThirdScopeMemoryBoundary", func(t *testing.T) { testThirdScopeMemoryBoundary(t, p) })
	t.Run("GroupGateInPods", func(t *testing.T) { testGroupGateInPods(t, p) })
	t.Run("NoTapNoWrite", func(t *testing.T) { testNoTapNoWrite(t, p) })
	t.Run("PrivateWordNeverReachesTheGroup", func(t *testing.T) { testPrivateWordNeverReachesTheGroup(t, p) })
	t.Run("StrangerOnTheHouseholdBot", func(t *testing.T) { testStrangerOnTheHouseholdBot(t, p) })
	t.Run("SecretIsolation", func(t *testing.T) { testPerMemberSecretIsolation(t, p) })
	t.Run("Doctor", func(t *testing.T) { testDoctorOnPerMember(t, p) })
}

// testDoctorOnPerMember runs the operator's own diagnostic on a per_member household:
// `kenward doctor --group` on the host, and the per-pod doctor inside each pod, which is
// the command the image's HEALTHCHECK runs.
//
// It asserts little and reports everything. doctor's job is to tell an operator the truth
// about a deployment, so the interesting output is its actual text on a configuration
// that has never been put in front of it.
func testDoctorOnPerMember(t *testing.T, p *perMemberRig) {
	hh := p.household(t, p.config(placeholderShared, placeholderDavid, placeholderJordan))
	sp := p.provision(t, hh)
	david, group := hh.memberPod("david"), hh.groupPod()

	// --- the host's view ---
	//
	// The host supervisor legitimately holds every secret, so --group and --member both
	// resolve here. This is the operator standing at the machine.
	for _, args := range [][]string{
		{"doctor", "--config", hh.h.config, "--group"},
		{"doctor", "--config", hh.h.config, "--member", "david"},
	} {
		before := len(hh.h.both())
		code := hh.h.run(args...)
		t.Logf("=== host: kenward %s (exit %d) ===\n%s",
			strings.Join(args[1:], " "), code, hh.h.both()[before:])
	}

	// --- each pod's own view ---
	//
	// A pod holds one unit's secrets, so its doctor must check that one unit and no
	// other. A household-wide check inside a member's container would fail on every
	// sibling secret the container correctly does not have — and, because this is the
	// HEALTHCHECK, would restart a perfectly good pod forever.
	p.runHousehold(t, hh, []string{david, group}, func() {
		for _, pod := range []string{group, david} {
			out, err := hh.exec(pod, "/usr/local/bin/kenward", "doctor",
				"--config", "/etc/kenward/kenward.yaml", "--data-dir", "/work/kenward")
			t.Logf("=== pod %s: kenward doctor (err=%v) ===\n%s", pod, err, out)

			// The pod must not be reporting a sibling's secret as missing: that is the
			// failure mode UnitScope exists to prevent, and in the HEALTHCHECK it is a
			// restart loop rather than a message.
			for who, other := range map[string]string{
				"david":  "KENWARD_PASSPHRASE_DAVID",
				"jordan": "KENWARD_PASSPHRASE_JORDAN",
			} {
				if pod == hh.memberPod(who) {
					continue
				}
				if strings.Contains(out, other) {
					t.Errorf("pod %s's doctor mentions %s — a secret belonging to %s, which "+
						"this pod correctly does not hold. A doctor that demands it is a "+
						"healthcheck that never passes:\n%s", pod, other, who, out)
				}
			}
		}

		// And the healthcheck exactly as the Dockerfile declares it, which names
		// /var/lib/kenward rather than the /work/kenward the supervisor actually gives
		// the pod. Reported rather than asserted: podman ignores HEALTHCHECK for OCI
		// images, so this only bites a docker-format build or a compose deployment.
		out, err := hh.exec(group, "/usr/local/bin/kenward", "doctor",
			"--config", "/etc/kenward/kenward.yaml", "--data-dir", "/var/lib/kenward")
		t.Logf("=== pod %s: the Dockerfile's HEALTHCHECK argv verbatim (err=%v) ===\n%s",
			group, err, out)
	})

	t.Logf("spaces this household was provisioned with: shared=%s david=%s jordan=%s",
		sp.shared, sp.david, sp.jordan)
}

// testThirdScopeMemoryBoundary is what this file exists for.
//
// Under per_member, david has two private chats: one with his own agent, on his own bot,
// in his own pod; and one with kenward, on the household's bot, in the group's pod. They
// are indistinguishable on the wire — same chat id, same sender — and everything about
// what they may touch differs. This drives both, for real, and then asks a separate lore
// process what each pod's store actually holds.
func testThirdScopeMemoryBoundary(t *testing.T, p *perMemberRig) {
	hh := p.household(t, p.config(placeholderShared, placeholderDavid, placeholderJordan))
	sp := p.provision(t, hh)

	davidPod, groupPod := hh.memberPod("david"), hh.groupPod()

	// --- 0. the pod boundary itself, before a single message ---
	//
	// Read by a separate lore process against each volume. The group's pod is the one
	// every member talks to privately, and it must not be able to name any member's
	// private space — not because scope.Resolve declines to, but because it is not there.
	if !hh.podHolds(groupPod, sp.shared) {
		t.Errorf("the group's pod does not hold the household's shared space %s; it holds %v",
			sp.shared, hh.podSpaces(groupPod))
	}
	if hh.podHolds(groupPod, sp.david) {
		t.Errorf("the group's pod holds david's private space %s. Every member's private chat with "+
			"kenward is served by this pod, so a private space reachable here is a private space "+
			"reachable from a conversation that must never touch one", sp.david)
	}
	if hh.podHolds(groupPod, sp.jordan) {
		t.Errorf("the group's pod holds jordan's private space %s", sp.jordan)
	}
	if !hh.podHolds(davidPod, sp.david) {
		t.Errorf("david's pod does not hold his own private space %s; it holds %v",
			sp.david, hh.podSpaces(davidPod))
	}
	if hh.podHolds(davidPod, sp.jordan) {
		t.Errorf("david's pod holds jordan's private space %s", sp.jordan)
	}

	kenwardWatch := watch(p.api.bot(t, e2eGroupToken))
	davidWatch := watch(p.api.bot(t, e2eDavidToken))

	// Two writes, one in each of david's private chats, each scripted to a `remember`
	// tool call so the assertion is about where capture puts it rather than about
	// whether a 27B model felt like calling a tool. The bodies carry distinct tokens so
	// a separate lore process can say exactly which store each landed in.
	householdToken := fmt.Sprintf("kenward-third-scope-%d", time.Now().UnixNano())
	privateToken := fmt.Sprintf("davids-own-agent-%d", time.Now().UnixNano())

	p.gw.setScript(func(req gatewayRequest) (string, bool) {
		switch {
		case strings.Contains(req.All(), "REMEMBER-HOUSEHOLD"):
			return toolCompletion("Bin day", "The bins go out on "+householdToken+".", "shared"), true
		case strings.Contains(req.All(), "REMEMBER-PRIVATE"):
			return toolCompletion("My cardiologist", "Appointment reference "+privateToken+".", "personal"), true
		}
		return "", false
	})

	p.runHousehold(t, hh, []string{davidPod, groupPod}, func() {
		// (a) david's private chat with kenward — the third scope.
		kenwardWatch.srv.Push(telegramtest.TextUpdate(davidTelegramID, davidTelegramID, "private",
			"REMEMBER-HOUSEHOLD please note the bin day"))
		asked := kenwardWatch.waitFor(t, davidTelegramID, 1, 3*time.Minute)
		if len(asked) == 0 {
			t.Fatalf("kenward never answered david's private message on the household bot; "+
				"the third scope has no unit serving it. Group pod log:\n%s", hh.logs(groupPod))
		}
		t.Logf("kenward -> david (third scope): %q", asked[len(asked)-1])

		// A write to the household's shared memory is put to the member first, always,
		// and written only if they say yes. Find the keyboard and tap it.
		//
		// "Only after a tap" is asserted by testNoTapNoWrite, which runs the same
		// proposal and never answers it. It cannot be asserted here: the store can only
		// be read from the host once the pod has stopped writing to it, and stopping the
		// household mid-question would retire the question along with it.
		tapShared(t, kenwardWatch, davidTelegramID)

		// --- the other half: the third scope READS the shared space ---
		//
		// Everything above is about writing. "A member's private chat with kenward reads
		// the shared space" is a separate claim and needs a separate turn: ask kenward,
		// in that same private chat, about the thing that was just written, and look at
		// what left the pod. The model gateway sees the prompt, so this is retrieval
		// observed rather than inferred from an answer.
		time.Sleep(15 * time.Second) // let the tap's write commit
		p.gw.reset()
		kenwardWatch.srv.Push(telegramtest.TextUpdate(davidTelegramID, davidTelegramID, "private",
			"remind me, when do the bins go out?"))
		if got := kenwardWatch.waitFor(t, davidTelegramID, 3, 3*time.Minute); len(got) == 0 {
			t.Errorf("kenward stopped answering david's private chat after the write")
		}
		if !gatewaySaw(p.gw, householdToken) {
			t.Errorf("david asked kenward, in his private chat with kenward, about something in the "+
				"household's shared memory, and the shared entry never reached the prompt. "+
				"The third scope is supposed to read the shared space:\n%s",
				gatewayDump(p.gw))
		} else {
			t.Logf("the third scope retrieved the household's shared entry into its own prompt")
		}

		// (b) david's private chat with his own agent — the direct scope, other pod.
		davidWatch.srv.Push(telegramtest.TextUpdate(davidTelegramID, davidTelegramID, "private",
			"REMEMBER-PRIVATE my cardiologist reference"))
		if got := davidWatch.waitFor(t, davidTelegramID, 1, 3*time.Minute); len(got) == 0 {
			t.Errorf("david's own agent never answered him; pod log:\n%s", hh.logs(davidPod))
		} else {
			t.Logf("david's agent -> david (direct scope): %q", got[len(got)-1])
		}
		// A note to a member's own private memory is announced rather than asked, so
		// there is nothing to tap; give the write a moment to land.
		time.Sleep(10 * time.Second)
	})

	// --- the assertions, every one of them from a separate lore process ---

	sharedHas := hh.podSearch(groupPod, sp.shared, householdToken)
	if !strings.Contains(sharedHas, householdToken) {
		t.Errorf("what david told kenward to remember never reached the household's shared space %s.\n"+
			"A tap on the shared-memory question is the whole of the write path for the third scope.\n"+
			"lore said:\n%s\n\nthe space ids that pod's store holds: %v\n\nthe pod's own log:\n%s",
			sp.shared, sharedHas, hh.podSpaces(groupPod), hh.logs(groupPod))
	}

	// And it is the *only* place it went. david's own pod must know nothing about it:
	// the third scope carries his identity and none of his memory.
	davidHas := hh.podSearch(davidPod, sp.david, householdToken)
	if strings.Contains(davidHas, householdToken) {
		t.Errorf("what david told kenward in the household conversation landed in his PRIVATE space %s. "+
			"The third scope writes to the shared space and nowhere else:\n%s", sp.david, davidHas)
	}

	// The mirror: what he told his own agent is his, and the pod every member talks to
	// must not hold it.
	privateHas := hh.podSearch(davidPod, sp.david, privateToken)
	if !strings.Contains(privateHas, privateToken) {
		t.Errorf("david's note to his own agent never reached his private space %s:\n%s\n\n"+
			"the space ids that pod's store holds: %v\n\nthe pod's own log:\n%s",
			sp.david, privateHas, hh.podSpaces(davidPod), hh.logs(davidPod))
	}
	if leaked := hh.podSearch(groupPod, sp.shared, privateToken); strings.Contains(leaked, privateToken) {
		t.Errorf("what david told his OWN agent turned up in the household's shared space %s. "+
			"That is a member's private memory published to the household:\n%s", sp.shared, leaked)
	}
}

// tapShared finds the shared-memory question's confirm button and presses it.
//
// It reads the keyboard off the sendMessage that drew it rather than constructing
// callback data, because the callback data is the product's and a test that made its own
// up would be asserting against itself.
func tapShared(t *testing.T, w botWatch, chatID int64) {
	t.Helper()
	srv := w.srv
	calls := srv.CallsFor("sendMessage")
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Form.Get("chat_id") != fmt.Sprint(chatID) {
			continue
		}
		if calls[i].Form.Get("reply_markup") == "" {
			continue
		}
		// The callback data is "<question token>:<index>" and never the choice id —
		// see transport.keyboardFor, which keeps the id off the wire so a long choice
		// name cannot overflow Telegram's 64-byte budget. So the button is identified
		// by its label, which is the only thing the wire carries, and the labels come
		// from internal/lang rather than being spelled out here.
		rows := telegramtest.Keyboard(t, calls[i])
		var labels []string
		for _, row := range rows {
			for _, b := range row {
				labels = append(labels, b.Text)
			}
		}

		// Before tapping: a household scope must never be offered the member's own
		// private space. capture builds the shared question as exactly two choices —
		// save to the household, or don't — and any personal option here would be the
		// third scope offering a write it is not allowed to make.
		for _, l := range labels {
			if l == lang.For("en").BtnPersonal || l == lang.For("en").BtnSavePersonal {
				t.Errorf("the third scope offered david %q — a write to his own private space. "+
					"A private chat with kenward reads and writes the shared space alone; "+
					"the whole keyboard was %q", l, labels)
			}
		}

		for _, row := range rows {
			for _, b := range row {
				if b.Text == lang.For("en").BtnSaveHousehold || b.Text == lang.For("en").BtnHousehold {
					t.Logf("tapping %q (%s)", b.Text, b.CallbackData)
					srv.Push(telegramtest.CallbackUpdate(
						fmt.Sprintf("cb-%d", time.Now().UnixNano()),
						chatID, chatID, calls[i].MessageID, b.CallbackData))
					return
				}
			}
		}
		t.Fatalf("the shared-memory question offers no way to say yes to the household; "+
			"its buttons were %q", labels)
	}
	t.Fatalf("kenward never drew a keyboard for david in chat %d; a write to the household's shared "+
		"memory must be asked about before it happens. Messages sent: %q", chatID, w.toChat(chatID))
}

// testGroupGateInPods checks that the group's pod answers the household chat only when
// addressed, which is the behaviour the single process has and the pod had never been
// asked for.
func testGroupGateInPods(t *testing.T, p *perMemberRig) {
	hh := p.household(t, p.config(placeholderShared, placeholderDavid, placeholderJordan))
	sp := p.provision(t, hh)

	groupPod := hh.groupPod()
	kenwardWatch := watch(p.api.bot(t, e2eGroupToken))

	// Live inference for this one: what the group gate decides is not a function of the
	// model's answer, so there is nothing to script.
	p.gw.setScript(nil)

	p.runHousehold(t, hh, []string{groupPod}, func() {
		// Not addressed. The household is talking among itself and kenward is not in the
		// conversation.
		kenwardWatch.srv.Push(telegramtest.TextUpdate(groupChatID, davidTelegramID, "supergroup",
			"are we out of milk again"))
		kenwardWatch.quiet(t, groupChatID, 30*time.Second, "an unaddressed group message")

		// Addressed, with the mention entity real Telegram sends.
		kenwardWatch.srv.Push(telegramtest.MentionUpdate(groupChatID, davidTelegramID,
			"@"+telegramtest.BotUsername+" are we out of milk?"))
		got := kenwardWatch.waitFor(t, groupChatID, 1, 3*time.Minute)
		if len(got) == 0 {
			t.Errorf("the group's pod did not answer a message that addressed it; pod log:\n%s",
				hh.logs(groupPod))
			return
		}
		reply := got[len(got)-1]
		t.Logf("kenward -> group (addressed, live model): %q", reply)

		// The retrieval disclosure is part of the reply and says whether the household's
		// memory was actually readable. A pod whose own store holds the configured shared
		// space and still cannot read it is a defect, and it is one that only shows up
		// here: no earlier test ever ran a turn inside a pod.
		if strings.Contains(reply, "couldn't be read") || strings.Contains(reply, "could not be read") {
			t.Errorf("the group's pod could not read the household's shared space %s, which its "+
				"own lore store holds (%v).\nThe disclosure line in the reply says so to the "+
				"household.\nPod log:\n%s",
				sp.shared, hh.podSpaces(groupPod), hh.logs(groupPod))
		}
	})
}

// testPrivateWordNeverReachesTheGroup checks the containment that the third scope's
// design depends on and that no assertion on a reply could reach.
//
// Under per_member the household's bot serves both the group chat and every member's
// private chat with kenward, and both live in the *same pod* — one process, one
// transport, one lore store. What keeps david's private word to kenward out of the
// group's prompt is therefore not a boundary anyone can see from outside: it is that
// buildHouseholdUnit makes one Unit per member, each with its own history ring, rather
// than one unit serving all of them. The runner says exactly that, in a comment, and
// this is the test of it.
//
// It is asserted against the prompt, not the reply. A model that simply chose not to
// repeat a leaked fact would pass a reply-based check while the fact sat in its context.
// The model gateway records what left the pod, so the question asked here is whether the
// words were ever in the request at all.
func testPrivateWordNeverReachesTheGroup(t *testing.T, p *perMemberRig) {
	hh := p.household(t, p.config(placeholderShared, placeholderDavid, placeholderJordan))
	p.provision(t, hh)

	groupPod := hh.groupPod()
	kenwardWatch := watch(p.api.bot(t, e2eGroupToken))

	secret := fmt.Sprintf("ORTHANC-%d", time.Now().UnixNano())

	// Scripted, so the turn is about what was sent rather than what a model replied.
	p.gw.setScript(func(gatewayRequest) (string, bool) {
		return completion("Noted."), true
	})

	p.runHousehold(t, hh, []string{groupPod}, func() {
		// david tells kenward something in private.
		kenwardWatch.srv.Push(telegramtest.TextUpdate(davidTelegramID, davidTelegramID, "private",
			"between us, the spare key is under the "+secret+" pot"))
		if got := kenwardWatch.waitFor(t, davidTelegramID, 1, 2*time.Minute); len(got) == 0 {
			t.Fatalf("kenward never answered david privately; pod log:\n%s", hh.logs(groupPod))
		}
		if !gatewaySaw(p.gw, secret) {
			t.Fatalf("david's private message never reached the model at all, so this test " +
				"would pass for the wrong reason")
		}

		// Everything from here belongs to the group's turn.
		p.gw.reset()
		kenwardWatch.srv.Push(telegramtest.MentionUpdate(groupChatID, jordanTelegramID,
			"@"+telegramtest.BotUsername+" where is the spare key?"))
		if got := kenwardWatch.waitFor(t, groupChatID, 1, 2*time.Minute); len(got) == 0 {
			t.Fatalf("the group's pod never answered the group; pod log:\n%s", hh.logs(groupPod))
		}

		if gatewaySaw(p.gw, secret) {
			var where []string
			for i, req := range p.gw.all() {
				if strings.Contains(req.All(), secret) {
					where = append(where, fmt.Sprintf("request %d", i))
				}
			}
			t.Errorf("what david said to kenward in private turned up in the GROUP's prompt (%s).\n"+
				"Both conversations are served by the same pod on the same bot, and what keeps "+
				"them apart is one Unit each — its own history ring — rather than one unit "+
				"serving every member. The whole household can read the group chat.",
				strings.Join(where, ", "))
		}
	})
}

// testNoTapNoWrite is the other half of "a write lands in shared, and only after a tap".
//
// The proposal is made and never answered. Nothing may reach the household's shared
// memory, because other people will have read it by the time the member regrets it —
// which is the stated reason the shared question exists at all and is not configurable.
func testNoTapNoWrite(t *testing.T, p *perMemberRig) {
	hh := p.household(t, p.config(placeholderShared, placeholderDavid, placeholderJordan))
	sp := p.provision(t, hh)
	groupPod := hh.groupPod()
	kenwardWatch := watch(p.api.bot(t, e2eGroupToken))

	token := fmt.Sprintf("never-tapped-%d", time.Now().UnixNano())
	p.gw.setScript(func(req gatewayRequest) (string, bool) {
		if strings.Contains(req.All(), "REMEMBER-HOUSEHOLD") {
			return toolCompletion("Alarm code", "The alarm code is "+token+".", "shared"), true
		}
		return "", false
	})

	p.runHousehold(t, hh, []string{groupPod}, func() {
		kenwardWatch.srv.Push(telegramtest.TextUpdate(davidTelegramID, davidTelegramID, "private",
			"REMEMBER-HOUSEHOLD the alarm code"))
		got := kenwardWatch.waitFor(t, davidTelegramID, 1, 3*time.Minute)
		if len(got) == 0 {
			t.Fatalf("kenward never put the question, so there is nothing to leave unanswered")
		}
		if !strings.Contains(got[len(got)-1], token) {
			t.Fatalf("the question kenward put is not the proposal this test made: %q", got[len(got)-1])
		}
		// And no tap. Give the pod long enough that a write would have happened.
		time.Sleep(20 * time.Second)
	})

	if after := hh.podSearch(groupPod, sp.shared, token); strings.Contains(after, token) {
		t.Errorf("kenward wrote to the household's shared memory without being told to. "+
			"The proposal was put as a question and never answered:\n%s", after)
	} else {
		t.Logf("nothing was written to the household's shared space without a tap")
	}
}

// gatewayDump renders the system prompts the pods sent since the last reset, which is
// what a retrieval assertion needs to see when it fails.
func gatewayDump(g *modelGateway) string {
	var b strings.Builder
	for i, req := range g.all() {
		fmt.Fprintf(&b, "--- request %d system prompt ---\n%s\n", i, req.System())
	}
	if b.Len() == 0 {
		return "(the pods sent no chat completion at all)"
	}
	return b.String()
}

// gatewaySaw reports whether any request the pods have made since the last reset carried
// this text anywhere in it.
func gatewaySaw(g *modelGateway, text string) bool {
	for _, req := range g.all() {
		if strings.Contains(req.All(), text) {
			return true
		}
	}
	return false
}

// testStrangerOnTheHouseholdBot checks the silence a non-member gets.
//
// Replying at all — even to refuse — confirms to a stranger that this bot is a kenward
// node serving a real household, which is the fact silence protects. It is asserted on
// the household's bot because that is the one a stranger is most likely to find.
func testStrangerOnTheHouseholdBot(t *testing.T, p *perMemberRig) {
	hh := p.household(t, p.config(placeholderShared, placeholderDavid, placeholderJordan))
	p.provision(t, hh)

	groupPod := hh.groupPod()
	kenwardWatch := watch(p.api.bot(t, e2eGroupToken))
	p.gw.setScript(nil)

	p.runHousehold(t, hh, []string{groupPod}, func() {
		kenwardWatch.srv.Push(telegramtest.TextUpdate(strangerTelegramI, strangerTelegramI, "private",
			"hello, what is this bot?"))
		kenwardWatch.quiet(t, strangerTelegramI, 45*time.Second, "a stranger on the household bot")

		// And a stranger in the household's own group chat, which is the other way in:
		// any member can add anyone to a Telegram group.
		kenwardWatch.srv.Push(telegramtest.MentionUpdate(groupChatID, strangerTelegramI,
			"@"+telegramtest.BotUsername+" what is the door code?"))
		kenwardWatch.quiet(t, groupChatID, 45*time.Second, "a stranger addressing the household group")
	})
}

// testPerMemberSecretIsolation re-confirms D-007 under per_member: each pod holds its own
// bot token and its own passphrase, and no sibling's, anywhere in its container.
func testPerMemberSecretIsolation(t *testing.T, p *perMemberRig) {
	hh := p.household(t, p.config(placeholderShared, placeholderDavid, placeholderJordan))
	david, jordan, group := hh.memberPod("david"), hh.memberPod("jordan"), hh.groupPod()

	p.runHousehold(t, hh, []string{david, jordan, group}, func() {})

	assertPodEnv(t, hh, david, map[string]string{
		"KENWARD_BOT_TOKEN_DAVID":  e2eDavidToken,
		"KENWARD_PASSPHRASE_DAVID": e2eDavidPass,
	}, map[string]string{
		"jordan's bot token":      e2eJordanToken,
		"jordan's passphrase":     e2eJordanPass,
		"the household bot token": e2eGroupToken,
	})
	assertPodEnv(t, hh, jordan, map[string]string{
		"KENWARD_BOT_TOKEN_JORDAN":  e2eJordanToken,
		"KENWARD_PASSPHRASE_JORDAN": e2eJordanPass,
	}, map[string]string{
		"david's bot token":       e2eDavidToken,
		"david's passphrase":      e2eDavidPass,
		"the household bot token": e2eGroupToken,
	})
	// The group's pod is the one that serves every member's private chat with kenward,
	// so it is the one where a member's passphrase would be most tempting and most
	// wrong: the third scope reads no private space, and a key that unwraps one has no
	// business here.
	assertPodEnv(t, hh, group, map[string]string{
		"KENWARD_BOT_TOKEN_HOUSEHOLD": e2eGroupToken,
	}, map[string]string{
		"david's bot token":   e2eDavidToken,
		"david's passphrase":  e2eDavidPass,
		"jordan's bot token":  e2eJordanToken,
		"jordan's passphrase": e2eJordanPass,
	})
}

// testPerMemberPodsServe is the precondition every other scenario in this file needs,
// and on its own it is already something no run has reached: a per_member household of
// real pods that authorise their bots and stay up.
//
// Every previous podman run died at getMe — see isolated_podman_test.go's file comment,
// which builds its whole assertion strategy around that. Here getMe succeeds, so a pod
// reaching "supervisor: started" means it got past everything that comment lists *and*
// past the Telegram handshake, and then went on serving.
func testPerMemberPodsServe(t *testing.T, p *perMemberRig) {
	hh := p.household(t, p.config(placeholderShared, placeholderDavid, placeholderJordan))

	david, jordan, group := hh.memberPod("david"), hh.memberPod("jordan"), hh.groupPod()
	pods := []string{david, jordan, group}

	sup, _ := hh.supervisorFor(p.r.image)
	hh.start(sup, func() bool {
		for _, pod := range pods {
			if !hh.containerExists(pod) || !strings.Contains(hh.logs(pod), "supervisor: started") {
				return false
			}
		}
		return true
	})

	for _, pod := range pods {
		log := hh.logs(pod)
		if !strings.Contains(log, "supervisor: started") {
			t.Errorf("pod %s never started serving; its log ends:\n%s", pod, lastLine(log))
			continue
		}
		if strings.Contains(lastLine(log), "getMe") {
			t.Errorf("pod %s still died at the Telegram handshake, so the stand-in is not "+
				"being reached:\n%s", pod, lastLine(log))
		}
	}

	// The group's pod is kenward's, and under per_member it is the one that holds every
	// member's private conversation with kenward as well as the group's.
	if log := hh.logs(group); !strings.Contains(log, "serving household group") {
		t.Errorf("the group's pod does not report serving the household group:\n%s", log)
	}
	for _, pod := range []string{david, jordan} {
		if log := hh.logs(pod); !strings.Contains(log, "serving member") {
			t.Errorf("pod %s does not report serving its member:\n%s", pod, log)
		}
	}

	// Each bot was authorised by the bot it belongs to, and by no other. This is the
	// per_member wiring stated on the wire: three separate Bot API identities, one per
	// pod, which is the thing one real token could never show.
	for label, token := range standInTokens() {
		if n := p.api.bot(t, token).CountFor("getMe"); n == 0 {
			t.Errorf("no pod ever authorised the bot for %s; that unit never opened its transport", label)
		}
	}
}
