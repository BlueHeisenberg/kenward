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

2. **Household name and shared space.**
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
full — and the path it wrote.

`--non-interactive` with flags for every answer, for scripted installs.

---

## `kenward run [--config PATH] [--data-dir PATH] [--member ID | --group]`

Runs the node. This is what the container entrypoint and the systemd unit call.

`--data-dir` overrides the per-OS default and is what the container image sets, since a
container's home directory is not where anyone expects state to live.

`--member` and `--group` exist for **isolated mode only**, where each pod runs exactly
one unit: a member's pod is started with `--member david`, the household's with
`--group`. In simple mode both are omitted and one process runs every unit. Passing
either in simple mode is a usage error rather than a silently ignored flag — a flag that
does nothing is how someone ends up believing they are isolated when they are not.

- Loads and validates the config; refuses to start on any validation error, printing all
  of them at once.
- Refuses to start with a missing environment variable, listing every one. Never starts
  half-configured.
- Logs, on startup, exactly one summary of what it will and will not do — the mode, the
  members served, and each space's tier chain. An operator should be able to read that
  line and know whether a private space can reach a provider.
- Handles SIGINT/SIGTERM: stop accepting messages, finish in-flight turns, lock all
  sessions, exit.

`--config` defaults to `kenward.yaml` in the working directory, then to the per-OS
config location.

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
  ✓ all referenced environment variables are set

Memory
  ✓ lore mcp responds (5 tools)
  ✓ space "david-private" reachable
  ✓ space "household" reachable
  ! lore serve is not running — spaces will not sync between instances

Transport
  ✓ Telegram authorises as @our_household_bot

Endpoints
  ✓ monster       local        answered in 412ms
  ✗ 5090          local        no route to host
  ✓ openrouter    cloud        answered in 890ms

Privacy
  In simple mode every member's key lives in one process. Whoever runs this
  machine can read every member's private memory. Separation between members is
  real; sealing against the operator is not. Isolated mode provides that.

  david    tiers: [local]              → will refuse rather than use cloud
  household tiers: [local, cloud]      → may use a provider
```

The Privacy section is golden-tested. It is the single most important output the product
produces, because it is where a claim becomes checkable, and it must never drift into
overstating what the mode delivers.

A powered-off endpoint is reported as a fact, not an error — `doctor` exits non-zero only
on configuration faults, an unreachable lore, or a Telegram authorisation failure.

---

## `kenward update [--check]`

Manual trigger for the update path described in
[IMPLEMENTATION.md](IMPLEMENTATION.md) section 9. `--check` reports what is available
and changes nothing. Without it, applies subject to the same rules the automatic path
follows: signature verified, drained first, health-checked after, rolled back on
failure, and consent required for a major version or a release flagged as changing
security-relevant defaults.

Prints the channel in use, and says so plainly when it is `off`.

---

## `kenward version`

Version, commit, build date, Go version, platform. One line.

---

## Conventions

- Exit codes: `0` success, `1` runtime failure, `2` configuration or usage error.
- Errors go to stderr; anything a script might parse goes to stdout.
- No command ever prints a bot token, an API key, a claim code that was already given
  out, or the contents of anyone's memory.
- `--json` on `doctor` and `version` for scripting. Not on the others; there is nothing
  to parse.
