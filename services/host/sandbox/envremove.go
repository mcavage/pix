// envremove.go — E2.4's production sandbox.PlanEnvRemove (docs/design/
// environments.md §10.3, docs/upstream/sbx-0.39-environments.md §10):
// planning the environment-scoped removal `sbx env create <effective>`
// pairs with, so scoped secrets that create call registered are removed
// through the SAME identity, never a bare name.
//
// Story 0's uatenvmatrix package pinned this policy's SHAPE early
// (uatenvmatrix/check_rm_scope_refusal.go's own planEnvRemoveRefusal) as a
// literal, check-owned duplicate, because "Story 0 has no production
// sandbox.PlanEnvRemove yet" (that file's own doc comment). This file is
// that production function. It does not replace the check's fixture, and
// the check does not import this package — see arch_test.go's
// TestArchitecture_UatenvmatrixNeverImportsEnvinfo sibling rule, which
// applies here too: uatenvmatrix stays a self-contained probe.
package sandbox

import "fmt"

// PlanEnvRemove composes the argv for `sbx env rm -f <effectivePath>`
// (docs/design/environments.md §10.3's "The normal mutation is `sbx env rm
// -f <effective>`"), scoped by the EXACT SAME two proofs PlanRemove and
// PlanForceRemove already share via validateScopedName, plus one more this
// environment-aware seam adds: the effective name this call recomputed
// must equal the pix-* instance name already recorded for this sandbox.
//
// Callers do not hand this function a workspace or a document to derive
// effectiveName from: like every other L1 capability in this package
// (doc.go, "a caller supplies any rendered pix-* name itself" —
// envinfo/doc.go's own words about this exact boundary), sandbox takes no
// dependency on envinfo. effectiveName is whatever the CALLER already
// recomputed for effectivePath — reading the effective document's own
// `name:` field back (the identity §6.2 says composition never
// determines, only fills in) is the intended source, but this function
// never reads a file itself.
//
// Refusing either proof composes NO argv at all:
//
//   - effectiveName outside the pix-* namespace, or unsafely charactered —
//     validateScopedName's existing check, unchanged;
//   - effectiveName safely pix-* scoped but NOT EQUAL to
//     recordedInstanceName — an effective file that was edited, replaced,
//     or simply belongs to a different sandbox than the one this caller's
//     own launch/session state recorded.
//
// On success, PlanEnvRemove returns exactly:
//
//	[]string{"env", "rm", "-f", effectivePath}
//
// and NEVER a `--prune-bindings` flag (or any other flag beyond `-f`):
// docs/design/environments.md §4/§9.2 documents host-global credential
// bindings and MCP registrations as PRESERVED by default across a removal,
// and pruning them is not this function's authority to request, ever —
// not behind a caller-supplied option, not as a "cleaner" default. A3's
// nonclaim stays exactly what it was (this run makes no claim that
// bindings/MCP registrations survive removal in every possible
// configuration) — see envremove_test.go's argv-matrix test, which proves
// the narrower, checkable half: no argv this function can produce ever
// asks for a prune.
//
// Like PlanRemove and PlanForceRemove, PlanEnvRemove never executes
// anything: it returns argv (or an error) and stops there. The `-f` it
// composes is a transport detail, not a widened authority — the identical
// posture PlanForceRemove's own doc comment states. The SAME two proofs
// (a kernel-verified zero-holder reference, or an explicitly named
// removal intent with no lease state left to prove against) must already
// be established by the CALLER, and the recordedInstanceName equality
// check above, before this function is ever reached — composing this argv
// is not itself the authority to run it.
func PlanEnvRemove(effectivePath, effectiveName, recordedInstanceName string) ([]string, error) {
	if err := validateScopedName(effectiveName); err != nil {
		return nil, err
	}
	if effectiveName != recordedInstanceName {
		return nil, fmt.Errorf(
			"sandbox: refusing to plan env removal of %q: effective name %q does not match recorded instance %q",
			effectivePath, effectiveName, recordedInstanceName)
	}
	if effectivePath == "" {
		return nil, fmt.Errorf("sandbox: refusing to plan env removal: empty effective path")
	}
	return []string{"env", "rm", "-f", effectivePath}, nil
}
