# Command surface

kenward is one binary. The command surface is small on purpose: a household assistant
that needs a manual is one that does not get installed.

Everything here is specified before implementation so `cmd/kenward` and
`internal/setup` agree. Output shown is illustrative of register and content, not
byte-exact except where marked golden.

---

## `kenward setup`

The first-run wizard. Interactive, writes `kenward.yaml`, and is the only place the
mode is chosen.

It asks, in order:

1. **The trust question.** Not "which mode do you want" — a non-technical installer can
   answer the first and cannot answer the second.

   ```
   Does everyone in this household trust whoever runs this machine to be able to
   read their private conversations?

     [1] Yes — it is our own family machine.      (Simple)
     [2] No, or I would rather it were sealed.    (Isolated — needs Linux with
                                                   Podman or Docker)
   ```

   Choosing Isolated on Windows or macOS explains why it is Linux-only, offers Simple,
   and does not pretend otherwise.

2. **Household name, and which lore space is the shared one.** The wizard lists the
   spaces lore actually holds and writes the **id** of the one chosen. It does not ask
   what to call the shared memory: an earlier draft did, slugified the answer, and so
   produced a configuration that wrote memories happily and returned nothing on the first
   read. Spaces are not created here or anywhere else in kenward — they exist already, or
   the wizard has nothing to list.
3. **The Telegram bot.** Simple mode: one token, with a short walkthrough of BotFather.
   Isolated mode: the household group bot now, and a note that each member creates their
   own during enrolment.
4. **Members.** Names only. Telegram ids are not asked for — they arrive through
   `kenward invite`, because asking someone to find their numeric Telegram id is a
   terrible first experience.
5. **Endpoints.** For each: name, base URL, model, tiers. The wizard probes each one as
   it is entered and says whether it answered, so a typo is caught immediately rather
   than at the first message.
6. **Tier chains.** Per member and for the group, defaulting to local-only for private
   spaces and offering to add a cloud tier explicitly. The default is the private one;
   opting into cloud is a deliberate act.

It finishes by printing the privacy statement for the chosen mode — the honest one, in
full, from `internal/privacy`, which is the same text `doctor` prints — and the path it
wrote.

Details the first draft of this document left out, settled during implementation:

- **The trust question has no default.** It is the one answer that cannot be fixed later
  by editing a file. On Windows or macOS, choosing the sealed option explains that
  isolated mode is Linux-only, offers simple mode by its own name, and **defaults to
  stopping** — so a stray Enter never downgrades the mode somebody just asked for.
- **Tokens are never written to the config**, only environment variable *names*. The
  wizard offers to write a `.env` beside the config at 0600, never overwrites an existing
  one, and prints variable names rather than values. On Linux it also says that the
  shipped systemd unit supplies secrets with `LoadCredential=` rather than an environment
  file, and that a secret comes from a variable, a file or a credential but from exactly
  one of them. Without that note an operator who has just been told to set
  `KENWARD_BOT_TOKEN` opens the unit, finds no `EnvironmentFile=` in it, and concludes
  they have misread one of the two.
- **Per-member tokens cannot exist yet at setup time** in isolated mode, since each member
  creates their own bot during enrolment. The wizard validates against the process
  environment overlaid with the variables it is telling the operator to create, and lists
  them under "kenward will not start until these are set".
- **The group's tier chain also defaults to local-only.** CLI.md originally said this for
  private spaces; defaulting the group the same way is the conservative reading, and
  opting into a provider stays a deliberate act everywhere.
- **A household with only cloud endpoints is warned and must confirm**, because a
  configuration in which nothing can ever be answered locally is worth noticing before it
  is written rather than after.

`--non-interactive` with flags for every answer, for scripted installs.

`--force` replaces an existing configuration. Without it, setup refuses when a file is
already there and says why: a household's configuration is full of decisions somebody
made once, and overwriting it because a wizard was run twice is not recoverable from the
file that no longer exists. The refusal is a usage error, so a script notices.

---

## `kenward run [--config PATH] [--data-dir PATH] [--member ID | --group] [--image REF]`

Runs the node. This is what the container entrypoint and the systemd unit call.

`--data-dir` overrides the `data_dir` in the configuration, which itself defaults to a
per-OS state location. It is what the container image sets, since a container's home
directory is not where anyone expects state to live.

`--member` and `--group` exist for **isolated mode only**, where each pod runs exactly
one unit: a member's pod is started with `--member david`, the household's with
`--group`. In simple mode both are omitted and one process runs every unit. Passing
either in simple mode is a usage error rather than a silently ignored flag — a flag that
does nothing is how someone ends up believing they are isolated when they are not.

`--image REF` is **isolated mode only** as well, and names the pod image the host
supervisor starts pods from. It has no configuration field, deliberately: there is no
sensible default for the artifact a household's private conversations run inside. Omitted,
a released build starts pods from `ghcr.io/blueheisenberg/kenward:<its own version>`, so
the host and its pods run the same code by default rather than by an operator remembering
to keep two strings in step. A development build has no published tag to fall back to, so
it refuses to start and says to pass `--image REF` — guessing a tag there would start pods
from somebody else's build of kenward.

- Loads and validates the config; refuses to start on any validation error, printing all
  of them at once.
- Refuses to start with a secret it cannot read, listing every one and where it looked —
  an environment variable that is unset or empty, a file that is missing or too
  permissive, a systemd credential that is not there. Never starts half-configured: a
  node missing one member's token would serve everyone else and silently drop that
  member.
- Logs, on startup, exactly one summary of what it will and will not do — the mode, the
  members served, and each space's tier chain. An operator should be able to read that
  line and know whether a private space can reach a provider.
- Handles SIGINT/SIGTERM: stop accepting messages, finish in-flight turns, lock all
  sessions, exit.

### Where the configuration and the data directory come from

Both settings are resolved the same way, and every command that takes them uses the same
resolution: **the flag wins, then the environment, then the built-in default.**

`--config`, in order:

1. `--config PATH`
2. `$KENWARD_CONFIG`
3. `kenward.yaml` in the working directory, if it exists
4. the per-OS config location — `kenward/kenward.yaml` under `os.UserConfigDir()`

The working directory comes before the per-OS location because that is where `kenward
setup` writes by default. Somebody who has just run setup and then runs kenward in the
same shell must not be told there is no configuration.

`--data-dir`, in order:

1. `--data-dir PATH`
2. `$KENWARD_DATA_DIR`
3. whatever `data_dir` says in the configuration, which itself falls back to the per-OS
   state location

The environment layer is not a convenience. The container image sets both variables, and
that is what makes `docker run image` work with no arguments at all — while an operator
passing a flag on the command line still wins over the image's own defaults. Changing
the precedence would either break the image or make it impossible to override.

---

## `kenward invite --name NAME [--ttl 24h]`

Mints a single-use claim code for a member.

```
Claim code for David:

    frost-amber-7431

Give this to David and have them message the bot. It works once and expires in 24
hours. Until they use it, the bot will not reply to them at all.
```

The code is stored hashed. Nothing else is printed — no QR, no link, no deep link that
would leak the code into a chat log.

---

## `kenward revoke MEMBER`

Unbinds a member's Telegram account. Prints plainly that this stops kenward serving
them, and that **the lore space key must be rotated separately** — kenward cannot do
that itself, and implying otherwise would be a false security claim.

---

## `kenward doctor`

The command that answers "is this actually working, and is it doing what I think?" Runs
every check and reports all results rather than stopping at the first failure.

```
kenward v0.1.0 — mode: simple

Configuration
  ✓ kenward.yaml parses and validates
      /etc/kenward/kenward.yaml
  ✓ every secret the configuration names can be read

Memory
  ✓ lore mcp responds
  ✓ space "7d5047bb-d939-4539-b3db-8b6221a2e245" reachable
  ✓ space "dac31e70-72e4-4b10-9cef-a6276c4a87b8" reachable
  ✓ space "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19" reachable
  ! `lore mcp` does not sync on its own
      run `lore serve` on the same LORE_HOME if this store should reach another
      machine

Sessions
  ✓ key custody: simple mode. One node passphrase, held by the operator, wraps
    every member's key (members: david, jordan). The operator can unlock any
    member's key and with it that member's private memory. That is this mode's
    stated limitation, not a bug.
      whether a key is unwrapped right now is not visible from here: unwrapped
      keys live in the running node's memory and this is a different process
  ✓ 2 members have a wrapped key on disk

Transport
  ✓ household: Telegram authorises as @our_household_bot

Endpoints
  ✓ monster     local  answered in 412ms
  ✓ openrouter  cloud  answered in 412ms

Privacy

Every member's memory is separate: what you tell kenward in a private chat is
stored in your own space, and the household group can never read it.

What this mode does NOT do is seal anything against whoever runs this machine.
All members' keys live in one process here, and one bot token carries every
conversation, so the person operating this computer can read every member's
private memory — on the disk, and in flight on its way to and from Telegram.
For most households that is fine — it is your own family machine, and you
already trust whoever set it up.

If that is not the arrangement you want, isolated mode gives each member their
own sealed process, their own key and their own bot. It needs Linux with Podman
or Docker.

Two things hold whichever mode you are in. A conversation whose tier chain
names only machines in the house never reaches a provider: when none of them
answers, kenward refuses rather than reaching further, and there is no setting
that changes that. And nothing is written to memory without the member seeing
the exact words first and saying yes.

Where each conversation may go

  David: [local] — will refuse rather than use a provider
  Jordan: [local, cloud] — may use a provider
  Casa: [local, cloud] — may use a provider
```

That is the whole report, and its shape is golden-tested end to end, for both modes, in
`cmd/kenward/testdata`. The Privacy block and the per-conversation lines under **Where
each conversation may go** are byte-exact: they come verbatim from `internal/privacy`,
which is also what `kenward setup` prints, so the two can never drift into promising
different things. It is the single most important output the product produces, because it
is where a claim becomes checkable, and it must never drift into overstating what the mode
delivers. The names, paths and space ids above are one household's; only the wording is
fixed.

**Note the space lines.** Those are lore space ids, from the id column of `lore spaces`,
because that is what `shared_space` and `private_space` must hold. A display name there
writes successfully and fails on the first read, so `doctor` reports one as a
configuration fault and exits 2 rather than passing it. The reasoning is in
[IMPLEMENTATION.md](IMPLEMENTATION.md) section 4.

An endpoint that does not answer is reported as a fact, not a failure, and the report
says so where it happens:

```
  An endpoint that does not answer is reported here, not failed. Household
  machines are legitimately switched off, and a conversation whose chain names
  only unreachable machines is refused rather than sent somewhere else.
```

`doctor` exits non-zero only on configuration faults, an unreachable lore, or a Telegram
authorisation failure. This matters beyond tidiness: the container's `HEALTHCHECK` runs
`doctor`, so treating a sleeping GPU box as unhealthy would put a perfectly good
household into a restart loop.

It takes `--member ID` and `--group`, and reads `KENWARD_MEMBER` / `KENWARD_GROUP` when
neither is given, exactly as `run` does — the container's `HEALTHCHECK` is a separate
process with no arguments, so the environment is how it learns which unit its container
is. Scoped that way, the report is about that one unit: the first line says so, and only
that unit's bot token, provider keys, lore spaces, key custody and tier notes are checked.
It has to be: a member's pod holds only that member's token, so a household-wide check
inside it would fail on every sibling secret the container correctly does not have.

---

## `kenward update [--check] [--config PATH] [--data-dir PATH]`

The update path described in [IMPLEMENTATION.md](IMPLEMENTATION.md) section 9. `--check`
reports what is available and changes nothing. Without it, applies: signature verified
against a key compiled into this binary, the staged binary run once before the swap,
swapped atomically, and rolled back automatically if the new version does not come up on
its next start.

Consent is required for a major version or a release flagged as changing
security-relevant behaviour, and the prompt says which of the two it is, because they
deserve different amounts of thought. Answer `y` or `n`. **Anything else is unanswered,
not declined** — no terminal, a pipe, a cron job, a closed stdin — which means nothing is
applied now and the same release is offered again next time. A considered `n` is
remembered and not raised again until a different version appears. One unheard question
must never suppress a security release permanently, and a pipe that ended is not somebody
deciding.

Prints the channel in use, and says so plainly when it is `off`.

A running node checks and applies on its own; this command exists for the cases that one
cannot handle. It judges a manifest and a build by exactly the same rules — same
signature check, same manifest-age refusal, same health check — because a `kenward
update` that was more permissive than the scheduled path would be the thing somebody
reaches for precisely when the automatic path has already refused. The one difference is
drain: a CLI invocation is serving nobody, so there is no turn in flight to wait for.

**It is currently the only way a major or security-flagged release gets applied**, since
the running node has no way to ask the household over Telegram yet and therefore holds
such releases back. Section 9 of [IMPLEMENTATION.md](IMPLEMENTATION.md) records why.

Applying an update does not restart the running node: it says so, and leaves the restart
to whoever runs the machine. Restarting the household's assistant out from under an
operator is not this command's decision.

---

## `kenward version`

Version, commit, build date, Go version, platform. One line.

---

## Not in this binary

Release tooling — key generation, manifest construction, signing, and reading a signed
manifest back — lives in a separate `kenward-release` binary that is not shipped to
households. See
[RELEASING.md](RELEASING.md). A household's copy has no reason to be able to sign
anything, and a capability present in a widely-installed binary is one an attacker
inherits.

---

## Conventions

- Exit codes: `0` success, `1` runtime failure, `2` configuration or usage error.
- Errors go to stderr; anything a script might parse goes to stdout.
- No command ever prints a bot token, an API key, a claim code that was already given
  out, or the contents of anyone's memory.
- `--json` on `doctor` and `version` for scripting. Not on the others; there is nothing
  to parse.
