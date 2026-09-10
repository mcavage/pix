package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"pix/host/cli"
	"pix/host/sys"
	nativeenv "pix/host/workflow/env"
)

type envUpgradeCmd struct {
	Name    string `arg:"" help:"Exact environment name."`
	Verbose bool   `help:"Show setup technical details and diagnostic output."`
}

func (c *envUpgradeCmd) Run(d *cli.Deps) error {
	return c.run(d, func(s *setupCmd, d *cli.Deps) error { return s.Run(d) })
}

// run shares the production setup caller; tests can replace its external effects.
func (c *envUpgradeCmd) run(d *cli.Deps, setup func(*setupCmd, *cli.Deps) error) error {
	home, err := envHome()
	if err != nil {
		return err
	}
	sel, err := nativeenv.ResolveIn(home, c.Name)
	if err != nil {
		return err
	}
	git := func(args ...string) (string, error) {
		// Fetching an environment must not execute newly pulled Git hooks.
		argv := append([]string{"-C", sel.Root, "-c", "core.hooksPath=/dev/null"}, args...)
		cmd := exec.Command("git", argv...)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	top, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("environment %q is not a Git checkout; update its files manually, then run pix setup --env %s", sel.Name, sel.Name)
	}
	top, err = filepath.EvalSymlinks(top)
	if err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(sel.Root)
	if err != nil {
		return err
	}
	if top != root {
		return fmt.Errorf("environment %q is inside a larger Git checkout; update that checkout manually, then run pix setup --env %s", sel.Name, sel.Name)
	}
	status, err := git("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("cannot check Git status for environment %q", sel.Name)
	}
	if status != "" {
		return fmt.Errorf("environment %q has local changes; commit or stash them before running pix env upgrade %s", sel.Name, sel.Name)
	}
	if _, err := git("symbolic-ref", "--quiet", "HEAD"); err != nil {
		return fmt.Errorf("environment %q has a detached HEAD; switch to a tracking branch before upgrading", sel.Name)
	}
	if _, err := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err != nil {
		return fmt.Errorf("environment %q has no upstream branch; configure a Git tracking branch before upgrading", sel.Name)
	}
	fmt.Fprintf(d.Out, "Updating environment %q…\n", sel.Name)
	if _, err := git("pull", "--ff-only", "--no-rebase", "--no-autostash", "--no-recurse-submodules"); err != nil {
		return fmt.Errorf("could not fast-forward environment %q; check its remote access and branch history with git -C %s status and git -C %s pull --ff-only; setup was not run", sel.Name, sys.ShellQuote(sel.Root), sys.ShellQuote(sel.Root))
	}
	fmt.Fprintln(d.Out, "Environment checkout updated. Running setup…")
	if err := setup(&setupCmd{Env: sel.Name, Verbose: c.Verbose}, d); err != nil {
		fmt.Fprintf(d.Err, "The checkout was updated; setup did not finish. Retry with pix setup --env %s.\n", sel.Name)
		return err
	}
	fmt.Fprintf(d.Out, "Environment %q upgraded. Restart your session with pix run --env %s.\n", sel.Name, sel.Name)
	return nil
}
