#!/bin/sh
# End-to-end exercise of install.sh. Runs INSIDE a throwaway Linux container —
# it moves system binaries about and installs into /usr/local/bin, so do not
# run it on a machine you care about. `task install:test` sets it up.
#
# It expects:
#   /mnt      the repository, read-only
#   /release  a directory of release assets: kenward_linux_amd64 + checksums.txt
#
# What it is for: install.sh is a couple of hundred lines of branching that
# nobody exercises until somebody pipes it into a shell on a machine that is
# not like the maintainer's. Every case below is one of those machines.
set -u

apt-get -qq update >/dev/null 2>&1
apt-get -qq install -y --no-install-recommends curl ca-certificates >/dev/null 2>&1
command -v curl >/dev/null || { echo "FAIL: no curl in the test container"; exit 1; }

export KENWARD_BASE_URL=file:///release
fail=0
check() {
	if [ "$1" = 0 ]; then
		echo "PASS: $2"
	else
		echo "FAIL: $2"
		fail=1
	fi
}

echo "=== 1. fresh install as root ==="
sh /mnt/install.sh --no-service || fail=1
kenward version | grep -q '^kenward '
check $? "installed and reports its version"
[ -x /usr/local/bin/kenward ]
check $? "landed in /usr/local/bin"
# Whatever this build calls itself is what the "already installed" checks pin.
installed="$(kenward version | awk '{print $2}')"

echo
echo "=== 2. re-run with that version pinned ==="
sh /mnt/install.sh --version "$installed" --no-service >/tmp/2.log 2>&1
grep -q 'Nothing to do' /tmp/2.log
check $? "recognises it is already installed and does nothing"

echo
echo "=== 3. --force reinstalls anyway ==="
sh /mnt/install.sh --version "$installed" --force --no-service >/tmp/3.log 2>&1
grep -q 'Checksum OK' /tmp/3.log
check $? "--force downloads again"

echo
echo "=== 4. a corrupted artifact is refused ==="
rm -f /usr/local/bin/kenward
mkdir -p /bad && cp /release/checksums.txt /bad/ && printf 'not a binary' >/bad/kenward_linux_amd64
KENWARD_BASE_URL=file:///bad sh /mnt/install.sh --no-service >/tmp/4.log 2>&1
[ $? -ne 0 ]
check $? "exits non-zero"
grep -q 'checksum mismatch' /tmp/4.log
check $? "says why"
[ ! -e /usr/local/bin/kenward ]
check $? "installed nothing"

echo
echo "=== 5. a release with no checksums.txt is refused ==="
mkdir -p /nosum && cp /release/kenward_linux_amd64 /nosum/
KENWARD_BASE_URL=file:///nosum sh /mnt/install.sh --no-service >/tmp/5.log 2>&1
[ $? -ne 0 ]
check $? "exits non-zero"
grep -q 'unverified binary' /tmp/5.log
check $? "refuses rather than installing something unchecked"

echo
echo "=== 6. unprivileged, no sudo, target not writable ==="
useradd -m tester >/dev/null 2>&1
su tester -c "KENWARD_BASE_URL=file:///release sh /mnt/install.sh --no-service" >/tmp/6.log 2>&1
check $? "succeeds anyway"
grep -q '/home/tester/.local/bin' /tmp/6.log
check $? "falls back to ~/.local/bin"
grep -q 'not on your PATH' /tmp/6.log
check $? "warns that the fallback is not on PATH"
[ -x /home/tester/.local/bin/kenward ]
check $? "the binary is there and executable"

echo
echo "=== 7. an explicitly chosen --dir is never silently overridden ==="
su tester -c "KENWARD_BASE_URL=file:///release sh /mnt/install.sh --dir /usr/local/bin --no-service" >/tmp/7.log 2>&1
[ $? -ne 0 ]
check $? "exits non-zero rather than installing somewhere else"
grep -q 'could not write to /usr/local/bin' /tmp/7.log
check $? "names the directory it could not write to"

echo
echo "=== 8. neither curl nor wget ==="
mv /usr/bin/curl /usr/bin/curl.off
sh /mnt/install.sh --no-service >/tmp/8.log 2>&1
st=$?
mv /usr/bin/curl.off /usr/bin/curl
[ "$st" -ne 0 ]
check $? "exits non-zero"
grep -q 'neither curl nor wget' /tmp/8.log
check $? "says what to install"

echo
echo "=== 9. an architecture with no build ==="
mkdir -p /fake
printf '#!/bin/sh\nif [ "$1" = -m ]; then echo mips64; else /usr/bin/uname "$@"; fi\n' >/fake/uname
chmod +x /fake/uname
PATH=/fake:$PATH sh /mnt/install.sh --no-service >/tmp/9.log 2>&1
[ $? -ne 0 ]
check $? "exits non-zero"
grep -q 'no build for mips64' /tmp/9.log
check $? "names the architecture rather than failing on a 404"

echo
echo '=== 10. --help through a pipe, where $0 is not a file ==='
cat /mnt/install.sh | sh -s -- --help >/tmp/10.log 2>&1
check $? "exits 0"
grep -q 'KENWARD_INSTALL_DIR' /tmp/10.log
check $? "prints the options"

echo
if [ "$fail" = 0 ]; then
	echo "ALL PASS"
else
	echo "SOME FAILED"
fi
exit "$fail"
