# Installing kenward

kenward is one binary. Setup asks two questions:

- **Trust** — does everyone here trust whoever runs this machine to be able to read their
  private conversations? Yes gives you **simple mode**; no gives you **isolated mode**.
- **Agents** — one assistant for the whole household, or one each?

Decide both before you start: answering "one each" changes what you need in place first.
[ARCHITECTURE.md](ARCHITECTURE.md) has the reasoning.

---

## Before you start

**Nothing to install but kenward.** It imports
[lore](https://github.com/BlueHeisenberg/lore) as a Go module and opens its memory store
inside its own process. There is no lore binary to fetch, no `lore init` to run and no
space to make by hand — `kenward setup` creates the household's shared space and one
private space per person, and writes their ids into `kenward.yaml`. If this machine
already has a lore store, kenward adds its spaces to it and changes nothing else.

Three things to have ready, none of them a program — and a fourth if you are giving
everybody their own assistant.

### 1. A Telegram bot

Message [@BotFather](https://t.me/BotFather), send `/newbot`, follow two prompts. Keep the
token it gives you; it looks like `123456789:AAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx`.

That token is a password — anyone holding it can read every message sent to the bot.

### 2. Turn bot privacy mode off

Required if the household is going to use a group chat. In the same chat with BotFather:
send `/setprivacy`, choose that bot, choose **Disable**. Do it **before** adding the bot to
any group.

> **This is the one step whose omission has no symptom.** Telegram turns bot privacy mode
> on for every new bot, and with it on the bot receives **nothing** sent in a group — not
> plain messages, not a reply to it, not even an @mention. Only `/start@thebot` is
> delivered. So the bot sits in the family group and ignores everyone, with no error, no
> warning and no log line anywhere, because nothing ever arrives to log.
>
> Telegram applies the change only to groups the bot joins **afterwards**. A bot already in
> the group has to be removed and added again — flip the setting and then test in the group
> it is already in, and you will see nothing change and conclude the fix did not work.

`kenward setup` and the dashboard wizard both check this the moment they have the token,
and `kenward doctor` reports it afterwards.

### 3. At least one inference endpoint

Anything OpenAI-compatible: vLLM, llama.cpp, Ollama, LM Studio, or a cloud provider. It
does not need to be awake during setup — kenward is built for machines that are usually
asleep, and setup will say so rather than refusing.

### 4. Only for "one assistant each": the group chat id

Under `agents: per_member`, `household.group_chat_id` is required. Setup asks for it with
no default and no skip, and loops until you give it a number.

Have the household's Telegram group already made, with the bot already in it, and a message
already sent in it. Then, with the bot's token in place of `TOKEN`:

```
https://api.telegram.org/botTOKEN/getUpdates
```

Look for `"chat":{"id":-1001234567890` — that number, minus sign and all.

If nothing comes back: the message was sent before the bot joined, or privacy mode is still
on, or the bot was added to the group before you disabled it (remove it and add it again).

---

## Where the memory goes

Reference rather than a step.

- The store is a lore home: `$LORE_HOME` if you set one, otherwise `~/.lore`. Setup creates
  it if it is not there.
- Setup makes one **shared** space for the household and one per person, named after the
  household — "Casa — household", "Casa — David".
- Every space kenward configures is a shared-kind space, the private ones included: a
  member's private space has two members in it, the person and the node.
- The configuration names spaces by **id**, never by display name. Setup writes the ids;
  `kenward doctor` refuses a configuration that carries a name there.
- Running setup again over a household that already exists reuses the spaces of those names
  rather than making a second set, so `--force` corrects a typo without splitting anybody's
  memory in half.

---

## Simple mode

One process, one bot, everyone's memory separated but all keys in one place. Whoever runs
the machine can read everything. For most households that is fine — it is their own family
machine — and it is by far the easiest thing to run.

### Linux

```sh
curl -fsSL https://raw.githubusercontent.com/BlueHeisenberg/kenward/main/install.sh | sh
kenward setup
```

The script picks the right build for your machine, checks it against the SHA-256 published
with the release, installs it to `/usr/local/bin` (or `~/.local/bin` if it cannot write
there), and offers to install the systemd unit. It is short and commented; read it first if
you would rather —
[install.sh](https://github.com/BlueHeisenberg/kenward/blob/main/install.sh).

Options, through a pipe, take a `-s --`:

```sh
curl -fsSL https://raw.githubusercontent.com/BlueHeisenberg/kenward/main/install.sh \
  | sh -s -- --dir "$HOME/.local/bin" --no-service
```

`--version v0.1.0` pins a release, `--force` reinstalls one you already have, `--help`
lists the rest.

The script checks the checksum, not the release *signature* — verifying that needs an
Ed25519 verifier, which a shell has not got. `kenward update` does verify signatures,
against a key compiled into the binary.

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
systemd unit in `/usr/lib/systemd/system/kenward.service`. The filename carries the
version, so name the one you want — the
[releases page](https://github.com/BlueHeisenberg/kenward/releases) lists them:

```sh
V=0.1.0
curl -fLO "https://github.com/BlueHeisenberg/kenward/releases/download/v$V/kenward_${V}_linux_amd64.deb"
sudo dpkg -i "kenward_${V}_linux_amd64.deb"
```

There is no apt or yum repository, and no plan for one.

#### Running it as a service

After a script or by-hand install:

```sh
sudo cp deploy/kenward.service /etc/systemd/system/
# adjust the config path in the unit
sudo systemctl enable --now kenward
```

The installer's service offer does this for you, pointing `ExecStart=` at wherever the
binary landed and checking the unit against `checksums.txt` before writing it. The unit
ships hardened, with each line commented so you can loosen one knowingly.

Nothing enables or starts it for you: kenward needs a config file and a bot token first.

#### Secrets

The unit supplies secrets as **systemd credentials** rather than environment variables: an
`EnvironmentFile=` value stays in the process's environment for as long as it runs and is
readable through `/proc`. Put one file per secret under `/etc/kenward/credentials/` and
name each on a `LoadCredential=` line:

| File | Secret |
| --- | --- |
| `bot_token` | the household bot |
| `bot_token.<member id>` | that member's bot |
| `api_key.<endpoint name>` | that endpoint's API key |

kenward looks for exactly those names with no configuration at all; you do not repeat them
in `kenward.yaml`.

Leave the source files `root:root` mode `0600`: systemd reads them as PID 1, before it
drops to the unit's user, so they never need chowning even under `DynamicUser=`. The
directory kenward reads them back from is an ephemeral tmpfs that exists only while the
unit runs — nothing you put there survives a restart.

A secret may instead come from a file you name yourself, through `bot_token_file`,
`members[].bot_token_file` or `endpoints[].api_key_file`. The value is the file's contents
with the trailing newline trimmed, and a file that is group- or world-readable is refused
outright, with its mode in the message. Naming both a file and a variable for the same
secret is a startup error rather than a precedence.


### macOS

The same installer works:

```sh
curl -fsSL https://raw.githubusercontent.com/BlueHeisenberg/kenward/main/install.sh | sh
kenward setup
```

By hand: download `kenward_darwin_arm64` (or `_amd64` on Intel) and `checksums.txt`, check
it with `shasum -a 256 --check --ignore-missing checksums.txt`, `chmod +x`, and move it
onto your PATH.

Nothing here is notarised, so Gatekeeper will refuse the binary the first time. Either
right-click, Open, and confirm, or clear the quarantine flag yourself:

```sh
xattr -d com.apple.quarantine /usr/local/bin/kenward
```

There is a Homebrew cask, and it clears the quarantine flag for you:

```sh
brew install --cask BlueHeisenberg/tap/kenward
```

It is generated by every release but is only pushed to the tap once `HOMEBREW_TAP_TOKEN`
and `HOMEBREW_TAP_SKIP` are set on the repository (`docs/RELEASING.md`, "Homebrew"). Until
they are, the command above fails and the by-hand route above is the one that works.

### Windows

```powershell
$v = 'https://github.com/BlueHeisenberg/kenward/releases/latest/download'
Invoke-WebRequest "$v/kenward_windows_amd64.exe" -OutFile kenward.exe
Get-FileHash kenward.exe -Algorithm SHA256   # compare against checksums.txt
```

Use `_arm64` on an ARM device. Put `kenward.exe` somewhere on your PATH and run
`kenward setup` from a terminal; it works the same as everywhere else.

The install script is not for Windows: it is POSIX shell, and it says so rather than
half-working under Git Bash.

### The desktop wrapper

Optional, and only worth it if you want a tray icon rather than a terminal.
`kenward-desktop` supervises the daemon in your own session, shows its state in the menu
bar or system tray, and opens the dashboard in your browser. It ships **with** the daemon
inside it, so these are complete installs — nothing else to download, nothing to put on
`PATH`. Running kenward headless stays fully supported, and most households should.

| | Artifact | Notes |
| --- | --- | --- |
| macOS | `kenward_<version>_macos.dmg` | Universal; drag `kenward.app` to Applications |
| Windows | `kenward_<version>_windows_amd64_setup.exe` | Per-user, no administrator; runs on ARM under emulation |
| Debian/Ubuntu | `kenward-desktop_<version>_amd64.deb`, `_arm64.deb` | `sudo dpkg -i …` |
| Fedora/RHEL | `kenward-desktop-<version>-1.x86_64.rpm`, `-1.aarch64.rpm` | `sudo rpm -i …` |

All four are in `checksums.txt` with everything else, and none of them is signed or
notarised. That has a visible consequence on two platforms:

- **macOS.** Double-clicking the app fails with a message that reads as though the download
  is damaged; it is not. The first launch has to be Finder → right-click → **Open** →
  **Open**; on macOS 15 and later you may also need System Settings → Privacy & Security →
  **Open Anyway**. Every launch after the first is ordinary. The `.dmg` carries
  `FIRST-LAUNCH.txt` saying this.
- **Windows.** SmartScreen shows "Windows protected your PC" and hides the run button
  behind **More info** → **Run anyway**. Separately, Windows 11 puts new tray icons in the
  overflow: the icon is behind the `^` chevron beside the clock until you drag it out.

On Linux the icon needs a tray, which stock GNOME has not had since 3.26 — install the
[AppIndicator extension](https://extensions.gnome.org/extension/615/appindicator-support/)
or run KDE, Xfce, Cinnamon, MATE or Ubuntu, where it works as shipped. The daemon is
supervised either way; only the icon is missing, and the program says so in its log.

Updating differs in two ways. The daemon inside the `.app` or the Windows install directory
self-updates exactly as it does anywhere, and the wrapper restarts it. The daemon inside
the `.deb` or `.rpm` lives in `/usr/bin` and cannot write to itself, so it updates when you
install a newer package. The wrapper itself never self-updates on any platform: download a
newer bundle. `docs/DESKTOP.md` has the rest.

### Container

The image is `ghcr.io/blueheisenberg/kenward`, published for linux/amd64 and linux/arm64 on
every release. `:latest` follows the newest non-prerelease; a version tag (`:v0.1.0`) pins
one, and pinning is the better habit for something holding your household's keys.

```sh
docker compose -f deploy/compose.simple.yml up -d
```

Read the compose file's header first — the three steps are all in it. It expects
`kenward.yaml` and a data directory bind-mounted beside it, and the bot token supplied
through a `.env` file. There is nothing to supply for lore: kenward opens its store in
process and creates it on the data directory the first time the container starts.

**Get the directory right.** Run the setup wizard with its `--data-dir` and `LORE_HOME`
pointed at the directory the compose file mounts, because the wizard is what creates this
household's lore spaces and writes their ids into `kenward.yaml`. A simple-mode node
deliberately will not create them for itself, so a wizard that made them in one store and a
container that opens another gives you a node whose every configured space is missing. It
says so and refuses to start rather than serve with no memory, but the fix is at setup
time.

---

## Isolated mode

One pod per member plus one for the household group, each with its own bot token, its own
key and its own lore instance.

**This is Linux only**, and it needs Podman or Docker. The isolation *is* the container
runtime, so there is no meaningful equivalent on Windows or macOS. Setup will say so if you
pick it there rather than failing later.

```sh
kenward setup            # choose the sealed option
docker compose -f deploy/compose.isolated.yml up -d
```

What each member is told, in `internal/privacy`'s own words and printed in full by
`kenward doctor`, is this: *nobody else in the household can read your private memory, and
neither can the person who runs this machine — not from the disk, not from a backup, and
not before your process has been unlocked.*

That last clause is about being unlocked, not about being present. D-019 retired the older,
stronger wording: a key re-locked after a quiet spell could only be unwrapped again by a
passphrase sent over Telegram, and kenward will not teach that habit. So once a member's
assistant is unlocked, its key stays in that process's memory until it stops or they lock
it.

Each member creates their own bot with BotFather and supplies that token to their own pod.
It is five extra minutes per person, and it is what stops the operator's single token being
able to read everyone's private conversations. A member's own bot serves their private chat
and never speaks in the group, so it needs no `/setprivacy` — the household group's bot is
the one that does.

The shipped `compose.isolated.yml` is a worked two-member example with comments showing how
to add a third. The services must not share a data volume; that sharing is exactly what the
mode exists to prevent.

### Letting kenward start the pods instead

```sh
kenward run --config kenward.yaml
```

This manages the pods for you: the volumes, the per-member secrets, each pod's own lore
store, and rolling every pod onto a new image after an update. `--image` is optional and
only names a different build if you have one; the published image works as-is.

There is no manual step left, provided `household.link_key` is set — which `kenward setup`
does for you. Each pod creates its own store, so the id in `household.shared_space` starts
as one space in the group's pod only; each member's pod then asks the group's to admit it,
both prove they hold the link key, and the sync daemons converge from there. Adding a member
to a household that is already running needs nothing but the member's pod.

Leave the link key out and the old step comes back: `lore space invite` in the pod that
holds the space and `lore join` in each of the others, run inside the pods with the CLI the
image carries. `kenward doctor` says which of the two states a pod is in.

**Two things to know before you point it at a household.**

- A member whose `tiers:` reach a cloud endpoint gets that endpoint's API key in their pod —
  they need it to route there at all — and so does every other member whose chain reaches
  the same endpoint, because it is one provider account and one key. If two people must not
  share a provider budget, give them two endpoints with two `api_key_env`s.
- A member whose chain reaches *no* endpoint with a key gets none, which is the case you
  want for anyone local-only.

---

## Adding people

```sh
kenward invite --name "David"
```

prints a single-use code that expires in 24 hours. Give it to them; they message the bot and
send it. They get a short explanation of the two memories on the way in.

Until they send the code the bot will not reply to them at all — not an error, not a prompt,
silence. That is deliberate: a bot username is publicly discoverable, and a reply would
confirm to a stranger that there is something here.

---

## Checking it works

```sh
kenward doctor
```

reports configuration, memory, transport and every endpoint, and then prints what your
chosen mode actually promises about privacy — including, in simple mode, that the operator
can read every member's private memory.

A powered-off endpoint is reported as a fact, not a failure. `doctor` exits non-zero only
for configuration faults, an unreachable lore, or a Telegram authorisation failure.

---

## Updating

A running kenward checks for updates on `update.check_interval` and applies them itself:
the manifest is verified against a key compiled into the binary, the new build is run once
before it is installed, in-flight conversations finish, the swap is atomic, and a version
that does not come up is rolled back automatically. Endpoint reachability is deliberately
not part of that health check — your GPU box being asleep is normal.

**One case still needs you.** A major version, or a release flagged as changing
security-relevant behaviour — the settings that decide whether a private conversation may
reach a provider — is never applied without somebody agreeing. Asking you over Telegram is
the intended way and is not built yet, so the running node holds such a release back and
keeps serving on the version it has, logging that it did. Patch and minor releases carry on
arriving normally. Apply the held-back ones yourself:

```sh
kenward update --check     # what is available; changes nothing
kenward update             # apply it, answering the question at the terminal
```

Answer `y` or `n`. Anything else, including no terminal at all, counts as unanswered
rather than declined — nothing is applied, and you are asked again next time, so one
unheard question cannot suppress a security release permanently.

Set `update.channel: off` in the config if you would rather never update at all. That path
is fully supported and kenward works indefinitely without ever updating.

`docs/IMPLEMENTATION.md` section 9 records which update requirements are wired and which
are still ahead, including why per-member pods cannot be rolled one at a time from inside
kenward.
