package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// The `ls` and `rm` verbs manage the pi-stack-* sandboxes that `run` and `task`
// create. `status` SHOWS them; these let you act on them without dropping to raw
// `sbx`. Both are scoped to pi-stack-* names on purpose: this tool manages the
// sandboxes it made, not every sbx box on the host (use `sbx` directly for the
// rest).

// fatalSbx prints a sandbox-command error (correctly prefixed, unlike the
// agent-scoped fatalLauncher) and exits non-zero.
func fatalSbx(err error) {
	fmt.Fprintf(os.Stderr, "pi-stack: %v\n", err)
	os.Exit(1)
}

// sbxBox is one parsed `sbx ls` row for a pi-stack sandbox.
type sbxBox struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Dir   string `json:"dir,omitempty"`
}

// knownSbxStates are the sbx lifecycle words we recognize when parsing a row,
// so we can pick the state column out regardless of sbx's exact layout.
var knownSbxStates = map[string]bool{
	"running": true, "stopped": true, "exited": true,
	"created": true, "paused": true, "restarting": true, "dead": true,
}

// parsePiStackBoxes filters `sbx ls` output to pi-stack-* rows and pulls name,
// state, and (best-effort) the workspace dir. It tolerates column drift: the
// state is whichever field is a known lifecycle word, and the dir is the last
// absolute-path-looking field.
func parsePiStackBoxes(sbxLsOut string) []sbxBox {
	var out []sbxBox
	for _, ln := range strings.Split(sbxLsOut, "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 1 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, "pi-stack-") {
			continue
		}
		b := sbxBox{Name: name}
		for _, f := range fields[1:] {
			switch {
			case knownSbxStates[strings.ToLower(f)]:
				b.State = strings.ToLower(f)
			case strings.HasPrefix(f, "/"):
				b.Dir = f
			}
		}
		if b.State == "" {
			b.State = "?"
		}
		out = append(out, b)
	}
	return out
}

// runLs lists the pi-stack sandboxes on this host.
func runLs(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(lsUsage)
		return
	}
	jsonOut := hasFlagLauncher(argv, "--json")
	env := defaultShellEnv()
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err != nil {
			fatalSbx(fmt.Errorf("sbx not found on PATH; install the Docker Sandboxes CLI to list sandboxes"))
		}
	}
	// BOUNDED (probeRun): a hung `sbx ls` fails with a message, never wedges.
	out, timedOut, err := probeRun(env, "sbx", "ls")
	if timedOut || err != nil {
		fatalSbx(fmt.Errorf("sbx ls failed: %v", err))
	}
	boxes := parsePiStackBoxes(out)
	if jsonOut {
		printJSONLauncher(boxes)
		return
	}
	if len(boxes) == 0 {
		fmt.Println("No pi-stack sandboxes. Start one with `pi-stack run`.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tDIR")
	for _, b := range boxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, b.State, b.Dir)
	}
	tw.Flush()
	fmt.Println()
	fmt.Println("Remove one:  pi-stack rm <name>   (or `sbx rm -f <name>` for non-pi-stack boxes)")
}

// runRm removes one or more pi-stack sandboxes via `sbx rm -f`. It refuses names
// that are not pi-stack-* (this tool manages its own boxes; use `sbx` for the
// rest) and `--all` removes every pi-stack-* box, with `--except <name>` to keep
// one (e.g. the box you are in).
func runRm(argv []string) {
	if wantsHelp(argv) || len(argv) == 0 {
		fmt.Print(rmUsage)
		if len(argv) == 0 {
			os.Exit(2)
		}
		return
	}
	env := defaultShellEnv()
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err != nil {
			fatalSbx(fmt.Errorf("sbx not found on PATH; install the Docker Sandboxes CLI to remove sandboxes"))
		}
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
		// below are mutating lifecycle commands and stay on env.run).
		out, timedOut, err := probeRun(env, "sbx", "ls")
		if timedOut || err != nil {
			fatalSbx(fmt.Errorf("sbx ls failed: %v", err))
		}
		keepSet := map[string]bool{}
		for _, k := range keep {
			keepSet[k] = true
		}
		for _, b := range parsePiStackBoxes(out) {
			if !keepSet[b.Name] {
				names = append(names, b.Name)
			}
		}
		if len(names) == 0 {
			fmt.Println("No pi-stack sandboxes to remove.")
			return
		}
	}

	rc := 0
	for _, n := range names {
		if !strings.HasPrefix(n, "pi-stack-") {
			fmt.Fprintf(os.Stderr, "refusing %q: not a pi-stack sandbox (use `sbx rm -f %s` for that)\n", n, n)
			rc = 1
			continue
		}
		if err := removePiStackSandbox(env, n); err != nil {
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

// removePiStackSandbox force-removes name via env and, on SUCCESS, clears the
// launcher's per-sandbox MCP receipt — a removed sandbox's receipt describes
// a dead lifetime. A failed rm returns the error and RETAINS the receipt: an
// unknown removal outcome must keep the evidence, never discard it on a
// guess. The receipt clear itself is best-effort (warn, don't fail the rm —
// the removal DID succeed, and the next launcher create's pre-create clear is
// the correctness backstop).
func removePiStackSandbox(env shellEnv, name string) error {
	if _, err := env.run("sbx", "rm", "-f", name); err != nil {
		return err
	}
	if err := clearRemovedSandboxReceipt(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removed %s but could not clear its mcp receipt: %v\n", name, err)
	}
	return nil
}

const lsUsage = `usage: pi-stack ls [--json]

List the pi-stack sandboxes on this host (name, state, workspace dir). These are
the boxes ` + "`pi-stack run`" + ` and ` + "`pi-stack task`" + ` create. For every sbx sandbox
(not just pi-stack's), use ` + "`sbx ls`" + `.
`

const rmUsage = `usage: pi-stack rm <name>... [--all] [--except <name>]

Remove pi-stack sandboxes (via ` + "`sbx rm -f`" + `). Scoped to pi-stack-* names; use
` + "`sbx rm`" + ` for other boxes.

  pi-stack rm pi-stack-tact              remove one
  pi-stack rm pi-stack-a pi-stack-b      remove several
  pi-stack rm --all --except pi-stack-pi-stack   remove all but one
`
