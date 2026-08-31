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
// M1 (security re-review, TOCTOU): the ORIGINAL two-call shape resolved the
// environment from disk AGAIN on every call (name in, a fresh
// nativeenv.ResolveIn + LoadHome out), so the two checks — one right after
// selection, one immediately before the actual `sbx env create`/`sbx exec`
// mutation — could each observe a DIFFERENT on-disk environment. An
// attacker able to write the environment directory between the two calls
// (a symlink repoint, a concurrent edit under ~/.pix/envs) could present a
// malicious `.sbxenv.yaml` at the moment runLaunchAttempt actually COMPILES
// the effective document and resolves it (T0), then swap in a byte-for-byte
// BENIGN, already-trusted environment before the second gate re-reads disk
// (T1) — the second call would see only the benign T1 content, find it
// trusted, and wave the T0-compiled, already-malicious effective document
// through to `sbx env create`.
//
// The fix: resolveEnvTrustSnapshot reads the environment from disk EXACTLY
// ONCE per launch attempt (folded into resolveRunEnvironment, run_env.go),
// producing an envTrustSnapshot whose bom/fingerprint are bound to the
// SAME in-memory *nativeenv.Environment (bomForLoaded) that
// runEffectiveInput/RenderEffectiveEnvironment compiles the actual launch
// from — never re-derived from a second LoadHome. Both gate calls take
// that ONE snapshot. The second call's checkDrift re-reads the environment
// directory (the one remaining legitimate disk read: proving nothing
// changed between resolution and mutation), but it exists ONLY to compare
// that fresh read's digest against the SNAPSHOT's own fingerprint — a
// mismatch refuses unconditionally, regardless of whether the fresh
// content would itself be independently trusted. The fresh read's own
// trust conclusion is never consulted; only its identity against the
// snapshot is.
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

// envTrustSnapshot is the exact in-memory environment state a launch
// resolved ONCE (resolveEnvTrustSnapshot, called from resolveRunEnvironment
// before anything else reads the environment). Every trust decision this
// launch attempt makes binds to snap.fingerprint — never a fresh,
// independently re-derived one — so a benign swap-in after this point
// cannot launder an already-captured malicious snapshot into `sbx env
// create`, and a malicious swap-in after an accepted benign snapshot
// cannot launch under that acceptance either. A zero value (sel.Name == "")
// means D17 `none`: nothing was selected, so every gate call on it is a
// no-op.
type envTrustSnapshot struct {
	home        pixhome.Paths
	sel         nativeenv.Selected
	bom         nativeenv.BillOfMaterials
	fingerprint string
}

// resolveEnvTrustSnapshot loads name's CURRENT bill of materials and
// fingerprint from ITS OWN already-resolved *nativeenv.Environment
// (loaded) — the SAME value resolveRunEnvironment hands to
// runEffectiveInput to compile this launch's actual effective document.
// This is the ONE disk read gateEnvTrust's decisions are bound to; nothing
// past this point re-derives the fingerprint independently.
func resolveEnvTrustSnapshot(home pixhome.Paths, sel nativeenv.Selected, loaded *nativeenv.Environment) (envTrustSnapshot, error) {
	if sel.Name == "" {
		return envTrustSnapshot{}, nil
	}
	bom, fp, err := bomForLoaded(loaded)
	if err != nil {
		return envTrustSnapshot{}, err
	}
	return envTrustSnapshot{home: home, sel: sel, bom: bom, fingerprint: fp}, nil
}

// gateEnvTrust is the whole fail-closed decision, bound to snap — the ONE
// in-memory selection this launch attempt resolved. checkDrift is true
// only for the SECOND, immediately-pre-mutation call: it re-reads the
// environment directory fresh and refuses outright if that fresh
// fingerprint no longer matches snap.fingerprint (identity/digest compare
// against the snapshot), rather than trusting whatever the fresh read
// happens to say about ITSELF.
func gateEnvTrust(d *cli.Deps, snap envTrustSnapshot, checkDrift bool) error {
	if snap.sel.Name == "" {
		return nil
	}
	name := snap.sel.Name
	if checkDrift {
		_, freshFP, err := environmentBoM(snap.sel)
		if err != nil {
			return fmt.Errorf("environment %q: could not verify it is unchanged since being resolved for this launch: %w", name, err)
		}
		if freshFP != snap.fingerprint {
			return fmt.Errorf(
				"environment %q changed on disk after this launch resolved it (fingerprint mismatch); refusing rather than re-evaluating the new content mid-launch; re-run `pix run`",
				name)
		}
	}
	// Zero host footprint (BillOfMaterials.Tier1 false) needs no acceptance
	// at all: nothing runs on this host, no credential is handed out, and no
	// mount is expanded, so there is no prompt and no trust-state write. The
	// drift check above still ran, so a file changed after resolution is
	// refused before this point rather than waved through as "nothing to
	// review" — the recheck reads the environment as it is NOW.
	if trustSatisfied(snap.home, snap.sel, snap.bom, snap.fingerprint) {
		return nil
	}
	if !d.Interactive {
		return fmt.Errorf(
			"refusing to run unreviewed environment %q on a non-interactive terminal; review and accept it first: pix env trust %s",
			name, name)
	}

	fmt.Fprintln(d.Err, "pix run: this environment has not been reviewed.")
	renderTrustBill(d.Err, name, snap.bom, false)
	fmt.Fprintf(d.Err, "  fingerprint: %s\n\n", snap.fingerprint)
	fmt.Fprint(d.Err, "Accept this host-execution footprint? [y/N] ")
	reader := bufio.NewReader(d.In)
	line, _ := reader.ReadString('\n')
	if !strings.EqualFold(strings.TrimSpace(line), "y") {
		return fmt.Errorf("not accepted; run `pix env trust %s` when ready, or launch with a different --env", name)
	}

	if err := os.MkdirAll(snap.home.StateTrustEnvironments, 0o700); err != nil {
		return err
	}
	rec := envTrustRecord{Root: snap.sel.Root, Fingerprint: snap.fingerprint, AcceptedAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(trustRecordPath(snap.home, name), b, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(d.Err, "pix run: environment %q trusted.\n", name)
	return nil
}

// runTrustGate wraps gateEnvTrust in run's own fail-closed exit shape: a
// SilentError so the root exit-code mapper never re-prefixes or re-renders
// an already-complete message.
func runTrustGate(d *cli.Deps, snap envTrustSnapshot, checkDrift bool) error {
	if terr := gateEnvTrust(d, snap, checkDrift); terr != nil {
		fmt.Fprintln(d.Err, "pix run: "+terr.Error())
		return cli.SilentError{Code: 1}
	}
	return nil
}
