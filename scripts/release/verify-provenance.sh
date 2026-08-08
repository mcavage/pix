#!/usr/bin/env bash
# AC-REL-04: SBOM/provenance — immutable version/digest record + verification
# AFTER manifest assembly (the `merge` job in .github/workflows/publish.yml
# creates the multi-arch manifest list and pushes it by digest).
#
# This script does NOT talk to a registry (that already happened in `merge`;
# see `docker buildx imagetools inspect` there). Its job is narrower and
# fully offline-testable: given a version and the digest that manifest
# assembly produced, it
#   1. validates both are well-formed (semver, `sha256:<64 hex>`),
#   2. writes/reads an append-only provenance record per version under
#      out/provenance/<version>.json, and
#   3. FAILS if a version's digest would be overwritten with a DIFFERENT
#      digest — a version's shipped digest is immutable once recorded. A
#      re-run with the SAME digest (e.g. a retried workflow step) is a no-op.
#
# Usage:
#   scripts/release/verify-provenance.sh <version> <digest> [git-sha] [out-dir]
set -euo pipefail

VERSION="${1:?usage: verify-provenance.sh <version> <digest> [git-sha] [out-dir]}"
DIGEST="${2:?usage: verify-provenance.sh <version> <digest> [git-sha] [out-dir]}"
GIT_SHA="${3:-unknown}"
OUT_DIR="${4:-out/provenance}"

if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "verify-provenance: version '$VERSION' is not semver (X.Y.Z)" >&2
	exit 1
fi

if ! [[ "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]]; then
	echo "verify-provenance: digest '$DIGEST' is not a well-formed sha256 digest" >&2
	exit 1
fi

mkdir -p "$OUT_DIR"
RECORD="$OUT_DIR/$VERSION.json"

if [ -f "$RECORD" ]; then
	EXISTING_DIGEST="$(grep -o '"digest"[[:space:]]*:[[:space:]]*"sha256:[0-9a-f]\{64\}"' "$RECORD" |
		sed -E 's/.*"(sha256:[0-9a-f]{64})".*/\1/')"
	if [ "$EXISTING_DIGEST" != "$DIGEST" ]; then
		echo "verify-provenance: IMMUTABILITY VIOLATION — v$VERSION was already recorded with" >&2
		echo "  $EXISTING_DIGEST" >&2
		echo "but this run wants to record a DIFFERENT digest:" >&2
		echo "  $DIGEST" >&2
		echo "a published version's digest can never change after the fact; if the build" >&2
		echo "genuinely differs, it needs a new version, not a rewritten record." >&2
		exit 1
	fi
	echo "verify-provenance: v$VERSION already recorded with the same digest — no-op"
	exit 0
fi

cat >"$RECORD" <<JSON
{
  "version": "$VERSION",
  "digest": "$DIGEST",
  "git_sha": "$GIT_SHA",
  "recorded_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
JSON

echo "verify-provenance: recorded v$VERSION -> $DIGEST ($RECORD)"
