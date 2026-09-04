// sessionctl.go — the hidden internal invocation modes architecture §7.2
// asks for: "the child runner is an invocation mode of the pix binary, not
// a daemon", reached only through a narrowly scoped MCP command this same
// binary also implements. Both modes below are dispatched BEFORE root.go's
// kong parser ever sees argv (see hiddenSessionVerb in root.go's dispatch):
// they are never a kong command field, never appear in knownVerbs(), and
// never appear in `pix help --all` — helpAll is generated straight off
// rootCmd's fields (help.go), and these two names are not one.
//
// Reserved declaration contract: the ONE static-mcp server name this
// feature will ever register as is session.ReservedMCPName
// ("pix-session"). It identifies a PIX-OWNED command (this binary,
// `pix __pix-session-mcp`), never a pack-contributed one, so a future
// Tier-1 trust-bill renderer (architecture §11) must recognize this exact
// name as a reserved, pix-owned local MCP command rather than routing it
// through the general pack host-exec bill of materials a THIRD PARTY
// integration would need. That trust-bill special case, and the launch-time
// registration that hands this server its env context, are follow-up wiring
// this change intentionally does not reach (see the handoff note in the
// commit this file ships with); this file's contract (the env names below,
// the reserved MCP name) is what that follow-up wires to.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"pix/host/session"
)

// hiddenSessionMCPVerb and hiddenSessionChildVerb are argv[0] tokens, never
// kong commands. The "__" prefix is not load-bearing on its own — what
// keeps them hidden is that dispatch (root.go) intercepts them before kong
// ever parses argv, so no help/usage code path can render them even by
// accident.
const (
	hiddenSessionMCPVerb   = "__pix-session-mcp"
	hiddenSessionChildVerb = "__pix-session-child"
)

// runHiddenSessionVerb handles the two internal invocation modes. handled
// is false for every other argv, so root.go's dispatch falls through to the
// ordinary kong path unchanged.
func runHiddenSessionVerb(argv []string, d *cliDeps) (code int, handled bool) {
	if len(argv) == 0 {
		return 0, false
	}
	switch argv[0] {
	case hiddenSessionMCPVerb:
		return runSessionMCP(d), true
	case hiddenSessionChildVerb:
		return runSessionChild(argv[1:], d), true
	default:
		return 0, false
	}
}

// cliDeps is the narrow slice of cli.Deps this file actually needs — its
// own type rather than importing cli.Deps's exact shape into every helper
// here — so the flags/env parsing above is directly unit-testable with
// plain io.Writer/io.Reader values.
type cliDeps struct {
	Out io.Writer
	Err io.Writer
	In  io.Reader
}

// sessionEnv is the fixed set of environment variables the hidden MCP mode
// reads its ServerContext from. Wiring these into a running sandbox at
// `pix run` launch time is the launch-boundary half of this feature; absent
// entirely, the server starts (so `tools/list` still answers) but refuses
// every delegate call with a clear, named gap rather than a panic.
const (
	envSessionTree     = "PIX_SESSION_TREE"
	envSessionParent   = "PIX_SESSION_PARENT"
	envSessionSandbox  = "PIX_SESSION_SANDBOX"
	envSessionInstance = "PIX_SESSION_INSTANCE"
	envSessionDir      = "PIX_SESSION_DIR"
	envSessionStore    = "PIX_SESSION_STORE"
)

func sessionContextFromEnv() (session.ServerContext, error) {
	ctx := session.ServerContext{
		TreeID:     os.Getenv(envSessionTree),
		ParentID:   os.Getenv(envSessionParent),
		Sandbox:    os.Getenv(envSessionSandbox),
		InstanceID: os.Getenv(envSessionInstance),
		SandboxDir: os.Getenv(envSessionDir),
		StoreRoot:  os.Getenv(envSessionStore),
	}
	var missing []string
	for name, v := range map[string]string{
		envSessionTree: ctx.TreeID, envSessionSandbox: ctx.Sandbox,
		envSessionInstance: ctx.InstanceID, envSessionDir: ctx.SandboxDir, envSessionStore: ctx.StoreRoot,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return ctx, fmt.Errorf("pix: session-control server is missing %s (not launched under a session-aware sandbox)", strings.Join(missing, ", "))
	}
	return ctx, nil
}

// runSessionMCP is `pix __pix-session-mcp`: a stdio JSON-RPC server for the
// WHOLE lifetime of the sandbox's Gateway connection to it. A missing
// session context degrades to "serve, but refuse every delegate call" (see
// spawnFailer below) rather than exiting non-zero, so `tools/list` still
// answers honestly instead of the Gateway seeing a dead command.
func runSessionMCP(d *cliDeps) int {
	ctx, err := sessionContextFromEnv()
	spawn := spawnDetachedChild
	if err != nil {
		fmt.Fprintln(d.Err, err)
		spawn = spawnFailer(err)
	}
	srv := session.NewServer(ctx, spawn, os.Stdin, os.Stdout)
	if serr := srv.Serve(); serr != nil {
		fmt.Fprintf(d.Err, "pix: session-control server: %v\n", serr)
		return 1
	}
	return 0
}

func spawnFailer(err error) session.Spawner {
	return func(session.ServerContext, string, string, session.ChildRequest) error { return err }
}

// spawnDetachedChild is the production Spawner: it re-execs THIS binary as
// `pix __pix-session-child`, detached into its own session (Setsid) so it
// is never a child of the MCP server's own process group and so a parent
// pi/pix process exiting sends it no signal — exactly the "outlives the
// interactive root" property architecture §7.2 asks for. It does not wait:
// RunChild's own reference hold is what keeps the sandbox alive while the
// child runs, not this call blocking on it.
func spawnDetachedChild(ctx session.ServerContext, treeID, nodeID string, req session.ChildRequest) error {
	self, err := os.Executable()
	if err != nil {
		self = "pix"
	}
	cmd := exec.Command(self, hiddenSessionChildVerb,
		"--tree", treeID, "--node", nodeID, "--parent", ctx.ParentID,
		"--sandbox", ctx.Sandbox, "--instance", ctx.InstanceID,
		"--dir", ctx.SandboxDir, "--store", ctx.StoreRoot,
		"--agent", req.Agent, "--task", req.Task, "--model", req.Model, "--target", req.Target,
	)
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pix: could not start the session child runner: %w", err)
	}
	// Deliberately not Wait()ed: a detached child-runner's exit is reaped by
	// init/the OS, and this process must return to the Gateway immediately.
	return nil
}

// runSessionChild is `pix __pix-session-child`: the child-runner itself.
// Every field it needs travels as an explicit flag (never a general
// command/argv), matching the same bounded contract the MCP tool enforces
// at its own boundary — this process is what THAT boundary spawns, so it
// re-validates rather than trusting its own argv blindly.
func runSessionChild(args []string, d *cliDeps) int {
	fs := flag.NewFlagSet(hiddenSessionChildVerb, flag.ContinueOnError)
	fs.SetOutput(d.Err)
	tree := fs.String("tree", "", "session tree id")
	node := fs.String("node", "", "this child's node id")
	parent := fs.String("parent", "", "parent node id")
	sandbox := fs.String("sandbox", "", "sandbox name")
	instance := fs.String("instance", "", "sbx instance id this reference binds to")
	dir := fs.String("dir", "", "sandbox session directory (reference locks live here)")
	store := fs.String("store", "", "session store root (node records live here)")
	agent := fs.String("agent", "", "delegated agent name")
	task := fs.String("task", "", "delegated task")
	model := fs.String("model", "", "delegated model")
	target := fs.String("target", "", "session target")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opts := session.ChildRunOpts{
		SandboxDir: *dir, StoreRoot: *store, TreeID: *tree, NodeID: *node, ParentID: *parent,
		Sandbox: *sandbox, InstanceID: *instance,
		Request: session.ChildRequest{Agent: *agent, Task: *task, Model: *model, Target: *target},
	}
	if err := session.RunChild(opts, defaultChildExecutor(*sandbox)); err != nil {
		fmt.Fprintf(d.Err, "pix: session child: %v\n", err)
		return 1
	}
	return 0
}

// defaultChildExecutor is the first (and, per architecture §7.2, only
// initially supported) target's real work: local-process re-enters the SAME
// sandbox to run the delegated agent, built from the request's bounded
// fields only — never from a caller-supplied argv. RunChild already refused
// any OTHER target before an Executor is ever invoked, so this is reached
// only for local-process.
func defaultChildExecutor(sandboxName string) session.Executor {
	return func(req session.ChildRequest) error {
		cmd := exec.Command("sbx", "exec", sandboxName, "--",
			"pi", "--no-extensions", "--agent", req.Agent, "-p", req.Task)
		if req.Model != "" {
			cmd.Args = append(cmd.Args, "--model", req.Model)
		}
		cmd.Env = os.Environ()
		return cmd.Run()
	}
}
