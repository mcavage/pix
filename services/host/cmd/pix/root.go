// root.go — the ONE kong root: the launcher's verb table as a typed tree, and the
// only thing that parses argv, dispatches a verb, or answers a help request. The
// tree is the single source of truth for all three (what dispatches, what the
// suggester knows, what help says), because EVERY verb is TYPED: the struct tags
// that parse a verb also generate its help and place it in a help tier
// (`group:`). `help` is the one `passthrough:""` command, for the opposite
// reason: `pix help <anything>` is a question, not a grammar, so it must answer
// rather than reject.
//
// Three decisions stay in FRONT of the parser because they are argv SHAPE, not
// grammar: the retired table (retired.go), which must answer before any config
// read or side effect; a bare positional naming a directory, which is `run DIR`
// (classifyBareArg); and the `task NAME path` rewrite.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"pix/host/cli"
	"pix/host/mcp"
	"pix/host/rpc"
	"pix/host/service"
	"pix/host/sys"
	"pix/host/workflow/launch"
)

// sbxAwareFail is the shared exit mapping for every launcher verb that shells
// to sbx directly (ls, rm; mcp_cmd.go's mcpFailed is the same contract for the
// mcp group): mcp.ErrSbxUnavailable prints in the verb's own words on stderr
// and exits rpc.ExitServiceDown (3), the SAME code `pix mcp`/`memory`/`secret`
// already use for "dependency unavailable" — never the generic 1 a plain
// error would fall through to.
func sbxAwareFail(d *cli.Deps, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mcp.ErrSbxUnavailable) {
		fmt.Fprintf(d.Err, "pix: %v\n", err)
		return cli.SilentError{Code: rpc.ExitServiceDown}
	}
	return err
}

// rootCmd is the verb table. Field ORDER and the `group:` keys ARE the tiered
// help: helpAll renders the listing straight off this declaration, so a verb
// cannot be dispatchable and undocumented at once.
type rootCmd struct {
	Run    runCmd    `cmd:"" group:"Workflow" help:"Launch the sandbox in DIR (default: .). This is the main one."`
	Resume resumeCmd `cmd:"" group:"Workflow" help:"Resume the exact session printed when pix exited."`
	Ls     lsCmd     `cmd:"" group:"Workflow" help:"List your pix sandboxes (name, state, dir)."`
	Rm     rmCmd     `cmd:"" group:"Workflow" help:"Remove pix sandboxes (scoped to pix-* names)."`
	Serve  serveCmd  `cmd:"" group:"Workflow" help:"Run the host services; serve stop|status|start."`
	Status statusCmd `cmd:"" group:"Workflow" aliases:"st" help:"What is up, what is down, what is next."`

	Setup  setupCmd  `cmd:"" group:"Setup & health" help:"Guided setup: keys, memory, pack."`
	Doctor doctorCmd `cmd:"" group:"Setup & health" help:"Diagnose problems and print the fix commands."`
	Reset  resetCmd  `cmd:"" group:"Setup & health" help:"Clean slate: move config+data aside, clear runtime state, remove sandboxes."`

	Memory memoryCmd `cmd:"" group:"Data" aliases:"mem" help:"recall | remember | forget | learnings | stats."`
	Pack   packCmd   `cmd:"" group:"Data" help:"ls | show | use | rm."`

	Models ModelsCmd `cmd:"" group:"Models & agents" help:"Which models pix can use, and which are wired."`
	Agent  AgentCmd  `cmd:"" group:"Models & agents" help:"List the roster: resolved model + WHY (new/edit/rm/reassess retired; edit agents/*.md)."`

	Config configCmd `cmd:"" group:"Config & context" help:"show | path | get | set | unset."`

	Task taskCmd `cmd:"" group:"Parallel work" help:"Parallel task checkouts of one repo."`

	Mcp    mcpCmd    `cmd:"" group:"Integrations & credentials" help:"add | ls | auth."`
	Secret SecretCmd `cmd:"" group:"Integrations & credentials" help:"ls | set | rm | check | sync the op-refs."`

	Version versionCmd `cmd:"" group:"Meta" help:"Print the stamped launcher version."`
	Help    helpCmd    `cmd:"" group:"Meta" passthrough:"" help:"Print this help (or a verb's usage)."`
}

// legacyArgs is the passthrough tail the last two unmigrated seams carry — `serve
// install`/`serve uninstall`, whose flags belong to a hand-rolled loop in the
// service package: kong stops parsing at the subcommand name, so the seam sees the
// whole argv, its own `--help` included.
type legacyArgs struct {
	Args []string `arg:"" optional:"" passthrough:"" help:"Passed to the verb unchanged."`
}

// testSeams is the one indirection the root's tests need: legacy proves a
// passthrough command gets its argv verbatim without running the seam behind
// it. Never set in production.
var testSeams struct {
	legacy func(verb string, args []string)
}

// legacyForward hands a passthrough seam its argv verbatim, or to the test seam
// when one is installed.
func legacyForward(verb string, args []string, fn func([]string) error) error {
	if testSeams.legacy != nil {
		testSeams.legacy(verb, args)
		return nil
	}
	return fn(args)
}

// versionCmd prints the stamped launcher version. Typed rather than passthrough:
// it has no flags, so its usage is entirely generated.
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
	argv = normalizeArgv(argv)
	// A bare positional is `run DIR` when it names a directory, and a verb typo
	// otherwise. kong would call both "unexpected argument".
	if a := argv[0]; !strings.HasPrefix(a, "-") && !knownVerbs()[a] {
		msg, launch := classifyBareArg(a)
		if !launch {
			fmt.Fprint(d.Err, msg)
			return 2
		}
		// A BARE positional (never the explicit `run` verb) is an IMPLICIT launch
		// decision: on a non-interactive terminal nobody is there to have meant it, so
		// it REFUSES rather than creating or attaching a sandbox from a script/pipe.
		// stderr only (never stdout, which a script may capture), exit 2, and the root
		// parser never runs: no create, no attach, no side effect. An explicit `pix run
		// DIR` from the same shell is unaffected.
		if !d.Interactive {
			fmt.Fprintf(d.Err, bareNonTTYRefusalFmt, resolvedBareArgPath(a))
			return 2
		}
		// `pix DIR` IS `pix run DIR`: re-normalize so the pi passthrough tail is
		// rewritten for the run grammar exactly as the explicit spelling is.
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
// path` -> `pix task path NAME`, so `cd "$(pix task foo path)"` reads as written,
// and run's `--` pi tail. A REWRITE, not a parse: the root still owns every
// decision downstream, and it never fires for a real subcommand.
func normalizeArgv(argv []string) []string {
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
	case "new", "run", "ls", "list", "path", "rm", "remove":
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
	Keep    []string `short:"k" help:"With --all: keep this one (repeatable)." placeholder:"NAME"`
	Except  []string `help:"Deprecated spelling of --keep." placeholder:"NAME"`
}

func (c *rmCmd) Run(d *cli.Deps) error {
	// The bare/flag-shape refusals live with the behaviour they protect
	// (launch.validateRmShape): this layer only reports whether the terminal is
	// interactive, which is a fact only it knows.
	return sbxAwareFail(d, launch.Rm(defaultShellEnv(), d.Out, d.Err, launch.RmOptions{
		Names: c.Names, All: c.All, Orphans: c.Orphans, Force: c.Force,
		Except:      append(append([]string(nil), c.Keep...), c.Except...),
		Interactive: d.Interactive,
	}))
}

// ── lifecycle: serve ────────────────────────────────────────────────────────

// serveCmd is the host-services group. Its DEFAULT command execs the sibling
// pix-host's `serve`, so `pix serve --port N` starts the daemon; the control verbs
// beside it are launcher-side and never reach the host binary.
func (c *serveCmd) Help() string { return service.Description }

type serveCmd struct {
	Exec      serveExecCmd      `cmd:"" default:"withargs" hidden:""`
	Stop      serveStopCmd      `cmd:"" help:"Stop a running serve (mode-aware: managed services go through their supervisor)."`
	Status    serveStatusCmd    `cmd:"" help:"Is serve running, and is the memory port up."`
	Start     serveInstallCmd   `cmd:"" passthrough:"" help:"Alias for install: (re)start the managed service."`
	Install   serveInstallCmd   `cmd:"" passthrough:"" help:"Install serve as a managed login service."`
	Uninstall serveUninstallCmd `cmd:"" passthrough:"" help:"Remove the managed login service."`
}

// serveExecCmd forwards to `pix-host serve`. --bind/--port are declared because
// they are the flags serve documents; a raw tail goes through `pix serve -- ARGS`.
type serveExecCmd struct {
	Bind string   `help:"Monitor ingest listen address (default 127.0.0.1, loopback-only)." placeholder:"ADDR"`
	Port int      `help:"Monitor ingest port (default 11437)." placeholder:"N"`
	Args []string `arg:"" optional:"" passthrough:"all" help:"Services to run (default: config's 'services'), then raw pix-host args."`
}

func (c *serveExecCmd) Run(d *cli.Deps) error {
	argv := append([]string{}, c.Args...)
	if c.Bind != "" {
		argv = append(argv, "--bind", c.Bind)
	}
	if c.Port != 0 {
		argv = append(argv, "--port", fmt.Sprint(c.Port))
	}
	return execHostServe(d, argv)
}

// execHostServe runs the sibling pix-host's `serve` through the one host-binary
// exec seam (execHostBinary), so serve and the router map a child failure the
// same way: the host already reported the problem in its own words.
func execHostServe(d *cli.Deps, argv []string) error {
	return execHostBinary(d, append([]string{"serve"}, argv...))
}

type serveStopCmd struct{}

func (c *serveStopCmd) Run(d *cli.Deps) error { return service.StopService(d.Out) }

type serveStatusCmd struct {
	JSON bool `help:"Emit machine-readable JSON."`
}

func (c *serveStatusCmd) Run(d *cli.Deps) error { return service.ReportStatus(d.Out, c.JSON) }

type serveInstallCmd struct{ legacyArgs }

func (c *serveInstallCmd) Run(d *cli.Deps) error {
	return legacyForward("serve install", c.Args, func(a []string) error { return service.RunInstall(d.Out, d.Err, a) })
}

type serveUninstallCmd struct{ legacyArgs }

func (c *serveUninstallCmd) Run(d *cli.Deps) error {
	return legacyForward("serve uninstall", c.Args, func(a []string) error { return service.RunUninstall(d.Out, d.Err, a) })
}
