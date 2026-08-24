// cleanup_test.go proves cleanupCreatedFixture (cleanup.go) in isolation: the
// ONE shared receipt-gated teardown every check that creates a real
// pix-uatenv-* fixture sandbox must call, on both its success and
// downstream-failure paths. These are this unit's red tests, written before
// cleanup.go existed — docs/design/environments.md section 9.3's production
// policy for `sbx env rm -f`, applied here to this matrix's own fixtures.
package uatenvmatrix

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// cleanupCall is one recorded Executor.Run invocation, keyed by argv only —
// everything these tests need to answer deterministically and to assert on
// afterward.
type cleanupCall struct {
	args []string
}

type cleanupFakeExecutor struct {
	calls []cleanupCall
	// ls answers every `sbx ls --json` fresh-probe call.
	lsOut string
	lsErr error
	// rm answers every `sbx env rm -f <path>` removal call.
	rmErr error
}

func (f *cleanupFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	f.calls = append(f.calls, cleanupCall{args: append([]string(nil), args...)})
	if len(args) > 0 && args[0] == "ls" {
		return f.lsOut, "", f.lsErr
	}
	if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
		return "", "", f.rmErr
	}
	return "", "", errors.New("cleanupFakeExecutor: unexpected call " + strings.Join(args, " "))
}

const cleanupTestFixtureName = "pix-uatenv-fixture-cleanup-test"
const cleanupTestFixturePath = "/tmp/does-not-matter/cleanup-test.sbxenv.yaml"

// TestCleanupCreatedFixture_NoReceiptNeverRemoves is this unit's first red
// test: a create call that itself failed (createErr != nil) carries no
// receipt at all, so cleanup must fail closed and issue ZERO further
// commands — matching check_failed_create_cleanup.go's own before-receipt
// policy, generalized into the one shared helper.
func TestCleanupCreatedFixture_NoReceiptNeverRemoves(t *testing.T) {
	var lw strings.Builder
	fe := &cleanupFakeExecutor{}
	err := cleanupCreatedFixture(context.Background(), &lw, fe, nil, "", cleanupTestFixturePath, cleanupTestFixtureName, "", errors.New("create failed"))
	if err != nil {
		t.Fatalf("expected no error for a receipt-less create (nothing to clean up), got: %v", err)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("expected zero executor calls with no create receipt, got %d: %#v", len(fe.calls), fe.calls)
	}
	if !strings.Contains(lw.String(), "no positive create receipt") {
		t.Errorf("artifact does not record why no removal was attempted: %s", lw.String())
	}
}

// TestCleanupCreatedFixture_MismatchedReceiptNeverRemoves proves an
// ACCEPTED create (err == nil) whose output never positively identifies
// expectedName is treated exactly like no receipt at all: zero removal
// authority, zero executor calls.
func TestCleanupCreatedFixture_MismatchedReceiptNeverRemoves(t *testing.T) {
	var lw strings.Builder
	fe := &cleanupFakeExecutor{}
	err := cleanupCreatedFixture(context.Background(), &lw, fe, nil, "", cleanupTestFixturePath, cleanupTestFixtureName, "accepted\n", nil)
	if err != nil {
		t.Fatalf("expected no error for a mismatched/unidentified receipt, got: %v", err)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("expected zero executor calls with a mismatched receipt, got %d: %#v", len(fe.calls), fe.calls)
	}
}

// TestCleanupCreatedFixture_FailedFreshProbeNeverRemoves proves a positive
// create receipt is NOT enough on its own: cleanup must reconfirm the exact
// same identity with a FRESH probe issued after create, and any probe error
// or absence fails closed without ever calling a removal command.
func TestCleanupCreatedFixture_FailedFreshProbeNeverRemoves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lsOut string
		lsErr error
	}{
		{"probe executor error", "", errors.New("dial tcp: connection refused")},
		{"probe succeeds but identity absent", "pix-uatenv-fixture-someone-else\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lw strings.Builder
			fe := &cleanupFakeExecutor{lsOut: tc.lsOut, lsErr: tc.lsErr}
			createOut := "created " + cleanupTestFixtureName + " (positively identified)\n"
			err := cleanupCreatedFixture(context.Background(), &lw, fe, nil, "", cleanupTestFixturePath, cleanupTestFixtureName, createOut, nil)
			if err == nil {
				t.Fatal("expected a fresh-probe failure to be reported as an error, got nil")
			}
			if len(fe.calls) != 1 {
				t.Fatalf("expected exactly 1 executor call (the fresh probe, no removal attempted), got %d: %#v", len(fe.calls), fe.calls)
			}
			if fe.calls[0].args[0] != "ls" {
				t.Fatalf("expected the one call to be the fresh probe (`sbx ls ...`), got %#v", fe.calls[0].args)
			}
			if !strings.Contains(lw.String(), "residue possible") {
				t.Errorf("artifact does not record residue evidence: %s", lw.String())
			}
		})
	}
}

// TestCleanupCreatedFixture_RemovalCommandFailureIsReported proves that once
// both proofs hold (receipt + fresh probe), cleanup issues the
// environment-scoped removal command, and a failure of THAT command is
// reported rather than silently swallowed.
func TestCleanupCreatedFixture_RemovalCommandFailureIsReported(t *testing.T) {
	var lw strings.Builder
	fe := &cleanupFakeExecutor{
		lsOut: "created " + cleanupTestFixtureName + " (positively identified)\n",
		rmErr: errors.New("sbx: exit status 1"),
	}
	createOut := "created " + cleanupTestFixtureName + " (positively identified)\n"
	err := cleanupCreatedFixture(context.Background(), &lw, fe, nil, "", cleanupTestFixturePath, cleanupTestFixtureName, createOut, nil)
	if err == nil {
		t.Fatal("expected the removal command failure to be reported as an error, got nil")
	}
	if len(fe.calls) != 2 {
		t.Fatalf("expected exactly 2 executor calls (fresh probe, then the removal attempt), got %d: %#v", len(fe.calls), fe.calls)
	}
	if fe.calls[1].args[0] != "env" || fe.calls[1].args[1] != "rm" {
		t.Fatalf("expected the second call to be the environment-scoped removal, got %#v", fe.calls[1].args)
	}
}

// TestCleanupCreatedFixture_ReceiptedAndProbeConfirmedRemoves is the success
// path: a positive receipt AND a fresh probe confirming the same identity
// removes via the environment-scoped `sbx env rm -f <fixturePath>` — never a
// bare `sbx rm`, never `--prune-bindings`.
func TestCleanupCreatedFixture_ReceiptedAndProbeConfirmedRemoves(t *testing.T) {
	var lw strings.Builder
	fe := &cleanupFakeExecutor{lsOut: "created " + cleanupTestFixtureName + " (positively identified)\n"}
	createOut := "created " + cleanupTestFixtureName + " (positively identified)\n"
	err := cleanupCreatedFixture(context.Background(), &lw, fe, nil, "", cleanupTestFixturePath, cleanupTestFixtureName, createOut, nil)
	if err != nil {
		t.Fatalf("expected success for a receipted, freshly-reconfirmed instance, got: %v", err)
	}
	if len(fe.calls) != 2 {
		t.Fatalf("expected exactly 2 executor calls (fresh probe, then removal), got %d: %#v", len(fe.calls), fe.calls)
	}
	probeCall, removeCall := fe.calls[0], fe.calls[1]
	if probeCall.args[0] != "ls" {
		t.Errorf("first call = %#v, want the fresh `sbx ls` probe", probeCall.args)
	}
	wantRemove := []string{"env", "rm", "-f", cleanupTestFixturePath}
	if len(removeCall.args) != len(wantRemove) {
		t.Fatalf("removal call = %#v, want %#v", removeCall.args, wantRemove)
	}
	for i, want := range wantRemove {
		if removeCall.args[i] != want {
			t.Errorf("removal call args[%d] = %q, want %q (full: %#v)", i, removeCall.args[i], want, removeCall.args)
		}
	}
	for _, a := range removeCall.args {
		if a == "--prune-bindings" {
			t.Fatal("removal call passed --prune-bindings; Pix never supplies it automatically")
		}
	}
	if !strings.Contains(lw.String(), "cleanup: removed "+cleanupTestFixtureName) {
		t.Errorf("artifact does not record the removal evidence: %s", lw.String())
	}
}

// TestCleanupCreatedFixture_NeverRemovesOutsidePixUatenvNamespace is the
// scope guard: cleanupCreatedFixture must refuse outright — before ever
// touching the injected Executor — for any name outside the dedicated
// pix-uatenv-* namespace, even one otherwise pix-* scoped (a real user
// sandbox, or a name an unrelated Pix workflow created).
func TestCleanupCreatedFixture_NeverRemovesOutsidePixUatenvNamespace(t *testing.T) {
	for _, name := range []string{
		"pix-some-real-user-sandbox",
		"pix-uat-worker-session",
		"not-even-pix-scoped",
		"",
	} {
		t.Run(name, func(t *testing.T) {
			var lw strings.Builder
			fe := &cleanupFakeExecutor{lsOut: "created " + name + " (positively identified)\n"}
			createOut := "created " + name + " (positively identified)\n"
			err := cleanupCreatedFixture(context.Background(), &lw, fe, nil, "", cleanupTestFixturePath, name, createOut, nil)
			if err == nil {
				t.Fatalf("expected an out-of-scope refusal for name %q, got nil", name)
			}
			if len(fe.calls) != 0 {
				t.Fatalf("expected zero executor calls for out-of-scope name %q, got %d: %#v", name, len(fe.calls), fe.calls)
			}
		})
	}
}
