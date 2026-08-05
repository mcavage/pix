// bootstrap.go: the "bare-minimum keys" provisioning flow used by `pix run`
// (auto, only when no key is present). Policy: 1Password (op required). `pix
// setup` does NOT use BootstrapProviderKeys — its setupProvisionKeys path
// validates and reconciles the configured refs into sbx. This file keeps the
// tri-state sbx probes both share.
package launch

import (
	"io"

	"pix/host/hostenv"
	"pix/host/secret"
)

// SbxModelKeyState probes sbx for a model provider key (anthropic/openai/
// google; github does not count) as a TRI-STATE: present says a key is set,
// probeOK says we could actually check. probeOK is false when sbx is absent,
// when `sbx secret ls` errors (control plane down), or when it hangs — the
// caller must NOT treat any of those as "no key", because under run's
// tri-state rule unknown PROCEEDS and only a confirmed absence blocks.
func SbxModelKeyState(env hostenv.Env) (present, probeOK bool) {
	if _, err := env.LookPath("sbx"); err != nil {
		return false, false
	}
	out, timedOut, err := env.RunTimed("sbx", "secret", "ls")
	if err != nil || timedOut {
		return false, false
	}
	return secret.AnyModelKeyInOutput(out), true
}

// AnyModelKeyPresent reports whether sbx has at least one model provider key.
// False when sbx cannot be probed (can't verify -> caller decides).
func AnyModelKeyPresent(env hostenv.Env) bool {
	present, _ := SbxModelKeyState(env)
	return present
}

// BootstrapProviderKeys is `run`'s "get a usable model key in place" step:
// resolve any existing 1Password refs into sbx; if a model key is now present,
// done; otherwise, on a TTY, offer 1Password (writing op-refs.env +
// hostmode.env and syncing into sbx) so the sandbox and host mode get keys from
// one paste.
//
// Returns whether a model key ended up present. It NEVER blocks or exits — run
// refuses to launch only when SbxModelKeyState POSITIVELY confirms no key.
// Idempotent: with a key present it does nothing beyond a cheap sbx probe.
func BootstrapProviderKeys(env hostenv.Env, in io.Reader, out io.Writer, tty bool) bool {
	secret.EnsureProviderKeysFromRefs(env, out)
	if AnyModelKeyPresent(env) {
		return true
	}
	if tty {
		secret.OfferOnePasswordKeys(env, in, out, tty)
		secret.EnsureProviderKeysFromRefs(env, out)
	}
	return AnyModelKeyPresent(env)
}
