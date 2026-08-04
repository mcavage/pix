#!/usr/bin/env bash
# AC-REL-01/02: THIRD_PARTY_NOTICES.md drift + fail-closed license-class gate,
# plus the "does it actually ship" checks (image + Homebrew tarball).
#
# Four checks, all must pass:
#   1. regenerate from scripts/legal/dependencies.json and diff against the
#      COMMITTED THIRD_PARTY_NOTICES.md — any drift fails (stale notices).
#   2. the LIVE Go module set (scripts/legal/list-go-modules.sh) validates
#      against the ledger + scripts/legal/notices-policy.json — an
#      undeclared dependency, or a disallowed license class, fails closed.
#   3. required attributions are present in the generated text: MPL-2.0 for
#      go-plugin/yamux, the Suture "planned" entry, and the patched pi-tui.
#   4. inclusion: the Dockerfile COPYs THIRD_PARTY_NOTICES.md into the image,
#      and the release workflow bundles it into the Homebrew darwin tarball.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FAILED=0
fail() {
	echo "check-third-party-notices: FAIL — $1" >&2
	FAILED=1
}
ok() { echo "check-third-party-notices: ok — $1"; }

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# --- 1. regenerate + diff -----------------------------------------------------
GENERATED="$TMP_DIR/THIRD_PARTY_NOTICES.generated.md"
node scripts/legal/generate-third-party-notices.mjs --write "$GENERATED"
if diff -u THIRD_PARTY_NOTICES.md "$GENERATED" >"$TMP_DIR/diff.txt"; then
	ok "THIRD_PARTY_NOTICES.md matches the ledger"
else
	fail "THIRD_PARTY_NOTICES.md is stale — run: node scripts/legal/generate-third-party-notices.mjs --write THIRD_PARTY_NOTICES.md"
	cat "$TMP_DIR/diff.txt" >&2
fi

# --- 2. live license-class gate -----------------------------------------------
LIVE="$TMP_DIR/live-modules.txt"
if bash scripts/legal/list-go-modules.sh >"$LIVE" 2>"$TMP_DIR/list.err"; then
	if node scripts/legal/generate-third-party-notices.mjs --check-live "$LIVE" >"$TMP_DIR/gate.out" 2>"$TMP_DIR/gate.err"; then
		ok "live Go module set passes the license-class gate"
	else
		fail "live Go module set failed the license-class gate (AC-REL-01)"
		cat "$TMP_DIR/gate.err" >&2
	fi
else
	fail "could not enumerate live Go modules (is Go installed / services/host buildable?)"
	cat "$TMP_DIR/list.err" >&2
fi

# --- 3. required attributions -------------------------------------------------
require_text() { # require_text <pattern> <label>
	if grep -qE "$1" THIRD_PARTY_NOTICES.md; then
		ok "notices include $2"
	else
		fail "notices are missing required attribution: $2"
	fi
}
require_text 'hashicorp/go-plugin.*MPL-2\.0|MPL-2\.0.*go-plugin' "go-plugin MPL-2.0"
require_text 'hashicorp/yamux.*MPL-2\.0|MPL-2\.0.*yamux' "yamux MPL-2.0"
require_text 'thejerf/suture' "Suture planned entry"
require_text 'planned' "planned-dependency marker"
require_text '@earendil-works/pi-tui' "patched pi-tui attribution"
require_text 'PATCH' "patched-component marker"

# --- 4. inclusion: image + tarball -------------------------------------------
if grep -qE 'COPY[[:space:]].*THIRD_PARTY_NOTICES\.md[[:space:]]' Dockerfile; then
	ok "Dockerfile COPYs THIRD_PARTY_NOTICES.md into the image"
else
	fail "Dockerfile does not COPY THIRD_PARTY_NOTICES.md into the image"
fi

if grep -q 'THIRD_PARTY_NOTICES.md' .github/workflows/publish.yml \
	&& grep -qE 'tar .*THIRD_PARTY_NOTICES\.md' .github/workflows/publish.yml; then
	ok "publish.yml bundles THIRD_PARTY_NOTICES.md into the Homebrew tarball"
else
	fail "publish.yml does not bundle THIRD_PARTY_NOTICES.md into the Homebrew tarball"
fi

exit "$FAILED"
