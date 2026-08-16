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

## The installer

`install.sh` is a couple of hundred lines of branching that nobody exercises until somebody
pipes it into a shell on a machine unlike this one. `install_test.sh` runs it against a
locally built binary served over `file://`, inside a throwaway container, once for each of
those machines: no write permission to the target, no `sudo`, no `curl`, an architecture
with no build, a corrupted download, a release with no checksums, an install that is
already there.

```sh
task install:test        # needs Docker
```

It moves system binaries about and installs into `/usr/local/bin`, so it runs in the
container and nowhere else.

The release build itself is rehearsable without publishing anything:

```sh
task release:check       # validate .goreleaser.yaml
task snapshot            # build every artifact into dist/, publish nothing
```

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
