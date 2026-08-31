#!/usr/bin/env bash
# Builds the Pix runtime archive: shipped skills + agent definitions + Pi UX
# defaults (docs/design/pix-v2-architecture.md §3 / §4.2), staged into the
# canonical ~/.pix/runtime/<version>/ layout:
#
#   runtime/<version>/
#     skills/
#     agents/
#     pi/
#       settings.json
#       keybindings.json
#       themes/
#
# This is a PACKAGING step only: it copies from the repo's live source
# directories into out/runtime-stage/ and tars the result. It never moves or
# rewrites skills/, agents/, settings.json, keybindings.json, or themes/ in
# place, so `make run`'s dev-mode live-skill loading (DEV_SKILLS: --no-skills
# --skill $(CURDIR)/skills) keeps reading the same repo-root paths untouched —
# editing a SKILL.md and /reload stays instant with no archive step involved.
#
# Usage: build-runtime-archive.sh <version> [out-tar-gz-path]
set -euo pipefail

# macOS archive tools otherwise synthesize AppleDouble `._*` members for
# extended attributes. Those members are outside Pix's canonical runtime tree
# and are correctly refused by the installer, so suppress them at the source.
export COPYFILE_DISABLE=1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION="${1:?usage: build-runtime-archive.sh <version> [out-tar-gz-path]}"
OUT="${2:-$ROOT/out/pix-runtime-${VERSION}.tar.gz}"

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

RUNTIME_DIR="$STAGE/runtime/${VERSION}"
mkdir -p "$RUNTIME_DIR/pi"

cp -R "$ROOT/skills" "$RUNTIME_DIR/skills"
cp -R "$ROOT/agents" "$RUNTIME_DIR/agents"
cp "$ROOT/settings.json" "$RUNTIME_DIR/pi/settings.json"
cp "$ROOT/keybindings.json" "$RUNTIME_DIR/pi/keybindings.json"
cp -R "$ROOT/themes" "$RUNTIME_DIR/pi/themes"

# manifest.json (per docs/design/pix-v2-surface.md §4.2) names what this
# runtime directory carries and at what version, so doctor can verify the
# installed runtime and the pinned image agree without re-deriving it.
node -e '
const fs = require("fs");
const [version, dir] = process.argv.slice(1);
fs.writeFileSync(
  dir + "/manifest.json",
  JSON.stringify({ schemaVersion: 1, version, contents: ["skills", "agents", "pi/settings.json", "pi/keybindings.json", "pi/themes"] }, null, 2) + "\n",
);
' -- "$VERSION" "$RUNTIME_DIR"

mkdir -p "$(dirname "$OUT")"
tar -C "$STAGE" -czf "$OUT" "runtime/${VERSION}"
echo "wrote $OUT"
