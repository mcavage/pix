package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// A name in the resolved profile's mcp list is registered with the sbx gateway
// ONLY if it is a LOCAL stdio server this host can serve. gog is a special local
// case (its serverCmd is the external Google Workspace CLI in MCP mode). Every
// other local name — slack, and any overlay server like `pio` or `fastmail` —
// registers as `pi-stack-host mcp <name>` (the serverCmd default). The set of
// local names is the SOURCE OF TRUTH from `pi-stack-host mcp --list`, so an
// overlay adds a private MCP server just by having its bridge listed there. A
// name in cfg.MCP that is NEITHER gog NOR local is a remote gateway-catalog
// server (notion/atlassian/…): it is attached a different way, so registration
// SKIPS it with an info line rather than wrongly registering it as local.

// runMcpCmd is the `mcp` verb tree: `register [name...]` and `ls`.
func runMcpCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(mcpUsage)
		return
	}
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, mcpUsage)
		os.Exit(2)
	}
	switch argv[0] {
	case "register":
		runMcpRegister(argv[1:])
	case "ls":
		if len(argv) > 1 {
			fmt.Fprintf(os.Stderr, "pi-stack mcp ls: unexpected argument %q\n\n%s", argv[1], mcpUsage)
			os.Exit(2)
		}
		runMcpLs()
	case "load":
		runMcpLoad(argv[1:])
	case "auth":
		runMcpAuth(argv[1:])
	case "bundle":
		runMcpBundle(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pi-stack mcp: unknown subcommand %q (want: register, ls, load, auth, bundle)\n", argv[0])
		os.Exit(2)
	}
}

// runMcpBundle manages the shipped public MCP catalog bundle
// (notion/atlassian/granola) via `sbx mcp bundle`. Bare (or `add`) registers the
// pinned pi-stack catalog in one step — the remote set that matches this build.
// `ls`/`rm` (and any other args) forward verbatim to `sbx mcp bundle`.
func runMcpBundle(argv []string) {
	var sbxArgs []string
	if len(argv) == 0 || (len(argv) == 1 && argv[0] == "add") {
		sbxArgs = []string{"mcp", "bundle", "add", mcpCatalogBundleName, "--url", mcpCatalogBundleURL(version)}
	} else {
		sbxArgs = append([]string{"mcp", "bundle"}, argv...)
	}
	if _, err := exec.LookPath("sbx"); err != nil {
		fmt.Printf("sbx not on PATH — would run: sbx %s (run it on the host)\n", strings.Join(sbxArgs, " "))
		return
	}
	cmd := exec.Command("sbx", sbxArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack mcp bundle: %v\n", err)
		os.Exit(1)
	}
}

// runMcpAuth is a thin passthrough to `sbx mcp auth <args...>` — the native
// hosted-control-plane OAuth flow for remote MCP servers (notion/atlassian/…),
// so repo-less hosts get it without the Makefile. All args/subcommands forward
// verbatim: `pi-stack mcp auth --all`, `pi-stack mcp auth notion`,
// `pi-stack mcp auth status --all`, `pi-stack mcp auth rm notion`.
func runMcpAuth(argv []string) {
	if _, err := exec.LookPath("sbx"); err != nil {
		fmt.Printf("sbx not on PATH — would run: sbx mcp auth %s (run it on the host)\n", strings.Join(argv, " "))
		return
	}
	cmd := exec.Command("sbx", append([]string{"mcp", "auth"}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack mcp auth: %v\n", err)
		os.Exit(1)
	}
}

// runMcpLoad attaches an ALREADY-REGISTERED MCP server to the RUNNING sandbox
// for DIR (default cwd) via `sbx mcp load <name> --sandbox <derived>`. Connected
// agents see the new tools immediately (MCP tools/list_changed), no recreate —
// the nightly gateway's live-attach that the old --mcp-at-create model couldn't
// do. Register first with `pi-stack mcp register` (or `sbx mcp add`).
func runMcpLoad(argv []string) {
	if len(argv) == 0 || wantsHelp(argv) {
		fmt.Fprint(os.Stderr, mcpUsage)
		os.Exit(2)
	}
	name := argv[0]
	ws := "."
	if len(argv) > 1 {
		ws = argv[1]
	}
	sandbox := deriveSandboxName(ws)
	if _, err := exec.LookPath("sbx"); err != nil {
		fmt.Printf("sbx not on PATH — would run: sbx mcp load %s --sandbox %s (run it on the host)\n", name, sandbox)
		return
	}
	cmd := exec.Command("sbx", "mcp", "load", name, "--sandbox", sandbox)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack mcp load: %v\n", err)
		os.Exit(1)
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
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack mcp register: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := registerServers(cfg, defaultShellEnv(), os.Stdout, argv, findHostBinary, activeContainerMCP(cfg)); err != nil {
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
	// containers maps a server name to its pack CONTAINER spec (Manifest or Image).
	// A Manifest name registers via `--local --url` (gateway resolves the OCI
	// image; creds Docker-side; never op-run wrapped). An Image name registers as
	// an op-run-wrapped `docker run <image>` (creds from op-refs forwarded via -e),
	// exactly like a local stdio server otherwise.
	containers map[string]packContainer
}

// serverCmd is the bare command+args the gateway must ultimately spawn for one
// server (before any op-run wrapping): gog with its hardened flags, or a
// pi-stack-host subcommand (slack + friends).
func (m mcpRegistrar) serverCmd(name string) []string {
	// Image container: the bare command is `docker run -i --rm -e <KEY>… <image>`.
	// addArgs op-run wraps it (when op-refs is present), so op resolves each KEY
	// from 1Password and `-e KEY` forwards it into the container.
	if c := m.containers[name]; c.Image != "" {
		argv := []string{"docker", "run", "-i", "--rm"}
		for _, k := range c.EnvKeys {
			argv = append(argv, "-e", k)
		}
		return append(argv, c.Image)
	}
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
// keyring password). When op-refs is ABSENT every local server is registered
// DIRECTLY as a bare command — 1Password is optional, so a no-creds server (a
// future `pio`) still registers, and a creds server just runs without injected
// creds until an op-refs.env is added (harmless: the op-run wrapper is a no-op
// for a server that needs no creds).
func (m mcpRegistrar) addArgs(name string) []string {
	// Manifest container: register the OCI server by manifest, run locally by the
	// gateway via Docker. No op-run wrap — its creds are provided Docker-side
	// (declared in the server's server.json), never through op-refs. (Image
	// containers fall through to serverCmd + the op-run wrapper below.)
	if c := m.containers[name]; c.Manifest != "" {
		return []string{"mcp", "add", name, "--local", "--url", c.Manifest}
	}
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
// every entry in the resolved profile's cfg.MCP (gog via its special path, every
// other name as `pi-stack-host mcp <name>`). It fails with a clear, actionable
// message rather than registering a junk command (op/gog missing, gog account
// unset). When sbx is absent it prints exactly what it WOULD run instead of
// crashing. hostResolver locates pi-stack-host (injected so tests stay
// hermetic).
func registerServers(cfg *config.Config, env shellEnv, out io.Writer,
	requested []string, hostResolver func() (string, error), containers map[string]packContainer) error {

	names := requested
	if len(names) == 0 {
		names = append(names, cfg.MCP...)
	}
	if len(names) == 0 {
		fmt.Fprintln(out, "Nothing to register: no local stdio servers requested or in config mcp.")
		fmt.Fprintln(out, "Enable one first, e.g.:  pi-stack config set mcp gog")
		return nil
	}

	// `sbx mcp add` registers against sbx's local data-plane gateway, which is
	// always available (no SBX_MCP_URL needed on nightly) — so there's no gateway
	// precondition to check here anymore.

	// Nil-safe lookPath: a partially-populated shellEnv (some tests set only
	// env.run) must degrade to "binary not found" rather than panic — the same
	// posture localMCPNames takes for a nil env.run. Every op/gog/sbx lookup below
	// goes through this.
	lookPath := env.lookPath
	if lookPath == nil {
		lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }
	}

	// The set of names this host can serve locally is the source of truth
	// (`pi-stack-host mcp --list`). gog is always a valid local special case even
	// though the bridge never lists it. Partition the requested names into gog,
	// confirmed-local servers, and remote gateway-catalog servers to SKIP.
	localSet, localKnown := localMCPNames(env, hostResolver)
	wantGog := false
	var localServers []string
	var containerServers []string // pack CONTAINER integrations (--local --url manifest)
	var skippedUnknown []string   // non-gog names skipped because the local set is unknown
	for _, n := range names {
		switch {
		case n == "gog":
			wantGog = true
		case containers[n].Manifest != "":
			// Manifest container: registered by --local --url, not a host --command,
			// so it doesn't depend on the pi-stack-host local-name set.
			containerServers = append(containerServers, n)
		case containers[n].Image != "":
			// Image container: an op-run-wrapped `docker run` — behaves like a local
			// stdio server (host-registered, op-refs-backed), just a different cmd.
			localServers = append(localServers, n)
		case !localKnown:
			// FAIL CLOSED: the local-name list could NOT be established
			// (pi-stack-host unresolved or `mcp --list` failed). We must NOT assume
			// an unknown name is local — that would register a gateway-catalog name
			// (e.g. notion) as `pi-stack-host mcp notion`. Skip every non-gog name
			// with an actionable warning and fail the command below.
			fmt.Fprintf(out, "  %s: cannot determine local MCP servers "+
				"(pi-stack-host mcp --list failed); skipping %s — re-run after building pi-stack-host\n", n, n)
			skippedUnknown = append(skippedUnknown, n)
		case !localSet[n]:
			// Not gog and not a local stdio server -> a remote gateway-catalog
			// server. It is attached a different way; do not register it as local.
			fmt.Fprintf(out, "  %s: gateway-catalog server, not locally registered\n", n)
		default:
			// Confirmed local: it is in the pi-stack-host `mcp --list` set.
			localServers = append(localServers, n)
		}
	}

	// skippedErr is non-nil when a requested non-gog name was skipped because the
	// local set could not be established. It is folded into the final error so the
	// command exits non-zero rather than reporting a silent success.
	var skippedErr error
	if len(skippedUnknown) > 0 {
		skippedErr = fmt.Errorf("could not determine local MCP servers "+
			"(pi-stack-host mcp --list failed); skipped %s — build pi-stack-host, then re-run",
			strings.Join(skippedUnknown, ", "))
	}

	// The final registration order: local servers, then container servers, then
	// gog (if requested).
	var finalNames []string
	finalNames = append(finalNames, localServers...)
	finalNames = append(finalNames, containerServers...)
	if wantGog {
		finalNames = append(finalNames, "gog")
	}
	if len(finalNames) == 0 {
		if skippedErr != nil {
			return skippedErr
		}
		fmt.Fprintln(out, "Nothing to register locally: every configured mcp name is a remote gateway-catalog server.")
		return nil
	}

	// Resolve op + op-refs. op-refs is the file of op:// refs the wrapper resolves
	// at spawn; when both op and op-refs are present we wrap every server in
	// `op run`. When either is absent we register BARE (1Password is optional):
	// a no-creds server registers fine, and a creds server runs uncredentialed
	// until an op-refs.env is added — never a hard failure.
	opPath, opErr := lookPath("op")
	opRefs := resolveOpRefs(env)
	opReady := opErr == nil && opRefs != ""

	reg := mcpRegistrar{containers: containers}
	if opReady {
		reg.op = opPath
		reg.opRefs = opRefs
	}

	if !opReady {
		if len(localServers) > 0 {
			// A confirmed non-gog local stdio server (slack, an overlay `pio`, ...)
			// can actually use op-refs. Best-effort: seed a template op-refs.env at
			// the absolute XDG path so the user has a concrete file to fill in later,
			// and note that we registered bare rather than failing.
			refsPath := defaultOpRefsPath(env)
			// ONE seeder: route through config.SeedOpRefsAt so the template + 0700 dir
			// / 0600 file + no-clobber rule is identical to `pi-stack setup`'s seeding.
			if created, err := config.SeedOpRefsAt(refsPath); err == nil && created {
				fmt.Fprintf(out, "seeded a template op-refs.env at %s\n", refsPath)
			}
			fmt.Fprintf(out, "note: no op-refs.env found; registered %s directly (bare, no 1Password) — "+
				"add creds to %s if a server needs them\n",
				strings.Join(finalNames, ", "), refsPath)
		} else if wantGog {
			// gog-only: gog authenticates via OAuth (gog auth login), never op-refs,
			// so do NOT seed op-refs.env or mention it. Register bare.
			fmt.Fprintln(out, "note: registered gog directly (bare); gog authenticates via OAuth (gog auth login)")
		}
		// container-only: nothing to seed — container creds are Docker-side, not op-refs.
	}

	if wantGog {
		gogPath, err := lookPath("gog")
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

	if len(localServers) > 0 {
		hb, err := hostResolver()
		if err != nil {
			return fmt.Errorf("pi-stack-host not found (needed for non-gog servers): %v", err)
		}
		reg.hostBin = hb
	}

	_, sbxErr := lookPath("sbx")
	sbxOK := sbxErr == nil
	if !sbxOK {
		fmt.Fprintln(out, "sbx not on PATH — here is what WOULD be registered (run these on the host):")
	}

	// Accumulate per-server failures so `pi-stack mcp register` exits non-zero on
	// ANY failure, while still attempting every server and printing each result.
	var regErrs []error
	for _, n := range finalNames {
		args := reg.addArgs(n)
		if !sbxOK {
			fmt.Fprintf(out, "  sbx %s\n", strings.Join(args, " "))
			continue
		}
		if _, err := env.run("sbx", args...); err != nil {
			fmt.Fprintf(out, "  FAILED to register: %s (%v)\n", n, err)
			regErrs = append(regErrs, fmt.Errorf("%s: %v", n, err))
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
	if len(regErrs) > 0 {
		return errors.Join(fmt.Errorf("%d server(s) failed to register: %w", len(regErrs), errors.Join(regErrs...)), skippedErr)
	}
	return skippedErr
}

// resolveStaticMCP returns, order-preserving and de-duplicated, the subset of
// `servers` to attach EAGERLY at create (emitted to sbx as --static-mcp; their
// tools sit in context from the start).
//
// The DEFAULT is DYNAMIC for every registered server, regardless of kind: once a
// server is registered (`pi-stack mcp register` / `sbx mcp add`) it's behind the
// local gateway, and the in-VM agent discovers + calls it on demand via
// mcp-find/mcp-exec/code-mode — the daemon spawns local stdio servers host-side
// (with their op-run creds) exactly as it serves remotes, so local vs remote is
// irrelevant here. This keeps heavy tool schemas out of context until needed.
//
// Two per-server override knobs (see cfg.MCPStatic/MCPDynamic): a server in
// MCPStatic is pinned eager; MCPDynamic wins if a server is somehow in both
// (explicit lazy beats explicit eager). No env/host probe needed — this is a
// pure eager-vs-lazy choice, not a local-vs-remote one.
func resolveStaticMCP(servers []string, cfg *config.Config) []string {
	static := map[string]bool{}
	for _, n := range cfg.MCPStatic {
		static[n] = true
	}
	for _, n := range cfg.MCPDynamic {
		delete(static, n) // MCPDynamic wins over MCPStatic
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range servers {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		if static[n] {
			out = append(out, n)
		}
	}
	return out
}

// localMCPNames asks the pi-stack-host binary which MCP servers it can serve
// locally (`pi-stack-host mcp --list`), the source of truth for local vs remote
// registration. It returns the set of names and whether the list was obtained.
// A missing binary or a failed call returns (nil,false); the caller then FAILS
// CLOSED — it registers only gog (a known special case) and SKIPS every other
// requested name rather than risk registering a remote gateway-catalog name as a
// local pi-stack-host subcommand.
func localMCPNames(env shellEnv, hostResolver func() (string, error)) (map[string]bool, bool) {
	if env.run == nil || hostResolver == nil {
		return nil, false
	}
	hb, err := hostResolver()
	if err != nil || hb == "" {
		return nil, false
	}
	out, err := env.run(hb, "mcp", "--list")
	if err != nil {
		return nil, false
	}
	set := map[string]bool{}
	for _, ln := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(ln); n != "" {
			set[n] = true
		}
	}
	return set, true
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
