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
| package unit tests | fake | fake | fake `memory.Memory` | every commit |
| `internal/memory` | — | — | **real store, temp home** | every commit |
| `internal/setup/spaces_lore_test.go` | — | — | **real store, temp home** | every commit |
| `internal/e2e` (most files) | `transport.Fake` | `httptest` | scripted recorder | every commit |
| `internal/e2e/telegram_test.go` | **real client, real HTTP** | `httptest` | scripted recorder | every commit |
| `internal/e2e/live_test.go` | `transport.Fake` | **real endpoint** | **real store** | `-tags integration` |
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
production wiring with a real `memory.Client` over a real embedded lore store and a real
`routing.Pool` over a real OpenAI-compatible endpoint, and fakes only Telegram, for which
the suite has no token. A third observer, `loreCLI`, reads and writes the same store
through the `lore` binary out of process, so what kenward wrote is checked with something
other than the library it wrote with.

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

Ten scenarios: a direct message round trip, a Spanish household holding a two-turn
conversation, retrieval reaching the prompt, a group message scoped to shared, an
overheard group message costing no completion while still reaching the history ring, a
Spanish conversation whose prompt is still English, cross-language retrieval across a
restart, the model's Markdown reaching the member as HTML, a member-requested capture
verified by a **separate `lore get` process** looking the entry up by id, and a local-only
chain refusing in two languages. Everything above it in the file was found by it within
minutes of first running, after a full suite had been green for days.

The refusal scenario is worth one more sentence, because it is the shape of failure this
suite is prone to. It asserted MarkdownV2 backticks — `` `local` ``, `` `attic` `` — and
71add9f moved every message into Telegram HTML months of commits ago. Behind a build tag,
nothing said so: the default run stayed green while the assertion had been wrong against
every live endpoint since. It now assembles the expected sentence from the same
`internal/lang` catalogue and the same `transport.Code` the node assembles it from, so
the next parse-mode change cannot leave it stale — and separately asserts that the tier
and the machine reach the member as identifiers, which is the half a catalogue-derived
expectation cannot check on its own.

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
KENWARD_E2E_ENDPOINT=http://192.168.1.20:8000/v1 \
KENWARD_E2E_MODEL=monster \
go test -tags integration -run TestLive -v ./internal/e2e/

# the capture judgement scorecard, same two variables, one more optional
KENWARD_E2E_ENDPOINT=… KENWARD_E2E_MODEL=… KENWARD_EVAL_REPEATS=3 \
go test -tags integration -run TestCaptureJudgement -v ./internal/assistant/

# real Podman, lifecycle only
KENWARD_TEST_IMAGE=docker.io/library/alpine go test -tags integration ./internal/supervisor/
```

| Variable | Read by | Meaning |
| --- | --- | --- |
| `KENWARD_E2E_ENDPOINT` | `internal/e2e/live_test.go`, `internal/assistant/judgement_eval_test.go` | OpenAI-compatible base URL, `/v1` included |
| `KENWARD_E2E_MODEL` | both of those | the model name to ask that endpoint for |
| `KENWARD_LORE_BIN` | `internal/e2e/live_test.go` | the `lore` binary, if it is not on `PATH` |
| `KENWARD_EVAL_REPEATS` | `internal/assistant/judgement_eval_test.go` | samples per case, default 3 |
| `KENWARD_E2E_SKIP` | both | waives the two model-backed suites (see below) |
| `KENWARD_TEST_IMAGE` | `internal/supervisor` | a container image the Podman test may run |

The store is not one of them: the live suite creates its own `LORE_HOME` under
`t.TempDir` and destroys it, so only the model endpoint is external.

**The two model-backed suites fail rather than skip when the endpoint is missing**, and
that is deliberate. `go test` prints nothing at all for a package whose tests skip — no
reason, no count, just `ok` and a duration — so a suite that skips on a missing endpoint
is indistinguishable from one that ran and passed, which is how two runs were believed on
2026-08-17. Nothing automated pays for the change: the `integration` tag already keeps
both files out of `go test ./...`, and no CI workflow in this repository passes the tag.
Somebody running the whole tagged suite for other equipment waives them with
`KENWARD_E2E_SKIP=1`, which is loud in the other direction — they had to type it.

The Podman test still skips on its own, and `internal/memory`'s and `internal/setup`'s
lore tests need no equipment at all, so `go test -tags integration ./...` with the waiver
set is green and says so.

`internal/e2e`'s other files are deliberately **not** tagged — they need nothing but the
test binary, so they run on every commit, which is the only reason they caught what they
caught.

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

### A clean install, walked end to end

`install_test.sh` proves the installer survives those unfriendly machines and stops
there. What it says nothing about is the next five minutes, and the next five minutes are
the first run — the one path that exists exactly once per install and is therefore the one
nothing ever walks a second time. `.e2e/fresh-install.sh` walks it, from nothing, every
time:

```sh
.e2e/fresh-install.sh <bot-token>
```

A temp root with no `kenward.yaml`, no data directory, no admin account and no lore store.
A release snapshot built and installed by `install.sh` itself over `file://`, so the
binary under test arrived the way a stranger's does. `lore init` into this run's own
store. The first-run dashboard, and the setup token it prints. The wizard screens in a
real browser — seven in simple mode, eight in isolated, which has one more for each
member's own bot token and passphrase — including `/v1/models` against a real vLLM. A member added from the
Members page. Then `kenward run` against the configuration that came out, with a real bot
token, and `curl` against the dashboard the wizard turned on. Then all of it deleted,
failures included.

Two steps wait for a browser: the wizard and the member add. They print what to fill in
and poll `kenward.yaml` until it says the browser did it, so Playwright drives them and so
does a person. `KENWARD_E2E_HOLD=<seconds>` keeps the node up at the end, which is the
door left open for claiming the code from a real Telegram account — that conversation has
its own script and is not duplicated here. The whole thing takes about two minutes plus
however long the browser takes.

Thirty-eight assertions, and the ones that earn their place are the ones about what
crossed a boundary rather than what a screen said: the bot token is in `.env` and nowhere
in `kenward.yaml`; `lore` itself lists the spaces the file claims; the claim code digest is
in `invites.json` under the member id the dashboard derived; `/overview` redirects and a
wrong setup token gets a 403 before an account exists. The context-window assertion was
written because the number was wrong: the endpoints step read 262144 off vLLM, displayed
it, and wrote 16384, because `setup.EndpointAnswer` had no field to carry it through. That
was a 262144-token machine configured as a 16384-token one, with nothing anywhere saying
so, and nothing else in this repository would have caught it.

The last step breaks the household on purpose. `LORE_HOME` is moved aside and `kenward
run` is started again against the same configuration; it must exit non-zero within
forty-five seconds, name what did not answer, and say `lore init`. A run that hangs fails
this as surely as one that serves.

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

A pod's `/work` volume starts empty, a lore home with no account in it cannot be opened,
and `kenward run` refuses to serve a unit whose memory layer does not answer — so a fresh
volume used to crash-loop until an operator ran `lore init` against it. `kenward run` now
does that for itself with `lore.Init` before it serves, and this suite exercises that
path rather than performing the step on its behalf.

## What this has and has not proved

It has proved that the logic is right, that the pieces fit together, and — since the live
and Telegram suites landed — that the two dependencies most likely to disagree with their
fakes do not: lore is now imported rather than imitated, so `internal/memory` has no fake
left to disagree with it, and the real `go-telegram` client confirms its offsets correctly
over real HTTP. Scope resolution holds over generated households, routing cannot
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
- **The shared space in isolated mode, beyond one run.** It works now: every pod runs
  `lore serve --lan` (D-044), and a three-pod household on real Podman read an entry
  written in one pod from the other two while every private entry stayed invisible to every
  sibling. `internal/memory/sync_test.go` asserts the daemon is supervised, restarted and
  stopped, and that its status is read from the daemon's own answer. What no test asserts is
  the convergence itself, because it needs more than one container.
- **An SELinux-enforcing host.** The `z` and `Z` labels on every bind mount were verified
  to be a no-op where SELinux is off. The enforcing host they exist for was not available.

So the correct description of this project today is **working in a laboratory**, not
working in a household. That is a smaller gap than the one this section used to describe,
and it is still the gap that matters.
