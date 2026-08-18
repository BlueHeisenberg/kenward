# syntax=docker/dockerfile:1.7

# kenward container image
#
# Two-stage build: compile a static binary on the official golang image, then
# copy just that binary onto a distroless base that carries nothing else.
#
# ---------------------------------------------------------------------------
# lore is NOT in this image, and kenward does not need it to be.
#
# kenward imports lore and calls it. The store is opened in-process, a fresh
# home is initialised with lore.Init, spaces are made with Store.CreateSpace,
# and since lore v0.5.0 the sync daemon — the thing that carries a household's
# shared memory between pods and between machines — runs in kenward's own
# process too, on the store it already has open (internal/memory/sync.go). There
# is no mode in which this binary executes a `lore` command, and `kenward run`
# no longer refuses to start over one being absent. Measured: an isolated
# household of containers converges its shared space with no lore binary
# anywhere inside them.
#
# The one thing still wanting the command is not kenward's to do. Two lore homes
# exchange a space only when both hold its id and its key, and the only thing
# that grants that is the invite handshake — `lore space invite` on one side,
# `lore join` on the other. lore's Go API exposes neither, deliberately: who may
# read a household's memory is a person's decision. So an operator provisioning
# membership needs a `lore` binary reachable inside the pod, once per household
# and once per member, and never again:
#
#   1. Bind-mount a `lore` binary built for this image's OS/arch to
#      /usr/local/bin/lore (read-only). See deploy/compose.simple.yml and
#      deploy/compose.isolated.yml for the exact volume line and §4b for the
#      commands.
#   2. Or build a derived image: `FROM ghcr.io/blueheisenberg/kenward:<tag>` and
#      `COPY` a `lore` binary to /usr/local/bin/lore. This is the only route open
#      to a pod started by `kenward run` in isolated mode, which has no
#      bind-mount to offer; pass the derived image with --image.
#
# Either way the binary must match this image's platform (linux/amd64 or
# linux/arm64) AND be statically linked:
#
#     CGO_ENABLED=0 GOOS=linux GOARCH=<amd64|arm64> go build -o lore ./cmd/lore
#
# CGO_ENABLED=0 is not optional and is not belt-and-braces. The final stage below
# is gcr.io/distroless/static-debian12, which has no dynamic loader at all, so a
# stock `go build -o lore ./cmd/lore` produces a dynamically linked binary that
# matches the platform, is executable, and still dies as:
#
#     exec /usr/local/bin/lore: no such file or directory
#
# naming a file that is plainly there. Measured, both ways: default build ->
# dynamically linked -> that error; CGO_ENABLED=0 -> static -> works.
#
# A household that never shares a space needs none of this. Private memory is one
# store in one pod and has never touched the binary.
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
