package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"pi-stack/host/config"
)

// localStdioMCP are the MCP servers pi-stack can register itself: local stdio
// servers the sbx gateway runs via `op run … -- <cmd>`. gog is the Google
// Workspace CLI's MCP mode; slack is a pi-stack-host subcommand. Everything else
// in cfg.MCP is a remote gateway-catalog server registered a different way, so
// `pi-stack mcp register` skips it.
var localStdioMCP = map[string]bool{"gog": true, "slack": true}

// runMcpCmd is the `mcp` verb tree: `register [name...]` and `ls`.
func runMcpCmd(argv []string) {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pi-stack mcp <register|ls> [name...]")
		os.Exit(2)
	}
	switch argv[0] {
	case "register":
		runMcpRegister(argv[1:])
	case "ls":
		runMcpLs()
	default:
		fmt.Fprintf(os.Stderr, "pi-stack mcp: unknown subcommand %q (want: register, ls)\n", argv[0])
		os.Exit(2)
	}
}

// runMcpLs shells `sbx mcp ls`, degrading cleanly when sbx is absent (e.g.
// inside the sandbox).
func runMcpLs() {
	if _, err := exec.LookPath("sbx"); err != nil {
		fmt.Println("sbx not on PATH — would run: sbx mcp ls (run it on the host)")
		return
	}
	cmd := exec.Command("sbx", "mcp", "ls")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack mcp ls: %v\n", err)
		os.Exit(1)
	}
}

// runMcpRegister is the CLI entry point: it registers the requested local stdio
// servers (or, with no args, the local ones in cfg.MCP) with the sbx gateway,
// porting `make mcp-register` so nobody needs the repo/Makefile.
func runMcpRegister(argv []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack mcp register: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := registerServers(cfg, defaultShellEnv(), os.Stdout, argv, findHostBinary); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack mcp register: %v\n", err)
		os.Exit(1)
	}
}

// mcpRegistrar carries the resolved ABSOLUTE paths + account needed to build a
// `sbx mcp add` command. The gateway daemon's PATH may not include op/gog, so
// every binary is registered by absolute path (matching the Makefile).
type mcpRegistrar struct {
	op      string // absolute op (1Password CLI)
	opRefs  string // absolute config/op-refs.env
	gog     string // absolute gog (only needed to register gog)
	account string // gog --account value
	hostBin string // absolute pi-stack-host (for slack + other host subcommands)
}

// addArgs builds the `sbx mcp add <name> …` argv for one server. Every server
// is wrapped in `op run --no-masking --env-file=<refs> -- <cmd…>` so creds
// resolve from 1Password at gateway spawn time (nothing is stored in the
// registration). Mirrors the hardened lines in the Makefile mcp-register target.
func (m mcpRegistrar) addArgs(name string) []string {
	base := []string{
		"mcp", "add", name,
		"--command", m.op,
		"--args", "run",
		"--args", "--no-masking",
		"--args", "--env-file=" + m.opRefs,
		"--args", "--",
	}
	switch name {
	case "gog":
		return append(base,
			"--args", m.gog,
			"--args", "--account", "--args", m.account,
			"--args", "--gmail-no-send",
			"--args", "--wrap-untrusted",
			"--args", "--readonly",
			"--args", "mcp",
			"--args", "--allow-tool", "--args", "read",
		)
	default:
		// slack + any other local stdio server is a pi-stack-host subcommand.
		return append(base,
			"--args", m.hostBin,
			"--args", "mcp",
			"--args", name,
		)
	}
}

// registerServers resolves + guards + builds + runs the `sbx mcp add` commands
// for the requested local stdio servers. With no requested names it registers
// the local ones (gog/slack) found in cfg.MCP. It fails with a clear, actionable
// message rather than registering a junk command (op/gog missing, gog account
// unset). When sbx is absent it prints exactly what it WOULD run instead of
// crashing. hostResolver locates pi-stack-host (injected so tests stay
// hermetic).
func registerServers(cfg *config.Config, env shellEnv, out io.Writer,
	requested []string, hostResolver func() (string, error)) error {

	names := requested
	if len(names) == 0 {
		for _, m := range cfg.MCP {
			if localStdioMCP[m] {
				names = append(names, m)
			}
		}
	}
	if len(names) == 0 {
		fmt.Fprintln(out, "Nothing to register: no local stdio servers (gog, slack) requested or in config mcp.")
		fmt.Fprintln(out, "Enable one first, e.g.:  pi-stack config set mcp gog")
		return nil
	}

	// op is the wrapper for every local server, so it is required first.
	opPath, err := env.lookPath("op")
	if err != nil {
		return fmt.Errorf("1Password CLI (op) not found — brew install 1password-cli")
	}

	// op-refs.env holds the op:// refs the wrapper resolves at spawn. Registering
	// without it would wire a command that can't authenticate.
	opRefs := resolveOpRefs(env)
	if opRefs == "" {
		return fmt.Errorf("config/op-refs.env not found — create it " +
			"(cp config/op-refs.env.example config/op-refs.env) and fill in your refs")
	}

	reg := mcpRegistrar{op: opPath, opRefs: opRefs}

	wantGog, wantHost := false, false
	for _, n := range names {
		if n == "gog" {
			wantGog = true
		} else {
			wantHost = true
		}
	}

	if wantGog {
		gogPath, err := env.lookPath("gog")
		if err != nil {
			return fmt.Errorf("gog is requested but gog not found — brew install gog")
		}
		account := strings.TrimSpace(cfg.GogAccount)
		if account == "" {
			return fmt.Errorf("gog is requested but no account is set — " +
				"run: pi-stack config set gog_account <you@example.com>")
		}
		reg.gog = gogPath
		reg.account = account
	}

	if wantHost {
		hb, err := hostResolver()
		if err != nil {
			return fmt.Errorf("pi-stack-host not found (needed for non-gog servers): %v", err)
		}
		reg.hostBin = hb
	}

	_, sbxErr := env.lookPath("sbx")
	sbxOK := sbxErr == nil
	if !sbxOK {
		fmt.Fprintln(out, "sbx not on PATH — here is what WOULD be registered (run these on the host):")
	}

	for _, n := range names {
		args := reg.addArgs(n)
		if !sbxOK {
			fmt.Fprintf(out, "  sbx %s\n", strings.Join(args, " "))
			continue
		}
		if _, err := env.run("sbx", args...); err != nil {
			fmt.Fprintf(out, "  FAILED to register: %s (%v)\n", n, err)
		} else {
			fmt.Fprintf(out, "  registered: %s\n", n)
		}
	}

	if sbxOK {
		fmt.Fprintln(out, "Each server resolves its creds from config/op-refs.env via op run at gateway spawn.")
	} else {
		fmt.Fprintln(out, "note: install Docker Sandboxes (sbx) to register — https://docs.docker.com/ai/sandboxes")
	}
	return nil
}
