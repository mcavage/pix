// mcp_cmd.go — the argv seams for `pix mcp`. Everything here owns os.Exit,
// resolves dependencies, and calls into the mcp capability; nothing here
// decides anything about MCP itself.
//
// The split is what let the capability come loose: register needed the active
// pack's containers and a real env, and load needed run's workspace
// validation. Those are composition concerns, so they belong at L4 — the
// capability now takes both as parameters.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"pix/host/cli"
	"pix/host/launcher"
	"pix/host/mcp"
	"pix/host/rpc"
	"pix/host/workspace"
)

// runMcpCmd is the `mcp` verb tree: `register [name...]` and `ls`.
func runMcpCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(mcp.McpUsage)
		return
	}
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, mcp.McpUsage)
		os.Exit(2)
	}
	switch argv[0] {
	case "register":
		runMcpRegister(argv[1:])
	case "ls":
		// Any extra args are forwarded verbatim to `sbx mcp ls` (e.g. a future
		// machine-readable format flag) rather than rejected — the plain-text
		// attachment note is skipped whenever args are present, so a script
		// parsing structured output never has to filter out prose.
		runMcpLs(argv[1:]...)
	case "load":
		runMcpLoad(argv[1:])
	case "auth":
		runMcpAuth(argv[1:])
	case "bundle":
		runMcpBundle(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pix mcp: unknown subcommand %q (want: register, ls, load, auth, bundle)\n", argv[0])
		os.Exit(2)
	}
}

// runMcpRegister is the CLI entry point: it registers the requested local stdio
// servers (or, with no args, the local ones in cfg.MCP) with the sbx gateway,
// porting `make mcp-register` so nobody needs the repo/Makefile.
func runMcpRegister(argv []string) {
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix mcp register: loading config: %v\n", err)
		os.Exit(1)
	}
	env := defaultShellEnv()
	if err := registerServers(cfg, env, os.Stdout, argv, launcher.FindHostBinary, activeContainerMCP(cfg)); err != nil {
		fmt.Fprintf(os.Stderr, "pix mcp register: %v\n", err)
		if errors.Is(err, mcp.ErrSbxUnavailable) {
			os.Exit(rpc.ExitServiceDown)
		}
		os.Exit(1)
	}
}

// runMcpBundle manages the shipped public MCP catalog bundle
// (notion/atlassian/granola) via `sbx mcp bundle`. Bare (or `add`) registers the
// pinned pix catalog in one step — the remote set that matches this build.
// `ls`/`rm` (and any other args) forward verbatim to `sbx mcp bundle`.
func runMcpBundle(argv []string) {
	var sbxArgs []string
	if len(argv) == 0 || (len(argv) == 1 && argv[0] == "add") {
		sbxArgs = []string{"mcp", "bundle", "add", mcp.McpCatalogBundleName, "--url", mcp.McpCatalogBundleURL(version)}
	} else {
		sbxArgs = append([]string{"mcp", "bundle"}, argv...)
	}
	mcp.ExitMcpVerb("mcp bundle", mcp.RunMcpBundleCore(exec.LookPath, os.Stdout, os.Stdin, os.Stderr, sbxArgs))
}

// runMcpAuth is a thin passthrough to `sbx mcp auth <args...>` — the native
// hosted-control-plane OAuth flow for remote MCP servers (notion/atlassian/…),
// so repo-less hosts get it without the Makefile. All args/subcommands forward
// verbatim: `pix mcp auth --all`, `pix mcp auth notion`,
// `pix mcp auth status --all`, `pix mcp auth rm notion`.
func runMcpAuth(argv []string) {
	mcp.ExitMcpVerb("mcp auth", mcp.RunMcpAuthCore(exec.LookPath, os.Stdout, os.Stdin, os.Stderr, argv))
}

// runMcpLoad attaches an ALREADY-REGISTERED MCP server to the RUNNING sandbox
// for DIR (default cwd) via `sbx mcp load <name> --sandbox <derived>`. Connected
// agents see the new tools immediately (MCP tools/list_changed), no recreate —
// the nightly gateway's live-attach that the old --mcp-at-create model couldn't
// do. Register first with `pix mcp register` (or `sbx mcp add`).
func runMcpLoad(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(mcp.McpUsage)
		return
	}
	name, ws, err := mcp.ParseMcpLoadArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix mcp load: %v\n\n%s", err, mcp.McpUsage)
		os.Exit(2)
	}
	// The sandbox name is resolved ONLY from a validated workspace: a usage
	// error above exits before anything is resolved, exec'd, or receipted.
	// Resolution goes through the hardened workspace->sandbox resolver so a
	// sandbox created with a CUSTOM name (`pix run --name pix-demo`)
	// is loaded into, not a same-named stranger; an ambiguous or untrustworthy
	// mapping REFUSES (exit 2) rather than targeting an arbitrary box.
	sandbox, rerr := mcp.ResolveMcpLoadSandbox(ws)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "pix mcp load: %v\n", rerr)
		os.Exit(2)
	}
	if _, err := exec.LookPath("sbx"); err != nil {
		// A command that promises to attach a server must not exit 0 having
		// done nothing — see mcp.ErrSbxUnavailable. mcp.McpWouldRun preserves the
		// exact recovery command; mcp.ExecSbxMcpLoadAndRecord (and hence the load
		// receipt) is never reached on this path.
		_ = mcp.McpWouldRun(os.Stdout, "mcp", "load", name, "--sandbox", sandbox)
		os.Exit(rpc.ExitServiceDown)
	}
	cmd := exec.Command("sbx", "mcp", "load", name, "--sandbox", sandbox)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// S03: append the load receipt ONLY after this exact `sbx mcp load` exec has
	// itself succeeded — never before, never on a failed load. A missing sbx
	// (the would-run branch above) never reaches this call at all.
	if err := mcp.ExecSbxMcpLoadAndRecord(cmd, sandbox, name); err != nil {
		var rerr *workspace.ReceiptRecordError
		if errors.As(err, &rerr) {
			// The attach itself succeeded — only the local receipt failed.
			// Report that distinctly (never a plain success) and exit non-zero
			// so doctor/status degrade honestly instead of trusting a record
			// that was never written.
			fmt.Fprintf(os.Stderr, "pix mcp load: %v\n", rerr)
			fmt.Fprintln(os.Stderr, "the server IS attached to the running sandbox; only pix's local record of it failed to write; retry `pix mcp load`, or check state-dir permissions.")
			os.Exit(1)
		}
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pix mcp load: %v\n", err)
		os.Exit(1)
	}
}

// runMcpLs shells `sbx mcp ls`, degrading cleanly when sbx is absent (e.g.
// inside the sandbox). extraArgs (if any) forward verbatim to `sbx mcp ls`.
func runMcpLs(extraArgs ...string) {
	mcp.ExitMcpVerb("mcp ls", mcp.RunMcpLsCore(exec.LookPath, os.Stdout, os.Stdin, os.Stderr, extraArgs...))
}
