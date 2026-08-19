# syntax=docker/dockerfile:1.7

# kenward container image
#
# Two-stage build: compile a static binary on the official golang image, then
# copy just that binary onto a distroless base that carries nothing else.
#
# ---------------------------------------------------------------------------
# lore IS in this image, and this is the part to read before changing anything.
#
# It is here for ONE reason, and it is not anything kenward runs. Read that before
# adding a second use or removing it.
#
# kenward reaches lore as a Go library. Opening a store, `lore init`, creating a
# space, every read and every write are library calls in this process
# (internal/memory, lore.Init, (*Store).CreateSpace), and as of lore v0.5.0 so is
# the sync daemon — (*Store).Serve runs it in-process on a store kenward already
# has open. kenward execs nothing.
#
# THE REASON IS NOW A FALLBACK RATHER THAN THE ONLY ROUTE, and if you are here to
# decide whether lore can leave this image, that is the change to weigh.
#
# The one step with no Go API used to be the one that makes a shared space SHARED
# across two stores: the membership handshake. (*Store).CreateSpace mints a fresh
# id in the store that calls it, so two pods each creating "household" end up with
# two different spaces that will never converge, and making one space span pods was
#
#     lore space invite <space-id> --lan --yes     # in the pod that has it
#     lore join <code> --yes                       # in each of the others
#
# with lore exporting neither. lore v0.7.0 exports GrantMembership,
# AcceptMembership and PublicIdentity — the same admission with the part that
# authenticates a stranger removed, usable only by a caller that already holds the
# owner's home or the grantee's — and kenward's internal/link uses them: the
# group's pod answers, each member's pod asks, both prove they hold
# household.link_key, and no command is run inside any container. lore's position
# is unchanged and kenward still agrees with it: a program must not decide to share
# one person's memory with another. Carrying out a decision an administrator
# already wrote into kenward.yaml is a different act.
#
# So the invite and the join are the fallback for a household that configures no
# link key — every household deployed before v0.7.0, which must keep working — and
# they still run inside a pod, and this image is still distroless: no shell, no
# coreutils, nothing to run them with. The only way `docker compose exec
# kenward-david /usr/local/bin/lore space invite …` can work is if this image
# carries lore.
#
# When lore may leave: when no supported household is still on the manual recipe.
# Not before. A household that upgrades into an image without lore, having never
# set a link key, has no route to shared memory at all and no way to get one from
# inside the container.
#
# It used to be left to the operator, bind-mounted from the host, on the reasoning
# that lore is a sibling project with its own release cadence and baking a copy in
# would pin kenward to whatever lore was current at image build time. The pinning
# worry is answered by building it here rather than downloading it:
#
#     go build github.com/BlueHeisenberg/lore/cmd/lore
#
# is resolved by this module's own go.mod, so the CLI is built from precisely the
# lore version kenward's library half is compiled against. Nothing to keep in step
# by hand, and a store provisioned by this binary and opened by kenward is
# provisioned and opened by one version of lore.
#
# What makes that build line work is `tool github.com/BlueHeisenberg/lore/cmd/lore`
# in go.mod: cmd/lore imports packages kenward's own code does not, and without the
# tool directive `go mod tidy` drops their go.sum entries and this build fails with
# "missing go.sum entry". Delete that directive and this stage fails loudly rather
# than silently shipping something stale — which is the point, and is the switch to
# pull the day the fallback above is retired and this whole block can go.
#
# CGO_ENABLED=0 for the lore build is not optional and is not belt-and-braces. The
# final stage below is gcr.io/distroless/static-debian12, which has no dynamic
# loader at all, so a stock `go build` produces a dynamically linked binary that
# matches the platform, is executable, and still dies as:
#
#     exec /usr/local/bin/lore: no such file or directory
#
# naming a file that is plainly there. Measured, both ways: default build ->
# dynamically linked -> that error; CGO_ENABLED=0 -> static -> works. The ENV
# below covers both builds in this stage, so it cannot be set for one and missed
# for the other.
#
# /usr/local/bin is on this base image's PATH — checked, `--entrypoint lore` runs
# it — so the compose files' `exec … lore` lines work with or without the absolute
# path. They keep the absolute path anyway, because an exec-form ENTRYPOINT does no
# shell lookup and the surrounding documentation is easier to follow when every
# invocation names the same file.
#
# A household that never shares a space needs none of this. Private memory is one
# store in one pod, and it has never touched the binary.
# ---------------------------------------------------------------------------

# ---- builder ----
FROM golang:1.25.0-bookworm AS builder

WORKDIR /src

# Module download is its own layer: it only invalidates when go.mod/go.sum
# change, not on every source edit.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build-time version metadata, injected the same way Taskfile.yml's `build`
# and `cross` tasks do (see internal/version.Version/Commit/Date). Leave the
# defaults as-is for a local `docker build`; CI overrides them with
# --build-arg to match `git describe`.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# TARGETOS/TARGETARCH are populated automatically by BuildKit/buildx for
# multi-platform builds (`docker buildx build --platform linux/amd64,linux/arm64`).
ARG TARGETOS
ARG TARGETARCH

ENV CGO_ENABLED=0
# Belt and braces: even if a workspace file reaches the context, ignore it.
ENV GOWORK=off

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags "-X github.com/BlueHeisenberg/kenward/internal/version.Version=${VERSION} \
    -X github.com/BlueHeisenberg/kenward/internal/version.Commit=${COMMIT} \
    -X github.com/BlueHeisenberg/kenward/internal/version.Date=${DATE}" \
    -o /out/kenward \
    ./cmd/kenward

# The lore CLI, for the membership handshake and nothing else — see the block at
# the top. An import path in a dependency module, not a directory in this
# repository, and that is what makes it the version go.mod requires. `-s -w` and no
# -X: kenward's version variables do not exist in lore's main package, and stamping
# lore with kenward's tag the day it grows a `main.version` would be a lie that
# nothing would catch.
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags "-s -w" \
    -o /out/lore \
    github.com/BlueHeisenberg/lore/cmd/lore

# lore's own licence, out of the module cache, so the image carries the licence of
# the exact lore it carries. Same holder and the same BSL 1.1 as kenward's, but
# the two texts differ in Licensed Work, Change Date and Additional Use Grant, so
# one cannot stand in for the other. The module cache is read-only; chmod after.
RUN cp "$(go list -m -f '{{ .Dir }}' github.com/BlueHeisenberg/lore)/LICENSE" /out/LICENSE.lore && \
    chmod 0644 /out/LICENSE.lore && cp LICENSE /out/LICENSE

# ---- final ----
# distroless "static" + "nonroot": CA certificates and an /etc/passwd entry
# for a fixed, non-root uid/gid (65532:65532) and nothing else — no shell, no
# package manager, no coreutils. kenward makes outbound HTTPS calls to
# Telegram and to cloud providers, so the CA bundle is the one thing besides
# the binary itself that this image actually needs.
FROM gcr.io/distroless/static-debian12:nonroot AS final

# Fixed, well-known non-root identity (baked into the distroless base, not
# assigned here): uid=65532 gid=65532, user "nonroot". If a bind-mounted data
# directory on the host needs to match, chown it to 65532:65532.
USER nonroot:nonroot

WORKDIR /var/lib/kenward

COPY --from=builder /out/kenward /usr/local/bin/kenward

# The lore CLI. Nothing in this image executes it: it is here so an operator can,
# for the one provisioning step lore exposes no Go API for. See the top of the file.
COPY --from=builder /out/lore /usr/local/bin/lore

# Both licences, because this image now redistributes two programs. Same licence
# family and the same copyright holder — both BSL 1.1, both David Perez
# (BlueHeisenberg) — so this is redistribution of one's own work, but the two texts
# differ in Licensed Work, Change Date and Additional Use Grant and neither can
# stand in for the other.
COPY --from=builder /out/LICENSE /out/LICENSE.lore /usr/share/doc/kenward/

# Default config path and data directory. Override by passing different
# arguments after the image name (see compose files in deploy/), not by
# rebuilding.
ENV KENWARD_CONFIG=/etc/kenward/kenward.yaml
ENV KENWARD_DATA_DIR=/var/lib/kenward

# Session keys, enrolment claim-code state, and (when the operator points
# LORE_HOME here) lore's own on-disk store all live
# under the data directory. Bind-mount or name-volume this in production —
# an ephemeral data dir means claim codes and lore's data disappear on
# container recreation.
VOLUME ["/var/lib/kenward"]

# Health = process up, config loads, the lore store answers, Telegram authorises.
# Deliberately does NOT probe any routing endpoint tier: household machines
# are legitimately powered off, and treating that as unhealthy would restart
# a perfectly healthy container in a loop (see docs/IMPLEMENTATION.md §9).
#
# No --member/--group here, deliberately, and it is not an omission: this is a
# second process with no arguments of its own, so it takes the unit from
# KENWARD_MEMBER / KENWARD_GROUP in the container's environment — which is
# exactly where an isolated pod's identity already lives, whether the pod was
# started by the host supervisor (which sets those variables) or by a compose
# file (deploy/compose.isolated.yml sets them alongside the `--member` in
# `command:`, for this reason). `doctor` then reports on that one unit: in an
# isolated pod it authorises that unit's bot token and probes that unit's lore
# spaces, and nothing else. It must not do more — a member's container holds
# only that member's token (D-007), so a household-wide check inside it would
# fail on every sibling secret the container correctly does not have, and
# restart a perfectly good pod forever. In Simple mode neither variable is set
# and the check is household-wide, as it should be.
#
# Absolute path, not a bare "kenward": distroless's PATH is not guaranteed to
# include /usr/local/bin, and exec-form HEALTHCHECK/ENTRYPOINT do no shell
# lookup to fall back on.
#
# PODMAN: this instruction is Docker-format only, and podman's default output
# format is OCI, which has no place to put it. A `podman build` of this file
# prints, once per stage:
#
#     HEALTHCHECK is not supported for OCI image format and will be ignored
#
# and produces an image with no healthcheck at all. Isolated mode supports both
# runtimes, so say what to do rather than let half the supported surface run
# unchecked:
#
#   * building with podman: pass `--format docker` and the healthcheck is kept.
#   * running an OCI image anyway: `podman run --health-cmd=...` supplies one
#     per container, and deploy/compose.*.yml declare their own `healthcheck:`
#     so a compose deployment is covered on either runtime.
#   * pods started by `kenward run` in isolated mode use none of this: the host
#     supervisor health-checks its own pods (internal/supervisor/health.go), and
#     that path is runtime-independent.
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/kenward", "doctor", "--config", "/etc/kenward/kenward.yaml", "--data-dir", "/var/lib/kenward"]

# ENTRYPOINT is the bare binary, not "kenward run": pinning the subcommand
# here would make every other one unreachable, since anything passed after
# the image name becomes a positional argument to whatever ENTRYPOINT names
# rather than a replacement for it — `docker run <image> version` would try
# to run "version" as a bogus argument to `run` instead of invoking the
# version subcommand. CMD supplies the default full command (run against the
# standard config/data paths), so a bare `docker run <image>` still starts
# the node, while `docker run <image> version`, `... doctor ...`, or
# `... invite --name David` all reach cmd/kenward's other subcommands
# directly. The one cost: overriding just the config path now means
# repeating `run` (`docker run <image> run --config /other/kenward.yaml`)
# rather than only the flag — a fair trade for keeping every subcommand
# reachable.
ENTRYPOINT ["/usr/local/bin/kenward"]
CMD ["run", "--config", "/etc/kenward/kenward.yaml", "--data-dir", "/var/lib/kenward"]
