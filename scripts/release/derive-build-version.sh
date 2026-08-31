#!/usr/bin/env bash
# Derives the version identity a LOCAL (dev) build stamps into the launcher,
# so a local build can never be mistaken for — or resolve to the same
# identity as — the clean X.Y.Z that ships from release CI and lives
# committed in package.json / Makefile VERSION / pi-kit/spec.yaml (see
# services/host/cmd/pix/versionlockstep_test.go). Both a release stack and a
# dev stack now COEXIST on the same machine: the release stack pins the
# clean version, the dev stack pins this derived one, and nothing collides.
#
# Grammar (deliberately legal as all three things a build version becomes):
#   - a SemVer 2.0.0 prerelease (dot-separated alphanumeric/hyphen
#     identifiers, no leading zero on a purely numeric one),
#   - an OCI image tag ([a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}),
#   - a single filesystem path segment (services/host/release/release.go's
#     `versionRE`, [A-Za-z0-9][A-Za-z0-9._-]*).
#
#   X.Y.(Z+1)-beta.<sha7>                       clean working tree
#   X.Y.(Z+1)-beta.<sha7>.dirty.<12hex>         dirty working tree
#
# X.Y.Z is read straight from package.json (the same file the lockstep test
# and release CI treat as the one source of truth) and the PATCH IS BUMPED BY
# ONE — package.json still names the LAST RELEASED version, so a local build
# one patch ahead can never be confused with, or accidentally satisfy a probe
# for, a real released tag (`launcher.IsReleased` in
# services/host/launcher/released.go still correctly reports this as
# UNRELEASED, since it is not a bare X.Y.Z).
#
# <sha7> is the 7-hex-character short form of `git rev-parse HEAD` for the
# checkout producing this build. <12hex> is the first 12 hex characters of
# the sha256 of `git diff HEAD` — staged AND unstaged changes to TRACKED
# files. Untracked files are not part of a diff and are deliberately
# excluded: this identity is about "does this build differ from a clean
# checkout of this commit", not "is there stray output lying around". Two
# dirty trees with byte-identical diffs against the same HEAD get the exact
# SAME derived version; any difference in the diff bytes changes it.
#
# This script never invents an identity: a malformed package.json version, a
# missing git checkout, an unresolvable HEAD, or a candidate that fails the
# legality checks below all FAIL LOUD (nonzero exit, message on stderr, no
# stdout) rather than print something a caller could mistake for a real
# version.
#
# Usage:
#   scripts/release/derive-build-version.sh [repo-root]
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
PKG_JSON="$ROOT/package.json"

if [ ! -f "$PKG_JSON" ]; then
	echo "derive-build-version: no package.json at $PKG_JSON" >&2
	exit 1
fi

BASE_VERSION="$(grep -m1 '"version"' "$PKG_JSON" | sed -E 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/')"

if [ -z "$BASE_VERSION" ] || ! [[ "$BASE_VERSION" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
	echo "derive-build-version: package.json version '$BASE_VERSION' is not plain X.Y.Z semver — refusing to derive a local build identity from a malformed base" >&2
	exit 1
fi

MAJOR="${BASH_REMATCH[1]}"
MINOR="${BASH_REMATCH[2]}"
PATCH="${BASH_REMATCH[3]}"
NEXT_PATCH=$((PATCH + 1))

if ! git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	echo "derive-build-version: $ROOT is not inside a git checkout — cannot derive a local build identity without git metadata" >&2
	exit 1
fi

if ! HEAD_SHA="$(git -C "$ROOT" rev-parse --short=7 HEAD 2>&1)"; then
	echo "derive-build-version: could not resolve HEAD in $ROOT (no commits yet?) — cannot derive a local build identity" >&2
	echo "$HEAD_SHA" >&2
	exit 1
fi
if ! [[ "$HEAD_SHA" =~ ^[0-9a-f]{7,}$ ]]; then
	echo "derive-build-version: unexpected HEAD short sha '$HEAD_SHA' — cannot derive a local build identity" >&2
	exit 1
fi
SHA7="${HEAD_SHA:0:7}"

sha256_hex() { # reads stdin, prints the lowercase hex digest
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 | awk '{print $1}'
	else
		echo "derive-build-version: neither sha256sum nor shasum is available to hash the diff" >&2
		return 1
	fi
}

# Staged + unstaged changes to TRACKED files only, against HEAD. This is
# deliberately NOT `git status --porcelain` (which would also flag untracked
# files this script must ignore) and NOT `git diff` alone (which omits staged
# changes) — `git diff HEAD` is the one invocation that covers both index and
# working tree in a single, deterministic byte stream.
DIFF_BYTES="$(git -C "$ROOT" diff HEAD)"

BUILD_VERSION="${MAJOR}.${MINOR}.${NEXT_PATCH}-beta.${SHA7}"
if [ -n "$DIFF_BYTES" ]; then
	DIRTY_HASH="$(printf '%s' "$DIFF_BYTES" | sha256_hex | cut -c1-12)"
	BUILD_VERSION="${BUILD_VERSION}.dirty.${DIRTY_HASH}"
fi

# Legality gate: SemVer prerelease grammar, OCI tag grammar, and plain
# path-segment safety. Failing here means the shape above produced something
# illegal (e.g. an all-numeric identifier with a leading zero) — refuse
# rather than emit it.
if ! [[ "$BUILD_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+-([0-9A-Za-z.-]+)$ ]]; then
	echo "derive-build-version: derived version '$BUILD_VERSION' is not X.Y.Z-<prerelease> — refusing to emit it" >&2
	exit 1
fi
PRERELEASE="${BASH_REMATCH[1]}"
IFS='.' read -r -a IDS <<<"$PRERELEASE"
for id in "${IDS[@]}"; do
	if [ -z "$id" ] || ! [[ "$id" =~ ^[0-9A-Za-z-]+$ ]]; then
		echo "derive-build-version: prerelease identifier '$id' in '$BUILD_VERSION' is not legal SemVer — refusing to emit it" >&2
		exit 1
	fi
	if [[ "$id" =~ ^[0-9]+$ ]] && [ "$id" != "0" ] && [[ "$id" == 0* ]]; then
		echo "derive-build-version: numeric prerelease identifier '$id' in '$BUILD_VERSION' has a leading zero, which SemVer forbids — refusing to emit it" >&2
		exit 1
	fi
done
if ! [[ "$BUILD_VERSION" =~ ^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$ ]]; then
	echo "derive-build-version: derived version '$BUILD_VERSION' is not a legal OCI tag — refusing to emit it" >&2
	exit 1
fi
case "$BUILD_VERSION" in
*/* | *\\*)
	echo "derive-build-version: derived version '$BUILD_VERSION' is not a safe path segment — refusing to emit it" >&2
	exit 1
	;;
esac

printf '%s\n' "$BUILD_VERSION"
