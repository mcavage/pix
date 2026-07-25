#!/usr/bin/env bash
# Re-runnable rename driver (U-W3.05, D9 mitigation).
#
# WHY THIS EXISTS
#   D9 relaxes the PRD's hard edge (name availability blocks all of Wave 3) to
#   "an unavailable name blocks Wave 4 publish only" -- but ONLY because the
#   rename itself is made re-runnable. This script is that re-runnability: the
#   entire MECHANICAL class of the rename (everything the inventory
#   (scripts/rename/inventory.tsv) dispositions `rename`) is produced by
#   running this driver over the inventory with a single target-name variable
#   (scripts/rename/name.env). Nobody types the literal new name by hand into
#   a mechanical surface, and picking a different name later is "edit
#   name.env, re-run this script", not "re-review 200 files".
#
# SCOPE: content in `rename` AND `manual` rows; PATH MOVES for `rename` only
#   `manual`-disposition files hold a COMPUTED RUNTIME IDENTITY (sandbox name,
#   branch prefix, config/state/data dir, launchd label, systemd unit, ...)
#   ALONGSIDE ordinary mechanical text (import-path literals, log prose, test
#   literals) -- per build-inventory.sh's own legend: "the driver rewrites the
#   mechanical part, but the file also holds a computed runtime identity ...
#   a human reads every occurrence in U-W3.09". Skipping manual files'
#   CONTENT entirely would leave a stale import path the instant go.mod's
#   module line (a `rename` row) is swept -- e.g. config.go (manual) imports
#   the literal path "pi-stack/host/config"; if go.mod renames the module and
#   this literal does not move with it, the tree stops compiling. So content
#   substitution runs over `rename` and `manual` alike; PIX-ADR-0004's
#   smallness guarantee is about what a human re-reads afterward (U-W3.09),
#   not about which files a mechanical sed pass is allowed to touch.
#
#   PATH moves are different: they are NOT run for `manual` rows here. A
#   directory-level move (e.g. `git mv cmd/pi-stack cmd/pix`, U-W3.08) is its
#   own reviewed unit; this driver never independently relocates a file whose
#   disposition says a human needs to look at it first. `keep` and
#   `keep-historical` rows are never touched at all, content or path.
#
#   One more path exclusion, found empirically (not designed up front): a
#   `rename` row's pathmv is ALSO skipped when the file lives under
#   DEFERRED_MOVE_DIRS (currently just services/host/cmd/pi-stack/, see that
#   variable below). 148 files in that one directory are individually
#   `pathmv=yes` because their PATH contains the token, but they are all ONE
#   Go package -- moving them one row at a time would, for however many rows
#   the loop has processed so far, leave the package split across TWO
#   directories, both named `main`, each referencing the other's unexported
#   symbols. `go build` on a full-tree scratch run caught this directly.
#   That whole-directory move is U-W3.08's job, done once, atomically.
#
# WHAT IT DOES, per `rename`-or-`manual` row:
#   1. content:  substitute every legacy token (scripts/rename/build-
#                inventory.sh --print-tokens, the SAME table the inventory
#                scanner uses) for the matching case form of TARGET_NAME.
#   2. path:     `rename` rows ONLY -- if the row's `pathmv` column is `yes`,
#                apply the same token substitution to the file's own path and
#                `git mv` it (falling back to a plain `mv` outside a git repo).
#
# RE-RUNNABLE, NOT JUST RUNNABLE
#   A file already renamed no longer matches any legacy token, so a second run
#   over the SAME inventory is a clean no-op for it (skipped: token-free). A
#   run against a freshly reverted tree with a DIFFERENT name.env produces a
#   different result. Proof recipe (this is what U-W3.05's gate asks for):
#   run with a throwaway name on a scratch branch, confirm the tree builds,
#   `git checkout .` to discard.
#
# Usage:
#   apply.sh [--root DIR] [--dry-run]   apply (or preview) the rename
#   apply.sh --self-test                prove re-runnability on a scratch tree
set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"
BUILD_INV="$SELF_DIR/build-inventory.sh"
INVENTORY_REL="scripts/rename/inventory.tsv"
NAME_ENV_REL="scripts/rename/name.env"

die() {
	echo "apply: $*" >&2
	exit 2
}

# --- target name & derived case forms ---------------------------------------
load_target_name() { # ROOT -> sets TARGET_NAME
	local root="$1" env_file="$root/$NAME_ENV_REL"
	if [ -z "${TARGET_NAME:-}" ]; then
		[ -f "$env_file" ] || die "missing $NAME_ENV_REL and TARGET_NAME is not set in the environment"
		# shellcheck disable=SC1090
		. "$env_file"
	fi
	[ -n "${TARGET_NAME:-}" ] || die "TARGET_NAME is empty"
	[[ "$TARGET_NAME" =~ ^[a-z][a-z0-9]*$ ]] ||
		die "TARGET_NAME '$TARGET_NAME' must be a single lowercase word (^[a-z][a-z0-9]*$) -- see the scope note in name.env"
}

ucfirst() {
	local s="$1"
	printf '%s%s' "$(printf '%s' "${s:0:1}" | tr '[:lower:]' '[:upper:]')" "${s:1}"
}

# form_replacement FORM -> the replacement text for TARGET_NAME in that case
# form. There are three forms in the token table (lower/title/upper); which
# OLD spelling (hyphen, underscore, space, or bare concatenation) mapped to a
# given form collapses to the same replacement here, because TARGET_NAME is a
# single word with no separator of its own to preserve.
form_replacement() {
	case "$1" in
	lower) printf '%s' "$TARGET_NAME" ;;
	title) ucfirst "$TARGET_NAME" ;;
	upper) printf '%s' "$TARGET_NAME" | tr '[:lower:]' '[:upper:]' ;;
	*) die "unknown token form '$1' (build-inventory.sh's token table grew a form apply.sh does not know)" ;;
	esac
}

# apply_tokens_to_string STRING -> STRING with every legacy token replaced,
# via bash's literal (non-regex) substring substitution -- exactly right for a
# path, and safe to reuse for file content on the small set of plain-text
# files this driver touches.
TOKEN_TABLE="" # path<TAB>form pairs, one per line, populated by load_tokens
load_tokens() { TOKEN_TABLE="$("$BUILD_INV" --print-tokens)"; }

apply_tokens_to_string() { # STRING
	local s="$1" tok form repl
	while IFS=$'\t' read -r tok form; do
		[ -n "$tok" ] || continue
		repl="$(form_replacement "$form")"
		s="${s//$tok/$repl}"
	done <<<"$TOKEN_TABLE"
	printf '%s' "$s"
}

# apply_tokens_to_file FILE -> rewrites FILE in place via a temp file + mv, so
# it works identically under GNU and BSD sed/without depending on sed at all
# (portable across the macOS host-setup path and Linux CI).
apply_tokens_to_file() { # FILE
	local f="$1" tmp
	tmp="$(mktemp)"
	# Read once, transform once: a per-line apply_tokens_to_string call would
	# be quadratic on huge files, so slurp with a single awk-free bash read
	# loop is skipped in favor of just running the same substitution over the
	# whole file content in one shot with sed, built from the same table.
	local sed_args=() tok form repl
	while IFS=$'\t' read -r tok form; do
		[ -n "$tok" ] || continue
		repl="$(form_replacement "$form")"
		sed_args+=(-e "s/$(sed_pattern_escape "$tok")/$(sed_replacement_escape "$repl")/g")
	done <<<"$TOKEN_TABLE"
	sed "${sed_args[@]}" "$f" >"$tmp" && mv "$tmp" "$f"
}

# Escape the two sides independently. BRE patterns and sed replacements have
# different metacharacters; treating them alike can turn a future literal token
# into a wildcard and rewrite unrelated content.
sed_pattern_escape() { printf '%s' "$1" | sed 's/[][\\.^$*\/]/\\&/g'; }
sed_replacement_escape() { printf '%s' "$1" | sed 's/[\\&\/]/\\&/g'; }

# DEFERRED_MOVE_DIRS: directories where 148+ individually-`pathmv`-tagged
# files share ONE package/dir identity that has to move as a single atomic
# `git mv olddir newdir` (U-W3.08's own title: "git mv cmd/pi-stack cmd/pix +
# templates + man page") -- NOT as N independent per-file moves. Moving them
# one file at a time (which is what a naive per-row pathmv would do) SPLITS
# a single Go package main across two directories mid-run: files already
# moved land in the new dir, files not yet visited stay in the old one, and
# for one `go build` in between they are simultaneously two different
# packages both named `main` that reference each other's unexported symbols.
# That failure mode was found empirically on a full-tree scratch run (see the
# U-W3.05 proof notes) and is exactly why this is a DEFERRED unit, not a
# generic-driver responsibility: content substitution below still runs
# (import literals, prose, test literals all still need it), only the git mv
# is skipped here.
DEFERRED_MOVE_DIRS=(services/host/cmd/pi-stack/)
is_deferred_move_path() { # PATH -> 0 if under a deferred-move dir, 1 otherwise
	local p="$1" d
	for d in "${DEFERRED_MOVE_DIRS[@]}"; do
		case "$p" in "$d"*) return 0 ;; esac
	done
	return 1
}

# --- driver -------------------------------------------------------------
run_apply() { # ROOT DRY_RUN(0|1)
	local root="$1" dry="$2"
	local path disp occ frz pathmv note
	local total=0 edited=0 moved=0 skipped=0

	load_target_name "$root"
	load_tokens
	echo "apply: TARGET_NAME='$TARGET_NAME' (lower=$(form_replacement lower) title=$(form_replacement title) upper=$(form_replacement upper))$([ "$dry" -eq 1 ] && echo ' [dry-run]')"

	while IFS=$'\t' read -r path disp occ frz pathmv note; do
		case "$disp" in rename | manual) ;; *) continue ;; esac
		# Path moves are only ever done for `rename` rows -- see the SCOPE note
		# above. A `manual` row still gets its content swept.
		local do_move=0
		if [ "$disp" = "rename" ] && [ "$pathmv" = "yes" ] && ! is_deferred_move_path "$path"; then
			do_move=1
		fi
		local f="$root/$path"
		if [ ! -f "$f" ]; then
			skipped=$((skipped + 1))
			continue
		fi
		total=$((total + 1))
		if [ "$dry" -eq 1 ]; then
			echo "  would edit: $path ($disp)"
			if [ "$do_move" -eq 1 ]; then
				local preview
				preview="$(apply_tokens_to_string "$path")"
				[ "$preview" != "$path" ] && echo "  would move: $path -> $preview"
			fi
			continue
		fi
		apply_tokens_to_file "$f"
		edited=$((edited + 1))
		if [ "$do_move" -eq 1 ]; then
			local newrel newf newdir
			newrel="$(apply_tokens_to_string "$path")"
			if [ "$newrel" != "$path" ]; then
				newf="$root/$newrel"
				newdir="$(dirname "$newf")"
				mkdir -p "$newdir"
				if git -C "$root" rev-parse --git-dir >/dev/null 2>&1; then
					git -C "$root" mv -f -- "$path" "$newrel" 2>/dev/null || mv -f -- "$f" "$newf"
				else
					mv -f -- "$f" "$newf"
				fi
				moved=$((moved + 1))
			fi
		fi
	done < <(grep -v '^#' "$root/$INVENTORY_REL")

	if [ "$dry" -eq 1 ]; then
		echo "apply: dry-run — $total row(s) would be touched (rows already renamed by a prior run are skipped: $skipped)"
	else
		echo "apply: processed $total row(s) (rename + manual content), edited $edited file(s), moved $moved (already-renamed rows skipped: $skipped)"
	fi
}

self_test() {
	local tmp rc=0
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' RETURN

	plant() { # NAME -> a fresh minimal fixture git repo at $tmp/$NAME
		local dir="$tmp/$1"
		rm -rf "$dir"
		mkdir -p "$dir/scripts/rename" "$dir/docs/pi-stack-widget" "$dir/services/host/cmd/pi-stack"
		cp "$BUILD_INV" "$dir/scripts/rename/build-inventory.sh"
		# Two files sharing the pi-stack/host/cmd/pi-stack package dir: one hits an
		# explicit `manual` RULE by filename (task.go), one falls through to the
		# generic Go-source `rename` rule. Both must get content-swept; NEITHER
		# may move independently -- that dir is a DEFERRED_MOVE_DIRS entry (the
		# split-package failure mode found on the full-tree scratch run).
		echo 'package main // sandbox pi-stack-t-<name>' >"$dir/services/host/cmd/pi-stack/task.go"
		echo 'package main // pi-stack prose' >"$dir/services/host/cmd/pi-stack/other.go"
		cat >"$dir/README.md" <<'EOF'
pi-stack is a harness. PI-STACK_TOKEN is an env var. Pi-Stack Corp made it.
Also written pi_stack, PI_STACK, PiStack, Pi Stack, pistack, PISTACK.
EOF
		# install.sh matches a real `manual` RULE (identity: PI_STACK_PREFIX and
		# the installed binary names) with pathmv=no -- exactly the shape this
		# self-test needs: content should be swept, path should NOT move.
		echo 'sandbox name: pi-stack-t-abcd1234, prefix PI_STACK_PREFIX' >"$dir/install.sh"
		echo 'pi-stack widget doc' >"$dir/docs/pi-stack-widget/pi-stack-file.md"
		(
			cd "$dir" &&
				git init -q &&
				git config user.email test@example.com &&
				git config user.name test &&
				git add -A &&
				git commit -q -m fixture
		)
		bash "$dir/scripts/rename/build-inventory.sh" --root "$dir" --write >/dev/null
		(cd "$dir" && git add -A && git commit -q -m inventory)
	}

	expect_absent_in() { # <file> <needle> <label>
		if grep -I -q -F -- "$2" "$1" 2>/dev/null; then
			echo "self-test FAIL: $3 -- '$2' is still present in $1" >&2
			rc=1
		else
			echo "self-test ok: $3"
		fi
	}

	# Both helpers scope to README.md ONLY: the fixture also carries a `keep`
	# file (its own copy of build-inventory.sh) whose TOKEN TABLE necessarily
	# contains every legacy spelling by definition, so a whole-tree grep for
	# e.g. "pi-stack" is always true and proves nothing.
	expect_absent() { # <root> <needle> <label>
		if grep -I -q -F -- "$2" "$1/README.md" 2>/dev/null; then
			echo "self-test FAIL: $3 -- '$2' is still present" >&2
			rc=1
		else
			echo "self-test ok: $3"
		fi
	}
	expect_present() { # <root> <needle> <label>
		if grep -I -q -F -- "$2" "$1/README.md" 2>/dev/null; then
			echo "self-test ok: $3"
		else
			echo "self-test FAIL: $3 -- '$2' is missing" >&2
			rc=1
		fi
	}

	# --- run 1: throwaway name "zzalpha" ------------------------------------
	plant fx1
	TARGET_NAME=zzalpha run_apply "$tmp/fx1" 0 >/dev/null

	expect_absent "$tmp/fx1" "pi-stack" "run 1: legacy lower token gone from README.md"
	expect_absent "$tmp/fx1" "Pi-Stack" "run 1: legacy title token gone from README.md"
	expect_absent "$tmp/fx1" "PI-STACK" "run 1: legacy upper token gone from README.md"
	expect_present "$tmp/fx1" "zzalpha is a harness" "run 1: lower form substituted"
	expect_present "$tmp/fx1" "ZZALPHA_TOKEN" "run 1: upper form substituted"
	expect_present "$tmp/fx1" "Zzalpha Corp" "run 1: title form substituted"
	if [ -f "$tmp/fx1/docs/zzalpha-widget/zzalpha-file.md" ]; then
		echo "self-test ok: run 1: pathmv=yes row renamed both the directory and the file"
	else
		echo "self-test FAIL: run 1: expected docs/zzalpha-widget/zzalpha-file.md to exist after the move" >&2
		rc=1
	fi
	if grep -I -q -F -- "sandbox name: zzalpha-t-abcd1234, prefix ZZALPHA_PREFIX" "$tmp/fx1/install.sh" 2>/dev/null; then
		echo "self-test ok: run 1: a MANUAL-disposition file's CONTENT is swept too (install.sh; the mechanical part, per build-inventory.sh's legend)"
	else
		echo "self-test FAIL: run 1: expected install.sh's content to be rewritten (manual rows get content swept, just never moved)" >&2
		rc=1
	fi
	if [ -f "$tmp/fx1/install.sh" ]; then
		echo "self-test ok: run 1: install.sh's PATH did not move (manual-row path moves are a dedicated reviewed unit, not this driver)"
	else
		echo "self-test FAIL: run 1: install.sh moved -- manual rows must never be path-relocated by this driver" >&2
		rc=1
	fi

	# --- the deferred-move directory: content swept, NEITHER file moves -----
	# This is the split-package failure mode: if these moved one row at a time,
	# a mid-run `go build` would see two `main` packages in two directories.
	if [ -d "$tmp/fx1/services/host/cmd/pi-stack" ] && [ ! -d "$tmp/fx1/services/host/cmd/zzalpha" ]; then
		echo "self-test ok: run 1: services/host/cmd/pi-stack/ itself did NOT move (DEFERRED_MOVE_DIRS -- U-W3.08's job, atomically, not this driver's)"
	else
		echo "self-test FAIL: run 1: services/host/cmd/pi-stack/ moved (or partially moved) -- this is exactly the split-package failure this guard exists to prevent" >&2
		rc=1
	fi
	expect_absent_in "$tmp/fx1/services/host/cmd/pi-stack/task.go" "pi-stack" "run 1: task.go's CONTENT is still swept even though its directory is deferred"
	expect_absent_in "$tmp/fx1/services/host/cmd/pi-stack/other.go" "pi-stack" "run 1: other.go's CONTENT is still swept even though its directory is deferred"

	# --- re-run: same name, same tree -> clean no-op ------------------------
	before="$(cd "$tmp/fx1" && git add -A && git status --porcelain)"
	TARGET_NAME=zzalpha run_apply "$tmp/fx1" 0 >/dev/null
	after="$(cd "$tmp/fx1" && git add -A && git status --porcelain)"
	if [ "$before" = "$after" ]; then
		echo "self-test ok: re-running with the SAME name is a no-op (nothing left to rewrite)"
	else
		echo "self-test FAIL: re-running with the same name produced further changes:" >&2
		echo "$after" >&2
		rc=1
	fi

	# --- revert, then re-run with a DIFFERENT name (D9's actual promise) ----
	plant fx2
	TARGET_NAME=zzalpha run_apply "$tmp/fx2" 0 >/dev/null
	(cd "$tmp/fx2" && git checkout -q -- . && git clean -qfd)
	expect_present "$tmp/fx2" "pi-stack is a harness" "revert: git checkout restores the original tree"

	TARGET_NAME=zzbeta run_apply "$tmp/fx2" 0 >/dev/null
	expect_present "$tmp/fx2" "zzbeta is a harness" "run 2 (after revert): a DIFFERENT target name is honored"
	expect_absent "$tmp/fx2" "zzalpha" "run 2 (after revert): no trace of the FIRST run's name leaks into the second"

	# --- dry-run makes no changes -------------------------------------------
	plant fx3
	before="$(cd "$tmp/fx3" && git status --porcelain)"
	TARGET_NAME=zzgamma run_apply "$tmp/fx3" 1 >/dev/null
	after="$(cd "$tmp/fx3" && git status --porcelain)"
	if [ "$before" = "$after" ]; then
		echo "self-test ok: --dry-run makes no changes"
	else
		echo "self-test FAIL: --dry-run modified the tree" >&2
		rc=1
	fi

	# --- bad target name is rejected -----------------------------------------
	plant fx4
	# run in a SUBSHELL: die() calls exit, which would otherwise terminate this
	# whole self-test rather than just the one case being checked.
	if (TARGET_NAME="Not Valid" run_apply "$tmp/fx4" 1) >/dev/null 2>&1; then
		echo "self-test FAIL: an invalid TARGET_NAME was accepted" >&2
		rc=1
	else
		echo "self-test ok: an invalid TARGET_NAME is rejected"
	fi

	return $rc
}

mode=apply
root="$REPO_ROOT"
dry=0
while [ $# -gt 0 ]; do
	case "$1" in
	--self-test) mode=self-test ;;
	--dry-run) dry=1 ;;
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
self-test) self_test ;;
apply) run_apply "$root" "$dry" ;;
esac
