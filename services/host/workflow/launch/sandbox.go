package launch

import (
	"encoding/json"
	"fmt"
	"io"
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

// OverlayReceiptDirs replaces best-effort sbx display data with Pix's trusted
// create receipt. The receipt records the canonical workspace passed to the
// successful create and is therefore authoritative when packs add other host
// paths to the sbx listing.
func OverlayReceiptDirs(boxes []workspace.SbxBox, stateDir string) {
	for i := range boxes {
		receipt, status, err := workspace.ReadMCPReceipt(stateDir, boxes[i].Name)
		if err == nil && status == workspace.MCPStateOK && receipt != nil {
			boxes[i].Dir = receipt.Workspace
		}
	}
}

// Ls lists the pix sandboxes on this host. It returns an error rather than
// exiting: the exit code is the root's one mapper, not this function's.
func Ls(env hostenv.Env, out io.Writer, jsonOut bool) error {
	if _, err := env.LookPath("sbx"); err != nil {
		return fmt.Errorf("sbx not found on PATH; install the Docker Sandboxes CLI to list sandboxes")
	}
	// BOUNDED (probeRun): a hung `sbx ls` fails with a message, never wedges.
	raw, timedOut, err := env.RunTimed("sbx", "ls")
	if timedOut || err != nil {
		return fmt.Errorf("sbx ls failed: %v", err)
	}
	boxes := workspace.ParsePixBoxes(raw)
	if stateDir, err := workspace.MCPStateDirFn(); err == nil {
		OverlayReceiptDirs(boxes, stateDir)
	}
	if jsonOut {
		b, err := json.MarshalIndent(boxes, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(b))
		return nil
	}
	if len(boxes) == 0 {
		fmt.Fprintln(out, "No pix sandboxes. Start one with `pix run`.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tDIR")
	for _, b := range boxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, b.State, b.Dir)
	}
	tw.Flush()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Remove one:  pix rm <name>   (or `sbx rm -f <name>` for non-pix boxes)")
	return nil
}

// RmOptions is the already-parsed `pix rm` invocation. Parsing is the root
// parser's job; deciding what a removal MEANS is this package's.
type RmOptions struct {
	Names  []string
	All    bool
	Except []string
}

// Rm removes one or more pix sandboxes via `sbx rm -f`. It refuses names that
// are not pix-* (this tool manages its own boxes; use `sbx` for the rest), and
// All removes every pix-* box, with Except keeping one (e.g. the box you are
// in). A per-name failure is reported as it happens and summarised as exit 1
// through a SilentError, because each cause was already named.
func Rm(env hostenv.Env, out, errOut io.Writer, opts RmOptions) error {
	if _, err := env.LookPath("sbx"); err != nil {
		return fmt.Errorf("sbx not found on PATH; install the Docker Sandboxes CLI to remove sandboxes")
	}
	names := append([]string(nil), opts.Names...)
	if opts.All {
		// BOUNDED (probeRun): the --all discovery listing is read-only; a hung
		// sbx fails with a message rather than wedging (the `sbx rm -f` calls
		// below are mutating lifecycle commands and stay on env.Run).
		raw, timedOut, err := env.RunTimed("sbx", "ls")
		if timedOut || err != nil {
			return fmt.Errorf("sbx ls failed: %v", err)
		}
		keep := map[string]bool{}
		for _, k := range opts.Except {
			keep[k] = true
		}
		for _, b := range workspace.ParsePixBoxes(raw) {
			if !keep[b.Name] {
				names = append(names, b.Name)
			}
		}
		if len(names) == 0 {
			fmt.Fprintln(out, "No pix sandboxes to remove.")
			return nil
		}
	}

	failed := false
	for _, n := range names {
		if !strings.HasPrefix(n, "pix-") {
			fmt.Fprintf(errOut, "refusing %q: not a pix sandbox (use `sbx rm -f %s` for that)\n", n, n)
			failed = true
			continue
		}
		if err := RemovePixSandbox(env, n); err != nil {
			fmt.Fprintf(errOut, "failed to remove %s: %v\n", n, err)
			failed = true
			continue
		}
		fmt.Fprintf(out, "removed %s\n", n)
	}
	if failed {
		return cli.SilentError{Code: 1}
	}
	return nil
}

// RemovePixSandbox force-removes name via env and, on SUCCESS, clears the
// launcher's per-sandbox MCP receipt — a removed sandbox's receipt describes
// a dead lifetime. A failed rm returns the error and RETAINS the receipt: an
// unknown removal outcome must keep the evidence, never discard it on a
// guess. The receipt clear itself is best-effort (warn, don't fail the rm —
// the removal DID succeed, and the next launcher create's pre-create clear is
// the correctness backstop).
func RemovePixSandbox(env hostenv.Env, name string) error {
	if _, err := env.Run("sbx", "rm", "-f", name); err != nil {
		return err
	}
	if err := workspace.ClearRemovedReceipt(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removed %s but could not clear its mcp receipt: %v\n", name, err)
	}
	return nil
}

// LsDescription and RmDescription are the long help the root's generated usage
// prints. They live with the behaviour they describe, so a change to one shows
// up in the other's diff; they replaced two hand-written usage constants.
const LsDescription = `List the pix sandboxes on this host (name, state, ws dir). These are the
boxes 'pix run' and 'pix task' create. For every sbx sandbox (not just pix's),
use 'sbx ls'.`

const RmDescription = `Remove pix sandboxes (via 'sbx rm -f'). Scoped to pix-* names; use 'sbx rm'
for other boxes.

  pix rm pix-tact                  remove one
  pix rm pix-a pix-b               remove several
  pix rm --all --except pix-pix    remove all but one`
