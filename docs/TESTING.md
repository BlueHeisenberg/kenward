# Testing

## What is tested where

Every package has table-driven unit tests. They make no network calls and never touch a
real Telegram, a real provider or a real lore — all three are interfaces, and each has a
fake.

That is the right way to test logic and it has a structural blind spot: a package whose
own tests all pass can still be wired to nothing. So there is also `internal/e2e`, which
builds a real configuration, a real supervisor, real scope resolution, real routing and a
real capture engine, and puts fakes only at the outer edges.

It earned itself on the first run. Nothing in the codebase ever unlocked a session:
`Provision` and `Unlock` had no non-test callers, so a simple-mode node would have
answered every private message with the locked notice, indefinitely, while group chats
worked normally because a group scope has no key. Every package's own tests passed
throughout.

Which edges are still faked is now different per file, and that difference is the point
of the rest of this document.

| Suite | Telegram | Model | lore | Runs on |
| --- | --- | --- | --- | --- |
| package unit tests | fake | fake | scripted fake | every commit |
| `internal/e2e` (most files) | `transport.Fake` | `httptest` | scripted recorder | every commit |
| `internal/e2e/telegram_test.go` | **real client, real HTTP** | `httptest` | scripted recorder | every commit |
| `internal/e2e/live_test.go` | `transport.Fake` | **real endpoint** | **real `lore mcp`** | `-tags integration` |
| `internal/memory/integration_test.go` | — | — | **real `lore mcp`** | `-tags integration` |
| `internal/setup/spaces_lore_test.go` | — | — | **real `lore` CLI** | `-tags integration` |
| `internal/supervisor/isolated_integration_test.go` | — | — | — | `-tags integration`, Linux, **real Podman** |

## The real Telegram client, on every commit

`internal/transport/telegramtest` is a local Bot API server on loopback, and
`internal/e2e/telegram_test.go` drives the production wiring against it: the real
`go-telegram/bot` client, real HTTP, real long-polling, real inline keyboards, and a real
`callback_query` tap that causes a real write. No production seam was added for it —
`WithAPIServer` and the transport injection point already existed.

The server is deliberately unhelpful in the ways Telegram is unhelpful. `getUpdates`
implements Telegram's actual offset semantics rather than draining a queue on read: an
update is held until a later poll asks past it and handed out again meanwhile
(`telegramtest/server.go:297`), so a client that never confirms anything is now
distinguishable from a correct one — break the confirmation and the suite reports one
message delivered seventeen times. A wrong token gets `401`
(`telegramtest/server.go:225`) so a misconfigured bot fails at `getMe` as it would in
production, and an unknown method gets `404` (`telegramtest/server.go:293`) rather than a
permissive `ok`. `Offsets()` and `Deliveries()` are the observers the tests assert on.

Six scenarios run there, including a capture confirmed by a real button tap, a capture
declined by one, and a failed poll that must neither lose nor repeat a message. Fourteen
mutations were introduced one at a time and each was watched to fail — the button tap
accepted from the wrong member, the keyboard left on a retired question, a declined
proposal writing anyway, a group turn touching a private space.

These run under a plain `go test`, not behind the integration tag, because the subject is
the wire rather than the store.

## Real lore and a real model

`internal/e2e/live_test.go` is the one suite with no fake below Telegram. It builds the
production wiring with a real `memory.Client` over a real `lore mcp` subprocess and a real
`routing.Pool` over a real OpenAI-compatible endpoint, and fakes only Telegram, for which
the suite has no token.

Two observers sit in the path and neither replaces a dependency: `spyMemory` records which
spaces each call named and then delegates, so every answer comes from lore; and
`recordingProxy` is an HTTP relay that records the request body and forwards it unchanged,
so every completion comes from the model. Assertions about what reached the model are
therefore assertions about real bytes on a real wire.

Each run builds its own store under `t.TempDir` and creates its own spaces there
(`live_test.go:115`). The first version wrote into a persistent space and left the entry
behind; after eight runs the accumulated near-duplicates crowded out the entry the run had
just written. lore exposes no way to delete an entry or a space, so isolation had to come
from the store being disposable rather than from cleanup — the same defect and the same
fix as `internal/memory/integration_test.go`.

Five scenarios: a direct message round trip, retrieval reaching the prompt, a group
message scoped to shared, a confirmed capture verified by a **separate `lore get`
process** looking the entry up by id, and a local-only chain refusing. Everything above it
in the file was found by it within minutes of first running, after a full suite had been
green for days.

## What has actually been run by hand

Some things have no automated form, either because they need a credential this repository
must not hold or because they need hardware. They were run once, deliberately, on
2026-08-16, and what they produced is recorded here because a manual run nobody wrote down
is indistinguishable from one that never happened.

- **A full round trip through `api.telegram.org`.** A real bot from BotFather, real
  long-polling against Telegram's own servers, a real claim-code enrolment, a real inline
  keyboard tapped from a phone, a memory write confirmed afterwards by an independent
  `lore` process, and retrieval of that memory in a later turn. Run twice: once against
  `qwen2.5:3b` and once against a real 27B (`Qwen3.8-27B` on vLLM). Nothing in this
  repository re-runs it, and nothing can without a token.
- **Two real inference endpoints**, through kenward's own routing stack. This is where
  `endpoints[].context_window` and `endpoints[].max_completion_tokens` came from, and
  where the reasoning-only turn was measured: the identical request returned a trace and
  null content at 20, 256 and 1024 completion tokens and a complete answer at 4096, under
  both `length` and `stop`.
- **Isolated mode against real Podman 4.9.3**, including `Isolated.Roll` recreating a
  member's pod and leaving that member's lore where it was, and the pod-mtime comparison
  in `Start` — with a revocation record in place the first `Start` rebuilds the pod and
  the second and third leave it alone.
- **Both compose files against real Docker Compose.** Neither had ever been run before
  that, and neither worked: `compose.simple.yml` crash-looped eight times in fifteen
  seconds on a passphrase prompt it printed into the log, and set no `LORE_HOME`, so lore
  fell back to `$HOME/.lore` on a read-only rootfs and memory was silently dead.

Only the last three left an artefact in this repository. The automated real-Podman test
(`internal/supervisor/isolated_integration_test.go`) covers the lifecycle — `Start` to
`StateReady` to `Stop` — and not `Roll`; `Roll`'s ordering, volume preservation and
stop-at-first-failure are covered against a fake backend in
`internal/supervisor/isolated_test.go:362-802`.

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

Temperature is pinned to zero in the live suite. It is unset in production, ollama then
defaults to 0.8, and a small model sampling at 0.8 decided differently about emitting a
tool call roughly one run in five. The model still decides; it just decides the same way
twice.

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

Tests needing real Podman, a real lore or a real model carry the `integration` build tag
and are excluded from the default run, because they need equipment rather than because
they matter less:

```sh
# real lore and a real model, in a store this run creates and throws away
KENWARD_E2E_ENDPOINT=http://localhost:11434/v1 \
KENWARD_E2E_MODEL=qwen2.5:0.5b \
go test -tags integration -run TestLive ./internal/e2e/

# real lore, parser and MCP handshake only
KENWARD_LORE_BIN=/path/to/lore go test -tags integration ./internal/memory/

# real Podman, lifecycle only
KENWARD_TEST_IMAGE=docker.io/library/alpine go test -tags integration ./internal/supervisor/
```

Each skips rather than fails when its equipment is absent, so `go test -tags integration
./...` on a bare machine is green and proves nothing.

`internal/e2e`'s other files are deliberately **not** tagged — they need nothing but the
test binary, so they run on every commit, which is the only reason they caught what they
caught.

## What this has and has not proved

It has proved that the logic is right, that the pieces fit together, and — since the live
and Telegram suites landed — that the two dependencies most likely to disagree with their
fakes do not: lore's real output still matches the golden corpus and answers the MCP
handshake this SDK performs, and the real `go-telegram` client confirms its offsets
correctly over real HTTP. Scope resolution holds over generated households, routing cannot
reach an endpoint outside a chain, a group conversation cannot name a private space, a
shared write requires approval, and the whole path from an inbound message to a reply
works when assembled.

What remains unproved is narrower than it was, and worth naming precisely:

- **Telegram itself.** Rate limits, chat migrations, privacy mode, poll conflicts, TLS,
  and a real network. `telegramtest` models `getUpdates` and nothing else Telegram does
  under load. The one manual run above is a single trip, not a soak.
- **A household using it.** Phase 3 of the roadmap is zero days old. Every measurement so
  far comes from a laboratory, and the only real product research available is a month of
  people who did not write this software trying to live with it.
- **The shared space in isolated mode.** It does not work, is known not to work, and is
  documented as an open limitation in `ARCHITECTURE.md` and in
  `deploy/compose.isolated.yml`. No test asserts it, because there is nothing yet to
  assert.
- **An SELinux-enforcing host.** The `z` and `Z` labels on every bind mount were verified
  to be a no-op where SELinux is off. The enforcing host they exist for was not available.

So the correct description of this project today is **working in a laboratory**, not
working in a household. That is a smaller gap than the one this section used to describe,
and it is still the gap that matters.
