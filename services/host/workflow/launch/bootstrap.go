// bootstrap.go: the "does this host have a model credential at all" question
// `pix run` asks before it creates anything, and the one-time interactive
// offer that fixes a No.
//
// The evidence changed in Wave C and the change is load-bearing: the answer
// comes from THIS PIX_HOME's configured op:// refs, never from `sbx secret ls`.
// A global sbx secret is host-wide — it belongs to whoever pushed it, survives
// this PIX_HOME, and is readable by every other stack's sandboxes — so
// treating one as proof that this launcher has a key is how a stack with no
// credentials of its own silently launched on someone else's. Pix writes only
// sandbox-scoped secrets (secret.PrepareSandboxSecrets) and reads only its own
// refs.
package launch

import (
	"io"

	"pix/host/hostenv"
	"pix/host/secret"
)

// ConfiguredModelKeyState is the tri-state the launch gate turns on: does this
// PIX_HOME configure at least one model provider ref (present), and could the
// question be answered at all (probeOK). An unreadable refs file answers
// (false, false) — unknown, which never refuses a launch.
func ConfiguredModelKeyState(env hostenv.Env) (present, probeOK bool) {
	names, state := secret.ConfiguredModelRefs(env)
	if state != secret.RefsAnswered {
		return false, false
	}
	return len(names) > 0, true
}

// AnyModelKeyPresent reports whether this PIX_HOME configures at least one
// model provider ref.
func AnyModelKeyPresent(env hostenv.Env) bool {
	present, _ := ConfiguredModelKeyState(env)
	return present
}

// BootstrapProviderKeys is `pix run`'s auto-provisioning step: if this
// PIX_HOME configures no model ref and we are on a TTY, offer to write one
// (op:// refs only — nothing is resolved here and nothing host-wide is
// written). It reports whether a model ref is configured afterwards.
func BootstrapProviderKeys(env hostenv.Env, in io.Reader, out io.Writer, tty bool) bool {
	if AnyModelKeyPresent(env) {
		return true
	}
	if tty {
		secret.OfferOnePasswordKeys(env, in, out, tty)
	}
	return AnyModelKeyPresent(env)
}
