# kenward — architecture

This is the *why* document: what kenward is, what it refuses to be, and the reasoning
behind the decisions that would otherwise look arbitrary in the code.

[IMPLEMENTATION.md](IMPLEMENTATION.md) is the *how* — the package tree, the interfaces,
the exact semantics of routing, capture and enrolment, and the configuration schema.
Where this document explains a choice, that one binds the code to it. Nothing is
duplicated between them on purpose: if you want the type, go there; if you want the
argument, stay here.

Anyone cloning this repository should be able to understand the design from this file
alone. Family-level context — how kenward relates to [lore](https://github.com/BlueHeisenberg/lore),
[keel](https://github.com/BlueHeisenberg/keel) and their shared history — lives in the
design repository and is not needed to read this.

## What a household runs

One household runs one kenward deployment, on whatever machine is always on. A mini PC, a
NAS, an LXC container on a Proxmox host, a desktop that never sleeps. It holds no GPU and
costs effectively nothing to keep running.

It has no inbound network surface today. It connects outbound to Telegram and outbound to
inference endpoints on the LAN or a tailnet. Household members need no VPN, no port
forwarding and no client software beyond Telegram, which they already have — and that stays
true for members, because the admin dashboard decided in **Non-goals** below is the
operator's console and nobody else's. That dashboard is the one thing that opens a port,
and its exposure rules are stated where it is.

There is no multi-tenant runtime and no `tenant_id` threaded through queries, in any
deployment shape. A household assistant is idle almost all of the time, so the memory cost
of running one deployment per household is trivial next to the cost of a cross-household
leak. This is also the decision that keeps a hosted offering from becoming a rewrite, and
it is free to honour now.

## Two modes, one binary

Not two products. Two topologies of the same binary, chosen once during setup.

| | **Simple** | **Isolated** |
| --- | --- | --- |
| Process model | one process, one goroutine tree per member | one pod per member |
| Keys | one address space, one operator passphrase | per-pod, argon2-wrapped by the member |
| Telegram | one household bot | one bot per member |
| Runs on | Windows, macOS, Linux, container | Linux with Podman or Docker |
| The operator can read private memory | **yes** | no |

The assistant loop, the memory policy, the routing and the capture flow are identical.
Only the supervisor differs. If a change to the per-member unit needs to know which mode
it is running in, that change is wrong.

This works **only** because the per-member assistant is an isolated unit from the first
commit — no shared mutable state keyed by member id, anywhere. That property cannot be
retrofitted: one map keyed by member in a shared address space turns the mode split into a
rewrite. It is therefore a build rule rather than an aspiration, and it is enforced in
[IMPLEMENTATION.md](IMPLEMENTATION.md) §0.

**The setup question is about trust, not topology.** A non-technical installer can answer
*"does everyone here trust whoever runs this machine to be able to read their private
conversations?"* They cannot answer *"do you want per-user pods?"* The question decides
the mode; the topology is a consequence.

**Simple mode is honest about being simple.** Separation between members is real in both
modes. Sealing against the operator exists only in Isolated. Simple mode must never be
described with the sealed-memory language, in this repository or anywhere else. Most
households are fine with that, because the operator is a member of their own family; the
ones that are not have Isolated mode.

Migration from Simple to Isolated has to stay possible — generate per-member keys, re-wrap,
split the process, hand out bot tokens — which is only true if Simple mode never writes
anything assuming a single key.

## Memory: lore, unchanged

kenward owns no knowledge model. It calls `lore_search`, `lore_get`, `lore_put`,
`lore_spaces` and `lore_share` on a `lore mcp` subprocess and treats lore's spaces,
entries, markers, confidence and origin fields as given.

The hard half of a household assistant is per-person identity, cryptographic separation,
invites and multi-device sync. That half already exists in lore, so kenward is the
assistant loop on top of it rather than a memory system built from scratch. The seam is
also what kept a change of implementation language from touching memory at all.

Three adaptations, all configuration rather than changes to lore:

**Private memory is a two-member space, not lore's `personal` space.** lore's `personal`
space is bound to one account and never crosses accounts, so a node could never read it —
which makes it useless for an assistant that has to answer from memory. A `shared`-kind
space whose members are exactly the person and the node keeps the cryptography honest and
requires no change to lore. This was the design's original reasoning and is now confirmed,
by reading lore's source, as the only workable option.

**Spaces are addressed explicitly, always.** lore's project spaces resolve from the
working directory and git remote, which is coding-assistant shaped and meaningless in a
house. Every call names its space. A search with no explicit space set is an error, never
"all" — that is the mechanical guarantee that no code path can accidentally search
everything.

**Retrieval is a union, writes are not.** The asymmetry is the product.

| Conversation | Reads | Writes |
| --- | --- | --- |
| Direct message from a member | their private space, then shared | their private space |
| Household group chat | shared space only | shared space |

A private conversation reads shared memory because that is useful — you should be able to
ask when the bins go out. A group conversation can never read a private space, because
that is the entire point. Copying something out of a private space into the shared one is
an explicit, reviewed act using `lore_share`, so that lore's own provenance survives the
move and the member sees the full text before it is published.

Multi-space results come back grouped in the order the spaces were asked for, not globally
re-ranked. Ranking across spaces is a policy decision that belongs to the assistant rather
than to the memory client — and it is genuinely unsolved: "private first" is an ordering,
not a ranking strategy.

### What depending on lore costs

Established by reading lore's source rather than assuming, and listed here because several
of these constrain the design:

- `lore mcp` is stdio only and returns unstructured text, not JSON, so the client is a
  parser tested against a golden corpus. A format change should fail loudly in one place.
- There is no Go client package — everything in lore is under `internal/` — so the wire is
  the only interface even though both sides are Go.
- **Sync is last-writer-wins per entry, not a CRDT.** The losing version is discarded
  silently, with no conflict record, and a machine with a fast clock wins every conflict.
  Household clocks should be synced, and nothing in kenward may assume a write it made is
  still there.
- **Delete is a signed tombstone, by id, and space-scoped.** `lore_delete(id, space)`
  stops an entry coming back from search and get, here and on every synced device;
  deleting an already-deleted entry is a no-op. It is not a shred, and nothing kenward
  says to a member may promise one. It is also no help after a write whose answer was
  lost — the id was in the receipt that never arrived.
- `lore mcp` alone never syncs; that needs a separate `lore serve`. Any deployment running
  more than one lore instance must run both.
- Invites are not exposed over MCP, so enrolment drives lore's CLI.
- `confidence` and `origin` are enforced enums, but **markers are free-form** — the
  familiar vocabulary is convention only and must not be validated against.
- Instances isolate by `LORE_HOME`, not by machine. Several lore daemons can run on one
  host, each holding a subset of spaces. This is what makes one lore per member pod
  viable, and it was the most important open question in the whole design.
- **They do not converge on their own.** See the open limitation below.

None of this justifies forking lore. Forking trades a list of known limits for an unbounded
amount of new work, and every one of these limits is survivable for a household.

### Open limitation: the household's shared space in isolated mode

This document used to say that per-pod lore instances converge on the household's shared
space. **They do not, and nothing in either deployment path makes them.**

One `LORE_HOME` per pod is one lore *account* per pod, and `lore init` gives each account
its own set of space ids. So the id `household.shared_space` names resolves inside
whichever store created it and nowhere else. Every other pod's `doctor` reports `space "…"
is not a space this lore store holds`, and a conversation in that scope reads nothing —
silently, because a turn that cannot read a space degrades that space rather than failing.

Making one shared space real across three stores is lore's own sharing: `lore space
invite` on the owning store, `lore join` on the others, and a `lore serve` reachable
between them. **The symbol whose existence would falsify this claim is a call to any of
those three from kenward** — there is none in `internal/supervisor`, none in
`internal/setup`, and no port, peer or daemon in `deploy/compose.isolated.yml`. The
supervisor path has the identical gap.

Each member's **private** space is unaffected: it lives in that member's own store, which
is the property the mode exists for. Until the sharing is wired, isolated mode's household
group has memory only in the group pod, and `deploy/compose.isolated.yml` says so in its
header.

This was found by running the mode rather than by reading it, and it is recorded rather
than fixed because the next section moves kenward from `lore mcp` to a Go import, which may
change what the fix looks like. Solving it twice would be the waste.

### Decided but not built: lore becomes an import

D-036 replaces the MCP seam described above with a Go package published at lore's module
root, which kenward imports. `cmd/lore` keeps `lore mcp` for every other MCP client and
`lore serve` for sync; what goes away is kenward's text-parsing layer, deleted rather than
ported.

The seam bought two real things — a language rewrite that cost the assistant and nothing
else, and a lore forced to have an external surface — and neither is still being paid for.
What it costs is countable: `internal/memory/parse.go` is 500 lines mirroring lore's format
strings, `errors.go` is 253 lines classifying error text, and `testdata/` holds 31 captured
fixtures to keep them honest. Several properties this document attributes to lore turn out
to be artefacts of the MCP server rather than of lore — search returns whole entries with
origin and timestamps, and the excerpt doctrine threaded through `internal/memory` and the
prompt exists because the server prints only the snippet.

**The never-fork rule survives unchanged.** kenward imports lore, defines no knowledge
model, reimplements nothing.

**Nothing of this is built**, and the falsifying symbol is a one-liner: a
`github.com/BlueHeisenberg/lore` line in `go.mod`. There is none, `internal/memory` still
speaks MCP over stdio, and everything above this section describes what the code does
today. Two preconditions also sit on lore's side — its `go.mod` carries an absolute-path
`replace` for agentmesh, so it builds on one machine, and it has no `LICENSE` file at all.

## Identity and enrolment

A member is a stable internal id, not a Telegram id. The Telegram id is bound to it at
enrolment and can be rebound; deriving identity from the transport would mean a member who
changes accounts becomes a different person to their own memory.

Resolving an inbound message to a scope — who this is, what this conversation may read,
what it may write, which tiers it may use — **is** the authorization decision. Everything
downstream obeys the resolved scope and re-derives nothing. There is one place to audit and
one place to test, and it is tested exhaustively, including unknown users, unknown chats, a
member messaging from an unexpected chat, and a group id colliding with a private space.

Authorization is deliberately not derived from group membership. Being added to the
household group chat by someone else grants nothing.

**Enrolment needs a secret, because Telegram bot usernames are publicly discoverable and
anyone may send `/start`.** Dynamic enrolment without one silently rewrites the model from
*anyone not enrolled is ignored* into *anyone who found the bot is enrolled*. So: claim
codes, minted by the operator, single-use, expiring, stored hashed, rate-limited and
compared in constant time.

An unknown sender receives **nothing at all** — not an error, not a prompt, not "you are
not authorised". A reply of any kind confirms to a stranger that the bot exists and is
someone's household assistant.

Removal unbinds the Telegram id and reports that the space key must be rotated in lore.
kenward cannot rotate it, and says so rather than implying the removal was complete.

## Inference routing

Most people with a local-inference habit have a scatter of machines: a GPU workstation, a
laptop, a mini PC, none of them on all the time. kenward treats them as a pool.

Endpoints carry tier tags. Each space declares an ordered chain of tiers it may use.
Routing walks the chain, load-balancing within a tier and falling through when a tier has
nothing reachable, and when the chain is exhausted it **refuses**.

There is deliberately no code path that widens a chain. No default, no fallback, no "if
nothing matched, try everything". This is tested directly: a pool containing a cloud
endpoint, asked for a local-only chain with no local endpoint reachable, must leave the
cloud endpoint with zero requests.

Two practical facts shape the mechanism. A powered-off machine sends no TCP reset, so a
request to it hangs on the operating system's connect timeout instead of falling through —
a short connect probe with a few seconds of result caching turns that into a two-second
skip. And failover happens before the first token only: once a response has begun
streaming there is no clean retry, and pretending otherwise produces spliced output.

**Two axes of policy, and no more:** capability tier (a stronger tier and a cheaper one)
and location tier (local versus cloud). Resisting a third axis is a design constraint
rather than an oversight — per-user, per-task policy matrices are what make self-hosted
products unusable. The one thing that could force a third axis is task difficulty, and
that is an open question rather than a plan (see below).

Model identity should be consistent within a tier. Mixing materially different models under
one tag makes answer quality vary between turns for reasons the user cannot see.

### Per-space routing policy is the privacy mechanism

A space allowed only local tiers reports that no local machine is reachable rather than
falling back to a provider. That is the difference between claiming privacy and having it,
and it is a configuration line rather than a subsystem: *private conversations never leave
my hardware, and here is the config that proves it.*

It also gives the availability story a purpose beyond convenience. When a private space has
no local backend awake, the correct behaviour is to say so — naming the tiers allowed and
the endpoints that could not answer — not to silently ship someone's private context to a
third party. The wording is careful about that second list: it holds endpoints that were
attempted alongside endpoints skipped for a cooldown or a failed probe, so the refusal says
they were unavailable rather than claiming an attempt that never happened.

The same principle runs past refusals. A rate-limited provider, a rejected key, a model
that declined the turn, a locked key — each produces a short notice saying what happened
and what the member can do about it, because silence is the one reply that teaches a
household the assistant is broken and unpredictable. Every one of those strings is a
product surface, and they are golden-tested for exactly that reason.

## Key custody, and what the privacy claim actually is

A model must see plaintext to answer. A server-side assistant therefore cannot be
end-to-end encrypted with respect to its own server, and no arrangement of keys changes
that. lore's relay avoids the problem by being a dumb pipe; kenward cannot.

What is left is reducing exposure honestly:

| Exposure | Answer | Residual |
| --- | --- | --- |
| At rest | Per-member key, argon2-derived, unwrapped into memory only while that member has an active session, never written to disk | Nothing meaningful — backups and stolen disks yield nothing |
| In flight | Per-member pod and per-member bot token, in Isolated mode | Host root |
| Inference | Local endpoints the household controls | Whoever owns the GPU box; watch provider prompt logging |

**Per-member bot tokens exist because encryption at rest does nothing for the channel.**
Whoever holds a bot's token can read every message sent to it. Under a single household
bot, the operator can read every private conversation in flight no matter how well the
memory is sealed on disk. Each member creating their own bot, running in their own pod,
closes that at the application layer, and costs about five minutes of setup per person. In
Simple mode there is no isolation to protect, so one household bot is both the right answer
and the honest one.

**The claim is not "the operator cannot read your memory."** It is: *the operator cannot
read it from disk, from a backup, or while you are not in session, and doing so requires
deliberately attacking their own family.* That is strong, checkable and true. The stronger
version is neither.

Three limits stated plainly, because they are the first things a privacy-minded reader will
check:

- **The node can read private spaces at all.** That is what being the second member of a
  two-member space means, and it is the price of an assistant that answers when your laptop
  is closed.
- **Root always wins.** An attacker with root on the host during an active session reads
  the unwrapped key out of memory. Isolated mode raises the cost; it does not create an
  impossibility.
- **In Simple mode the operator can read everything** — every member's private memory, at
  rest and in flight. That is the mode's known limitation, not a bug, and `kenward doctor`
  says so out loud.

An unresolved product question sits underneath all of this: unlock-on-message is usable and
weaker, unlock-on-passphrase is strong but means nothing can happen for a member while they
are away. The idle timeout is a guess until there is real usage.

## Capture

The rule used to be "no automatic memory writing", which was correct and unusable — a
member has to notice something is worth remembering and then say so, which nobody does. The
first refinement made the model *propose* and the member *decide*, with buttons, on every
write.

**That was still one question too many, and D-038 splits the two directions apart.** A
throttle limits how often the question is asked; it does not change what the question does
to somebody's attention. A confirmation on every private write makes the assistant's memory
feel like paperwork, and the failure that follows is not a bad write — it is a household
that stops letting it remember anything.

- **A private write is performed and announced, with an Undo on the announcement.** The
  member sees the exact words, after the fact, and one tap removes them.
- **A shared write is still approved before it happens, and that is not configurable.**
- **Announcing a write is not configurable.** A memory that fills silently is a different
  product from this one.
- **Announcing a read is configurable, default on.** It is information rather than a
  control.

Undo needed a delete lore did not have. It became a lore change: `lore_delete(id, space)`
writes a signed tombstone that propagates, and `internal/memory.Delete` reaches it. A
tombstone is not the same promise as a removal, so the announcement says which — *"it
won't come back in an answer, here or on any other device"* rather than *"erased"*.

Three endings, three sentences, because the entry is in a different state in each: gone,
still there because lore refused, or unknown because lore never answered. Reporting either
of the last two as "undone" would be the plainest lie the product could tell — the member
asked for something to stop existing and would be told it had. An undo also counts as a
decline, or the next turn's proposal is written straight back.

This document previously claimed that *nothing is written to memory without the member
seeing the exact words first and saying yes.* That claim is retired rather than quietly
softened, because a claim that weakens without saying so is worse than one never made.
What replaces it is narrower and still true: nothing is written without the member being
shown the exact words, and nothing leaves their private space without their approval first.

Two invariants, not preferences:

1. **A group conversation may never offer "personal."** Otherwise the household chat
   becomes a write path into a private space, which is the one thing the memory model
   exists to prevent.
2. **Private → shared is a separate, louder act**, showing the full text before publishing,
   because publication is irreversible from the household's point of view: other people
   have read it by the time anyone regrets it, and deleting the copy afterwards does not
   unread it. That is why the shared path has no policy switch while the private one
   does.

Plus a throttle on whatever is still asked: one proposal per turn, and none for anything
already retrievable. Otherwise everyone learns to reflexively decline, and the feature is
worse than not having it. A proposal that was declined recently is suppressed rather than
repeated.

Answers — approvals and Undos alike — are accepted only from the member being asked. In a
group chat, everyone can see and tap an inline keyboard, and without that filter another
member could route someone else's memory. A timeout on an approval is treated as declined,
never as accepted.

**Where the code is:** D-038 is decided and not yet implemented. Until it is, every private
write is still a question, and `internal/capture` still says there is deliberately no way
to turn that question off. The symbols that will say it landed are an announce-and-Undo
path in `internal/capture` and an outcome for it beside `OutcomeSaved` and
`OutcomeDeclined` in `capture.OutcomeKind`.

## Tool surface

Deliberately thin, plus MCP passthrough so households bring their own tools.

Household assistants are only useful with real-world access — calendar, shopping,
documents, home automation — and building those integrations is unbounded work that has
drowned every project in this category. kenward ships a small native tool set and otherwise
acts as an MCP client, which makes integrations the ecosystem's problem rather than the
maintainer's.

What gets built natively is decided by watching a household use it for a month, not by
guessing. This is the phase where scope becomes unbounded if it is allowed to.

**The known risk:** models in the 27–30B class remain unreliable at multi-turn tool
calling, and an assistant sold on privacy that flubs tool calls burns trust quickly. If
that holds when measured, routing needs a difficulty axis — a third axis, which the
two-axis constraint above forbids. This is the only open question that can still force an
architecture change, and it is deliberately measured before anything is built that would
have to absorb it.

## Language and stack

Go 1.25, single static binary, no cgo. Cross-platform everywhere except the isolated
supervisor, which is Linux-only and degrades with a clear error rather than a panic.

Go rather than Python because the two packages worth inheriting are Go; because lore, the
thing being talked to, is Go; because per-member processes cost roughly 25 MB each instead
of 150 MB, which matters once there is one per household member; and because a single
static binary is a materially better self-hosted install story than a container-only one.
Go 1.25 is the floor because the MCP SDK requires it.

Three dependencies do real work — `go-telegram/bot` for transport, the official MCP Go SDK
for lore, `yaml.v3` for configuration — plus [keel](https://github.com/BlueHeisenberg/keel)
for the mechanisms that are not kenward's to own: process isolation, the sealed vault, the
OpenAI-compatible model client, and signed self-update. keel knows nothing about
households; no exported keel signature contains a domain noun. Adding a dependency outside
that set is a decision recorded here, not a `go get`.

Telegram is the transport because it is a globally reachable, outbound-only channel that
every household member already has, and because group chats and direct messages map
naturally onto shared and private spaces — the access model falls out of the transport
rather than needing to be invented on top of it. It is a thin adapter behind an interface;
replacing it does not touch memory or routing.

## Deferred

Sketched so they are not accidentally designed out, and explicitly not part of the first
build.

**Streaming responses.** Telegram's message-edit approach is a refinement, not a
requirement, and it interacts badly with tier failover.

**Cloud-fallback gateway.** A maintainer-operated OpenAI-compatible endpoint used as the
default cloud tier, so a fresh install works without the user obtaining provider keys.
Requires nothing today beyond the existing rule that endpoints come from configuration, and
users can substitute their own keys at any time.

**Hosted deployment.** Because Telegram is already outbound-only and globally reachable,
the message path never needs to traverse the maintainer's infrastructure. A hosted offering
is therefore thin: pairing, updates, encrypted backups, and lore's relay for cross-device
sync. Note that Isolated mode on someone else's infrastructure does not seal anything
against that someone else — root always wins — so the strong privacy claim is a property of
self-hosting, not of the mode alone.

**Per-person device nodes.** Running kenward on each member's own machine would remove its
ability to read private spaces at all, at the cost of private memory only working while that
person's device is awake. It is the architecturally pure answer and the wrong trade for a
household that wants an always-available assistant.

**Inter-agent messaging.** Once there is one agent per member, agents coordinating becomes
meaningful — discovery and transport for it exist in
[agentmesh](https://github.com/BlueHeisenberg/agentmesh), which lore already imports. Its
anonymous, ephemeral identity model is not adopted: identity comes from lore. One rule
governs the whole feature and is a precondition rather than a detail — **an agent answering
another agent may read the shared space only, never its own member's private space**, and
anything beyond that requires per-request confirmation from the member being asked. Without
it, *"ask David's agent when he's free"* quietly becomes an oracle for querying David's
private memory.

## Non-goals

Billing, tenant orchestration, a control plane, SSO, organisations and teams, usage quotas,
cost tracking, and any multi-tenant runtime. All of it is bolt-on precisely because one
household is one deployment, and none of it is designed around.

**A web dashboard and an onboarding wizard used to be on that list, and are not any more.**

The premise underneath the old line was that the operator is the author. Someone who can
edit `kenward.yaml`, read a systemd unit and run `kenward doctor` genuinely does not need a
dashboard, and for that person the line was right. The premise is gone: the operator this
project is now aimed at is a stranger who downloaded an app. They have no terminal open,
and everything kenward asks of an operator — mint a claim code, see who is enrolled, notice
a pod crash-looping, read the privacy statement for the mode they are actually running —
has no interface at all for them.

So kenward gets a web dashboard, admin-only, with the setup wizard as its first screen. It
is the operator's console, not a chat client; members keep Telegram.

Its exposure is decided with it, because this is the first inbound port the design has ever
opened and *"it has no inbound network surface"* — the second paragraph of **What a
household runs** — is a property being given up deliberately rather than by accident.

- **Setup always happens on loopback, before any account exists.** First run binds the
  loopback interface and prints a single-use setup token the operator must paste. Nothing
  else is reachable until an admin account exists.
- **LAN or tailnet exposure is an explicit post-setup choice**, never a default, and never
  something setup itself can be completed over.
- **TLS is required for LAN.** A tailnet carries its own transport encryption; a household
  LAN does not, and an admin console handing a session cookie to anyone on the same Wi-Fi
  is worse than no console.

The cost is the largest single expansion of attack surface in this project: an HTTP server,
sessions, an admin authentication story and a CSRF posture, in a process holding household
keys. The discipline that keeps it honest is that **the dashboard must never be the only
way to do anything.** A server install with no tray and no browser is a complete
deployment, so the CLI keeps parity rather than decaying into an escape hatch — which means
every operator action gets two implementations from here on, permanently.

On a laptop the desktop app is a tray icon that supervises the daemon and opens the
dashboard in the user's own browser. No embedded webview: it would buy a title bar and cost
a browser engine per platform, a second rendering target, its own update problem, and
everything the real browser already provides — the password manager, the extensions, the
zoom and accessibility settings, and a URL that can be bookmarked or opened from a phone on
the tailnet.

**This is a decision recorded ahead of the code, not a description of it.** Three symbols
say how far it has got, so the question is answerable by `ls` rather than by asking: an
`internal/dashboard` package, a `cmd/kenward-desktop` binary, and a listener — anything
calling `net/http.Server.ListenAndServe` or `net.Listen` — reachable from `kenward run`.
When this paragraph was written none of the three existed, and the daemon's only use of
`net/http` was the outbound probe in `cmd/kenward/probes.go`.
