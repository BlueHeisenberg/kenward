# kenward — architecture

This is the *why* document: what kenward is, what it refuses to be, and the reasoning
behind the decisions that would otherwise look arbitrary in the code.

[IMPLEMENTATION.md](IMPLEMENTATION.md) is the *how* — the package tree, the interfaces, the
exact semantics of routing, capture and enrolment, and the configuration schema. Where this
document explains a choice, that one binds the code to it. Nothing is duplicated between
them on purpose: if you want the type, go there; if you want the argument, stay here.

Anyone cloning this repository should be able to understand the design from this file
alone. Family-level context — how kenward relates to
[lore](https://github.com/BlueHeisenberg/lore),
[keel](https://github.com/BlueHeisenberg/keel) and their shared history — lives in the
design repository and is not needed to read this.

## What a household runs

One household runs one kenward deployment, on whatever machine is always on. A mini PC, a
NAS, an LXC container on a Proxmox host, a desktop that never sleeps. It holds no GPU and
costs effectively nothing to keep running.

Its only inbound network surface is the admin dashboard, and there is none at all unless the
configuration asks for one. Everything else is outbound: Telegram, and inference endpoints
on the LAN or a tailnet.

Household members need no VPN, no port forwarding and no client software beyond Telegram,
which they already have. That stays true for members: the dashboard is the operator's
console and nobody else's, and its exposure rules are stated under **Non-goals** below.

**No multi-tenancy, in any deployment shape.** There is no `tenant_id` threaded through
queries. A household assistant is idle almost all of the time, so the memory cost of running
one deployment per household is trivial next to the cost of a cross-household leak. This is
also the decision that keeps a hosted offering from becoming a rewrite, and it is free to
honour now.

## Two modes, one binary

Not two products. Two topologies of the same binary, chosen once during setup.

| | **Simple** | **Isolated** |
| --- | --- | --- |
| Process model | one process, one goroutine tree per member | one pod per member |
| Keys | one address space, one operator passphrase | per-pod, argon2-wrapped by the member |
| Telegram | one household bot | one bot per member |
| Runs on | Windows, macOS, Linux, container | Linux with Podman or Docker |
| The operator can read private memory | **yes** | no |

The assistant loop, the memory policy, the routing and the capture flow are identical. Only
the supervisor differs. If a change to the per-member unit needs to know which mode it is
running in, that change is wrong.

### Why the split is cheap

This works **only** because the per-member assistant is an isolated unit from the first
commit — no shared mutable state keyed by member id, anywhere.

That property cannot be retrofitted: one map keyed by member in a shared address space turns
the mode split into a rewrite. It is therefore a build rule rather than an aspiration, and it
is enforced in [IMPLEMENTATION.md](IMPLEMENTATION.md) §0.

### The setup question is about trust, not topology

A non-technical installer can answer *"does everyone here trust whoever runs this machine to
be able to read their private conversations?"* They cannot answer *"do you want per-user
pods?"* The question decides the mode; the topology is a consequence.

### Simple mode is honest about being simple

Separation between members is real in both modes. Sealing against the operator exists only in
isolated mode. Simple mode must never be described with the sealed-memory language, in this
repository or anywhere else. Most households are fine with that, because the operator is a
member of their own family; the ones that are not have isolated mode.

Moving a household from simple to isolated is not built, and there is no path for it today.
It has to stay possible — generate per-member keys, re-wrap, split the process, hand out bot
tokens — which is only true if simple mode never writes anything assuming a single key.

## Memory: lore, unchanged

kenward owns no knowledge model. It imports `github.com/BlueHeisenberg/lore`, opens one store
per lore home in its own process, and treats lore's spaces, entries, markers, confidence and
origin fields as given.

The hard half of a household assistant is per-person identity, cryptographic separation,
invites and multi-device sync. That half already exists in lore, so kenward is the assistant
loop on top of it rather than a memory system built from scratch. The seam is also what kept
a change of implementation language from touching memory at all.

Three adaptations, all configuration rather than changes to lore.

### Private memory is a two-member space, not lore's `personal` space

lore's `personal` space is bound to one account and never crosses accounts, so a node could
never read it — which makes it useless for an assistant that has to answer from memory. A
`shared`-kind space whose members are exactly the person and the node keeps the cryptography
honest and requires no change to lore. This was the design's original reasoning and is now
confirmed, by reading lore's source, as the only workable option.

### Spaces are addressed explicitly, always

lore's project spaces resolve from the working directory and git remote, which is
coding-assistant shaped and meaningless in a house. Every call names its space. A search with
no explicit space set is an error, never "all" — that is the mechanical guarantee that no code
path can accidentally search everything.

### Retrieval is a union, writes are not

The asymmetry is the product.

| Conversation | Reads | Writes |
| --- | --- | --- |
| Direct message with a member's own assistant | their private space, then shared | their private space |
| Household group chat | shared space only | shared space |
| Direct message with kenward itself | shared space only | shared space |

The third row is D-054's and exists only under `household.agents: per_member`; it is
described under **Personas** below.

A member's own conversation reads shared memory because that is useful — you should be able to
ask when the bins go out. Nothing but a member's own assistant ever reads their private space,
because that is the entire point.

Copying something out of a private space into the shared one is an explicit, reviewed act
using lore's own copy, so that its provenance survives the move and the member sees the full
text before it is published.

Multi-space results come back grouped in the order the spaces were asked for, not globally
re-ranked. Ranking across spaces is a policy decision that belongs to the assistant rather
than to the memory client — and it is genuinely unsolved: "private first" is an ordering, not
a ranking strategy.

### What depending on lore costs

Established by reading lore's source rather than assuming, and listed here because several of
these constrain the design.

**What the API gives:**

- **lore's public Go API is the interface**, and it is a compatibility promise: typed entries,
  typed errors, `context.Context` on everything that touches the database. Everything under
  lore's `internal/` is explicitly not promised — sync, signing, membership, the schema — and
  kenward reaches none of it.
- **Search returns whole entries**, body included, with a snippet alongside and with origin,
  timestamps and version. This was not true of `lore mcp`, which rendered the snippet and
  discarded the entry; the excerpt doctrine that ran through `internal/memory` and the prompt
  existed only for that reason, and both are gone. See PROMPT.md.
- **Space creation and init are on the API** (`lore.Init`, `Store.CreateSpace`), and so is
  creating a space *at a chosen id* (`Store.CreateSpaceWithID`, idempotent). A pod therefore
  creates its own store AND the spaces `kenward.yaml` names for it, at those ids.
- **Granting membership is on the API too**, as of lore v0.7.0, but only in a shape a caller
  that holds both homes can use: `Store.GrantMembership` on the owner's,
  `Store.AcceptMembership` on the grantee's, and no composition of the two that joins an
  arbitrary space to an arbitrary account. Deciding to share one person's memory with another
  is still a person's; kenward's `internal/link` carries out the decision an administrator
  already wrote into `kenward.yaml`, and `lore space invite` / `lore join` remain for the
  households that configure no link key.
- `confidence` and `origin` are enforced enums, but **markers are free-form** — the familiar
  vocabulary is convention only and must not be validated against.

**What the storage layer does not give:**

- **Sync is last-writer-wins per entry, not a CRDT.** The losing version is discarded
  silently, with no conflict record, and a machine with a fast clock wins every conflict.
  Household clocks should be synced, and nothing in kenward may assume a write it made is
  still there.
- **Delete is a signed tombstone, by id, and space-scoped.** It stops an entry coming back
  from search and get, here and on every synced device; deleting an already-deleted entry is a
  no-op. It is not a shred, and nothing kenward says to a member may promise one.
- **A write's outcome is always known.** The store commits and returns, or returns an error
  having written nothing. There is no third case, and kenward no longer carries an
  `ErrWriteUncertain` for one — that existed because a lost MCP response left an entry that
  might exist under an id kenward never received.
- **Opening a store does not sync it.** That needs the sync daemon running, which kenward
  starts in its own process on the store it opened (`lore.(*Store).Serve`), and it pokes it
  after each write (`lore.Options.NotifyOnWrite`) so the write leaves the machine now rather
  than at the daemon's next poll. Any deployment running more than one lore instance must run
  both. Two stores converge only where a daemon is running *and* a space is shared.
- **Instances isolate by `LORE_HOME`, not by machine.** Several lore daemons can run on one
  host, each holding a subset of spaces. This is what makes one lore per member pod viable,
  and it was the most important open question in the whole design.

None of this justifies forking lore. Forking trades a list of known limits for an unbounded
amount of new work, and every one of these limits is survivable for a household.

### Built: the household's shared space in isolated mode

This section used to be an open limitation: per-pod lore instances did not converge on the
household's shared space, and nothing in either deployment path made them. **D-044 closed
it**, and `internal/memory.(*Client).Serve` (`internal/memory/sync.go`) is the symbol that
says so.

Every isolated unit runs lore's own sync daemon — `lore.(*Store).Serve`, in this process, on
the very store the unit reads and writes through — and `cmd/kenward/run.go`'s
`startSyncDaemon` starts exactly one per pod. It lives in the binary the image already runs,
so both deployment paths get it with no compose or service change.

**The defect it closed** is worth keeping, because it explains the shape of the fix. One
`LORE_HOME` per pod is one lore *account* per pod, and `lore init` gives each account its own
set of space ids, so the id `household.shared_space` named resolved inside whichever store
created it and nowhere else. Every other pod's `doctor` printed `space "…" is not a space this
lore store holds` while nothing acted on it, and a conversation in that scope read nothing —
silently, because a turn that cannot read a space degrades that space rather than failing. The
cause was that nothing was running the sync daemon at all.

**Membership is no longer an operator step.** Carrying an entry between homes needs the daemon
*and* a shared space, and the second half used to be `lore space invite` on the owning store
and `lore join` on the others, typed inside two containers, once per member. `internal/link`
does it: the group's pod answers, each member's pod asks, both prove they hold
`household.link_key`, and nobody runs anything.

What made that legitimate is not a change of position on whose decision sharing memory is — it
is still a person's — but the observation that the person had already made it when they added
the member to `kenward.yaml`. A household that configures no link key keeps the manual recipe
and keeps working. `doctor` reports what it can see — whether this store holds the space,
whether a daemon is running, when it last synced, how many instances it reached — and says
which of the two states a missing space is in rather than failing the report.

**The isolation guarantee is lore's, and structural** rather than something kenward configures
correctly. A sync exchange opens with a blinded space-id intersection over `HMAC(space_key,
…)`, so two stores exchange a space only when both already hold its id and its key, and only
the invite handshake hands a key over. A member's private space is generated inside that
member's pod; a sibling cannot compute its blinded id, cannot name it, and is refused if it
asks. Verified on real Podman with a three-pod household on the real image: an entry written in
one pod read from the other two, every private entry invisible from any sibling.

### Built: lore is an import

D-036 replaced the MCP seam with lore's public Go package, imported at
`github.com/BlueHeisenberg/lore`. `cmd/lore` keeps `lore mcp` for every other MCP client and
`lore serve` for sync; kenward's text-parsing layer was deleted rather than ported.

The seam bought two real things — a language rewrite that cost the assistant and nothing else,
and a lore forced to have an external surface — and neither was still being paid for. What it
cost was countable and is now gone: `internal/memory/parse.go` was 500 lines mirroring lore's
format strings, its error classifier 253 lines matching error prose, and `testdata/` held 31
captured fixtures to keep them honest. Seven defects in one session came out of that layer.

`memory.Memory` did not move. It was written as the seam and it held: roughly thirty call sites
across `assistant`, `capture`, `setup`, `supervisor` and `cmd/kenward` are untouched by the
change.

**Nothing shells out any more.** `lore serve` was the last of it, and lore v0.5.0's
`(*Store).Serve` runs the same daemon in kenward's process on the store it already has open, so
**no mode of kenward needs a `lore` binary**. The command survives in the wider story as a
fallback: `lore space invite` and `lore join` still grant membership of a shared space by hand,
for a household that configures no `household.link_key`. The automatic route is a Go call like
everything else (`internal/link`).

**The never-fork rule survives unchanged.** kenward imports lore, defines no knowledge model,
reimplements nothing.

## Identity and enrolment

A member is a stable internal id, not a Telegram id. The Telegram id is bound to it at
enrolment and can be rebound; deriving identity from the transport would mean a member who
changes accounts becomes a different person to their own memory.

**Resolving a message to a scope is the authorization decision.** Who this is, what this
conversation may read, what it may write, which tiers it may use — that resolution *is* the
decision, and everything downstream obeys it and re-derives nothing. There is one place to
audit and one place to test, and it is tested exhaustively: unknown users, unknown chats, a
member messaging from an unexpected chat, and a group id colliding with a private space.

Authorization is deliberately not derived from group membership. Being added to the household
group chat by someone else grants nothing.

**Enrolment needs a secret**, because Telegram bot usernames are publicly discoverable and
anyone may send `/start`. Dynamic enrolment without one silently rewrites the model from
*anyone not enrolled is ignored* into *anyone who found the bot is enrolled*. So: claim codes,
minted by the operator, single-use, expiring, stored hashed, rate-limited and compared in
constant time.

An unknown sender receives **nothing at all** — not an error, not a prompt, not "you are not
authorised". A reply of any kind confirms to a stranger that the bot exists and is someone's
household assistant.

Removal unbinds the Telegram id and reports that the space key must be rotated in lore. kenward
cannot rotate it, and says so rather than implying the removal was complete.

## Inference routing

Most people with a local-inference habit have a scatter of machines: a GPU workstation, a
laptop, a mini PC, none of them on all the time. kenward treats them as a pool.

Endpoints carry tier tags. Each space declares an ordered chain of tiers it may use. Routing
walks the chain, load-balancing within a tier and falling through when a tier has nothing
reachable, and when the chain is exhausted it **refuses**.

There is deliberately no code path that widens a chain. No default, no fallback, no "if nothing
matched, try everything". This is tested directly: a pool containing a cloud endpoint, asked for
a local-only chain with no local endpoint reachable, must leave the cloud endpoint with zero
requests.

Two practical facts shape the mechanism:

- **A powered-off machine sends no TCP reset**, so a request to it hangs on the operating
  system's connect timeout instead of falling through. A short connect probe with a few seconds
  of result caching turns that into a two-second skip.
- **Failover happens before the first token only.** Once a response has begun streaming there
  is no clean retry, and pretending otherwise produces spliced output.

**Two axes of policy, and no more:** capability tier (a stronger tier and a cheaper one) and
location tier (local versus cloud). Resisting a third axis is a design constraint rather than an
oversight — per-user, per-task policy matrices are what make other self-hosted products
unusable. The one thing that could force a third axis is task difficulty, and that is an open
question rather than a plan (see **Tool surface**).

Model identity should be consistent within a tier. Mixing materially different models under one
tag makes answer quality vary between turns for reasons the user cannot see.

### Per-space routing policy is the privacy mechanism

A space allowed only local tiers reports that no local machine is reachable rather than falling
back to a provider. That is the difference between claiming privacy and having it, and it is a
configuration line rather than a subsystem: *private conversations never leave my hardware, and
here is the config that proves it.*

It also gives the availability story a purpose beyond convenience. When a private space has no
local backend awake, the correct behaviour is to say so — naming the tiers allowed and the
endpoints that could not answer — not to silently ship someone's private context to a third
party. The wording is careful about that second list: it holds endpoints that were attempted
alongside endpoints skipped for a cooldown or a failed probe, so the refusal says they were
unavailable rather than claiming an attempt that never happened.

The same principle runs past refusals. A rate-limited provider, a rejected key, a model that
declined the turn, a locked key — each produces a short notice saying what happened and what the
member can do about it, because silence is the one reply that teaches a household its assistant
is unpredictable. Every one of those strings is a product surface, and they are golden-tested
for exactly that reason.

## Key custody, and what the privacy claim actually is

A model must see plaintext to answer. A server-side assistant therefore cannot be end-to-end
encrypted with respect to its own server, and no arrangement of keys changes that. lore's relay
sidesteps the problem by being a dumb pipe that never reads what it carries; anything that has
to answer the words cannot.

What is left is reducing exposure honestly:

| Exposure | Answer | Residual |
| --- | --- | --- |
| At rest | Per-member key, argon2-derived, unwrapped into memory only once that member's process has been unlocked, never written to disk | Nothing meaningful — backups and stolen disks yield nothing |
| In flight | Per-member pod and per-member bot token, in isolated mode | Host root |
| Inference | Local endpoints the household controls | Whoever owns the GPU box; watch provider prompt logging |

### Per-member bot tokens exist because encryption at rest does nothing for the channel

Whoever holds a bot's token can read every message sent to it. Under a single household bot,
the operator can read every private conversation in flight no matter how well the memory is
sealed on disk. Each member creating their own bot, running in their own pod, closes that at
the application layer, and costs about five minutes of setup per person.

In simple mode there is no isolation to protect, so one household bot is both the right answer
and the honest one.

### The claim, word for word

**It is not "the operator cannot read your memory."** It is `internal/privacy`'s wording,
because that package is where the promise is made to the member and a paraphrase here is how
the two drift a word at a time: *nobody else in the household can read your private memory, and
neither can the person who runs this machine — not from the disk, not from a backup, and not
before your process has been unlocked.* Doing so means deliberately attacking their own family.
That is strong, checkable and true.

**D-019 retired the stronger version, which this document used to make.** Idle-locking is only
meaningful if re-unlocking needs the passphrase again; the only channel a member has is
Telegram; and typing a passphrase into a chat hands it to Telegram's servers, to the member's
own message history, and in simple mode to whoever holds the bot token. Unlock-on-message was
the usable and weaker option, and it is refused.

So a passphrase reaches a process at its start — environment variable, interactive prompt, or a
systemd credential — and the consequence is stated rather than engineered around: once a
member's assistant is unlocked, their key stays in that process's memory until it stops or they
lock it. The last clause of the claim is therefore about being unlocked, not about being
present, and the withdrawn wording — *away* rather than *unlocked* — must not come back.

What is left open is only the knob. `session.idle_timeout` exists, is off by default, and a
household that turns it on buys an assistant that stops answering until somebody walks to the
machine; the right number is a guess until there is real usage.

### Three limits, stated plainly

Because they are the first things a privacy-minded reader will check.

- **The node can read private spaces at all.** That is what being the second member of a
  two-member space means, and it is the price of an assistant that answers when your laptop is
  closed.
- **Root always wins.** An attacker with root on the host during an active session reads the
  unwrapped key out of memory. Isolated mode raises the cost; it does not create an
  impossibility.
- **In simple mode the operator can read everything** — every member's private memory, at rest
  and in flight. That is the mode's known limitation rather than a defect, and `kenward doctor`
  says so out loud.

## Capture

The rule used to be "no automatic memory writing", which was correct and unusable — a member
has to notice something is worth remembering and then say so, which nobody does. The first
refinement made the model *propose* and the member *decide*, with buttons, on every write.

**That was still one question too many, and D-038 splits the two directions apart.** A throttle
limits how often the question is asked; it does not change what the question does to somebody's
attention. A confirmation on every private write makes the assistant's memory feel like
paperwork, and the failure that follows is not a bad write — it is a household that stops
letting it remember anything.

- **A private write is performed and announced, with an Undo on the announcement.** The member
  sees the exact words, after the fact, and one tap removes them.
- **A shared write is still approved before it happens, and that is not configurable.**
- **Announcing a write is not configurable.** A memory that fills silently is a different
  product from this one.
- **Announcing a read is configurable, default on.** It is information rather than a control.

**Where the code is:** D-038 is built. The symbols that say it landed are
`capture.OutcomeUndone` beside `OutcomeSaved` and `OutcomeDeclined`, and the announce-and-Undo
path `(*capture.Engine).writeAndAnnounce` with its `capture.ChoiceUndo` button, all in
`internal/capture/capture.go`.

### What Undo is allowed to promise

Undo needed a delete lore did not have. It became a lore change: a space-scoped delete writes a
signed tombstone that propagates, and `internal/memory.Delete` reaches it.

A tombstone is not the same promise as a removal, so the product says which — *"it won't come
back in an answer, not here and not on any other device"* rather than *"erased"* — on the
onboarding card that introduces the Undo button, which is where a member learns the memory
model. The undo itself only strikes the entry through and says *"Not saved to your private
memory"*: the member tapped the button a second ago and can see what they took back, and a
paragraph restating the sync model on every tap is read once and skipped after. Nothing
anywhere is allowed to say *erased*.

Three endings, three sentences, because the entry is in a different state in each: gone, still
there because lore refused, or unknown because lore never answered. Reporting either of the
last two as "undone" would be the plainest lie the product could tell — the member asked for
something to stop existing and would be told it had. An undo also counts as a decline, or the
next turn's proposal is written straight back.

### The claim that was retired

This document previously claimed that *nothing is written to memory without the member seeing
the exact words first and saying yes.* That claim is retired rather than quietly softened,
because a claim that weakens without saying so is worse than one never made.

What replaces it is narrower and still true: nothing is written without the member being shown
the exact words, and nothing leaves their private space without their approval first.

### Two invariants, not preferences

1. **A conversation may offer "personal" exactly when it already has one** —
   `domain.Scope.AllowsPrivateCapture`, a property of the kind. Otherwise a shared conversation
   becomes a write path into a private space, which is the one thing the memory model exists to
   prevent. It is stated positively rather than as "never the group", for the reason given
   under **Personas**.
2. **Private → shared is a separate, louder act**, showing the full text before publishing,
   because publication is irreversible from the household's point of view: other people have
   read it by the time anyone regrets it, and deleting the copy afterwards does not unread it.
   That is why the shared path has no policy switch while the private one does.

### Throttling and who may answer

One proposal per turn, and none for anything already retrievable. Otherwise everyone learns to
reflexively decline, and the feature is worse than not having it. A proposal that was declined
recently is suppressed rather than repeated.

Answers — approvals and Undos alike — are accepted only from the member being asked. In a group
chat, everyone can see and tap an inline keyboard, and without that filter another member could
route someone else's memory. A timeout on an approval is treated as declined, never as accepted.

## Personas, and one assistant or one each

`household.agents` is either `shared` — there is kenward and nothing else — or `per_member`,
where every member gets their own named agent for their private chat and kenward remains the
family agent in the group. `shared` is the default and is what kenward has always done.

### It is not `mode`, and must never be described as though it were

Mode is a security question, answered by topology: can whoever runs this machine read a member's
private plaintext. This is a presentation question: who the household is talking to.

Under `per_member` a member has their own bot so their agent is its own *contact* — that is a
separate contact, not a separate secret, and the mode is what seals memory. Conflating the two
would sell isolation as a naming scheme. The wizard asks them as two questions, in that order,
and a test fails the build if the identity step's transcript so much as mentions containers or
sealing.

The two meet in exactly one place, and it is arithmetic rather than privacy. An agent is a
Telegram contact; simple mode runs one bot for the whole household; two agents behind one
contact are one agent. So `per_member` requires `mode: isolated`, and it is **refused rather
than downgraded** — the downgrade would resolve every member's private chat to kenward's and
quietly take away the assistant they were promised.

### A third scope came with it

Under `per_member`, kenward is reachable in a private chat too, on the household bot. That
conversation reads and writes the shared space and nothing else, exactly as the group does, and
it exists for the two things a group chat makes impossible: adding to the household's memory
without notifying everybody, and asking what the household knows without asking in front of
everybody.

It carries a member — kenward has to know who is asking in order to authorise them — and
knowing that is a different thing from being allowed near their private memory. This is why the
capture boundary is stated as `Kind == ScopeDirect` and not as "not the group": the negative
form was true while there were two kinds, and would have silently admitted the third.

### A persona is four fields

`agent_name`, `language`, `tone`, `character` — and every empty value reproduces the behaviour
kenward had before the section existed, so a household that deletes the block loses nothing.

`household.persona` is kenward's own. `members[].persona` is the member's, written by the member
in the Telegram tutorial rather than by an operator on their behalf; it falls back to the
household's **per field**, so somebody who wrote a character and said nothing about language
keeps the household's language.

kenward's own name is not configurable. Stating `household.persona.agent_name` is a validation
error rather than a rename, because it is the name this documentation, the logs and
`kenward doctor` all use.

### Under `shared` the voice is the household's and the language is the member's

Three of the four fields have no personal layer: name, tone and character are what "one
assistant" means, and an assistant that is a flat clerk to one member and a wry ship's captain
to another is two assistants wearing one name.

Language is not one of the three, because language is a property of a conversation rather than
of an assistant — the same person answers in whichever language they are addressed in, and the
prompt already works that way (an empty `language` leaves the model mirroring whatever the
member writes).

What a shared persona actually discarded was never the model's language. It was
`capture.Options.Language`, which decides the button labels, the write announcements, the undo
hints, the alias line folded into a stored body and whether the English-gloss line renders at
all. A Spanish member of a default household got model prose in Spanish wrapped in English
buttons, with the gloss and the aliases suppressed outright — and the tutorial had asked them
their language first.

Structurally the fix is free: a unit is built per member in every mode, each with its own prompt
and its own capture engine. Contrast `agent_name`, which is not free — an agent is a contact,
and simple mode has one bot.

**The tutorial asks only what the household will read back.** Under `shared` that is the
language question and nothing else; under `per_member` it is all four. A question whose answer
is resolved away is a question with no consequence, and recording it into `state.json` makes the
file look like configuration that does something.

### Persona length is a trust boundary, not taste

The limits are 80 runes a line and 1000 for the character. The budget loop trims retrieved
entries and never trims the persona, so an unbounded character could push the scope disclosure
and the capture rules out of a small endpoint's context window — the one way persona text could
countermand them without the model ever being asked to.

The prompt renders the persona inside a delimited block, indented so no line of it reaches
column zero, followed by a guard stating that it governs wording alone. Nothing inside a persona
can close the block or forge a heading; a character quoting `</persona>` renders as an indented
line saying `</persona>`, which is what it is.

## The group is heard, and answered only when addressed

Telegram turns bot privacy mode on for every new bot, and with it on the bot receives nothing in
a group: not plain messages, not a reply, not an @mention. Nothing arrives, so nothing is
logged, and a household adds the bot to their family group and it ignores everyone with no
symptom anybody can search for. kenward therefore **requires privacy mode disabled**, which
setup instructs, checks against Telegram in a loop, and `kenward doctor` checks again.

That turns the problem inside out: every sentence a family says to each other now arrives here.
So kenward rebuilds, deliberately, the gate Telegram used to provide for free. The set is
Telegram's own privacy-mode set restated where kenward can see it — an @mention of this bot, a
reply to one of its own messages, a bot command not aimed at some other bot by name, or a text
mention carrying its user record. **A bare name prefix is not a mention**: "kenward, what time
is dinner" carries no Telegram entity and is not addressed.

Two properties are worth stating because each was a decision.

**"Addressed" is a fact about the message, not a policy.** The transport computes it on every
message, private chats included, and the policy is one line in the assistant: overheard means
group scope and not addressed. Keying it on scope kind rather than on "is this a group" is what
keeps a member's private chat with kenward out of it.

**An overheard message is added to the history ring and nothing else runs.** No retrieval, no
router, no capture, not even the turn slot — the conversation is context for the question that
comes later.

Capture not running is the part worth defending, because the argument the other way is a good
one: ambient household knowledge is exactly what a shared memory is for. It loses anyway,
because capture asks a question and a question is a reply, and an assistant that interjects into
a family conversation to ask whether it should remember something is one the household mutes.
Ambient capture is a feature to design deliberately, with its own consent shape; it is not a
side effect of listening.

The gate fails open when the transport does not know its own username — every message counts as
addressed. Answering too much is wrong in a way a household can see and report; gating the whole
household into silence is not.

## Reminders: the one unprompted message

Every other message kenward sends is a reply. A reminder is not, and that is the whole of its
proactive capability: a piece of text and a time to send it. When the time comes the text is
sent verbatim.

**A reminder never reaches a model.** Nothing is generated, no memory is searched, no provider
is contacted — and this is structural rather than a rule somebody follows. The package is given
no router and no memory client, so it cannot reach a provider because it has nothing to reach
one with, and that is checkable by reading its imports.

A scheduled job that invokes the model is a job that spends tokens all night with nobody asking,
against whatever tier its chain reaches; the privacy claim about tier chains would have to be
qualified, and it is not.

The rest is bounds, because an unprompted message is the one thing a household cannot refuse
turn by turn.

**The cap.** `reminders.max_per_day` caps deliveries per conversation. Reaching it is
deliberately **not** announced, since saying so would itself be an unprompted message sent to
report that too many unprompted messages had been sent; it goes to the log and to
`kenward doctor`, where the person who can change the number is looking. A negative cap turns
delivery off entirely while still allowing reminders to be set and listed, so a household can go
quiet without every member cancelling their own.

**Missed occurrences** get different answers by kind, because household machines are
legitimately asleep. A **repeating** reminder delivers at most one missed occurrence, the most
recent, and only if it is younger than `catch_up_window`: a node off overnight owes the
household one bin reminder, not sixteen. A **one-off** is delivered however late, and the window
does not apply — there is exactly one of it ever, so it cannot flood anything, and it is a
promise to a person. A late delivery says when it was due.

**Storage** is one file per unit, never one file keyed by member, and the daily ledger is
persisted with it so a crash-looping unit cannot reset its own allowance. A reminder is counted
and advanced before it is handed off to be sent, so a crash loses a message rather than
repeating one.

## The node speaks ten languages; the prompt speaks one

`persona.language` is free text, passed to the model rather than looked up in a table, because
that is what makes "answer in Valencian" work at all.

It reaches a second place under a different rule. The strings kenward itself writes — the
retrieval line, the locked notice, the refusals, the capture buttons, the enrolment tutorial —
come from a closed catalogue, `internal/lang`, in ten languages: English, Spanish, Catalan,
Portuguese, French, Italian, Dutch, German, Chinese and Arabic. Outside that list the model
still answers in that language and kenward's own machinery stays English, which is a stated
limit rather than a surprise.

The line between the two is where the words come from. Anything a member reads that kenward
composed is translated. The system prompt, the tool descriptions and the JSON-schema
descriptions are not: they are addressed to the model, they are golden-checked against PROMPT.md,
and translating them would multiply the surface that has to stay in step with the code by ten
while changing nothing anybody reads.

Glyphs and markup stay outside the catalogue — they are structural, and the caller prepends them
— so a translator cannot lose a `🔍` or unbalance an `<i>`. Missing fields fall back to English
**per field**, and a test asserts the fallback has nothing to do, so an incomplete language is a
build failure rather than a household reading half a sentence in the wrong tongue. Matching
accepts codes, endonyms and exonyms ("español", "castellano", "中文", "pt-BR"), and anything
unrecognised resolves to English.

## Tool surface

Deliberately thin, plus MCP passthrough so households bring their own tools. **The passthrough
is not built**: there is no MCP client in the module today, and the tool set is the native one —
`remember`, `publish`, `remind`, `unremind`.

Household assistants are only useful with real-world access — calendar, shopping, documents,
home automation — and building those integrations is unbounded work that has drowned other
projects in this category. kenward ships a small native tool set and otherwise acts as an MCP
client, which makes integrations the ecosystem's problem rather than the maintainer's.

What gets built natively is decided by watching a household use it for a month, not by guessing.
This is the phase where scope becomes unbounded if it is allowed to.

**The known risk sits in the models, not in the design.** Models in the 27–30B class remain
unreliable at multi-turn tool calling, and any assistant sold on privacy loses trust quickly if
its tool calls misfire. If that unreliability holds when measured, routing needs a difficulty
axis — a third axis, which the two-axis constraint above forbids. This is the only open question
that can still force an architecture change, and it is deliberately measured before anything is
built that would have to absorb it.

## Language and stack

Go 1.25, single static binary, no cgo. Cross-platform everywhere except the isolated supervisor,
which is Linux-only and degrades with a clear error rather than a panic.

The one qualification is the optional desktop wrapper, `cmd/kenward-desktop`, which is a second
binary rather than a mode of the first precisely so that this claim survives: a menu bar needs
cgo on macOS, and the distroless image has no libc to link against. See
[DESKTOP.md](DESKTOP.md). The daemon is unchanged and headless remains first-class — the wrapper
starts `kenward run` as a child process and never imports its internals.

**Go rather than Python** because the two packages worth inheriting are Go; because lore, the
thing being talked to, is Go; because per-member processes cost roughly 25 MB each instead of
150 MB, which matters once there is one per household member; and because a single static binary
is a materially better self-hosted install story than a container-only one. Go 1.25 is the
floor: it was the MCP SDK's requirement when it was set, and lore's own module keeps it there
now that the SDK is gone.

**Three dependencies do real work** — `go-telegram/bot` for transport,
[lore](https://github.com/BlueHeisenberg/lore) itself for memory, `yaml.v3` for configuration —
plus [keel](https://github.com/BlueHeisenberg/keel) for the mechanisms that are not kenward's to
own: process isolation, the sealed vault, the OpenAI-compatible model client, and signed
self-update. keel knows nothing about households; no exported keel signature contains a domain
noun. Adding a dependency outside that set is a decision recorded here, not a `go get`.

A fourth, `fyne.io/systray`, was added for the desktop wrapper and reaches nothing else. It was
chosen over `getlantern/systray`, which is unmaintained since 2024 and needs GTK3 and
libayatana-appindicator headers on Linux, and over Wails and Fyne, which ship a whole toolkit
for a menu. It is Apache-2.0, so it imposes nothing on kenward's BSL 1.1; its Linux backend is a
pure-Go StatusNotifierItem implementation over D-Bus, and its Windows backend is pure-Go
syscalls, so cgo is required on macOS alone.

**Telegram is the transport** because it is a globally reachable, outbound-only channel that
every household member already has, and because group chats and direct messages map naturally
onto shared and private spaces — the access model falls out of the transport rather than needing
to be invented on top of it. It is a thin adapter behind an interface; replacing it does not
touch memory or routing.

## Deferred

Sketched so they are not accidentally designed out, and explicitly not part of the first build.

**Streaming responses.** Telegram's message-edit approach is a refinement, not a requirement,
and it interacts badly with tier failover.

**Cloud-fallback gateway.** A maintainer-operated OpenAI-compatible endpoint used as the default
cloud tier, so a fresh install works without the user obtaining provider keys. Requires nothing
today beyond the existing rule that endpoints come from configuration, and users can substitute
their own keys at any time.

**Hosted deployment.** Because Telegram is already outbound-only and globally reachable, the
message path never needs to traverse the maintainer's infrastructure. A hosted offering is
therefore thin: pairing, updates, encrypted backups, and lore's relay for cross-device sync.
Note that isolated mode on someone else's infrastructure does not seal anything against that
someone else — root always wins — so the strong privacy claim is a property of self-hosting, not
of the mode alone.

**Per-person device nodes.** Running kenward on each member's own machine would remove its
ability to read private spaces at all, at the cost of private memory only working while that
person's device is awake. It is the architecturally pure answer and the wrong trade for a
household that wants an always-available assistant.

**Inter-agent messaging.** Once there is one agent per member, agents coordinating becomes
meaningful — discovery and transport for it exist in
[agentmesh](https://github.com/BlueHeisenberg/agentmesh), which lore already imports. Its
anonymous, ephemeral identity model is not adopted: identity comes from lore.

One rule governs the whole feature and is a precondition rather than a detail — **an agent
answering another agent may read the shared space only, never its own member's private space**,
and anything beyond that requires per-request confirmation from the member being asked. Without
it, *"ask David's agent when he's free"* quietly becomes an oracle for querying David's private
memory.

## Non-goals

Billing, tenant orchestration, a control plane, SSO, organisations and teams, usage quotas, cost
tracking, and any multi-tenant runtime. All of it is bolt-on precisely because one household is
one deployment, and none of it is designed around.

### The dashboard used to be on that list

A web dashboard and an onboarding wizard were non-goals, and are not any more.

The premise underneath the old line was that the operator is the author. Someone who can edit
`kenward.yaml`, read a systemd unit and run `kenward doctor` genuinely does not need a
dashboard, and for that person the line was right. The premise is gone: the operator this
project is now aimed at is a stranger who downloaded an app. They have no terminal open, and
everything kenward asks of an operator — mint a claim code, see who is enrolled, notice a pod
crash-looping, read the privacy statement for the mode they are actually running — has no
interface at all for them.

So kenward gets a web dashboard, admin-only, with the setup wizard as its first screen. It is
the operator's console, not a chat client; members keep Telegram.

**Its exposure is decided with it**, because this is the first inbound port the design has ever
opened and *"it has no inbound network surface"* — the second paragraph of **What a household
runs** — is a property being given up deliberately rather than by accident.

- **Setup always happens on loopback, before any account exists.** First run binds the loopback
  interface and prints a single-use setup token the operator must paste. Nothing else is
  reachable until an admin account exists.
- **LAN or tailnet exposure is an explicit post-setup choice**, never a default, and never
  something setup itself can be completed over.
- **TLS is required for LAN.** A tailnet carries its own transport encryption; a household LAN
  does not, and an admin console handing a session cookie to anyone on the same Wi-Fi is worse
  than no console.

The cost is the largest single expansion of attack surface in this project: an HTTP server,
sessions, an admin authentication story and a CSRF posture, in a process holding household keys.
The discipline that keeps it honest is that **the dashboard must never be the only way to do
anything.** A server install with no tray and no browser is a complete deployment, so the CLI
keeps parity rather than decaying into an escape hatch — which means every operator action gets
two implementations from here on, permanently.

On a laptop the desktop app is a tray icon that supervises the daemon and opens the dashboard in
the user's own browser. No embedded webview: it would buy a title bar and cost a browser engine
per platform, a second rendering target, its own update problem, and everything the real browser
already provides — the password manager, the extensions, the zoom and accessibility settings,
and a URL that can be bookmarked or opened from a phone on the tailnet.

**This was recorded ahead of the code, and the code has caught up.** Three symbols said how far
it had got, so the question stayed answerable by `ls` rather than by asking, and all three now
exist: the `internal/dashboard` package with `dashboard.New` and `(*Server).Listen`, the
`cmd/kenward-desktop` binary, and the listener reachable from `kenward run` through
`startDashboard` in `cmd/kenward/run.go`. What has *not* happened is a release: none of it has
ever been installed by anyone but the maintainer.
