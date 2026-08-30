// root.go — the ONE kong root: the launcher's verb table as a typed tree, and
// the only thing that parses argv, dispatches a verb, or answers a help
// request. Every verb here is the v2 accepted surface
// (docs/design/pix-v2-surface.md §3): run (implicit and explicit), ls, rm,
// task new/ls/path/rm, env list/show/default/trust, secret
// list/set/rm/check, setup, doctor, reset, help, version. Nothing else is
// dispatchable: a removed verb (status, resume, config, mcp, models, agent,
// pack, serve, uat, and every compatibility alias) gets kong's ordinary
// unknown-command answer, not a retirement message.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"pix/host/cli"
	"pix/host/mcp"
	"pix/host/sys"
	"pix/host/workflow/launch"
)

// exitServiceDown is the distinct exit code a verb returns when a resource it
// shells out to (sbx) is unreachable, so scripts can tell "service down" (3)
// apart from a usage error (2) or a generic failure (1). It was rpc.ExitServiceDown
// before the Pix v2 cutover deleted the custom memory JSON-RPC package that
// constant lived in for no reason but proximity (AC-16).
const exitServiceDown = 3

// sbxAwareFail is the shared exit mapping for every launcher verb that shells
// to sbx directly (ls, rm): mcp.ErrSbxUnavailable prints in the verb's own
// words on stderr and exits rpc.ExitServiceDown (3), never the generic 1 a
// plain error would fall through to.
func sbxAwareFail(d *cli.Deps, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mcp.ErrSbxUnavailable) {
		fmt.Fprintf(d.Err, "pix: %v\n", err)
		return cli.SilentError{Code: exitServiceDown}
	}
	return err
}

// rootCmd is the verb table. Field ORDER and the `group:` keys ARE the tiered
// help: helpAll renders the listing straight off this declaration, so a verb
// cannot be dispatchable and undocumented at once.
type rootCmd struct {
	Run runCmd `cmd:"" group:"Workflow" help:"Launch the sandbox in DIR (default: .). This is the main one."`
	Ls  lsCmd  `cmd:"" group:"Workflow" help:"List your pix sandboxes (name, state, dir)."`
	Rm  rmCmd  `cmd:"" group:"Workflow" help:"Remove pix sandboxes (scoped to pix-* names)."`

	Setup  setupCmd  `cmd:"" group:"Setup & health" help:"Guided setup: PIX_HOME, images, memory container."`
	Doctor doctorCmd `cmd:"" group:"Setup & health" help:"Diagnose problems and print the fix commands."`
	Reset  resetCmd  `cmd:"" group:"Setup & health" help:"Clean slate: remove sandboxes + memory container, back up PIX_HOME."`

	Task taskCmd `cmd:"" group:"Parallel work" help:"Parallel task checkouts of one repo: new | ls | path | rm."`

	Env    envCmd    `cmd:"" group:"Environments & credentials" help:"Named environments under ~/.pix/envs: list | show | default | trust."`
	Secret SecretCmd `cmd:"" group:"Environments & credentials" help:"1Password references: list | set | rm | check."`

	Version versionCmd `cmd:"" group:"Meta" help:"Print the stamped launcher version."`
	Help    helpCmd    `cmd:"" group:"Meta" passthrough:"" help:"Print this help (or a verb's usage)."`
}

// testSeams is the one indirection help_cmd.go's passthrough seam still
// checks. No verb routes through it anymore (serve install/uninstall, the
// last passthrough seams, were removed with v2's surface cut), but the
// field stays so help_cmd.go need not special-case "nothing is wired".
var testSeams struct {
	legacy func(verb string, args []string)
}

// versionCmd prints the stamped launcher version. Typed rather than
// passthrough: it has no flags, so its usage is entirely generated.
type versionCmd struct{}

func (c *versionCmd) Run(d *cli.Deps) error {
	fmt.Fprintln(d.Out, version)
	return nil
}

// ── dispatch ────────────────────────────────────────────────────────────────

// newRootDeps builds the Deps a real (non-test) dispatch runs against.
func newRootDeps() *cli.Deps {
	return &cli.Deps{
		Sys: sys.Real{}, Out: os.Stdout, Err: os.Stderr,
		In: os.Stdin, Interactive: cli.IsTTY(os.Stdin),
	}
}

// dispatch runs one argv against the root and returns the process exit code.
// It is the SINGLE exit mapper: 0 success, 1 failure, 2 a wrong invocation, and
// a SilentError's own code (3, readiness' unverifiable arm).
func dispatch(argv []string, d *cli.Deps) int {
	// The two hidden session-control invocation modes (sessionctl.go) are
	// intercepted BEFORE anything else touches argv: never normalized, never
	// handed to classifyBareArg, never parsed by kong, so no help/usage/
	// suggestion code path can ever render or dispatch them as an ordinary
	// verb. This is what "not listed in help" means structurally rather than
	// as a kong `hidden:""` tag someone could remove without noticing.
	if code, handled := runHiddenSessionVerb(argv, &cliDeps{Out: d.Out, Err: d.Err, In: d.In}); handled {
		return code
	}
	// `pix --dev` is the direct shorthand for an implicit dev launch. Preserve
	// the same TTY boundary as bare `pix`/`pix DIR`; scripts have the explicit
	// and auditable `pix run --dev` spelling.
	if len(argv) > 0 && argv[0] == "--dev" && !d.Interactive {
		fmt.Fprintln(d.Err, "pix: refusing an implicit --dev launch on a non-interactive terminal. Run it explicitly instead: pix run --dev")
		return 2
	}
	argv = normalizeArgv(argv)
	// A bare positional is `run DIR` when it names a directory, and a verb typo
	// otherwise. kong would call both "unexpected argument".
	if a := argv[0]; !strings.HasPrefix(a, "-") && !knownVerbs()[a] {
		msg, doLaunch := classifyBareArg(a)
		if !doLaunch {
			fmt.Fprint(d.Err, msg)
			return 2
		}
		// A BARE positional (never the explicit `run` verb) is an IMPLICIT
		// launch decision: on a non-interactive terminal nobody is there to
		// have meant it, so it REFUSES rather than creating or attaching a
		// sandbox from a script/pipe. stderr only, exit 2, and the root parser
		// never runs: no create, no attach, no side effect. An explicit
		// `pix run DIR` from the same shell is unaffected.
		if !d.Interactive {
			fmt.Fprintf(d.Err, bareNonTTYRefusalFmt, resolvedBareArgPath(a))
			return 2
		}
		// `pix DIR` IS `pix run DIR`.
		argv = normalizeArgv(append([]string{"run"}, argv...))
	}
	err := cli.RunRoot[rootCmd]("pix", "A personal, multi-model pi coding agent in a Docker sandbox.", helpText, argv, d)
	if err != nil {
		var silent cli.SilentError
		if !errors.As(err, &silent) {
			fmt.Fprintf(d.Err, "pix: %v\n", err)
		}
	}
	return cli.ExitCode(err)
}

// normalizeArgv rewrites the shapes the grammar cannot express: `pix task NAME
// path` -> `pix task path NAME`, so `cd "$(pix task foo path)"` reads as
// written, and run's `--` pi tail. A REWRITE, not a parse: the root still owns
// every decision downstream, and it never fires for a real subcommand.
func normalizeArgv(argv []string) []string {
	// Plain `pix` is implicit `pix run`; let its dev-mode spelling take the
	// same direct form instead of making `pix --dev` an unknown root flag.
	if len(argv) > 0 && argv[0] == "--dev" {
		argv = append([]string{"run"}, argv...)
	}
	if len(argv) == 3 && argv[0] == "task" && argv[2] == "path" && !isTaskKnownVerb(argv[1]) {
		return []string{"task", "path", argv[1]}
	}
	if argv[0] == "run" {
		return rewriteRunPassthrough(argv)
	}
	return argv
}

// isTaskKnownVerb guards the name-then-verb rewrite: it must never fire for a
// real subcommand or its aliases.
func isTaskKnownVerb(v string) bool {
	switch v {
	case "new", "ls", "list", "path", "rm", "remove":
		return true
	}
	return false
}

// ── lifecycle: ls / rm ──────────────────────────────────────────────────────

func (c *lsCmd) Help() string { return launch.LsDescription }

type lsCmd struct {
	JSON bool `help:"Emit machine-readable JSON."`
}

func (c *lsCmd) Run(d *cli.Deps) error {
	return sbxAwareFail(d, launch.Ls(defaultShellEnv(), d.Out, c.JSON))
}

func (c *rmCmd) Help() string { return launch.RmDescription }

type rmCmd struct {
	Names   []string `arg:"" optional:"" help:"Sandbox names to remove (pix-* only)."`
	All     bool     `help:"Remove every pix-* sandbox (never forced)."`
	Orphans bool     `help:"Remove only pix-owned sandboxes with zero live references and no keep (never forced)."`
	Force   bool     `short:"f" help:"Force-remove an explicitly named sandbox, skipping the zero-reference proof. Refused with --all/--orphans."`
	Keep    []string `short:"k" help:"With --all/--orphans: keep this one (repeatable)." placeholder:"NAME"`
}

func (c *rmCmd) Run(d *cli.Deps) error {
	return sbxAwareFail(d, launch.Rm(defaultShellEnv(), d.Out, d.Err, launch.RmOptions{
		Names: c.Names, All: c.All, Orphans: c.Orphans, Force: c.Force,
		Except:      c.Keep,
		Interactive: d.Interactive,
	}))
}
