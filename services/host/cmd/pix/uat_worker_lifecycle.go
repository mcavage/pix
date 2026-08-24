// uat_worker_lifecycle.go — U2 of the self-development UAT auth repair
// (docs/design/self-development-uat.md): `pix run --dev` starting and
// supervising `pix-host uat-worker` from the launcher's own process, so it
// inherits the operator's authenticated host context instead of the sbx
// gateway's unauthenticated ancestry. U1 (ce3f3433) landed the reusable
// socket/relay/planner primitives; this wires them into the create and
// attach lifecycles.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"pix/host/hostenv"
	"pix/host/uat"
)

// ensureUatWorkerOrFail starts (or adopts) the session's uat-worker before
// anything that could depend on it — a create's gateway relay, or a --dev
// attach — is allowed to proceed, and fails the whole launch closed if it
// cannot: this must never let a session claim dev UAT while the relay is
// dead. repoRoot is only required to start a REPLACEMENT worker; adopting an
// already-live one (uat.EnsureWorker's dial-first check) never touches it.
func ensureUatWorkerOrFail(env hostenv.Env, repoRoot, uatStateRoot string, rec *uat.Registration) error {
	runnerState := filepath.Join(uatStateRoot, "sessions", rec.SessionID)
	deps := uat.DefaultEnsureWorkerDeps()
	// This layer owns the process boundary (processboundary_test.go): the uat
	// package below it never names os.Stderr itself, so the worker's own
	// diagnostics are wired here rather than discarded.
	deps.Spawn = uat.WorkerSpawn(os.Stderr, os.Stderr)
	// Dial first, here too: a live worker is adopted without resolving
	// pix-host or anything else about starting one — the same rule
	// uat.EnsureWorker itself enforces, kept at this layer so a caller whose
	// HostBinary probe is expensive or fallible never pays for it on the
	// common attach-to-a-live-session path.
	if c, derr := deps.Dial(uat.SessionSocketPath(runnerState), 1, 0); derr == nil {
		_ = c.Close()
		return nil
	}
	hostBin, err := env.HostBinary()
	if err != nil {
		return fmt.Errorf("resolve pix-host for uat-worker: %w", err)
	}
	if _, err := uat.EnsureWorker(deps, hostBin, repoRoot, runnerState, rec.SessionID); err != nil {
		return fmt.Errorf("uat-worker: %w", err)
	}
	return nil
}
