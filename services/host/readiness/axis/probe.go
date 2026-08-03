package axis

import (
	"errors"
	"fmt"
	"os/exec"
	"pix/host/hostenv"
	"pix/host/sys"
	"strings"
	"time"
)

// doctor_probe.go is the SHARED bounded-probe + canonical-executable-trust
// machinery used by both the MCP truth group (doctor_mcp.go, S05) and the gog
// group (doctor_gog.go, S07). It was consolidated here after both stories
// independently grew the same primitives during integration: ProbeListTools /
// probeStatus / classifyProbeErr (the `--list-tools` probe + its outcome
// classification) and trustedExecPath / mcp.TrustedGogSpawn (the
// canonical-executable trust gate). doctor_gog.go's copy had grown one extra
// outcome — ProbeDeniedByPolicy, an EXPLICIT policy/permission refusal
// distinguished from a generic probe error — so that is the superset kept
// here; both callers (gogSpawnCheck and mcpLocalCheck) map it to
// readiness.VerdictDenied, and every other unclassified failure stays unverifiable.

// probeStatus/ProbeResult are the STRUCTURED outcome of a `--list-tools`
// probe: a clean non-empty list is healthy; a clean EMPTY list is a verified
// zero-tools failure (the headless creds/keyring trap); a timeout or exec
// error is unverifiable — doctor doesn't know, so it must never mislabel
// those as a keyring failure; and an EXPLICIT policy denial in a failed
// probe's output is a positive refusal (verdict denied), not a setup gap.
type probeStatus int

const (
	ProbeToolsOK        probeStatus = iota // clean exit, non-empty tool list
	ProbeNoTools                           // clean exit, ZERO tools — a verified failure
	probeTimedOut                          // hit the probe deadline — unverifiable
	ProbeError                             // exec failure / non-zero exit / missing binary — unverifiable
	ProbeDeniedByPolicy                    // failed with an EXPLICIT policy/permission denial
)

type ProbeResult struct {
	Status probeStatus
	Detail string // short, value-free diagnostic for timeout/error outcomes
	Tools  int    // tool-line count on a clean exit
}

// ProbeListTools runs argv with `--list-tools` appended, BOUNDED by probeRun's
// timeout + output cap, and classifies the outcome. A failed probe's output is
// run through sys.ClassifyProbeFailure so an EXPLICIT policy denial classifies as
// sys.ProbeDenied rather than a generic error. The diagnostic is deliberately
// generic (never raw error text), so a registered command's tokens — which may
// carry pasted secrets — can never leak through an error message.
func ProbeListTools(env hostenv.Env, argv []string) ProbeResult {
	if len(argv) == 0 {
		return ProbeResult{Status: ProbeError, Detail: "has no command to run"}
	}
	full := append(append([]string{}, argv...), "--list-tools")
	out, timedOut, err := env.RunTimed(full[0], full[1:]...)
	if timedOut {
		return ProbeResult{Status: probeTimedOut, Detail: fmt.Sprintf("timed out after %s", probeTimeout)}
	}
	if err != nil {
		if sys.ClassifyProbeFailure(out, err) == sys.ProbeDenied {
			return ProbeResult{Status: ProbeDeniedByPolicy, Detail: "positively refused by policy/permission"}
		}
		return ProbeResult{Status: ProbeError, Detail: classifyProbeErr(err)}
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	if n == 0 {
		return ProbeResult{Status: ProbeNoTools}
	}
	return ProbeResult{Status: ProbeToolsOK, Tools: n}
}

// classifyProbeErr maps a probe error to a short, value-free diagnostic: it
// distinguishes a missing/non-executable binary from a non-zero exit without
// ever echoing raw error text (which could embed registered-command tokens).
func classifyProbeErr(err error) string {
	var xe *exec.Error
	if errors.As(err, &xe) {
		return "could not run (binary not found or not executable)"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exited non-zero (%s)", ee.ProcessState)
	}
	return "could not be run"
}

// fake returns the embedded System as the test double, for fixtures that build
// a base env and then override one seam. TEST-ONLY: it panics on a real env,
// which is the right outcome for test-only code reached in production — the
// alternative is a silent no-op, and silent no-ops are what this refactor
// exists to delete.
// probeTimeout bounds every registered-command probe so doctor can never wedge
// on a hung MCP server; probeMaxOutput caps how much of its output we capture.
const (
	probeTimeout   = 5 * time.Second
	probeMaxOutput = 64 << 10 // 64KB
)
