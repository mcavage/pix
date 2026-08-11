package pack

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/packinfo"
	"pix/host/secret"
	"pix/host/service"
)

// RunPackSetup runs a pack's required setup contributions after adoption
// through the normal Tier-1 trust gate. Every step is resumable: its bounded
// check runs first, apply runs only when check fails, and the same check must
// pass afterward before Pix reports readiness.
func RunPackSetup(env hostenv.Env, out io.Writer, root string, requested []string, interactive bool) error {
	p, err := packinfo.LoadPack(root)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, id := range requested {
		wanted[strings.TrimSpace(id)] = true
	}
	known := map[string]bool{}
	for _, step := range p.Manifest.Setup {
		known[step.ID] = true
	}
	for id := range wanted {
		if id == "" || !known[id] {
			return fmt.Errorf("pack has no setup hook %q", id)
		}
	}
	snapshots, cleanup, err := snapshotAcceptedPackSetup(env, p, wanted)
	if err != nil {
		return err
	}
	defer cleanup()
	for _, step := range p.Manifest.Setup {
		if !step.Required && !wanted[step.ID] {
			continue
		}
		label := strings.TrimSpace(step.Description)
		if label == "" {
			label = step.ID
		}
		check := func() (bool, string, bool) {
			return packSetupCheck(env, snapshots[step.ID], step.CheckArgs), "", true
		}
		apply := func() error { return env.RunInteractive(snapshots[step.ID], step.ApplyArgs...) }
		if step.Declarative() {
			check = func() (bool, string, bool) { return checkRequires(env, step) }
			apply = func() error { return applySteps(env, out, step) }
		}

		ok, why, fixable := check()
		if ok {
			fmt.Fprintf(out, "  ✓ %s: ready\n", label)
			continue
		}
		// A requirement nothing can fix for you is reported the same way
		// whether or not this is an interactive run: the answer is identical,
		// and it is a command the user runs, not a prompt they answer.
		if !fixable || len(step.Apply) == 0 && step.Declarative() {
			return fmt.Errorf("pack setup %s: %s", step.ID, why)
		}
		// Only refuse non-interactively if a remediation actually NEEDS a
		// terminal. `exec` applies are bounded and answer to nobody — that is
		// the whole distinction between the two kinds — so refusing them under
		// --yes made the scripted path unable to complete a step it was
		// perfectly capable of completing. An executable hook is opaque, so it
		// keeps the conservative treatment.
		if !interactive && stepNeedsTerminal(step) {
			return fmt.Errorf("pack setup %s is not ready (%s) and needs interactive authorization; "+
				"re-run without --yes/--non-interactive", step.ID, why)
		}
		fmt.Fprintf(out, "\npack setup: %s\n", label)
		if why != "" {
			fmt.Fprintf(out, "  needed: %s\n", why)
		}
		if err := apply(); err != nil {
			return fmt.Errorf("pack setup %s failed: %w", step.ID, err)
		}
		if ok, after, _ := check(); !ok {
			return fmt.Errorf("pack setup %s ran, but its own check still fails (%s)", step.ID, after)
		}
		fmt.Fprintf(out, "  ✓ %s: verified\n", label)
	}
	return nil
}

// stepNeedsTerminal reports whether fixing this step requires a TTY: any
// `interactive` remediation, or an executable hook (whose behaviour pix cannot
// see, so it must assume the worst).
func stepNeedsTerminal(step packinfo.SetupStep) bool {
	if !step.Declarative() {
		return true
	}
	for _, a := range step.Apply {
		if a.Kind == "interactive" {
			return true
		}
	}
	return false
}

// checkRequires evaluates a declarative step's conditions in order, returning
// the FIRST unmet one, a plain description of it, and whether running the
// step's apply steps could plausibly fix it.
//
// Order is the pack's, and it matters: a `bin` check before a `probe` that runs
// that binary means a user missing the tool is told to install it rather than
// shown a confusing exec failure.
func checkRequires(env hostenv.Env, step packinfo.SetupStep) (ok bool, why string, fixable bool) {
	for _, r := range step.Require {
		switch r.Kind {
		case "bin":
			if _, err := env.LookPath(r.Name); err != nil {
				// NOT fixable, for the same reason an op-ref is not: installing
				// software is the user's decision, and pix must not run a
				// package manager on their behalf. It also failed badly — the
				// applies ran anyway and the first one invoked the very binary
				// that is missing, so a correct install hint was immediately
				// buried under a raw `executable file not found in $PATH`.
				return false, fmt.Sprintf("%s is not installed — %s", r.Name, r.Install), false
			}
		case "op-ref":
			if !secret.OpRefFilled(env, r.Env) {
				// NOT fixable by any apply, and saying otherwise wastes a user's
				// time. Pix cannot put a secret into someone's 1Password vault:
				// only they can, and then only they can name the reference. So
				// this reports the exact command and stops, instead of running
				// an unrelated remediation that cannot possibly help.
				return false, fmt.Sprintf("%s is not set — run: pix secret set %s op://<vault>/<item>/<field>", r.Env, r.Env), false
			}
		case "probe":
			if _, timedOut, err := env.RunTimed(r.Argv[0], r.Argv[1:]...); err != nil || timedOut {
				return false, fmt.Sprintf("`%s` does not pass", strings.Join(r.Argv, " ")), true
			}
		}
	}
	return true, "", false
}

// applySteps runs a declarative step's remediations in order. An interactive
// apply inherits the terminal because it may open a browser and hold a
// localhost callback; a bounded one must not, so it cannot hang setup.
func applySteps(env hostenv.Env, out io.Writer, step packinfo.SetupStep) error {
	for _, a := range step.Apply {
		if e := strings.TrimSpace(a.Explain); e != "" {
			fmt.Fprintf(out, "  %s\n", e)
		}
		switch a.Kind {
		case "interactive":
			if err := env.RunInteractive(a.Argv[0], a.Argv[1:]...); err != nil {
				return fmt.Errorf("`%s`: %w", strings.Join(a.Argv, " "), err)
			}
		default: // "exec"
			if _, timedOut, err := env.RunTimed(a.Argv[0], a.Argv[1:]...); err != nil || timedOut {
				if timedOut {
					return fmt.Errorf("`%s` timed out", strings.Join(a.Argv, " "))
				}
				return fmt.Errorf("`%s`: %w", strings.Join(a.Argv, " "), err)
			}
		}
	}
	return nil
}

// snapshotAcceptedPackSetup copies every selected executable into a private
// launcher-owned directory, fingerprints the host surface from the CAPTURED
// bytes and requires an exact accepted trust record — so check, apply and
// re-check all execute the same immutable snapshot path.
func snapshotAcceptedPackSetup(env hostenv.Env, p *packinfo.Info, wanted map[string]bool) (map[string]string, func(), error) {
	paths := map[string]string{}
	cleanup := func() {}
	if p == nil {
		return paths, cleanup, nil
	}
	// Only an EXECUTABLE step has bytes to snapshot. A declarative step is
	// data in the manifest, already covered by the fingerprint below, with no
	// file to copy and nothing to exec.
	allBytes := map[string][]byte{}
	for _, step := range p.Manifest.Setup {
		if step.Declarative() {
			continue
		}
		data, err := service.ReadFileNoSymlink(filepath.Join(p.Root, step.Path))
		if err != nil {
			return nil, cleanup, fmt.Errorf("setup hook %q could not be snapshotted safely: %w", step.ID, err)
		}
		allBytes[step.ID] = data
	}
	bom := ComputeHostBoM(p)
	fp, _, err := computeHostExecFingerprintWithSetup(p.Root, bom, allBytes)
	if err != nil {
		return nil, cleanup, err
	}
	if err := requireAcceptedFingerprint(p, fp, "setup hooks"); err != nil {
		return nil, cleanup, err
	}
	state, err := config.StateDir()
	if err != nil {
		return nil, cleanup, err
	}
	base := filepath.Join(state, "pack-setup-snapshots")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, cleanup, err
	}
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	for _, step := range p.Manifest.Setup {
		if step.Declarative() || (!step.Required && !wanted[step.ID]) {
			continue
		}
		path := filepath.Join(dir, step.ID)
		if err := os.WriteFile(path, allBytes[step.ID], 0o500); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		paths[step.ID] = path
	}
	return paths, cleanup, nil
}

// PlanPackSetupRequests assigns each optional --with id to the one pack that
// declares it, before any hook runs. Unknown and ambiguous ids fail without
// partially applying required hooks from an earlier pack.
func PlanPackSetupRequests(roots, requested []string) (map[string][]string, error) {
	plan := map[string][]string{}
	owners := map[string][]string{}
	seenRoots := map[string]bool{}
	for _, root := range roots {
		key := packinfo.CanonicalizePackRoot(root)
		if seenRoots[key] {
			continue
		}
		seenRoots[key] = true
		p, err := packinfo.LoadPack(root)
		if err != nil {
			return nil, err
		}
		for _, step := range p.Manifest.Setup {
			owners[step.ID] = append(owners[step.ID], root)
		}
	}
	for _, raw := range requested {
		id := strings.TrimSpace(raw)
		matches := owners[id]
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no active pack has setup hook %q", id)
		case 1:
			plan[matches[0]] = append(plan[matches[0]], id)
		default:
			return nil, fmt.Errorf("setup hook %q is declared by multiple active packs (%s); hook IDs must be unique across a composed stack", id, strings.Join(matches, ", "))
		}
	}
	return plan, nil
}

func packSetupCheck(env hostenv.Env, path string, args []string) bool {
	_, timedOut, err := env.RunTimed(path, args...)
	return !timedOut && err == nil
}
