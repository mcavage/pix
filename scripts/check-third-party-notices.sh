#!/usr/bin/env bash
# AC-REL-01/02: THIRD_PARTY_NOTICES.md drift + fail-closed license-class gate,
# plus the "does it actually ship" checks (image + Homebrew tarball).
#
# Four checks, all must pass:
#   1. regenerate from scripts/legal/dependencies.json and diff against the
#      COMMITTED THIRD_PARTY_NOTICES.md — any drift fails (stale notices).
#   2. the LIVE Go module set (scripts/legal/list-go-modules.sh) validates
#      against the ledger + scripts/legal/notices-policy.json BOTH WAYS — an
#      undeclared dependency, a disallowed license class, or a ledger row that
#      is no longer in the live build graph (stale attribution) fails closed.
#   3. required attributions are present in the generated text: MPL-2.0 for
#      go-plugin/yamux, Suture, and the patched pi-tui.
#   4. inclusion: the Dockerfile COPYs THIRD_PARTY_NOTICES.md, NOTICE.md,
#      LICENSE and licenses/ into the image, and the release workflow bundles
#      all four into the Homebrew darwin tarball.
#   5. copyleft disclosure: every MPL-2.0 component has a Source Code Form URL
#      pinned to the version actually linked, the verbatim MPL-2.0 text really
#      exists at licenses/MPL-2.0.txt, and the notices do not contradict
#      themselves about whether that text is reproduced.
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
		ok "live Go module set passes the license-class gate, and the ledger carries no stale rows"
	else
		fail "live Go module set failed the license-class gate (AC-REL-01, either direction)"
		cat "$TMP_DIR/gate.err" >&2
	fi
else
	fail "could not enumerate live Go modules (is Go installed / services/host buildable?)"
	cat "$TMP_DIR/list.err" >&2
fi

# --- 2b. baked-tool (ruff/fd/go) version gate --------------------------------
# These are static binaries `curl`'d straight from GitHub Releases / go.dev in
# the Dockerfile (not npm/go.mod managed), pinned by an ARG. Fails closed if
# the Dockerfile ARG drifts from the ledger's recorded, license-verified
# version (e.g. someone bumps RUFF_VERSION without touching the ledger).
if node scripts/legal/generate-third-party-notices.mjs --check-baked-tools images/agent/Dockerfile >"$TMP_DIR/baked.out" 2>"$TMP_DIR/baked.err"; then
	ok "baked-tool (ruff/fd/go) versions match the ledger"
else
	fail "baked-tool (ruff/fd/go) version drift between Dockerfile and the ledger"
	cat "$TMP_DIR/baked.err" >&2
fi

# --- 2b2. ARG-pinned npm global (typescript) version gate --------------------
# `npm install -g typescript` with no version resolves to whatever the registry
# serves at build time, which makes the ledger's recorded version/license a
# claim about an unreproducible build. Fails closed on drift between
# ARG TYPESCRIPT_VERSION and the ledger's npmGlobal entry.
if node scripts/legal/generate-third-party-notices.mjs --check-npm-pins images/agent/Dockerfile >"$TMP_DIR/npmpins.out" 2>"$TMP_DIR/npmpins.err"; then
	ok "ARG-pinned npm globals (typescript) match the ledger"
else
	fail "ARG-pinned npm global version drift between Dockerfile and the ledger"
	cat "$TMP_DIR/npmpins.err" >&2
fi

if grep -qE 'npm install -g --ignore-scripts "typescript@\$\{TYPESCRIPT_VERSION\}"' images/agent/Dockerfile; then
	ok "Dockerfile installs typescript at the pinned ARG version"
else
	fail "Dockerfile installs an UNPINNED global typescript (use typescript@\${TYPESCRIPT_VERSION})"
fi

if [ "$(node -p 'require("./package.json").devDependencies.typescript')" = \
	"$(node -p 'require("./scripts/legal/dependencies.json").npmGlobal.find(e=>e.name==="typescript").version')" ]; then
	ok "package.json devDependency typescript matches the ledger version"
else
	fail "package.json devDependency typescript and the ledger disagree on the typescript version"
fi

# --- 2c. copyleft (MPL-2.0) disclosure gate ----------------------------------
# A Source Code Form URL that does not pin the linked version, or a referenced
# license text that does not ship, fails closed.
if node scripts/legal/generate-third-party-notices.mjs --check-copyleft-disclosure . >"$TMP_DIR/copyleft.out" 2>"$TMP_DIR/copyleft.err"; then
	ok "MPL-2.0 components disclose a pinned Source Code Form URL + a shipped license text"
else
	fail "MPL-2.0 copyleft-disclosure gate failed (source URL / license text)"
	cat "$TMP_DIR/copyleft.err" >&2
fi

if [ -f licenses/MPL-2.0.txt ] && grep -q 'Mozilla Public License Version 2.0' licenses/MPL-2.0.txt &&
	grep -q 'Exhibit B' licenses/MPL-2.0.txt; then
	ok "licenses/MPL-2.0.txt carries the full, verbatim MPL-2.0 text"
else
	fail "licenses/MPL-2.0.txt is missing or truncated (the full MPL-2.0 text must ship)"
fi

# The notices file must not both claim the text is reproduced and claim it is
# not. This is the exact contradiction B1 closed; keep it closed.
if grep -q 'Full upstream license texts are not reproduced verbatim here' THIRD_PARTY_NOTICES.md; then
	fail "notices still carry the blanket 'no license texts reproduced' claim while shipping licenses/MPL-2.0.txt (self-contradiction)"
else
	ok "notices make no self-contradictory claim about reproduced license texts"
fi

# --- 3. required attributions -------------------------------------------------
require_text() { # require_text <pattern> <label>
	if grep -qE "$1" THIRD_PARTY_NOTICES.md; then
		ok "notices include $2"
	else
		fail "notices are missing required attribution: $2"
	fi
}
require_text 'astral-sh/ruff' "ruff (baked tool) attribution"
require_text 'sharkdp/fd' "fd (baked tool) attribution"
require_text 'go\.dev/dl' "Go toolchain (baked tool) attribution"
# go-plugin/yamux (MPL-2.0) and Suture were deleted with pix-host's
# supervision tree in the Pix v2 cutover (docs/design/pix-v2-architecture.md
# §14, AC-16). Their attribution is required ONLY while the ledger actually
# carries a live entry for them — requiring it unconditionally would fail
# closed on a correct, honest ledger the moment the dependency is gone.
if [ "$(node -e 'process.stdout.write(String((require("./scripts/legal/dependencies.json").goModules||[]).filter(m=>m.class==="weak-copyleft").length))')" != "0" ]; then
	require_text 'MPL-2\.0' "weak-copyleft (MPL-2.0) attribution for the live ledger entry/entries"
	require_text 'licenses/MPL-2\.0\.txt' "pointer to the shipped MPL-2.0 license text"
else
	ok "no weak-copyleft (MPL-2.0) dependency in the ledger (go-plugin/yamux deleted with pix-host; nothing to attribute)"
fi
if [ "$(node -e 'process.stdout.write(String((require("./scripts/legal/dependencies.json").goModules||[]).filter(m=>/suture/.test(m.module)).length))')" != "0" ]; then
	require_text 'thejerf/suture' "Suture attribution"
else
	ok "no live Suture dependency in the ledger (deleted with pix-host's supervision tree; nothing to attribute)"
fi
# The planned-dependency marker is only required while the ledger actually
# carries a planned entry.
if [ "$(node -e 'process.stdout.write(String((require("./scripts/legal/dependencies.json").goModulesPlanned||[]).length))')" != "0" ]; then
	require_text 'planned' "planned-dependency marker"
else
	ok "no planned dependencies in the ledger (nothing to disclose)"
fi
require_text '@earendil-works/pi-tui' "patched pi-tui attribution"
require_text 'PATCH' "patched-component marker"

# --- 4. inclusion: image + tarball -------------------------------------------
if grep -qE 'COPY[[:space:]].*THIRD_PARTY_NOTICES\.md[[:space:]]' images/agent/Dockerfile; then
	ok "Dockerfile COPYs THIRD_PARTY_NOTICES.md into the image"
else
	fail "Dockerfile does not COPY THIRD_PARTY_NOTICES.md into the image"
fi

# MIT s2 ("included in all copies") + MPL-2.0 s3.1: pix's own license and the
# MPL text must travel with the image, not only live in the repo.
if grep -qE 'COPY[[:space:]].*[[:space:]]LICENSE[[:space:]]' images/agent/Dockerfile; then
	ok "Dockerfile COPYs LICENSE into the image"
else
	fail "Dockerfile does not COPY LICENSE into the image (MIT s2)"
fi

if grep -qE 'COPY[[:space:]].*[[:space:]]licenses/[[:space:]]' images/agent/Dockerfile; then
	ok "Dockerfile COPYs licenses/ (MPL-2.0 text) into the image"
else
	fail "Dockerfile does not COPY licenses/ into the image (MPL-2.0 s3.1)"
fi

if grep -q 'THIRD_PARTY_NOTICES.md' .github/workflows/publish.yml \
	&& grep -qE 'tar .*THIRD_PARTY_NOTICES\.md' .github/workflows/publish.yml; then
	ok "publish.yml bundles THIRD_PARTY_NOTICES.md into the Homebrew tarball"
else
	fail "publish.yml does not bundle THIRD_PARTY_NOTICES.md into the Homebrew tarball"
fi

if grep -qE 'tar .*NOTICE\.md LICENSE licenses' .github/workflows/publish.yml \
	&& grep -qF 'licenses/MPL-2.0.txt" "$stage/licenses/MPL-2.0.txt"' .github/workflows/publish.yml; then
	ok "publish.yml bundles LICENSE + licenses/MPL-2.0.txt into the Homebrew tarball"
else
	fail "publish.yml does not bundle LICENSE + licenses/MPL-2.0.txt into the Homebrew tarball"
fi

# --- 6. provenance/SBOM wiring + durable legal basis --------------------------
if grep -q 'scripts/release/verify-provenance.sh' .github/workflows/publish.yml \
	&& grep -q 'needs.merge.outputs.digest' .github/workflows/publish.yml; then
	ok "publish.yml runs verify-provenance.sh against the published manifest digest"
else
	fail "publish.yml does not run verify-provenance.sh against the merge job's published digest (AC-REL-04)"
fi

if grep -qF 'image: ${{ env.AGENT_IMAGE }}@${{ needs.merge.outputs.digest }}' .github/workflows/publish.yml; then
	ok "publish.yml generates the SBOM against the PUBLISHED image digest"
else
	fail "publish.yml does not generate an SBOM against the published image digest (AC-REL-04)"
fi

if grep -q 'continue-on-error: true' .github/workflows/legal.yml; then
	fail "legal.yml still has a continue-on-error job — either gate it or delete the claim"
else
	ok "legal.yml has no silently-passing (continue-on-error) job"
fi

for doc in docs/legal/AUTHORIZATIONS.md docs/legal/PRIVACY.md; do
	if [ -f "$doc" ]; then
		ok "$doc exists"
	else
		fail "$doc is missing"
	fi
done

if grep -q 'docs/legal/AUTHORIZATIONS.md' NOTICE.md; then
	ok "NOTICE.md points at the durable authorization record"
else
	fail "NOTICE.md does not point at docs/legal/AUTHORIZATIONS.md (the DHI basis of record)"
fi

if grep -q 'Docker, Inc.' LICENSE; then
	ok "LICENSE names Docker, Inc. as the copyright holder"
else
	fail "LICENSE copyright holder does not match the recorded employer-IP authorization"
fi

if grep -qi 'MIT license' CONTRIBUTING.md && grep -qi 'inbound' CONTRIBUTING.md; then
	ok "CONTRIBUTING.md states the inbound=outbound MIT contribution license"
else
	fail "CONTRIBUTING.md does not state the inbound contribution license (MIT)"
fi

exit "$FAILED"
