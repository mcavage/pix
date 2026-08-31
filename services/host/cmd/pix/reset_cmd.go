// reset_cmd.go — the `pix reset` command struct. Flags are declared once here
// and parsed by the root; what is moved aside, in what order, and what refuses
// without a confirmation is workflow/reset's.
//
// The one thing this layer OWNS is the sandbox sweep it injects: reset is L3
// and workflow/launch is its L3 sibling, so only the command layer may hold
// both. It passes `pix rm --all` itself rather than a re-implementation, which
// is what makes "reset never force-removes a sandbox" a property of the code
// instead of a promise in a comment.
package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/workflow/launch"
	"pix/host/workflow/reset"
)

func (c *resetCmd) Help() string { return reset.Description }

// resetCmd is the v2 surface's whole `pix reset` flag set
// (docs/design/pix-v2-surface.md §3.8): --yes, the ONE noninteractive
// confirmation escape. M2 (security re-review): --keep-memory,
// --keep-sandboxes, and --force existed in an earlier draft, parsed, and
// were never read by ResetHome or anything else — three flags a user could
// pass and have silently ignored, each promising a behavior (skip the
// memory wipe, leave sandboxes alone, force past a stuck container) v2
// reset does not have and never wired. They are deleted, not merely
// undocumented: ResetHome's contract is unconditional (docs/design/
// pix-v2-architecture.md §12 — sandboxes, then the memory container proven
// absent, then PIX_HOME, every time), and a flag that parses but does
// nothing is worse than no flag, because it tells a user something false
// about what just happened.
type resetCmd struct {
	Yes bool `short:"y" help:"Skip the confirmation prompt (still required on a non-interactive terminal)."`
}

func (c *resetCmd) Run(d *cli.Deps) error {
	// pix reset is the v2 PIX_HOME clean slate (docs/design/
	// pix-v2-architecture.md §12, §16.14): sweep sandboxes, then stop+remove+
	// prove-absent the memory container, then rename PIX_HOME aside. It never
	// reads the (still-live, v1) workspace config — ResetHome does not need
	// to know which MCP registrations to drop; it only tears down what it
	// itself owns.
	if !c.Yes && !d.Interactive {
		return cli.Usagef("refusing to reset a non-interactive terminal without confirmation; re-run with --yes")
	}
	home, err := pixhome.Resolve()
	if err != nil {
		return err
	}
	// M2 (security re-review): an interactive reset must show EXACTLY what
	// is about to happen and default to No, the same posture `pix env
	// trust`/the run trust gate already hold for a host-mutating decision
	// — never a bare confirmation with no bill, and never a silent proceed
	// just because the terminal happens to be a TTY. --yes (only) skips
	// this prompt; it never skips the operations themselves or their fixed
	// order, and a decline here mutates NOTHING (runs before ResetHome is
	// ever called).
	if !c.Yes {
		renderResetBill(d.Out, home.Home)
		fmt.Fprint(d.Out, "Proceed? [y/N] ")
		reader := bufio.NewReader(d.In)
		line, _ := reader.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(line), "y") {
			fmt.Fprintln(d.Out, "pix: not confirmed; nothing was changed.")
			return cli.SilentError{Code: 1}
		}
	}
	res, err := reset.ResetHome(reset.HomeDeps{
		Home:   home.Home,
		Sweep:  rmAllSandboxes(d),
		Out:    d.Out,
		ErrOut: d.Err,
		Now:    time.Now,
	})
	if err != nil {
		return err
	}
	if res.BackupPath != "" {
		fmt.Fprintf(d.Out, "pix: PIX_HOME moved aside to %s\n", res.BackupPath)
	} else {
		fmt.Fprintln(d.Out, "pix: nothing to reset (PIX_HOME did not exist).")
	}
	return nil
}

// renderResetBill prints the EXACT, fixed operations ResetHome performs, in
// the order it performs them — the bill a confirming "y" is answering for.
// It never varies by flag (there are none left to vary it): every
// interactive reset sees the same three lines.
func renderResetBill(out io.Writer, home string) {
	fmt.Fprintln(out, "pix reset will:")
	fmt.Fprintln(out, "  1. remove every pix-* sandbox (the same proof-gated path as `pix rm --all`)")
	fmt.Fprintf(out, "  2. stop and remove the %q container\n", container.Name)
	fmt.Fprintf(out, "  3. rename %s aside to a timestamped backup (nothing is deleted)\n\n", home)
}

// rmAllSandboxes is `pix rm --all` as a callable: the SAME non-forced,
// zero-reference-proof teardown the verb runs, with Force unset (launch.Rm
// refuses --force with --all anyway, so this cannot drift into a bulk force
// seam) and no keep set, since reset is explicitly asking for all of them.
func rmAllSandboxes(d *cli.Deps) reset.Sweep {
	return func(out, errOut io.Writer) error {
		return sbxAwareFail(d, launch.Rm(defaultShellEnv(), out, errOut, launch.RmOptions{
			All: true, Interactive: d.Interactive,
		}))
	}
}
