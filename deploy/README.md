# Deploying kenward

Four ways to run kenward; pick one.

| File | Use when |
| --- | --- |
| [`compose.simple.yml`](compose.simple.yml) | Simple mode — one household bot, one container. Works on Windows, macOS, Linux. |
| [`compose.isolated.yml`](compose.isolated.yml) | Isolated mode — one container per member plus one for the group. Linux with Podman or Docker only. Normally generated from `kenward.yaml`; the checked-in copy is a worked two-member example. |
| [`kenward.service`](kenward.service) | Running the plain Linux binary under systemd instead of a container. |
| [`../Dockerfile`](../Dockerfile) | Building the image itself. Read its top comment first — it explains why `lore` is not inside it and how to supply one. |

Pick Simple unless you have a household member whose privacy from *the person
running the machine* matters more than the convenience of one process. See
`README.md`'s "Two modes, one binary" table for the honest trade-off.

## What you fill in

Every one of these files assumes two things exist next to it, and none of
them create either:

- **`kenward.yaml`** — the household's configuration (`docs/IMPLEMENTATION.md`
  §4). Referenced by path, never generated here.
- **`.env`** (compose) or **`/etc/kenward/kenward.env`** (systemd) — the
  actual secret values: bot token(s) and any provider API key kenward.yaml's
  `bot_token_env` / `api_key_env` fields name. kenward.yaml itself never
  contains a literal token or key, only the name of the environment variable
  that holds one.

For the container paths you also need a `lore` binary on the host to
bind-mount in — see the Dockerfile's note on why it isn't baked into the
image.

## The one warning that matters

**`kenward.yaml` and `.env` (or `kenward.env`) hold real secrets — Telegram
bot tokens, provider API keys, and (in kenward.yaml) the shape of a real
household's private/shared space split. Never commit either one, anywhere,
under any name.** `.gitignore` and `.dockerignore` both already block the
obvious filenames (`kenward.yaml`, `.env`, `*.token`, `*.key`, `*.pem`, `/data/`,
`/state/`), but a rename or a copy under a different path defeats that —
check `git status` before committing when you've touched deploy configuration.
If a secret does land in git history, rotating it (new bot token, new API
key) is the fix; deleting the file afterwards is not.
