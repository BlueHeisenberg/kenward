# Installing kenward

kenward is one binary. Which mode you run is chosen during setup, by answering one
question about trust. Setup asks a second question that is not about security at all —
one assistant for the whole household, or one each — and answering "one each" changes
what you need before you start, so it is worth deciding both now. See
[ARCHITECTURE.md](ARCHITECTURE.md) if you want the reasoning.

---

## Before you start

**Nothing to install but kenward.** It keeps its own memory: it imports
[lore](https://github.com/BlueHeisenberg/lore) as a Go module, opens the store inside
its own process, and creates that store the first time it runs if this machine has
never had one. There is no lore binary to fetch, no `lore init` to run, and no space to
make by hand — `kenward setup` makes the household's shared space and one private space
per person, and writes their ids into `kenward.yaml` itself. If you already use lore on
this machine, kenward adds its spaces to the store you have and changes nothing else. If
you want lore's own CLI for your own use, that is a separate thing you may install or
not; kenward does not care either way.

So: two things, neither of them a program — and a third if you are giving everybody
their own assistant.

**A Telegram bot.** Message [@BotFather](https://t.me/BotFather), send `/newbot`, follow
two prompts, and keep the token it gives you. It looks like
`123456789:AAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx`. That token is a password — anyone
holding it can read every message sent to the bot.

**Then, in the same chat, send `/setprivacy`, choose that bot, and choose Disable.** This
is not optional if the household is going to use a group chat, and it is the one step
whose omission has no symptom. Telegram turns *bot privacy mode* on for every new bot,
and with it on the bot receives **nothing** sent in a group — not plain messages, not a
reply to it, not even an @mention. Only `/start@thebot` is delivered. So the bot sits in
the family group and ignores everyone, with no error, no warning and no log line
anywhere, because nothing ever arrives to log.

Do it **before** adding the bot to any group. Telegram applies the change only to groups
the bot joins afterwards, so a bot that is already in the group has to be removed and
added again — and somebody who flips the setting and then tests in the group it is
already in will see nothing change and conclude the fix did not work. `kenward setup` and
the dashboard wizard both check this the moment they have the token, and `kenward doctor`
reports it afterwards.

**At least one inference endpoint.** Anything OpenAI-compatible: vLLM, llama.cpp,
Ollama, LM Studio, or a cloud provider. It does not need to be awake during setup —
kenward is built for machines that are usually asleep, and setup will say so rather than
refusing.

**And, if you answer "one assistant each": the household's Telegram group, already made,
with the bot already in it, and a message already sent in it.** Under `agents:
per_member`, kenward itself lives in the group chat and nowhere else — every private chat
belongs to somebody's own assistant — so `household.group_chat_id` is required, and setup
asks for it with no default and no skip. It loops until you give it a number, because a
per-member household without one has no kenward in it at all, in the group or anywhere.

There is no second route to that number: nobody claims a group the way a member claims an
invite. To find it, with the bot's token where `TOKEN` is:

```
https://api.telegram.org/botTOKEN/getUpdates
```

and look for `"chat":{"id":-1001234567890` — that number, minus sign and all. If nothing
comes back, the message was sent before the bot joined, or privacy mode is still on, or
the bot was added to the group before you disabled it (in which case remove it and add it
again).

---

## Where the memory goes

Worth knowing, though nothing here is a step.

kenward's store is a lore home: `$LORE_HOME` if you set one, otherwise `~/.lore`. Setup
creates it if it is not there, and creates one **shared** space for the household and one
per person, named after the household — "Casa — household", "Casa — David". Everything
kenward configures is a shared-kind space, including the private ones: a member's private
space has two members in it, the person and the node, and lore's `personal` kind never
crosses accounts, so kenward could not read one.

The configuration names spaces by **id**, never by display name — lore does not enforce
unique names, and a name in that field would write memories quite happily and return
nothing on the first read. Setup writes the ids, so this is a trap you can no longer fall
into; it is described because `kenward doctor` refuses a configuration that has one, and
that refusal is easier to read if you know why it exists.

Running setup again over a household that already exists reuses the spaces of those
names rather than making a second set, so `--force` corrects a typo without splitting
anybody's memory in half.

---

## Simple mode

One process, one bot, everyone's memory separated but all keys in one place. Whoever
runs the machine can read everything. For most households that is fine — it is their own
family machine — and it is by far the easiest thing to run.

### Linux

```sh
curl -fsSL https://raw.githubusercontent.com/BlueHeisenberg/kenward/main/install.sh | sh
kenward setup
```

The script picks the right build for your machine, checks it against the SHA-256 published
with the release, installs it to `/usr/local/bin` (or `~/.local/bin` if it cannot write
there), and offers to install the systemd unit. It is short and commented; read it first
if you would rather —
[install.sh](https://github.com/BlueHeisenberg/kenward/blob/main/install.sh). Options,
through a pipe, take a `-s --`:

```sh
curl -fsSL https://raw.githubusercontent.com/BlueHeisenberg/kenward/main/install.sh \
  | sh -s -- --dir "$HOME/.local/bin" --no-service
```

`--version v0.1.0` pins a release, `--force` reinstalls one you already have, `--help`
lists the rest.

What it does not check is the release *signature*. That covers the update manifest and
needs an Ed25519 verifier, which a shell has not got; the checksum proves you got the file
GitHub published and no more. `kenward update` from then on does verify signatures, against
a key compiled into the binary.

**By hand**, if you would rather not pipe anything into a shell:

```sh
curl -fLO https://github.com/BlueHeisenberg/kenward/releases/latest/download/kenward_linux_amd64
curl -fLO https://github.com/BlueHeisenberg/kenward/releases/latest/download/checksums.txt
sha256sum --check --ignore-missing checksums.txt
chmod +x kenward_linux_amd64
sudo install -m 0755 kenward_linux_amd64 /usr/local/bin/kenward
```

`kenward_linux_arm64` for a Raspberry Pi or an Ampere box.

**From a package**, if you would rather your package manager knew about it. `.deb` and
`.rpm` builds are attached to every release; they put the binary in `/usr/bin` and the
systemd unit in `/usr/lib/systemd/system/kenward.service`. The package filename carries
the version, so name the one you want — the
[releases page](https://github.com/BlueHeisenberg/kenward/releases) lists them:

```sh
V=0.1.0
curl -fLO "https://github.com/BlueHeisenberg/kenward/releases/download/v$V/kenward_${V}_linux_amd64.deb"
sudo dpkg -i "kenward_${V}_linux_amd64.deb"
```

There is no apt or yum repository, and there is no plan for one — a repository is a second
distribution channel to keep honest, and the signed update manifest is the one that
matters.

To run it as a service after a script or by-hand install, copy `deploy/kenward.service`
into `/etc/systemd/system/`, adjust the config path, then `systemctl enable --now kenward`.
(The installer's service offer does this for you, including pointing `ExecStart=` at
wherever the binary actually landed. It takes the unit from the release and checks it
against `checksums.txt` before writing it, the same as the binary — a unit file names
what runs as root, so it is not installed unverified.) The unit ships with hardening already applied and
each line commented, so you can loosen one knowingly rather than discovering it later.
Nothing enables or starts it for you: kenward needs a config file and a bot token first,
and a unit that fails on install is a unit people learn to ignore.

The unit supplies secrets as **systemd credentials** rather than environment variables,
because an `EnvironmentFile=` value stays in the process's environment for as long as it
runs, is readable through `/proc`, and is inherited by every child it spawns — including
the `lore serve` subprocess, which has no business seeing a Telegram token. Put one file
per secret under `/etc/kenward/credentials/` — `bot_token` for the household bot,
`bot_token.<member id>` and `api_key.<endpoint name>` where they apply — and name each on
a `LoadCredential=` line. kenward looks for exactly those names with no configuration at
all; you do not repeat them in `kenward.yaml`.

Leave the source files `root:root` mode `0600`: systemd reads them as PID 1, before it
drops to the unit's user, so they never need chowning even under `DynamicUser=`, where
the uid changes on every start. The directory kenward reads them back from is an
ephemeral tmpfs that exists only while the unit runs — nothing you put there survives a
restart, and it is not a place to keep state.

A secret may also come from a file you name yourself, through `bot_token_file`,
`members[].bot_token_file` or `endpoints[].api_key_file`. The value is the file's
contents with the trailing newline trimmed, and a file that is group- or world-readable is
refused outright, with its mode in the message. What you may not do is name both a file
and a variable for the same secret: that is a startup error rather than a precedence,
because two sources mean one of them is a belief about where the value comes from that is
not true.

**One caveat while this is being finished.** kenward validates all three sources today,
but the parts that actually hand a token to a bot still read the environment variable. A
household supplying its token *only* by file or credential will pass validation and then
find no token where it is needed. Until that is plumbed through, keep `bot_token_env` and
`api_key_env` set, and treat the unit's `LoadCredential=` lines as where this is going.

### macOS

The same installer works:

```sh
curl -fsSL https://raw.githubusercontent.com/BlueHeisenberg/kenward/main/install.sh | sh
kenward setup
```

Or by hand: download `kenward_darwin_arm64` (or `_amd64` on Intel) and `checksums.txt`,
check it with `shasum -a 256 --check --ignore-missing checksums.txt`, `chmod +x`, and move
it onto your PATH.

Nothing here is notarised, so Gatekeeper will refuse the binary the first time. Either
right click, Open, and confirm, or clear the quarantine flag yourself:

```sh
xattr -d com.apple.quarantine /usr/local/bin/kenward
```

There is a Homebrew cask, and it clears the quarantine flag for you:

```sh
brew install --cask BlueHeisenberg/tap/kenward
```

It is generated by every release but is only pushed to the tap once
`HOMEBREW_TAP_TOKEN` and `HOMEBREW_TAP_SKIP` are set on the repository
(`docs/RELEASING.md`, "Homebrew"). Until they are, the command above fails and the
by-hand route two paragraphs up is the one that works.

### Windows

Download `kenward_windows_amd64.exe` (or `_arm64` on an ARM device) from the
[latest release](https://github.com/BlueHeisenberg/kenward/releases/latest), check it
against `checksums.txt`, rename it to `kenward.exe`, put it somewhere on your PATH, and run
it from a terminal. `kenward setup` works the same as everywhere else.

```powershell
$v = 'https://github.com/BlueHeisenberg/kenward/releases/latest/download'
Invoke-WebRequest "$v/kenward_windows_amd64.exe" -OutFile kenward.exe
Get-FileHash kenward.exe -Algorithm SHA256   # compare against checksums.txt
```

The install script is not for Windows: it is POSIX shell, and it says so rather than
half-working under Git Bash.

### The desktop wrapper

Optional, and only worth it if you want a tray icon rather than a terminal.
`kenward-desktop` supervises the daemon in your own session, shows its state in the
menu bar or system tray, and opens the dashboard in your browser. It ships **with** the
daemon inside it, so these are complete installs — nothing else to download, nothing to
put on `PATH`. Everything on this page above still applies if you would rather run
kenward headless, and most households should.

| | Artifact | Notes |
| --- | --- | --- |
| macOS | `kenward_<version>_macos.dmg` | Universal; drag `kenward.app` to Applications |
| Windows | `kenward_<version>_windows_amd64_setup.exe` | Per-user, no administrator; runs on ARM under emulation |
| Debian/Ubuntu | `kenward-desktop_<version>_amd64.deb`, `_arm64.deb` | `sudo dpkg -i …` |
| Fedora/RHEL | `kenward-desktop-<version>-1.x86_64.rpm`, `-1.aarch64.rpm` | `sudo rpm -i …` |

All four are in `checksums.txt` with everything else, and none of them is signed or
notarised. That has a visible consequence on two platforms:

- **macOS.** Double-clicking the app fails with a message that reads as though the
  download is damaged. The first launch has to be Finder → right-click → **Open** →
  **Open**; on macOS 15 and later you may also need System Settings → Privacy &
  Security → **Open Anyway**. Every launch after the first is ordinary. The `.dmg`
  carries `FIRST-LAUNCH.txt` saying this.
- **Windows.** SmartScreen shows "Windows protected your PC" and hides the run button
  behind **More info** → **Run anyway**. Separately, Windows 11 puts new tray icons in
  the overflow: the icon is behind the `^` chevron beside the clock until you drag it
  out.

On Linux the icon needs a tray, which stock GNOME has not had since 3.26 — install the
[AppIndicator extension](https://extensions.gnome.org/extension/615/appindicator-support/)
or run KDE, Xfce, Cinnamon, MATE or Ubuntu, where it works as shipped. The daemon is
supervised either way; only the icon is missing, and the program says so in its log
rather than leaving you to guess.

Two things the wrapper changes about updating. The daemon inside the `.app` or the
Windows install directory self-updates exactly as it does anywhere, and the wrapper
restarts it — that is what the wrapper is for. The daemon inside the `.deb` or `.rpm`
lives in `/usr/bin` and cannot write to itself, so it updates when you install a newer
package. And the wrapper itself never self-updates on any platform: download a newer
bundle. `docs/DESKTOP.md` has the rest.

### Container

The image is `ghcr.io/blueheisenberg/kenward`, published for linux/amd64 and linux/arm64
on every release. `:latest` follows the newest non-prerelease; a version tag
(`:v0.1.0`) pins one, and pinning is the better habit for something holding your
household's keys.

```sh
docker compose -f deploy/compose.simple.yml up -d
```

Edit the compose file first: it expects a config file and a data directory bind-mounted,
and the bot token supplied through a `.env` file beside it. The image does **not**
contain lore — bind-mount the binary or build a derived image, as the Dockerfile's own
comments explain.

---

## Isolated mode

One pod per member plus one for the household group, each with its own bot token, its
own key and its own lore instance. Nobody — including whoever runs the machine — can
read another member's private memory from disk, from a backup, or while that member is
away.

**This is Linux only**, and it needs Podman or Docker. The isolation *is* the container
runtime, so there is no meaningful equivalent on Windows or macOS. Setup will say so if
you pick it there rather than failing later.

```sh
kenward setup            # choose the sealed option
docker compose -f deploy/compose.isolated.yml up -d
```

Each member creates their own bot with BotFather and supplies that token to their own
pod. It is five extra minutes per person, and it is what stops the operator's single
token being able to read everyone's private conversations. A member's own bot serves
their private chat and never speaks in the group, so it needs no `/setprivacy` — the
household group's bot is the one that does.

The shipped `compose.isolated.yml` is a worked two-member example with comments showing
how to add a third. The services must not share a data volume; that sharing is exactly
what the mode exists to prevent.

### Letting kenward start the pods instead

There is a second path, and it manages the pods for you:

```sh
kenward run --config kenward.yaml --image localhost/kenward-with-lore:dev
```

`--image` is not optional in practice. The published image deliberately carries no lore,
and this path has no bind-mount to add one with, so build a derived image —
`FROM ghcr.io/blueheisenberg/kenward:<tag>` plus a `COPY` of a `lore` binary for that
image's OS and architecture — and name it here. Build that binary with
`CGO_ENABLED=0 GOOS=linux GOARCH=<amd64|arm64> go build -o lore ./cmd/lore`: the base is
distroless static and has no dynamic loader, so a default `go build` produces a binary
for the right platform that still fails as `exec /usr/local/bin/lore: no such file or
directory`, naming a file that is plainly there. Everything else is handled: the volumes,
the per-member secrets, each pod's own lore store, and rolling every pod onto a new image
after an update.

**Two things to know before you point it at a household.** A member whose `tiers:` reach
a cloud endpoint gets that endpoint's API key in their pod — they need it to route there
at all — and so does every other member whose chain reaches the same endpoint, because it
is one provider account and one key. If two people must not share a provider budget, give
them two endpoints with two `api_key_env`s. And a member whose chain reaches *no* endpoint
with a key gets none, which is the case you want for anyone local-only.

---

## Adding people

```sh
kenward invite --name "David"
```

prints a single-use code that expires in 24 hours. Give it to them; they message the bot
and send it. Until they do, the bot will not reply to them at all — not an error, not a
prompt, silence. That is deliberate: a bot username is publicly discoverable, and a reply
would confirm to a stranger that there is something here.

They will get a short explanation of the two memories on the way in.

---

## Checking it works

```sh
kenward doctor
```

reports configuration, memory, transport and every endpoint, and then prints what your
chosen mode actually promises about privacy — including, in simple mode, that the
operator can read every member's private memory.

A powered-off endpoint is reported as a fact, not a failure. `doctor` exits non-zero only
for configuration faults, an unreachable lore, or a Telegram authorisation failure.

---

## Updating

A running kenward checks for updates on `update.check_interval` and applies them itself:
the manifest is verified against a key compiled into the binary, the new build is run
once before it is installed, in-flight conversations are allowed to finish, the swap is
atomic, and a version that does not come up is rolled back automatically. Endpoint
reachability is deliberately not part of that health check — your GPU box being asleep is
normal, and treating it as a fault would roll a good update back forever.

**One case still needs you.** A major version, or a release flagged as changing
security-relevant behaviour — the settings that decide whether a private conversation may
reach a provider — is never applied without somebody agreeing. Asking you over Telegram
is the intended way and is not built yet, so the running node will not apply such a
release at all: it logs that it is holding back and keeps serving on the version it has.
Patch and minor releases carry on arriving normally. Until the question can reach you in
the chat, apply those yourself:

```sh
kenward update --check     # what is available; changes nothing
kenward update             # apply it, answering the question at the terminal
```

Answer `y` or `n`. Anything else, including no terminal at all, counts as unanswered
rather than declined — nothing is applied, and you are asked again next time. That is
deliberate: one unheard question must never suppress a security release permanently.

Set `update.channel: off` in the config if you would rather never update at all. That
path is fully supported and kenward works indefinitely without ever updating.

`docs/IMPLEMENTATION.md` section 9 records exactly which of the update requirements are
wired and which are still ahead, including why per-member pods cannot be rolled one at a
time from inside kenward.
