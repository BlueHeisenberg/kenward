# Installing kenward

> **Status: not yet installable.** kenward is under construction and there is no
> published binary or image. This document describes the intended install and is
> written down now so the build has something to be measured against.

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
shared space for the household plus one private space per member. kenward will tell you
if it cannot reach it.

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
curl -L https://github.com/BlueHeisenberg/kenward/releases/latest/download/kenward_linux_amd64 -o kenward
chmod +x kenward
sudo mv kenward /usr/local/bin/
kenward setup
```

To run it as a service, copy `deploy/kenward.service` into `/etc/systemd/system/`, adjust
the config path, then `systemctl enable --now kenward`. The unit ships with hardening
already applied and each line commented, so you can loosen one knowingly rather than
discovering it later.

### macOS

Download `kenward_darwin_arm64` (or `_amd64` on Intel), make it executable, and run
`kenward setup`. macOS will refuse to run an unnotarised binary the first time; right
click, Open, and confirm.

### Windows

Download `kenward_windows_amd64.exe` and run it from a terminal. `kenward setup` works
the same as everywhere else.

### Container

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

kenward updates itself: signed, verified against a key compiled into the binary, applied
atomically, health-checked afterwards, and rolled back automatically if the new version
does not come up. It waits for a quiet moment rather than restarting mid-conversation,
and a major version asks you first.

Set `update.channel: off` in the config if you would rather do it yourself. That path is
fully supported and kenward works indefinitely without ever updating.
