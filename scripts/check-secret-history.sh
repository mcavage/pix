#!/usr/bin/env bash
# AC-REL-01: full-history secret scan over EVERY ref (not just HEAD), with a
# JSON artifact and a fail-closed exit code. Wraps scripts/legal/secret-scan.mjs
# with this repo's reviewed allowlist (scripts/legal/secret-scan-allowlist.txt).
#
# CI (see .github/workflows/legal.yml) must checkout with `fetch-depth: 0` AND
# fetch all branches/tags, or this only proves the checked-out ref is clean —
# the entire point is catching a secret that was committed then "removed" in
# a later commit but still lives in history.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

OUT_DIR="${SECRET_SCAN_OUT_DIR:-out/secret-scan}"
mkdir -p "$OUT_DIR"
REPORT="$OUT_DIR/report.json"

node scripts/legal/secret-scan.mjs --self-test || {
	echo "check-secret-history: pattern self-test failed -- refusing to trust the scan" >&2
	exit 1
}

node scripts/legal/secret-scan.mjs \
	--scan . \
	--out "$REPORT" \
	--allowlist scripts/legal/secret-scan-allowlist.txt
rc=$?

echo "check-secret-history: report at ${REPORT#$ROOT/}"
exit "$rc"
