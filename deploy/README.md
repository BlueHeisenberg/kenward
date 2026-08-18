# Deploying kenward

Four ways to run kenward; pick one.

| File | Use when |
| --- | --- |
| [`compose.simple.yml`](compose.simple.yml) | Simple mode — one household bot, one container. Works on Windows, macOS, Linux. |
| [`compose.isolated.yml`](compose.isolated.yml) | Isolated mode — one container per member plus one for the group. Linux with Podman or Docker only. Normally generated from `kenward.yaml`; the checked-in copy is a worked two-member example. |
| [`kenward.service`](kenward.service) | Running the plain Linux binary under systemd instead of a container. Secrets load as systemd credentials, not environment variables — see below. |
| [`../Dockerfile`](../Dockerfile) | Building the image itself. Read its top comment first — it explains why `lore` is not inside it and how to supply one. |

Pick Simple unless you have a household member whose privacy from *the person
running the machine* matters more than the convenience of one process. See
`README.md`'s "Two modes, one binary" table for the honest trade-off.

## What you fill in

Every one of these files assumes `kenward.yaml` exists next to it — the
household's configuration (`docs/IMPLEMENTATION.md` §4), referenced by path
and never generated here — plus the actual secret values, supplied
differently depending on the path:

- **Compose (`compose.simple.yml`, `compose.isolated.yml`)**: a `.env` file
  next to the compose file, holding bot token(s) and any provider API key
  kenward.yaml's `bot_token_env` / `api_key_env` fields name — plus the session
  passphrase, which is not optional in either mode. Simple mode takes one node
  passphrase in `KENWARD_PASSPHRASE`, which `session.passphrase_env` in
  kenward.yaml should also name — the variable alone works, but only a named
  source is checked when the file loads, and "KENWARD_PASSPHRASE is not set" at
  startup beats discovering it from a restart loop. Isolated mode takes one per
  member, named by that member's `passphrase_env`, never one shared, because
  each member's key is wrapped under their own and that is what stops one
  container's compromise opening another member's memory. Leave it out and
  kenward refuses to start: a container has neither systemd's credentials nor a
  terminal to be prompted at, so the service restart-loops on exit 2 until the
  variable exists. Name a source only if it is really how the passphrase
  arrives — a named variable the deployment does not set is itself a refusal,
  which is why the systemd path below names none.
- **systemd (`kenward.service`)**: individual files under
  `/etc/kenward/credentials/`, one per secret, loaded via `LoadCredential=`
  — see that file's comments for the exact names.

kenward.yaml itself never contains a literal token or key, only the name of
an environment variable or a credential/file path that holds one.

**The two paths deliberately use different mechanisms, not out of
inconsistency:** systemd has `LoadCredential=`, which hands each secret to
the process as its own read-only file in a service-private, non-swappable
directory that disappears when the unit stops — invisible to `/proc` and not
silently inherited by a spawned child process. Containers have no equivalent
primitive that Compose can drive portably, so the compose files fall back to
environment variables, scoped as tightly as `environment:` blocks allow (see
below). If you want file-backed secrets in a container too, kenward's config
also accepts the `*_file` form (a path to a file holding the value) — pair it
with a secret mounted read-only at 0600, e.g. a Docker/Podman secret or a
bind-mounted file, rather than `environment:`.

**Neither compose file needs a lore store prepared, and `compose.simple.yml`
needs no lore binary at all.** kenward opens its store in process through lore's
Go API and creates it on the volume the first time the container starts, so
there is no `lore init` step in either path and none you should run by hand —
least of all against an isolated member's work volume, which is reachable from
nowhere else precisely so that a host cannot write into it and read it back. The
spaces `kenward.yaml` names were made by whichever wizard wrote that file.

`compose.isolated.yml` does still bind-mount a `lore` binary, and it is the last
place that needs one: each pod has its own store, and they exchange the
household's shared space by each running `lore serve --lan`. Build it with
`CGO_ENABLED=0` for the image's platform — see the Dockerfile's note on why it
is not baked into the image and why the static build is not optional. A pod
whose `memory.lore_command[0]` is not on `$PATH` is refused at startup with a
message about shared memory; a *simple* container without it is not refused,
because it runs nothing.

`compose.isolated.yml` additionally needs one file per member holding that
member's outstanding claim codes: run `kenward invite --name NAME` on the host
*before* bringing that member's service up, and it writes
`<data_dir>/invites/<id>.json` for the service to mount read-only. This is the
only route a code minted on the host takes to the container that has to redeem
it — the claim happens against the member's own bot, in that container, against
the invite store on that container's own volume. Bring the service up first and
your container runtime will create a *directory* where the file should be, and
kenward will refuse to start and say so.

Neither compose file uses `env_file:` — each service's `environment:` block
names exactly the variables it needs, interpolated from `.env` at parse time
rather than the whole file being injected. In `compose.isolated.yml` this is
**load-bearing, not tidiness**: each member's container receives only its own
bot token (and only the provider keys its own tier chain can reach), which is
what makes it true that no container can read another member's private
Telegram conversation. If you ever see `env_file:` re-added to that file,
that isolation has been undone — treat it as a regression, not a cleanup.
Even so, that token still sits in that member's own container's environment
— readable by anything with access to *that* container, which is a weaker
guarantee than systemd's credential files give the binary-install path; see
`compose.isolated.yml`'s header for the `*_file` alternative if that gap
matters for your household.

## Running a one-off command against a compose deployment

The image's `ENTRYPOINT` is the bare `kenward` binary, not `kenward run` — the
default `CMD` runs the node, but every other subcommand (`version`, `doctor`,
`invite`, `revoke`, `update`) is reachable too. That matters most for
`invite`, which is how an operator with a running household actually adds a
member; it needs the same config and data volume the running service already
has, so run it as a one-off against the same service rather than a bare
`docker run` with hand-copied mounts:

```
docker compose -f compose.simple.yml run --rm kenward invite --name David
```

`invite` and `revoke` need no secrets — one mints a digest and the other
clears a binding, and neither opens a bot, unwraps a key or calls a provider —
so both load `kenward.yaml` for its structure alone and run from a container
holding only its own member's variables. They did not always: unscoped, they
demanded every member's bot token and passphrase, and since no container in
Isolated mode holds a sibling's secrets — that is the mode, not an oversight —
every service failed the command on variables it correctly did not have:

```
kenward: /etc/kenward/kenward.yaml cannot be served (problems):
  - members[0].bot_token_env: environment variable KENWARD_BOT_TOKEN_DAVID is not set
  - members[0].passphrase_env: environment variable KENWARD_PASSPHRASE_DAVID is not set
```

**In Isolated mode, run them on the host anyway**, and for a reason that has
nothing to do with secrets: a code minted inside a service is written to *that
service's* named volume, which no other service and no host path can mount, so
it reaches nobody. Run them where `.env` already is, with `--data-dir` pointing
at the directory the compose file bind-mounts from — see
`compose.isolated.yml`'s header, step 5 and "TO REVOKE A MEMBER LATER".

On an SELinux-enforcing host there is a second reason. Every bind mount in both
compose files carries `z` or `Z`, without which a container may not read the
host file at all; the per-member invite and revocation files use `Z`, which is
private to one container, and a `compose run` against that member's service
inherits those mounts and relabels them to the throwaway container — leaving the
running one locked out of its own invites. `compose.isolated.yml`'s header sets
out which option each mount gets and why.

`run` here reuses that service's image, volumes and
environment, so the claim code lands in the same data directory and store
the running node reads from. Don't use `docker compose exec` instead unless
the service is already up — `exec` runs inside the *existing* container's
process, while `run` starts a fresh, short-lived one from the same
definition, which is what you want for a command that isn't the long-running
node itself.

## The one warning that matters

**`kenward.yaml`, `.env`, the files under `invites/`, and the files under
`/etc/kenward/credentials/`
hold real secrets — Telegram bot tokens, provider API keys, and (in
kenward.yaml) the shape of a real household's private/shared space split.
Never commit any of them, anywhere, under any name.** `.gitignore` and
`.dockerignore` both already block the obvious filenames (`kenward.yaml`,
`.env`, `*.token`, `*.key`, `*.pem`, `/data/`, `/state/`), but a rename or a
copy under a different path defeats that — check `git status` before
committing when you've touched deploy configuration. If a secret does land
in git history, rotating it (new bot token, new API key) is the fix;
deleting the file afterwards is not.
