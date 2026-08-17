//go:build integration && linux

package main

// A defect, reduced to the smallest thing that shows it.
//
// # What it is
//
// In isolated mode the household's kenward.yaml is provisioned into each pod at
// PodConfigPath, and that happens at Create and Recreate time only. Nothing afterwards
// compares what a pod is holding with what is on the host:
//
//   - supervisor.NewIsolated reads the file once, into IsolatedOptions.ConfigFile's
//     bytes, and hands those bytes to the backend when it makes a container.
//   - Roll recreates every pod, but only when the *image* changes.
//   - recreateStalePods recreates a pod when its invite seed or its revocation record is
//     newer than the pod. pod.stale() asks about those two files and nothing else, and
//     it answers false for the group's pod unconditionally.
//
// So an operator who edits kenward.yaml and restarts `kenward run` gets a host
// supervisor on the new configuration and pods still running the old one. Nothing says
// so. `kenward doctor` on the host reads the new file and reports on it; the pods keep
// serving the old one until something unrelated — a new image, a revocation — happens to
// rebuild them.
//
// # Why it matters more than it looks
//
// It is the documented first-run path for isolated mode, not an exotic edit.
// docs/IMPLEMENTATION.md §8 has the operator create the household's lore spaces *after*
// the pods exist — they can only be created inside a pod, by that pod's own lore account
// — and then put the ids lore minted into kenward.yaml. Following that recipe writes a
// configuration the pods never read. The household then runs with whatever space ids the
// file had before, and the symptom is not a startup failure but this, from inside the
// pod, at the first turn that touches memory:
//
//	✗ space "…" is not a space this lore store holds
//
// with reads returning "(couldn't be read)" to the household and writes refused with
// "the memory store refused the write". That is how it was found: the third-scope suite
// provisioned spaces exactly as §8 says to, and every memory assertion failed while
// every pod looked healthy.
//
// # What this test does
//
// Nothing but the mechanism. No Telegram, no lore, no model: bring a household up, edit
// one visible byte of kenward.yaml, start it again on the same image, and read the
// configuration back out of the container with `podman cp`.
//
//	go test -tags integration -run TestPodConfigGoesStale -timeout 20m -v ./cmd/kenward/

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

func TestPodConfigGoesStale(t *testing.T) {
	r := newRig(t)
	hh := newHousehold(t, r, soloFor("david", "David"), householdEnv())
	pod := hh.memberPod("david")

	// --- 1. up once, on the configuration as written ---

	hh.bootstrapVolumes(r.image, []string{pod})

	first := string(hh.readFromContainer(pod, supervisor.PodConfigPath))
	if !strings.Contains(first, "name: Ashfield") {
		t.Fatalf("the pod is not holding the configuration this test wrote:\n%s", first)
	}

	// --- 2. the operator edits kenward.yaml ---
	//
	// The household's name, because it is a value with no behaviour attached: if a pod
	// misses this it is missing every other edit too, and asserting on a name keeps the
	// test about the delivery mechanism rather than about whatever the changed setting
	// would have done.
	edited := strings.Replace(first, "name: Ashfield", "name: Ashfield Edited", 1)
	if edited == first {
		t.Fatalf("could not edit the configuration for the test")
	}
	writeFile(t, hh.h.config, []byte(edited), 0o644)

	// --- 3. `kenward run` again, same image ---

	sup, backend := hh.supervisorFor(r.image)
	hh.start(sup, func() bool { return reachedTelegram(hh, pod) })

	// --- 4. what is the pod actually holding? ---

	got := string(hh.readFromContainer(pod, supervisor.PodConfigPath))
	switch {
	case strings.Contains(got, "name: Ashfield Edited"):
		t.Logf("the pod picked up the edited configuration; the defect this test was written "+
			"for is fixed. Recreates this run: %d", backend.ops("Recreate", pod))
	default:
		t.Errorf(`DEFECT: a pod keeps the configuration it was created with.

The host's kenward.yaml now says "name: Ashfield Edited". The pod at %s is still
holding "name: Ashfield", after a full stop and start of the isolated supervisor on
the same image. podman recorded %d Recreate call(s) for it this run.

Nothing in the supervisor compares a pod's provisioned configuration with the host's:
the bytes are read once in NewIsolated and written only by Create/Recreate; Roll fires
on an image change; and pod.stale() asks only about the invite seed and the revocation
record — and answers false for the group's pod always.

Operator consequence: editing kenward.yaml and restarting has no effect on a running
household, and nothing reports the divergence. docs/IMPLEMENTATION.md §8's own
provisioning recipe ends in exactly this state, because it has the operator write
lore's minted space ids into kenward.yaml after the pods already exist.

--- the pod is holding ---
%s`, supervisor.PodConfigPath, backend.ops("Recreate", pod), got)
	}
}
