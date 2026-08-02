package main

import (
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/workspace"
	"strings"
	"text/tabwriter"
)

// The `ls` and `rm` verbs manage the pix-* sandboxes that `run` and `task`
// create. `status` SHOWS them; these let you act on them without dropping to raw
// `sbx`. Both are scoped to pix-* names on purpose: this tool manages the
// sandboxes it made, not every sbx box on the host (use `sbx` directly for the
// rest).

// fatalSbx prints a sandbox-command error (correctly prefixed, unlike the
// agent-scoped fatalLauncher) and exits non-zero.
func fatalSbx(err error) {
	fmt.Fprintf(os.Stderr, "pix: %v\n", err)
	os.Exit(1)
}

// overlayReceiptDirs replaces best-effort sbx display data with Pix's trusted
// create receipt. The receipt records the canonical workspace passed to the
// successful create and is therefore authoritative when packs add other host
// paths to the sbx listing.
func overlayReceiptDirs(boxes []workspace.SbxBox, stateDir string) {
	for i := range boxes {
		receipt, status, err := workspace.ReadMCPReceipt(stateDir, boxes[i].Name)
		if err == nil && status == workspace.MCPStateOK && receipt != nil {
			boxes[i].Dir = receipt.Workspace
		}
	}
}

// runLs lists the pix sandboxes on this host.
func runLs(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(lsUsage)
		return
	}
	// hasJSONFlag rather than a shared helper: `ls` is the last verb still
	// parsing argv by hand, and a one-line scan beats keeping a generic
	// arg-parsing kit alive for it. It goes when `ls` migrates.
	jsonOut := false
	for _, a := range argv {
		if a == "--json" {
			jsonOut = true
		}
	}
	env := defaultShellEnv()
	if _, err := env.LookPath("sbx"); err != nil {
		fatalSbx(fmt.Errorf("sbx not found on PATH; install the Docker Sandboxes CLI to list sandboxes"))
	}
	// BOUNDED (probeRun): a hung `sbx ls` fails with a message, never wedges.
	out, timedOut, err := env.RunTimed("sbx", "ls")
	if timedOut || err != nil {
		fatalSbx(fmt.Errorf("sbx ls failed: %v", err))
	}
	boxes := workspace.ParsePixBoxes(out)
	if stateDir, err := workspace.MCPStateDirFn(); err == nil {
		overlayReceiptDirs(boxes, stateDir)
	}
	if jsonOut {
		printJSONLauncher(boxes)
		return
	}
	if len(boxes) == 0 {
		fmt.Println("No pix sandboxes. Start one with `pix run`.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tDIR")
	for _, b := range boxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, b.State, b.Dir)
	}
	tw.Flush()
	fmt.Println()
	fmt.Println("Remove one:  pix rm <name>   (or `sbx rm -f <name>` for non-pix boxes)")
}

// runRm removes one or more pix sandboxes via `sbx rm -f`. It refuses names
// that are not pix-* (this tool manages its own boxes; use `sbx` for the
// rest) and `--all` removes every pix-* box, with `--except <name>` to keep
// one (e.g. the box you are in).
func runRm(argv []string) {
	if cli.WantsHelp(argv) || len(argv) == 0 {
		fmt.Print(rmUsage)
		if len(argv) == 0 {
			os.Exit(2)
		}
		return
	}
	env := defaultShellEnv()
	if _, err := env.LookPath("sbx"); err != nil {
		fatalSbx(fmt.Errorf("sbx not found on PATH; install the Docker Sandboxes CLI to remove sandboxes"))
	}

	var names, keep []string
	all := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--all":
			all = true
		case a == "--except":
			if i+1 >= len(argv) {
				fatalSbx(fmt.Errorf("--except needs a name"))
			}
			i++
			keep = append(keep, argv[i])
		case strings.HasPrefix(a, "-"):
			fatalSbx(fmt.Errorf("unknown flag %q\n\n%s", a, rmUsage))
		default:
			names = append(names, a)
		}
	}

	if all {
		// BOUNDED (probeRun): the --all discovery listing is read-only; a hung
		// sbx fails with a message rather than wedging (the `sbx rm -f` calls
		// below are mutating lifecycle commands and stay on env.Run).
		out, timedOut, err := env.RunTimed("sbx", "ls")
		if timedOut || err != nil {
			fatalSbx(fmt.Errorf("sbx ls failed: %v", err))
		}
		keepSet := map[string]bool{}
		for _, k := range keep {
			keepSet[k] = true
		}
		for _, b := range workspace.ParsePixBoxes(out) {
			if !keepSet[b.Name] {
				names = append(names, b.Name)
			}
		}
		if len(names) == 0 {
			fmt.Println("No pix sandboxes to remove.")
			return
		}
	}

	rc := 0
	for _, n := range names {
		if !strings.HasPrefix(n, "pix-") {
			fmt.Fprintf(os.Stderr, "refusing %q: not a pix sandbox (use `sbx rm -f %s` for that)\n", n, n)
			rc = 1
			continue
		}
		if err := removePixSandbox(env, n); err != nil {
			fmt.Fprintf(os.Stderr, "failed to remove %s: %v\n", n, err)
			rc = 1
			continue
		}
		fmt.Printf("removed %s\n", n)
	}
	if rc != 0 {
		os.Exit(rc)
	}
}

// removePixSandbox force-removes name via env and, on SUCCESS, clears the
// launcher's per-sandbox MCP receipt — a removed sandbox's receipt describes
// a dead lifetime. A failed rm returns the error and RETAINS the receipt: an
// unknown removal outcome must keep the evidence, never discard it on a
// guess. The receipt clear itself is best-effort (warn, don't fail the rm —
// the removal DID succeed, and the next launcher create's pre-create clear is
// the correctness backstop).
func removePixSandbox(env hostenv.Env, name string) error {
	if _, err := env.Run("sbx", "rm", "-f", name); err != nil {
		return err
	}
	if err := workspace.ClearRemovedReceipt(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removed %s but could not clear its mcp receipt: %v\n", name, err)
	}
	return nil
}

const lsUsage = `usage: pix ls [--json]

List the pix sandboxes on this host (name, state, ws dir). These are
the boxes ` + "`pix run`" + ` and ` + "`pix task`" + ` create. For every sbx sandbox
(not just pix's), use ` + "`sbx ls`" + `.
`

const rmUsage = `usage: pix rm <name>... [--all] [--except <name>]

Remove pix sandboxes (via ` + "`sbx rm -f`" + `). Scoped to pix-* names; use
` + "`sbx rm`" + ` for other boxes.

  pix rm pix-tact              remove one
  pix rm pix-a pix-b      remove several
  pix rm --all --except pix-pix   remove all but one
`
