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
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/mcp"
	"pix/host/monitor"
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
	Ls     lsCmd     `cmd:"" group:"Workflow" help:"List your pix sandboxes (name, state, dir)."`
	Rm     rmCmd     `cmd:"" group:"Workflow" help:"Remove pix sandboxes (scoped to pix-* names)."`
	Serve  serveCmd  `cmd:"" group:"Workflow" help:"Run the host services; serve stop|status|start."`
	Status statusCmd `cmd:"" group:"Workflow" aliases:"st" help:"What is up, what is down, what is next."`

	Setup  setupCmd  `cmd:"" group:"Setup & health" help:"Guided setup: keys, memory, pack."`
	Doctor doctorCmd `cmd:"" group:"Setup & health" help:"Diagnose problems and print the fix commands."`

	Memory memoryCmd `cmd:"" group:"Data" aliases:"mem" help:"recall | remember | forget | learnings | stats."`
	Pack   packCmd   `cmd:"" group:"Data" help:"ls | show | use | rm."`

	Monitor monitorCmd `cmd:"" group:"Observability" help:"Follow a sandbox's out-of-sandbox traffic."`

	Models ModelsCmd `cmd:"" group:"Models & agents" help:"Which models pix can use, and which are wired."`
	Agent  AgentCmd  `cmd:"" group:"Models & agents" help:"List the roster: resolved model + WHY (new/edit/rm/reassess retired; edit agents/*.md)."`

	Config configCmd `cmd:"" group:"Config & context" help:"show | path | get | set | unset."`

	Task taskCmd `cmd:"" group:"Parallel work" help:"Parallel task checkouts of one repo."`

	Mcp    mcpCmd    `cmd:"" group:"Integrations & credentials" help:"register | ls | load | auth | bundle."`
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

// testSeams are the two indirections the root's tests need — legacy proves a
// passthrough command gets its argv verbatim without running the seam behind it,
// monitor supplies a context + store root without a signal handler. Never set in
// production.
var testSeams struct {
	legacy  func(verb string, args []string)
	monitor func(*monitorCmd, *cli.Deps) error
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

// ── monitor ─────────────────────────────────────────────────────────────────

// monitorDescription is the verb's long help: the operational facts (reader, not
// listener; where the store comes from; the env the in-VM tap reads) that
// generated usage cannot infer from a struct tag.
const monitorDescription = `Concisely follow a sandbox's out-of-sandbox traffic (model requests,
responses, tool + MCP calls, context/control events), as captured on disk.

This is a PURE READER: it never binds a port or starts a listener. The
ingest listener that receives events from the in-VM tap runs inside
'pix serve' (:11437, loopback-only by default: see 'pix serve --help'
for its --bind/--port flags). With no --path, monitor tails the same store
root serve writes to; run 'pix serve' first (or already have it running)
for there to be anything to follow.

DEFAULT IS ONE-SHOT: with no --follow and not run at an interactive
terminal (a pipe, 'pix monitor --json | head -5', a script), monitor prints
whatever is ALREADY stored and exits — it never blocks waiting for more. If
nothing is stored AND no ingest listener is running, that is reported as an
actionable error (exit 3), not silent empty output: run 'pix serve', or pass
--path at a store that already has something in it. Nothing stored while an
ingest listener IS running is reported as empty success (there is genuinely
nothing yet).

An interactive terminal keeps the old live-follow default (equivalent to
--follow) and prints one banner line to stderr naming what it found before
it starts waiting, so a TTY run is never a silent, indefinite hang.

NOTE: live events only flow from a sandbox created with an image that
includes the monitor extension + the :11437 network allowlist entry (baked
via 'make load' on the host). A stale sandbox predates that and shows no
events — rebuild/reload the image, then recreate the sandbox.

ENV (read by the in-VM extension, documented here for discoverability):
  PIX_MONITOR=0        disable the in-VM tap entirely (no events sent)
  PIX_MONITOR_URL      override the host ingest URL
                       (default http://host.docker.internal:11437)`

// monitorCmd is a pure offline reader over the on-disk event store — hence a
// --path and no --bind.
func (c *monitorCmd) Help() string { return monitorDescription }

type monitorCmd struct {
	Name   string `arg:"" optional:"" help:"Filter to one sandbox/session by id substring, CASE-SENSITIVE."`
	Path   string `help:"Read this store directory instead of <state-dir>/monitor." placeholder:"DIR"`
	JSON   bool   `help:"Print the raw stored event JSON (one object per line, pipe to jq)."`
	Follow bool   `short:"f" help:"Keep streaming as new events land instead of the one-shot default. Implied at an interactive terminal."`
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
// deterministically instead of signalling.
func (c *monitorCmd) follow(ctx context.Context, d *cli.Deps, tty bool) error {
	root := c.Path
	if root == "" {
		var err error
		if root, err = config.MonitorStoreRoot(); err != nil {
			return fmt.Errorf("resolve monitor store root: %w", err)
		}
	}
	// OpenStore is read-only: a one-shot run must not fabricate an empty
	// store just by looking, which would turn "no store yet" (actionable)
	// into indistinguishable real emptiness.
	store, err := monitor.OpenStore(root)
	if err != nil {
		return err
	}
	cfg := monitor.FollowConfig{Filter: c.Name, JSON: c.JSON, TTY: tty, Out: d.Out}

	if !c.Follow && !tty {
		return c.once(store, cfg, d)
	}
	if tty {
		fmt.Fprintln(d.Err, monitorBanner(store))
	}
	monitor.Follow(ctx, store, cfg)
	return nil
}

// once prints whatever is already stored and returns, per DEFAULT IS
// ONE-SHOT above. Empty output is honest success only when a live ingest
// listener could plausibly still fill the store in; with nothing stored AND
// no listener running, empty output would be indistinguishable from broken,
// so that combination is reported instead as an actionable, nonzero error.
func (c *monitorCmd) once(store *monitor.Store, cfg monitor.FollowConfig, d *cli.Deps) error {
	metas, err := store.List()
	if err != nil {
		return fmt.Errorf("monitor: list stored streams: %w", err)
	}
	if len(metas) == 0 && !ingestUp() {
		fmt.Fprintln(d.Err, "pix monitor: no stored events, and no ingest listener is running.")
		fmt.Fprintln(d.Err, "  Start it with `pix serve` (or point --path at a store that already has data), then re-run.")
		fmt.Fprintln(d.Err, "  `pix monitor --follow` keeps this command open and waits for events instead of exiting.")
		return cli.SilentError{Code: rpc.ExitServiceDown}
	}
	return monitor.Once(store, cfg)
}

// ingestUp reports whether a `pix-host serve` daemon (the only process that
// ever writes to the monitor store) appears to be running, so an empty
// one-shot read can tell "nothing has happened yet" apart from "nothing ever
// will". A false negative just makes the error message fire when serve
// happens to be reachable some OTHER way (e.g. a still-warming managed
// unit); it never blocks a real read, since it only gates the error path.
func ingestUp() bool {
	up, _ := service.ServeIdentityUp(service.ManagedActive, config.ServePidPath(), 0)
	return up
}

// monitorBanner is the one honest line an interactive follow prints to
// stderr before it starts waiting, so a TTY run is never a silent hang: it
// says what is already stored and whether an ingest listener was even found
// to feed it anything more.
func monitorBanner(store *monitor.Store) string {
	metas, _ := store.List()
	up := ingestUp()
	switch {
	case len(metas) == 0 && !up:
		return "pix monitor: no stored events and no ingest listener detected — following anyway, but nothing will arrive until `pix serve` is running. Ctrl-C to stop."
	case len(metas) == 0:
		return "pix monitor: no stored events yet — following live, waiting for the first one. Ctrl-C to stop."
	default:
		return fmt.Sprintf("pix monitor: following %d stored stream(s) live. Ctrl-C to stop.", len(metas))
	}
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
