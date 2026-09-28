#!/bin/sh
# agentsd installer: downloads a released binary, verifies its SHA-256
# checksum (and the cosign signature when cosign is available), and installs
# it to ~/.local/bin without sudo. It never edits shell rc files, never runs
# `agentsd init`, and never installs the service.
#
#   curl -fsSL https://raw.githubusercontent.com/Hunt4Bugs/agentsd/main/install.sh | sh
#   sh install.sh --version v0.1.0 --dir /usr/local/bin
#
# Environment: AGENTSD_VERSION, AGENTSD_INSTALL_DIR.
#
# Everything runs inside main(), so a download cut off part-way executes nothing.

set -eu

REPO="Hunt4Bugs/agentsd"

say() { printf 'agentsd-install: %s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

usage() {
	cat >&2 <<USAGE
usage: install.sh [--version vX.Y.Z] [--dir DIR]
  --version  release to install (default: latest; env AGENTSD_VERSION)
  --dir      install directory (default: ~/.local/bin; env AGENTSD_INSTALL_DIR)
USAGE
}

fetch() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https,file' -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "need curl or wget"
	fi
}

latest_version() {
	# github.com/<repo>/releases/latest redirects to .../releases/tag/<tag>.
	if command -v curl >/dev/null 2>&1; then
		url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$BASE/latest") || return 1
	else
		url=$(wget -S --spider "$BASE/latest" 2>&1 | sed -n 's/^ *Location: *//p' | tail -n 1 | tr -d '\r') || return 1
	fi
	tag=${url##*/}
	case "$tag" in v[0-9]*) printf '%s\n' "$tag" ;; *) return 1 ;; esac
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "need sha256sum or shasum"
	fi
}

main() {
	version="${AGENTSD_VERSION:-}"
	dir="${AGENTSD_INSTALL_DIR:-$HOME/.local/bin}"
	while [ $# -gt 0 ]; do
		case "$1" in
		--version) [ $# -ge 2 ] || die "--version needs a value"; version="$2"; shift 2 ;;
		--dir) [ $# -ge 2 ] || die "--dir needs a value"; dir="$2"; shift 2 ;;
		-h | --help) usage; exit 0 ;;
		*) usage; die "unknown argument: $1" ;;
		esac
	done

	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported OS $(uname -s) (agentsd supports macOS and Linux)" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m) (agentsd supports amd64 and arm64)" ;;
	esac
	command -v tar >/dev/null 2>&1 || die "need tar"

	# AGENTSD_BASE_URL exists for the installer's own tests (mock releases).
	BASE="${AGENTSD_BASE_URL:-https://github.com/$REPO/releases}"
	if [ -z "$version" ]; then
		version=$(latest_version) || die "could not resolve the latest release; pass --version"
	fi
	case "$version" in v*) ;; *) version="v$version" ;; esac
	num=${version#v}

	tarball="agentsd_${num}_${os}_${arch}.tar.gz"
	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t agentsd)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "downloading agentsd $version ($os/$arch)"
	fetch "$BASE/download/$version/$tarball" "$tmp/$tarball" || die "download failed: $BASE/download/$version/$tarball"
	fetch "$BASE/download/$version/checksums.txt" "$tmp/checksums.txt" || die "could not download checksums.txt"

	expected=$(awk -v f="$tarball" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt")
	[ -n "$expected" ] || die "$tarball is not listed in checksums.txt"
	actual=$(sha256 "$tmp/$tarball")
	[ "$expected" = "$actual" ] || die "checksum mismatch for $tarball (expected $expected, got $actual); aborting"
	say "checksum verified"

	if command -v cosign >/dev/null 2>&1; then
		fetch "$BASE/download/$version/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" ||
			die "cosign is installed but the signature bundle could not be downloaded"
		cosign verify-blob \
			--certificate-identity "https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$version" \
			--certificate-oidc-issuer https://token.actions.githubusercontent.com \
			--bundle "$tmp/checksums.txt.sigstore.json" "$tmp/checksums.txt" >/dev/null 2>&1 ||
			die "signature verification failed; aborting"
		say "signature verified (cosign)"
	else
		say "cosign not found; skipped signature verification (checksum still verified)"
	fi

	mkdir -p "$tmp/x"
	tar -xzf "$tmp/$tarball" -C "$tmp/x"
	[ -f "$tmp/x/agentsd" ] || die "archive does not contain agentsd"

	mkdir -p "$dir" || die "cannot create $dir"
	# Write alongside, then rename: atomic, safe over a running daemon.
	staged="$dir/.agentsd.$$"
	cp "$tmp/x/agentsd" "$staged" || die "cannot write to $dir"
	chmod 0755 "$staged"
	mv -f "$staged" "$dir/agentsd"
	say "installed $dir/agentsd"

	case ":$PATH:" in
	*":$dir:"*) ;;
	*)
		say "$dir is not on your PATH. Add this to your shell profile:"
		# shellcheck disable=SC2016 # $PATH is meant literally in the hint
		printf '\n    export PATH="%s:$PATH"\n\n' "$dir" >&2
		;;
	esac

	cat >&2 <<NEXT
Next steps:
  agentsd init      # create config, state dirs, and an example agent
  agentsd doctor    # check paths, runtimes, and the socket
  agentsd apply     # create the AHS roots and register agents
  agentsd service install
NEXT
}

main "$@"
