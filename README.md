# kenward

**A household AI assistant where each person's memory is actually their own.**

kenward is a self-hosted assistant for a family or a group of friends. Everyone talks
to it through Telegram — privately, or in a shared group. What it remembers about you
privately stays private; what the group teaches it is shared with the group. That
boundary is separate stores, separate keys and separate processes — not a
`WHERE user_id =` clause you have to take on faith.

It runs on hardware you already own, routing each request to whichever of your machines
is awake, and falling back to a cloud provider only where you allow it.

> **Status: in development, and not yet released.** Everything needed to publish is
> wired: a tag builds six binaries, a container image, `.deb`/`.rpm` packages and a
> signed update manifest. No tag has been cut, so the releases page is still empty and
> there is nothing to install yet. [docs/INSTALL.md](docs/INSTALL.md) describes the
> install the first tag turns on; [docs/RELEASING.md](docs/RELEASING.md) describes cutting
> it.

## The gap it fills

Personal-assistant projects are built for one person on one box. Add a second human and
you get one of two bad outcomes: a single shared brain that leaks everyone's private
context into everyone else's conversations, or separate installs with no shared
knowledge at all. Neither is what a household wants. A family wants the assistant to
know that Tuesday is bin day and that the boiler code is 4471, while *not* telling your
brother what you asked about last night.

Doing that properly means per-person identity, cryptographic separation, invites and
multi-device sync — the boring, expensive half of the problem. That half already exists
in [lore](https://github.com/BlueHeisenberg/lore), so kenward is the assistant loop on
top of it rather than a memory system built from scratch.

## Two kinds of memory

**Private memory** — what the assistant knows about you: preferences, ongoing concerns,
the shape of your week. A lore space with exactly two members: you and the node.

**Shared memory** — what the household knows collectively: house logistics, recurring
plans, decisions everyone should be able to recall. A lore space with every member in
it.

| Conversation | Reads | Writes |
| --- | --- | --- |
| Direct message from a member | their private space, then shared | their private space |
| Household group chat | shared space only | shared space |

Private conversations can read shared memory, because that is useful. Group
conversations can never read private memory, because that is the entire point. Copying
something out of a private space into the shared one is an explicit, reviewed act.

Capture is proposed by the assistant and confirmed by you — with buttons, in the chat.
Nothing is written to memory silently, and a group conversation can never write into
anyone's private space.

## Two modes, one binary

Chosen during setup, by answering one question: *does everyone here trust whoever runs
this machine to be able to read their private conversations?*

| | **Simple** | **Isolated** |
| --- | --- | --- |
| Process model | one process | one pod per member |
| Keys | one address space | per-pod, argon2-wrapped |
| Telegram | one household bot | one bot per member |
| Runs on | Windows, macOS, Linux, container | Linux with Podman or Docker |
| The operator can read your private memory | **yes** | no — not from disk, not from a backup, not outside your session |

The assistant, the memory policy and the routing are identical in both. Only the
supervisor differs.

**Simple mode is honest about being simple**: separation between members is real, but
sealing against the machine's owner is not. Most households are fine with that — it is
their own family. The ones that aren't have Isolated mode.

## Runs on whatever is awake

Endpoints are grouped into tiers; each space declares the tiers it may use, in order.
Requests load-balance within a tier and fall through to the next when a tier has nothing
reachable. A machine that is powered off is skipped in about two seconds rather than
hanging the conversation.

Cloud providers are just another tier — one you opt into per space. A private space
configured local-only will tell you no local machine is available rather than quietly
shipping your private context to a provider. That is the difference between claiming
privacy and having it.

## Built on

- **[lore](https://github.com/BlueHeisenberg/lore)** — the memory layer, reached over
  MCP. kenward owns no knowledge model.
- **[keel](https://github.com/BlueHeisenberg/keel)** — domain-free mechanisms: sandbox
  isolation, sealed vault, model client, self-update.

## Licence

Business Source License 1.1 — see [LICENSE](LICENSE) for the exact terms.

You may run kenward in production for yourself, your household, or within a single
organization. What the licence gates is offering kenward, or a derivative of it, to
third parties as a hosted or managed service — that needs a separate licence, so get in
touch. Each version converts to Apache 2.0 on 2030-08-14.

BSL is source-available rather than OSI open source. That is understood and deliberate.
