// upgrade_auto.go — `pix run`'s automatic post-upgrade reconcile.
//
// Upgrading the package (`brew upgrade pix`, a new local bundle) replaces
// the binary and the release bundle beside it, but leaves this PIX_HOME
// still holding the PREVIOUS release's runtime tree, images, memory
// container and MCP registration. Before this file, the only thing that
// reconciled those was `pix setup`, so an ordinary `pix` after every
// upgrade either ran against stale artifacts or made the user re-run setup
// by hand. That is a chore with no decision in it: every artifact this
// reconcile touches is MACHINE-OWNED and stack-scoped, and the release
// bundle beside the binary is the authority on what it should be.
//
// The rules, all of them fail-closed:
//
//   - No installed manifest at all is FIRST RUN, not an upgrade. Nothing is
//     provisioned implicitly; `pix setup` stays the explicit bootstrap.
//   - An exact manifest match is the steady state: one file read, no Docker,
//     no Gateway, no output.
//   - A mismatch runs ONLY machineSetup (setup_cmd.go): runtime install,
//     release record, digest-pinned images, default-env ensure, this stack's
//     memory container, this stack's scoped memory MCP name. It never
//     solicits or writes a credential, never accepts environment trust, and
//     never executes a `[[setup]]` hook.
//   - The image/config drift confirmation is auto-answered YES, but only
//     ever reaches a container container.Reconcile has ALREADY proven this
//     stack owns: a foreign or missing stack-ownership label refuses inside
//     Reconcile before ConfirmReplace is consulted at all (container.go,
//     ActionRefusedForeignStack).
//   - A failure AFTER the manifest was recorded restores the previous
//     installed manifest, so the next run retries the same reconcile rather
//     than believing a half-applied upgrade landed. The newly installed
//     runtime tree and data are left alone: they are content-addressed by
//     version and re-installing over them is idempotent.
//
// A changed pix-memory MCP endpoint under this home's reserved name is
// still the existing fail-closed manual removal: provision's registrar
// returns an error rather than overwriting a registration no receipt proves
// we own, and this reconcile propagates it (and rolls the manifest back).
package main

import (
	"fmt"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/release"
)

// autoUpgradeSeamsFor is the seam factory `pix run` uses. Production is
// productionSetupSeams; a test swaps it to prove the CALLER actually
// invokes this reconcile (not merely that the function works when called).
var autoUpgradeSeamsFor = productionSetupSeams

// autoReconcileRelease is `pix run`'s pre-launch reconcile. It returns an
// error only when a real upgrade was attempted and failed; every "we cannot
// tell" answer (no bundle beside the binary, an unreadable install record)
// leaves the launch exactly as it was, because a launcher that cannot find
// its own bundle is a development build, not a broken installation.
func autoReconcileRelease(d *cli.Deps, s setupSeams) error {
	home, err := pixhome.Resolve()
	if err != nil {
		return nil
	}
	installed, err := release.LoadInstalled(home.Home)
	if err != nil || installed == nil {
		// No record: first run. Preserve the explicit bootstrap — an
		// unprovisioned host is `pix setup`'s job, and silently doing it
		// from a launch would provision a machine nobody asked to provision.
		return nil
	}
	bundle, err := s.DiscoverBundle()
	if err != nil || bundle == nil {
		return nil
	}
	if bundle.Manifest == *installed {
		// The steady state, and the whole cost of this feature on a
		// machine that has not upgraded: one manifest read and a compare.
		return nil
	}
	if verr := bundle.VerifyArchive(); verr != nil {
		// A drifted binary whose own bundle does not verify is exactly the
		// case NOT to auto-apply. Say so once and let the launch continue
		// against what is installed.
		fmt.Fprintf(d.Err, "pix: this build ships release %s but its bundle failed verification (%v); not reconciling automatically. Run `pix setup` when you have looked at it.\n",
			bundle.Manifest.Version, verr)
		return nil
	}

	previous := *installed
	res, serr := machineSetup(home, s, *bundle, autoConfirmOwnedReplace)
	if serr != nil {
		restoreInstalledManifest(d, home, previous)
		return fmt.Errorf("pix: automatic upgrade to %s failed: %w\npix: nothing else was changed; the next run retries it, or run `pix setup` yourself", bundle.Manifest.Version, serr)
	}
	if !res.Ready() {
		restoreInstalledManifest(d, home, previous)
		return fmt.Errorf("pix: automatic upgrade to %s did not reach a verified state (pix-memory container %s)\npix: run `pix doctor` for the exact gap; the next run retries the upgrade",
			bundle.Manifest.Version, res.Container.Action)
	}
	fmt.Fprintf(d.Err, "pix: upgraded to %s (kit %s, pix-memory %s)\n", bundle.Manifest.Version, res.KitRevision, res.Container.Action)
	return nil
}

// autoConfirmOwnedReplace answers the pix-memory replace confirmation YES.
// It is only ever REACHED for a container container.Reconcile has already
// proven carries THIS stack's ownership label: a foreign or missing owner
// returns ActionRefusedForeignStack without consulting a confirmer at all
// (container.go). So this is not "replace whatever is in the way", it is
// "an image or config drift on our own container, on the upgrade path that
// exists to fix exactly that".
func autoConfirmOwnedReplace(current container.Info, want container.Spec) bool { return true }

// restoreInstalledManifest puts the PREVIOUS release record back after a
// failed upgrade, so the next run sees the same mismatch and retries rather
// than treating a half-applied upgrade as done. The newly installed runtime
// tree and PIX_HOME data are deliberately NOT removed: they are versioned
// content, re-installing over them is idempotent, and deleting data on a
// failure path is how a retry turns into a loss.
func restoreInstalledManifest(d *cli.Deps, home pixhome.Paths, previous release.Manifest) {
	if err := release.SaveInstalled(home.Home, previous); err != nil {
		fmt.Fprintf(d.Err, "pix: warning: could not restore the previous release record (%s): %v; run `pix setup`\n", previous.Version, err)
	}
}
