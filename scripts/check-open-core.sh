#!/usr/bin/env bash
# Open-core boundary guard. The entire safety story is "company/private-pack
# content stays out of the public repo + image." That rests on:
#   1. two hand-maintained allowlists (.gitignore and .dockerignore) that MUST
#      mirror each other, checked below (skills/agents);
#   2. nothing company-specific being git-tracked outside them (the generic
#      marker guard, below);
#   3. `pix-host` having exactly ONE host-side extension point — the
#      generic, SHA-pinned `[plugins.*]` external-process mechanism — and no
#      other compile-in extension seam ever quietly reappearing (the
#      compile-in guard, below). Private context ships as a runtime **pack**
#      (skills + knowledge + config), a **container** MCP integration, or a
#      standalone **host daemon** — see docs/design/packs.md.
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

# --- Host compile-in extension boundary guard -------------------------------
#
# pix-host has exactly one host-side extension point: the generic,
# SHA-pinned `[plugins.*]` external-process mechanism (services/host/plugin).
# There is no compile-in extension seam — no private Go source symlinked into
# services/host, and no init()-registered "extra*" factory hooks wiring extra
# commands/services/servers into the binary at build time. This guard fails
# closed if either pattern reappears in tracked source.
COMPILE_IN_MARKERS_REGEX='extraMcpServers|extraServiceFactories|extraServiceAliases|extraBrokerFactory|extraCommands\b|extraUsage\b'

# Paths excluded from the guard: this script itself, which has to NAME the
# markers to look for them.
COMPILE_IN_GUARD_EXCLUDE_REGEX='^scripts/check-open-core\.sh$'

# run_compile_in_guard scans `git ls-files` in the CURRENT directory's repo
# for COMPILE_IN_MARKERS_REGEX, skipping COMPILE_IN_GUARD_EXCLUDE_REGEX paths,
# and prints one "path:line:text" hit per line (empty output = clean).
# Factored out so `--self-test` can run the EXACT same logic against a
# disposable fixture repo instead of this repo's tracked tree — proving the
# guard actually trips without planting anything in real tracked source.
run_compile_in_guard() {
  local f
  while IFS= read -r f; do
    [ -f "$f" ] || continue
    grep -nE "$COMPILE_IN_MARKERS_REGEX" "$f" 2>/dev/null | sed "s#^#$f:#" || true
  done < <(git ls-files | grep -vE "$COMPILE_IN_GUARD_EXCLUDE_REGEX" || true)
  return 0
}

# run_symlink_guard flags any tracked file under services/host that is a
# symlink (mode 120000 in the git index). services/host ships as one
# compiled Go binary from source that lives in this tree; a symlinked-in
# private source file dropped alongside it is the one build-time pattern
# that could reintroduce private code without it ever being reviewed here.
run_symlink_guard() {
  git ls-files -s -- services/host 2>/dev/null | awk '$1 == "120000" { $1=$2=$3=""; sub(/^ */, ""); print }'
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
  hits="$(cd "$tmp" && run_compile_in_guard)"
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

  # A second fixture: a symlinked file tracked under services/host must also trip.
  mkdir -p "$tmp/services/host"
  printf 'package main\n' >"$tmp/services/host/private.go"
  ln -s private.go "$tmp/services/host/linked.go"
  git -C "$tmp" add -A
  local link_hits
  link_hits="$(cd "$tmp" && run_symlink_guard)"
  if [ -z "$link_hits" ]; then
    echo "SELF-TEST FAILED: symlink guard did not trip on a planted services/host symlink" >&2
    return 1
  fi
  if ! printf '%s\n' "$link_hits" | grep -q 'linked\.go$'; then
    echo "SELF-TEST FAILED: symlink guard hit doesn't reference linked.go: $link_hits" >&2
    return 1
  fi

  echo "self-test OK: compile-in guard trips on a planted marker and a planted symlink in a disposable fixture repo (this repo's tracked source was never touched)"
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

# Run the compile-in extension guard against THIS repo's real tracked tree.
compile_in_hits="$(run_compile_in_guard)"
if [ -n "$compile_in_hits" ]; then
  note "unapproved compile-in extension marker(s) found in tracked source (see docs/design/packs.md):"
  echo "$compile_in_hits" | sed 's/^/    /'
fi

symlink_hits="$(run_symlink_guard)"
if [ -n "$symlink_hits" ]; then
  note "unexpected symlinked file(s) tracked under services/host (no source is ever symlinked in):"
  echo "$symlink_hits" | sed 's/^/    /'
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
