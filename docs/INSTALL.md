# Installing kenward

kenward is one binary. Which mode you run is chosen during setup, by answering one
question about trust — see [ARCHITECTURE.md](ARCHITECTURE.md) if you want the reasoning
before you decide.

---

## Before you start

Three things, none of them kenward:

**A Telegram bot.** Message [@BotFather](https://t.me/BotFather), send `/newbot`, follow
two prompts, and keep the token it gives you. It looks like
`123456789:AAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx`. That token is a password — anyone
holding it can read every message sent to the bot.

**[lore](https://github.com/BlueHeisenberg/lore), running.** kenward owns no memory of
its own; it talks to lore over MCP. You need lore installed, initialised, and holding a
shared space for the household plus one private space per member. **Create them in lore
first** — kenward creates a space nowhere, not at setup and not when somebody claims an
invite.

Then run `lore spaces` and keep the **id** column. That is what goes in `shared_space`
and `private_space`, not the name you gave the space. lore does not enforce unique names,
so kenward keys everything on ids; put a name there and it will write memories quite
happily and return nothing on the first read. `kenward doctor` refuses a configuration
like that outright, which is the only reason it is a nuisance rather than a week of lost
captures.

**At least one inference endpoint.** Anything OpenAI-compatible: vLLM, llama.cpp,
Ollama, LM Studio, or a cloud provider. It does not need to be awake during setup —
kenward is built for machines that are usually asleep, and setup will say so rather than
refusing.

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

`--version v0.2.0` pins a release, `--force` reinstalls one you already have, `--help`
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
systemd unit in `/usr/lib/systemd/system/kenward.service`:

```sh
curl -fLO https://github.com/BlueHeisenberg/kenward/releases/latest/download/kenward_0.2.0_linux_amd64.deb
sudo dpkg -i kenward_0.2.0_linux_amd64.deb
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
the `lore mcp` subprocess, which has no business seeing a Telegram token. Put one file
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

Once `github.com/BlueHeisenberg/homebrew-tap` exists there will be a cask, and it clears
the flag for you:

```sh
brew install --cask BlueHeisenberg/tap/kenward
```

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

### Container

The image is `ghcr.io/blueheisenberg/kenward`, published for linux/amd64 and linux/arm64
on every release. `:latest` follows the newest non-prerelease; a version tag
(`:v0.2.0`) pins one, and pinning is the better habit for something holding your
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
token being able to read everyone's private conversations.

The shipped `compose.isolated.yml` is a worked two-member example with comments showing
how to add a third. The services must not share a data volume; that sharing is exactly
what the mode exists to prevent.

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
