// root.go — the ONE kong root: the launcher's whole verb table as a typed
// tree, and the only thing that parses argv, dispatches a verb, or answers a
// help request.
//
// It replaces main.go's `switch args[0]`, the last place three sources of
// truth could disagree: the switch decided what was dispatchable, a
// hand-written `knownVerbs` map decided what the suggester knew about, and a
// pile of usage constants decided what help said. A verb could be in any two
// of the three (`gworkspace` once was). Here the tree IS all three.
//
// Two decisions stay in FRONT of the parser, because they are argv SHAPE, not
// grammar: the retired table (retired.go), which must answer before any config
// read or side effect, and a bare positional that names an existing directory,
// which is `run DIR` (classifyBareArg) — plus the `task NAME path` rewrite.
//
// Migration status: the lifecycle group (ls, rm, reset, serve), task, monitor
// and version are TYPED — struct tags parse them and generate their help.
// Every other verb is a `passthrough:""` command whose argv reaches its
// existing seam VERBATIM, so the cutover changed no behaviour it did not
// migrate. Each adapter that lands deletes its field's Args tail.
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
	"pix/host/workflow/provision"
)

// rootCmd is the verb table. Ordering follows the tiered help so the two read
// the same way; kong sorts its own listing.
type rootCmd struct {
	// ── typed: migrated to struct tags ──────────────────────────────────────
	Ls      lsCmd      `cmd:"" help:"List your pix sandboxes (name, state, dir)."`
	Rm      rmCmd      `cmd:"" help:"Remove pix sandboxes (scoped to pix-* names)."`
	Reset   resetCmd   `cmd:"" help:"Move Pix's state aside (reversible). (WRITES)"`
	Serve   serveCmd   `cmd:"" help:"Run the host services; serve stop|status|start."`
	Task    taskCmd    `cmd:"" help:"Parallel task checkouts of one repo."`
	Monitor monitorCmd `cmd:"" help:"Follow a sandbox's out-of-sandbox traffic."`

	// ── legacy adapters: argv reaches the existing seam verbatim ────────────
	Run     legacyRunCmd    `cmd:"" passthrough:"" help:"Launch the sandbox in DIR (default: .)."`
	Status  legacyStatusCmd `cmd:"" passthrough:"" aliases:"st" help:"What is up, what is down, what is next."`
	Version versionCmd      `cmd:"" help:"Print the stamped launcher version."`
	Config  legacyConfigCmd `cmd:"" passthrough:"" help:"show | path | get | set | unset."`
	Doctor  legacyDoctorCmd `cmd:"" passthrough:"" help:"Diagnose problems and print the fix commands."`
	Setup   legacySetupCmd  `cmd:"" passthrough:"" help:"Guided setup: keys, memory, pack."`
	Mcp     legacyMcpCmd    `cmd:"" passthrough:"" help:"register | ls | load | auth | bundle."`
	Pack    legacyPackCmd   `cmd:"" passthrough:"" help:"new | add | ls | show | use | rm."`
	Secret  legacySecretCmd `cmd:"" passthrough:"" help:"ls | set | rm | check | sync the op-refs."`
	Memory  legacyMemoryCmd `cmd:"" passthrough:"" aliases:"mem" help:"recall | remember | forget | learnings | stats."`
	Models  legacyModelsCmd `cmd:"" passthrough:"" help:"Which models pix can use, and which are wired."`
	Agent   legacyAgentCmd  `cmd:"" passthrough:"" help:"ls | new | edit | rm | reassess."`
	State   legacyStateCmd  `cmd:"" passthrough:"" help:"reset (grouped alias)."`
	Help    legacyHelpCmd   `cmd:"" passthrough:"" help:"Print this help (or a verb's usage)."`
}

// legacyArgs is the passthrough tail every unmigrated verb carries. kong stops
// parsing at the verb name, so the seam behind it sees exactly the argv the
// switch used to hand it — including flags kong knows nothing about, and a
// `--help` that verb answers itself.
type legacyArgs struct {
	Args []string `arg:"" optional:"" passthrough:"" help:"Passed to the verb unchanged."`
}

type legacyRunCmd struct{ legacyArgs }
type legacyStatusCmd struct{ legacyArgs }
type legacyConfigCmd struct{ legacyArgs }
type legacyDoctorCmd struct{ legacyArgs }
type legacySetupCmd struct{ legacyArgs }
type legacyMcpCmd struct{ legacyArgs }
type legacyPackCmd struct{ legacyArgs }
type legacySecretCmd struct{ legacyArgs }
type legacyMemoryCmd struct{ legacyArgs }
type legacyModelsCmd struct{ legacyArgs }
type legacyAgentCmd struct{ legacyArgs }
type legacyStateCmd struct{ legacyArgs }
type legacyHelpCmd struct{ legacyArgs }

func (c *legacyRunCmd) Run(*cli.Deps) error    { return legacyForward("run", c.Args, runVerb) }
func (c *legacyStatusCmd) Run(*cli.Deps) error { return legacyForward("status", c.Args, runStatusCmd) }
func (c *legacyConfigCmd) Run(*cli.Deps) error {
	return legacyForward("config", c.Args, provision.RunConfig)
}
func (c *legacyDoctorCmd) Run(*cli.Deps) error { return legacyForward("doctor", c.Args, runDoctorCmd) }
func (c *legacySetupCmd) Run(*cli.Deps) error  { return legacyForward("setup", c.Args, runSetupCmd) }
func (c *legacyMcpCmd) Run(*cli.Deps) error    { return legacyForward("mcp", c.Args, runMcpCmd) }
func (c *legacyPackCmd) Run(*cli.Deps) error   { return legacyForward("pack", c.Args, runPackCmd) }
func (c *legacySecretCmd) Run(*cli.Deps) error { return legacyForward("secret", c.Args, runSecretCmd) }
func (c *legacyMemoryCmd) Run(*cli.Deps) error { return legacyForward("memory", c.Args, runMemory) }
func (c *legacyModelsCmd) Run(*cli.Deps) error { return legacyForward("models", c.Args, runModels) }
func (c *legacyAgentCmd) Run(*cli.Deps) error  { return legacyForward("agent", c.Args, runAgent) }
func (c *legacyStateCmd) Run(*cli.Deps) error  { return legacyForward("state", c.Args, runState) }

func (c *legacyHelpCmd) Run(d *cli.Deps) error {
	if legacyIntercepted("help", c.Args) {
		return nil
	}
	runHelp(d, c.Args)
	return nil
}

// testSeams are the two indirections the root's own tests need: legacy proves
// an adapter is handed its argv verbatim without running a verb that launches
// a sandbox, and monitor supplies a context + store root without a signal
// handler. One var, because each is a global the production path never sets.
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

// runHelp is the `help` verb: the tiered screen, `--all`, or one verb's usage.
func runHelp(d *cli.Deps, argv []string) {
	if len(argv) > 0 {
		if argv[0] == "--all" {
			fmt.Fprint(d.Out, helpAllText)
			return
		}
		if u, ok := verbUsage(argv[0]); ok {
			fmt.Fprint(d.Out, u)
			return
		}
	}
	fmt.Fprint(d.Out, helpText)
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

// dispatch parses and runs one argv against the root, and returns the process
// exit code. It is the SINGLE exit mapper: 0 success, 1 failure, 2 the user's
// invocation was wrong, and whatever code a command asked for by name (3, the
// "unverifiable" arm of the readiness contract, is the only other one in use).
func dispatch(argv []string, d *cli.Deps) int {
	argv = normalizeArgv(argv)
	// A bare positional is `run DIR` when it names a directory, and a verb typo
	// otherwise. kong would call both "unexpected argument".
	if a := argv[0]; !strings.HasPrefix(a, "-") && !knownVerbs[a] {
		msg, launch := classifyBareArg(a)
		if launch {
			runVerb(argv)
			return 0
		}
		fmt.Fprint(d.Err, msg)
		return 2
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

// normalizeArgv rewrites the one argv shape the grammar cannot express:
// `pix task NAME path` -> `pix task path NAME`, so `cd "$(pix task foo path)"`
// reads the way a user writes it. It never fires when the first token is
// itself a subcommand, and it is a REWRITE, not a parse — the root still owns
// every decision downstream of it.
func normalizeArgv(argv []string) []string {
	if len(argv) == 3 && argv[0] == "task" && argv[2] == "path" && !isTaskKnownVerb(argv[1]) {
		return []string{"task", "path", argv[1]}
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

// monitorCmd is the `monitor` verb: a pure offline reader over the on-disk
// event store, which is why it has a --path and no --bind.
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
// deterministically instead of signalling, and defaultRoot/tty are injected.
// There is no listener to inject — monitor is a Follow loop over the store.
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
	return launch.Ls(launch.DefaultEnv(), d.Out, c.JSON)
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
	return launch.Rm(launch.DefaultEnv(), d.Out, d.Err, launch.RmOptions{
		Names: c.Names, All: c.All, Except: c.Except,
	})
}

// ── lifecycle: serve ────────────────────────────────────────────────────────

// serveCmd is the host-services group. Its DEFAULT command execs the sibling
// pix-host binary's `serve`, so `pix serve` and `pix serve --port N` still
// start the daemon; the control verbs beside it (stop/status/start/…) are
// launcher-side and never reach the host binary.
func (c *serveCmd) Help() string { return service.Description }

type serveCmd struct {
	Exec      serveExecCmd      `cmd:"" default:"withargs" hidden:""`
	Stop      serveStopCmd      `cmd:"" help:"Stop a running serve (mode-aware: managed services go through their supervisor)."`
	Status    serveStatusCmd    `cmd:"" help:"Is serve running, and is the memory port up."`
	Start     serveInstallCmd   `cmd:"" passthrough:"" help:"Alias for install: (re)start the managed service."`
	Install   serveInstallCmd   `cmd:"" passthrough:"" help:"Install serve as a managed login service."`
	Uninstall serveUninstallCmd `cmd:"" passthrough:"" help:"Remove the managed login service."`
}

// serveExecCmd forwards to `pix-host serve`. --bind/--port are declared here
// because they are the two flags the host's serve documents; anything else is
// an argument, and a raw tail can always be forced with `pix serve -- ARGS`.
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

// execHostServe runs the sibling pix-host binary's `serve` subcommand, found
// next to this binary or on PATH.
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
