#!/usr/bin/env bash
# Resolve the immutable digest for a base image ref, for the AC-REL-03
# `--build-arg BASE_IMAGE=...@sha256:<digest>` path documented in the
# Dockerfile. This script does exactly one thing: ask a live, credentialed
# `docker buildx imagetools inspect` for the digest and print it. It never
# guesses, caches, or fabricates a digest — if you have no registry session,
# it fails, on purpose (see docs/legal/FINDINGS.md: "DHI digest pin" is an
# open item that needs a human with DHI entitlement, not an agent).
#
# Usage:
#   scripts/release/resolve-base-digest.sh <image-ref>
#   scripts/release/resolve-base-digest.sh --parse <json-file>   # test-only
#
# `--parse` extracts the manifest digest from an already-captured
# `docker buildx imagetools inspect --format '{{json .}}'` JSON document,
# so the parsing logic is testable without Docker or network access.
set -euo pipefail

parse_digest_json() { # parse_digest_json <json-file>
	# `imagetools inspect --format '{{json .}}'` documents its own top-level
	# shape as {"manifest": {"digest": "sha256:...", ...}, ...}. Grep+sed
	# rather than a JSON parser dependency: this script must run in the same
	# minimal environment as the Dockerfile build, no jq guaranteed.
	local file="$1"
	local digest
	digest="$(grep -o '"digest"[[:space:]]*:[[:space:]]*"sha256:[0-9a-f]\{64\}"' "$file" | head -1 |
		sed -E 's/.*"(sha256:[0-9a-f]{64})".*/\1/')"
	if [ -z "$digest" ]; then
		echo "resolve-base-digest: no sha256 digest found in $file" >&2
		return 1
	fi
	echo "$digest"
}

if [ "${1:-}" = "--parse" ]; then
	parse_digest_json "${2:?usage: --parse <json-file>}"
	exit $?
fi

REF="${1:?usage: resolve-base-digest.sh <image-ref>}"

if ! command -v docker >/dev/null 2>&1; then
	echo "resolve-base-digest: docker is not installed — cannot resolve a real digest" >&2
	echo "resolve-base-digest: refusing to fabricate one; see docs/legal/FINDINGS.md" >&2
	exit 2
fi

TMP_JSON="$(mktemp)"
trap 'rm -f "$TMP_JSON"' EXIT

if ! docker buildx imagetools inspect --format '{{json .}}' "$REF" >"$TMP_JSON" 2>&1; then
	echo "resolve-base-digest: could not inspect $REF (no session, no entitlement, or ref does not exist)" >&2
	cat "$TMP_JSON" >&2
	exit 1
fi

parse_digest_json "$TMP_JSON"
