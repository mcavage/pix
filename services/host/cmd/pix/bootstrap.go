// bootstrap.go: the "bare-minimum keys" provisioning flow used by `pix run`
// (auto, only when no key is present). Policy: 1Password (op required). `pix
// setup` does NOT use bootstrapProviderKeys. Its setupProvisionKeys path
// validates and reconciles the configured 1Password refs into sbx. This file keeps
// the tri-state sbx probes (sbxModelKeyState, secret.SbxAllModelKeysPresent) both share.
package main

import (
	"io"
	"pix/host/hostenv"
	"pix/host/readiness/axis"
	"pix/host/secret"
)

// sbxModelKeyState probes sbx for a model provider key (anthropic/openai/google;
// github does not count) as a TRI-STATE: present says a key is set, probeOK says
// we could actually check. probeOK is false when sbx is absent OR `sbx secret ls`
// errors (control plane down) — the caller must NOT treat that as "no key".
func sbxModelKeyState(env hostenv.Env) (present, probeOK bool) {
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
	return axis.AnyModelKeyInOutput(out), true
}

// anyModelKeyPresent reports whether sbx has at least one model provider key.
// Returns false when sbx can't be probed (can't verify -> caller decides).
func anyModelKeyPresent(env hostenv.Env) bool {
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
func bootstrapProviderKeys(env hostenv.Env, in io.Reader, out io.Writer, tty bool) bool {
	secret.EnsureProviderKeysFromRefs(env, out)
	if anyModelKeyPresent(env) {
		return true
	}
	if tty {
		secret.OfferOnePasswordKeys(env, in, out, tty)
		secret.EnsureProviderKeysFromRefs(env, out)
	}
	return anyModelKeyPresent(env)
}
