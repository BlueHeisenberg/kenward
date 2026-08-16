#!/bin/sh
# kenward installer.
#
#   curl -fsSL https://raw.githubusercontent.com/BlueHeisenberg/kenward/main/install.sh | sh
#
# It downloads one binary, checks it against the SHA-256 published with the
# release, puts it somewhere on your PATH, and offers to install the systemd
# unit. It starts nothing and enables nothing: kenward cannot run without a
# config file and a bot token, and a service that fails on install teaches
# people to ignore failed services.
#
# What it does NOT do: verify the release signature. The Ed25519 signature on
# a kenward release covers the update manifest, and checking it needs an
# Ed25519 verifier, which a POSIX shell has not got. So the checksum below
# proves you got the file GitHub published and nothing more; it is not
# protection against a compromised GitHub account. If that matters to you,
# check the signed manifest yourself with `kenward-release verify`.
#
# Options, as flags (curl ... | sh -s -- --version v0.1.0) or environment
# variables:
#
#   --version V     KENWARD_VERSION       tag to install, or "latest" (default)
#   --dir D         KENWARD_INSTALL_DIR   where to put it (default /usr/local/bin)
#   --force         KENWARD_FORCE=1       reinstall even if that version is there
#   --no-service    KENWARD_NO_SERVICE=1  skip the systemd question
#   --help

set -eu

REPO="BlueHeisenberg/kenward"
RAW_BASE="https://raw.githubusercontent.com/$REPO"

VERSION="${KENWARD_VERSION:-latest}"
INSTALL_DIR="${KENWARD_INSTALL_DIR:-/usr/local/bin}"
FORCE="${KENWARD_FORCE:-0}"
NO_SERVICE="${KENWARD_NO_SERVICE:-0}"
# Undocumented on purpose: set by the test that runs this script against a
# locally built snapshot instead of a published release.
BASE_URL="${KENWARD_BASE_URL:-}"

# Whether the install directory was named rather than defaulted. A directory
# somebody chose is not a suggestion, and quietly installing somewhere else is
# how a machine ends up with two kenwards.
if [ -n "${KENWARD_INSTALL_DIR:-}" ]; then DIR_WAS_CHOSEN=1; else DIR_WAS_CHOSEN=0; fi

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf 'kenward install: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
kenward installer.

  curl -fsSL https://raw.githubusercontent.com/BlueHeisenberg/kenward/main/install.sh | sh

  --version V     KENWARD_VERSION       tag to install, or "latest" (default)
  --dir D         KENWARD_INSTALL_DIR   where to put it (default /usr/local/bin)
  --force         KENWARD_FORCE=1       reinstall even if that version is there
  --no-service    KENWARD_NO_SERVICE=1  skip the systemd question
  --help

Pass flags through a pipe with `sh -s --`:

  curl -fsSL .../install.sh | sh -s -- --dir "$HOME/.local/bin" --no-service
EOF
	exit 0
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
	--dir)     [ $# -ge 2 ] || die "--dir needs a value"; INSTALL_DIR="$2"; DIR_WAS_CHOSEN=1; shift 2 ;;
	--force)      FORCE=1; shift ;;
	--no-service) NO_SERVICE=1; shift ;;
	-h|--help)    usage ;;
	*) die "unknown option $1 (try --help)" ;;
	esac
done

# --- what are we running on -------------------------------------------------

uname_s="$(uname -s)"
case "$uname_s" in
Linux)  GOOS=linux ;;
Darwin) GOOS=darwin ;;
MINGW*|MSYS*|CYGWIN*|Windows_NT)
	die "this script is for Linux and macOS. On Windows, download
  https://github.com/$REPO/releases/latest/download/kenward_windows_amd64.exe
  (or _arm64.exe), put it somewhere on your PATH, and run \`kenward setup\`." ;;
*)
	die "kenward has no build for $uname_s. Supported: Linux and macOS, on
  x86-64 and arm64. Build from source if you need another one:
  GOWORK=off go build ./cmd/kenward" ;;
esac

uname_m="$(uname -m)"
case "$uname_m" in
x86_64|amd64)         GOARCH=amd64 ;;
aarch64|arm64|armv8*) GOARCH=arm64 ;;
*)
	die "kenward has no build for $uname_m — only x86-64 and arm64.
  Build from source: GOWORK=off go build ./cmd/kenward" ;;
esac

ASSET="kenward_${GOOS}_${GOARCH}"

# --- how do we fetch things -------------------------------------------------

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "neither curl nor wget is installed, so there is nothing here to
  download with. Install one of them, or fetch the binary by hand from
  https://github.com/$REPO/releases/latest"
fi

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	else
		return 1
	fi
}

# verify_checksum FILE ASSET_NAME
#
# Checks FILE against the line for ASSET_NAME in the release's checksums.txt,
# and dies if it cannot. Every file this script writes outside $tmp goes
# through here first — the binary and the systemd unit alike. The unit decides
# what runs as root, so "it is only a config file" is exactly backwards: it is
# the one file where an unverified byte becomes a root command.
#
# A missing entry, an absent checksums.txt and a missing hasher are all
# refusals, never warnings: there is no version of "install it anyway" that is
# safe here.
verify_checksum() {
	_file="$1"
	_asset="$2"
	_want="$(awk -v f="$_asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
	[ -n "$_want" ] || die "checksums.txt does not mention $_asset, so there is nothing
  to check it against. Refusing to install something unverified."
	_got="$(sha256_of "$_file")" || die "no sha256sum, shasum or openssl on this
  machine, so the download cannot be checked. Install one of them, or verify the
  file by hand against $BASE_URL/checksums.txt"
	if [ "$_got" != "$_want" ]; then
		die "checksum mismatch on $_asset.
  expected $_want
  got      $_got
  Either the download was corrupted, or what was served is not what was
  published. Nothing further was installed."
	fi
}

if [ -z "$BASE_URL" ]; then
	if [ "$VERSION" = latest ]; then
		BASE_URL="https://github.com/$REPO/releases/latest/download"
	else
		BASE_URL="https://github.com/$REPO/releases/download/$VERSION"
	fi
fi

# --- is it already here -----------------------------------------------------

existing="$(command -v kenward 2>/dev/null || true)"
if [ -n "$existing" ]; then
	existing_version="$("$existing" version 2>/dev/null | head -n1 || true)"
	[ -n "$existing_version" ] || existing_version="a build that will not report its version"
	say "Already installed: $existing_version"
	say "                   at $existing"
	# -F: a version is a literal, not a pattern. Without it "--version 1.2.3"
	# matches "1x2x3", and "--version '.*'" matches anything at all — and the
	# answer to a match here is "skip the install", which is the wrong way to
	# be wrong.
	if [ "$FORCE" != 1 ] && [ "$VERSION" != latest ] &&
		printf '%s' "$existing_version" | grep -qwF -- "$VERSION"; then
		say ""
		say "That is already $VERSION. Nothing to do — pass --force to reinstall it anyway."
		exit 0
	fi
	say ""
fi

# --- download and check -----------------------------------------------------

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t kenward)"
trap 'rm -rf "$tmp"' EXIT
trap 'rm -rf "$tmp"; exit 130' INT
trap 'rm -rf "$tmp"; exit 143' TERM

say "Downloading $ASSET ($VERSION)..."
fetch "$BASE_URL/$ASSET" "$tmp/$ASSET" || die "could not download $BASE_URL/$ASSET
  If you asked for a particular version, check the tag exists:
  https://github.com/$REPO/releases"

fetch "$BASE_URL/checksums.txt" "$tmp/checksums.txt" || die "the release has no
  checksums.txt, so the download cannot be checked. Refusing to install an
  unverified binary."

verify_checksum "$tmp/$ASSET" "$ASSET"
say "Checksum OK."

chmod +x "$tmp/$ASSET"

# --- put it somewhere -------------------------------------------------------

as_root() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi; }

# place prints the directory the binary ended up in, or fails.
place() {
	# Root can make the directory it was told to use; an unprivileged run
	# cannot, and must not be told it succeeded somewhere else.
	if [ "$(id -u)" = 0 ]; then
		mkdir -p "$INSTALL_DIR" 2>/dev/null || true
	fi

	if [ -d "$INSTALL_DIR" ] && [ -w "$INSTALL_DIR" ]; then
		if mv -f "$tmp/$ASSET" "$INSTALL_DIR/kenward"; then
			printf '%s' "$INSTALL_DIR"
			return 0
		fi
	fi

	if [ "$(id -u)" != 0 ] && command -v sudo >/dev/null 2>&1; then
		warn "$INSTALL_DIR is not writable; asking sudo to install there."
		if sudo install -d -m 0755 "$INSTALL_DIR" &&
			sudo install -m 0755 "$tmp/$ASSET" "$INSTALL_DIR/kenward"; then
			printf '%s' "$INSTALL_DIR"
			return 0
		fi
		warn "sudo could not install to $INSTALL_DIR."
	fi

	if [ "$DIR_WAS_CHOSEN" = 1 ]; then
		return 1
	fi

	fallback="$HOME/.local/bin"
	mkdir -p "$fallback" || return 1
	mv -f "$tmp/$ASSET" "$fallback/kenward" || return 1
	printf '%s' "$fallback"
}

dir="$(place)" || die "could not write to $INSTALL_DIR.
  Either run this with sudo, or choose a directory you own:
  curl -fsSL $RAW_BASE/main/install.sh | sh -s -- --dir \"\$HOME/.local/bin\""

bin="$dir/kenward"
say "Installed $("$bin" version | head -n1)"
say "          at $bin"

case ":$PATH:" in
*":$dir:"*) ;;
*)
	say ""
	warn "$dir is not on your PATH. Add this to your shell profile:"
	warn "    export PATH=\"$dir:\$PATH\""
	;;
esac

if [ -n "$existing" ] && [ "$existing" != "$bin" ]; then
	say ""
	warn "There is still an older kenward at $existing, and PATH may reach it first."
	warn "Remove it, or the version you just installed is not the one that runs."
fi

# --- offer the service ------------------------------------------------------

offer_service() {
	if [ "$NO_SERVICE" = 1 ] || [ "$GOOS" != linux ]; then return 0; fi
	command -v systemctl >/dev/null 2>&1 || return 0
	# Piped into sh, stdin is the script itself, so the question and the answer
	# both go to the terminal. No terminal means nobody to ask.
	[ -r /dev/tty ] || return 0

	say ""
	printf 'Install the systemd unit at /etc/systemd/system/kenward.service? [y/N] '
	read -r answer </dev/tty || return 0
	case "$answer" in
	y | Y | yes | YES) ;;
	*) say "Skipped."; return 0 ;;
	esac

	# From the release, next to the binary, and checked against the same
	# checksums.txt. It used to come from raw.githubusercontent.com/…/main —
	# whatever was on the branch at that instant, verified by nothing, written
	# into /etc/systemd/system as root. A unit file names what runs as root, so
	# it gets the binary's treatment, not a config file's.
	unit="$tmp/kenward.service"
	fetch "$BASE_URL/kenward.service" "$unit" || {
		warn "could not download the unit file; copy deploy/kenward.service by hand."
		return 0
	}
	verify_checksum "$unit" kenward.service
	# The unit ships with the default install path baked in. If the binary went
	# somewhere else, a unit pointing at /usr/local/bin fails on the first start
	# with a message about a missing executable.
	sed "s#^ExecStart=/usr/local/bin/kenward#ExecStart=$bin#" "$unit" >"$unit.out"

	as_root install -m 0644 "$unit.out" /etc/systemd/system/kenward.service || {
		warn "could not write /etc/systemd/system/kenward.service."
		return 0
	}
	as_root systemctl daemon-reload || true

	say ""
	say "Unit installed, and deliberately not started: kenward needs a config file"
	say "and a bot token first. Read the comments in the unit — every hardening line"
	say "is explained — then:"
	say ""
	say "    kenward setup                              # writes /etc/kenward/kenward.yaml"
	say "    sudo mkdir -p -m 0700 /etc/kenward/credentials"
	say "    # one file per secret, mode 0600, named on a LoadCredential= line"
	say "    sudo systemctl enable --now kenward"
}

offer_service

say ""
say "Next: kenward setup"
say "Then: kenward doctor"
say "Docs: https://github.com/$REPO/blob/main/docs/INSTALL.md"
