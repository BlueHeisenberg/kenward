# syntax=docker/dockerfile:1.7

# kenward container image
#
# Two-stage build: compile a static binary on the official golang image, then
# copy just that binary onto a distroless base that carries nothing else.
#
# ---------------------------------------------------------------------------
# IMPORTANT — lore is NOT in this image.
#
# kenward's only route to its memory layer is spawning `lore mcp` as a
# subprocess (kenward.yaml: memory.lore_command, default ["lore", "mcp"]).
# Without a `lore` binary on $PATH inside the container, kenward starts but
# has no memory: no retrieval, no capture, no enrolment history. This image
# deliberately does not bundle it — lore is a sibling project with its own
# release cadence, and baking a copy in here would pin kenward to whatever
# lore version happened to be current at image build time.
#
# Operators supply it one of two ways:
#   1. Bind-mount a `lore` binary built for this image's OS/arch to
#      /usr/local/bin/lore (read-only). See deploy/compose.simple.yml and
#      deploy/compose.isolated.yml for the exact volume line.
#   2. Build a derived image: `FROM ghcr.io/blueheisenberg/kenward:<tag>` and
#      `COPY` a `lore` binary to /usr/local/bin/lore.
# Either way the binary must match this image's platform (linux/amd64 or
# linux/arm64) — a host-built lore binary will not run unmodified inside the
# container.
#
# Remedy 1 is not available to a pod started by `kenward run` in isolated mode:
# keel's sandbox.Spec has no host bind-mount, so the host supervisor has nothing
# to mount with, and remedy 2 with `--image` is the only route there. Since the
# default for --image is this image, which has no lore, a supervisor-started
# household needs a derived image or it has no memory. `kenward run` refuses to
# start a unit whose memory.lore_command is not on PATH rather than serving one
# that quietly records nothing — see docs/IMPLEMENTATION.md §8, "How a
# supervisor-started pod gets lore".
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
# memory.lore_command's LORE_HOME here) lore's own on-disk store all live
# under the data directory. Bind-mount or name-volume this in production —
# an ephemeral data dir means claim codes and lore's data disappear on
# container recreation.
VOLUME ["/var/lib/kenward"]

# Health = process up, config loads, lore MCP responds, Telegram authorises.
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
