// run_trust.go — the CRITICAL trust-boundary fix (security re-review):
// `pix run` used to compile the effective document, record the fingerprint,
// and hand it to `sbx env create` for a NEWLY SELECTED environment without
// ever checking whether that environment had been reviewed at all. Only the
// REATTACH path (DecideEnvAttach's Reviewed field) consulted trust — a
// first-ever `pix run --env NAME` (or a fresh machine default) against an
// attacker-authored `.sbxenv.yaml` ran its host commands and mounted its
// paths with zero review, on both an interactive and a non-interactive
// terminal.
//
// gateEnvTrust closes that gap, fail-closed:
//   - no environment selected (D17 `none`): always passes; there is nothing
//     authored to review.
//   - already trusted (the BOM fingerprint matches a prior acceptance):
//     passes silently — this is the ordinary steady-state case on every run
//     after the first.
//   - untrusted, non-interactive terminal: REFUSED outright. No bill is
//     printed (a script capturing stdout/stderr should not receive one),
//     and nothing is created or mutated.
//   - untrusted, interactive terminal (first use): prints the EXACT same
//     canonical bill of materials `pix env trust` itself prints, and
//     requires an explicit "y" — the default is NO, matching `pix env
//     trust`'s own posture. Accepting records the SAME acceptance file `pix
//     env trust` would, so this environment is trusted on every subsequent
//     run (interactive or not) until its BOM fingerprint changes again.
//
// runLaunchAttempt calls this TWICE: once immediately after the environment
// is resolved (fail fast, before any config load, provider-key probe, or
// sbx side effect), and again immediately before the actual sbx mutation
// (RunSession, which performs the real `sbx env create`/`sbx exec`) —
// recomputed fresh each time rather than trusting the first call's answer,
// closing the TOCTOU window where the environment directory changes (a
// symlink swap, a concurrent edit under ~/.pix/envs) between the two
// checks. The second call is cheap: an already-trusted environment (the
// overwhelmingly common case, including immediately after the first call
// accepted it) returns instantly with no re-prompt.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/pixhome"
	nativeenv "pix/host/workflow/env"
)

// gateEnvTrust is the whole fail-closed decision. name is the SELECTED
// environment's name ("" for D17 none). It never resolves --env itself —
// the caller already did that — so a name here is always the exact
// registered environment this launch is about to use.
func gateEnvTrust(d *cli.Deps, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	home, err := pixhome.Resolve()
	if err != nil {
		return err
	}
	sel, err := nativeenv.ResolveIn(home, name)
	if err != nil {
		return err
	}
	trusted, fp, bomErr := trustAccepted(home, sel)
	if bomErr != nil {
		return fmt.Errorf("environment %q: could not verify trust: %w", name, bomErr)
	}
	if trusted {
		return nil
	}
	if !d.Interactive {
		return fmt.Errorf(
			"refusing to run unreviewed environment %q on a non-interactive terminal; review and accept it first: pix env trust %s",
			name, name)
	}

	bom, fp2, err := environmentBoM(sel)
	if err != nil {
		return fmt.Errorf("environment %q: could not verify trust: %w", name, err)
	}
	// fp2 recomputes the SAME fingerprint trustAccepted just derived; both
	// calls read the identical environment state a moment apart, so they
	// agree barring a concurrent edit — in which case fp2 (freshest) is what
	// gets recorded below, never the earlier fp.
	fp = fp2

	fmt.Fprintln(d.Err, "pix run: this environment has not been reviewed.")
	renderTrustBill(d.Err, sel.Name, bom, false)
	fmt.Fprintf(d.Err, "  fingerprint: %s\n\n", fp)
	fmt.Fprint(d.Err, "Accept this host-execution footprint? [y/N] ")
	reader := bufio.NewReader(d.In)
	line, _ := reader.ReadString('\n')
	if !strings.EqualFold(strings.TrimSpace(line), "y") {
		return fmt.Errorf("not accepted; run `pix env trust %s` when ready, or launch with a different --env", name)
	}

	if err := os.MkdirAll(home.StateTrustEnvironments, 0o700); err != nil {
		return err
	}
	rec := envTrustRecord{Root: sel.Root, Fingerprint: fp, AcceptedAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(trustRecordPath(home, sel.Name), b, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(d.Err, "pix run: environment %q trusted.\n", sel.Name)
	return nil
}

// runTrustGate wraps gateEnvTrust in run's own fail-closed exit shape: a
// SilentError so the root exit-code mapper never re-prefixes or re-renders
// an already-complete message.
func runTrustGate(d *cli.Deps, name string) error {
	if terr := gateEnvTrust(d, name); terr != nil {
		fmt.Fprintln(d.Err, "pix run: "+terr.Error())
		return cli.SilentError{Code: 1}
	}
	return nil
}
