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
#   * It restores host state it changed (a serve it installed is uninstalled; a
#     serve it stopped is restarted) in a trap that runs on every exit path.
#
# Exit codes: 0 all checks passed, 1 a check failed, 2 the run was incomplete
# (missing prerequisite, refused to start, cleanup could not finish).

set -uo pipefail

START_PWD="$(pwd -P)"
RUN_ID="uat-$$"
BOX_PREFIX="pix-${RUN_ID}"
WITH_OAUTH=0
WITH_SERVICES=1
PASS=0; FAIL=0; SKIP=0
CREATED_BOXES=()
EXTRA_BOXES=()   # digest-named boxes this run created (names discovered, not chosen)
INSTALLED_SERVE=0
STOPPED_SERVE=0
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

# assert_not_contains "needle" "name" cmd...
assert_not_contains() {
  local needle="$1" name="$2"; shift 2
  local out; out="$("$@" 2>&1)"
  if printf '%s' "$out" | grep -qF -- "$needle"; then
    fail "$name" "output unexpectedly contained '$needle'"
  else pass "$name"; fi
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
  if [ "$STOPPED_SERVE" = 1 ]; then
    (pix serve >/dev/null 2>&1 &) ; printf '  restarted the serve this script stopped\n'
  fi
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
  if [ "$FAIL" -gt 0 ]; then printf '  %s\n' "$(red 'UAT FAILED')"; exit 1; fi
  if [ "$PASS" -eq 0 ]; then printf '  %s\n' "$(red 'UAT INCOMPLETE: nothing was actually asserted')"; exit 2; fi
  if [ "$SKIP" -gt 0 ]; then
    printf '  %s — %d check(s) could not run; this is NOT a clean release verdict\n' "$(ylw 'UAT INCOMPLETE')" "$SKIP"; exit 2
  fi
  [ "$rc" -ne 0 ] && { printf '  %s (script exited %d)\n' "$(red 'UAT ABORTED')" "$rc"; exit 2; }
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

# --- 2. digest naming: same basename, different workspaces ---------------------
head1 "[2] Digest-suffixed sandbox naming (two workspaces, one basename)"
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

# --- 3. launch, instance identity, and the attach fingerprint ------------------
head1 "[3] Launch, instance record, attach fingerprint"
BOX1="${BOX_PREFIX}-one"
if runbox "$BOX1" "$WORK/a/proj" -- -p 'print the single word ready' >"$WORK/run1.log" 2>&1; then
  pass "pix run launched $BOX1 non-interactively"
else
  fail "pix run launched $BOX1" "$(tail -3 "$WORK/run1.log" | tr '\n' ' ')"
fi
assert_contains "$BOX1" "pix ls lists $BOX1" pix ls

STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/pix"
REC="$(find "$STATE_DIR" -name record.json -newer "$WORK" 2>/dev/null | head -1)"
if [ -n "$REC" ]; then
  INSTANCE="$(sed -n 's/.*"instance_id"[: ]*"\([^"]*\)".*/\1/p' "$REC")"
  if [ -n "$INSTANCE" ]; then pass "lease record carries an instance id ($INSTANCE)"
  else fail "lease record instance id" "no instance_id in $REC"; fi
else
  skip "lease instance record" "no record.json written under $STATE_DIR"
fi

# An attach whose create-time MCP set no longer matches must be REFUSED, not
# silently attached: that is the fingerprint gate.
if OUT="$( (cd "$WORK/a/proj" && pix run . --name "$BOX1" --mcp definitely-not-registered -- -p hi) 2>&1 )"; then
  fail "attach fingerprint gate" "an attach with a changed MCP set was accepted: $(printf '%s' "$OUT" | tail -2 | tr '\n' ' ')"
else
  pass "attach fingerprint gate refused a changed MCP set"
fi

# --- 4. exit-code propagation (the last shell's status is pix's status) --------
head1 "[4] Exit status propagation"
(cd "$WORK/a/proj" && pix run . --name "$BOX1" -- --definitely-not-a-pi-flag) >/dev/null 2>&1
RC=$?
if [ "$RC" -eq 0 ]; then fail "last-exit propagation" "a failing inner command produced exit 0"
else pass "last-exit propagation (inner failure surfaced as exit $RC)"; fi
assert_exit 2 "bare 'pix <not-a-dir>' refuses" pix definitely-not-a-directory-"$RUN_ID"

# --- 5. multi-shell references and teardown ------------------------------------
head1 "[5] Multi-shell references: the LAST shell tears down"
BOX2="${BOX_PREFIX}-multi"
newbox "$BOX2"
(cd "$WORK/b/proj" && pix run . --name "$BOX2" -- -p 'sleep quietly' >/dev/null 2>&1) &
SH1=$!
sleep 5
(cd "$WORK/b/proj" && pix run . --name "$BOX2" -- -p 'second shell' >/dev/null 2>&1) &
SH2=$!
sleep 5
if pix ls 2>/dev/null | grep -q "$BOX2"; then pass "$BOX2 is up with two shells attached"
else fail "$BOX2 with two shells" "sandbox not listed while two shells hold it"; fi
wait $SH1 2>/dev/null
sleep 3
if pix ls 2>/dev/null | grep -q "$BOX2"; then pass "sandbox survives the FIRST shell leaving"
else fail "sandbox survives the first shell leaving" "torn down while a second shell still held it"; fi
wait $SH2 2>/dev/null
sleep 8
if pix ls 2>/dev/null | grep -q "$BOX2"; then
  fail "teardown on last shell exit" "$BOX2 outlived every shell without --keep"
else pass "teardown on last shell exit"; fi

# --- 6. --keep, and the orphan reaper that must respect it ---------------------
head1 "[6] --keep marker and orphan reaping"
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

# --- 7. host services: launchd + the memory unit -------------------------------
if [ "$WITH_SERVICES" = 0 ]; then
  skip "host services" "--no-services was passed"
else
head1 "[7] Host services: serve status, launchd restart, memory unit restart"
assert_exit 0 "pix serve status" pix serve status
assert_contains "schema_version" "pix doctor --json is machine-readable" pix doctor --json

if ! pix serve status 2>/dev/null | grep -q "running"; then
  if pix serve install >/dev/null 2>&1; then INSTALLED_SERVE=1; sleep 5; else die "cannot start serve; the service checks cannot run"; fi
fi

UNITS="$(pix serve status --json 2>/dev/null)"
if printf '%s' "$UNITS" | grep -q '"units"'; then
  pass "serve status --json publishes the supervision tree"
  for field in '"identity"' '"state"' '"restarts"' '"generation"' '"reattached"' '"last_probe_us"'; do
    if printf '%s' "$UNITS" | grep -q "$field"; then pass "unit report carries $field"
    else fail "unit report carries $field" "missing from serve status --json"; fi
  done
  # The published snapshot must not carry a credential: assert on the shapes.
  if printf '%s' "$UNITS" | grep -qiE '(sk-[a-z0-9-]{16,}|xox[baprs]-|Bearer [A-Za-z0-9._-]{8,})'; then
    fail "snapshot carries no secrets" "credential-shaped text in serve status --json"
  else pass "snapshot carries no secrets"; fi
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
      if pix serve status 2>/dev/null | grep -q "not running"; then pass "'serve stop' is mode-aware (KeepAlive did not respawn it)"
      else fail "'serve stop' is mode-aware" "serve came back after stop — it was stopped by pid, not through launchd"; fi
      STOPPED_SERVE=0
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

# --- 8. external OAuth integrations (interactive, opt-in) ----------------------
head1 "[8] External OAuth-backed MCP servers"
if [ "$WITH_OAUTH" = 0 ]; then
  skip "remote OAuth servers" "--with-oauth not passed (this run is NOT a full release verdict)"
else
  assert_exit 0 "pix mcp bundle registers the public catalog" pix mcp bundle
  assert_exit 0 "pix mcp ls" pix mcp ls
  for s in notion atlassian granola; do
    assert_contains "$s" "catalog server $s is registered" pix mcp ls
  done
  printf '\n  A browser will open per server. Complete each OAuth flow, then return here.\n'
  pix mcp auth --all
  printf '  Did every OAuth flow complete without an error? [y/N] '
  read -r ans
  case "$ans" in
    y|Y|yes) pass "operator confirmed every remote OAuth flow completed" ;;
    *) fail "remote OAuth flows" "operator did not confirm completion" ;;
  esac
  # Registration is host state; a session sees tools only if attached. Prove the
  # distinction is still reported honestly rather than as "attached".
  assert_not_contains "attached" "mcp ls reports registration, not session attachment" pix mcp ls
fi

head1 "Done — see the verdict below"
