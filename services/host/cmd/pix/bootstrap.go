// bootstrap.go: the "bare-minimum keys" provisioning flow used by `pix run`
// (auto, only when no key is present). Policy: 1Password (op required). `pix
// setup` does NOT use bootstrapProviderKeys. Its setupProvisionKeys path
// validates and reconciles the configured 1Password refs into sbx. This file keeps
// the tri-state sbx probes (sbxModelKeyState, sbxAllModelKeysPresent) both share.
package main

import (
	"io"
)

// sbxModelKeyState probes sbx for a model provider key (anthropic/openai/google;
// github does not count) as a TRI-STATE: present says a key is set, probeOK says
// we could actually check. probeOK is false when sbx is absent OR `sbx secret ls`
// errors (control plane down) — the caller must NOT treat that as "no key".
func sbxModelKeyState(env shellEnv) (present, probeOK bool) {
	if _, err := env.LookPath("sbx"); err != nil {
		return false, false
	}
	// BOUNDED (probeRun): a hung `sbx secret ls` degrades to probeOK=false —
	// under run's tri-state rule that PROCEEDS (unknown never blocks a launch;
	// only a positively confirmed missing key does) — never a wedged preflight.
	out, timedOut, err := env.RunTimed("sbx", "secret", "ls")
	if err != nil || timedOut {
		return false, false
	}
	return anyModelKeyInOutput(out), true
}

// anyModelKeyInOutput reports whether out (the text of `sbx secret ls`) shows
// any of the model provider keys set. Pure — the SINGLE definition of "what
// counts as a present model key", shared by sbxModelKeyState (which owns the
// live sbx probe) and doctor's providers group (which reuses an
// already-fetched probe result) so the two can never diverge.
func anyModelKeyInOutput(out string) bool {
	for _, k := range modelProviders {
		if grepWord(out, k) {
			return true
		}
	}
	return false
}

// sbxSecretsProbeState distinguishes WHY `sbx secret ls` couldn't answer, so
// callers with a MANDATORY-keys invariant (setupProvisionKeys) can fail-open
// only for genuine portability (sbx isn't installed here at all — there is
// nothing to reconcile against) and fail CLOSED with a diagnostic when sbx IS
// installed but its control plane errored (a real, fixable problem, not "no
// sandbox here"). sbxModelKeyState (the `run`/bootstrap path) deliberately
// keeps its own coarser tri-state — `run` fails open on EITHER cause, since
// its only question is "is there a key", not "can I trust a completeness
// claim".
type sbxSecretsProbeState int

const (
	sbxSecretsAbsent sbxSecretsProbeState = iota // sbx not on PATH: fail-open (portability)
	sbxSecretsError                              // sbx on PATH but `sbx secret ls` failed: fail CLOSED
	sbxSecretsOK                                 // sbx on PATH and `sbx secret ls` succeeded
)

// probeSbxSecrets runs `sbx secret ls` and classifies the result into
// sbxSecretsProbeState. out is only meaningful when state == sbxSecretsOK.
func probeSbxSecrets(env shellEnv) (out string, state sbxSecretsProbeState) {
	if _, err := env.LookPath("sbx"); err != nil {
		return "", sbxSecretsAbsent
	}
	// BOUNDED (probeRun): a hung `sbx secret ls` classifies as sbxSecretsError
	// (sbx IS on PATH — a real, diagnosable problem, never "absent") instead of
	// hanging the caller forever.
	o, timedOut, err := env.RunTimed("sbx", "secret", "ls")
	if err != nil || timedOut {
		return "", sbxSecretsError
	}
	return o, sbxSecretsOK
}

// sbxAllModelKeysPresent probes sbx for ALL THREE model provider keys
// (anthropic/openai/google). `pix setup`'s mandatory-keys invariant
// requires ALL three (not merely one), so this is deliberately stricter than
// sbxModelKeyState, which `run` uses to decide "is there ANY usable key".
func sbxAllModelKeysPresent(env shellEnv) (all bool, state sbxSecretsProbeState) {
	out, state := probeSbxSecrets(env)
	if state != sbxSecretsOK {
		return false, state
	}
	for _, k := range modelProviders {
		if !grepWord(out, k) {
			return false, sbxSecretsOK
		}
	}
	return true, sbxSecretsOK
}

// anyModelKeyPresent reports whether sbx has at least one model provider key.
// Returns false when sbx can't be probed (can't verify -> caller decides).
func anyModelKeyPresent(env shellEnv) bool {
	present, _ := sbxModelKeyState(env)
	return present
}

// bootstrapProviderKeys is `run`'s "get a usable model key in place" step:
//  1. resolve any existing 1Password key refs into sbx (the no-ritual path);
//  2. if a model key is now present, done;
//  3. otherwise, on a TTY, offer 1Password (writes op-refs.env + hostmode.env and
//     syncs into sbx) so both the sandbox and host mode get keys from one paste.
//
// Returns whether a model key ended up present. It NEVER blocks or exits \u2014 the
// caller decides (run refuses to launch only when sbxModelKeyState POSITIVELY
// confirms no key). Idempotent: with a key already present it does nothing beyond
// a cheap sbx probe (op is never touched). Only `run` calls this; `setup` uses
// the stronger setupProvisionKeys.
func bootstrapProviderKeys(env shellEnv, in io.Reader, out io.Writer, tty bool) bool {
	ensureProviderKeysFromRefs(env, out)
	if anyModelKeyPresent(env) {
		return true
	}
	if tty {
		offerOnePasswordKeys(env, in, out, tty)
		ensureProviderKeysFromRefs(env, out)
	}
	return anyModelKeyPresent(env)
}
