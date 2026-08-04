package mcp

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestSbxCanary_RealBinary is the Story09 "real sbx executable, no mocks"
// canary for the mcp package's read path: it drives RunMcpLsCore with the
// REAL exec.LookPath (never a stubbed lookPath func, never systest.Fake), so
// `sbx mcp ls` is either genuinely exec'd or genuinely absent — there is no
// third, faked, outcome.
//
// Every other RunMcpLsCore/RunMcpBundleCore/RunMcpAuthCore test in this
// package injects lookPath so the suite stays hermetic without sbx installed;
// that is correct for exercising ErrSbxUnavailable's plumbing, but it means
// nothing here ever proves the real sbx invocation actually happens the way
// the doc comments claim. This test is that proof, on whichever side of
// "installed or not" the host actually is:
//
//   - sbx ABSENT (the common case in CI/sandboxes): asserts the HONEST
//     ErrSbxUnavailable path — never a silent success, never a hang — then
//     skips the installed-only assertion.
//   - sbx PRESENT: asserts RunMcpLsCore never reports ErrSbxUnavailable (a
//     real binary on PATH must never be reported as missing); the command's
//     own success/failure is left unchecked, since a real `sbx mcp ls` result
//     depends on this host's actual gateway state, not on this test.
func TestSbxCanary_RealBinary(t *testing.T) {
	var out, errOut bytes.Buffer
	err := RunMcpLsCore(exec.LookPath, &out, strings.NewReader(""), &errOut)
	_, lookErr := exec.LookPath("sbx")

	if lookErr != nil {
		if !errors.Is(err, ErrSbxUnavailable) {
			t.Fatalf("sbx is not on PATH: want ErrSbxUnavailable, got %v", err)
		}
		t.Skip("sbx is not installed on this host; asserted the honest ErrSbxUnavailable path above")
	}

	if errors.Is(err, ErrSbxUnavailable) {
		t.Fatal("a real sbx binary is on PATH but RunMcpLsCore reported ErrSbxUnavailable")
	}
}
