// bootstrap.go: the "bare-minimum keys" provisioning flow used by `pi-stack run`
// (auto, only when no key is present). Policy: steer to 1Password. `pi-stack
// setup` does NOT use bootstrapProviderKeys — it runs the stronger
// setupProvisionKeys (always sources from 1Password, force-syncs sbx); this file
// keeps the tri-state sbx probe (sbxModelKeyState) both share.
package main

import (
	"io"
)

// sbxModelKeyState probes sbx for a model provider key (anthropic/openai/google;
// github does not count) as a TRI-STATE: present says a key is set, probeOK says
// we could actually check. probeOK is false when sbx is absent OR `sbx secret ls`
// errors (control plane down) — the caller must NOT treat that as "no key".
func sbxModelKeyState(env shellEnv) (present, probeOK bool) {
	if env.lookPath == nil || env.run == nil {
		return false, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return false, false
	}
	out, err := env.run("sbx", "secret", "ls")
	if err != nil {
		return false, false
	}
	for _, k := range modelProviders {
		if grepWord(out, k) {
			return true, true
		}
	}
	return false, true
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
