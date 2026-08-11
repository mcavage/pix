package main

// mcp_cmd.go is `pix mcp`: the typed verb tree, plus the composition the mcp
// capability deliberately does not do for itself (the active pack's containers, a
// real env, the host-binary resolver). kong owns the grammar, mcpFailed the one
// exit mapping.
//
// The honesty contract is why this verb maps exits itself: a subcommand that
// PROMISES an operation must never exit 0 having done nothing. A missing sbx
// prints the command to run by hand and exits 3 (rpc.ExitServiceDown); a child's
// own exit code propagates as-is.

import (
	"errors"
	"fmt"
	"os/exec"

	"pix/host/cli"
	"pix/host/mcp"
	"pix/host/packinfo"
	"pix/host/rpc"
	"pix/host/workspace"
)

// mcpCmd is a child of the kong root. There is no default subcommand: a bare `pix
// mcp` is an incomplete invocation, exit 2.
func (c *mcpCmd) Help() string {
	return `Wire MCP servers into the sbx gateway.

Registration is HOST state: it makes a server known to the gateway, which is
not the same as a sandbox seeing its tools. A sandbox picks up registered
servers when it launches, so add first, then start (or restart) the sandbox.
Neither 'pix status' nor 'pix doctor' can see inside a running session, so they
report host registration and say so.

'add' takes three shapes:
  pix mcp add <name> --url <url>   a hosted server, by URL
  pix mcp add <name>               a server your active pack declares, built
                                   from its manifest (including the 1Password
                                   wrapper, when it declares credentials)
  pix mcp add                      every server in your config's mcp list

A hosted server usually needs 'pix mcp auth <name>' after it is added.`
}

// The surface is deliberately three verbs. It was six, and the extra three
// taught more than they did: `register` vs a native `sbx mcp add` was a
// distinction only the implementation cared about (both register a server; one
// happens to build the command for you), `bundle` was a shortcut for three
// named SaaS vendors that has no business in a public CLI, and `load` attached
// a server to a running sandbox, which is a recreate away in a stack whose
// sandboxes are disposable.
type mcpCmd struct {
	Add  mcpAddCmd  `cmd:"" help:"Register a server with the gateway. (WRITES)"`
	Ls   mcpLsCmd   `cmd:"" help:"List servers registered with the gateway (host state, not your sandbox)."`
	Auth mcpAuthCmd `cmd:"" passthrough:"" help:"Authorize remote OAuth servers (sbx mcp auth; e.g. --all)."`
}

// mcpFailed is the ONE mapping from an mcp failure to an exit code: it prints
// in the verb's own words and returns a SilentError, so the root does not
// report the same cause twice.
func mcpFailed(d *cli.Deps, sub string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mcp.ErrSbxUnavailable) {
		// McpWouldRun already printed the recovery command to stderr.
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

// mcpAddCmd is the ONE registration verb. It covers what used to be three:
// a hosted server by URL, a server the ACTIVE PACK declares (config's mcp list
// resolved against its integrations), and the shipped catalog names pix already
// knows an endpoint for. "Which verb registers my server" is not a question a
// user should have to answer.
type mcpAddCmd struct {
	Names []string `arg:"" optional:"" help:"Server name(s). Omit to register every server in the config mcp list."`
	URL   string   `help:"Register NAME as a hosted server at this URL." placeholder:"URL"`
}

func (c *mcpAddCmd) Run(d *cli.Deps) error {
	// --url describes exactly one server, so it cannot pair with a name list or
	// with the bare "everything in config" form. Caught here rather than
	// silently registering the first name at that URL.
	if c.URL != "" {
		if len(c.Names) != 1 {
			return cli.Usagef("mcp add --url: needs exactly one server name")
		}
		return mcpFailed(d, "add", mcp.AddRemoteServers(d.Out, d.Err,
			[]mcp.CatalogServer{{Name: c.Names[0], URL: c.URL}}))
	}
	// A name pix already has a URL for registers without making the user go
	// find it again. Any remaining name falls through to the builder path,
	// which is the one that can construct a credentialed local command.
	var known []mcp.CatalogServer
	var rest []string
	for _, n := range c.Names {
		if url, ok := mcp.KnownRemoteURL(n); ok {
			known = append(known, mcp.CatalogServer{Name: n, URL: url})
			continue
		}
		rest = append(rest, n)
	}
	if len(known) > 0 {
		if err := mcp.AddRemoteServers(d.Out, d.Err, known); err != nil {
			return mcpFailed(d, "add", err)
		}
		fmt.Fprintln(d.Out, "Authorize it with: pix mcp auth "+known[0].Name)
		if len(rest) == 0 {
			return nil
		}
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return mcpFailed(d, "add", fmt.Errorf("loading config: %w", err))
	}
	env := defaultShellEnv()
	return mcpFailed(d, "add", registerServers(cfg, env, d.Out, rest, packinfo.ActiveServerMCP(cfg)))
}

// mcpLsCmd shells `sbx mcp ls`, degrading honestly when sbx is absent (e.g. inside
// the sandbox): the caller asked for gateway state and got none, which is exit 3,
// never a quiet success implying "zero servers". Extra args forward verbatim and
// suppress the plain-text attachment note, so a script parsing structured output
// never has to filter out prose.
type mcpLsCmd struct {
	Args []string `arg:"" optional:"" passthrough:"all" help:"Forwarded verbatim to 'sbx mcp ls'."`
}

func (c *mcpLsCmd) Run(d *cli.Deps) error {
	return mcpFailed(d, "ls", mcp.RunMcpLsCore(exec.LookPath, d.Out, d.In, d.Err, c.Args...))
}

// mcpAuthCmd is a thin passthrough to `sbx mcp auth <args...>`: the hosted
// control-plane OAuth flow, without the Makefile.
type mcpAuthCmd struct {
	Args []string `arg:"" optional:"" passthrough:"all" help:"Forwarded verbatim (e.g. --all, notion, status --all, rm notion)."`
}

func (c *mcpAuthCmd) Run(d *cli.Deps) error {
	return mcpFailed(d, "auth", mcp.RunSbxMcpCore(exec.LookPath, d.Out, d.In, d.Err, append([]string{"mcp", "auth"}, c.Args...)))
}
