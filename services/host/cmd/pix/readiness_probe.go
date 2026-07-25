package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// doctor_probe.go is the SHARED bounded-probe + canonical-executable-trust
// machinery used by both the MCP truth group (doctor_mcp.go, S05) and the gog
// group (doctor_gog.go, S07). It was consolidated here after both stories
// independently grew the same primitives during integration: probeListTools /
// probeStatus / classifyProbeErr (the `--list-tools` probe + its outcome
// classification) and trustedExecPath / trustedGogSpawn (the
// canonical-executable trust gate). doctor_gog.go's copy had grown one extra
// outcome — probeDeniedByPolicy, an EXPLICIT policy/permission refusal
// distinguished from a generic probe error — so that is the superset kept
// here; both callers (gogSpawnCheck and mcpLocalCheck) map it to
// verdictDenied, and every other unclassified failure stays unverifiable.

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
// run through classifyProbeFailure so an EXPLICIT policy denial classifies as
// probeDenied rather than a generic error. The diagnostic is deliberately
// generic (never raw error text), so a registered command's tokens — which may
// carry pasted secrets — can never leak through an error message.
func probeListTools(env shellEnv, argv []string) probeResult {
	if len(argv) == 0 {
		return probeResult{status: probeError, detail: "has no command to run"}
	}
	full := append(append([]string{}, argv...), "--list-tools")
	out, timedOut, err := probeRun(env, full[0], full[1:]...)
	if timedOut {
		return probeResult{status: probeTimedOut, detail: fmt.Sprintf("timed out after %s", probeTimeout)}
	}
	if err != nil {
		if classifyProbeFailure(out, err) == probeDenied {
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

// trustedExecPath is the canonical-executable gate: it returns the exec token
// doctor may run for base, and whether the registered token is trusted. A bare
// name (no path separator) is trusted as-is — exec resolves it through PATH at
// spawn time, which IS lookPath's answer; there is no recorded path for an
// attacker to swap. A path-carrying token must be byte-equal (cleaned) to the
// PATH-resolved binary — STRICT equality only, with symlink resolution
// deliberately NOT consulted (a check-time symlink bless followed by exec of
// the registered path is a race the attacker wins by swapping the link). On
// success the returned token is the RESOLVER's canonical path, never the
// registered spelling, so the exec'd token is the trusted one by construction.
// Anything else (a look-alike /tmp/gog, a fake op) is untrusted and never
// executed.
func trustedExecPath(env shellEnv, tok, base string) (string, bool) {
	if filepath.Base(tok) != base {
		return "", false
	}
	if !strings.ContainsAny(tok, `/\`) {
		return tok, true // bare name: exec resolves via PATH = lookPath's answer
	}
	if env.lookPath == nil {
		return "", false
	}
	canonical, err := env.lookPath(base)
	if err != nil || canonical == "" {
		return "", false
	}
	if filepath.Clean(tok) != filepath.Clean(canonical) {
		return "", false
	}
	return filepath.Clean(canonical), true
}

// trustedGogSpawn reports whether a registered gog command is BOTH the
// recognized gog shape (gogSpawnArgv) AND built from canonical executables:
// the inner gog binary must match env.lookPath("gog"), and — when op-wrapped —
// the op binary must match env.lookPath("op"). On success it returns the
// NORMALIZED argv: the gog/op executable tokens replaced with the resolvers'
// canonical paths, so the caller execs the TRUSTED tokens, never the
// registered spelling. Only that normalized spawn is ever executed as a probe.
func trustedGogSpawn(env shellEnv, argv []string) ([]string, bool) {
	inner, ok := gogSpawnArgv(env, argv)
	if !ok {
		return nil, false
	}
	gogTok, gogOK := trustedExecPath(env, inner[0], "gog")
	if !gogOK {
		return nil, false
	}
	norm := append([]string(nil), argv...)
	// inner is the suffix gogSpawnArgv/unwrapOpRun peeled off argv, so the
	// inner executable sits at len(argv)-len(inner); >0 means op-wrapped.
	innerStart := len(argv) - len(inner)
	norm[innerStart] = gogTok
	if innerStart > 0 {
		opTok, opOK := trustedExecPath(env, argv[0], "op")
		if !opOK {
			return nil, false
		}
		norm[0] = opTok
	}
	return norm, true
}
