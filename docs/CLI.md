# Command surface

kenward is one binary. The command surface is small on purpose: a household assistant
that needs a manual is one that does not get installed.

Everything here is specified before implementation so `cmd/kenward` and
`internal/setup` agree. Output shown is illustrative of register and content, not
byte-exact except where marked golden.

---

## `kenward setup`

The first-run wizard at a terminal. Interactive, writes `kenward.yaml`, and is one of the
two places the mode is chosen — the other is the admin dashboard's own wizard, which asks
the same questions through the same code.

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

2. **Household name.** One question, and the household's shared memory follows from it:
   the wizard creates a shared lore space called "<household> — household" and writes its
   **id** into the file. It does not ask which space to use and it does not ask what to
   call one.

   Both of those were questions once and both were wrong. Asking what to call it
   slugified the answer into the id field, producing a configuration that wrote memories
   happily and returned nothing on the first read. Asking *which* fixed that and cost
   more: spaces had to exist before the wizard ran, so installing kenward meant fetching
   lore, running `lore init`, running `lore space create` once per person, running `lore
   spaces`, and copying an id column into a wizard whose only remaining job was to write
   down what those five commands produced. lore exports `CreateSpace`, so the wizard
   makes them, as the dashboard's first-run wizard already did.

   Run setup again over a household that exists and the spaces of those names are reused,
   not duplicated: `--force` corrects a typo without splitting anybody's memory in half.
3. **The Telegram bot.** Simple mode: one token, with a short walkthrough of BotFather.
   Isolated mode: the household group bot now, and a note that each member creates their
   own during enrolment.

   **The walkthrough includes `/setprivacy` → the bot → Disable, and setup then checks
   it.** Telegram turns bot privacy mode on for every new bot, and with it on the bot
   receives *nothing* sent in a group chat — not plain messages, not a reply to it, not
   an @mention; only `/start@thebot` is delivered. A household adds the bot to their
   family group and it ignores everyone, with no error, no warning and no log line
   anywhere, because nothing arrives to log. It is the worst failure shape this product
   has, and it is the default state of every bot BotFather hands out.

   So it is in the walkthrough *before* the token is pasted, and setup asks Telegram
   about it as soon as it has one: `getMe` returns `can_read_all_group_messages`, and
   when it is false the wizard says what is wrong, what to do in BotFather, and offers
   to check again — which is what turns the paragraph into something verifiable while
   somebody is still standing at the machine. It never blocks: a household that will
   never use a group chat, and one whose connection is down, both carry on.

   The ordering matters and the copy says so. **Telegram applies a privacy-mode change
   only to groups the bot joins afterwards**, so a bot already in the group has to be
   removed and added again. Doing it during setup, before the bot is in anything, costs
   one message to BotFather. `kenward doctor` reports the same thing, as a warning, for
   the household that maps its group months later.
4. **Members.** Names only. Each person's private memory is created from their name —
   a shared-kind space called "<household> — <name>", because a member's private space
   has two members in it, the person and the node, and lore's `personal` kind never
   crosses accounts. Telegram ids are not asked for either: they arrive through `kenward
   invite`, because asking someone to find their numeric Telegram id is a terrible first
   experience.
5. **The identity question, and kenward's persona.** A second question, and it is not the
   trust question's other half — conflating them is the mistake this pair exists to
   prevent. That one is about security and is answered by topology; this one is about
   presentation, about who the household is talking to.

   They meet in exactly one place, and it is a counting problem rather than a privacy
   one: an agent is a Telegram contact, simple mode runs one bot for the whole
   household, and two agents behind one contact are one agent. So `[2]` below is offered
   in both modes and declined in simple mode, with that reason — never downgraded,
   because a household that asked for their own assistants and got kenward wearing
   several names would have no way to tell. `config.Validate` refuses the same pair in a
   hand-written file.

   ```
   One assistant for the whole household, or one each?

     [1] One — kenward, for everybody.            (the character below is then
                                                   everyone's, not just yours)
     [2] One each, and kenward for the group.     (each person names their own
                                                   and writes it in Telegram)
   ```

   `[1]` is the default, because it is what kenward has always done. Under it there is no
   personal layer, so the persona the admin writes next is **everyone's**, and the wizard
   says so at the point of asking rather than in a paragraph above it. Under `[2]` each
   member writes their own in the Telegram tutorial and the admin is only setting the
   family agent's.

   **`[2]` then asks for the household group's Telegram chat id, and will not take a
   blank one.** It is the only numeric Telegram id the wizard ever asks for, and it is
   asked because under one assistant each there is no other route to it: kenward has two
   conversations of its own — the household group, and each member's private chat with
   kenward on the household bot — and the supervisor builds **both** off the same id,
   only when `household.group_chat_id` is set. A `per_member` configuration without one is a
   household that cannot be reached at all — not in the group, and not in a private chat
   — which is the same failure `[2]` + simple is refused to prevent, arriving through the
   other door. `doctor` warns about it afterwards, and a warning nobody was told to run
   is not a remedy, so this is a refusal in the wizard instead. The question says how to
   find the number: add the bot to the group, send a message, and read it off
   `https://api.telegram.org/bot<TOKEN>/getUpdates`. The dashboard's wizard and its
   settings page refuse the same combination, and so does `--non-interactive`, through
   `--agents`: all three front-ends reach it through the same function, so there is one
   refusal rather than three.

   Then three questions about how kenward writes — language, tone, character — each with
   an answer that changes nothing. Pressing Enter three times gives the assistant kenward
   has always been: English, brief, no character.

   The language question says what it does and does not reach, because it is the sort of
   half-answer somebody otherwise discovers a week later. It is free text and is passed
   to the **model** as written, so a household may ask for a language kenward has never
   heard of. It also chooses kenward's own strings — the capture announcements, the
   refusals, the retrieval line, the reminder notices, the locked notice, the memory
   model explained to each member when they join — but only from the closed list those
   are written in, which the wizard prints rather than paraphrases. Name anything else
   and the model still answers in it while kenward's own strings stay English.

   Under `[2]`, the privacy statement printed at the end gains a paragraph saying what a
   member's own bot is doing: it is that member's assistant's own **contact**, and it is
   the **mode** — not the bot — that seals their memory. Somebody who has just made a bot
   in BotFather with their own name on it has every reason to believe otherwise, and the
   ceremony is identical either way. That paragraph is `internal/privacy`'s and is
   golden-tested with the rest.

6. **Endpoints.** For each: name, base URL, model, tiers. The wizard probes each one as
   it is entered and says whether it answered, so a typo is caught immediately rather
   than at the first message.

   It does **not** ask for the endpoint's context window or its completion cap, because
   it cannot check either answer: the probe learns that an address answers, not what the
   server behind it was started with, and an unverifiable number typed into a wizard is
   worse than a default. It writes `context_window` and `max_completion_tokens` out at
   their defaults instead — 16384 and 4096 — so the keys are visible in the file for the
   operator who knows the real figures. Raise them there; on a reasoning model, raise the
   cap in particular, or it will think through its whole budget and answer nothing.
7. **Tier chains.** Per member and for the group, defaulting to local-only for private
   spaces and offering to add a cloud tier explicitly. The default is the private one;
   opting into cloud is a deliberate act.
8. **How long a conversation stays in mind** — `history.reset_every`. Off, or how often
   the last few turns of each conversation are dropped: `6h`, `24h`, up to a day, counted
   from midnight. Off is the default and pressing Enter takes it.

   It is asked rather than left to the file for one reason: the symptom of this setting
   is an assistant that has lost the thread, which is indistinguishable from a fault, so
   somebody who was never shown the question has no reason to believe the behaviour is
   theirs to choose. Most of the question's words are spent on what it does *not* clear
   — a person who came here for an assistant that remembers things will otherwise read
   "clear the conversation" and answer the question they think they were asked.

It finishes by printing the privacy statement for the chosen mode — the honest one, in
full, from `internal/privacy`, which is the same text `doctor` prints — and the path it
wrote.

Two things leave the machine while it runs, and the banner says so rather than claiming
otherwise: each endpoint address is connected to, and the bot token is shown to Telegram
to ask which bot it is and whether it can hear a group chat. Nothing else typed into the
wizard is sent anywhere, and nothing is written until the end.

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

`--non-interactive` takes its answers from flags, for scripted installs. Every question
the wizard asks has one: `--mode`, `--household-name`, `--shared-space`,
`--bot-token-env`, `--agents`, `--group-chat-id`, `--persona-language`, `--persona-tone`,
`--persona-character`, `--member`, `--member-space`, `--member-tiers`, `--group-tiers`,
`--endpoint`, `--data-dir`, `--write-env-file`.

It refuses what the questions refuse, in the same words and for the same reasons,
because all three front-ends reach the same function in `internal/setup`:

- `--agents per_member` needs `--mode isolated`. An agent is a Telegram contact, simple
  mode runs one bot for the whole household, and two agents behind one contact are one
  agent.
- `--agents per_member` needs `--group-chat-id`. Under one assistant each, kenward
  itself lives in the group and nowhere else, so a household without one has no kenward
  in it — and `kenward run --group` refuses to start such a pod.

Both are refused before any lore space is created, so a rejected invocation leaves
nothing behind.

**There is no flag for any secret**, and that is deliberate rather than missing: no
`--bot-token`, no member passphrase, no endpoint API key. A value in `argv` is a value in
`ps`, in the shell history and in the CI log, and the configuration already names an
environment variable for each secret — a script exports those, which is the channel every
deployment path uses anyway. The dashboard's wizard has fields for them because a browser
form has no other channel. `--write-env-file` therefore writes only what setup itself
collected, which under `--non-interactive` is nothing.

`history.reset_every`, `memory.search_limit`, `session.idle_timeout`,
`capture.max_proposals_per_turn` and `update.channel` have no flags either, also by
design: a scripted install that says nothing about them should get the defaults, not a
widened flag surface. A script that wants a schedule edits the file it just generated.

`--force` replaces an existing configuration. Without it, setup refuses when a file is
already there and says why: a household's configuration is full of decisions somebody
made once, and overwriting it because a wizard was run twice is not recoverable from the
file that no longer exists. The refusal is a usage error, so a script notices.

There are two first-run wizards, and this is the one at a terminal. The other is the
admin dashboard's, below; they ask the same questions in the same words, because the
dashboard collects its answers into the same `internal/setup` types and finishes by
running this wizard's own scripted path. Two wizards writing two dialects of one file is
how the second one comes to promise something the first does not.

---

## `kenward dashboard [--config PATH] [--data-dir PATH] [--bind ADDR]`

Serves the admin dashboard on its own, without running the household.

This is how a first run is done in a browser. It works with no configuration at all,
which is precisely why it is a separate command: `kenward run` refuses to start without
one, and the point of the wizard is that there is not one yet.

On the way up it prints where it is listening, and — if this household has no admin
account — a single-use setup token:

```
kenward dashboard listening on http://127.0.0.1:8770

This household has no admin account yet. Open the dashboard and paste this token:

    1p3JcC0BdEuG-jHVJHhOB7uT4YRl9DAKZOr0dCGrb4M

It works once and expires in 30m0s. Reissue with `kenward setup-token`.
Setup happens on loopback: nothing on your network can reach that page.
```

The token goes to stdout and nowhere else. It is not logged, because a log is a file, and
a live credential in a file a service manager rotates into somebody's journal is a
credential with a longer life than thirty minutes.

`--bind` overrides the address for this run. It cannot be used to make a claim: binding
somewhere that is not loopback while the configuration says `exposure: loopback` is
refused, with the reason — exposure is chosen in the dashboard, under Access, on loopback,
after the account that protects it exists. Opening the port first and deciding afterwards
is the order this refuses.

Once a household is configured, `kenward run` serves the same dashboard from inside the
node when `dashboard.enabled` is set — same package, same routes, same account. This
command is for the times something is wrong enough that the node will not start.

---

## `kenward setup-token [--config PATH] [--data-dir PATH]`

Reissues the first-run token and prints it. Whatever was outstanding stops working:
there is one token.

It exists because the token lives thirty minutes and the process that printed the first
one may have been started hours ago, or by a service manager whose output nobody kept.

It refuses, as a usage error, once an admin account exists. There is nothing a setup
token could be for at that point, and minting one would be minting a way past the
password. If the password is lost, delete `dashboard/admin.json` under the data directory
from a shell on the machine — that is the whole recovery procedure, and it requires the
access an attacker would need anyway.

---

## `kenward run [--config PATH] [--data-dir PATH] [--member ID | --group] [--image REF] [--invites PATH] [--revoked PATH]`

Runs the node. This is what the container entrypoint and the systemd unit call.

`--data-dir` overrides the `data_dir` in the configuration, which itself defaults to a
per-OS state location. It is what the container image sets, since a container's home
directory is not where anyone expects state to live.

**It refuses to start unless memory actually answers.** Before anything is built, `run`
creates this node's lore home if the machine has never had one, then opens the store and
searches it once — the same check `doctor` performs, through the same seam, so the two
cannot drift. The isolated **host supervisor** is exempt, because it starts pods and
holds no memory client of its own; each pod asks the question of itself on its own way
up.

**And it refuses a store that is not this household's.** "Does the store answer" stopped
being a sufficient question the moment `run` began creating its own lore home: a store
kenward minted a second ago answers perfectly and holds nothing. So a store holding
**none** of the spaces this unit is configured for is a refusal, naming the spaces and
the store. That is what a household whose memory has been moved, restored or handed a
fresh container volume looks like, and serving on it is worse than not starting — the
node authorises its bot, greets everybody and remembers nothing anyone says to it.

A store holding **some** of them is not a refusal. That is one member's mistyped id, one
conversation's problem; it is logged at startup and the household is served. The one unit
allowed to hold none of its spaces is an **isolated pod**, because a pod is provisioned by
an operator running `lore space create`, `lore space invite` and `lore join` *inside a
running pod* — one that refused to start until its spaces existed could never be given
them (`deploy/compose.isolated.yml`, steps 4b and 4c).

**No lore binary is looked for, in any mode, and there is no longer a key naming one.**
`run` used to check `memory.lore_command[0]` against `$PATH` and refuse without it, on
the reasoning that spawning `lore mcp` was kenward's only route to memory. That route is
gone, and so is the last one after it: the store, every read and write, and — since lore
v0.5.0 — the sync daemon are all library calls in this process. A refusal over an absent
program would refuse a node that works, and an isolated household of pods has been
measured converging its shared space with no `lore` on `$PATH` anywhere inside them.
`memory.lore_command` has been removed with the last reader; a file that still carries it
is refused with a line saying to delete it.

The published image does still carry the lore CLI, and that is not a contradiction. It is
there for the one step with no Go API — the `lore space invite` / `lore join` membership
handshake an operator runs by hand inside a pod. `run` never invokes it.

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

`--invites PATH` names a file of outstanding claim codes to import into this unit's own
invite store on the way up. It exists for **isolated mode** and for one reason: `kenward
invite` mints into the host's store, and the claim is redeemed inside the member's pod,
against the store on that pod's own volume — two files on two filesystems, with nothing
crossing between them by itself. The file is what crosses. Both deployment paths put it
at `/etc/kenward/invites.json`: the host supervisor provisions it there at pod-create
time, the compose deployment bind-mounts it read-only. A path that names no file means no
invite is outstanding, which is the ordinary case; a path that exists and cannot be read
is a refusal, because the alternative is a pod that comes up, is handed a real code, and
refuses it in the silence enrolment owes a stranger.

`--revoked PATH` is the same crossing in the other direction, and the only one revocation
has. It names a record written by `kenward revoke` — a member id and a time — which this
unit applies to the enrolment state on its own volume before it serves anybody, clearing
the binding it names. It exists because the binding lives here rather than on the host:
the claim was redeemed in this pod, and a host that could reach into a running member's
volume to clear it could read that volume too. Both deployment paths put the record at
`/etc/kenward/revoked.json`. A path that names no file is no revocation, which is the
ordinary case; a path that exists and cannot be read is a refusal, because the
alternative is a pod that starts and goes on serving an account somebody revoked. A
record naming a different member is refused outright rather than applied to whoever this
pod serves, and a binding made *after* the recorded time is kept — a member who was
revoked, invited again and claimed again must not be unbound by the old record on every
rolling update.

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

**It reads no secret, and so demands none.** `invite` writes a digest; it opens no bot,
unwraps no key and calls no provider. It loads `kenward.yaml` for its structure alone —
the household roster, the modes, the paths — and resolves not one bot token, passphrase
or API key. `revoke` is the same. That is not a convenience: unscoped, both commands
demanded *every* member's secrets, and in isolated mode no container holds a sibling's,
so neither could be run anywhere at all. What is still refused is unchanged: a file that
cannot describe a household, and a `--name` or member id the file does not declare.

**In isolated mode it also writes the digest where the member's pod will find it**, and
says so:

```
This household is isolated, so the code travels to jordan's pod when that pod is
next created. If it is already running, restart kenward before handing the code over.
```

The file is `<data_dir>/invites/<member-id>.json`: that member's outstanding codes and no
other member's, which matters because the store this is derived from holds every
member's. The deployment carries it into the pod (`run --invites`, above). It is a
snapshot, so a code minted while that member's container is already running reaches it on
the next pod creation rather than immediately — the same staleness the host's own view of
enrolment has, and the reason that line is printed rather than left to be discovered when
the member reports that nothing happened.

---

## `kenward revoke MEMBER`

Unbinds a member's Telegram account. Prints plainly what it has done, what it has not,
and that **the lore space key must be rotated separately** — kenward cannot do that
itself, and implying otherwise would be a false security claim.

**It refuses while `kenward.yaml` declares that member's `telegram_id`.** A hand-written
`telegram_id` is not in the enrolment record kenward owns, so clearing that record around
it revokes nothing: the next start reads the line and serves the account again. kenward
does not rewrite your configuration, so it names the line and stops before anything has
been cleared:

```
kenward: /etc/kenward/kenward.yaml declares telegram_id: 12345678 for member david, and
kenward does not rewrite your configuration. Clearing the enrolment record around it
would revoke nothing: the next start would read that line and serve the account again.

Delete this line from members[david]:

      telegram_id: 12345678

then run this command again.
```

**A revocation takes effect at the next start, in both modes.** A running node decided
who it serves when it started and does not re-read this while it runs, so the output says
to restart.

**It also deletes that member's reminders** (simple mode; in isolated mode they are on the
member's own pod volume, out of the host's reach, exactly as the binding is). That is not
tidying: a reminder stores the chat it is delivered to, so a file left behind would send
the household's reminders to the revoked account the day that member id is enrolled again.
A file that cannot be removed is reported and does not fail the revocation, which has
already happened by then.

**In isolated mode the binding is not here at all**, and the command says so rather than
reporting a revocation it did not perform:

```
Jordan is NOT unbound yet: the binding lives in their own pod, and this command
has recorded the revocation rather than performed it.
Their lore space "5f2a9c14-…" has NOT been re-keyed — kenward cannot rotate a lore key.
…

This household is isolated, so jordan's binding lives in jordan's own pod and this command
cannot reach it. The revocation is recorded at

    /var/lib/kenward/revocations/jordan.json

and that pod applies it the next time it is created. Restart kenward now — until
you do, that pod is still serving them.
```

The restart is what creates the pod again: the host supervisor recreates, at start and
before the monitors run, any member pod that could be holding a stale copy of these files
— a revoked member's, and an unclaimed member's with a code outstanding. Every other pod
is started rather than replaced. The member's work volume, and therefore their lore, is
preserved by the recreation.

The claim was redeemed inside that member's pod, against the enrolment state on that
pod's own volume, and the host must not write there — every mechanism that could is one
edit from reading it back, and that volume holds the member's wrapped key and their lore.
So the record crosses the same one-way, create-time channel a claim code takes
(`run --revoked`, above), and the pod clears its own binding on the way up. The delay is
real and is the price of the boundary; what it replaced was a command that reported
success while the pod carried on serving the account.

The compose deployment mounts the record itself — see the header of
`deploy/compose.isolated.yml`.

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
  ✓ one assistant for this household: kenward answers every private chat and the
    group

Memory
  ✓ lore answers
  ✓ space "7d5047bb-d939-4539-b3db-8b6221a2e245" reachable
  ✓ space "dac31e70-72e4-4b10-9cef-a6276c4a87b8" reachable
  ✓ space "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19" reachable
  ! this lore store does not sync on its own
      isolated mode runs a sync daemon in each pod; a simple-mode node has one
      store holding every space and nothing to converge with
  ✓ conversations keep their recent turns until the node restarts
    (history.reset_every is off)

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
that changes that.

And nothing is written to memory without you being told. A note to your own
private memory is written first and then shown to you in full — the exact words
and the space they went to — with an Undo button that removes it. Anything
going to the household's shared memory is shown to you first and written only
if you say yes, because other people will have read it by the time you regret
it. Your household can turn the private half back into a question, and some do.
It cannot turn off being told, and it cannot turn off the question for the
shared memory.

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

**The assistants line says which conversations exist**, which is `household.agents`'
only visible consequence. The report above is a household with one assistant. A
household that gave every member their own gets this instead, and the second detail
line is the one worth reading — it names a chat an operator would otherwise only find
out about by messaging the bot:

```
  ✓ one agent each: every member has their own assistant, and kenward is the
    household's
      kenward is also reachable in a private chat, on the household bot
      that conversation reads and writes the household's shared memory only, and
      never a member's private memory
```

**Note the space lines.** Those are lore space ids, because that is what `shared_space`
and `private_space` must hold — the wizard that made the spaces wrote them there, and
nobody types one. A display name in that field writes successfully and fails on the first
read, so `doctor` reports it rather than passing it. The reasoning is in
[IMPLEMENTATION.md](IMPLEMENTATION.md) section 4.

Whether that line also changes the **exit code** is the same judgement `run` makes about
whether to serve, and deliberately the same code (`judgeMemory`). One mistyped id among
several is a `!` and exit 0, because `run` serves that household. A store holding none of
its spaces is a `✗` and a non-zero exit, because `run` refuses it. This used to be exit 2
either way, and the disagreement was the defect: `doctor` is the image's `HEALTHCHECK`, so
a condition the node starts through and the health check fails on is a container marked
unhealthy forever and restarted into the same state. Unhealthy has one meaning here —
*kenward would refuse to start on this* — and everything else is a line on the report.

An endpoint that does not answer is reported as a fact, not a failure, and the report
says so where it happens:

```
  An endpoint that does not answer is reported here, not failed. Household
  machines are legitimately switched off, and a conversation whose chain names
  only unreachable machines is refused rather than sent somewhere else.
```

The **Access** section, immediately after Configuration, is the dashboard's bind address
and exposure as a first-class line:

```
Access
  ✓ admin dashboard: off — no port is opened
      nothing listens; kenward is configured and operated from this machine's
      own shell
```

or, on a household that has opened one:

```
Access
  ! admin dashboard: lan on https://192.168.1.20:8770
      every device on your own network can reach the admin login
      the certificate is self-signed; check its fingerprint against the
      browser's once
      a tailnet or VPN is the better way in from another machine
```

It is a section rather than a line under Configuration because it is the only thing in
this report that says who can reach *this machine*; everything else answers whether the
node works. Nothing in it changes the exit code — see below — and a configuration that is
genuinely unsafe (LAN with no TLS, an exposure that contradicts its own bind) never gets
this far: it is refused at load and appears as the parse failure it is.

The **Reminders** section, between Memory and Sessions, is what this node will send when
nobody is talking to it — the only question on the report whose wrong answer arrives as a
message on somebody's phone:

```
Reminders
  ✓ at most 6 unprompted messages a day, per conversation
      times are on this machine's own clock; a repeating reminder missed by more
      than 6h0m0s is skipped rather than delivered late, and a one-off is
      delivered however late it is
  ✓ David has 2 reminders scheduled
      every day at 07:30 (a3f0)
      Monday 4 August at 09:00 (b91c)
```

One line per unit this report covers, scoped like Sessions: in a member's pod a sibling's
reminders are not merely unreadable, they are none of that pod's business. **The reminder
text is deliberately not printed** — these are a member's private business and this report
is read over an operator's shoulder; when and how often is what an operator needs. When
the day's allowance is spent the section says so, because the member is never told (an
unprompted message announcing that too many unprompted messages had been sent is not a
thing to build). Nothing in it changes the exit code either.

The privacy block gains a second paragraph, from the same `internal/privacy` the first
one comes from, saying what the listener means. "Whoever runs the machine" stopped being
the whole truth the moment a port could be open.

`doctor` exits non-zero only on configuration faults, an unreachable lore, a store that
holds none of this unit's spaces, or a Telegram authorisation failure — which is exactly
the list of things `run` refuses to start on, and that is the rule rather than a
coincidence. This matters beyond tidiness: the container's `HEALTHCHECK` runs `doctor`,
so treating a sleeping GPU box as unhealthy would put a perfectly good household into a
restart loop, and so would failing on anything the node itself is content to serve
through.

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
- A setup token is printed to stdout by `kenward dashboard` and `kenward setup-token` and
  is never logged. It is the one secret this binary ever prints besides a claim code, and
  like a claim code it exists in the clear exactly once.
