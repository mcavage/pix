package secret

import (
	"os/exec"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys"
)

// TestOpCanary_RealBinary is the Story09 "real op executable, no mocks"
// canary: it drives OpInstalled/OpSignedIn through sys.Real{} — the actual
// production Exec, never systest.Fake, never a LookPath/Run override — so the
// only thing standing between this test and the real `op` CLI is whatever is
// genuinely on this machine's PATH.
//
// Every OTHER op-touching test in this package (and mcp/pack/slack) fakes
// LookPath so it never depends on 1Password being installed; that is correct
// for unit coverage of the parsing/classification logic, but it means NOTHING
// in the suite ever proves the actual exec wiring against a real op binary.
// This test is that proof, on whichever side of "installed or not" the host
// actually is:
//
//   - op ABSENT (the common case in CI/sandboxes): asserts the HONEST
//     not-installed path — OpInstalled and OpSignedIn both false, never a
//     crash or a false positive — then skips the installed-only assertions.
//   - op PRESENT (a dev laptop, or a CI runner with the CLI staged): asserts
//     OpInstalled agrees with the real exec.LookPath, and that OpSignedIn
//     completes without panicking. OpSignedIn calls ONLY `op account list`
//     (never `op read`, by contract — see its doc comment), so this never
//     resolves, prints, or persists an actual secret value even when a real
//     signed-in account exists.
func TestOpCanary_RealBinary(t *testing.T) {
	env := hostenv.Env{System: sys.Real{}}
	installed := OpInstalled(env)
	_, lookErr := exec.LookPath("op")

	if lookErr != nil {
		if installed {
			t.Fatalf("OpInstalled(sys.Real{})=true but the real exec.LookPath(\"op\") failed: %v", lookErr)
		}
		if OpSignedIn(env) {
			t.Fatal("OpSignedIn(sys.Real{})=true with no op binary genuinely on PATH")
		}
		t.Skip("op is not installed on this host; asserted the honest not-installed path above")
	}

	if !installed {
		t.Fatal("a real op binary is on PATH but OpInstalled(sys.Real{}) reports false")
	}
	// Real invocation of `op account list` via the actual exec.Command path.
	// Any outcome (true or false) is a fact about this machine's op state, not
	// a mock artifact — the only requirement is that it returns rather than
	// panicking or hanging.
	_ = OpSignedIn(env)
}
