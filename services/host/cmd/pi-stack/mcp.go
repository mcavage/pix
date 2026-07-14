package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// serverCmd is the bare command+args the gateway must ultimately spawn for one
// server (before any op-run wrapping): gog with its hardened flags, or a
// pi-stack-host subcommand (slack + friends).
func (m mcpRegistrar) serverCmd(name string) []string {
	switch name {
	case "gog":
		return []string{
			m.gog,
			"--account", m.account,
			"--gmail-no-send",
			"--wrap-untrusted",
			"--readonly",
			"mcp",
			"--allow-tool", "read",
		}
	default:
		// slack + any other local stdio server is a pi-stack-host subcommand.
		return []string{m.hostBin, "mcp", name}
	}
}

// addArgs builds the `sbx mcp add <name> …` argv for one server. When op-refs is
// present (m.opRefs != "") the command is wrapped in
// `op run --no-masking --env-file=<refs> -- <cmd…>` so creds resolve from
// 1Password at gateway spawn time (needed for slack's token + gog's headless
// keyring password). When op-refs is ABSENT the server (only gog reaches this
// path) is registered DIRECTLY as a bare command — 1Password is optional for gog.
func (m mcpRegistrar) addArgs(name string) []string {
	cmd := m.serverCmd(name)
	if m.opRefs == "" {
		// Bare registration: --command <cmd[0]> --args <cmd[1]> ...
		args := []string{"mcp", "add", name, "--command", cmd[0]}
		for _, c := range cmd[1:] {
			args = append(args, "--args", c)
		}
		return args
	}
	// op-run wrapper: --command <op> --args run … --args -- --args <cmd...>
	args := []string{
		"mcp", "add", name,
		"--command", m.op,
		"--args", "run",
		"--args", "--no-masking",
		"--args", "--env-file=" + m.opRefs,
		"--args", "--",
	}
	for _, c := range cmd {
		args = append(args, "--args", c)
	}
	return args
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

	// The sbx MCP gateway must be enabled (like the Makefile's mcp-register
	// target): `sbx mcp add` only works when SBX_MCP_URL points at the gateway.
	// Fail up front rather than emit a confusing per-server failure.
	if env.getenv == nil || strings.TrimSpace(env.getenv("SBX_MCP_URL")) == "" {
		return fmt.Errorf("MCP gateway not enabled: export SBX_MCP_URL=https://gateway.docker.com")
	}

	// Partition into gog (op is OPTIONAL — it only injects a headless keyring
	// password) and strict-op servers (slack + other host subcommands, whose
	// tokens MUST come from 1Password).
	wantGog := false
	var strictOp []string
	for _, n := range names {
		if n == "gog" {
			wantGog = true
		} else {
			strictOp = append(strictOp, n)
		}
	}

	// Resolve op + op-refs. op-refs is the file of op:// refs the wrapper resolves
	// at spawn; when both are present we wrap every server in `op run`.
	opPath, opErr := env.lookPath("op")
	opRefs := resolveOpRefs(env)
	opReady := opErr == nil && opRefs != ""

	// Strict-op servers cannot be registered without op + a filled op-refs.env.
	// Seed a template at the absolute XDG path (never a repo-relative one) and
	// tell the user exactly which refs to fill.
	if len(strictOp) > 0 && !opReady {
		return opRequiredErr(env, out, opErr, strictOp)
	}

	reg := mcpRegistrar{}
	if opReady {
		reg.op = opPath
		reg.opRefs = opRefs
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
		if !opReady {
			fmt.Fprintf(out, "note: no op-refs.env found; registered gog directly — if the gateway "+
				"can't unlock gog's keyring headlessly, add GOG_KEYRING_PASSWORD to %s (see docs/gog-setup.md)\n",
				defaultOpRefsPath(env))
		}
	}

	if len(strictOp) > 0 {
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
		if reg.opRefs != "" {
			fmt.Fprintf(out, "Each wrapped server resolves its creds from %s via op run at gateway spawn.\n", reg.opRefs)
		}
	} else {
		fmt.Fprintln(out, "note: install Docker Sandboxes (sbx) to register — https://docs.docker.com/ai/sandboxes")
	}
	return nil
}

// defaultOpRefsPath computes the absolute XDG op-refs.env path from the injected
// env (mirrors config.OpRefsPath but stays hermetic under test): $PI_STACK_CONFIG
// dir, else $XDG_CONFIG_HOME/pi-stack, else ~/.config/pi-stack — all + op-refs.env.
// This is the path repo-less hosts must create, so every user-facing message and
// the seeder reference it (never a meaningless repo-relative config/op-refs.env).
func defaultOpRefsPath(env shellEnv) string {
	if env.getenv != nil {
		if p := env.getenv("PI_STACK_CONFIG"); p != "" {
			return filepath.Join(filepath.Dir(p), "op-refs.env")
		}
		if xdg := env.getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "pi-stack", "op-refs.env")
		}
	}
	if env.homeDir != nil {
		if home := env.homeDir(); home != "" {
			return filepath.Join(home, ".config", "pi-stack", "op-refs.env")
		}
	}
	return config.OpRefsPath()
}

// opRequiredErr handles the case where a strict-op server (slack + other host
// subcommands) was requested but op/op-refs is missing. It seeds a template
// op-refs.env at the absolute XDG path (best-effort, via env.writeFile so tests
// stay hermetic) and returns an actionable error naming that path and the refs
// to fill — never a repo-relative config/op-refs.env.
func opRequiredErr(env shellEnv, out io.Writer, opErr error, strictOp []string) error {
	refsPath := defaultOpRefsPath(env)
	if opErr != nil {
		return fmt.Errorf("1Password CLI (op) not found — %s require it for their tokens; brew install 1password-cli",
			strings.Join(strictOp, ", "))
	}
	// op is present but op-refs.env is absent: seed a template so the user has a
	// concrete file to fill.
	seeded := false
	if env.writeFile != nil {
		if env.statFile == nil || !env.statFile(refsPath) {
			if err := env.writeFile(refsPath, []byte(config.OpRefsTemplate), 0o600); err == nil {
				seeded = true
			}
		}
	}
	if seeded {
		fmt.Fprintf(out, "seeded a template op-refs.env at %s\n", refsPath)
	}
	return fmt.Errorf("%s require 1Password but no filled op-refs.env was found — "+
		"fill in %s (SLACK_TOKEN=op://<vault>/<slack-item>/credential and any other server tokens), then re-run",
		strings.Join(strictOp, ", "), refsPath)
}
