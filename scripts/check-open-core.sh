#!/usr/bin/env bash
# Open-core boundary guard. The entire safety story is "company/private-pack
# content stays out of the public repo + image." That rests on:
#   1. two hand-maintained allowlists (.gitignore and .dockerignore) that MUST
#      mirror each other, checked below (skills/agents);
#   2. nothing company-specific being git-tracked outside them (the generic
#      marker guard, below);
#   3. the retired build-time "overlay" concept (a peer-repo mixin kit +
#      compiled-in `host/overlay_*.go` Go symlinks, `OVERLAY=.. make run`)
#      never quietly reappearing (the legacy-concept guard, below). Private
#      context ships as a runtime **pack** (skills + knowledge + config), a
#      **container** MCP integration, or a standalone **host daemon** now —
#      see docs/design/packs.md. This script is deliberately NOT built around
#      "is host/overlay_*.go tracked" any more: that file pattern was never the
#      safety story on its own (it was gitignored, not guarded), and the
#      compile-in seam it fed (`extraCommands`/`extraServiceFactories`/
#      `extraMcpServers`/…) has been removed outright, not just gitignored.
#
# build-free, so it runs in public CI without the DHI base image.
#
# Exit non-zero (and say why) on any drift or leak.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { echo "FAIL: $*"; fail=1; }

# Pull the "!<dir>/<name>" allowlist out of an ignore file for a given prefix,
# normalized (no leading '!', no trailing '/'), sorted.
allow() { # $1=file $2=prefix(skills|agents)
  grep -E "^!$2/" "$1" 2>/dev/null | sed -E "s#^!##; s#/\$##" | sort -u
}

# --- Legacy build-time-overlay concept guard --------------------------------
#
# Markers of the retired mechanism that must NEVER reappear in tracked source:
# an `OVERLAY=` env var driving a build/run, a link to the deleted
# docs/OVERLAY.md, the deleted examples/overlay/ path, the removed
# hostStateOverlay type, and the removed private compile-in hooks
# (extraMcpServers and its siblings). Keep this list in sync with what
# AGENTS.md/README.md/docs/design/packs.md describe as retired.
LEGACY_MARKERS_REGEX='OVERLAY=|docs/OVERLAY(\.md)?|examples/overlay|hostStateOverlay|extraMcpServers|extraServiceFactories|extraServiceAliases|extraBrokerFactory|extraCommands\b|extraUsage\b'

# Paths excluded from the guard: this script itself (it has to NAME the
# markers to look for them) and the design docs that document the retired
# overlay as HISTORICAL RECORD — each of those carries its own
# retired/superseded disclaimer and is not a live claim. Nothing else is
# excluded: a marker anywhere else in tracked source is a real regression.
LEGACY_GUARD_EXCLUDE_REGEX='^(scripts/check-open-core\.sh|docs/design/(packs|packs-v2|packs-v2-impl|profiles|onboarding-v2-spec)\.md)$'

# run_legacy_guard scans `git ls-files` in the CURRENT directory's repo for
# LEGACY_MARKERS_REGEX, skipping LEGACY_GUARD_EXCLUDE_REGEX paths, and prints
# one "path:line:text" hit per line (empty output = clean). Factored out so
# `--self-test` can run the EXACT same logic against a disposable fixture repo
# instead of this repo's tracked tree — proving the guard actually trips
# without planting anything in real tracked source.
run_legacy_guard() {
  local f
  while IFS= read -r f; do
    [ -f "$f" ] || continue
    grep -nE "$LEGACY_MARKERS_REGEX" "$f" 2>/dev/null | sed "s#^#$f:#" || true
  done < <(git ls-files | grep -vE "$LEGACY_GUARD_EXCLUDE_REGEX" || true)
  return 0
}

# self_test proves run_legacy_guard actually trips: it builds a THROWAWAY git
# repo in a tempdir, plants a forbidden marker in one tracked file plus an
# unrelated clean file, and asserts the guard flags the marker and only the
# marker. It never reads or writes anything in THIS repo's tracked source.
self_test() {
  local tmp hits
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  git init -q "$tmp"
  git -C "$tmp" config user.email test@example.com
  git -C "$tmp" config user.name "open-core self-test"
  printf 'var extraMcpServers = map[string]func(){}\n' >"$tmp/planted.go"
  printf 'nothing to see here\n' >"$tmp/clean.txt"
  git -C "$tmp" add -A
  hits="$(cd "$tmp" && run_legacy_guard)"
  if [ -z "$hits" ]; then
    echo "SELF-TEST FAILED: guard did not trip on a planted extraMcpServers marker" >&2
    return 1
  fi
  if ! printf '%s\n' "$hits" | grep -q '^planted\.go:'; then
    echo "SELF-TEST FAILED: guard hit doesn't reference planted.go: $hits" >&2
    return 1
  fi
  if printf '%s\n' "$hits" | grep -q '^clean\.txt:'; then
    echo "SELF-TEST FAILED: guard false-positived on an unrelated clean file: $hits" >&2
    return 1
  fi
  echo "self-test OK: legacy-concept guard trips on a planted marker in a disposable fixture repo (this repo's tracked source was never touched)"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

for kind in skills agents; do
  g="$(allow .gitignore "$kind")"
  d="$(allow .dockerignore "$kind")"
  # agents are tracked as files (agents/foo.md); strip .md so the two lists,
  # which both list bare names per kind, compare on the same axis.
  if [ "$g" != "$d" ]; then
    note "$kind allowlists differ between .gitignore and .dockerignore:"
    diff <(echo "$g") <(echo "$d") | sed 's/^/    /' || true
  fi
done

# No tracked skill dir may sit outside the .dockerignore allowlist. Only real
# skills (skills/<name>/...) count; bare files like skills/.gitkeep are scaffold.
allowed_skills="$(allow .dockerignore skills)"
for d in $(git ls-files skills/ | grep -E '^skills/[^/]+/' | sed -E 's#^(skills/[^/]+)/.*#\1#' | sort -u); do
  echo "$allowed_skills" | grep -qx "$d" || note "tracked skill not in allowlist (would leak): $d"
done

# No tracked agent file may sit outside the allowlist (compare paths verbatim —
# the allowlist lists agents/<name>.md, same as git ls-files).
allowed_agents="$(allow .dockerignore agents)"
for f in $(git ls-files agents/ | sort -u); do
  echo "$allowed_agents" | grep -qx "$f" || note "tracked agent not in allowlist (would leak): $f"
done

# Run the legacy-concept guard against THIS repo's real tracked tree.
legacy_hits="$(run_legacy_guard)"
if [ -n "$legacy_hits" ]; then
  note "retired build-time-overlay marker(s) found in tracked source (the private overlay concept is dead — see docs/design/packs.md):"
  echo "$legacy_hits" | sed 's/^/    /'
fi

# Belt-and-suspenders: no internal-only marker (your private codenames, account
# IDs, vault paths) may appear in any tracked file. The markers themselves are
# sensitive, so they live in a GITIGNORED file (config/open-core-markers.txt, one
# regex per line) — not in this public script. Absent in a public clone, which has
# no internal names to protect anyway, so the check simply skips.
# NOTE: capture the output and test it — do NOT pipe into `grep -q` under
# `set -o pipefail`: xargs exits 123 when any batch finds no match, which pipefail
# propagates, making the `if` read "clean" even when markers are present.
marker_file="config/open-core-markers.txt"
if [ -f "$marker_file" ]; then
  markers="$(grep -vE '^[[:space:]]*(#|$)' "$marker_file" | paste -sd'|' -)"
  if [ -n "$markers" ]; then
    marker_hits="$(git ls-files -z | xargs -0 grep -nIE "$markers" 2>/dev/null || true)"
    if [ -n "$marker_hits" ]; then
      note "internal marker(s) found in tracked file(s):"
      echo "$marker_hits" | sed 's/^/    /'
    fi
  fi
fi

if [ "$fail" -eq 0 ]; then
  echo "open-core boundary OK: skills + agents allowlists mirror, tracked tree clean."
fi
exit "$fail"
