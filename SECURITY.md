# Security

kenward holds households' private conversations and the keys to their memory. This
document says what it defends against, what it does not, and how to tell us when it
fails.

## Reporting a vulnerability

**Do not open a public issue.** Contact the maintainer directly — see the GitHub profile
for [BlueHeisenberg](https://github.com/BlueHeisenberg).

Please include what you did, what happened, and what you expected. A proof of concept is
welcome but not required; a clear description of the mechanism is worth more than a
working exploit.

You will get an acknowledgement. If the report is valid you will be told what the fix is
and when it ships, and credited unless you would rather not be. If it is not valid you
will be told why, plainly.

There is no bounty. This is one person's project.

## What is in scope

Anything that breaks one of these:

- **A group conversation cannot read any member's private memory.** Not through
  retrieval, not through capture, not through an entry id, not through an inter-agent
  message.
- **A member's private memory is not readable by another member.**
- **A space configured for local tiers only never reaches a provider.** No fallback, no
  default, no widening under any error condition.
- **Nothing is written to memory without the member being told**, in their own words,
  every time — there is no setting that makes a write silent.
- **Nothing reaches the household's shared memory without an explicit confirmation from
  the member being asked**, and there is no setting that changes that either. Publishing
  to the household is the one irreversible act in the product.
- **A write to a member's own private memory is performed and then announced**, with an
  Undo button, unless the household has set `capture.private_writes: ask`. A tap — an
  approval or an Undo — is only accepted from the member it was addressed to.
- **A stranger who finds the bot gets nothing** — no reply, no acknowledgement, no
  confirmation that the bot exists.
- **In isolated mode**, one member's process cannot read another member's key, token or
  plaintext.
- **An update cannot be applied without a valid signature** from a key the running binary
  already trusts.
- **A secret value never reaches a log line, an error message or a formatted
  configuration.** Names, paths, variable names and file modes are printed freely,
  because that is what makes a fault fixable; values are not, and a resolved secret is
  held behind an accessor rather than in a struct field so that `%+v` has nothing to
  find.

Also in scope: anything that causes kenward to *claim* one of the above while not doing
it. A privacy product that misreports its own posture is worse than one that reports it
accurately and does less.

## What is out of scope, and why

These are known and accepted, not oversights. Each is documented where it matters.

**Root on the host wins.** A model must see plaintext to answer, so a server-side
assistant cannot be end-to-end encrypted with respect to its own server. While a member's
process is running and unlocked, their key is in that process's memory and root can reach
it. What changes between modes is how much work that takes and whether it requires
deliberately attacking your own household.

**Simple mode's operator can read everything.** By design and by documentation. All
members' keys live in one process and one bot token carries every conversation. That is
the mode's stated limitation; it is not a vulnerability. Reports that simple mode does not
seal against the operator will be closed with a pointer to this paragraph.

**Telegram sees message content.** kenward is a Telegram bot. Telegram's servers carry
every message. If that is unacceptable for a household, kenward is the wrong tool, and no
configuration of it changes that.

**Whoever holds a bot token can read that bot's messages.** This is Telegram's model, not
kenward's. It is why isolated mode gives each member their own token.

**A secret supplied as an environment variable is visible to the whole process tree.**
It sits in the environment for the life of the process, is readable through `/proc` by
anything with access to it, and is inherited by every child — including the `lore serve`
sync daemon, which has no business holding a Telegram token. This is why the shipped
systemd unit uses `LoadCredential=` rather than `EnvironmentFile=`, and why
`bot_token_file` and `api_key_file` exist. The compose path stays on environment
variables because Compose has no portable equivalent, scoped as tightly as per-service
`environment:` blocks allow; that gap is documented in `deploy/README.md` rather than
defended.

**A leaked release signing key compromises every installation that updates.** There is no
revocation mechanism, deliberately — a revocation channel is one more thing an attacker
controlling the update host can manipulate. See [docs/RELEASING.md](docs/RELEASING.md)
for the response.

**The key record can be rolled back** by anyone who can write it, re-enabling a
passphrase that was rotated away. A freshness marker written locally rolls back with the
record it protects, so it would be ceremony rather than security. Mitigated by file
permissions; documented rather than defended.

**lore's sync is last-writer-wins.** A machine with a fast clock wins every conflict and
the losing version is discarded silently. Keep household clocks synchronised.

**Denial of service against a household's own node**, by a member of that household, is
not treated as a vulnerability. They can also unplug it.

## Supported versions

Pre-1.0. Only the latest release is supported. There are no backports.

## Cryptography

kenward does not implement its own. Key sealing is
[keel/vault](https://github.com/BlueHeisenberg/keel): Argon2id key derivation,
AES-256-GCM envelopes with a version byte and mandatory additional authenticated data
binding each ciphertext to the record it belongs to. Update manifests are Ed25519-signed
with support for multiple trusted keys so rotation is possible.

keel's own limitations are listed in its README and are worth reading if you are
assessing this: in particular, its AAD defeats ciphertext relocation only if callers pass
the identity they were asked for rather than one read out of the record they fetched.
kenward's session layer does the former, and has a test that constructs the latter and
asserts it fails.
