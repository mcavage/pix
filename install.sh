#!/bin/sh
# pix installer — fetches the two host binaries (pix + pix-host)
# from a GitHub release and drops them in ~/.local/bin. No repo checkout, no sudo.
#
#   curl -fsSL https://raw.githubusercontent.com/mcavage/pix/main/install.sh | sh
#
# Inspect before you pipe to a shell — the source is right here:
#   https://github.com/mcavage/pix/blob/main/install.sh
#
# What it does:
#   - detects your OS (darwin/linux) + arch (amd64/arm64)
#   - resolves the latest release (or PIX_VERSION if you set one)
#   - downloads pix-<os>-<arch>, pix-host-<os>-<arch>, and SHA256SUMS
#   - verifies each binary's sha256 against SHA256SUMS (aborts on mismatch)
#   - installs both to ~/.local/bin (chmod +x), never touching an existing config
#
# Env knobs:
#   PIX_VERSION   pin a version (e.g. 0.0.42); default resolves 'latest'
#   PIX_PREFIX    install dir (default: ~/.local/bin)
#   PIX_DRYRUN=1       print the resolved URLs + paths, download nothing
#   PIX_FORCE_INSTALL=1 override an existing unrelated `pix` command
#
# Uninstall:
#   curl -fsSL .../install.sh | sh -s -- --uninstall
set -eu

REPO="mcavage/pix"
GH="https://github.com/${REPO}"
PREFIX="${PIX_PREFIX:-${HOME}/.local/bin}"
CONFIG_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/pix"
CONFIG_FILE="${CONFIG_DIR}/config.toml"
SOURCE_URL="${GH}/blob/main/install.sh"

BINARIES="pix pix-host"

log()  { printf '%s\n' "$*"; }
info() { printf '  %s\n' "$*"; }
err()  { printf 'install.sh: %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }

# --- OS/arch detection ------------------------------------------------------
# Emits "<os> <arch>" using the Go convention (darwin/linux, amd64/arm64).
detect_platform() {
	os_raw="$(uname -s)"
	arch_raw="$(uname -m)"

	case "$os_raw" in
		Darwin) os="darwin" ;;
		Linux)  os="linux" ;;
		*) die "unsupported OS '$os_raw' (need Darwin or Linux)" ;;
	esac

	case "$arch_raw" in
		x86_64 | amd64) arch="amd64" ;;
		arm64 | aarch64) arch="arm64" ;;
		*) die "unsupported arch '$arch_raw' (need x86_64/amd64 or arm64/aarch64)" ;;
	esac

	printf '%s %s' "$os" "$arch"
}

# --- HTTP helpers -----------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

# Pick a downloader once so the rest of the script is tool-agnostic.
if have curl; then
	DL="curl"
elif have wget; then
	DL="wget"
else
	DL=""
fi

# fetch URL OUTFILE — download URL to OUTFILE, failing loudly on HTTP errors.
fetch() {
	case "$DL" in
		curl) curl -fsSL "$1" -o "$2" ;;
		wget) wget -q "$1" -O "$2" ;;
		*) die "need curl or wget on PATH to download files" ;;
	esac
}

# resolve_latest — follow the /releases/latest redirect and read the version out
# of the resulting .../tag/v<VER> URL. Works with curl or wget.
resolve_latest() {
	url="${GH}/releases/latest"
	loc=""
	case "$DL" in
		curl) loc="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url")" ;;
		wget) loc="$(wget -q -S --max-redirect=0 "$url" 2>&1 | awk '/^  Location: /{print $2}' | tail -1)" ;;
		*) die "need curl or wget on PATH to resolve the latest version" ;;
	esac
	# Trailing path component is 'v<VER>'; strip the leading 'v'.
	ver="${loc##*/tag/}"
	ver="${ver##*/}"
	ver="${ver#v}"
	[ -n "$ver" ] || die "could not resolve the latest version from $url"
	printf '%s' "$ver"
}

# --- sha256 -----------------------------------------------------------------
# sha256_of FILE — print the hex digest, portable across linux/darwin.
sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "need sha256sum or shasum to verify downloads"
	fi
}

# verify FILE NAME SUMSFILE — compare FILE's digest to the NAME entry in SUMSFILE.
verify() {
	file="$1"; name="$2"; sums="$3"
	want="$(awk -v n="$name" '$2 == n || $2 == "*"n {print $1}' "$sums" | head -1)"
	[ -n "$want" ] || die "no SHA256SUMS entry for '$name'"
	got="$(sha256_of "$file")"
	if [ "$want" != "$got" ]; then
		err "checksum MISMATCH for $name"
		err "  expected: $want"
		err "  got:      $got"
		die "refusing to install a binary that does not match SHA256SUMS"
	fi
}

# --- install ----------------------------------------------------------------
do_install() {
	platform="$(detect_platform)"
	os="${platform% *}"
	arch="${platform#* }"

	if [ -n "${PIX_VERSION:-}" ]; then
		ver="$PIX_VERSION"
	elif [ "${PIX_DRYRUN:-}" = "1" ]; then
		# Don't hit the network in a dry run; show a placeholder.
		ver="${PIX_VERSION:-<latest>}"
	else
		log "Resolving latest release..."
		ver="$(resolve_latest)"
	fi

	base="${GH}/releases/download/v${ver}"
	sums_url="${base}/SHA256SUMS"

	if [ "${PIX_DRYRUN:-}" = "1" ]; then
		log "DRY RUN — nothing will be downloaded or written."
		log "Source: ${SOURCE_URL}"
		log "Platform: ${os}/${arch}"
		log "Version:  ${ver}"
		log "SHA256SUMS: ${sums_url}"
		for b in $BINARIES; do
			asset="${b}-${os}-${arch}"
			log "Download:  ${base}/${asset}"
			log "Install:   ${PREFIX}/${b}"
		done
		log "Config:    ${CONFIG_FILE} (seeded by 'pix setup', left untouched here)"
		return 0
	fi

	preflight_collision
	check_required_prereqs

	if [ -x "${PREFIX}/pix" ] && [ -x "${PREFIX}/pix-host" ]; then
		current="$("${PREFIX}/pix" version 2>/dev/null || true)"
		current_ver="$(printf '%s\n' "$current" | awk '{print $NF}' | sed 's/^v//')"
		if [ "$current_ver" = "$ver" ]; then
			log "install: Pix ${ver} is already current at ${PREFIX}"
			return 0
		fi
	fi

	[ -n "$DL" ] || die "need curl or wget on PATH"

	tmp="$(mktemp -d "${TMPDIR:-/tmp}/pix-install.XXXXXX")"
	# shellcheck disable=SC2064
	trap "rm -rf '$tmp'" EXIT INT TERM

	# Verify-all-then-install-all: a partial install (new pix + stale
	# pix-host, or vice versa) is a mismatched pair that can misbehave subtly.
	# So stage EVERYTHING in the temp dir and verify EVERY checksum first; only
	# once all binaries are downloaded, verified, and made executable do we move
	# any into place. Any failure before that point installs nothing (the temp dir
	# is cleaned by the EXIT trap and ${PREFIX} is left untouched).
	log "Downloading SHA256SUMS..."
	fetch "$sums_url" "${tmp}/SHA256SUMS"

	for b in $BINARIES; do
		asset="${b}-${os}-${arch}"
		log "Downloading ${asset}..."
		fetch "${base}/${asset}" "${tmp}/${b}"
		verify "${tmp}/${b}" "$asset" "${tmp}/SHA256SUMS"
		chmod +x "${tmp}/${b}"
	done

	# Everything verified — now install. These moves are the only writes to
	# ${PREFIX}; they happen last so a failed/mismatched download never lands.
	mkdir -p "$PREFIX"
	for b in $BINARIES; do
		mv -f "${tmp}/${b}" "${PREFIX}/${b}"
	done

	report "$os" "$arch" "$ver"
}

# report — the first-run summary: where it landed, PATH status, next command.
report() {
	os="$1"; arch="$2"; ver="$3"
	log ""
	log "Installed pix v${ver} (${os}/${arch}):"
	for b in $BINARIES; do
		info "${PREFIX}/${b}"
	done

	case ":${PATH}:" in
		*":${PREFIX}:"*)
			log ""
			log "${PREFIX} is already on your PATH."
			;;
		*)
			log ""
			shell_name="$(basename "${SHELL:-sh}")"
			case "$shell_name" in
				zsh) rc="${HOME}/.zshrc" ;;
				bash) rc="${HOME}/.bashrc" ;;
				*) rc="${HOME}/.profile" ;;
			esac
			log "Add Pix to PATH in ${rc}:"
			info "export PATH=\"${PREFIX}:\$PATH\""
			log "Then reload the named shell:"
			info "exec \"${SHELL:-/bin/sh}\" -l"
			;;
	esac

	if [ -f "$CONFIG_FILE" ]; then
		log ""
		log "Existing config left untouched: ${CONFIG_FILE}"
	fi


	assert_installed_resolution
	log ""
	log "Next:"
	info "pix setup"
}

preflight_collision() {
	for binary in pix pix-host; do
		found="$(command -v "$binary" 2>/dev/null || true)"
		[ -z "$found" ] && continue
		[ "$found" = "${PREFIX}/${binary}" ] && continue
		[ "${PIX_FORCE_INSTALL:-0}" = "1" ] && continue
		if [ -t 0 ] && [ -t 1 ]; then
			printf 'install: an existing %s command resolves to %s; continue? [y/N] ' "$binary" "$found"
			read -r answer
			case "$answer" in y|Y|yes|YES) continue ;; esac
		fi
		die "existing ${binary} command at ${found}; set PIX_FORCE_INSTALL=1 to replace intentionally"
	done
}

assert_installed_resolution() {
	for binary in pix pix-host; do
		found="$(command -v "$binary" 2>/dev/null || true)"
		[ -z "$found" ] && continue
		if [ "$found" != "${PREFIX}/${binary}" ]; then
			die "installed ${binary} at ${PREFIX}/${binary}, but PATH resolves ${found}; put ${PREFIX} before the shadowing directory"
		fi
	done
}

check_required_prereqs() {
	missing=0
	if ! have op; then
		err "missing required dependency: op"
		err "  fix: brew install 1password-cli"
		missing=1
	fi
	if ! have sbx; then
		err "missing required dependency: sbx"
		err "  fix: brew install docker/tap/sbx@nightly"
		missing=1
	fi
	[ "$missing" -eq 0 ] || die "install required dependencies, then run the installer again"
}

# --- uninstall --------------------------------------------------------------
do_uninstall() {
	log "Removing pix binaries from ${PREFIX}:"
	for b in $BINARIES; do
		if [ -e "${PREFIX}/${b}" ]; then
			rm -f "${PREFIX}/${b}"
			info "removed ${PREFIX}/${b}"
		else
			info "not found: ${PREFIX}/${b}"
		fi
	done

	if [ -e "$CONFIG_FILE" ] || [ -d "$CONFIG_DIR" ]; then
		log ""
		printf 'Also remove config at %s? [y/N] ' "$CONFIG_DIR"
		# Prefer the controlling terminal (so a `curl | sh -s -- --uninstall` still
		# prompts), but fall back to stdin, then to an empty (=keep) answer. The
		# `|| :` chain keeps `set -e` from aborting when no tty/stdin is attached.
		ans=""
		{ read -r ans </dev/tty || read -r ans || ans=""; } 2>/dev/null
		case "$ans" in
			y | Y | yes | YES)
				rm -rf "$CONFIG_DIR"
				info "removed ${CONFIG_DIR}"
				;;
			*)
				info "kept ${CONFIG_DIR}"
				;;
		esac
	fi
	log "Done."
}

# --- entry ------------------------------------------------------------------
main() {
	action="install"
	for arg in "$@"; do
		case "$arg" in
			--uninstall) action="uninstall" ;;
			-h | --help)
				sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
				exit 0
				;;
			*) die "unknown argument '$arg' (try --uninstall or --help)" ;;
		esac
	done

	case "$action" in
		install) do_install ;;
		uninstall) do_uninstall ;;
	esac
}

main "$@"
