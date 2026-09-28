#!/bin/sh
# Tests install.sh against a mock release served from file:// URLs.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

case "$(uname -s)" in Darwin) os=darwin ;; *) os=linux ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; *) arch=arm64 ;; esac
version=v0.0.1
tarball="agentsd_0.0.1_${os}_${arch}.tar.gz"
rel="$work/releases/download/$version"
mkdir -p "$rel" "$work/pkg"
printf '#!/bin/sh\necho "agentsd v0.0.1"\n' >"$work/pkg/agentsd"
chmod +x "$work/pkg/agentsd"
(cd "$work/pkg" && tar -czf "$rel/$tarball" agentsd)
sum() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
printf '%s  %s\n' "$(sum "$rel/$tarball")" "$tarball" >"$rel/checksums.txt"

fail() { echo "FAIL: $*" >&2; exit 1; }
run() { # dir
	PATH="/usr/bin:/bin:/usr/sbin:/sbin" AGENTSD_BASE_URL="file://$work/releases" \
		AGENTSD_VERSION="$version" AGENTSD_INSTALL_DIR="$1" sh "$root/install.sh" 2>"$work/log"
}

# Happy path.
run "$work/bin" || { cat "$work/log"; fail "install failed"; }
[ "$("$work/bin/agentsd")" = "agentsd v0.0.1" ] || fail "installed binary does not run"
grep -q "checksum verified" "$work/log" || fail "no checksum message"
grep -q "not on your PATH" "$work/log" || fail "no PATH hint"
echo "ok: install"

# Reinstall over an existing binary.
run "$work/bin" || fail "reinstall failed"
echo "ok: reinstall"

# Checksum mismatch aborts and installs nothing.
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "$tarball" >"$rel/checksums.txt"
if run "$work/bin2"; then fail "install succeeded despite checksum mismatch"; fi
grep -q "checksum mismatch" "$work/log" || { cat "$work/log"; fail "no mismatch message"; }
[ ! -e "$work/bin2/agentsd" ] || fail "binary installed despite mismatch"
echo "ok: checksum mismatch aborts"

# A truncated script executes nothing.
head -c 2000 "$root/install.sh" >"$work/truncated.sh"
out=$(sh "$work/truncated.sh" 2>&1 || true)
case "$out" in *downloading*) fail "truncated script ran" ;; esac
echo "ok: truncated script is inert"
