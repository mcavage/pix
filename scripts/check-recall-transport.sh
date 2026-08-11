#!/usr/bin/env bash
# FF4a — append-only context transport guard.
#
# Recall and output styles are delivered as append-only `before_agent_start`
# messages. Rewriting the system prompt every turn moves the provider's
# prefix-cache divergence point to byte 0 of the request. Removing one of these
# messages later moves the divergence point backwards. Both waste the cache.
#
# WHY THIS IS A BUILD GUARD AND NOT A COMMENT: the regression is invisible.
# Putting `systemPrompt` back works perfectly, breaks no test that does not
# specifically look for it, and only shows up as a bill. Worse, AGENTS.md
# documents the OPPOSITE pattern for display-only messages ("strip them in the
# `context` hook by customType"), so the most likely way to break this is a
# well-meaning follow-up applying that advice to `pix-recalled-context`.
#
# Four rules:
#   R1  no protected context extension returns `systemPrompt` from
#       before_agent_start;
#   R2  the shared recall helper lives OUTSIDE extensions/ (pi loads every .ts
#       there as an extension factory and crashes on one that is not);
#   R3  nothing filters a protected custom message out of the message list;
#   R4  every sibling directory imported by an extension is included in the
#       image's COPY list. A helper can resolve and test in the repo but still be
#       absent from the sandbox, which makes pi fail before the first turn.
#
# Usage: check-recall-transport.sh [--self-test]
#   --self-test  proves the guard actually catches each violation, by running
#                it against a scratch tree with the violation planted. A guard
#                nobody has seen fail is not known to work.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

APPEND_ONLY_EXTENSIONS=(extensions/memory-recall.ts extensions/output-style.ts)
HELPER=lib/recall-message.ts
CUSTOM_TYPES=("pix-recalled-context" "pix-output-style")
RECALL_CUSTOM_TYPE="pix-recalled-context"
AGENT_DIR="/home/agent/.pi/agent"

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

	# --- R1: no systemPrompt out of an append-only context extension --------
	for f in "${APPEND_ONLY_EXTENSIONS[@]}"; do
		[ -f "$root/$f" ] || {
			note "$f is missing; the append-only transport guard has nothing to check (did a file move? update this script)"
			continue
		}
		if strip_comments "$root/$f" | grep -anE '\bsystemPrompt\b' >/dev/null; then
			note "$f touches systemPrompt. Durable context is append-only: return { message } from before_agent_start, never { systemPrompt }."
			strip_comments "$root/$f" | grep -anE '\bsystemPrompt\b' | sed 's/^/    /' >&2
		fi
		# Do not use grep -q here: with pipefail it exits after the first match,
		# the producer can receive SIGPIPE on CI, and a valid file looks absent.
		if ! strip_comments "$root/$f" | grep -aE '(return \{[[:space:]]*message:|^[[:space:]]*message:)' >/dev/null; then
			note "$f does not return { message: … } from before_agent_start; durable context is not being delivered on the message channel."
		fi
	done

	# --- R2: the shared helper is not an extension --------------------------
	if [ ! -f "$root/$HELPER" ]; then
		note "$HELPER is missing; both recall extensions must build their message through the one shared helper."
	fi
	if [ -f "$root/extensions/recall-message.ts" ]; then
		note "extensions/recall-message.ts exists. pi loads EVERY .ts under extensions/ as an extension factory and crashes at startup on one that is not; the helper belongs in lib/."
	fi

	# --- R3: nothing strips a protected append-only message -----------------
	# A `context` hook that filters another customType is fine. One that removes
	# recalled context or the active output style is not.
	local custom_type strip_re hits
	for custom_type in "${CUSTOM_TYPES[@]}"; do
		strip_re="(!==?|filter|exclude|omit|strip|delete|drop|reject|splice)[^\"']*[\"']${custom_type}|[\"']${custom_type}[\"'][^\"']*(!==?)"
		while IFS= read -r f; do
			[ -f "$f" ] || continue
			hits="$(strip_comments "$f" | grep -anE "$strip_re" || true)"
			[ -n "$hits" ] || continue
			note "${f#"$root/"} looks like it removes ${custom_type} from the message list. Append-only context must survive the context hook:"
			printf '%s\n' "$hits" | sed 's/^/    /' >&2
		done < <(grep -rl "$custom_type" "$root/extensions" 2>/dev/null || true)
	done

	# --- R4: everything extensions/ imports from a sibling dir is baked ------
	# Collect the `../<dir>/` import targets across extensions/, then require the
	# Dockerfile to COPY each one into $AGENT_DIR/<dir>/ (the path `../<dir>/`
	# resolves to from $AGENT_DIR/extensions/ inside the sandbox).
	local dockerfile="$root/Dockerfile" dir imported
	imported="$(
		for f in "$root"/extensions/*.ts; do
			[ -f "$f" ] || continue
			strip_comments "$f"
		done | grep -aoE 'from "\.\./[A-Za-z0-9_.-]+/' | sed -e 's:^from "\.\./::' -e 's:/$::' | sort -u
	)"
	if [ ! -f "$dockerfile" ]; then
		note "Dockerfile is missing; cannot verify that extension dependencies are baked into the image."
	else
		while IFS= read -r dir; do
			[ -n "$dir" ] || continue
			if ! grep -E "^COPY .*[[:space:]]${dir}/[[:space:]]+${AGENT_DIR}/${dir}/" "$dockerfile" >/dev/null; then
				note "extensions/ imports from ../${dir}/ but the Dockerfile never COPYs ${dir}/ to ${AGENT_DIR}/${dir}/. The import resolves in this repo and fails in the sandbox; pi exits 1 on startup. Add: COPY --chown=agent:agent ${dir}/ ${AGENT_DIR}/${dir}/"
			fi
		done <<<"$imported"
	fi


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
		(cd "$ROOT" && tar cf - extensions lib Dockerfile) | (cd "$tmp/tree" && tar xf -)
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
	cp "$tmp/tree/$HELPER" "$tmp/tree/extensions/recall-message.ts"
	expect "helper copied into extensions/ is caught" 1

	plant
	rm -f "$tmp/tree/$HELPER"
	expect "missing shared helper is caught" 1

	plant
	printf '\nconst pruned = msgs.filter((m) => m.customType !== "%s");\n' "$RECALL_CUSTOM_TYPE" >>"$tmp/tree/extensions/timestamps.ts"
	expect "a context hook stripping the recall message is caught" 1

	plant
	printf '\nconst pruned = msgs.filter((m) => m.customType !== "pix-output-style");\n' >>"$tmp/tree/extensions/timestamps.ts"
	expect "a context hook stripping the output-style message is caught" 1

	plant
	grep -v "^COPY .*[[:space:]]lib/[[:space:]]" "$tmp/tree/Dockerfile" >"$tmp/Dockerfile.nolib"
	mv "$tmp/Dockerfile.nolib" "$tmp/tree/Dockerfile"
	expect "a Dockerfile that stops baking lib/ is caught" 1

	plant
	printf '\nimport { x } from "../types/pix-shims.d.ts";\n' >>"$tmp/tree/extensions/timestamps.ts"
	expect "an extension importing from an unbaked sibling dir is caught" 1


	return $rc
}

if [ "${1:-}" = "--self-test" ]; then
	self_test
	exit $?
fi

check_tree "$ROOT"
rc=$?
[ $rc -eq 0 ] && echo "context transport: append-only, helper outside extensions/, protected types survive context hooks"
exit $rc
