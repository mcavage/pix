package main

import (
	"fmt"
	"io"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/secret"

	"pix/host/config"
)

// readiness_launch.go is the ONE lazy snapshot the FAST surfaces share:
// `run` (the warnings printed before a launch), `status`/bare `pix` (the
// landing screen) and `onboard`'s closing report. Doctor and setup request
// every axis; these three request a deliberately small subset, because they
// are on the daily path and a slow daily command is a command people stop
// reading.
//
// Two properties are load-bearing here:
//
//  1. EVIDENCE IS PASSED IN, NEVER RE-PROBED. `run` already probes
//     `sbx secret ls` for its launch gate and `status` already probes it for
//     the per-provider booleans. Handing that result to the snapshot is what
//     keeps "render readiness too" from doubling the command's subprocess
//     count.
//  2. LAZINESS IS THE COST MODEL. Only the axes in fastReadinessAxes are
//     built; everything else is absent from the snapshot, which is different
//     from ready (snapshot.Checks reports ok=false, so no renderer can invent
//     a verdict for an axis nobody paid to probe).

// sbxKeyEvidence is one `sbx secret ls` result an invocation ALREADY paid for.
// state distinguishes "sbx is not installed here" from "sbx is installed and
// its control plane errored", which the providers axis renders differently
// (unverifiable either way, but only the second is a host problem).
type sbxKeyEvidence struct {
	out   string
	state secret.SbxSecretsProbeState
}

// ok reports whether the probe actually answered, i.e. whether out may be read
// as truth about which keys are set.
func (e sbxKeyEvidence) ok() bool { return e.state == secret.SbxSecretsOK }

// probeSbxKeyEvidence runs the ONE shared secrets probe, for a caller that has
// no evidence in hand yet. A caller that already probed passes its own result
// instead of calling this.
func probeSbxKeyEvidence(env hostenv.Env) sbxKeyEvidence {
	out, state := secret.ProbeSbxSecrets(env)
	return sbxKeyEvidence{out: out, state: state}
}

// providersReadinessAxes builds the providers axis from evidence alone: it
// runs NO probe of its own, so it is free for a caller that has already
// probed. The checks are doctor's own (modelKeyCoreCheck, runIntentKeyCheck),
// so run/status/onboard cannot disagree with doctor about the one core
// launch requirement.
func providersReadinessAxes(cfg *config.Config, ev sbxKeyEvidence) map[readiness.Axis]readiness.AxisBuilder {
	return map[readiness.Axis]readiness.AxisBuilder{
		readiness.AxisProviders: func() []readiness.Check {
			return []readiness.Check{
				inferenceCoreCheck(cfg, ev.out, ev.ok()),
				runIntentKeyCheck(cfg, ev.out, ev.ok()),
			}
		},
	}
}

// fastReadinessAxes is the axis subset the fast surfaces request: the ONE core
// launch requirement (a model provider key) plus the two host services whose
// absence silently degrades a session (recall, knowledge). Everything else —
// ollama, models, pack, gworkspace, per-server MCP — belongs to `pix
// doctor`, which is the command whose job is to be thorough.
var fastReadinessAxes = []readiness.Axis{readiness.AxisProviders, readiness.AxisServiceMemory, readiness.AxisServiceKnowledge}

// fastReadinessSnapshot builds the shared fast snapshot from evidence the
// caller already has. The service axes are identity-verified (never a bare
// dial), and both are lazy: a disabled service costs one dial, an enabled and
// running one costs a local JSON-RPC round trip.
func fastReadinessSnapshot(cfg *config.Config, env hostenv.Env, ev sbxKeyEvidence) readiness.Snapshot {
	builders := providersReadinessAxes(cfg, ev)
	for a, b := range serviceReadinessAxes(env, enabled(cfg, "memory"), enabled(cfg, "knowledge"), env.IdentityProbe) {
		builders[a] = b
	}
	return readiness.Build(readiness.Request{Axes: fastReadinessAxes}, builders)
}

// axisReady reports whether a BUILT axis is verified ready. An axis that was
// never requested (absent from the snapshot) is never ready: a renderer must
// not turn "nobody probed this" into a green row.
func axisReady(s readiness.Snapshot, a readiness.Axis) bool {
	_, v, ok := s.AxisVerdict(a)
	return ok && v == readiness.VerdictReady
}

// launchWarningLimit is how many readiness rows `pix run` may print
// before it stops and points at doctor. A wall of readiness text on the daily
// command trains the user to ignore all of it, so the limit is small and the
// remainder is a count, not more rows.
const launchWarningLimit = 3

// readinessWarnings returns the rows a fast surface should warn about: every
// non-note check that is not verified ready, worst first (verified failures
// before "can't check from here") and stable within a rank so the same host
// renders the same order twice.
func readinessWarnings(s readiness.Snapshot) []readiness.Check {
	var failed, unverifiable []readiness.Check
	for _, c := range s.All() {
		if c.Note || c.Result() == readiness.VerdictReady {
			continue
		}
		if c.Result() == readiness.VerdictUnverifiable {
			unverifiable = append(unverifiable, c)
			continue
		}
		failed = append(failed, c)
	}
	return append(failed, unverifiable...)
}

// renderReadinessWarnings prints at most limit warning rows to w through the
// shared vocabulary (glyph/word — a fast surface never spells a
// glyph or a verdict word itself), then a single "N more" pointer at doctor.
// It returns the TOTAL number of warnings, not the number printed, so a caller
// can report the true tally. It never blocks and never exits: the only thing
// that stops a launch is the provider-key gate in run.go.
func renderReadinessWarnings(w io.Writer, s readiness.Snapshot, limit int) int {
	rows := readinessWarnings(s)
	if len(rows) == 0 {
		return 0
	}
	shown := rows
	if limit > 0 && len(rows) > limit {
		shown = rows[:limit]
	}
	for _, c := range shown {
		fmt.Fprintf(w, "  %s %s: %s (%s)\n", readiness.Glyph(c), c.Label, readiness.Word(c), c.EvidenceString())
		// A verified failure always carries its exact repair command; an
		// unverifiable row never does (we do not know there is anything to
		// repair, so a command here would be a guess).
		if v := c.Result(); (v == readiness.VerdictTodo || v == readiness.VerdictDenied) && c.Todo != "" {
			fmt.Fprintf(w, "      fix: %s\n", c.Todo)
		}
	}
	if n := len(rows) - len(shown); n > 0 {
		fmt.Fprintf(w, "  (%d more: run `%s`)\n", n, readiness.Footer("run", s))
	}
	return len(rows)
}
