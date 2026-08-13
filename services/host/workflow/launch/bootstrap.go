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
func AnyModelKeyPresent(env hostenv.Env) bool {
	present, _ := SbxModelKeyState(env)
	return present
}

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


