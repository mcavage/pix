package main

// mcp_cmd.go is `pix mcp`: the typed verb tree, plus the composition the mcp
// capability deliberately does not do for itself (the active pack's
// containers, a real env, the host-binary resolver). kong owns the grammar;
// mcpFailed owns the one exit mapping.
//
// The honesty contract is why this verb has a custom mapping at all: a
// subcommand that PROMISES an operation must never exit 0 having done nothing.
// sbx missing prints the exact command to run by hand and exits 3
// (rpc.ExitServiceDown); a child's own exit code is propagated as-is.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"pix/host/cli"
	"pix/host/launcher"
	"pix/host/mcp"
	"pix/host/rpc"
	"pix/host/workflow/pack"
	"pix/host/workspace"
)

// mcpCmd is a child of the kong root. There is no default subcommand: a bare
// `pix mcp` is an incomplete invocation (exit 2), as it always was.
func (c *mcpCmd) Help() string {
	return `Wire MCP servers into the sbx gateway, and into a running sandbox.

Registration is HOST state: it makes a server known to the gateway, which is
not the same as a sandbox seeing its tools. A session sees a server's tools
only once it was preloaded at create ('pix run --replace') or attached live
('pix mcp load'). 'pix status'/'pix doctor' report what is actually live.

Remote catalog servers (notion/atlassian/granola) come from 'pix mcp bundle',
then 'pix mcp auth --all'.`
}

type mcpCmd struct {
	Register mcpRegisterCmd `cmd:"" help:"Register local stdio MCP servers with the gateway. (WRITES)"`
	Ls       mcpLsCmd       `cmd:"" help:"List servers registered with the gateway (host state, not your sandbox)."`
	Load     mcpLoadCmd     `cmd:"" help:"Attach a registered server to the RUNNING sandbox — live, no recreate. (WRITES)"`
	Auth     mcpAuthCmd     `cmd:"" passthrough:"" help:"Authorize remote OAuth servers (sbx mcp auth; e.g. --all)."`
	Bundle   mcpBundleCmd   `cmd:"" passthrough:"" help:"Register the shipped public catalog bundle. (WRITES)"`
}

// mcpFailed is the ONE mapping from an mcp failure to an exit code: it prints
// in the verb's own words and returns a SilentError, so the root does not
// report the same cause twice.
func mcpFailed(d *cli.Deps, sub string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mcp.ErrSbxUnavailable) {
		// McpWouldRun already printed the recovery command to stdout.
		return cli.SilentError{Code: rpc.ExitServiceDown}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// The child already said what was wrong in its own words.
		return cli.SilentError{Code: exit.ExitCode()}
	}
	fmt.Fprintf(d.Err, "pix mcp %s: %v\n", sub, err)
	return cli.SilentError{Code: 1}
}

// mcpRegisterCmd registers the requested local stdio servers (default: the
// local ones in cfg.MCP) — `make mcp-register` without the repo.
type mcpRegisterCmd struct {
	Names []string `arg:"" optional:"" help:"Servers to register (default: every local server in the resolved mcp list)."`
}

func (c *mcpRegisterCmd) Run(d *cli.Deps) error {
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return mcpFailed(d, "register", fmt.Errorf("loading config: %w", err))
	}
	env := defaultShellEnv()
	return mcpFailed(d, "register", registerServers(cfg, env, d.Out, c.Names, launcher.FindHostBinary, pack.ActiveContainerMCP(cfg)))
}

// mcpLsCmd shells `sbx mcp ls`, degrading honestly when sbx is absent (e.g.
// inside the sandbox): the caller asked for gateway state and got none, so
// that is exit 3, never a quiet success implying "zero servers". Extra args
// forward verbatim, and suppress the plain-text attachment note so a script
// parsing structured output never has to filter out prose.
type mcpLsCmd struct {
	Args []string `arg:"" optional:"" passthrough:"all" help:"Forwarded verbatim to 'sbx mcp ls'."`
}

func (c *mcpLsCmd) Run(d *cli.Deps) error {
	return mcpFailed(d, "ls", mcp.RunMcpLsCore(exec.LookPath, d.Out, d.In, d.Err, c.Args...))
}

// mcpLoadCmd attaches an ALREADY-REGISTERED server to the RUNNING sandbox for
// DIR. Connected agents see the new tools immediately (MCP tools/list_changed).
type mcpLoadCmd struct {
	Name string `arg:"" help:"A server already registered with the gateway."`
	Dir  string `arg:"" optional:"" default:"." help:"Workspace whose sandbox to attach to (default: cwd)."`
}

func (c *mcpLoadCmd) Run(d *cli.Deps) error {
	// Arity is kong's; what survives is the VALIDATION a parser cannot do — a
	// blank name, and a workspace `pix run` would refuse.
	name, ws, err := mcp.ParseMcpLoadArgs(mcpLoadArgs(c.Name, c.Dir))
	if err != nil {
		return cli.Usagef("mcp load: %v", err)
	}
	// The sandbox name is resolved ONLY from a validated workspace, through the
	// hardened resolver: a sandbox created with a CUSTOM name is the one loaded
	// into, and an ambiguous or untrustworthy mapping REFUSES rather than
	// targeting an arbitrary box.
	sandbox, rerr := mcp.ResolveMcpLoadSandbox(ws)
	if rerr != nil {
		return cli.Usagef("mcp load: %v", rerr)
	}
	if _, err := exec.LookPath("sbx"); err != nil {
		// A command that promises to attach a server must not exit 0 having done
		// nothing (mcp.ErrSbxUnavailable). McpWouldRun preserves the exact
		// recovery command; no load receipt is reached on this path.
		return mcpFailed(d, "load", mcp.McpWouldRun(d.Out, "mcp", "load", name, "--sandbox", sandbox))
	}
	cmd := exec.Command("sbx", "mcp", "load", name, "--sandbox", sandbox)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, d.Out, d.Err
	// S03: append the load receipt ONLY after this exact `sbx mcp load` exec has
	// itself succeeded — never before, never on a failed load.
	err = mcp.ExecSbxMcpLoadAndRecord(cmd, sandbox, name)
	var recErr *workspace.ReceiptRecordError
	if errors.As(err, &recErr) {
		// The attach succeeded, only the local receipt failed: report that
		// distinctly and exit non-zero, so doctor/status degrade honestly
		// instead of trusting a record that was never written.
		fmt.Fprintf(d.Err, "pix mcp load: %v\n", recErr)
		fmt.Fprintln(d.Err, "the server IS attached to the running sandbox; only pix's local record of it failed to write; retry `pix mcp load`, or check state-dir permissions.")
		return cli.SilentError{Code: 1}
	}
	return mcpFailed(d, "load", err)
}

// mcpLoadArgs rebuilds the pair ParseMcpLoadArgs validates — the shared
// statement of the load contract; kong took only the counting from it.
func mcpLoadArgs(name, dir string) []string {
	if dir == "" || dir == "." {
		return []string{name}
	}
	return []string{name, dir}
}

// mcpAuthCmd is a thin passthrough to `sbx mcp auth <args...>`: the hosted
// control-plane OAuth flow, without the Makefile.
type mcpAuthCmd struct {
	Args []string `arg:"" optional:"" passthrough:"all" help:"Forwarded verbatim (e.g. --all, notion, status --all, rm notion)."`
}

func (c *mcpAuthCmd) Run(d *cli.Deps) error {
	return mcpFailed(d, "auth", mcp.RunSbxMcpCore(exec.LookPath, d.Out, d.In, d.Err, append([]string{"mcp", "auth"}, c.Args...)))
}

// mcpBundleCmd manages the shipped public MCP catalog bundle. Bare (or `add`)
// registers the pinned set matching this build; anything else forwards
// verbatim to `sbx mcp bundle`.
type mcpBundleCmd struct {
	Args []string `arg:"" optional:"" passthrough:"all" help:"add (default) | ls | rm ... — forwarded to 'sbx mcp bundle'."`
}

func (c *mcpBundleCmd) Run(d *cli.Deps) error {
	sbxArgs := append([]string{"mcp", "bundle"}, c.Args...)
	if len(c.Args) == 0 || (len(c.Args) == 1 && c.Args[0] == "add") {
		sbxArgs = []string{"mcp", "bundle", "add", mcp.McpCatalogBundleName, "--url", mcp.McpCatalogBundleURL(version)}
	}
	return mcpFailed(d, "bundle", mcp.RunSbxMcpCore(exec.LookPath, d.Out, d.In, d.Err, sbxArgs))
}
