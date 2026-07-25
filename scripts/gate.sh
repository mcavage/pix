#!/usr/bin/env bash
# The FAST gate: everything a push/PR must pass before a human looks at it, run
# in one shot with per-segment timings and an ABSOLUTE wall-clock budget.
#
#   build -> go vet -> go test (NON-race) -> node --test -> tsc --noEmit
#         -> open-core boundary -> recall transport -> rename guard (once it exists)
#
# WHICH BUDGET APPLIES TO WHAT (this is the whole point of the split):
#   * THIS script is the timed one. It runs the NON-race Go suite and is
#     budgeted at GATE_BUDGET_MS (default 12000 ms) absolute.
#   * `go test -race ./...` is NOT run here. The race detector costs a
#     multiple of wall time by design, so it lives in its own CI job with NO
#     timing gate at all (.github/workflows/test.yml, job `race`). Timing a
#     race run would either make the budget meaningless or make the gate flaky.
#
# WHY 12 s, AND WHY IT IS A FIXED NUMBER:
#   Measured steady-state on a warm checkout after the W0a/W0b test-latency
#   work (recorded in uat/w0-test-timing.log and uat/w0-gate-timing.log):
#     go build 0.6s + go vet 0.2s + go test 7.4s + node 0.4s + tsc 0.5s
#     + open-core 0.2s  ~= 9.3 s wall.
#   12 s = that measurement plus ~3 s of headroom for a slower runner. It is a
#   CEILING DERIVED FROM A RECORDED BASELINE, never "previous run x 2": a
#   relative budget ratchets upward one slow PR at a time and can only ever
#   catch a single-commit cliff, never the slow slide that actually kills a
#   suite. GATE_TARGET_MS (10 s) is the soft line: between target and budget
#   the gate warns and still passes.
#
#   Raising the budget is a deliberate, reviewable edit to this line -- not
#   something a slow PR can do to itself.
#
# SLOW ITEMS ARE COMMENTS, NOT BLOCKS:
#   Any single Go test or node test file over GATE_SLOW_MS (1000 ms) is
#   reported (and annotated in CI) as a reviewable finding. It never fails the
#   gate on its own; only the total budget does.
#
# Env knobs:
#   GATE_BUDGET_MS  absolute hard-fail ceiling in ms (default 12000; 0 = off)
#   GATE_TARGET_MS  soft warn line in ms (default 10000)
#   GATE_SLOW_MS    per-test "reviewable finding" line (default 1000)
#   GATE_OUT_DIR    where the timing artifact is written (default out/gate)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GATE_BUDGET_MS="${GATE_BUDGET_MS:-12000}"
GATE_TARGET_MS="${GATE_TARGET_MS:-10000}"
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
# nothing. -v is what makes PER-TEST timing available to parse below.
go_test() { (cd services/host && go test -count=1 -v ./...); }

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

run_segment "go-build" "go-build" go_build
run_segment "go-vet" "go-vet" go_vet
run_segment "go-test" "go-test" go_test
run_segment "node-test" "node-test" node_test
run_segment "typecheck" "typecheck" typecheck
run_segment "open-core" "open-core" open_core
run_segment "recall-xport" "recall-xport" recall_transport

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
elif [ "$TOTAL_MS" -gt "$GATE_TARGET_MS" ]; then
	BUDGET_NOTE="over target, under budget"
	echo "budget: ok — over the $(fmt_ms "$GATE_TARGET_MS") target but under the $(fmt_ms "$GATE_BUDGET_MS") ceiling"
	annotate warning "fast gate took $(fmt_ms "$TOTAL_MS"), over the $(fmt_ms "$GATE_TARGET_MS") target"
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
