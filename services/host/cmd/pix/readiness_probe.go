package main

import (
	"errors"
	"fmt"
	"os/exec"
	"pix/host/hostenv"
	"pix/host/sys"
	"strings"
)

// doctor_probe.go is the SHARED bounded-probe + canonical-executable-trust
// machinery used by both the MCP truth group (doctor_mcp.go, S05) and the gog
// group (doctor_gog.go, S07). It was consolidated here after both stories
// independently grew the same primitives during integration: probeListTools /
// probeStatus / classifyProbeErr (the `--list-tools` probe + its outcome
// classification) and trustedExecPath / mcp.TrustedGogSpawn (the
// canonical-executable trust gate). doctor_gog.go's copy had grown one extra
// outcome — probeDeniedByPolicy, an EXPLICIT policy/permission refusal
// distinguished from a generic probe error — so that is the superset kept
// here; both callers (gogSpawnCheck and mcpLocalCheck) map it to
// readiness.VerdictDenied, and every other unclassified failure stays unverifiable.

// probeStatus/probeResult are the STRUCTURED outcome of a `--list-tools`
// probe: a clean non-empty list is healthy; a clean EMPTY list is a verified
// zero-tools failure (the headless creds/keyring trap); a timeout or exec
// error is unverifiable — doctor doesn't know, so it must never mislabel
// those as a keyring failure; and an EXPLICIT policy denial in a failed
// probe's output is a positive refusal (verdict denied), not a setup gap.
type probeStatus int

const (
	probeToolsOK        probeStatus = iota // clean exit, non-empty tool list
	probeNoTools                           // clean exit, ZERO tools — a verified failure
	probeTimedOut                          // hit the probe deadline — unverifiable
	probeError                             // exec failure / non-zero exit / missing binary — unverifiable
	probeDeniedByPolicy                    // failed with an EXPLICIT policy/permission denial
)

type probeResult struct {
	status probeStatus
	detail string // short, value-free diagnostic for timeout/error outcomes
	tools  int    // tool-line count on a clean exit
}

// probeListTools runs argv with `--list-tools` appended, BOUNDED by probeRun's
// timeout + output cap, and classifies the outcome. A failed probe's output is
// run through sys.ClassifyProbeFailure so an EXPLICIT policy denial classifies as
// sys.ProbeDenied rather than a generic error. The diagnostic is deliberately
// generic (never raw error text), so a registered command's tokens — which may
// carry pasted secrets — can never leak through an error message.
func probeListTools(env hostenv.Env, argv []string) probeResult {
	if len(argv) == 0 {
		return probeResult{status: probeError, detail: "has no command to run"}
	}
	full := append(append([]string{}, argv...), "--list-tools")
	out, timedOut, err := env.RunTimed(full[0], full[1:]...)
	if timedOut {
		return probeResult{status: probeTimedOut, detail: fmt.Sprintf("timed out after %s", probeTimeout)}
	}
	if err != nil {
		if sys.ClassifyProbeFailure(out, err) == sys.ProbeDenied {
			return probeResult{status: probeDeniedByPolicy, detail: "positively refused by policy/permission"}
		}
		return probeResult{status: probeError, detail: classifyProbeErr(err)}
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	if n == 0 {
		return probeResult{status: probeNoTools}
	}
	return probeResult{status: probeToolsOK, tools: n}
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
