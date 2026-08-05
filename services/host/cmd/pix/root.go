// root.go — the ONE kong root: the launcher's verb table as a typed tree, and
// the only thing that parses argv, dispatches a verb, or answers a help
// request. It replaces main.go's `switch args[0]`, which let three sources of
// truth disagree — the switch decided what was dispatchable, a hand-written
// knownVerbs map decided what the suggester knew, and usage constants decided
// what help said. Here the tree is all three.
//
// Two decisions stay in FRONT of the parser because they are argv SHAPE, not
// grammar: the retired table (retired.go), which must answer before any config
// read or side effect, and a bare positional naming a directory, which is
// `run DIR` (classifyBareArg) — plus the `task NAME path` rewrite.
//
// EVERY verb is now TYPED: struct tags parse it, generate its help, and place
// it in a help tier (`group:`). The passthrough seams that handed a verb's argv
// verbatim to a hand-rolled switch are gone, and with them the usage constants
// that stood beside those switches — `pix help <verb>` re-enters the root, so
// the usage a user reads is generated from the tags that parse the verb.
//
// `help` is the one command that stays `passthrough:""`, and for the opposite
// reason: `pix help <anything>` is a question, not a grammar, so it must answer
// rather than reject.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/launcher"
	"pix/host/monitor"
	"pix/host/service"
	"pix/host/sys"
	"pix/host/workflow/launch"
)

// rootCmd is the verb table. Field ORDER and the `group:` keys ARE the tiered
// help: helpAll renders the listing straight off this declaration, so a verb
// cannot be dispatchable and undocumented at the same time.
type rootCmd struct {
	Run    runCmd    `cmd:"" group:"Workflow" help:"Launch the sandbox in DIR (default: .). This is the main one."`
	Ls     lsCmd     `cmd:"" group:"Workflow" help:"List your pix sandboxes (name, state, dir)."`
	Rm     rmCmd     `cmd:"" group:"Workflow" help:"Remove pix sandboxes (scoped to pix-* names)."`
	Serve  serveCmd  `cmd:"" group:"Workflow" help:"Run the host services; serve stop|status|start."`
	Status statusCmd `cmd:"" group:"Workflow" aliases:"st" help:"What is up, what is down, what is next."`

	Setup  setupCmd  `cmd:"" group:"Setup & health" help:"Guided setup: keys, memory, pack."`
	Doctor doctorCmd `cmd:"" group:"Setup & health" help:"Diagnose problems and print the fix commands."`

	Memory memoryCmd `cmd:"" group:"Data" aliases:"mem" help:"recall | remember | forget | learnings | stats."`
	Pack   packCmd   `cmd:"" group:"Data" help:"new | add | ls | show | use | rm."`

	Monitor monitorCmd `cmd:"" group:"Observability" help:"Follow a sandbox's out-of-sandbox traffic."`

	Models ModelsCmd `cmd:"" group:"Models & agents" help:"Which models pix can use, and which are wired."`
	Agent  AgentCmd  `cmd:"" group:"Models & agents" help:"ls | new | edit | rm | reassess."`

	Config configCmd `cmd:"" group:"Config & context" help:"show | path | get | set | unset."`

	Task taskCmd `cmd:"" group:"Parallel work" help:"Parallel task checkouts of one repo."`

	Mcp    mcpCmd    `cmd:"" group:"Integrations & credentials" help:"register | ls | load | auth | bundle."`
	Secret SecretCmd `cmd:"" group:"Integrations & credentials" help:"ls | set | rm | check | sync the op-refs."`

	State stateCmd `cmd:"" group:"State (on-disk lifecycle)" help:"reset (grouped alias)."`
	Reset resetCmd `cmd:"" group:"State (on-disk lifecycle)" help:"Move Pix's state aside (reversible). (WRITES)"`

	Version versionCmd `cmd:"" group:"Meta" help:"Print the stamped launcher version."`
	Help    helpCmd    `cmd:"" group:"Meta" passthrough:"" help:"Print this help (or a verb's usage)."`
}

// legacyArgs is the passthrough tail the last two unmigrated seams carry —
// `serve install`/`serve uninstall`, whose flags belong to a hand-rolled loop
// in the service package: kong stops parsing at the subcommand name, so the
// seam sees the argv the switch used to hand it, its own `--help` included.
type legacyArgs struct {
	Args []string `arg:"" optional:"" passthrough:"" help:"Passed to the verb unchanged."`
}

// testSeams are the two indirections the root's tests need: legacy proves a
// passthrough command gets its argv verbatim without running the seam behind
// it; monitor supplies a context + store root without a signal handler. Never
// set in production.
var testSeams struct {
	legacy  func(verb string, args []string)
	monitor func(*monitorCmd, *cli.Deps) error
}

func legacyIntercepted(verb string, args []string) bool {
	if testSeams.legacy == nil {
		return false
	}
	testSeams.legacy(verb, args)
	return true
}

func legacyForward(verb string, args []string, fn func([]string)) error {
	if legacyIntercepted(verb, args) {
		return nil
	}
	fn(args)
	return nil
}

// versionCmd prints the stamped launcher version. Typed rather than
// passthrough: it has no flags at all, so its usage is entirely generated.
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
		// Story04c: a BARE positional (never the explicit `run` verb) is an
		// IMPLICIT launch decision — on a non-interactive terminal there is
		// nobody to have meant it, so it refuses rather than silently creating
		// or attaching a sandbox from a script/pipe. stderr only (never stdout,
		// which a script may be capturing), exit 2, and the root parser never
		// runs at all: no create, no attach, no side effect of any kind. An
		// explicit `pix run DIR` from the same non-interactive shell is
		// unaffected — see run_cmd.go's own TTY-driven exec -it/-i choice.
		if !d.Interactive {
			fmt.Fprintf(d.Err, bareNonTTYRefusalFmt, resolvedBareArgPath(a))
			return 2
		}
		// `pix DIR` IS `pix run DIR`; re-normalize so the pi passthrough tail is
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

// normalizeArgv rewrites the one shape the grammar cannot express:
// `pix task NAME path` -> `pix task path NAME`, so `cd "$(pix task foo path)"`
// reads as written. A REWRITE, not a parse: the root still owns every decision
// downstream, and it never fires for a real subcommand.
func normalizeArgv(argv []string) []string {
	if len(argv) == 3 && argv[0] == "task" && argv[2] == "path" && !isTaskKnownVerb(argv[1]) {
		return []string{"task", "path", argv[1]}
	}
	// `run ... -- <pi args>`: kong eats the `--` and would feed the first pi arg
	// to run's DIR positional, so the tail is rewritten into repeated
	// `--pi-arg=` values before the parser sees it (see run_cmd.go).
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

// ── monitor ─────────────────────────────────────────────────────────────────

// monitorDescription is the verb's long help: the operational facts (reader,
// not listener; where the store comes from; the env the in-VM tap reads) that
// generated usage cannot infer from a struct tag.
const monitorDescription = `Concisely follow a sandbox's out-of-sandbox traffic (model requests,
responses, tool + MCP calls, context/control events), as captured on disk.

This is a PURE READER: it never binds a port or starts a listener. The
ingest listener that receives events from the in-VM tap runs inside
'pix serve' (:11437, loopback-only by default — see 'pix serve --help'
for its --bind/--port flags). With no --path, monitor tails the same store
root serve writes to; run 'pix serve' first (or already have it running)
for there to be anything to follow.

NOTE: live events only flow from a sandbox created with an image that
includes the monitor extension + the :11437 network allowlist entry (baked
via 'make load' on the host). A stale sandbox predates that and shows no
events — rebuild/reload the image, then recreate the sandbox.

ENV (read by the in-VM extension, documented here for discoverability):
  PIX_MONITOR=0        disable the in-VM tap entirely (no events sent)
  PIX_MONITOR_URL      override the host ingest URL
                       (default http://host.docker.internal:11437)`

// monitorCmd is a pure offline reader over the on-disk event store — which is
// why it has a --path and no --bind.
func (c *monitorCmd) Help() string { return monitorDescription }

type monitorCmd struct {
	Name string `arg:"" optional:"" help:"Filter to one sandbox/session by id substring, CASE-SENSITIVE."`
	Path string `help:"Read this store directory instead of <state-dir>/monitor." placeholder:"DIR"`
	JSON bool   `help:"Print the raw stored event JSON (one object per line, pipe to jq)."`
}

func (c *monitorCmd) Run(d *cli.Deps) error {
	if testSeams.monitor != nil {
		return testSeams.monitor(c, d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	tty := false
	if f, ok := d.Out.(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	return c.follow(ctx, d, tty)
}

// follow is the testable core: ctx governs the run so a test cancels
// deterministically instead of signalling. There is no listener to inject.
func (c *monitorCmd) follow(ctx context.Context, d *cli.Deps, tty bool) error {
	root := c.Path
	if root == "" {
		var err error
		if root, err = config.MonitorStoreRoot(); err != nil {
			return fmt.Errorf("resolve monitor store root: %w", err)
		}
	}
	store, err := monitor.NewStore(monitor.StoreConfig{Root: root})
	if err != nil {
		return err
	}
	monitor.Follow(ctx, store, monitor.FollowConfig{Filter: c.Name, JSON: c.JSON, TTY: tty, Out: d.Out})
	return nil
}

// ── lifecycle: ls / rm ──────────────────────────────────────────────────────

func (c *lsCmd) Help() string { return launch.LsDescription }

type lsCmd struct {
	JSON bool `help:"Emit machine-readable JSON."`
}

func (c *lsCmd) Run(d *cli.Deps) error {
	return launch.Ls(defaultShellEnv(), d.Out, c.JSON)
}

func (c *rmCmd) Help() string { return launch.RmDescription }

type rmCmd struct {
	Names  []string `arg:"" optional:"" help:"Sandbox names to remove (pix-* only)."`
	All    bool     `help:"Remove every pix-* sandbox."`
	Except []string `help:"With --all: keep this one (repeatable)." placeholder:"NAME"`
}

func (c *rmCmd) Run(d *cli.Deps) error {
	if !c.All && len(c.Names) == 0 {
		return cli.Usagef("name a sandbox to remove, or use --all (see `pix rm --help`)")
	}
	return launch.Rm(defaultShellEnv(), d.Out, d.Err, launch.RmOptions{
		Names: c.Names, All: c.All, Except: c.Except,
	})
}

// ── lifecycle: serve ────────────────────────────────────────────────────────

// serveCmd is the host-services group. Its DEFAULT command execs the sibling
// pix-host's `serve`, so `pix serve --port N` still starts the daemon; the
// control verbs beside it are launcher-side and never reach the host binary.
func (c *serveCmd) Help() string { return service.Description }

type serveCmd struct {
	Exec      serveExecCmd      `cmd:"" default:"withargs" hidden:""`
	Stop      serveStopCmd      `cmd:"" help:"Stop a running serve (mode-aware: managed services go through their supervisor)."`
	Status    serveStatusCmd    `cmd:"" help:"Is serve running, and is the memory port up."`
	Start     serveInstallCmd   `cmd:"" passthrough:"" help:"Alias for install: (re)start the managed service."`
	Install   serveInstallCmd   `cmd:"" passthrough:"" help:"Install serve as a managed login service."`
	Uninstall serveUninstallCmd `cmd:"" passthrough:"" help:"Remove the managed login service."`
}

// serveExecCmd forwards to `pix-host serve`. --bind/--port are declared
// because they are the flags that serve documents; a raw tail still goes
// through with `pix serve -- ARGS`.
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

// execHostServe runs the sibling pix-host's `serve`, found next to this binary
// or on PATH.
func execHostServe(d *cli.Deps, argv []string) error {
	bin, err := launcher.FindHostBinary()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, append([]string{"serve"}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, d.Out, d.Err
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// The host already reported the problem in its own words.
			return cli.SilentError{Code: exit.ExitCode()}
		}
		return fmt.Errorf("exec %s: %w", bin, err)
	}
	return nil
}

type serveStopCmd struct{}

func (c *serveStopCmd) Run(d *cli.Deps) error { return service.StopService(d.Out) }

type serveStatusCmd struct {
	JSON bool `help:"Emit machine-readable JSON."`
}

func (c *serveStatusCmd) Run(d *cli.Deps) error { return service.ReportStatus(d.Out, c.JSON) }

type serveInstallCmd struct{ legacyArgs }

func (c *serveInstallCmd) Run(*cli.Deps) error {
	return legacyForward("serve install", c.Args, service.RunInstall)
}

type serveUninstallCmd struct{ legacyArgs }

func (c *serveUninstallCmd) Run(*cli.Deps) error {
	return legacyForward("serve uninstall", c.Args, service.RunUninstall)
}
