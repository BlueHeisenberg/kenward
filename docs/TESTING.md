# Testing

## What is tested where

Every package has table-driven unit tests. They make no network calls and never touch a
real Telegram, a real provider or a real lore — all three are interfaces, and each has a
fake.

That is the right way to test logic and it has a structural blind spot: a package whose
own tests all pass can still be wired to nothing. So there is also `internal/e2e`, which
builds a real configuration, a real supervisor, real scope resolution, real routing and a
real capture engine, and puts fakes only at the three outer edges — the memory client, an
`httptest` server speaking the provider protocol, and the transport.

It earned itself on the first run. Nothing in the codebase ever unlocked a session:
`Provision` and `Unlock` had no non-test callers, so a simple-mode node would have
answered every private message with the locked notice, indefinitely, while group chats
worked normally because a group scope has no key. Every package's own tests passed
throughout.

## Assertions that can fail

Some assertions were mutation-tested — deliberately broken to confirm they fail — because
an assertion that cannot fail is decoration that reads like proof.

The clearest example: the test that a local-only space refuses rather than reaching a
provider asserts the cloud endpoint received **zero** requests. Appending a cloud tier to
that member's chain flips it to a normal answer and one request, so the zero is
load-bearing rather than vacuously true of a test that never routes anything.

Refusal messages, rendered prompts and the privacy statements are golden-tested.
Changing one is a deliberate edit to a fixture, which is the point: nobody should be able
to soften a privacy claim by accident, and the diff should say plainly that they did it.

## Running them

```sh
go test ./...           # everything except integration
go test -race ./...     # needs cgo
```

`-race` needs a C toolchain, which the Windows development machine does not have, so race
runs happen on Linux — in CI, or locally in a container:

```sh
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=1 golang:1.25-bookworm \
    go test -race ./...
```

CI runs gofmt, `go vet`, `go build` and `go test -race` on Linux, macOS and Windows.

## Integration tests

Tests needing real Podman or a real lore carry the `integration` build tag and are
excluded from the default run:

```sh
go test -tags integration ./...
```

They are excluded because they need equipment, not because they matter less. `internal/e2e`
is deliberately **not** among them — it needs nothing but the test binary, so it runs on
every commit, which is the only reason it caught what it caught.

### Isolated mode against real Podman

`cmd/kenward/isolated_podman_test.go` is the only test that starts real containers, and
it exists because isolated mode's defects all lived where no fake could see them: between
`internal/supervisor`, which tests the capability against a fake container backend, and
`cmd/kenward`, which wires it. It builds the image from this repository's own `Dockerfile`,
adds a real `lore` binary to it, and drives `kenward run`'s own isolated wiring against
real podman — the pods' argv and provisioned configuration, which secrets each pod does
and does not hold, the wrapped key landing on the `/work` named volume, a rolling update
preserving a member's lore across a real `Recreate`, a claim code minted on the host
reaching an unenrolled member's own store, a revocation reaching the pod that actually
holds the binding, and a stale pod being rebuilt once rather than on every start.

```sh
go test -tags integration -run TestIsolatedPodman -timeout 30m ./cmd/kenward/
```

It needs Linux, `podman` and `go` on PATH, and a `lore` checkout beside this one
(`KENWARD_E2E_LORE_SRC` overrides the location). Missing any of those it skips and says
which. Every run builds its own image, its own volumes and its own stores under an
`sbx-kwe2e-` name prefix and removes all of it afterwards, failures included; it never
touches a real `~/.lore` or any podman object outside that prefix.

Telegram cannot be exercised — no bot token exists — so every pod runs as far as `getMe`,
is refused and exits. The test asserts that rather than tolerating it: everything it is
about happens before that call and leaves evidence on the pod's volume, and a pod that
died earlier died of something the test is looking for.

One operator step has no automation behind it and the test performs it: a pod's `/work`
volume starts empty, `lore mcp` exits before the MCP handshake when its `LORE_HOME` holds
no account, and `kenward run` refuses to serve a unit whose memory layer does not answer.
So a supervisor-started pod on a fresh volume crash-loops until somebody runs `lore init`
against that volume, and nothing in kenward does it.

## What this has and has not proved

It has proved that the logic is right and that the pieces fit together: scope resolution
holds over generated households, routing cannot reach an endpoint outside a chain, a
group conversation cannot name a private space, nothing is written to memory without a
confirmation, and the whole path from an inbound message to a reply works when assembled.

It has **not** proved the product works, because every boundary is a fake. Nothing has yet
run against a real lore, a real Telegram bot, or a real inference endpoint. A fake agrees
with whatever the code expects — that is what makes it useful and what makes it worthless
for this question. Real lore returns prose this codebase parses with a golden corpus, and
that corpus was written by reading lore's source rather than by watching it run. Telegram
has rate limits and message semantics no `httptest` server imposes. A real model produces
tool calls of a quality no scripted response reflects.

So the correct description of this project today is **implemented**, not working. The gap
closes the first time a message round-trips from a real phone through a real bot to a real
model and back, which is Phase 0's exit criterion and the next thing to do.
