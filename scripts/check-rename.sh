#!/usr/bin/env bash
# Rename guard (U-W3.04, AC-P0-402).
#
# The reviewable artifact for the rename is the INVENTORY
# (scripts/rename/inventory.tsv), not a hand-maintained regex allowlist
# (PIX-ADR-0004) -- a plan and a guard that are two different documents
# always drift. This script IS the guard, and it is a pure scan over the SAME
# generator that produces the inventory (scripts/rename/build-inventory.sh),
# so the guard and the plan can never disagree with each other; they can only
# disagree with the tree, which is exactly the failure this exists to catch.
#
# Four checks, all against the live tree:
#   R1  the checked-in inventory is not stale (build-inventory.sh --check)
#       -- this alone also catches every "unclassified" file: a file the
#       RULES table does not cover shows up as an extra/differing row, so the
#       diff is non-empty.
#   R2  every FROZEN literal (persisted customType strings already written to
#       disk in .pi-sessions/) appears EXACTLY its pinned number of times --
#       not "at least", because MORE hits means a new writer of the string
#       appeared without updating the pin, and FEWER means one got quietly
#       rewritten (the corruption this whole class exists to prevent).
#   R3  the out-of-pattern keep set survives (AC-P0-411): things that never
#       match the legacy-token scan, so they never appear as inventory rows,
#       and are therefore invisible to R1 -- a careless sweep could still take
#       them.
#
# On the PRE-rename tree (today) all three pass trivially: nothing has moved
# yet, so there is nothing to have moved wrongly. The guard becomes load-
# bearing the moment U-W3.07 opens the first mechanical commit, and it is
# already sitting in the gate (scripts/gate.sh) waiting for that day.
#
# Usage: check-rename.sh [--self-test]
#   --self-test  proves the guard actually catches each violation, by running
#                it against a scratch copy of the tracked tree with the
#                violation planted. A guard nobody has seen fail is not known
#                to work.
set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/.." && pwd)"
BUILD_INV="$SELF_DIR/rename/build-inventory.sh"
INVENTORY_REL="scripts/rename/inventory.tsv"

# --- R2: frozen literals, pinned exactly --------------------------------
# literal <TAB> expected count. Sourced from build-inventory.sh's own FROZEN
# table (--print-frozen) so the pin lives in exactly one place; this script
# only re-asserts it against the tree.
frozen_table() { "$BUILD_INV" --print-frozen; }

check_frozen() { # ROOT -> prints failures, returns count
	local root="$1" lit want got n=0
	while IFS=$'\t' read -r lit want _; do
		[ -n "$lit" ] || continue
		got="$(git -C "$root" grep -I -o -F --no-color -e "$lit" -- . 2>/dev/null | wc -l | tr -d ' ')"
		if [ "$got" != "$want" ]; then
			echo "FAIL: frozen literal '$lit' occurs $got time(s) in the tracked tree, expected exactly $want (see build-inventory.sh FROZEN table)" >&2
			n=$((n + 1))
		fi
	done < <(frozen_table)
	return $n
}

# --- R3: the out-of-pattern keep set (AC-P0-411) ------------------------
# Each entry: a human label and a shell predicate that must be true.
check_keep_set() { # ROOT -> prints failures, returns count
	local root="$1" n=0
	has_text() { git -C "$root" grep -I -q -F -- "$1" 2>/dev/null; }
	has_path() { git -C "$root" ls-files -- "$1" 2>/dev/null | grep -q .; }

	local port
	for port in 11435 11436 11437; do
		if ! has_text "$port"; then
			echo "FAIL: port $port has disappeared from the tracked tree (AC-P0-411 keep set: ports)" >&2
			n=$((n + 1))
		fi
	done
	if ! git -C "$root" grep -I -q -w -F -e gog -- . 2>/dev/null; then
		echo "FAIL: the external 'gog' executable name has disappeared from the tracked tree (AC-P0-411 keep set)" >&2
		n=$((n + 1))
	fi
	if ! has_text ".pi-sessions/"; then
		echo "FAIL: '.pi-sessions/' has disappeared from the tracked tree (AC-P0-411 keep set)" >&2
		n=$((n + 1))
	fi
	if ! has_path 'pi-kit/*'; then
		echo "FAIL: pi-kit/ is no longer a tracked directory (AC-P0-411 keep set: old version-pinned kit URLs reference this directory name)" >&2
		n=$((n + 1))
	fi
	return $n
}

check_all() { # ROOT
	local root="$1" fail=0

	if ! "$BUILD_INV" --root "$root" --check >/dev/null 2>&1; then
		echo "FAIL: $INVENTORY_REL is stale against the tree -- re-run scripts/rename/build-inventory.sh --write" >&2
		"$BUILD_INV" --root "$root" --check 2>&1 | sed 's/^/    /' >&2
		fail=1
	fi

	check_frozen "$root" || fail=1
	check_keep_set "$root" || fail=1

	return $fail
}

self_test() {
	local tmp rc=0
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' RETURN

	# Fresh copy of exactly the tracked tree (index, not history -- this
	# catches staged-but-uncommitted changes too), re-committed into its OWN
	# git repo (build-inventory.sh shells out to `git grep`/`git ls-files`, so
	# it needs a real repo under the copy, not just files on disk).
	plant() {
		rm -rf "$tmp/tree"
		mkdir -p "$tmp/tree"
		(cd "$REPO_ROOT" && git ls-files -z) |
			(cd "$REPO_ROOT" && tar --null -T - -cf -) |
			(cd "$tmp/tree" && tar xf -)
		(
			cd "$tmp/tree" &&
				git init -q &&
				git config user.email test@example.com &&
				git config user.name test &&
				git add -A &&
				git commit -q -m snapshot
		)
	}

	expect() { # <label> <expected: 0=pass 1=fail>
		local status
		# `git grep`/`git ls-files` only see tracked content; a new file must be
		# staged (not necessarily committed) to become visible, same as a real PR.
		git -C "$tmp/tree" add -A >/dev/null 2>&1 || true
		check_all "$tmp/tree" >/dev/null 2>&1
		status=$?
		if { [ "$status" -eq 0 ] && [ "$2" -ne 0 ]; } || { [ "$status" -ne 0 ] && [ "$2" -eq 0 ]; }; then
			echo "self-test FAIL: $1 -- guard exited $status, expected $([ "$2" -eq 0 ] && echo 0 || echo nonzero)" >&2
			rc=1
		else
			echo "self-test ok: $1"
		fi
	}

	plant
	expect "clean tree passes" 0

	plant
	echo 'a stray pi-stack mention nobody classified' >"$tmp/tree/uncommitted-drift.txt"
	expect "a new file with a legacy token not in the inventory is caught (stale inventory)" 1

	plant
	# Read the literal from the FROZEN table at runtime rather than spelling it
	# out here: this script's own source must not gain an extra tracked hit on
	# a literal whose whole point is a pinned, exact count.
	frozen_lit="$(frozen_table | head -1 | cut -f1)"
	printf '\n// %s\n' "$frozen_lit" >>"$tmp/tree/extensions/todo-autoclear.ts"
	expect "an extra frozen-literal occurrence is caught" 1

	plant
	# Blank out every occurrence of the port, wherever it lives in the tree.
	find "$tmp/tree" -type f -exec grep -Il '11435' {} \; | while read -r f; do
		sed -i.bak 's/11435/xxxxx/g' "$f" && rm -f "$f.bak"
	done
	expect "removing every occurrence of a keep-set port is caught" 1

	plant
	rm -rf "$tmp/tree/pi-kit"
	expect "removing the pi-kit/ directory is caught" 1

	return $rc
}

if [ "${1:-}" = "--self-test" ]; then
	self_test
	exit $?
fi

check_all "$REPO_ROOT"
rc=$?
[ $rc -eq 0 ] && echo "rename guard: inventory current, frozen literals pinned, AC-P0-411 keep set intact"
exit $rc
