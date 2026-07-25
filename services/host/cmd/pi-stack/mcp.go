package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"pi-stack/host/config"
)

// mcpCatalogNames is the SINGLE public source of truth for the shipped MCP
// catalog bundle (config/mcp-catalog.bundle.json): exactly the servers
// `pi-stack mcp bundle` registers. Both onboarding's mcp allowlist
// (validateOnboardingResult) and classifyMCP's remote-vs-custom split reuse
// this set, so a plausible-looking but unshipped name (e.g. "linear") can
// never be treated as a confirmed remote-catalog server -- that would
// recommend `pi-stack mcp bundle` as a repair command that silently can't
// register it. mcp_catalog_test.go anti-drift-parses the bundle JSON itself
// against this map, so it can never silently diverge from what the bundle
// actually ships.
var mcpCatalogNames = map[string]bool{
	"notion":    true,
	"atlassian": true,
	"granola":   true,
}

// mcpCatalogSummary renders mcpCatalogNames as a stable, sorted,
// human-readable list (e.g. "atlassian, granola, notion") for help/detail
// text, so a doc string never has to hand-enumerate the catalog and risk
// drifting from it.
func mcpCatalogSummary() string {
	names := make([]string, 0, len(mcpCatalogNames))
	for n := range mcpCatalogNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "/")
}

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

// gogHardenedArgv builds the EXACT hardened gog invocation used both when
// registering gog with the sbx gateway (mcpRegistrar.serverCmd) and when
// probing it directly (gog_setup.go's bare headless probe, R1-06) — a single
// definition so a direct probe can never silently drift from what actually
// gets registered. gogBin is normally the canonical PATH-resolved gog binary.
func gogHardenedArgv(gogBin, account string) []string {
	return []string{
		gogBin,
		"--account", account,
		"--gmail-no-send",
		"--wrap-untrusted",
		"--readonly",
		"mcp",
		"--allow-tool", "read",
	}
}

// serverCmd is the bare command+args the gateway must ultimately spawn for one
// server (before any op-run wrapping): gog with its hardened flags, or a
// pi-stack-host subcommand (slack + friends).
func (m mcpRegistrar) serverCmd(name string) []string {
	switch name {
	case "gog":
		return gogHardenedArgv(m.gog, m.account)
	default:
		// slack + any other local stdio server is a pi-stack-host subcommand.
		return []string{m.hostBin, "mcp", name}
	}
}

// execArgv returns the EXACT, literal command line the sbx gateway will exec to
// spawn server `name` — serverCmd's bare invocation, wrapped in
// `op run --no-masking --env-file=<refs> -- <cmd…>` when m.opRefs is present so
// creds resolve from 1Password at gateway spawn time (needed for slack's token +
// gog's headless keyring password), or returned bare when m.opRefs is empty —
// 1Password is optional, so a no-creds server (a future `pio`) still registers,
// and a creds server just runs without injected creds until an op-refs.env is
// added — never a hard failure.
//
// This is the single source of truth for "what will actually run": addArgs
// below re-encodes it into sbx's --command/--args form, and gogRegisteredArgv
// calls it directly so a probe of the real headless path can never drift from
// what gets registered (finding #2).
func (m mcpRegistrar) execArgv(name string) []string {
	cmd := m.serverCmd(name)
	if m.opRefs == "" {
		return cmd
	}
	wrapped := []string{m.op, "run", "--no-masking", "--env-file=" + m.opRefs, "--"}
	return append(wrapped, cmd...)
}

// addArgs builds the `sbx mcp add <name> …` argv for one server: execArgv's
// literal command line, re-encoded as --command <argv[0]> --args <argv[1]> ...
func (m mcpRegistrar) addArgs(name string) []string {
	cmd := m.execArgv(name)
	args := []string{"mcp", "add", name, "--command", cmd[0]}
	for _, c := range cmd[1:] {
		args = append(args, "--args", c)
	}
	return args
}

// gogRegisteredArgv returns the EXACT argv the sbx gateway will exec for gog,
// given resolved binaries/refs/account — literally mcpRegistrar{...}.execArgv
// ("gog") for the same inputs registerServers would use. gog_setup.go's
// pre-commit headless verification and doctor's best-effort reconstruction
// fallback (when sbx's own registration can't be read) both call THIS, so
// neither can silently probe lighter flags/wrapper than what registration will
// actually run (finding #2). Pass opBin/opRefs both "" for a bare (no
// 1Password) probe — mirrors registerServers' opReady gate, where op and
// op-refs are only ever used together.
func gogRegisteredArgv(gogBin, opBin, opRefs, account string) []string {
	return mcpRegistrar{gog: gogBin, account: account, op: opBin, opRefs: opRefs}.execArgv("gog")
}

// gogBareRegistrationNote is the ONE shared message printed whenever gog is
// registered without an op-refs.env wrapper (1Password not configured): gog
// authenticates via its own OAuth flow, never op-refs, so this points at the
// guided `pi-stack gog setup` recovery path — never the raw `gog auth login`
// recipe (finding #2) — rather than the (irrelevant, for gog) op-refs seed.
func gogBareRegistrationNote(out io.Writer) {
	fmt.Fprintln(out, "note: registered gog directly (bare); gog authenticates via its own OAuth flow — "+
		"if it ever needs re-authorizing, run: pi-stack gog setup")
}

// buildGogRegistrar resolves op + op-refs EXACTLY ONCE and pairs them with the
// already-resolved gogPath/account into an IMMUTABLE mcpRegistrar snapshot
// (R3, finding #1). gogSetup calls this a single time, before its headless
// verification probe; the returned reg is then used, UNCHANGED, both to build
// the probe's argv (reg.execArgv("gog")) and to register it
// (registerGogRegistrar -> reg.addArgs("gog")) — no re-resolution of
// op/op-refs/gog happens between the two, so a PATH or op-refs.env mutation in
// that window can never register a different command than the one that was
// just proven healthy.
func buildGogRegistrar(env shellEnv, gogPath, account string) mcpRegistrar {
	lookPath := env.lookPath
	if lookPath == nil {
		lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }
	}
	reg := mcpRegistrar{gog: gogPath, account: account}
	opPath, opErr := lookPath("op")
	opRefs := resolveOpRefs(env)
	if opErr == nil && opRefs != "" {
		reg.op = opPath
		reg.opRefs = opRefs
	}
	return reg
}

// registerGogRegistrar registers gog with the sbx gateway using the EXACT,
// already-resolved reg snapshot gogSetup built via buildGogRegistrar and
// probed against — no independent re-resolution here (R3, finding #1). It
// funnels into the same runRegistrationLoop the generic registerServers path
// uses, so the actual `sbx mcp add` execution/error handling never drifts
// between the two callers.
func registerGogRegistrar(reg mcpRegistrar, env shellEnv, out io.Writer) error {
	if reg.opRefs == "" {
		gogBareRegistrationNote(out)
	}
	lookPath := env.lookPath
	if lookPath == nil {
		lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }
	}
	return runRegistrationLoop(reg, []string{"gog"}, env, out, lookPath)
}

// rawAddArgs builds a literal `sbx mcp add <name> --command <argv[0]> --args
// <argv[1]> ...` argv from an already-resolved command line (e.g. one read
// back via `sbx mcp get`). Used by gog_setup.go's registration rollback
// (R1-08) to re-register a PRIOR command exactly as it was, without having to
// reconstruct it from config.
func rawAddArgs(name string, argv []string) []string {
	if len(argv) == 0 {
		return []string{"mcp", "add", name}
	}
	args := []string{"mcp", "add", name, "--command", argv[0]}
	for _, c := range argv[1:] {
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
	requested []string, hostResolver func() (string, error)) error {

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
	var skippedUnknown []string // non-gog names skipped because the local set is unknown
	for _, n := range names {
		switch {
		case n == "gog":
			wantGog = true
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

	// The final registration order: local servers, then gog (if requested).
	var finalNames []string
	finalNames = append(finalNames, localServers...)
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

	reg := mcpRegistrar{}
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
		} else {
			// gog-only: gog authenticates via its own OAuth flow, never op-refs, so
			// do NOT seed op-refs.env or mention it. Register bare.
			gogBareRegistrationNote(out)
		}
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

	if regErr := runRegistrationLoop(reg, finalNames, env, out, lookPath); regErr != nil {
		return errors.Join(regErr, skippedErr)
	}
	return skippedErr
}

// runRegistrationLoop runs `sbx mcp add <name> ...` for every name in names,
// using the given, ALREADY fully-resolved reg — it never re-resolves op/gog/
// hostBin itself. This is the one execution path both registerServers (which
// resolves reg fresh, per generic call) and gogSetup's dedicated
// registerGogRegistrar (which resolves reg exactly once, before probing, to
// close the TOCTOU where re-resolving after the probe could register a
// different command — R3, finding #1) funnel into, so the actual `sbx mcp
// add` execution/error handling/messaging never drifts between the two.
func runRegistrationLoop(reg mcpRegistrar, names []string, env shellEnv, out io.Writer, lookPath func(string) (string, error)) error {
	_, sbxErr := lookPath("sbx")
	sbxOK := sbxErr == nil
	if !sbxOK {
		fmt.Fprintln(out, "sbx not on PATH — here is what WOULD be registered (run these on the host):")
	}

	// Accumulate per-server failures so a caller (`pi-stack mcp register`, or
	// gogSetup) exits non-zero on ANY failure, while still attempting every
	// server and printing each result.
	var regErrs []error
	for _, n := range names {
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
		return fmt.Errorf("%d server(s) failed to register: %w", len(regErrs), errors.Join(regErrs...))
	}
	return nil
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

// resolveStaticMCPForRun is the entry point run.go/task.go actually call to
// compute o.StaticMCP. It layers one more rule on top of resolveStaticMCP:
//
// configServers (cfg.MCP, the persistent config/pack list) keep the plain
// default-dynamic/mcp_static semantics unchanged.
//
// explicit (o.MCP, a per-run `--mcp NAME` CLI flag) is a ONE-RUN, explicit
// promise from the user to attach that server now, so unlike a config entry it
// defaults to EAGER (--static-mcp) rather than dynamic -- the whole point of
// typing `--mcp` on a launch is to have the tools in context for that session
// without first editing mcp_static. The only thing that can override an
// explicit --mcp back to lazy is the stronger, already-documented mcp_dynamic
// knob (a persistent, deliberate "keep this one lazy" pin) -- mcp_static has no
// say over an explicit flag since it would be redundant (the flag already means
// eager) and mcp_dynamic already outranks mcp_static in resolveStaticMCP.
//
// Order-preserving and de-duplicated across configServers then explicit, same
// as resolveStaticMCP.
func resolveStaticMCPForRun(configServers, explicit []string, cfg *config.Config) []string {
	dynamic := map[string]bool{}
	for _, n := range cfg.MCPDynamic {
		dynamic[n] = true
	}
	configStatic := map[string]bool{}
	for _, n := range cfg.MCPStatic {
		if !dynamic[n] { // mcp_dynamic wins over mcp_static, same as resolveStaticMCP
			configStatic[n] = true
		}
	}
	explicitSet := map[string]bool{}
	for _, n := range explicit {
		if n != "" {
			explicitSet[n] = true
		}
	}

	var out []string
	seen := map[string]bool{}
	all := append(append([]string(nil), configServers...), explicit...)
	for _, n := range all {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		eager := configStatic[n] || (explicitSet[n] && !dynamic[n])
		if eager {
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
// local pi-stack-host subcommand. The call goes through probeRun (bounded,
// falls back to env.run when env.probe is nil) so a doctor/status caller that
// wires a real timeout never wedges on this either (R2-02's every-discovery-
// subprocess-is-bounded posture).
func localMCPNames(env shellEnv, hostResolver func() (string, error)) (map[string]bool, bool) {
	if hostResolver == nil {
		return nil, false
	}
	hb, err := hostResolver()
	if err != nil || hb == "" {
		return nil, false
	}
	out, timedOut, err := probeRun(env, hb, "mcp", "--list")
	if err != nil || timedOut {
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

// mcpClass is the local-vs-remote-vs-unknown partition a configured MCP name
// (besides gog, which is a special local case handled separately — see
// registerServers' doc comment) falls into, as established by localMCPNames.
type mcpClass int

const (
	// mcpClassUnknown means localMCPNames itself could not be established
	// (pi-stack-host unresolved or `mcp --list` failed/timed out) — the caller
	// must NOT guess either way: no local probe/exec, no remote-catalog
	// registration guidance, evidence stays unverifiable.
	mcpClassUnknown mcpClass = iota
	// mcpClassLocal means the name is in the CONFIRMED local set (`pi-stack-host
	// mcp --list`): honest to probe/exec (mcpProbeCheck) and to recommend
	// `pi-stack mcp register` for.
	mcpClassLocal
	// mcpClassRemote means the local set is known and does NOT contain the
	// name, AND the name IS in mcpCatalogNames (the shipped public catalog
	// bundle: notion/atlassian/granola). It must never be probed/exec'd as a
	// local command, and its registration guidance is `pi-stack mcp bundle`,
	// never `pi-stack mcp register` — that command actually registers this name.
	mcpClassRemote
	// mcpClassCustom means the name is confirmed NON-local but is also NOT in
	// mcpCatalogNames (e.g. "linear", or a bespoke overlay remote server): a
	// plausible-looking gateway-catalog name that the shipped bundle simply
	// doesn't carry. It must never be probed/exec'd as local (same as remote),
	// but it ALSO must never recommend `pi-stack mcp bundle` — that command
	// only registers mcpCatalogNames and would silently no-op for this name,
	// a broken repair. There is no host-known repair command for it at all.
	mcpClassCustom
)

// classifyMCP applies the local/remote/custom/unknown partition to one
// configured name, given the (localSet, localKnown) pair localMCPNames
// returned. Callers exclude gog before reaching this — it is a special local
// case with its own dedicated check, never remote-catalog.
func classifyMCP(name string, localSet map[string]bool, localKnown bool) mcpClass {
	if !localKnown {
		return mcpClassUnknown
	}
	if localSet[name] {
		return mcpClassLocal
	}
	if mcpCatalogNames[name] {
		return mcpClassRemote
	}
	return mcpClassCustom
}

// mcpAuthResult is the outcome mcpAuthStatus classifies a `sbx mcp auth
// status <name>` probe into. authUnknown covers output doctor/status cannot
// confidently parse as either a pass or a fail — it must never guess (a
// misread failure would recommend a repair command that doesn't apply, and a
// misread success would silently hide a real auth gap).
type mcpAuthResult int

const (
	mcpAuthUnknown mcpAuthResult = iota
	mcpAuthOK
	mcpAuthFailed
)

// mcpAuthStatus parses `sbx mcp auth status <name>` output (name-scoped: sbx
// prints only this server's state) into the tri-state above. It is
// deliberately lenient about exact wording (this is a passthrough to sbx, not
// a format pi-stack controls — see runMcpAuth) but conservative about
// ambiguity: a negative phrase anywhere wins over a positive one, and neither
// present at all is unknown rather than a guess.
func mcpAuthStatus(out string) mcpAuthResult {
	lower := strings.ToLower(out)
	for _, neg := range []string{"not authenticated", "unauthenticated", "not authorized", "unauthorized", "needs auth", "not logged in", "expired", "no token"} {
		if strings.Contains(lower, neg) {
			return mcpAuthFailed
		}
	}
	for _, pos := range []string{"authenticated", "authorized", "logged in", " ok", "\tok"} {
		if strings.Contains(lower, pos) {
			return mcpAuthOK
		}
	}
	if strings.TrimSpace(lower) == "ok" {
		return mcpAuthOK
	}
	return mcpAuthUnknown
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
