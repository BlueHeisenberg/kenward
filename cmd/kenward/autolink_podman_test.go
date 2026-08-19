//go:build integration && linux

package main

// The acceptance test for the last manual step in isolated mode.
//
// # What it claims
//
// An isolated household comes up on real podman, a member is added, and that
// member's own lore store ends up holding the household's shared space, able to
// read what the household wrote and to write into it — with NO command run inside
// any container. Then the same for a member added to a household that is ALREADY
// RUNNING, which is the case a step that only works at install time silently fails.
//
// Every assertion is made by a SEPARATE `lore` process, run by the host against the
// pod's own named volume. Nothing here believes a kenward log line: the logs are
// part of what is under test.
//
// # What is real and what is not
//
// Real: podman, the image built from this repository's own Dockerfile against the
// lore checkout beside it, the pods' named volumes, `kenward run` with the argv
// `supervisor.PodCommand` produces, each pod initialising its own lore home and its
// own spaces, lore's sync daemon in each pod, internal/link's desk and joiner over
// real mDNS, and the lore binary the assertions use.
//
// Two things are not.
//
// The Bot API. No bot token exists, and a pod that cannot authorise one exits at
// getMe about 170ms in — before the handshake has had any time at all. So
// api.telegram.org is a stand-in on this host, reached over TLS exactly as
// production reaches the real one: podman's base_hosts_file resolves the name here
// and a CA bundle mounted into each container trusts the certificate. The pod's
// binary and its configuration are unmodified. This is thirdscope_podman_test.go's
// mechanism and the comment at the top of that file is the long version.
//
// THE NETWORK, and this one is a real substitution worth reading. Isolated mode
// puts each pod in its own network namespace on the container runtime's bridge, and
// that is where the pods find each other — lore's sync daemon and internal/link's
// desk both discover over mDNS there. This machine cannot provide it: it is podman
// 4.9.3 under WSL2, whose kernel has no working bridge<->veth path, so
// /etc/containers/containers.conf pins every container to slirp4netns. Under
// slirp4netns each container gets its own userspace netstack and its own copy of
// 10.0.2.100, and NO container can reach any sibling — measured, not assumed:
//
//	$ podman run -d --name a alpine sh -c 'nc -l -p 9000'
//	$ podman inspect -f '{{.NetworkSettings.IPAddress}}' a   ->   (empty)
//	$ podman run --rm alpine nc -w 3 <a> 9000                ->   cannot reach
//
// That is not a property of kenward and no design can be proved against it. It is
// also not new here: it means lore's cross-pod sync (D-044) cannot move an entry
// between two pods on this machine either, whatever provisions the membership.
//
// So the pods are started inside one `podman pod`, which gives them a shared
// network namespace: still isolated from the host, still real containers on real
// volumes, and siblings reachable from each other — which is exactly the property
// the bridge provides on a host whose kernel can do it. What is therefore NOT
// proved here is that the bridge's namespace boundary is crossed correctly; what IS
// proved is everything above it, which is where all of this change lives.
//
// # Running it
//
//	go test -tags integration -run TestAutoLinkAgainstPods -timeout 40m -v ./cmd/kenward/
//
// on a Linux host with podman, `go`, a lore checkout beside this one (or
// KENWARD_E2E_LORE_SRC), root (it binds :443 to stand in for api.telegram.org), and
// a podman whose containers can reach the host. It skips, rather than fails, where
// the host cannot provide those. Everything it makes is prefixed `kwlink-` and is
// destroyed with the test.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Names and paths. Everything this file creates carries linkPrefix so cleanup can
// be a filter rather than a list kept in memory.
const (
	linkPrefix = "kwlink-"
	// podWork is where each pod's named volume is mounted, matching
	// supervisor.DefaultLoreHome and DefaultPodDataDir, both of which live under it.
	podWork = "/work"
	// podLoreHome and podDataDir are supervisor.DefaultLoreHome and
	// supervisor.DefaultPodDataDir. Spelled rather than imported so a change to
	// either shows up here as a failing test rather than as a test that quietly
	// moved with it.
	podLoreHome = "/work/lore"
	podDataDir  = "/work/kenward"
	// podConfigPath is supervisor.PodConfigPath, for the same reason.
	podConfigPath = "/etc/kenward/kenward.yaml"
)

// The household. Two members at first; jordan is added to a running household in
// the last scenario. The space ids are chosen HERE and never read back out of a
// pod, which is the point of lore.CreateSpaceWithID and what makes the assertions
// below able to name a space the wizard decided on.
const (
	sharedSpaceID = "dac31e70-72e4-4b10-9cef-a6276c4a87b8"
	davidSpaceID  = "7d5047bb-d939-4539-b3db-8b6221a2e245"
	jordanSpaceID = "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19"

	householdToken = "9000000001:AAH-kwlink-household-token-XXXXXXXXXX"
	davidToken     = "9000000002:AAH-kwlink-david-token-YYYYYYYYYYYYY"
	jordanToken    = "9000000003:AAH-kwlink-jordan-token-ZZZZZZZZZZZZ"

	// theLinkKey is what makes the whole thing automatic. wrongLinkKey is what a
	// container on the same network that is not part of this household would have.
	theLinkKey   = "kwlink-household-link-key-not-a-real-one"
	wrongLinkKey = "kwlink-a-different-households-link-key!!"
)

// householdConfig renders kenward.yaml. linkKey empty writes no link_key_env at
// all, which is the state every household deployed before this change is in and
// the state the first scenario asserts is still broken.
func householdConfig(linkKey string, members ...string) string {
	var b strings.Builder
	b.WriteString("mode: isolated\nhousehold:\n  name: Ashfield\n")
	fmt.Fprintf(&b, "  shared_space: %s\n", sharedSpaceID)
	if linkKey != "" {
		b.WriteString("  link_key_env: KENWARD_LINK_KEY\n")
	}
	b.WriteString("  group_chat_id: -1001234567890\n  tiers: [local]\n")
	b.WriteString("telegram:\n  bot_token_env: KENWARD_BOT_TOKEN_HOUSEHOLD\nmembers:\n")
	for _, m := range members {
		switch m {
		case "david":
			fmt.Fprintf(&b, "  - id: david\n    name: David\n    telegram_id: 12345678\n"+
				"    private_space: %s\n    tiers: [local]\n"+
				"    bot_token_env: KENWARD_BOT_TOKEN_DAVID\n    passphrase_env: KENWARD_PASSPHRASE_DAVID\n",
				davidSpaceID)
		case "jordan":
			fmt.Fprintf(&b, "  - id: jordan\n    name: Jordan\n    telegram_id: 87654321\n"+
				"    private_space: %s\n    tiers: [local]\n"+
				"    bot_token_env: KENWARD_BOT_TOKEN_JORDAN\n    passphrase_env: KENWARD_PASSPHRASE_JORDAN\n",
				jordanSpaceID)
		}
	}
	b.WriteString("endpoints:\n  - name: attic\n    base_url: http://attic.localdomain:8000/v1\n" +
		"    model: qwen3-4b\n    tags: [local]\n    timeout: 120s\n")
	return b.String()
}

// -----------------------------------------------------------------------------
// the rig
// -----------------------------------------------------------------------------

type linkRig struct {
	t       *testing.T
	podman  string
	env     []string // CONTAINERS_CONF and the registries file, for every podman call
	image   string
	loreBin string // the host's own lore, for every assertion
	pod     string
	caFile  string // the image's CA bundle plus the stand-in's root
	scratch string
	api     *botAPI
	tag     string
}

func newLinkRig(t *testing.T) *linkRig {
	t.Helper()

	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman is not on PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH; this test builds the image and a lore binary")
	}
	if os.Geteuid() != 0 {
		t.Skip("this test binds :443 to stand in for api.telegram.org and needs root")
	}
	repo := repoRoot(t)
	loreSrc, ok := findLoreCheckout(repo)
	if !ok {
		t.Skipf("no lore checkout beside %s (set KENWARD_E2E_LORE_SRC)", repo)
	}

	scratch := t.TempDir()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	r := &linkRig{
		t:       t,
		podman:  podman,
		scratch: scratch,
		tag:     tag,
		image:   "localhost/kenward-kwlink:" + tag,
		pod:     linkPrefix + tag,
	}

	// One containers.conf for every podman call this file makes: the machine's own,
	// plus the line that points api.telegram.org at this host. Never an edit to
	// /etc — and the machine's own file is the BASE rather than a fresh one,
	// because on this host it is what pins netns=slirp4netns and a container
	// without that setting does not start at all.
	regConf := filepath.Join(scratch, "registries.conf")
	writeFile(t, regConf, []byte("unqualified-search-registries = [\"docker.io\"]\n"), 0o644)
	hosts := filepath.Join(scratch, "hosts")
	writeFile(t, hosts, []byte(hostFromPod+" api.telegram.org\n"), 0o644)
	base, err := os.ReadFile("/etc/containers/containers.conf")
	if err != nil {
		t.Logf("no /etc/containers/containers.conf to extend (%v); writing a fresh one", err)
		base = []byte("[containers]\n")
	}
	conf := filepath.Join(scratch, "containers.conf")
	writeFile(t, conf, append(append([]byte(nil), base...),
		[]byte(fmt.Sprintf("\nbase_hosts_file = %q\n", hosts))...), 0o644)
	r.env = append(os.Environ(),
		"CONTAINERS_CONF="+conf,
		"CONTAINERS_REGISTRIES_CONF="+regConf)

	t.Cleanup(r.purge)

	r.loreBin = buildLore(t, scratch, loreSrc)
	r.buildImage(repo, loreSrc)

	// The stand-in first: trustStorePlus appends its root to the image's bundle.
	r.api = newBotAPI(t, map[string]string{
		"household": householdToken, "david": davidToken, "jordan": jordanToken,
	})
	r.caFile = r.trustStorePlus(scratch)

	// One pod, one network namespace: see the substitution note at the top.
	if out, err := r.try("pod", "create", "--name", r.pod); err != nil {
		t.Fatalf("podman pod create: %v\n%s", err, out)
	}
	r.requireSiblingsCanSeeEachOther()
	return r
}

// buildImage builds the pod image from a scratch copy of this repository with the
// sibling lore checkout replaced into it.
//
// The replace is not a convenience. kenward's go.mod requires a lore version that
// carries GrantMembership, and while that version is unreleased the module proxy
// cannot serve it — so an image built the ordinary way would contain a kenward
// whose internal/link does not compile. Copying the checkout in and pointing go.mod
// at it builds the image from exactly the lore this change is being tested against,
// which is what a released version will do too.
//
// The Dockerfile's `COPY go.mod go.sum` / `RUN go mod download` pair is dropped
// from the copy used here: it is a layer-cache optimisation, and with a filesystem
// replace it runs before the replaced source has been copied in and fails.
func (r *linkRig) buildImage(repo, loreSrc string) {
	t := r.t
	t.Helper()
	ctx := filepath.Join(r.scratch, "ctx")
	copyTree(t, repo, ctx)
	copyTree(t, loreSrc, filepath.Join(ctx, ".lore"))

	edit := exec.Command("go", "mod", "edit", "-replace", "github.com/BlueHeisenberg/lore=./.lore")
	edit.Dir = ctx
	edit.Env = append(os.Environ(), "GOWORK=off")
	if out, err := edit.CombinedOutput(); err != nil {
		t.Fatalf("pointing the build context's go.mod at the local lore: %v\n%s", err, out)
	}

	df, err := os.ReadFile(filepath.Join(repo, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the Dockerfile: %v", err)
	}
	body := strings.ReplaceAll(string(df), "COPY go.mod go.sum ./\nRUN go mod download\n", "")
	if strings.Contains(body, "go mod download") {
		// Not fatal, but it means the pair was not where this expects and the build
		// below will fail on a missing ./.lore in a way that is hard to read.
		t.Logf("warning: the Dockerfile's `go mod download` line was not removed as expected")
	}
	dfPath := filepath.Join(ctx, "Dockerfile.kwlink")
	writeFile(t, dfPath, []byte(body), 0o644)

	t.Logf("building %s from %s (lore replaced with %s)", r.image, repo, loreSrc)
	bctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(bctx, r.podman, "build", "-t", r.image, "-f", dfPath, ctx)
	cmd.Dir = ctx
	cmd.Env = r.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("podman build: %v\n%s", err, out)
	}
}

// trustStorePlus copies the image's own CA bundle out and appends the stand-in's
// root, so a pod trusts everything it trusted before plus one more thing.
func (r *linkRig) trustStorePlus(dir string) string {
	t := r.t
	t.Helper()
	name := linkPrefix + "ca-" + r.tag
	if out, err := r.try("create", "--name", name, r.image, "version"); err != nil {
		t.Fatalf("creating a container to read the image's CA bundle: %v\n%s", err, out)
	}
	defer func() { _, _ = r.try("rm", "-f", name) }()
	dst := filepath.Join(dir, "ca-certificates.crt")
	if out, err := r.try("cp", name+":"+podCABundlePath, dst); err != nil {
		t.Fatalf("copying the CA bundle out of the image: %v\n%s", err, out)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading the image's CA bundle: %v", err)
	}
	writeFile(t, dst, append(b, r.api.caPEM...), 0o644)
	return dst
}

// requireSiblingsCanSeeEachOther proves the one host fact everything below depends
// on, before any expensive work: two containers in this pod can reach each other.
func (r *linkRig) requireSiblingsCanSeeEachOther() {
	t := r.t
	t.Helper()
	name := linkPrefix + "probe-" + r.tag
	if out, err := r.try("run", "-d", "--pod", r.pod, "--name", name,
		"docker.io/library/alpine:latest", "sh", "-c",
		"while true; do echo sibling-reachable | nc -l -p 19999; done"); err != nil {
		t.Skipf("cannot start a probe container: %v\n%s", err, out)
	}
	defer func() { _, _ = r.try("rm", "-f", "-t", "1", name) }()
	time.Sleep(2 * time.Second)
	out, err := r.try("run", "--rm", "--pod", r.pod,
		"docker.io/library/alpine:latest", "nc", "-w", "3", "127.0.0.1", "19999")
	if err != nil || !strings.Contains(out, "sibling-reachable") {
		t.Skipf("containers in one podman pod cannot reach each other here (%v):\n%s", err, out)
	}
}

func (r *linkRig) try(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.podman, args...)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *linkRig) must(args ...string) string {
	r.t.Helper()
	out, err := r.try(args...)
	if err != nil {
		r.t.Fatalf("podman %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// purge removes every container, pod, volume and image this file could have made.
// It filters on the prefix rather than on a list, so a container left behind by a
// scenario that panicked still goes.
func (r *linkRig) purge() {
	if out, err := r.try("ps", "-aq", "--filter", "name=^"+linkPrefix); err == nil {
		for _, id := range strings.Fields(out) {
			_, _ = r.try("rm", "-f", "-t", "1", id)
		}
	}
	_, _ = r.try("pod", "rm", "-f", r.pod)
	if out, err := r.try("volume", "ls", "-q", "--filter", "name=^"+linkPrefix); err == nil {
		for _, v := range strings.Fields(out) {
			_, _ = r.try("volume", "rm", "-f", v)
		}
	}
	_, _ = r.try("rmi", "-f", r.image)
}

// -----------------------------------------------------------------------------
// starting a unit
// -----------------------------------------------------------------------------

// unit is one running pod of the household, as this file drives it.
type unit struct {
	name   string // container and volume name
	member string // "" for the group
}

// start runs one unit with the argv supervisor.PodCommand produces, its own named
// volume, the household configuration, and only the secrets that unit is entitled
// to — plus the link key, when linkKey is not empty.
//
// Nothing here is `podman exec`. The container is created, started, and never
// spoken to again: every claim this file makes is read off its volume afterwards.
func (r *linkRig) start(u unit, configPath, linkKey string) {
	t := r.t
	t.Helper()
	args := []string{
		"run", "-d", "--pod", r.pod, "--name", u.name,
		"--volume", u.name + ":" + podWork,
		"--volume", configPath + ":" + podConfigPath + ":ro",
		"--volume", r.caFile + ":" + podCABundlePath + ":ro",
		"--env", "LORE_HOME=" + podLoreHome,
		"--env", "KENWARD_DATA_DIR=" + podDataDir,
		"--env", "KENWARD_BOT_TOKEN_HOUSEHOLD=" + householdToken,
	}
	switch u.member {
	case "":
		// The group's pod and no member's secrets at all.
	case "david":
		args = append(args, "--env", "KENWARD_BOT_TOKEN_DAVID="+davidToken,
			"--env", "KENWARD_PASSPHRASE_DAVID=david-passphrase")
	case "jordan":
		args = append(args, "--env", "KENWARD_BOT_TOKEN_JORDAN="+jordanToken,
			"--env", "KENWARD_PASSPHRASE_JORDAN=jordan-passphrase")
	}
	if linkKey != "" {
		args = append(args, "--env", "KENWARD_LINK_KEY="+linkKey)
	}
	unitFlag := "--group"
	if u.member != "" {
		unitFlag = "--member=" + u.member
	}
	// The real contract: exactly the argv the supervisor puts in a pod's spec.
	args = append(args, r.image)
	args = append(args, podCommand(unitFlag)...)
	r.must(args...)
}

// podCommand mirrors supervisor.PodCommand. It is spelled out rather than imported
// so that a change to the argv contract shows up here as a failing test rather than
// as a test that silently followed it.
func podCommand(unitFlag string) []string {
	argv := []string{"run", "--config=" + podConfigPath, "--data-dir=" + podDataDir}
	if unitFlag != "--group" {
		argv = append(argv, "--invites=/etc/kenward/invites.json", "--revoked=/etc/kenward/revoked.json")
	}
	return append(argv, unitFlag)
}

// waitServing blocks until a unit's log says it is serving, which under the
// stand-in it can actually do.
//
// This is the ONE place a log is read, and it is not an assertion: it is how the
// test knows the pod got far enough to be worth asking about. Every claim is made
// against the volume.
func (r *linkRig) waitServing(u unit, within time.Duration) {
	t := r.t
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		out, _ := r.try("logs", u.name)
		if strings.Contains(out, "supervisor: started") || strings.Contains(out, "event=serve") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	out, _ := r.try("logs", u.name)
	t.Fatalf("pod %s never reported serving within %s; its log:\n%s", u.name, within, lastLines(out, 40))
}

// lastLines is the end of a log, for a failure message. image_test.go already has
// a tail() of its own for a different shape, so this one is named for what it
// does rather than competing with it.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// -----------------------------------------------------------------------------
// the assertions: a separate lore process against a pod's own volume
// -----------------------------------------------------------------------------

// loreHome returns the host path of a pod's lore home.
//
// Podman named volumes live on this host's filesystem, which is what makes an
// out-of-band check possible at all. Note what this does NOT do: it never enters
// the container.
func (r *linkRig) loreHome(u unit) string {
	t := r.t
	t.Helper()
	out := strings.TrimSpace(r.must("volume", "inspect", "-f", "{{.Mountpoint}}", u.name))
	if out == "" {
		t.Fatalf("podman reports no mountpoint for volume %s", u.name)
	}
	return filepath.Join(out, "lore")
}

// lore runs the host's own lore binary against a pod's store.
func (r *linkRig) lore(u unit, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.loreBin, args...)
	cmd.Env = append(os.Environ(), "LORE_HOME="+r.loreHome(u))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// holdsSpace reports whether a pod's own store holds a space.
//
// Locally present is lore's membership check — a space a store does not hold was
// never carried there — so this single question is the whole of "was this pod
// admitted".
func (r *linkRig) holdsSpace(u unit, spaceID string) bool {
	out, err := r.lore(u, "spaces")
	if err != nil {
		return false
	}
	return strings.Contains(out, spaceID)
}

// waitForSpace polls a pod's store until it holds a space.
func (r *linkRig) waitForSpace(u unit, spaceID string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if r.holdsSpace(u, spaceID) {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// put writes an entry into a pod's store, as that pod's own lore account, through a
// separate lore process. It is how this file asks "may this store write here?" —
// lore refuses a space this account is not a writer of, so a successful put IS the
// membership assertion, not a proxy for one.
func (r *linkRig) put(u unit, spaceID, title, body string) error {
	// The body is positional: `lore put --domain D --title T <body...>`.
	out, err := r.lore(u, "put", "--space", spaceID, "--domain", "house/kwlink",
		"--title", title, body)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

// waitForEntry polls a pod's store until a word turns up in a space.
//
// The word rather than the title: lore's index is conjunctive over whole terms
// with no stemming and no prefix matching, so a query has to be a term the entry
// actually contains. Flags before the query, because the flag package stops at the
// first non-flag argument.
func (r *linkRig) waitForEntry(u unit, spaceID, word string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		out, err := r.lore(u, "search", "--space", spaceID, word)
		if err == nil && strings.Contains(out, word) {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// copyTree copies a directory and prunes the .git inside the copy. Never the
// original: this must not write into a repository it does not own.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	mkdirAll(t, dst)
	if out, err := exec.Command("cp", "-a", src+"/.", dst).CombinedOutput(); err != nil {
		t.Fatalf("copying %s to %s: %v\n%s", src, dst, err, out)
	}
	for _, junk := range []string{".git", "go.work", "go.work.sum"} {
		_ = os.RemoveAll(filepath.Join(dst, junk))
	}
}

// -----------------------------------------------------------------------------
// the scenarios
// -----------------------------------------------------------------------------

func TestAutoLinkAgainstPods(t *testing.T) {
	r := newLinkRig(t)

	t.Run("TheFailingStateWithNoLinkKey", func(t *testing.T) { testFailingState(t, r) })
	t.Run("AMemberIsAdmittedWithNobodyRunningAnything", func(t *testing.T) { testAdmitted(t, r) })
	t.Run("AMemberAddedToARunningHousehold", func(t *testing.T) { testAddedToRunning(t, r) })
	t.Run("AStrangerWithTheWrongLinkKeyIsRefused", func(t *testing.T) { testStrangerRefused(t, r) })
}

// testFailingState is today's code, on this machine, quoted.
//
// A household with no household.link_key is exactly what every deployment made
// before this change is, and it is the state the two `docker compose exec` lines
// existed to get out of. The group's pod creates the shared space; david's pod
// creates his private one; david's store never comes to hold the household's, and
// nothing in either pod does anything about it.
func testFailingState(t *testing.T, r *linkRig) {
	cfg := filepath.Join(r.scratch, "unlinked.yaml")
	writeFile(t, cfg, []byte(householdConfig("", "david")), 0o644)

	group := unit{name: linkPrefix + "u-group-" + r.tag}
	david := unit{name: linkPrefix + "u-david-" + r.tag, member: "david"}
	r.start(group, cfg, "")
	r.start(david, cfg, "")
	r.waitServing(group, 3*time.Minute)
	r.waitServing(david, 3*time.Minute)

	// Long enough that "it had not got round to it yet" is not the explanation:
	// the joiner's retry interval is fifteen seconds and its mDNS browse is six.
	time.Sleep(60 * time.Second)

	if !r.holdsSpace(group, sharedSpaceID) {
		t.Fatalf("the group's pod does not hold the household space it is supposed to create:\n%s",
			mustOut(r.lore(group, "spaces")))
	}
	if !r.holdsSpace(david, davidSpaceID) {
		t.Fatalf("david's pod does not hold his own private space:\n%s",
			mustOut(r.lore(david, "spaces")))
	}
	if r.holdsSpace(david, sharedSpaceID) {
		t.Fatal("david's pod holds the household space with no link key configured; " +
			"the failing state this change fixes is not reproducible, so the scenarios below prove nothing")
	}
	t.Logf("as expected, with no household.link_key david's own store holds only his private space:\n%s",
		mustOut(r.lore(david, "spaces")))

	for _, u := range []unit{group, david} {
		_, _ = r.try("rm", "-f", "-t", "5", u.name)
		_, _ = r.try("volume", "rm", "-f", u.name)
	}
}

// testAdmitted is the acceptance test.
//
// The same household with a link key. Nothing is run inside any container: the two
// are started and then only their volumes are read. David's store must come to hold
// the household space, must be able to READ what the group wrote into it, and must
// be able to WRITE into it and have that reach the group.
func testAdmitted(t *testing.T, r *linkRig) {
	cfg := filepath.Join(r.scratch, "linked.yaml")
	writeFile(t, cfg, []byte(householdConfig(theLinkKey, "david", "jordan")), 0o644)

	group := unit{name: linkPrefix + "l-group-" + r.tag}
	david := unit{name: linkPrefix + "l-david-" + r.tag, member: "david"}
	// Deliberately no t.Cleanup here. The two scenarios after this one are about a
	// household that is STILL RUNNING, which is the whole point of them; the rig's
	// purge removes every kwlink- container and volume when the parent test ends.
	r.start(group, cfg, theLinkKey)
	r.start(david, cfg, theLinkKey)
	r.waitServing(group, 3*time.Minute)
	r.waitServing(david, 3*time.Minute)

	if !r.waitForSpace(david, sharedSpaceID, 3*time.Minute) {
		out, _ := r.try("logs", david.name)
		t.Fatalf("david's pod was never admitted to the household's shared space.\nspaces:\n%s\nlog:\n%s",
			mustOut(r.lore(david, "spaces")), lastLines(out, 40))
	}
	t.Logf("david's own store now holds the household space, with no command run in any container:\n%s",
		mustOut(r.lore(david, "spaces")))

	// READ: the group writes, david's store receives it over lore's own sync.
	if err := r.put(group, sharedSpaceID, "bin day", "the bins go out on kwlinkbinsday"); err != nil {
		t.Fatalf("the group's own store cannot write to the space it owns: %v", err)
	}
	if !r.waitForEntry(david, sharedSpaceID, "kwlinkbinsday", 3*time.Minute) {
		t.Fatalf("what the household wrote never reached david's store:\n%s",
			mustOut(r.lore(david, "search", "--space", sharedSpaceID, "kwlinkbinsday")))
	}

	// WRITE: david's store authors into the household space. lore refuses an
	// account that is not a writer of a space, so this succeeding is the membership
	// assertion rather than a proxy for it — and the entry reaching the group is
	// the household actually having it.
	if err := r.put(david, sharedSpaceID, "boiler service", "the boiler service is in kwlinkboilermonth"); err != nil {
		t.Fatalf("david's store cannot write into the household space: %v", err)
	}
	if !r.waitForEntry(group, sharedSpaceID, "kwlinkboilermonth", 3*time.Minute) {
		t.Fatalf("what david wrote never reached the household:\n%s",
			mustOut(r.lore(group, "search", "--space", sharedSpaceID, "kwlinkboilermonth")))
	}

	// And the property the mode exists for, unchanged: the household's own pod has
	// no idea david has a private space.
	if r.holdsSpace(group, davidSpaceID) {
		t.Errorf("the group's pod holds david's private space; that is the one thing isolated mode promises it does not:\n%s",
			mustOut(r.lore(group, "spaces")))
	}
}

// testAddedToRunning is the case a step that only works at install time fails.
//
// The household from the scenario above is still running. Jordan is added: a new
// container on a new volume, and nothing else. Not a restart of the group's pod,
// not a command anywhere.
func testAddedToRunning(t *testing.T, r *linkRig) {
	group := unit{name: linkPrefix + "l-group-" + r.tag}
	if out, err := r.try("inspect", "--type", "container", "-f", "{{.State.Running}}", group.name); err != nil || !strings.Contains(out, "true") {
		t.Skipf("the household from the previous scenario is not running (%v: %s); "+
			"this scenario is about adding to a running one", err, strings.TrimSpace(out))
	}
	cfg := filepath.Join(r.scratch, "linked.yaml")
	jordan := unit{name: linkPrefix + "l-jordan-" + r.tag, member: "jordan"}
	r.start(jordan, cfg, theLinkKey)
	r.waitServing(jordan, 3*time.Minute)

	if !r.waitForSpace(jordan, sharedSpaceID, 3*time.Minute) {
		out, _ := r.try("logs", jordan.name)
		t.Fatalf("jordan's pod, added to a running household, was never admitted.\nspaces:\n%s\nlog:\n%s",
			mustOut(r.lore(jordan, "spaces")), lastLines(out, 40))
	}
	// And the household's memory, written before jordan existed, is there.
	if !r.waitForEntry(jordan, sharedSpaceID, "kwlinkbinsday", 3*time.Minute) {
		t.Fatalf("jordan joined the space but the household's existing memory did not arrive:\n%s",
			mustOut(r.lore(jordan, "search", "--space", sharedSpaceID, "kwlinkbinsday")))
	}
	t.Logf("jordan, added to a running household with no restart and no command anywhere, holds:\n%s",
		mustOut(r.lore(jordan, "spaces")))
}

// testStrangerRefused is the authority boundary where it actually matters: on the
// network, against the running desk.
//
// A container on the same network, running the same image, configured for the same
// household and the same space — and holding a different link key. It must never be
// admitted. This is the case podman's default network makes real: it is shared with
// every other container the same user runs.
func testStrangerRefused(t *testing.T, r *linkRig) {
	group := unit{name: linkPrefix + "l-group-" + r.tag}
	if out, err := r.try("inspect", "--type", "container", "-f", "{{.State.Running}}", group.name); err != nil || !strings.Contains(out, "true") {
		t.Skip("the household's desk is not running; nothing to be refused by")
	}
	cfg := filepath.Join(r.scratch, "linked.yaml")
	stranger := unit{name: linkPrefix + "s-jordan-" + r.tag, member: "jordan"}
	// A fresh volume, so this store has never been admitted to anything.
	_, _ = r.try("volume", "rm", "-f", stranger.name)
	r.start(stranger, cfg, wrongLinkKey)
	r.waitServing(stranger, 3*time.Minute)
	time.Sleep(60 * time.Second)

	if r.holdsSpace(stranger, sharedSpaceID) {
		t.Fatalf("a store with the wrong household link key was admitted to the household's shared space:\n%s",
			mustOut(r.lore(stranger, "spaces")))
	}
	t.Logf("refused, as it must be — the stranger's store holds only what it made itself:\n%s",
		mustOut(r.lore(stranger, "spaces")))
}

// mustOut renders a (output, error) pair for a log line.
func mustOut(out string, err error) string {
	if err != nil {
		return out + "\n(" + err.Error() + ")"
	}
	return out
}
