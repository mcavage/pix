#!/usr/bin/env bash
# FF4a — recall transport guard.
#
# Recall used to be delivered by rewriting the system prompt every turn. That
# moves the provider's prefix-cache divergence point to byte 0 of the request:
# nothing before it is reusable, so every turn pays full prefill. Recall is now
# an append-only `before_agent_start` MESSAGE (lib/recall-message.ts).
#
# WHY THIS IS A BUILD GUARD AND NOT A COMMENT: the regression is invisible.
# Putting `systemPrompt` back works perfectly, breaks no test that does not
# specifically look for it, and only shows up as a bill. Worse, AGENTS.md
# documents the OPPOSITE pattern for display-only messages ("strip them in the
# `context` hook by customType"), so the most likely way to break this is a
# well-meaning follow-up applying that advice to `pix-recalled-context`.
#
# Three rules:
#   R1  no recall extension returns `systemPrompt` from before_agent_start;
#   R2  the shared helper lives OUTSIDE extensions/ (pi loads every .ts there
#       as an extension factory and crashes on one that is not);
#   R3  nothing filters `pix-recalled-context` out of the message list.
#       Dropping an already-sent message moves the divergence point BACKWARDS,
#       which is strictly worse than the bug this transport fixed.
#
# Usage: check-recall-transport.sh [--self-test]
#   --self-test  proves the guard actually catches each violation, by running
#                it against a scratch tree with the violation planted. A guard
#                nobody has seen fail is not known to work.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

RECALL_EXTENSIONS=(extensions/memory-recall.ts extensions/knowledge-recall.ts)
HELPER=lib/recall-message.ts
CUSTOM_TYPE="pix-recalled-context"

fail=0
note() {
	echo "FAIL: $*" >&2
	fail=1
}

# Strip // line comments and /* */ blocks so the guard reads CODE, not the
# comments that necessarily name the very patterns it bans.
strip_comments() { # <file>
	sed -e 's://.*::' "$1" | awk '
		{ line = $0
		  while (match(line, /\/\*.*\*\//)) sub(/\/\*.*\*\//, "", line)
		  if (inblock) { if (match(line, /\*\//)) { sub(/^.*\*\//, "", line); inblock = 0 } else next }
		  if (match(line, /\/\*/)) { sub(/\/\*.*$/, "", line); inblock = 1 }
		  print line }'
}

check_tree() { # <root>
	local root="$1" f
	fail=0

	# --- R1: no systemPrompt out of a recall extension ----------------------
	for f in "${RECALL_EXTENSIONS[@]}"; do
		[ -f "$root/$f" ] || {
			note "$f is missing; the recall transport guard has nothing to check (did a file move? update this script)"
			continue
		}
		if strip_comments "$root/$f" | grep -nE '\bsystemPrompt\b' >/dev/null; then
			note "$f touches systemPrompt. Recall is append-only: return { message } from before_agent_start, never { systemPrompt }."
			strip_comments "$root/$f" | grep -nE '\bsystemPrompt\b' | sed 's/^/    /' >&2
		fi
		# Do not use grep -q here: with pipefail it exits after the first match,
		# the producer can receive SIGPIPE on CI, and a valid file looks absent.
		if ! strip_comments "$root/$f" | grep -E 'return \{ message:' >/dev/null; then
			note "$f does not return { message: … } from before_agent_start; recall is not being delivered on the message channel."
		fi
	done

	# --- R2: the shared helper is not an extension --------------------------
	if [ ! -f "$root/$HELPER" ]; then
		note "$HELPER is missing; both recall extensions must build their message through the one shared helper."
	fi
	if [ -f "$root/extensions/recall-message.ts" ]; then
		note "extensions/recall-message.ts exists. pi loads EVERY .ts under extensions/ as an extension factory and crashes at startup on one that is not; the helper belongs in lib/."
	fi

	# --- R3: nothing strips the recall message ------------------------------
	# A `context` hook that filters by customType is fine; one that filters out
	# pix-recalled-context is not. Look for the custom type appearing anywhere
	# near a negation or a filter/exclusion verb in code (not comments).
	local strip_re="(!==?|filter|exclude|omit|strip|delete|drop|reject|splice)[^\"']*[\"']${CUSTOM_TYPE}|[\"']${CUSTOM_TYPE}[\"'][^\"']*(!==?)"
	local hits
	while IFS= read -r f; do
		[ -f "$f" ] || continue
		hits="$(strip_comments "$f" | grep -nE "$strip_re" || true)"
		[ -n "$hits" ] || continue
		note "${f#"$root/"} looks like it removes ${CUSTOM_TYPE} from the message list. Recall must survive the context hook:"
		printf '%s\n' "$hits" | sed 's/^/    /' >&2
	done < <(grep -rl "$CUSTOM_TYPE" "$root/extensions" 2>/dev/null || true)

	return $fail
}

self_test() {
	local tmp status rc=0
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' RETURN

	# Fresh copy of the real tree, so each case starts from a known-clean state.
	plant() {
		rm -rf "$tmp/tree"
		mkdir -p "$tmp/tree"
		(cd "$ROOT" && tar cf - extensions lib) | (cd "$tmp/tree" && tar xf -)
	}

	expect() { # <label> <expected-rc>
		check_tree "$tmp/tree" >/dev/null 2>&1
		status=$?
		if [ "$status" -ne "$2" ]; then
			echo "self-test FAIL: $1 — guard exited $status, expected $2" >&2
			rc=1
		else
			echo "self-test ok: $1"
		fi
	}

	plant
	expect "clean tree passes" 0

	plant
	printf '\nconst leak = { systemPrompt: "x" };\n' >>"$tmp/tree/extensions/memory-recall.ts"
	expect "systemPrompt back in memory-recall.ts is caught" 1

	plant
	printf '\nconst leak = { systemPrompt: "x" };\n' >>"$tmp/tree/extensions/knowledge-recall.ts"
	expect "systemPrompt back in knowledge-recall.ts is caught" 1

	plant
	cp "$tmp/tree/$HELPER" "$tmp/tree/extensions/recall-message.ts"
	expect "helper copied into extensions/ is caught" 1

	plant
	rm -f "$tmp/tree/$HELPER"
	expect "missing shared helper is caught" 1

	plant
	printf '\nconst pruned = msgs.filter((m) => m.customType !== "%s");\n' "$CUSTOM_TYPE" >>"$tmp/tree/extensions/timestamps.ts"
	expect "a context hook stripping the recall message is caught" 1

	return $rc
}

if [ "${1:-}" = "--self-test" ]; then
	self_test
	exit $?
fi

check_tree "$ROOT"
rc=$?
[ $rc -eq 0 ] && echo "recall transport: append-only, helper outside extensions/, nothing strips $CUSTOM_TYPE"
exit $rc
