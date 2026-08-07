#!/usr/bin/env bash
# verify-pix-lifecycle.sh — the HOST half of release UAT, as assertions.
#
# docs/HOST-UAT.md is the human checklist; this is the part a machine can prove.
# It exercises the sandbox lifecycle (digest naming, instance identity, attach
# fingerprint, multi-shell teardown, --keep, orphan reaping, exit propagation)
# and the host services (launchd-managed serve, memory unit restart across a
# SIGKILL) against the real `pix`, `sbx` and `docker` on YOUR machine.
#
#   bash scripts/macos/verify-pix-lifecycle.sh            # lifecycle + services
#   bash scripts/macos/verify-pix-lifecycle.sh --with-oauth   # + interactive OAuth
#   bash scripts/macos/verify-pix-lifecycle.sh --no-services  # lifecycle only
#
# Rules this script keeps, because a UAT script that lies is worse than none:
#
#   * Every check ASSERTS. Nothing prints PASS unless a command ran and its
#     output/exit code was compared. A prerequisite we could not test is SKIP,
#     counted separately, and a run with skips never prints a clean PASS.
#   * It only ever touches sandboxes it created, named pix-uat-<pid>-*. It never
#     runs `pix rm --all`, never `--force`, and refuses to proceed if a sandbox
#     of that name already exists.
#   * It never changes your working directory out from under you: all work
#     happens in a temp tree, and the final check asserts $PWD is what it was.
#   * It restores host state it changed (a serve it installed is uninstalled,
#     which also covers a serve it stopped mid-run; an MCP catalog bundle it
#     registered is removed again) in a trap that runs on every exit path.
#   * It never certifies a stale daemon: host-service checks preflight WHICH
#     pix-host binary an already-running serve is, and refuse rather than
#     silently test one that does not match this build.
#   * That preflight + the reversible launchd install run BEFORE this script's
#     first `pix run` (section [2], ahead of the lifecycle sections), not
#     after. Section [8]'s host-service checks then CONSUME that already-up
#     daemon; they never preflight or lazily install it a second time.
#   * Multi-shell holds (section [6]) are two REAL `pix run` sessions blocked
#     on their own named FIFO, never a `-p` prompt: no model call is ever
#     placed, and each is released — one, then the other — by closing that
#     FIFO's write end, a deterministic EOF, not a guessed `sleep`. Every FD
#     opened for a hold is force-closed in the exit trap, on every exit path,
#     and each backgrounded holder closes BOTH hold FDs for itself first (its
#     own subshell, not just the exec'd `pix`) so the SECOND background job
#     can never inherit the FIRST job's still-open write end — an inherited
#     write fd on an unrelated process is exactly what would keep a FIFO's
#     reader from ever seeing EOF, deadlocking the release this section
#     depends on.
#   * The optional interactive OAuth confirmation reads only from /dev/tty,
#     with a bound, and a missing TTY is SKIP, never FAIL — the real verdict
#     comes from a machine-readable probe (pix doctor --json), not a human's
#     say-so.
#   * The verdict is precedence-ordered: a `die`/abort (script exited non-zero
#     before reaching its own end) reports ABORTED/exit 2 even when earlier
#     sections already accumulated real FAILs — an aborted run is a different,
#     more urgent fact than "some checks failed" and must never be silently
#     downgraded to plain UAT FAILED.
#
# Exit codes: 0 all checks passed, 1 a check failed, 2 the run was incomplete
# (missing prerequisite, refused to start, cleanup could not finish, or an
# abort/die fired ahead of the accumulated pass/fail/skip counts).

set -uo pipefail

START_PWD="$(pwd -P)"
RUN_ID="uat-$$"
BOX_PREFIX="pix-${RUN_ID}"
WITH_OAUTH=0
WITH_SERVICES=1
PASS=0; FAIL=0; SKIP=0
# Readiness windows bounded appropriately for a COLD post-`make load` image
# pull: the first `pix run` against a freshly loaded image tag can spend real
# wall time pulling/extracting layers before the sandbox is even creatable,
# and a second shell's ATTACH still needs that same image already present.
# Both are overridable via env (never hand-edit a smaller magic number back
# into a call site instead) so a test harness can shrink them to exercise the
# same bounded-loop logic in a fraction of a second, and so production never
# silently regresses to a bound too short to survive a real cold pull.
UAT_CREATE_WAIT_SECS="${UAT_CREATE_WAIT_SECS:-180}"
UAT_ATTACH_WAIT_SECS="${UAT_ATTACH_WAIT_SECS:-90}"
# Poll interval every bounded_wait_* loop below sleeps between checks.
# Production default is one real second per poll (so a MAXSECS argument reads
# as wall-clock seconds); a test harness can shrink this so a MAXSECS well
# past 30 — proving the windows above actually outlive the old too-short
# bound — costs a fraction of a real second instead of real minutes.
UAT_POLL_INTERVAL="${UAT_POLL_INTERVAL:-1}"
CREATED_BOXES=()
EXTRA_BOXES=()   # digest-named boxes this run created (names discovered, not chosen)
INSTALLED_SERVE=0
MCP_ADDED_NAMES=()        # exact registrations this run added; remove only these in cleanup
HOLD_PIDS=()              # pix run pids backgrounded behind a hold FIFO (section [6])
WORK=""

for arg in "$@"; do
  case "$arg" in
    --with-oauth) WITH_OAUTH=1 ;;
    --no-services) WITH_SERVICES=0 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) printf 'unknown flag: %s\n' "$arg" >&2; exit 2 ;;
  esac
done

# --- reporting -----------------------------------------------------------------
red() { printf '\033[31m%s\033[0m' "$*"; }
grn() { printf '\033[32m%s\033[0m' "$*"; }
ylw() { printf '\033[33m%s\033[0m' "$*"; }
pass() { PASS=$((PASS+1)); printf '  %s %s\n' "$(grn PASS)" "$1"; }
fail() { FAIL=$((FAIL+1)); printf '  %s %s\n' "$(red FAIL)" "$1"; [ $# -gt 1 ] && printf '       %s\n' "$2"; return 0; }
skip() { SKIP=$((SKIP+1)); printf '  %s %s — %s\n' "$(ylw SKIP)" "$1" "${2:-no reason given}"; }
head1() { printf '\n\033[1m%s\033[0m\n' "$*"; }
die()  { printf '\n%s %s\n' "$(red 'INCOMPLETE:')" "$*" >&2; exit 2; }

# assert_exit CODE "name" cmd... — runs cmd, compares its exit status.
assert_exit() {
  local want="$1" name="$2"; shift 2
  local out; out="$("$@" 2>&1)"; local got=$?
  if [ "$got" = "$want" ]; then pass "$name (exit $got)"
  else fail "$name" "wanted exit $want, got $got: $(printf '%s' "$out" | tail -3 | tr '\n' ' ')"; fi
}

# assert_contains "needle" "name" cmd... — runs cmd, greps its combined output.
assert_contains() {
  local needle="$1" name="$2"; shift 2
  local out; out="$("$@" 2>&1)"
  if printf '%s' "$out" | grep -qF -- "$needle"; then pass "$name"
  else fail "$name" "output did not contain '$needle': $(printf '%s' "$out" | tail -3 | tr '\n' ' ')"; fi
}

# unit_num JSON FIELD — the numeric FIELD of the memory unit inside a
# `serve status --json` payload (indented JSON, one field per line). Reading it
# with awk beats a regex over the whole document, which would happily return
# serve's own top-level "pid".
unit_num() {
  printf '%s' "$1" | awk -v f="\"$2\"" '
    index($0, "\"name\": \"memory\"") > 0 { inmem = 1 }
    inmem && index($0, f) > 0 { gsub(/[^0-9]/, "", $0); if ($0 != "") { print; exit } }'
}

# port_open HOST PORT — nc when present, bash /dev/tcp otherwise.
port_open() {
  if command -v nc >/dev/null 2>&1; then nc -z "$1" "$2" >/dev/null 2>&1
  else (exec 3<>"/dev/tcp/$1/$2") >/dev/null 2>&1; fi
}

# serve_is_running — the machine-readable answer to "is serve up", read from
# `pix serve status --json`'s own boolean `running` field. A text check like
# `pix serve status | grep -q running` is NOT safe: the human-readable line
# for the down state is literally "serve: not running", which CONTAINS the
# substring "running" — so that grep matches (and reports "found") in BOTH
# states, making `! grep -q running` permanently false and silently skipping
# whatever it was meant to gate (e.g. installing serve when it is actually
# down). The JSON boolean has no such substring trap.
serve_is_running() {
  printf '%s' "$(pix serve status --json 2>/dev/null)" | grep -q '"running": *true'
}

# bounded_wait PID MAXSECS — polls once a second for PID to exit, up to
# MAXSECS. A background `pix run` that wedges must never hang the whole UAT
# run on a bare `wait`: past the bound it is killed and `wait` still reaps it
# (avoiding a zombie), and the caller gets a definite answer either way.
# Returns 0 if the process exited on its own, 1 if it had to be killed.
bounded_wait() {
  local pid="$1" max="${2:-60}" i=0
  while kill -0 "$pid" 2>/dev/null; do
    i=$((i+1))
    if [ "$i" -ge "$max" ]; then
      kill -9 "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null
      return 1
    fi
    sleep "$UAT_POLL_INTERVAL"
  done
  wait "$pid" 2>/dev/null
  return 0
}

# fifo_release FD — closes the write end held at file descriptor FD, sending
# EOF to whatever process is blocked reading the far end of that FIFO. This is
# the deterministic "release" primitive behind the multi-shell FIFO holds
# (section [6]): the reader's next read() returns 0 the instant this close
# happens — no data ever crosses the pipe, so nothing sent could ever be
# mistaken for a submitted prompt (a model call). Idempotent: closing an
# already-closed (or never-opened) fd is swallowed, not reported, so a
# repeated call — including from the exit trap, after a normal release
# already ran — is always safe.
fifo_release() {
  local fd="$1"
  eval "exec ${fd}>&-" 2>/dev/null
  return 0
}

# ls_json_lists NAME — the machine-readable answer to "does pix ls --json
# positively list NAME right now", distinguishing a genuine ABSENCE (the
# command ran fine, NAME just is not in it) from an ls FAILURE (the command
# itself did not run cleanly). Echoes exactly one of LISTED / ABSENT /
# ERROR:<stderr tail> — never collapses the latter into the former the way a
# bare `pix ls | grep -q NAME` does (grep -q says "not found" for both an
# empty/absent listing AND a totally failed command).
ls_json_lists() {
  local name="$1" out rc errfile
  errfile="$(mktemp "${TMPDIR:-/tmp}/pix-uat-lsjson.XXXXXX")"
  out="$(pix ls --json 2>"$errfile")"; rc=$?
  if [ "$rc" -ne 0 ]; then
    printf 'ERROR:%s\n' "$(tr '\n' ' ' <"$errfile")"
    rm -f "$errfile"
    return 0
  fi
  rm -f "$errfile"
  if printf '%s' "$out" | grep -qE "\"name\": *\"${name}\""; then
    printf 'LISTED\n'
  else
    printf 'ABSENT\n'
  fi
}

# bounded_wait_listed NAME MAXSECS — polls ls_json_lists once a second, up to
# MAXSECS, for a POSITIVE listing. A FIFO write end connecting only proves the
# backgrounded shell opened stdin for reading — it says nothing about whether
# pix itself has created or attached the sandbox yet, so callers must wait on
# this (a real, machine-readable fact) rather than treat the FIFO connect
# itself as "up". Returns 0 once LISTED, 1 on timeout while still ABSENT, 2
# the instant ls_json_lists reports ERROR (never conflated with "still
# absent" — a failing probe is a different fact than a genuinely empty one).
bounded_wait_listed() {
  local name="$1" max="${2:-30}" i=0 state
  while [ "$i" -lt "$max" ]; do
    state="$(ls_json_lists "$name")"
    case "$state" in
      LISTED) return 0 ;;
      ERROR:*) printf '%s\n' "$state" >&2; return 2 ;;
    esac
    i=$((i+1))
    sleep "$UAT_POLL_INTERVAL"
  done
  return 1
}

# bounded_wait_attach_log LOG PID MAXSECS — polls once a second, up to
# MAXSECS, for pix's own "attaching" evidence (run_cmd.go prints it to
# stderr on every attach) to appear in LOG, AND for PID to still be alive at
# that instant. A FIFO connect proves only that PID opened stdin, never that
# a SECOND live RunSession reference actually attached — this is the real
# observable proof of that second reference. Returns 0 once both hold
# together, 1 on timeout, and 1 immediately if PID has already exited (a
# dead process past this point can never become that live second reference,
# no matter how much longer this waits).
bounded_wait_attach_log() {
  local log="$1" pid="$2" max="${3:-30}" i=0
  while [ "$i" -lt "$max" ]; do
    if ! kill -0 "$pid" 2>/dev/null; then
      return 1
    fi
    if grep -qi "attaching" "$log" 2>/dev/null; then
      kill -0 "$pid" 2>/dev/null && return 0
      return 1
    fi
    i=$((i+1))
    sleep "$UAT_POLL_INTERVAL"
  done
  return 1
}

# bounded_wait_absent NAME MAXSECS — the teardown-side counterpart to
# bounded_wait_listed: teardown is not necessarily synchronous with the last
# holding shell's process exit, so this polls up to MAXSECS for a positive
# ABSENT reading rather than trusting a single immediate probe. Same
# ERROR-vs-absent distinction: return 0 once ABSENT, 1 on timeout while still
# LISTED, 2 the instant ls_json_lists itself fails (never treated as proof of
# teardown).
bounded_wait_absent() {
  local name="$1" max="${2:-30}" i=0 state
  while [ "$i" -lt "$max" ]; do
    state="$(ls_json_lists "$name")"
    case "$state" in
      ABSENT) return 0 ;;
      ERROR:*) printf '%s\n' "$state" >&2; return 2 ;;
    esac
    i=$((i+1))
    sleep "$UAT_POLL_INTERVAL"
  done
  return 1
}

# assert_box_listed / assert_box_not_listed NAME LABEL — the pass/fail wrapper
# around the bounded ls_json_lists waits that section [6] asserts through, so
# every presence check there is both bounded (a wedged `pix ls` cannot hang
# this script) and distinguishes an ls FAILURE from a genuine absence/
# presence, instead of silently treating both the same way a bare `grep -q`
# on a possibly-failed command would.
assert_box_listed() {
  local name="$1" label="$2" rc
  bounded_wait_listed "$name" "$UAT_CREATE_WAIT_SECS"; rc=$?
  case "$rc" in
    0) pass "$label" ;;
    2) fail "$label" "pix ls --json itself failed (not merely absent)" ;;
    *) fail "$label" "pix ls --json ran fine but did not list $name within ${UAT_CREATE_WAIT_SECS}s" ;;
  esac
}
assert_box_not_listed() {
  local name="$1" label="$2" rc
  bounded_wait_absent "$name" "$UAT_CREATE_WAIT_SECS"; rc=$?
  case "$rc" in
    0) pass "$label" ;;
    2) fail "$label" "pix ls --json itself failed (cannot confirm teardown, not proof of it)" ;;
    *) fail "$label" "$name is still listed after ${UAT_CREATE_WAIT_SECS}s" ;;
  esac
}

# resolve_symlink_final PATH — follows a POSIX `readlink` chain for PATH's
# final component only (macOS ships no `readlink -f`/`realpath` guarantee, so
# neither can be assumed). This is the piece that makes a make-install
# symlink (e.g. /usr/local/bin/pix-host -> ../Cellar/pix/1.2.3/bin/pix-host)
# and the real executable `lsof` reports for an already-running process
# compare equal: without it, abs_path would realpath only the DIRECTORY and
# leave the symlinked basename unresolved, so the two paths would never match
# even though they name the same file. Bounded to 32 hops against a symlink
# cycle (dangling or self-referential); a RELATIVE link target is resolved
# against the symlink's OWN directory, per readlink(2)/POSIX semantics, not
# the caller's cwd. Every path stays one string end to end (never split on
# whitespace), so a target containing spaces survives intact.
resolve_symlink_final() {
  local p="$1" hops=0 target dir
  while [ -L "$p" ] && [ "$hops" -lt 32 ]; do
    target="$(readlink "$p")" || break
    case "$target" in
      /*) p="$target" ;;
      *) dir="$(dirname "$p")"; p="$dir/$target" ;;
    esac
    hops=$((hops+1))
  done
  printf '%s\n' "$p"
}

# abs_path PATH — realpath of an existing file without touching $PWD (a `cd`
# in a subshell), so callers can compare two binaries by path even when one
# arrived as a relative/symlinked name. Resolves a symlinked FINAL component
# (resolve_symlink_final) before realpath-ing the containing directory, so a
# symlink and the real file it ultimately targets always compare equal. Empty
# in, empty out.
abs_path() {
  local p="$1"
  [ -n "$p" ] && [ -e "$p" ] || return 0
  p="$(resolve_symlink_final "$p")"
  (cd "$(dirname "$p")" 2>/dev/null && printf '%s/%s\n' "$(pwd -P)" "$(basename "$p")")
}

# current_bin_path NAME — the absolute, symlink-resolved path NAME would run
# as right now (empty if NAME is not on PATH).
current_bin_path() { abs_path "$(command -v "$1" 2>/dev/null)"; }

# running_bin_path PID — best-effort absolute path of PID's executable, so the
# host-services section can tell an already-running serve APART from the
# pix-host this run just built, instead of silently trusting whatever answers
# :11435. Empty means "could not tell" — never treat that as a match.
running_bin_path() {
  local pid="$1" p=""
  if command -v lsof >/dev/null 2>&1; then
    p="$(lsof -p "$pid" 2>/dev/null | awk '$4=="txt"{print $NF; exit}')"
  fi
  if [ -z "$p" ]; then
    p="$(ps -p "$pid" -o comm= 2>/dev/null | awk '{$1=$1; print; exit}')"
  fi
  abs_path "$p"
}

# --- guards --------------------------------------------------------------------
[ -f /.dockerenv ] && die "this is the HOST script; it cannot run inside a sandbox"
[ -n "${PIX_IN_SANDBOX:-}" ] && die "PIX_IN_SANDBOX is set; run this on the host"
command -v pix >/dev/null 2>&1 || die "pix is not on PATH (make install)"
command -v sbx >/dev/null 2>&1 || die "sbx is not on PATH"
command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
[ "$(uname)" = "Darwin" ] || printf '%s\n' "$(ylw 'note:') not macOS — the launchd checks will be skipped, everything else still runs"

# The script must not contain the destructive shapes it forbids. Self-checking is
# cheap insurance against a future edit reintroducing them.
# SELFCHECK: executable lines only (comments and this check are excluded), so a
# future edit that adds a blast-radius flag stops the run instead of running it.
if grep -vE '^[[:space:]]*#|SELFCHECK' "$0" | grep -qE '(^|[^-])pix rm ([^|]*)(--all|--force|[[:space:]]-f([[:space:]]|$))'; then
  die "a blast-radius removal flag (all/force) appeared in this script — refusing to run"
fi

if pix ls 2>/dev/null | grep -q "$BOX_PREFIX"; then
  die "a sandbox named ${BOX_PREFIX}* already exists; remove it before re-running"
fi

# --- cleanup: only what we made, always ----------------------------------------
cleanup() {
  local rc=$?
  head1 "Cleanup"
  # Deterministic FIFO holds (section [6]): force-release both writer fds and
  # kill any held pix run process still alive, so an abort mid-hold (a `die`
  # elsewhere, or the script erroring out before it reached its own release
  # lines) can never leave an orphaned process or a leaked file descriptor
  # behind. fifo_release is idempotent, so a normal run that already released
  # both fds pays nothing extra here.
  fifo_release 5
  fifo_release 6
  for p in "${HOLD_PIDS[@]:-}"; do
    [ -z "$p" ] && continue
    kill -0 "$p" 2>/dev/null && kill -9 "$p" 2>/dev/null
    wait "$p" 2>/dev/null
  done
  for b in "${CREATED_BOXES[@]:-}" "${EXTRA_BOXES[@]:-}"; do
    [ -z "$b" ] && continue
    # Only two ways a name gets here: this script chose it (our prefix), or this
    # script watched it appear during its own run (EXTRA_BOXES).
    case " ${EXTRA_BOXES[*]:-} " in
      *" $b "*) ;;
      *) case "$b" in
           "$BOX_PREFIX"*) ;;
           *) printf '  refusing to remove %s: not ours\n' "$b"; continue ;;
         esac ;;
    esac
    if pix ls 2>/dev/null | grep -q "^\s*$b\b\|$b"; then
      pix rm "$b" >/dev/null 2>&1 || printf '  could not remove %s (remove it by hand: pix rm %s)\n' "$b" "$b"
    fi
  done
  if [ "$INSTALLED_SERVE" = 1 ]; then
    pix serve uninstall >/dev/null 2>&1 \
      && printf '  launchd service uninstalled\n' \
      || printf '  %s could not uninstall the launchd service; run: pix serve uninstall\n' "$(red '!')"
  fi
  for s in "${MCP_ADDED_NAMES[@]:-}"; do
    [ -z "$s" ] && continue
    sbx mcp rm "$s" >/dev/null 2>&1 \
      && printf '  MCP server %s removed (restored pre-run state)\n' "$s" \
      || printf '  %s could not remove MCP server %s; run: sbx mcp rm %s\n' "$(red '!')" "$s" "$s"
  done
  [ -n "$WORK" ] && rm -rf "$WORK"
  # cwd safety is an ASSERTION, not a hope.
  if [ "$(pwd -P)" != "$START_PWD" ]; then
    printf '  %s working directory changed: %s -> %s\n' "$(red 'FAIL')" "$START_PWD" "$(pwd -P)"
    FAIL=$((FAIL+1))
  fi
  if pix ls 2>/dev/null | grep -q "$BOX_PREFIX"; then
    printf '  %s sandboxes from this run survived cleanup:\n' "$(red 'FAIL')"
    pix ls | grep "$BOX_PREFIX"
    FAIL=$((FAIL+1))
  fi
  verdict "$rc"
}
verdict() {
  local rc="${1:-0}"
  head1 "Result"
  printf '  %d passed, %d failed, %d skipped\n' "$PASS" "$FAIL" "$SKIP"
  # rc (die/abort) is checked FIRST, ahead of the accumulated FAIL count: a
  # `die` can fire after earlier sections already racked up real FAILs (e.g.
  # host services refusing a stale daemon in section [2], after section [1]
  # already failed a flag check), and that abort is a DIFFERENT, more urgent
  # fact than "some checks failed" — the run never reached its own end, so
  # whatever it did or did not get to assert past that point is unknown, not
  # failed. Reporting it as plain UAT FAILED (exit 1) would let a human read
  # the printed failed-count as the whole story and miss that the run was cut
  # short; ABORTED (exit 2, "incomplete") is the honest word for that.
  if [ "$rc" -ne 0 ]; then printf '  %s (script exited %d)\n' "$(red 'UAT ABORTED')" "$rc"; exit 2; fi
  if [ "$FAIL" -gt 0 ]; then printf '  %s\n' "$(red 'UAT FAILED')"; exit 1; fi
  if [ "$PASS" -eq 0 ]; then printf '  %s\n' "$(red 'UAT INCOMPLETE: nothing was actually asserted')"; exit 2; fi
  if [ "$SKIP" -gt 0 ]; then
    printf '  %s — %d check(s) could not run; this is NOT a clean release verdict\n' "$(ylw 'UAT INCOMPLETE')" "$SKIP"; exit 2
  fi
  printf '  %s\n' "$(grn 'UAT PASSED')"; exit 0
}
trap cleanup EXIT

WORK="$(mktemp -d "${TMPDIR:-/tmp}/pix-uat.XXXXXX")" || die "cannot create a work dir"

# newbox NAME DIR — records the name so cleanup can only ever remove ours.
newbox() { CREATED_BOXES+=("$1"); }

# runbox NAME DIR -- ARGS… — a non-interactive `pix run` (pi gets -p), returning
# pix's exit status. Interactive attach is what the human checklist covers.
runbox() {
  local name="$1" dir="$2"; shift 2
  newbox "$name"
  (cd "$dir" && pix run . --name "$name" "$@")
}

# --- 1. the CLI surface this script depends on actually exists -----------------
head1 "[1] Command + flag surface (nothing below runs a flag we did not verify)"
assert_exit 0 "pix help --all" pix help --all
assert_exit 0 "pix version" pix version
for verb in run rm ls serve mcp task status doctor; do
  assert_exit 0 "pix help $verb" pix help "$verb"
done
assert_contains "--keep" "pix run declares --keep" pix help run
assert_contains "--name" "pix run declares --name" pix help run
assert_contains "--json" "pix ls declares --json" pix help ls
assert_contains "--orphans" "pix rm declares --orphans" pix help rm
assert_contains "serve status" "pix serve declares status" pix help serve
# A retired verb must answer PIX_RETIRED and do nothing else.
assert_exit 2 "pix host is retired (exit 2)" pix host
assert_contains "PIX_RETIRED" "pix host names its replacement" pix host

# --- 2. host services: launchd preflight + install, BEFORE any pix run --------
# This runs before section [3]'s first `pix run`, on purpose: once a sandbox
# exists, installing/uninstalling the managed serve mid-lifecycle-test would
# make later serve state indistinguishable from "changed because the
# lifecycle checks changed it" rather than a clean precondition every later
# section (lifecycle AND host-services) can rely on. Section [8] below
# CONSUMES the daemon this section establishes; it does not preflight or
# install serve again — that would just be re-running this same lazy-start
# logic a second time, after sandboxes it should have preceded already exist.
if [ "$WITH_SERVICES" = 0 ]; then
  skip "host services preflight" "--no-services was passed"
else
head1 "[2] Host services: launchd preflight + install"
# Never silently test whatever daemon happens to answer :11435. If a serve is
# already running, prove it is THIS build's pix-host before section [8] (or
# anything else) relies on it — a stale, unmanaged daemon from before this
# rebuild would otherwise pass every check there for a binary this run never
# touched.
CUR_HOSTBIN="$(current_bin_path pix-host)"
printf '  testing pix-host binary: %s\n' "${CUR_HOSTBIN:-<pix-host not found on PATH>}"
RUNNING_PID="$(pix serve status --json 2>/dev/null | sed -n 's/.*"pid"[: ]*\([0-9]*\).*/\1/p' | head -1)"
if [ -n "$RUNNING_PID" ] && kill -0 "$RUNNING_PID" 2>/dev/null; then
  RUNNING_BIN="$(running_bin_path "$RUNNING_PID")"
  if [ -n "$CUR_HOSTBIN" ] && [ -n "$RUNNING_BIN" ]; then
    if [ "$RUNNING_BIN" = "$CUR_HOSTBIN" ]; then
      die "serve is already running (pid $RUNNING_PID) from this build, but a clean UAT must install and exercise the launchd-managed service itself. Run 'pix serve stop', then re-run this script; it will install the current build reversibly and uninstall it during cleanup."
    else
      die "a serve is already running (pid $RUNNING_PID, binary $RUNNING_BIN) that is NOT the pix-host this run just built ($CUR_HOSTBIN); testing it would certify a stale daemon. Fix: pix serve stop (or, if it is unmanaged: kill $RUNNING_PID), then re-run this script so it starts the current build."
    fi
  else
    die "serve pid $RUNNING_PID is live, but its executable or the current pix-host could not be resolved; cannot prove which binary would be tested. Stop it with 'pix serve stop' (or kill $RUNNING_PID if unmanaged), then re-run."
  fi
fi

if ! serve_is_running; then
  if pix serve install >/dev/null 2>&1; then
    INSTALLED_SERVE=1; sleep 5
    pass "installed and started serve from the current build ($CUR_HOSTBIN) — reversible: uninstalled on exit"
  else
    die "cannot start serve; the service checks cannot run"
  fi
fi
fi

# --- 3. digest naming: same basename, different workspaces ---------------------
head1 "[3] Digest-suffixed sandbox naming (two workspaces, one basename)"
mkdir -p "$WORK/a/proj" "$WORK/b/proj"
PRE_BOXES="$(pix ls 2>/dev/null | awk 'NR>1{print $1}' | sort)"
(cd "$WORK/a/proj" && pix run . --keep -- -p 'digest a' >/dev/null 2>&1)
(cd "$WORK/b/proj" && pix run . --keep -- -p 'digest b' >/dev/null 2>&1)
POST_BOXES="$(pix ls 2>/dev/null | awk 'NR>1{print $1}' | sort)"
DIGEST_BOXES="$(comm -13 <(printf '%s\n' "$PRE_BOXES") <(printf '%s\n' "$POST_BOXES"))"
# Whatever they are called, they are OURS: record them so cleanup removes exactly
# these and nothing else.
while read -r b; do [ -n "$b" ] && EXTRA_BOXES+=("$b"); done <<<"$DIGEST_BOXES"
DIGEST_COUNT="$(printf '%s\n' "$DIGEST_BOXES" | grep -c . )"
if [ "$DIGEST_COUNT" -ne 2 ]; then
  fail "digest naming" "two same-basename workspaces produced $DIGEST_COUNT new sandbox(es), not 2 (they aliased, or a launch failed)"
else
  N1="$(printf '%s\n' "$DIGEST_BOXES" | sed -n 1p)"
  N2="$(printf '%s\n' "$DIGEST_BOXES" | sed -n 2p)"
  if [ "$N1" = "$N2" ]; then
    fail "digest naming" "both workspaces resolved to $N1"
  elif printf '%s' "$N1" | grep -qE '^pix-proj-[0-9a-f]+$' && printf '%s' "$N2" | grep -qE '^pix-proj-[0-9a-f]+$'; then
    pass "digest naming: $N1 != $N2, both digest-suffixed from basename 'proj'"
  else
    fail "digest naming" "names are not the documented pix-<basename>-<digest> shape: $N1 / $N2"
  fi
fi

# --- 4. launch, instance identity, and the attach fingerprint ------------------
# BOX1 is launched with --keep. A plain non-interactive `pix run` (no --keep)
# tears itself down the instant its inner `-p` command exits — which is
# EXACTLY what a non-interactive launch does — so every check below that
# depends on BOX1 still existing (pix ls, the exact record.json path, its
# instance id, and the changed-MCP fingerprint-refusal attach) would race a
# teardown that has often already won by the time this script's very next
# line runs, producing a false FAIL that has nothing to do with the thing
# being tested. --keep only changes WHEN the box is torn down, never HOW an
# attach is validated: SessionFingerprint (services/host/workflow/launch/
# session.go) is computed purely from the static MCP set + template, with no
# Keep field in it at all, so keeping BOX1 alive here cannot mask (nor fake)
# the fingerprint-refusal check below — that refusal is exercised for real,
# against a box that is only still around to attach to because it was kept.
# Section [5] reuses this SAME kept box for exit-status propagation, then
# removes it explicitly, before section [6]'s own $BOX2 is ever created.
head1 "[4] Launch, instance record, attach fingerprint"
BOX1="${BOX_PREFIX}-one"
LAUNCH1_OK=1
if runbox "$BOX1" "$WORK/a/proj" --keep -- -p 'print the single word ready' >"$WORK/run1.out" 2>"$WORK/run1.err"; then
  pass "pix run launched $BOX1 non-interactively"
else
  LAUNCH1_OK=0
  fail "pix run launched $BOX1" "stderr: $(tail -5 "$WORK/run1.err" 2>/dev/null | tr '\n' ' ')"
fi
assert_contains "$BOX1" "pix ls lists $BOX1" pix ls

# Lease state is keyed by the FINAL resolved sandbox name (o.Name in
# run_cmd.go), and --name travels verbatim, so with an explicit --name (as
# above) that name IS the key: the record path is deterministic, not a
# guess. The `find -newer $WORK` this replaces was unsound on its own terms
# on a host running ANY other concurrent pix activity: an unrelated pix run
# elsewhere on the same machine could write a record.json newer than $WORK
# under this SAME STATE_DIR, and `head -1` would happily pick up THAT one
# instead of ours.
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/pix"
REC="$STATE_DIR/sandboxes/$BOX1/record.json"
if [ "$LAUNCH1_OK" = 1 ]; then
  if [ -f "$REC" ]; then
    INSTANCE="$(sed -n 's/.*"instance_id"[: ]*"\([^"]*\)".*/\1/p' "$REC")"
    if [ -n "$INSTANCE" ]; then pass "lease record carries an instance id ($INSTANCE)"
    else fail "lease record instance id" "no instance_id in $REC: $(tr '\n' ' ' <"$REC")"; fi
  else
    fail "lease instance record" "no record.json at exact path $REC; launch stderr: $(tail -5 "$WORK/run1.err" 2>/dev/null | tr '\n' ' ')"
  fi
else
  skip "lease instance record" "pix run launched $BOX1 failed above; no record is expected — see its captured stderr"
fi

# An attach whose create-time MCP set no longer matches must be REFUSED, not
# silently attached: that is the fingerprint gate.
if OUT="$( (cd "$WORK/a/proj" && pix run . --name "$BOX1" --mcp definitely-not-registered -- -p hi) 2>&1 )"; then
  fail "attach fingerprint gate" "an attach with a changed MCP set was accepted: $(printf '%s' "$OUT" | tail -2 | tr '\n' ' ')"
else
  pass "attach fingerprint gate refused a changed MCP set"
fi

# --- 5. exit-code propagation (the last shell's status is pix's status) --------
head1 "[5] Exit status propagation"
(cd "$WORK/a/proj" && pix run . --name "$BOX1" -- --definitely-not-a-pi-flag) >/dev/null 2>&1
RC=$?
if [ "$RC" -eq 0 ]; then fail "last-exit propagation" "a failing inner command produced exit 0"
else pass "last-exit propagation (inner failure surfaced as exit $RC)"; fi
assert_exit 2 "bare 'pix <not-a-dir>' refuses" pix definitely-not-a-directory-"$RUN_ID"

# $BOX1 was kept alive (section [4]'s --keep) purely so the record/instance-id/
# fingerprint-refusal checks there, and the exit-propagation check just above,
# had a live box to inspect and attach to. Every one of those inspections has
# now run to completion, so remove it explicitly here — mirroring section
# [7]'s "an explicit 'pix rm' still removes a kept sandbox" check — rather
# than letting it ride to the end-of-script cleanup trap. That keeps it from
# ever leaking into, or colliding with, section [6]'s own $BOX2, and proves
# the explicit-rm-of-a-kept-box path a second, independent time.
if pix rm "$BOX1" >/dev/null 2>&1 && ! pix ls 2>/dev/null | grep -q "$BOX1"; then
  pass "explicit 'pix rm' removes the kept $BOX1 sandbox before section [6]"
else
  fail "explicit rm of kept $BOX1" "$BOX1 is still listed after 'pix rm $BOX1'"
fi

# --- 6. multi-shell references and teardown ------------------------------------
head1 "[6] Multi-shell references: the LAST shell tears down (deterministic FIFO holds)"
BOX2="${BOX_PREFIX}-multi"
newbox "$BOX2"
# Two REAL `pix run` invocations, neither ever given a -p prompt: each is an
# actual interactive RunSession holding its own reference to $BOX2, and
# neither can ever place a model call because neither ever sends a message.
# Each reads its stdin from its OWN named FIFO instead of a prompt string:
#   * opening a FIFO for read blocks until a writer connects, so the
#     backgrounded `pix run` does not even exec until this script opens the
#     matching write end below — that connect (not a guessed `sleep`) is the
#     deterministic "it is really up now" signal;
#   * holding the write end open then keeps the session alive indefinitely
#     (nothing queued to read, no EOF);
#   * fifo_release closes the write end on cue, delivering EOF to that ONE
#     session the instant we choose — releasing shell 1 and then shell 2 is
#     therefore an ORDERED, exact sequence, never a race against a sleep. No
#     byte ever crosses either pipe, so nothing sent could ever be mistaken
#     for a submitted prompt either.
# bounded_wait remains the backstop after each release: if a held session
# does not treat stdin EOF as a clean exit, it is still killed and reported
# rather than hanging the rest of this script.
FIFO_DIR="$WORK/holds"
mkdir -p "$FIFO_DIR" || die "cannot create the FIFO hold directory"
FIFO1="$FIFO_DIR/shell1"; FIFO2="$FIFO_DIR/shell2"
mkfifo "$FIFO1" "$FIFO2" || die "cannot create the multi-shell hold FIFOs"
LOG1="$FIFO_DIR/shell1.log"; LOG2="$FIFO_DIR/shell2.log"

# Each backgrounded job closes fds 5 and 6 for ITSELF first (`exec 5>&- 6>&-`
# as its own first command, not a redirect merely attached to `pix run`):
# without that, the SECOND job below would fork while fd 5 is already open in
# this script and inherit that write end into its own subshell process, which
# never execs it away and never closes it either — a silent extra writer that
# would keep FIFO1 from ever reaching EOF no matter how deterministically we
# close OUR fd 5 later. Order-independent: each job closes both, whichever it
# does or does not yet need. Each job's own combined stdout+stderr goes to its
# OWN log file (never /dev/null) so this script can later look for pix's own
# evidence of what it did, not just infer it from a pipe connecting.
(exec 5>&- 6>&-; cd "$WORK/b/proj" && pix run . --name "$BOX2" <"$FIFO1" >"$LOG1" 2>&1) &
SH1=$!
HOLD_PIDS+=("$SH1")
exec 5>"$FIFO1" || die "cannot open the shell-1 hold FIFO for writing"

# The write-end open above only returned once shell 1's backgrounded `pix run`
# connected its OWN read end — but that connect proves only that the shell
# opened stdin for reading, the same thing an `exec <"$FIFO1"` with no pix
# behind it at all would also prove. It says nothing about whether pix itself
# has created $BOX2 yet. Wait for the real, machine-readable fact instead:
# pix ls --json positively listing it, bounded so a wedged create cannot hang
# this script, and never conflating an ls FAILURE with a still-absent box.
WL1=$(bounded_wait_listed "$BOX2" "$UAT_CREATE_WAIT_SECS"; echo $?)
case "$WL1" in
  0) pass "$BOX2 observed via pix ls --json after the first shell's FIFO connected" ;;
  2) fail "$BOX2 observed via pix ls --json" "pix ls --json itself failed (see stderr above) — not merely absent" ;;
  *) fail "$BOX2 observed via pix ls --json" "not listed within ${UAT_CREATE_WAIT_SECS}s of the first shell's FIFO connecting: $(tail -3 "$LOG1" 2>/dev/null | tr '\n' ' ')" ;;
esac

(exec 5>&- 6>&-; cd "$WORK/b/proj" && pix run . --name "$BOX2" <"$FIFO2" >"$LOG2" 2>&1) &
SH2=$!
HOLD_PIDS+=("$SH2")
exec 6>"$FIFO2" || die "cannot open the shell-2 hold FIFO for writing"

# Same caveat, sharper here: $BOX2 already exists (shell 1 created it), so
# shell 2's FIFO connect proves only that ITS OWN stdin opened — never that a
# SECOND live RunSession reference actually attached. The real proof is pix's
# own "attaching" evidence in shell 2's OWN log (run_cmd.go prints it on
# every attach) together with shell 2's process still being alive at that
# instant — a dead process past this point could never be that second
# reference no matter how long this waited.
if bounded_wait_attach_log "$LOG2" "$SH2" "$UAT_ATTACH_WAIT_SECS"; then
  pass "$BOX2's second shell observably attached (log evidence + live process)"
else
  fail "$BOX2's second shell observably attached" "no 'attaching' evidence in $LOG2 with pid $SH2 still alive within ${UAT_ATTACH_WAIT_SECS}s: $(tail -3 "$LOG2" 2>/dev/null | tr '\n' ' ')"
fi

assert_box_listed "$BOX2" "$BOX2 is up with two shells attached"

fifo_release 5   # release shell 1's hold: it sees EOF now, deterministically
if bounded_wait "$SH1" 60; then :; else fail "first shell exited" "background pix run (pid $SH1) did not exit within 60s and was killed"; fi
assert_box_listed "$BOX2" "sandbox survives the FIRST shell leaving"

fifo_release 6   # release shell 2's hold: the LAST reference goes with it
if bounded_wait "$SH2" 60; then :; else fail "second shell exited" "background pix run (pid $SH2) did not exit within 60s and was killed"; fi
assert_box_not_listed "$BOX2" "teardown on last shell exit"

# --- 7. --keep, and the orphan reaper that must respect it ---------------------
head1 "[7] --keep marker and orphan reaping"
BOX3="${BOX_PREFIX}-keep"
newbox "$BOX3"
(cd "$WORK/b/proj" && pix run . --name "$BOX3" --keep -- -p 'kept box' >/dev/null 2>&1)
sleep 3
if pix ls 2>/dev/null | grep -q "$BOX3"; then pass "--keep survived the last shell exiting"
else fail "--keep survived the last shell exiting" "$BOX3 was torn down despite --keep"; fi
pix rm --orphans >/dev/null 2>&1
if pix ls 2>/dev/null | grep -q "$BOX3"; then pass "orphan reaper refuses a kept sandbox"
else fail "orphan reaper refuses a kept sandbox" "--orphans removed a --keep box"; fi
if pix rm "$BOX3" >/dev/null 2>&1 && ! pix ls 2>/dev/null | grep -q "$BOX3"; then
  pass "an explicit 'pix rm' still removes a kept sandbox"
else
  fail "explicit rm of a kept sandbox" "$BOX3 is still listed"
fi

# --- 8. host services: consume the ALREADY-installed managed daemon -----------
if [ "$WITH_SERVICES" = 0 ]; then
  skip "host services" "--no-services was passed"
else
head1 "[8] Host services: serve status, launchd restart, memory unit restart"
assert_exit 0 "pix serve status" pix serve status
assert_contains "schema_version" "pix doctor --json is machine-readable" pix doctor --json

# The launchd preflight + install already ran in section [2] — BEFORE any
# sandbox in this run existed. This section CONSUMES that managed daemon
# ($CUR_HOSTBIN, $INSTALLED_SERVE) rather than preflighting or installing it
# again here: a second lazy-start attempt this late would just be re-running
# the same gate after the lifecycle sections it was meant to precede have
# already created and torn down sandboxes.
UNITS="$(pix serve status --json 2>/dev/null)"
if printf '%s' "$UNITS" | grep -q '"units"'; then
  pass "serve status --json publishes the supervision tree"
  for field in '"identity"' '"state"' '"restarts"' '"generation"' '"reattached"' '"last_probe_us"'; do
    if printf '%s' "$UNITS" | grep -q "$field"; then pass "unit report carries $field"
    else fail "unit report carries $field" "missing from serve status --json"; fi
  done
  # The published snapshot must not carry a credential: assert on the shapes.
  # But do it HONESTLY: a negative regex over an EMPTY units[] trivially finds
  # nothing and would pass "no secrets" without ever scanning a single unit's
  # worth of real data. In a service-enabled full run at least the memory unit
  # is always supervised (AGENTS.md: memory ALWAYS runs as a supervised
  # go-plugin unit), so a zero-unit snapshot here is a real gap, not proof of
  # cleanliness — fail it explicitly instead of letting the regex pass vacuously.
  UNIT_COUNT="$(printf '%s' "$UNITS" | grep -c '"identity"')"
  if [ "$UNIT_COUNT" -eq 0 ]; then
    fail "snapshot carries no secrets" "0 units in serve status --json; cannot honestly certify a secret scan over units that were never present in a service-enabled full run"
  elif printf '%s' "$UNITS" | grep -qiE '(sk-[a-z0-9-]{16,}|xox[baprs]-|Bearer [A-Za-z0-9._-]{8,})'; then
    fail "snapshot carries no secrets" "credential-shaped text in serve status --json"
  else pass "snapshot carries no secrets ($UNIT_COUNT unit(s) scanned)"; fi
else
  fail "serve status --json publishes the supervision tree" "no units[] in the JSON"
fi
assert_contains '"supervisor"' "doctor --json carries the supervisor object" pix doctor --json

# memory unit restart: kill the CHILD, prove the listener never dropped.
MEM_PID="$(unit_num "$UNITS" pid)"
if [ -z "$MEM_PID" ]; then
  MEM_PID="$(pgrep -f 'pix-host plugin memory' | head -1)"
fi
GEN0="$(unit_num "$UNITS" generation)"; GEN0="${GEN0:-0}"
if [ -n "$MEM_PID" ] && kill -0 "$MEM_PID" 2>/dev/null; then
  kill -9 "$MEM_PID" 2>/dev/null
  DROPPED=0
  for _ in $(seq 1 60); do
    if ! port_open 127.0.0.1 11435; then DROPPED=1; fi
    NEW="$(pix serve status --json 2>/dev/null)"
    GEN1="$(unit_num "$NEW" generation)"
    if [ -n "$GEN1" ] && [ "${GEN1:-0}" -gt "${GEN0:-0}" ] && printf '%s' "$NEW" | grep -q '"state": *"running"'; then
      break
    fi
    sleep 1
  done
  if [ "$DROPPED" = 1 ]; then fail ":11435 stayed bound across the memory restart" "the port stopped accepting connections"
  else pass ":11435 stayed bound across the memory restart"; fi
  if [ -n "${GEN1:-}" ] && [ "${GEN1:-0}" -gt "${GEN0:-0}" ]; then pass "memory unit restarted (generation ${GEN0:-?} -> $GEN1)"
  else fail "memory unit restarted" "generation did not advance within 60s"; fi
  if pix memory stats >/dev/null 2>&1; then pass "memory answers again after the restart"
  else fail "memory answers again after the restart" "pix memory stats failed"; fi
else
  skip "memory unit restart" "no memory child pid to kill (is memory in config's services?)"
fi

# launchd: a managed daemon must come back by itself, and 'serve stop' must be
# mode-aware (a bare SIGTERM is undone by KeepAlive — invariant #3).
if [ "$(uname)" = "Darwin" ] && [ "$INSTALLED_SERVE" = 1 ]; then
  SERVE_PID="$(pix serve status --json 2>/dev/null | sed -n 's/.*"pid"[: ]*\([0-9]*\).*/\1/p' | head -1)"
  if [ -n "$SERVE_PID" ]; then
    kill -9 "$SERVE_PID" 2>/dev/null
    BACK=""
    for _ in $(seq 1 30); do
      sleep 1
      BACK="$(pix serve status --json 2>/dev/null | sed -n 's/.*"pid"[: ]*\([0-9]*\).*/\1/p' | head -1)"
      [ -n "$BACK" ] && [ "$BACK" != "$SERVE_PID" ] && break
    done
    if [ -n "$BACK" ] && [ "$BACK" != "$SERVE_PID" ]; then pass "launchd respawned serve ($SERVE_PID -> $BACK)"
    else fail "launchd respawned serve" "no new pid within 30s of killing $SERVE_PID"; fi
    if pix serve stop >/dev/null 2>&1; then
      sleep 10
      if ! serve_is_running; then pass "'serve stop' is mode-aware (KeepAlive did not respawn it)"
      else fail "'serve stop' is mode-aware" "serve came back after stop — it was stopped by pid, not through launchd"; fi
    else
      fail "'serve stop'" "returned non-zero"
    fi
  else
    skip "launchd respawn" "serve status published no pid"
  fi
elif [ "$(uname)" = "Darwin" ]; then
  skip "launchd respawn" "serve was already running unmanaged; this script does not take over your daemon"
else
  skip "launchd respawn" "not macOS"
fi
fi

# --- 9. external OAuth integrations (interactive, opt-in) ----------------------
head1 "[9] External OAuth-backed MCP servers"
if [ "$WITH_OAUTH" = 0 ]; then
  skip "remote OAuth servers" "--with-oauth not passed (this run is NOT a full release verdict)"
else
  PRE_MCP_LS="$(pix mcp ls 2>&1)"
  MISSING_CATALOG=()
  for s in notion atlassian granola; do
    if ! printf '%s' "$PRE_MCP_LS" | grep -qE "(^|[[:space:]])${s}([[:space:]]|$)"; then
      MISSING_CATALOG+=("$s")
    fi
  done
  if [ "${#MISSING_CATALOG[@]}" -eq 0 ]; then
    pass "shipped catalog servers are already registered (pre-existing host state; left unchanged)"
  elif pix mcp bundle >/dev/null 2>&1; then
    POST_BUNDLE_LS="$(pix mcp ls 2>&1)"
    for s in "${MISSING_CATALOG[@]}"; do
      if printf '%s' "$POST_BUNDLE_LS" | grep -qE "(^|[[:space:]])${s}([[:space:]]|$)"; then
        MCP_ADDED_NAMES+=("$s")
      fi
    done
    pass "pix mcp bundle registers the missing public catalog servers"
  else
    fail "pix mcp bundle registers the public catalog" "pix mcp bundle exited non-zero"
  fi
  assert_exit 0 "pix mcp ls" pix mcp ls
  for s in notion atlassian granola; do
    assert_contains "$s" "catalog server $s is registered" pix mcp ls
  done

  # Before forcing any browser flow, ask what's ALREADY true: pix doctor --json
  # already carries the exact registered+authenticated evidence line for a
  # remote catalog server (health/mcp.go's attachmentCaveat), so a server that
  # is already registered and authenticated from a prior run — the common case
  # on a host that has done this before — is certified a PASS right here,
  # with zero browser flow invoked for it. Only servers doctor cannot already
  # certify are gaps; ONLY those are authorized, and each individually.
  PRE_AUTH_DOCTOR="$(pix doctor --json 2>/dev/null)"
  AUTH_GAPS=()
  for s in notion atlassian granola; do
    if printf '%s' "$PRE_AUTH_DOCTOR" | grep -qF "$s: registered (host registration"; then
      : # already registered+authenticated — certified below by the shared
        # doctor-evidence classification loop; no browser flow needed.
    else
      AUTH_GAPS+=("$s")
    fi
  done

  if [ "${#AUTH_GAPS[@]}" -eq 0 ]; then
    pass "no OAuth gaps: notion/atlassian/granola are already registered+authenticated (pix doctor --json evidence) — no browser flow invoked"
  else
    printf '\n  A browser will open per server needing authorization to complete its OAuth flow.\n'
    # Authorize each GAP server INDIVIDUALLY, recording its own exact exit code
    # and output via assert_exit. `pix mcp auth --all` would sweep in every
    # OTHER server registered on this host (an unrelated 8th server from a
    # private pack, or leftover state from a prior session) — one broken,
    # unrelated server would then fail this release check for a server this
    # release never shipped and never asked to authorize. Equally, a server
    # doctor already certified above is never re-authorized just because it
    # shares this loop with servers that do need it.
    for s in "${AUTH_GAPS[@]}"; do
      assert_exit 0 "pix mcp auth $s completed" pix mcp auth "$s"
    done
  fi

  # Completion is CERTIFIED by a machine-readable probe, never by an operator's
  # say-so: pix doctor --json runs health/mcp.go's own registered+authenticated
  # check for every configured remote, in the exact same words doctor always
  # uses. A nervous or absent operator can never manufacture a PASS this way,
  # and a truthful gap can never be waved through by a hopeful click either.
  DOCTOR_JSON="$(pix doctor --json 2>/dev/null)"
  # Only an explicit ready/authenticated line is a PASS: health/mcp.go's
  # attachmentCaveat ("registered (host registration; attachment to a live
  # session is not checkable from here)") is the ONE phrase it emits once a
  # server verified registered AND (being remote) authenticated. Every other
  # shape is a real gap or an honest unknown, never a silent pass — in
  # particular the bare substring "$s:" used to match "not registered",
  # "registration unknown", and "auth not checkable from here" too, which is
  # exactly the false-PASS this rewrite closes.
  for s in notion atlassian granola; do
    if printf '%s' "$DOCTOR_JSON" | grep -qF "$s: registered (host registration"; then
      pass "catalog server $s authenticated (pix doctor --json evidence)"
    elif printf '%s' "$DOCTOR_JSON" | grep -qF "$s: not registered"; then
      fail "catalog server $s authenticated" "pix doctor --json reports it NOT REGISTERED after 'pix mcp bundle'"
    elif printf '%s' "$DOCTOR_JSON" | grep -qF "$s: registered, not authenticated"; then
      fail "catalog server $s authenticated" "pix doctor --json still reports it unauthenticated after 'pix mcp auth $s'"
    else
      skip "catalog server $s auth" "pix doctor --json carries no explicit ready/authenticated evidence for $s (unknown or unclassified registration/auth state)"
    fi
  done

  # Optional human confirmation. It is informational when absent or declined:
  # the machine probe above already rendered the required verdict, so an
  # optional observation must not turn an otherwise complete run INCOMPLETE.
  # Read from /dev/tty specifically and bound the wait so it cannot hang.
  # A `[ -r /dev/tty ] && [ -w /dev/tty ]` stat check is not enough: the device
  # node's permission bits can pass while the OPEN itself still fails (ENXIO,
  # "no controlling terminal") — exactly the case for a backgrounded or fully
  # non-interactive invocation. Attempt the real open (fd 3, read-write) with
  # its own stderr suppressed so a failed open prints no device-noise error to
  # the release log; the fd is the actual test, and either branch below is
  # already purely informational.
  if exec 3<>/dev/tty 2>/dev/null; then
    printf '  Optional: did every OAuth browser flow look right to you? [y/N] ' >&3 2>/dev/null
    if read -r -t 30 ans <&3 2>/dev/null; then
      case "$ans" in
        y|Y|yes) pass "operator confirmed the OAuth flows looked right" ;;
        *) printf '  note: optional operator confirmation was not affirmative; machine auth evidence above remains authoritative\n' ;;
      esac
    else
      printf '  note: no optional operator answer within 30s; machine auth evidence above remains authoritative\n'
    fi
    exec 3<&- 2>/dev/null; exec 3>&- 2>/dev/null
  else
    printf '  note: no controlling TTY for optional confirmation; machine auth evidence above remains authoritative\n'
  fi

  # Registration is host state; a session sees tools only if attached. The
  # disclaimer must say so HONESTLY (a positive claim) — and mcp ls must never
  # assert present-tense session attachment (a precise negative claim). A bare
  # `grep attached` check is wrong on its own terms here: the honest
  # disclaimer's own prose says "not what's attached to..." and "attaches a
  # registered server to a running sandbox now", both containing the substring
  # "attached"/"attaches" while asserting exactly the fact this check wants.
  assert_contains "not what's attached to" "mcp ls prints the honest host-registration disclaimer" pix mcp ls
  MCP_LS_OUT="$(pix mcp ls 2>&1)"
  if printf '%s' "$MCP_LS_OUT" | grep -qE "[Ii]s (now |currently )?attached to (your|this|the running) sandbox|[Ss]ession (is )?attached"; then
    fail "mcp ls reports registration, not session attachment" "output made a present-tense attachment claim: $(printf '%s' "$MCP_LS_OUT" | grep -E '[Ii]s (now |currently )?attached|[Ss]ession (is )?attached' | head -1)"
  else
    pass "mcp ls reports registration, not session attachment"
  fi
fi

head1 "Done — see the verdict below"
