#!/usr/bin/env bash
# Rename inventory generator (U-W3.03, AC-P0-401 / AC-P0-411).
#
# WHY THIS EXISTS
#   The reviewable artifact of the rename is an INVENTORY, not a diff
#   (PIX-ADR-0004). Nobody can meaningfully read a 269-file mechanical diff;
#   everybody can read one line per file saying what happens to it. This script
#   produces that artifact, and `scripts/check-rename.sh` scans the tree against
#   it, so the guard and the plan are the same object and cannot drift.
#
# WHAT A ROW MEANS
#   path        tracked file holding at least one legacy identifier token
#   disposition rename | keep | keep-historical | manual   (see below)
#   occurrences total legacy tokens in the file's CONTENT
#   frozen      how many of those are inside a FROZEN literal (never rewritten,
#               anywhere, by anything -- see FROZEN below)
#   pathmv      yes when the file's own PATH holds a token (needs a `git mv`)
#   note        why this disposition, in the reviewer's language
#
#   rename          fully mechanical. `scripts/rename/apply.sh` does 100% of it.
#   manual          the driver rewrites the mechanical part, but the file also
#                   holds a COMPUTED RUNTIME IDENTITY (sandbox name, branch
#                   prefix, config/state/data dir, workspace marker, env var,
#                   launchd label, systemd unit, service-identity name, pidfile
#                   or process-invocation pattern, persisted on-disk format).
#                   A human reads every occurrence in U-W3.09. This is the class
#                   PIX-ADR-0004 exists to make small enough to actually read.
#   keep            frozen forever. The driver must never touch the file.
#   keep-historical immutable history (changelog entries, upstream reports, old
#                   release notes). Renaming history rewrites the past into a
#                   name that never shipped.
#
# TARGET NAMES ARE NOT IN HERE, ON PURPOSE (D9)
#   A row records a DISPOSITION, never a target string. Changing the target name
#   re-runs `apply.sh` with a new `name.env`; it never re-reviews the inventory.
#
# THE OUT-OF-PATTERN KEEP SET (AC-P0-411)
#   These do not match the legacy token set, so they never appear as rows. They
#   are listed here because they are the things a careless sweep would take with
#   it, and the guard asserts they survive:
#     ports 11435 / 11436 / 11437      the external `gog` executable name
#     provider service names           skill / agent / capability / intent names
#     `.pi-sessions/`                  `pi-kit/` as a directory name (old
#                                      version-pinned kit URLs reference it)
#
# Usage:
#   build-inventory.sh [--print]        write the inventory to stdout (default)
#   build-inventory.sh --write          rewrite scripts/rename/inventory.tsv
#   build-inventory.sh --check          exit 1 if the checked-in file is stale
#   build-inventory.sh --print-tokens   the legacy token table (token<TAB>form)
#   build-inventory.sh --print-frozen   the frozen literal table (lit<TAB>n<TAB>why)
#   build-inventory.sh --root DIR       scan DIR's git tree instead of this repo
set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"
INVENTORY_REL="scripts/rename/inventory.tsv"
# The inventory is excluded from its own scan. It quotes every token it counts,
# so counting itself would never reach a fixed point: each write would change
# the count that produced it.
SCAN_PATHSPEC=(-- . ":(exclude)$INVENTORY_REL")

# --- the legacy identifier token set ----------------------------------------
# Every spelling of the old name that appears in the tree, each tagged with the
# CASE FORM its replacement takes. apply.sh derives the replacement from the
# single target name in name.env using this form; the scanner counts matches of
# the same tokens. One table, two consumers, no drift.
#
# Ordered longest-first so a scan never double-counts an overlap.
TOKENS=(
	$'pi-stack\tlower'
	$'pi_stack\tlower'
	$'Pi-Stack\ttitle'
	$'Pi_Stack\ttitle'
	$'Pi Stack\ttitle'
	$'PiStack\ttitle'
	$'PI-STACK\tupper'
	$'PI_STACK\tupper'
	$'pistack\tlower'
	$'PISTACK\tupper'
)

# --- frozen literals ---------------------------------------------------------
# Strings that keep the OLD name permanently, in every file, forever, because
# something outside this repo has already persisted them. Renaming one is
# silent data corruption, so each carries a pinned count the guard asserts.
#
# literal <TAB> expected occurrences in the tracked tree <TAB> why
FROZEN=(
	$'pi-stack-todo-cleared\t7\tpersisted customType in .pi-sessions/ (PIX-ADR-0007, AC-P0-409): renaming it silently resurrects cleared todos after compaction or resume'
	$'pi-stack-compaction-continuation\t3\tpersisted customType in .pi-sessions/ (PIX-ADR-0007, AC-P0-409): already written into session transcripts on disk'
)

# --- runtime-identity evidence ----------------------------------------------
# Content patterns that make a file DANGEROUS regardless of where it lives: it
# constructs or matches a string some other process, or a file already on disk,
# depends on. Any hit forces `manual` so a human reads the file in U-W3.09.
#
# Deliberately NOT here: PI_STACK_* env vars. Renaming those is mechanical and
# every consumer is compiler- or test-backstopped, and listing them would drag
# ~80 test files into the class that exists precisely to stay small enough to
# read.
#
# Prose never promotes on content: a doc that MENTIONS .pi-stack/ is describing
# the thing, not constructing it, and dragging every design doc into the manual
# class is how the class stops being readable. An explicit path RULE can still
# mark a document manual.
EVIDENCE_EXEMPT='\.(md|1)$'

# regex <TAB> what it is
EVIDENCE=(
	$'\\.pi-stack[/"\'`]\tconstructs the .pi-stack/ workspace marker path (Go/TS boundary, FF10)'
	$'\\.pi-stack-\tpersisted marker FILE name on disk'
	$'pi-stack-t-\tcomposes the task sandbox name (AC-P0-406)'
	$'"pi-stack/"\tcomposes the task branch prefix (AC-P0-408)'
	$'"pi-stack-"\tcomposes or matches a sandbox name by prefix (AC-P0-405)'
	$'com\\.pi-stack\tlaunchd label'
	$'pi-stack-serve\tsystemd unit / service log name'
	$'pi-stack-(memory|knowledge)\tservice-identity name returned by the readiness RPC (AC-P0-212)'
	$'pi_stack_version\tkey inside archive manifests already written to disk'
)

# --- disposition rules -------------------------------------------------------
# Ordered; FIRST MATCH WINS. A path matching nothing is `unclassified`, and the
# guard fails on it -- that is the drift control. Directory-shaped rules mean a
# new file in a covered dir is classified automatically; a genuinely new KIND of
# path stops the gate until a human dispositions it.
#
# Two dispositions are computed from content, before any rule is consulted:
#   * every occurrence frozen        -> keep   (nothing left to rewrite)
#   * some occurrences frozen        -> manual (a mixed file always gets eyes)

# regex <TAB> disposition <TAB> note
RULES=(
	# --- keep: tooling that must NAME the old identifier to police it ---------
	$'^scripts/rename/\tkeep\trename tooling: the token table and the inventory name the legacy identifier by definition'
	$'^scripts/check-rename\\.sh$\tkeep\trename guard: names the legacy identifier by definition'

	# --- keep-historical: the past is not renamed ----------------------------
	$'^CHANGELOG\\.md$\tkeep-historical\treleased history: entries describe versions published under the old name, including their asset URLs'
	$'^docs/upstream/\tkeep-historical\tupstream bug reports as filed: the text was sent to another project under the old name'

	# --- manual: computed runtime identity, read line by line in U-W3.09 -----
	$'^services/host/cmd/pi-stack/task\\.go$\tmanual\tidentity: task sandbox name (pi-stack-t-), branch prefix (pi-stack/), per-repo state dirs (AC-P0-406/407/408)'
	$'^services/host/cmd/pi-stack/sandbox\\.go$\tmanual\tidentity: the pi-stack-* prefix that SCOPES destructive sandbox removal (AC-P0-405)'
	$'^services/host/cmd/pi-stack/run\\.go$\tmanual\tidentity: deriveSandboxName composes pi-stack-<workspace> to mirror sbx default naming'
	$'^services/host/cmd/pi-stack/status\\.go$\tmanual\tidentity: filters `sbx ls` rows by the pi-stack- sandbox prefix'
	$'^services/host/cmd/pi-stack/sandboxmcpstate\\.go$\tmanual\tidentity: validates and joins sandbox names in the MCP receipt store'
	$'^services/host/cmd/pi-stack/workspacestate\\.go$\tmanual\tidentity: .pi-stack/ workspace markers read across the Go/TS boundary (FF10)'
	$'^services/host/cmd/pi-stack/hoststate\\.go$\tmanual\tidentity: .pi-stack/host-state.json workspace marker'
	$'^services/host/cmd/pi-stack/state\\.go$\tmanual\tidentity: config/state/data dir roots'
	$'^services/host/cmd/pi-stack/config\\.go$\tmanual\tidentity: config file path and env-var overrides'
	$'^services/host/cmd/pi-stack/reset\\.go$\tmanual\tidentity: names the config/state/data dirs it DELETES; a wrong string here deletes the wrong tree'
	$'^services/host/cmd/pi-stack/serve_(install|ctl|start|reload)\tmanual\tidentity: launchd label, systemd unit, pidfile ownership and pgrep invocation matching (AC-P0-410)'
	$'^services/host/cmd/pi-stack/templates/\tmanual\tidentity: the launchd label and systemd unit name themselves; the file NAMES also move'
	$'^services/host/cmd/pi-stack/packtrust\\.go$\tmanual\tidentity: launcher-owned pack trust host-state store'
	$'^services/host/cmd/pi-stack/packhost\\.go$\tmanual\tidentity: pack host-state paths'
	$'^services/host/cmd/pi-stack/sandboxname_test\\.go$\tmanual\tthe identity pins themselves (AC-P0-406): composed names and the truncation threshold are updated DELIBERATELY in U-W3.09, never by the driver'
	$'^services/host/identity\\.go$\tmanual\tidentity: the service-identity names the readiness RPC returns (AC-P0-211/212); a stale daemon is detected by this string'
	$'^services/host/config/\tmanual\tidentity: config dir, env-var prefix and persisted config keys'
	$'^services/host/memory_(backup|restore)\\.go$\tmanual\tpersisted format: the pi_stack_version key inside archive manifests already on disk'
	$'^install\\.sh$\tmanual\tidentity: PI_STACK_PREFIX and the installed binary names, with no back-compat read (AC-P0-612)'
	$'^scripts/macos/host-setup\\.sh$\tmanual\tidentity: launchd label and ~/Library/Logs paths'
	$'^extensions/monitor\\.ts$\tmanual\tidentity: the process-invocation regex used to detect a pi/pi-stack command line (AC-P0-410), plus PI_STACK_* env'
	$'^extensions/help\\.ts$\tmanual\tidentity: the .pi-stack-help-nudged persisted marker file'
	$'^extensions/(memory-recall|memory-capture|knowledge-recall|ollama-bridge)\\.ts$\tmanual\tidentity: reads .pi-stack/ workspace markers written by the Go launcher (FF10)'
	$'^bin/pi-stack$\tmanual\tidentity: the launcher shim composes the sandbox invocation; its own path also moves'
	$'^Makefile$\tmanual\tidentity: the dev sandbox name, image tag and .pi-stack/ marker paths'
	$'^pi-kit/spec\\.yaml$\tmanual\tidentity: image reference and sandbox naming, while `pi-kit/` itself is a KEEP (AC-P0-411)'
	$'^\\.gitignore$\tmanual\tidentity: the ignored workspace-marker path (.pix/host-state.json must stay ignored, AC-P0-405)'

	# --- rename: mechanical, driver-produced ---------------------------------
	$'^services/host/.*\\.go$\trename\tGo source: import paths, message prose and test literals; compiler- and test-backstopped'
	$'^services/host/cmd/pi-stack/pi-stack\\.1$\trename\tman page: prose plus its own filename'
	$'^services/host/.*\\.(json|md|mod)$\trename\thost data and docs'
	$'^(docs|skills|agents)/\trename\tprose'
	$'^(extensions|lib|types)/.*\\.ts$\trename\tsandbox-side TypeScript: comments and user-facing strings'
	$'^tests/\trename\tnode tests: literals track the code they cover'
	$'^scripts/\trename\tdev and patch scripts'
	$'^\\.github/\trename\tCI workflows and issue templates'
	$'^(themes|config|pi-kit)/\trename\tpackaging data'
	$'^(README|AGENTS|CONTRIBUTING|SECURITY|CODE_OF_CONDUCT)\\.md$\trename\tprose'
	$'^(Dockerfile|package\\.json|package-lock\\.json|routing\\.json|capabilities\\.json|\\.dockerignore)$\trename\tbuild and packaging metadata'
)

die() {
	echo "build-inventory: $*" >&2
	exit 2
}

print_tokens() {
	local t
	for t in "${TOKENS[@]}"; do printf '%s\n' "$t"; done
}

print_frozen() {
	local f
	for f in "${FROZEN[@]}"; do printf '%s\n' "$f"; done
}

# scan_content ROOT -> "path<TAB>occurrences" for every tracked text file that
# holds at least one legacy token. One git grep for the whole tree: 436 files
# through 12 fixed patterns costs ~15 ms, which is what keeps this affordable
# inside the fast gate.
scan_content() {
	local root="$1" args=() tok
	while IFS=$'\t' read -r tok _; do args+=(-e "$tok"); done < <(print_tokens)
	git -C "$root" grep -I -o -F --no-color "${args[@]}" "${SCAN_PATHSPEC[@]}" 2>/dev/null |
		awk -F: '{ n[$1]++ } END { for (p in n) printf "%s\t%d\n", p, n[p] }' |
		LC_ALL=C sort
}

# scan_frozen ROOT -> "path<TAB>frozen-token-count". Each frozen literal holds
# exactly one legacy token, so its match count is already in the same unit as
# scan_content. If a multi-token literal is ever frozen, weight it here rather
# than letting the two counts silently disagree.
scan_frozen() {
	local root="$1" args=() lit
	while IFS=$'\t' read -r lit _ _; do args+=(-e "$lit"); done < <(print_frozen)
	git -C "$root" grep -I -o -F --no-color "${args[@]}" -- . 2>/dev/null |
		awk -F: '{ n[$1]++ } END { for (p in n) printf "%s\t%d\n", p, n[p] }' |
		LC_ALL=C sort
}

# scan_evidence ROOT -> "path<TAB>label" for files holding runtime-identity
# evidence. One git grep per pattern (9 total, ~5 ms each); first pattern wins,
# so the table is ordered most-specific first.
scan_evidence() {
	local root="$1" rule re label path
	local -A seen=()
	for rule in "${EVIDENCE[@]}"; do
		IFS=$'\t' read -r re label <<<"$rule"
		while IFS= read -r path; do
			[ -n "$path" ] || continue
			[[ "$path" =~ $EVIDENCE_EXEMPT ]] && continue
			[ -n "${seen[$path]:-}" ] && continue
			seen["$path"]="$label"
			printf '%s\t%s\n' "$path" "$label"
		done < <(git -C "$root" grep -I -l -E --no-color -e "$re" "${SCAN_PATHSPEC[@]}" 2>/dev/null)
	done
}

classify() { # classify PATH OCC FROZEN EVIDENCE -> "disposition<TAB>note"
	local path="$1" occ="$2" frz="$3" ev="$4" rule re disp note
	if [ "$frz" -gt 0 ] && [ "$frz" -eq "$occ" ]; then
		printf 'keep\tevery occurrence is a frozen literal (see the FROZEN table): nothing here is rewritable\n'
		return
	fi
	for rule in "${RULES[@]}"; do
		IFS=$'\t' read -r re disp note <<<"$rule"
		[[ "$path" =~ $re ]] || continue
		case "$disp" in
		keep | keep-historical | manual)
			printf '%s\t%s\n' "$disp" "$note"
			return
			;;
		esac
		# A `rename` rule matched, but content can still promote the file.
		if [ -n "$ev" ]; then
			printf 'manual\tidentity: %s\n' "$ev"
			return
		fi
		if [ "$frz" -gt 0 ]; then
			printf 'manual\tmixed file: holds a frozen literal alongside rewritable text, so the driver must not sweep it blind\n'
			return
		fi
		printf '%s\t%s\n' "$disp" "$note"
		return
	done
	printf 'unclassified\tno rule matched: add one to build-inventory.sh RULES before this can land\n'
}

path_has_token() { # path_has_token PATH -> yes|no  (uses TOKEN_LIST)
	local path="$1" tok
	for tok in "${TOKEN_LIST[@]}"; do
		case "$path" in *"$tok"*)
			echo yes
			return
			;;
		esac
	done
	echo no
}

generate() { # generate ROOT -> the whole inventory file on stdout
	local root="$1" path occ frz disp note pathmv tok

	TOKEN_LIST=()
	while IFS=$'\t' read -r tok _; do TOKEN_LIST+=("$tok"); done < <(print_tokens)

	local -A frozen_at=() evidence_at=()
	while IFS=$'\t' read -r path occ; do frozen_at["$path"]="$occ"; done < <(scan_frozen "$root")
	local label
	while IFS=$'\t' read -r path label; do evidence_at["$path"]="$label"; done < <(scan_evidence "$root")

	local total=0 nfiles=0 nrename=0 nkeep=0 nhist=0 nmanual=0 nunc=0
	local -a rows=()
	while IFS=$'\t' read -r path occ; do
		frz="${frozen_at[$path]:-0}"
		IFS=$'\t' read -r disp note <<<"$(classify "$path" "$occ" "$frz" "${evidence_at[$path]:-}")"
		pathmv="$(path_has_token "$path")"
		rows+=("$(printf '%s\t%s\t%s\t%s\t%s\t%s' "$path" "$disp" "$occ" "$frz" "$pathmv" "$note")")
		total=$((total + occ))
		nfiles=$((nfiles + 1))
		case "$disp" in
		rename) nrename=$((nrename + 1)) ;;
		keep) nkeep=$((nkeep + 1)) ;;
		keep-historical) nhist=$((nhist + 1)) ;;
		manual) nmanual=$((nmanual + 1)) ;;
		*) nunc=$((nunc + 1)) ;;
		esac
	done < <(scan_content "$root")

	cat <<EOF
# Rename inventory - GENERATED by scripts/rename/build-inventory.sh. Do not hand-edit.
#
# This file is the review artifact for the rename (AC-P0-401, PIX-ADR-0004).
# Reviewing it means reading the DISPOSITION column: the counts are re-derived
# from the tree on every gate run by scripts/check-rename.sh, so a row can never
# describe a file that no longer looks like that.
#
# disposition: rename | keep | keep-historical | manual
#   rename           mechanical; scripts/rename/apply.sh does all of it
#   manual           holds a computed runtime identity; a human reads every
#                    occurrence in U-W3.09 (this is the small, dangerous class)
#   keep             frozen forever; the driver never touches the file
#   keep-historical  immutable history; renaming it rewrites the past
#
# columns: path  disposition  occurrences  frozen  pathmv  note
#
# totals: $nfiles files, $total occurrences
#   rename $nrename | manual $nmanual | keep $nkeep | keep-historical $nhist | unclassified $nunc
EOF
	[ ${#rows[@]} -gt 0 ] && printf '%s\n' "${rows[@]}"
	return 0
}

mode=print
root="$REPO_ROOT"
while [ $# -gt 0 ]; do
	case "$1" in
	--print) mode=print ;;
	--write) mode=write ;;
	--check) mode=check ;;
	--print-tokens) mode=tokens ;;
	--print-frozen) mode=frozen ;;
	--root)
		shift
		root="${1:-}"
		[ -n "$root" ] || die "--root needs a directory"
		;;
	-h | --help)
		sed -n '2,40p' "${BASH_SOURCE[0]}"
		exit 0
		;;
	*) die "unknown argument $1" ;;
	esac
	shift
done

case "$mode" in
tokens) print_tokens ;;
frozen) print_frozen ;;
print) generate "$root" ;;
write)
	out="$root/$INVENTORY_REL"
	generate "$root" >"$out.tmp" && mv "$out.tmp" "$out"
	echo "build-inventory: wrote $INVENTORY_REL"
	;;
check)
	tmp="$(mktemp)"
	generate "$root" >"$tmp"
	if diff -u "$root/$INVENTORY_REL" "$tmp" >/dev/null 2>&1; then
		rm -f "$tmp"
	else
		echo "build-inventory: $INVENTORY_REL is stale — re-run scripts/rename/build-inventory.sh --write" >&2
		diff -u "$root/$INVENTORY_REL" "$tmp" 2>&1 | head -40 >&2
		rm -f "$tmp"
		exit 1
	fi
	;;
esac
