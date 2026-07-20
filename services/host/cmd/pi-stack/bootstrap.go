// bootstrap.go: the SHARED "bare-minimum keys" provisioning flow used by BOTH
// `pi-stack run` (auto, only when no key is present) and `pi-stack setup` (as the
// keys step of the guided flow). Same code, one policy: steer to 1Password.
package main

import (
	"io"
)

// anyModelKeyPresent reports whether sbx has at least one model provider key
// (anthropic/openai/google). github does not count. Returns false when sbx can't
// be probed (can't verify -> caller decides).
func anyModelKeyPresent(env shellEnv) bool {
	if env.lookPath == nil || env.run == nil {
		return false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return false
	}
	out, err := env.run("sbx", "secret", "ls")
	if err != nil {
		return false
	}
	for _, k := range modelProviders {
		if grepWord(out, k) {
			return true
		}
	}
	return false
}

// bootstrapProviderKeys is the shared "get a usable model key in place" step:
//  1. resolve any existing 1Password key refs into sbx (the no-ritual path);
//  2. if a model key is now present, done;
//  3. otherwise, on a TTY, offer 1Password (writes op-refs.env + hostmode.env and
//     syncs into sbx) so both the sandbox and host mode get keys from one paste.
//
// Returns whether a model key ended up present. It NEVER blocks or exits \u2014 the
// caller decides what to do when it returns false (run refuses to launch; setup
// reports and continues). Idempotent: with a key already present it does nothing
// beyond a cheap sbx probe (op is never touched).
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
