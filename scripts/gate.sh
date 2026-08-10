#!/usr/bin/env bash
# The FAST gate: everything a push/PR must pass before a human looks at it, run
# in one shot with per-segment timings and an ABSOLUTE wall-clock budget.
#
#   build -> go vet -> go test (NON-race) -> node --test -> tsc --noEmit
#         -> open-core boundary -> recall transport -> arch-metrics budget
#         -> rename guard (once it exists)
#
# WHICH BUDGET APPLIES TO WHAT (this is the whole point of the split):
#   * THIS script is the timed one. It runs the NON-race Go suite and reports
#     wall time against GATE_BUDGET_MS. That ceiling is ADVISORY BY DEFAULT
#     (0 = off) and is enforced only where it is set explicitly, which today
#     means CI (75000 ms, see .github/workflows/test.yml). A local `make gate`
#     still prints the total and the slow-test list; it no longer fails a
#     correct suite for being slow. See "WHY THE LOCAL CEILING IS OFF" below.
#   * `go test -race ./...` is NOT run here. The race detector costs a
#     multiple of wall time by design, so it lives in its own CI job with NO
#     timing gate at all (.github/workflows/test.yml, job `race`). Timing a
#     race run would either make the budget meaningless or make the gate flaky.
#
# WHY THE LOCAL CEILING IS OFF (2026-08-08):
#   The old default was 34000 ms, derived from a recorded baseline of three
#   consecutive warm runs (2026-08-02): 30.2 / 29.9 / 29.2 s wall, broken down
#   as go-build 0.8s + go-vet 0.4s + go-test 24.2s + node-test 0.9s +
#   typecheck 0.7s + open-core 1.8s + recall-xport 0.1s + rename-guard 0.3s.
#
#   That baseline stopped describing the suite. Re-measured on the same
#   machine after the lifecycle rearchitecture landed, three consecutive warm
#   runs: 53.8 / 54.6 / 54.9 s wall, broken down as go-test 32.8s +
#   node-test 16.1s + open-core 2.2s + typecheck 0.7s + go-build 0.7s +
#   rename-guard 0.4s + arch-metrics 0.4s + go-vet 0.3s + recall-xport 0.1s.
#   The growth is real work, not a regression to hunt: node-test went 0.9s ->
#   16.1s when the legal, semantic-diff, and lifecycle-UAT suites landed, and
#   go-test 24.2s -> 32.8s alongside them.
#
#   So the ceiling was failing correct suites on wall time alone. Rather than
#   re-derive a number that the next honest batch of tests invalidates again,
#   the local default is now 0 (OFF): the gate still measures, still prints
#   the total, and still reports every test over GATE_SLOW_MS as a reviewable
#   finding, but a slow-yet-passing suite no longer fails a developer's build.
#   ENFORCEMENT MOVED TO ONE PLACE: CI sets GATE_BUDGET_MS explicitly (75000
#   ms, .github/workflows/test.yml), so a genuine cliff is still caught before
#   merge, by the runner whose timing is actually comparable run to run.
#
#   To enforce locally again, set it per-invocation or in your environment:
#     GATE_BUDGET_MS=60000 make gate
#
#   If the total needs to come DOWN rather than the ceiling up, the lever is
#   the node-test segment: scripts/gate.sh globs `tests/*.test.mjs` only, so a
#   slow behavioral suite can move to `tests/slow/` -- but note that directory
#   has no generic runner today (its one file is invoked by name in
#   legal.yml), so moving a test there without also wiring it into a CI job
#   silently stops running it.
#
#   HISTORY, because the number moved and the reason matters:
#   The previous ceiling was 12 s, from a baseline of `go test` at 7.4 s. That
#   baseline stopped describing the suite: it is now 24 s. The gate was red for
#   the whole of the cmd/pix drain, and two REAL causes were fixed rather than
#   papered over --
#     * TestCheckHostPiVersion_TimesOut slept a literal 2 s (6% of the old
#       ceiling in one test) because the probe timeout was a constant; it is
#       injectable now and the test asserts the bound in 50 ms.
#     * cmd/pix held 1,042 tests in ONE package, which Go runs sequentially.
#       Draining it into 25 packages let them run concurrently: 31 s -> 24 s.
#   What is left is not fat. cmd/pix still runs 639 tests summing to 15 s with
#   no hotspot -- ~24 ms each, sequential, because 29 of its 91 test files call
#   t.Setenv and therefore cannot call t.Parallel. THAT is the remaining lever:
#   adding t.Parallel() to the 62 files that can take it. It is a real change
#   with real risk (tests that mutate package vars would race), so it is not
#   bundled into a refactor commit. Until someone does it, 34 s is the honest
#   number, and it is honest BECAUSE it was measured rather than guessed.
#
#   The ceiling drifted back up to ~32.5 s wall (over the 30 s target, still
#   under 34 s) as more individually-slow tests accumulated: a 12.4 s cold
#   `go build` fixture rebuild, several Suture backoff/wedged-timeout cases,
#   a memory-unit-restart integration test, and half a dozen tests that each
#   `go build` the real pix / pix-host binary for one CLI-exit-code or
#   help-text roundtrip. None of those are unit or safety-invariant LOGIC
#   tests -- they are integration/process tests whose cost is the build or the
#   deliberate wait, not the assertion. `go test -short ./...` (added here)
#   plus a per-site `if testing.Short() { t.Skip(...) }` on exactly those
#   tests restores the budget to a measured 11-13 s wall without touching any
#   unit or safety-invariant test and without raising the 34 s ceiling -- the
#   ceiling stays put because the fix was making the gate accurately fast
#   again, not moving the goalposts. Every skipped test still runs, unskipped,
#   in the untimed `race` and `metrics` CI jobs (.github/workflows/test.yml),
#   which call `go test -race ./...` / `go test -cover ./...` with no -short
#   flag; tests/ci-gate.test.mjs asserts both of those stay -short-free so a
#   skip here can never quietly become a skip everywhere.
#
#   Sole exception to "skip a whole test": TestLaunchGateRefusesOnlyAPositive
#   NoKey's "store hangs" case execs a real `sleep 10` to prove the model-key
#   probe's own deadline is enforced (a genuine timed hang probe). Skipping the
#   WHOLE test under -short would have also dropped the actual safety-invariant
#   assertion (AGENTS.md invariant #6: refuse launch only on a POSITIVE
#   no-key answer) from the fast gate, so only that one table row skips; every
#   other case in the same t.Run table -- including the no-key refusal --
#   stays unconditional.
#
# SLOW ITEMS ARE COMMENTS, NOT BLOCKS:
#   Any single Go test or node test file over GATE_SLOW_MS (1000 ms) is
#   reported (and annotated in CI) as a reviewable finding. It never fails the
#   gate on its own; only the total budget does.
#
# Env knobs:
#   GATE_BUDGET_MS  absolute hard-fail ceiling in ms (default 0 = off locally;
#                   CI sets 75000 explicitly)
#   GATE_TARGET_MS  soft warn line in ms (default 120000). Raised from 30000:
#                   the suite runs ~55 s, so the old target fired on EVERY run.
#                   A warning that is always on is not a warning, it is noise
#                   that teaches you to skim past the gate's output -- which is
#                   the one place a real finding would appear. 2 min leaves real
#                   headroom above today's number, so when it does fire it means
#                   something.
#   GATE_SLOW_MS    per-test "reviewable finding" line (default 1000)
#   GATE_OUT_DIR    where the timing artifact is written (default out/gate)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GATE_BUDGET_MS="${GATE_BUDGET_MS:-0}"
GATE_TARGET_MS="${GATE_TARGET_MS:-120000}"
GATE_SLOW_MS="${GATE_SLOW_MS:-1000}"
GATE_OUT_DIR="${GATE_OUT_DIR:-$ROOT/out/gate}"

mkdir -p "$GATE_OUT_DIR"
LOG_DIR="$GATE_OUT_DIR/logs"
rm -rf "$LOG_DIR"
mkdir -p "$LOG_DIR"

# --- millisecond clock -------------------------------------------------------
# bash 5 exposes EPOCHREALTIME with no subprocess at all (the common path: CI
# runners and the sandbox). Fall back to GNU date, then python3, then whole
# seconds on an ancient bash without any of them.
_CLOCK=""
_pick_clock() {
	if [ -n "${EPOCHREALTIME:-}" ]; then _CLOCK=epochrealtime; return; fi
	local probe
	probe="$(date +%s%N 2>/dev/null)"
	case "$probe" in
	*[!0-9]* | "") ;;
	*) if [ ${#probe} -gt 12 ]; then _CLOCK=gnudate; return; fi ;;
	esac
	if command -v python3 >/dev/null 2>&1; then _CLOCK=python; return; fi
	_CLOCK=seconds
}
_pick_clock

now_ms() {
	case "$_CLOCK" in
	epochrealtime)
		local us=${EPOCHREALTIME/[.,]/} # locale may use a comma
		echo $((10#$us / 1000))
		;;
	gnudate) echo $(($(date +%s%N) / 1000000)) ;;
	python) python3 -c 'import time;print(int(time.time()*1000))' ;;
	*) echo $(($(date +%s) * 1000)) ;;
	esac
}

fmt_ms() { printf '%d.%03ds' $(($1 / 1000)) $(($1 % 1000)); }

json_escape() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# CI annotations: a slow test is a comment a reviewer reads, not a red build.
annotate() { # annotate <error|warning|notice> <message>
	[ -n "${GITHUB_ACTIONS:-}" ] && printf '::%s::%s\n' "$1" "$2"
	return 0
}

# --- segment bookkeeping -----------------------------------------------------
SEG_NAMES=()
SEG_MS=()
SEG_STATUS=()
SLOW_KIND=()
SLOW_NAME=()
SLOW_MS=()
FAILED=0

record_slow() { # record_slow <kind> <ms> <name>
	SLOW_KIND+=("$1")
	SLOW_MS+=("$2")
	SLOW_NAME+=("$3")
	annotate warning "slow $1 ($(fmt_ms "$2")): $3"
}

# run_segment <name> <log-basename> <command...>
# Runs the command with output captured; prints PASS/FAIL + elapsed. Segments
# always ALL run: a gate that stops at the first failure hides the other four.
run_segment() {
	local name="$1" logname="$2"
	shift 2
	local log="$LOG_DIR/$logname.log" start end ms rc
	start="$(now_ms)"
	"$@" >"$log" 2>&1
	rc=$?
	end="$(now_ms)"
	ms=$((end - start))
	SEG_NAMES+=("$name")
	SEG_MS+=("$ms")
	if [ "$rc" -eq 0 ]; then
		SEG_STATUS+=("ok")
		printf '  %-12s %8s  ok\n' "$name" "$(fmt_ms "$ms")"
	else
		SEG_STATUS+=("fail")
		FAILED=1
		printf '  %-12s %8s  FAIL (exit %d)\n' "$name" "$(fmt_ms "$ms")" "$rc"
	fi
	return $rc
}

skip_segment() { # skip_segment <name> <reason>
	SEG_NAMES+=("$1")
	SEG_MS+=(0)
	SEG_STATUS+=("skipped")
	printf '  %-12s %8s  skipped (%s)\n' "$1" "-" "$2"
}

# --- preflight ---------------------------------------------------------------
need() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "gate: missing required tool '$1' ($2)" >&2
		exit 2
	}
}
need go "install Go, or see the go-version pin in .github/workflows/test.yml"
need node "install node 25.x"
[ -d node_modules ] || {
	echo "gate: node_modules/ is absent — run 'npm ci --ignore-scripts' first" >&2
	exit 2
}

echo "fast gate (non-race) — budget $(fmt_ms "$GATE_BUDGET_MS") absolute, target $(fmt_ms "$GATE_TARGET_MS")"
echo ""

GATE_START="$(now_ms)"

# --- segments ----------------------------------------------------------------
go_build() { (cd services/host && go build ./...); }
go_vet() { (cd services/host && go vet ./...); }
# -count=1 defeats the test cache: a gate that reports a cached PASS measures
# nothing. -v is what makes PER-TEST timing available to parse below. -short
# skips the individually-slow (>1s) integration/process tests that are
# genuinely slow BY DESIGN (fixture builds, Suture backoff/wedged, memory
# restart, timed hang probes, host binary CLI roundtrips): every one of them
# is still run in full by the untimed `race` and `metrics` CI jobs
# (.github/workflows/test.yml), which invoke `go test ./...` / `go test
# -race ./...` with NO -short. All unit and safety-invariant logic tests stay
# unconditional here; see the per-test `if testing.Short() { t.Skip(...) }`
# comments for the reasoning at each site.
go_test() { (cd services/host && go test -count=1 -v -short ./...); }

NODE_JUNIT="$LOG_DIR/node-test.junit.xml"
node_test() {
	# Node 25's JUnit reporter can wait forever for its destination stream to
	# finish under redirected CI output. The default spec reporter exits cleanly;
	# the segment wall time remains the enforced budget metric.
	rm -f "$NODE_JUNIT"
	# Bound worker fan-out: Node 25 can strand child test processes when this
	# suite uses the machine-wide default under redirected output.
	# Some extension fixtures intentionally create timers/listeners. Node 25 can
	# leave their worker handles alive for minutes on GitHub runners even after
	# every test completed; force-exit terminates only after the runner's result.
	node --test --test-force-exit --test-concurrency=4 tests/*.test.mjs
}
typecheck() { npx --no-install tsc --noEmit; }
open_core() { bash scripts/check-open-core.sh; }
# FF4a: recall stays on the append-only message channel. Invisible when broken
# (it works, costs money, and no other test looks for it), so it is a build
# guard rather than a comment.
recall_transport() { bash scripts/check-recall-transport.sh; }

# AC-GATE-03/04: the shrink-only per-package budget ratchet (LOC, exports,
# globals, edges, os.Exit calls — see scripts/arch-metrics). This is the CHEAP
# half: go/parser only, no `go test`, no coverage. The full corpus + coverage +
# LOC report is deliberately NOT here — it lives in the untimed `metrics` CI
# job (.github/workflows/test.yml) so it can never blow this budget.
ARCH_METRICS_BIN="$GATE_OUT_DIR/arch-metrics"
arch_metrics() {
	(cd scripts/arch-metrics && go build -o "$ARCH_METRICS_BIN" . && go test ./...) &&
		"$ARCH_METRICS_BIN" -root services/host -budgets scripts/arch-metrics/budgets.json
}

# AC-GATE-05: no unreachable production code. The audit that deleted 18 dead
# functions found two whose deaths were mechanical -- closeFrontDoorListeners
# lost both call sites when the monitor block went, ExecSbxMcpLoad was the
# implementation of a verb that had been removed -- so nothing but a tool was
# ever going to notice them. `deadcode` costs ~2s and the baseline is ZERO, which
# is the only baseline worth gating: any number above zero has to be curated, and
# a curated list of "known dead code" is just the vestigial surface with extra
# steps.
#
# PINNED and run through `go run`, deliberately NOT a `tool` directive in
# services/host/go.mod: deadcode pulls golang.org/x/telemetry and bumps three
# more modules, and putting a linter's dependencies into the SHIPPED module graph
# would drag them through the notices and SBOM gates for no benefit. The module
# cache makes the second run free.
#
# It SKIPS (never fails) when the tool cannot be fetched, so an offline laptop
# gets a gate that still runs everything else. CI always has the network, so the
# guard is real where it has to be.
DEADCODE_PKG="golang.org/x/tools/cmd/deadcode@v0.48.0"
deadcode_guard() {
	local out rc
	out="$(cd services/host && go run "$DEADCODE_PKG" -test ./... 2>&1)"
	rc=$?
	# A non-zero exit is NOT automatically "skip". Only an inability to OBTAIN the
	# tool is (offline laptop, proxy down); anything else -- a tool crash, a
	# package that does not typecheck -- is a real failure this must report, or
	# the guard would quietly disarm itself exactly when something is wrong.
	if [ $rc -ne 0 ]; then
		case "$out" in
		*"module lookup disabled"* | *"dial tcp"* | *"no such host"* | *"i/o timeout"* | *"connection refused"* | *"proxyconnect"* | *"certificate"*)
			printf 'deadcode: SKIPPED, could not fetch %s (offline?):\n%s\n' "$DEADCODE_PKG" "$out"
			return 0
			;;
		esac
		printf 'deadcode: FAILED to run (%s):\n%s\n' "$DEADCODE_PKG" "$out"
		return 1
	fi
	if [ -n "$out" ]; then
		printf 'deadcode: unreachable code (delete it, or wire it up):\n%s\n' "$out"
		return 1
	fi
	printf 'deadcode: no unreachable funcs\n'
}

run_segment "go-build" "go-build" go_build
run_segment "go-vet" "go-vet" go_vet
run_segment "go-test" "go-test" go_test
run_segment "node-test" "node-test" node_test
run_segment "typecheck" "typecheck" typecheck
run_segment "open-core" "open-core" open_core
run_segment "recall-xport" "recall-xport" recall_transport
run_segment "arch-metrics" "arch-metrics" arch_metrics
run_segment "deadcode" "deadcode" deadcode_guard

# The rename guard lands with the W3 cutover (U-W3.04). Wiring it in
# CONDITIONALLY means this gate ships now and picks the guard up the moment the
# script exists — no second edit, and no red gate in between.
if [ -f scripts/check-rename.sh ]; then
	run_segment "rename-guard" "rename-guard" bash scripts/check-rename.sh
else
	skip_segment "rename-guard" "scripts/check-rename.sh not present yet"
fi

GATE_END="$(now_ms)"
TOTAL_MS=$((GATE_END - GATE_START))

# --- per-package / per-test timing ------------------------------------------
echo ""
echo "go packages:"
# -a: a test that prints a stray NUL (some fixtures do) makes grep call the
# whole log binary and swallow every match.
grep -aE '^(ok|FAIL|---|\?)' "$LOG_DIR/go-test.log" 2>/dev/null |
	grep -avE '^--- (PASS|FAIL|SKIP)' | sed 's/^/  /' || true

while IFS=$'\t' read -r ms name; do
	[ -n "${name:-}" ] && record_slow "go-test" "$ms" "$name"
done < <(awk -v slow="$GATE_SLOW_MS" '
	match($0, /--- (PASS|FAIL|SKIP): /) {
		rest = substr($0, RSTART + RLENGTH)
		if (match(rest, / \([0-9]+\.[0-9]+s\)$/)) {
			name = substr(rest, 1, RSTART - 1)
			ms = substr(rest, RSTART + 2, RLENGTH - 4) * 1000
			if (ms >= slow) printf "%d\t%s\n", ms, name
		}
	}' "$LOG_DIR/go-test.log" 2>/dev/null | sort -rn)

# Parse slow individual tests from the default spec reporter without enabling
# Node's hanging JUnit destination path. File-level wall time is represented by
# the node-test segment above; this list is diagnostic only.
while IFS=$'\t' read -r ms name; do
	[ -n "${name:-}" ] && record_slow "node-test" "$ms" "$name"
done < <(awk -v slow="$GATE_SLOW_MS" '
	match($0, / \(([0-9.]+)(ms|s)\)$/) {
		tail = substr($0, RSTART + 2, RLENGTH - 3)
		unit = tail ~ /ms$/ ? "ms" : "s"
		sub(/(ms|s)$/, "", tail)
		ms = tail * (unit == "s" ? 1000 : 1)
		name = substr($0, 1, RSTART - 1)
		sub(/^[^[:alnum:]]+/, "", name)
		if (ms >= slow) printf "%d\t%s\n", ms, name
	}' "$LOG_DIR/node-test.log" 2>/dev/null | sort -rn)

# --- failures ----------------------------------------------------------------
if [ "$FAILED" -ne 0 ]; then
	echo ""
	i=0
	while [ $i -lt ${#SEG_NAMES[@]} ]; do
		if [ "${SEG_STATUS[$i]}" = "fail" ]; then
			echo "=== ${SEG_NAMES[$i]} output ======================================"
			cat "$LOG_DIR/${SEG_NAMES[$i]}.log"
			echo ""
		fi
		i=$((i + 1))
	done
fi

# --- verdict -----------------------------------------------------------------
echo ""
echo "total: $(fmt_ms "$TOTAL_MS")"

VERDICT="pass"
BUDGET_NOTE=""
if [ "$FAILED" -ne 0 ]; then
	VERDICT="fail"
	# Timing of a failing run is noise (a panicking package exits early, a
	# hanging one blows past everything). Report it, do not judge it.
	BUDGET_NOTE="not evaluated (a segment failed)"
	echo "budget: $BUDGET_NOTE"
elif [ "$GATE_BUDGET_MS" -gt 0 ] && [ "$TOTAL_MS" -gt "$GATE_BUDGET_MS" ]; then
	VERDICT="over-budget"
	BUDGET_NOTE="OVER by $(fmt_ms $((TOTAL_MS - GATE_BUDGET_MS)))"
	echo "budget: FAIL — $(fmt_ms "$TOTAL_MS") > $(fmt_ms "$GATE_BUDGET_MS") absolute ceiling"
	annotate error "fast gate took $(fmt_ms "$TOTAL_MS"), over the $(fmt_ms "$GATE_BUDGET_MS") absolute budget"
	FAILED=1
elif [ "$TOTAL_MS" -gt "$GATE_TARGET_MS" ] && [ "$GATE_BUDGET_MS" -gt 0 ]; then
	BUDGET_NOTE="over target, under budget"
	echo "budget: ok — over the $(fmt_ms "$GATE_TARGET_MS") target but under the $(fmt_ms "$GATE_BUDGET_MS") ceiling"
	annotate warning "fast gate took $(fmt_ms "$TOTAL_MS"), over the $(fmt_ms "$GATE_TARGET_MS") target"
elif [ "$TOTAL_MS" -gt "$GATE_TARGET_MS" ]; then
	# No ceiling set (the local default): report the number, judge nothing.
	BUDGET_NOTE="over target, no ceiling enforced"
	echo "budget: advisory — $(fmt_ms "$TOTAL_MS"), over the $(fmt_ms "$GATE_TARGET_MS") target; no ceiling enforced (set GATE_BUDGET_MS to enforce one)"
else
	BUDGET_NOTE="under target"
	echo "budget: ok — under the $(fmt_ms "$GATE_TARGET_MS") target"
fi

if [ ${#SLOW_NAME[@]} -gt 0 ]; then
	echo ""
	echo "reviewable findings (over $(fmt_ms "$GATE_SLOW_MS") — comment, not a block):"
	i=0
	while [ $i -lt ${#SLOW_NAME[@]} ]; do
		printf '  %-10s %8s  %s\n' "${SLOW_KIND[$i]}" "$(fmt_ms "${SLOW_MS[$i]}")" "${SLOW_NAME[$i]}"
		i=$((i + 1))
	done
fi

# --- artifact ----------------------------------------------------------------
{
	printf '{\n'
	printf '  "schema": 1,\n'
	printf '  "generated_at": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf '  "git_sha": "%s",\n' "$(git rev-parse HEAD 2>/dev/null || echo unknown)"
	printf '  "race": false,\n'
	printf '  "budget_ms": %s,\n' "$GATE_BUDGET_MS"
	printf '  "target_ms": %s,\n' "$GATE_TARGET_MS"
	printf '  "slow_threshold_ms": %s,\n' "$GATE_SLOW_MS"
	printf '  "total_ms": %s,\n' "$TOTAL_MS"
	printf '  "verdict": "%s",\n' "$VERDICT"
	printf '  "budget_note": "%s",\n' "$(json_escape "$BUDGET_NOTE")"
	printf '  "segments": [\n'
	i=0
	while [ $i -lt ${#SEG_NAMES[@]} ]; do
		[ $i -gt 0 ] && printf ',\n'
		printf '    {"name": "%s", "ms": %s, "status": "%s"}' \
			"${SEG_NAMES[$i]}" "${SEG_MS[$i]}" "${SEG_STATUS[$i]}"
		i=$((i + 1))
	done
	printf '\n  ],\n'
	printf '  "slow_items": [\n'
	i=0
	while [ $i -lt ${#SLOW_NAME[@]} ]; do
		[ $i -gt 0 ] && printf ',\n'
		printf '    {"kind": "%s", "ms": %s, "name": "%s"}' \
			"${SLOW_KIND[$i]}" "${SLOW_MS[$i]}" "$(json_escape "${SLOW_NAME[$i]}")"
		i=$((i + 1))
	done
	printf '\n  ]\n'
	printf '}\n'
} >"$GATE_OUT_DIR/timing.json"

echo ""
echo "timing artifact: ${GATE_OUT_DIR#$ROOT/}/timing.json (logs in ${LOG_DIR#$ROOT/}/)"

# GitHub job summary: the total, where a human already looks.
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### fast gate (non-race)"
		echo ""
		echo "**total $(fmt_ms "$TOTAL_MS")** / budget $(fmt_ms "$GATE_BUDGET_MS") — $VERDICT"
		echo ""
		echo "| segment | elapsed | status |"
		echo "| --- | ---: | --- |"
		i=0
		while [ $i -lt ${#SEG_NAMES[@]} ]; do
			echo "| ${SEG_NAMES[$i]} | $(fmt_ms "${SEG_MS[$i]}") | ${SEG_STATUS[$i]} |"
			i=$((i + 1))
		done
	} >>"$GITHUB_STEP_SUMMARY"
fi

exit "$FAILED"
