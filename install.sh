#!/bin/sh
# ModelFabric installer.
#
#   curl -fsSL https://modelfabric.sh/install.sh | sh
#
# Downloads the prebuilt binary for this machine, verifies it against the
# release's SHA-256 checksums, and installs it. POSIX sh on purpose: this runs
# on a fresh box before anything else is there.
#
# Environment:
#   MFSH_VERSION      release tag to install (default: the latest release)
#   MFSH_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   MFSH_BASE_URL     release host, for a mirror or a private build

set -eu

REPO="gregbugaj/modelfabric"
BIN="mfsh"
VERSION="${MFSH_VERSION:-}"
INSTALL_DIR="${MFSH_INSTALL_DIR:-$HOME/.local/bin}"
BASE_URL="${MFSH_BASE_URL:-https://github.com/$REPO/releases/download}"

# Colour only when stdout is a terminal, matching the CLI's own rule.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	B=$(printf '\033[1m'); DIM=$(printf '\033[2m'); R=$(printf '\033[31m')
	G=$(printf '\033[32m'); Y=$(printf '\033[33m'); X=$(printf '\033[0m')
else
	B=''; DIM=''; R=''; G=''; Y=''; X=''
fi

say()  { printf '%s\n' "$*"; }
info() { printf '%s\n' "${DIM}$*${X}"; }
warn() { printf '%s\n' "${Y}warning:${X} $*" >&2; }
die()  { printf '%s\n' "${R}error:${X} $*" >&2; exit 1; }

usage() {
	cat <<EOF
${B}ModelFabric installer${X}

  curl -fsSL https://modelfabric.sh/install.sh | sh

Options (environment variables):
  MFSH_VERSION       release tag to install        (default: latest)
  MFSH_INSTALL_DIR   install location              (default: \$HOME/.local/bin)
  MFSH_BASE_URL      release host                  (default: GitHub releases)

Examples:
  MFSH_VERSION=v0.3.0 sh install.sh
  MFSH_INSTALL_DIR=/usr/local/bin sh install.sh
EOF
}

case "${1:-}" in
	-h | --help) usage; exit 0 ;;
esac

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

# --- what are we running on? ---------------------------------------------

detect_platform() {
	os=$(uname -s)
	arch=$(uname -m)

	case "$os" in
		Linux)  os=linux ;;
		Darwin) os=darwin ;;
		*)      die "unsupported operating system: $os (ModelFabric ships linux and darwin builds)" ;;
	esac

	case "$arch" in
		x86_64 | amd64) arch=amd64 ;;
		aarch64 | arm64) arch=arm64 ;;
		*) die "unsupported architecture: $arch" ;;
	esac

	# There is no darwin/amd64 build: ModelFabric's Mac support is Apple silicon
	# (Metal and MLX), so an Intel Mac has nothing to run.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
		die "ModelFabric ships Apple silicon builds only; this is an Intel Mac"
	fi

	PLATFORM="$os-$arch"
}

# --- fetching -------------------------------------------------------------

fetch() { # url -> stdout
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	else
		wget -qO- "$1"
	fi
}

fetch_to() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	else
		wget -qO "$2" "$1"
	fi
}

latest_version() {
	# The redirect target of /releases/latest carries the tag, so this needs no
	# API token and is not rate-limited the way api.github.com is.
	url=$(
		if command -v curl >/dev/null 2>&1; then
			curl -fsSLI -o /dev/null -w '%{url_effective}' \
				"https://github.com/$REPO/releases/latest" 2>/dev/null
		else
			wget -q -S --max-redirect=10 -O /dev/null \
				"https://github.com/$REPO/releases/latest" 2>&1 |
				sed -n 's/^ *Location: *//p' | tail -1
		fi
	) || true
	printf '%s\n' "${url##*/}"
}

sha256_of() { # file -> hex
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		printf ''
	fi
}

# --- install --------------------------------------------------------------

main() {
	command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
		die "curl or wget is required"
	need uname
	need mktemp

	detect_platform

	if [ -z "$VERSION" ]; then
		info "resolving the latest release..."
		VERSION=$(latest_version)
		case "$VERSION" in
			v*) ;;
			*) die "could not resolve the latest release; set MFSH_VERSION to a tag, e.g. MFSH_VERSION=v0.1.0" ;;
		esac
	fi

	asset="$BIN-$PLATFORM"
	url="$BASE_URL/$VERSION/$asset"

	say "${B}ModelFabric${X} $VERSION  ${DIM}($PLATFORM)${X}"

	tmp=$(mktemp -d) || die "could not create a temporary directory"
	trap 'rm -rf "$tmp"' EXIT INT TERM

	info "downloading $asset"
	fetch_to "$url" "$tmp/$BIN" ||
		die "download failed: $url
       If that release has no $PLATFORM build, see the source install:
       https://github.com/$REPO#install"

	[ -s "$tmp/$BIN" ] || die "downloaded file is empty: $url"

	# Verify against the release's checksums file. A release without one is a
	# reason to say so, not a reason to install something unverified quietly.
	if fetch "$BASE_URL/$VERSION/checksums.txt" > "$tmp/checksums.txt" 2>/dev/null &&
		[ -s "$tmp/checksums.txt" ]; then
		want=$(grep -F " $asset" "$tmp/checksums.txt" | cut -d' ' -f1 | head -1)
		got=$(sha256_of "$tmp/$BIN")
		if [ -z "$want" ]; then
			warn "checksums.txt has no entry for $asset; skipping verification"
		elif [ -z "$got" ]; then
			warn "no sha256sum or shasum on this machine; skipping verification"
		elif [ "$want" != "$got" ]; then
			die "checksum mismatch for $asset
       expected $want
       got      $got"
		else
			info "sha256 verified"
		fi
	else
		warn "no checksums.txt in release $VERSION; the binary was not verified"
	fi

	mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"

	# chmod before the move: a binary that lands without the executable bit
	# fails later with a confusing "Permission denied".
	chmod +x "$tmp/$BIN"

	dest="$INSTALL_DIR/$BIN"
	if [ -e "$dest" ] && ! [ -w "$dest" ]; then
		die "$dest exists and is not writable; re-run with MFSH_INSTALL_DIR set elsewhere, or use sudo"
	fi
	mv -f "$tmp/$BIN" "$dest" || die "could not install to $dest"

	say "${G}installed${X} $dest"

	case ":$PATH:" in
		*":$INSTALL_DIR:"*) ;;
		*)
			say ""
			warn "$INSTALL_DIR is not on your PATH. Add it:"
			say "    export PATH=\"\$PATH:$INSTALL_DIR\""
			;;
	esac

	say ""
	say "Next:"
	say "  ${B}mfsh doctor${X}   check everything ModelFabric depends on"
	say "  ${B}mfsh up${X}       start the node"
	say "  ${B}mfsh get${X} qwen/qwen3-0.6b"
}

main "$@"
