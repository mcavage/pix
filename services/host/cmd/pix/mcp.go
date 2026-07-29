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

	"pix/host/config"
)

// mcpCatalogNames is the SINGLE public source of truth for the shipped MCP
// catalog bundle (config/mcp-catalog.bundle.json): exactly the servers
// `pix mcp bundle` registers. classifyMCPServer's remote-vs-custom split
// reuses this set, so a plausible-looking but unshipped name (e.g. "linear")
// can never be treated as a confirmed catalog server — that would recommend
// `pix mcp bundle` as a repair command that silently can't register it.
var mcpCatalogNames = map[string]bool{
	"notion":    true,
	"atlassian": true,
	"granola":   true,
}

// mcpCatalogSummary renders mcpCatalogNames as a stable, sorted,
// human-readable list (e.g. "atlassian/granola/notion") for help/detail text,
// so a doc string never has to hand-enumerate the catalog and risk drifting
// from it.
func mcpCatalogSummary() string {
	names := make([]string, 0, len(mcpCatalogNames))
	for n := range mcpCatalogNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "/")
}

// errSbxUnavailable is the sentinel every mcp subcommand that PROMISES an
// operation (register/load/auth/bundle) returns when sbx isn't on PATH,
// instead of silently exiting 0 after only printing what it would have run.
// It maps to exitServiceDown (3) — the same "evidence/dependency unavailable"
// code `pix memory`/`secret` use — never a bare exit 0. `pix mcp ls`
// is read-only but is deliberately held to the SAME contract (documented
// here, the one place this policy is decided): the caller asked for gateway
// state and got none, so a truthful exit code beats a quiet success that
// implies "zero servers registered."
var errSbxUnavailable = fmt.Errorf("sbx not on PATH")

// mcpWouldRun prints the exact host command a user can run manually (the
// recovery path every mutating mcp subcommand must preserve verbatim) and
// returns errSbxUnavailable so the caller exits non-zero. Centralized so
// register/ls/load/auth/bundle can never phrase or exit-code this differently
// from one another.
func mcpWouldRun(out io.Writer, args ...string) error {
	fmt.Fprintf(out, "sbx not on PATH; would run: sbx %s (run it on the host)\n", strings.Join(args, " "))
	return errSbxUnavailable
}

// exitMcpVerb is the shared exit dispatcher for the mcp subcommands that were
// refactored to return an error instead of calling os.Exit deep inside their
// logic (so tests can drive the core hermetically and only the outer wrapper
// touches the process). errSbxUnavailable -> exitServiceDown (3); a captured
// child exit code is propagated as-is; anything else is a generic failure (1)
// with the error printed once.
func exitMcpVerb(ctx string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, errSbxUnavailable) {
		os.Exit(exitServiceDown)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	fmt.Fprintf(os.Stderr, "pix %s: %v\n", ctx, err)
	os.Exit(1)
}

// A name in the resolved profile's mcp list is registered with the sbx gateway
// ONLY if it is a LOCAL stdio server this host can serve. gog is a special local
// case (its serverCmd is the external Google Workspace CLI in MCP mode). Every
// other local name — slack, and any other locally-servable name — registers as
// `pix-host mcp <name>` (the serverCmd default). The set of local names is
// the SOURCE OF TRUTH from `pix-host mcp --list`. A
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

// runMcpBundle manages the shipped public MCP catalog bundle
// (notion/atlassian/granola) via `sbx mcp bundle`. Bare (or `add`) registers the
// pinned pix catalog in one step — the remote set that matches this build.
// `ls`/`rm` (and any other args) forward verbatim to `sbx mcp bundle`.
func runMcpBundle(argv []string) {
	var sbxArgs []string
	if len(argv) == 0 || (len(argv) == 1 && argv[0] == "add") {
		sbxArgs = []string{"mcp", "bundle", "add", mcpCatalogBundleName, "--url", mcpCatalogBundleURL(version)}
	} else {
		sbxArgs = append([]string{"mcp", "bundle"}, argv...)
	}
	exitMcpVerb("mcp bundle", runMcpBundleCore(exec.LookPath, os.Stdout, os.Stdin, os.Stderr, sbxArgs))
}

// runMcpBundleCore is runMcpBundle's testable core: lookPath is injected so a
// test can force the sbx-absent branch hermetically (no PATH manipulation, no
// subprocess) and assert errSbxUnavailable + the printed recovery command
// without ever exec'ing anything.
func runMcpBundleCore(lookPath func(string) (string, error), out io.Writer, in io.Reader, errW io.Writer, sbxArgs []string) error {
	if _, err := lookPath("sbx"); err != nil {
		return mcpWouldRun(out, sbxArgs...)
	}
	cmd := exec.Command("sbx", sbxArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errW
	return cmd.Run()
}

// runMcpAuth is a thin passthrough to `sbx mcp auth <args...>` — the native
// hosted-control-plane OAuth flow for remote MCP servers (notion/atlassian/…),
// so repo-less hosts get it without the Makefile. All args/subcommands forward
// verbatim: `pix mcp auth --all`, `pix mcp auth notion`,
// `pix mcp auth status --all`, `pix mcp auth rm notion`.
func runMcpAuth(argv []string) {
	exitMcpVerb("mcp auth", runMcpAuthCore(exec.LookPath, os.Stdout, os.Stdin, os.Stderr, argv))
}

// runMcpAuthCore is runMcpAuth's testable core (see runMcpBundleCore).
func runMcpAuthCore(lookPath func(string) (string, error), out io.Writer, in io.Reader, errW io.Writer, argv []string) error {
	args := append([]string{"mcp", "auth"}, argv...)
	if _, err := lookPath("sbx"); err != nil {
		return mcpWouldRun(out, args...)
	}
	cmd := exec.Command("sbx", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errW
	return cmd.Run()
}

// runMcpLoad attaches an ALREADY-REGISTERED MCP server to the RUNNING sandbox
// for DIR (default cwd) via `sbx mcp load <name> --sandbox <derived>`. Connected
// agents see the new tools immediately (MCP tools/list_changed), no recreate —
// the nightly gateway's live-attach that the old --mcp-at-create model couldn't
// do. Register first with `pix mcp register` (or `sbx mcp add`).
func runMcpLoad(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(mcpUsage)
		return
	}
	name, ws, err := parseMcpLoadArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix mcp load: %v\n\n%s", err, mcpUsage)
		os.Exit(2)
	}
	// The sandbox name is resolved ONLY from a validated workspace: a usage
	// error above exits before anything is resolved, exec'd, or receipted.
	// Resolution goes through the hardened workspace->sandbox resolver so a
	// sandbox created with a CUSTOM name (`pix run --name pix-demo`)
	// is loaded into, not a same-named stranger; an ambiguous or untrustworthy
	// mapping REFUSES (exit 2) rather than targeting an arbitrary box.
	sandbox, rerr := resolveMcpLoadSandbox(ws)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "pix mcp load: %v\n", rerr)
		os.Exit(2)
	}
	if _, err := exec.LookPath("sbx"); err != nil {
		// A command that promises to attach a server must not exit 0 having
		// done nothing — see errSbxUnavailable. mcpWouldRun preserves the
		// exact recovery command; execSbxMcpLoadAndRecord (and hence the load
		// receipt) is never reached on this path.
		_ = mcpWouldRun(os.Stdout, "mcp", "load", name, "--sandbox", sandbox)
		os.Exit(exitServiceDown)
	}
	cmd := exec.Command("sbx", "mcp", "load", name, "--sandbox", sandbox)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// S03: append the load receipt ONLY after this exact `sbx mcp load` exec has
	// itself succeeded — never before, never on a failed load. A missing sbx
	// (the would-run branch above) never reaches this call at all.
	if err := execSbxMcpLoadAndRecord(cmd, sandbox, name); err != nil {
		var rerr *receiptRecordError
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

// parseMcpLoadArgs enforces `pix mcp load NAME [DIR]` EXACTLY: NAME is
// required, non-blank, and never flag-shaped (load takes no flags); at most
// one DIR may follow, and it must exist and be a directory —
// validateRunWorkspace, the SAME helper `pix run` uses, so load and run
// can never disagree about what counts as a workspace. Any violation is a
// usage error (exit 2 in the caller) BEFORE a sandbox name is derived or any
// sbx command runs, so a failed usage can never attach anything or write a
// receipt.
func parseMcpLoadArgs(argv []string) (name, ws string, err error) {
	const want = "want: pix mcp load NAME [DIR]"
	if len(argv) == 0 {
		return "", "", fmt.Errorf("missing server NAME (%s)", want)
	}
	if len(argv) > 2 {
		return "", "", fmt.Errorf("unexpected argument %q (%s)", argv[2], want)
	}
	name = strings.TrimSpace(argv[0])
	if name == "" {
		return "", "", fmt.Errorf("server NAME must not be blank (%s)", want)
	}
	if strings.HasPrefix(name, "-") {
		return "", "", fmt.Errorf("unknown flag %q — mcp load takes no flags (%s)", name, want)
	}
	ws = "."
	if len(argv) == 2 {
		ws = argv[1]
		if strings.TrimSpace(ws) == "" || strings.HasPrefix(ws, "-") {
			return "", "", fmt.Errorf("invalid DIR %q — mcp load takes no flags (%s)", ws, want)
		}
	}
	if verr := validateRunWorkspace(ws); verr != nil {
		return "", "", verr
	}
	return name, ws, nil
}

// resolveMcpLoadSandbox resolves the sandbox `mcp load` targets for a
// VALIDATED workspace via the hardened workspace->sandbox resolver
// (workspaceresolve.go): a unique trustworthy receipt mapping wins (custom
// `run --name` sandboxes), a positively-clean "no mapping" scan falls back to
// the derived default name (an old sandbox predating the Workspace field),
// and anything else — an ambiguous mapping, an untrustworthy receipt store,
// or an unresolvable state dir — returns an error so the caller REFUSES
// instead of attaching a server to an arbitrary box.
func resolveMcpLoadSandbox(ws string) (string, error) {
	dir, err := sandboxMCPStateDirFn()
	if err != nil {
		return "", fmt.Errorf("resolving pix state dir: %w", err)
	}
	res := resolveWorkspaceSandbox(dir, ws)
	switch res.Outcome {
	case workspaceSandboxMapped, workspaceSandboxDefault:
		return res.Sandbox, nil
	default: // ambiguous / untrusted — never target an arbitrary box
		return "", fmt.Errorf("cannot resolve which sandbox belongs to %s: %s", ws, res.Detail)
	}
}

// recordMcpLoadReceipt appends the load receipt for name on sandbox — called
// ONLY by execSbxMcpLoadAndRecord, after mcp.go's OWN `sbx mcp load` has
// already exec'd successfully.
func recordMcpLoadReceipt(sandbox, name string) error {
	dir, err := sandboxMCPStateDirFn()
	if err != nil {
		return &receiptRecordError{op: "mcp load", sandbox: sandbox, name: name, err: fmt.Errorf("resolving pix state dir: %w", err)}
	}
	if err := appendLoadReceipt(dir, sandbox, name, nil); err != nil {
		return &receiptRecordError{op: "mcp load", sandbox: sandbox, name: name, err: err}
	}
	return nil
}

// execSbxMcpLoadAndRecord runs cmd (the already-composed `sbx mcp load ...`
// invocation) and — ONLY when the exec itself succeeds — appends the load
// receipt for sandbox/name. Mirrors execSbxRunAndRecordCreate's ordering
// contract (run.go): a failed exec returns before any receipt write is
// attempted, and a writeCreateReceipt-equivalent failure here surfaces as
// *receiptRecordError so the caller reports "attached, but state unrecorded"
// rather than a plain success or a plain failure.
func execSbxMcpLoadAndRecord(cmd *exec.Cmd, sandbox, name string) error {
	if err := cmd.Run(); err != nil {
		return err
	}
	return recordMcpLoadReceipt(sandbox, name)
}

// runMcpLs shells `sbx mcp ls`, degrading cleanly when sbx is absent (e.g.
// inside the sandbox). extraArgs (if any) forward verbatim to `sbx mcp ls`.
func runMcpLs(extraArgs ...string) {
	exitMcpVerb("mcp ls", runMcpLsCore(exec.LookPath, os.Stdout, os.Stdin, os.Stderr, extraArgs...))
}

// runMcpLsCore is runMcpLs's testable core (see runMcpBundleCore). `mcp ls` is
// read-only, but per the errSbxUnavailable policy above it still exits 3 when
// evidence (the gateway's registered-server list) is unavailable rather than
// exiting 0 with nothing printed — a caller cannot tell "zero servers" from
// "couldn't ask" otherwise.
//
// `sbx mcp ls` output is HOST registration only — it says nothing about
// whether a registered server is actually attached to the sandbox you are
// running right now (registration and attachment are different moments: a
// server registered after a sandbox was created never retroactively appears
// in it). mcpLsAttachmentNote is appended after a plain-text listing to make
// that distinction explicit and name the next command; it is skipped for a
// machine-readable format (any `pix mcp ls` args, e.g. a future `-o json`)
// so a script parsing the output never has to filter out prose.
func runMcpLsCore(lookPath func(string) (string, error), out io.Writer, in io.Reader, errW io.Writer, extraArgs ...string) error {
	if _, err := lookPath("sbx"); err != nil {
		return mcpWouldRun(out, append([]string{"mcp", "ls"}, extraArgs...)...)
	}
	cmd := exec.Command("sbx", append([]string{"mcp", "ls"}, extraArgs...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errW
	if err := cmd.Run(); err != nil {
		return err
	}
	if len(extraArgs) == 0 {
		fmt.Fprint(out, mcpLsAttachmentNote)
	}
	return nil
}

// mcpLsAttachmentNote is the plain-text disclaimer `pix mcp ls` prints after a
// successful listing: the gateway's registration table, not a report of what
// the CURRENT sandbox has loaded. `pix status`/`pix doctor` are the surfaces
// that actually probe a live sandbox; `pix mcp load` attaches a registered
// server to one that's already running, and `pix run --replace` recreates it
// with everything in the resolved mcp list preloaded.
const mcpLsAttachmentNote = "\nNote: this is the gateway's HOST registration list, not what's attached to\n" +
	"your current sandbox. See `pix status` / `pix doctor` for what's live,\n" +
	"`pix mcp load <name>` to attach a registered server to a running sandbox,\n" +
	"or `pix run --replace` to recreate it with everything preloaded.\n"

// runMcpRegister is the CLI entry point: it registers the requested local stdio
// servers (or, with no args, the local ones in cfg.MCP) with the sbx gateway,
// porting `make mcp-register` so nobody needs the repo/Makefile.
func runMcpRegister(argv []string) {
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix mcp register: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := registerServers(cfg, defaultShellEnv(), os.Stdout, argv, findHostBinary, activeContainerMCP(cfg)); err != nil {
		fmt.Fprintf(os.Stderr, "pix mcp register: %v\n", err)
		if errors.Is(err, errSbxUnavailable) {
			os.Exit(exitServiceDown)
		}
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
	hostBin string // absolute pix-host (for slack + other host subcommands)
	// containers maps a server name to its pack CONTAINER/REMOTE spec (Manifest,
	// Image, or RemoteURL). A Manifest name registers via `--local --url` (gateway
	// resolves the OCI image; creds Docker-side; never op-run wrapped). An Image
	// name registers as an op-run-wrapped `docker run <image>` (creds from op-refs
	// forwarded via -e), exactly like a local stdio server otherwise. A RemoteURL
	// name registers via `--url` (a remote MCP endpoint the gateway OAuths
	// host-side; no op-run wrap).
	containers map[string]packContainer
}

// gogHardenedArgv builds the EXACT hardened gog invocation used both when
// registering gog with the sbx gateway (mcpRegistrar.serverCmd) and when
// probing it directly (gog_setup.go's headless verification, doctor's
// best-effort reconstruction) — a single definition so a direct probe can
// never silently drift from what actually gets registered. gogBin is normally
// the canonical PATH-resolved gog binary.
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
// pix-host subcommand (slack + friends).
func (m mcpRegistrar) serverCmd(name string) []string {
	// Image container: the bare command is `docker run -i --rm -e <KEY>… <image>`.
	// addArgs op-run wraps it (when op-refs is present), so op resolves each KEY
	// from 1Password and `-e KEY` forwards it into the container.
	if c := m.containers[name]; c.Image != "" {
		argv := []string{"docker", "run", "-i", "--rm"}
		for _, k := range c.EnvKeys {
			argv = append(argv, "-e", k)
		}
		keys := make([]string, 0, len(c.EnvValues))
		for key := range c.EnvValues {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			argv = append(argv, "-e", key+"="+c.EnvValues[key])
		}
		return append(argv, c.Image)
	}
	switch name {
	case gwServerName:
		return gogHardenedArgv(m.gog, m.account)
	default:
		// slack + any other local stdio server is a pix-host subcommand.
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
	// Remote container: register the remote MCP endpoint by URL. No --local (it's a
	// remote HTTP server, not an OCI image the gateway runs) and no op-run wrap —
	// OAuth is discovered + handled host-side by the gateway on first use.
	if c := m.containers[name]; c.RemoteURL != "" {
		return []string{"mcp", "add", name, "--url", c.RemoteURL}
	}
	return rawAddArgs(name, m.execArgv(name))
}

// execArgv returns the EXACT, literal command line the sbx gateway will exec to
// spawn server `name` — serverCmd's bare invocation, wrapped in
// `op run --no-masking --env-file=<refs> -- <cmd…>` when m.opRefs is present so
// creds resolve from 1Password at gateway spawn time, or returned bare when
// m.opRefs is empty (1Password is optional). This is the single source of
// truth for "what will actually run": addArgs re-encodes it into sbx's
// --command/--args form, and gogRegisteredArgv calls it directly so a probe of
// the real headless path can never drift from what gets registered. Container
// (manifest/remote) servers never route through here — addArgs short-circuits
// them above.
func (m mcpRegistrar) execArgv(name string) []string {
	cmd := m.serverCmd(name)
	if m.opRefs == "" {
		return cmd
	}
	return append(opRunWrapPrefix(m.op, m.opRefs), cmd...)
}

// opRunWrapPrefix is the ONE op-run wrapper grammar the launcher ever
// generates: `<op> run --no-masking --env-file=<refs> --`. It is shared
// between the generator (execArgv above) and the recognizer (doctor.go's
// unwrapOpRun), so what doctor trusts to unwrap can never drift from what
// registration actually writes — any other op subcommand, option set,
// ordering, or env file is by definition not launcher-generated.
func opRunWrapPrefix(op, refs string) []string {
	return []string{op, "run", "--no-masking", "--env-file=" + refs, "--"}
}

// rawAddArgs builds a literal `sbx mcp add <name> --command <argv[0]> --args
// <argv[1]> ...` argv from an already-resolved command line (e.g. one read
// back via `sbx mcp get`). Used both by addArgs above and by gog_setup.go's
// registration rollback, which must re-register a PRIOR command exactly as it
// was without reconstructing it from config.
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

// gogRegisteredArgv returns the EXACT argv the sbx gateway will exec for gog,
// given resolved binaries/refs/account — literally mcpRegistrar{…}.execArgv
// ("gog") for the same inputs registerServers would use. gog_setup.go's
// pre-commit headless verification and doctor's best-effort reconstruction
// fallback (when sbx's own registration can't be read) both call THIS, so
// neither can silently probe lighter flags/wrapper than what registration will
// actually run. Pass opBin/opRefs both "" for a bare (no 1Password) probe —
// mirrors registerServers' opReady gate, where op and op-refs are only ever
// used together.
func gogRegisteredArgv(gogBin, opBin, opRefs, account string) []string {
	return mcpRegistrar{gog: gogBin, account: account, op: opBin, opRefs: opRefs}.execArgv(gwServerName)
}

// gogBareRegistrationNote is the ONE shared message printed whenever gog is
// registered without an op-refs.env wrapper (1Password not configured): gog
// authenticates via its own OAuth flow, never op-refs, so this points at the
// guided `pix gworkspace setup` recovery path — never a raw legacy direct-login
// recipe.
func gogBareRegistrationNote(out io.Writer) {
	fmt.Fprintln(out, "note: registered gog directly (bare); gog authenticates via its own OAuth flow — "+
		"if it ever needs re-authorizing, run: pix gworkspace setup")
}

// buildGogRegistrar resolves op + op-refs EXACTLY ONCE and pairs them with the
// already-resolved gogPath/account into an IMMUTABLE mcpRegistrar snapshot.
// gogSetup calls this a single time, before its headless verification probe;
// the returned reg is then used, UNCHANGED, both to build the probe's argv
// (reg.execArgv("gog")) and to register it (registerGogRegistrar ->
// reg.addArgs("gog")) — no re-resolution of op/op-refs/gog happens between the
// two, so a PATH or op-refs.env mutation in that window can never register a
// different command than the one that was just proven healthy.
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
// probed against — no independent re-resolution here, so the registered
// command is byte-for-byte the one that was just verified.
func registerGogRegistrar(reg mcpRegistrar, env shellEnv, out io.Writer) error {
	if env.run == nil {
		return fmt.Errorf("internal: shellEnv.run not wired")
	}
	if reg.opRefs == "" {
		gogBareRegistrationNote(out)
	}
	if _, err := env.run("sbx", reg.addArgs(gwServerName)...); err != nil {
		return fmt.Errorf("sbx mcp add %s: %w", gwServerName, err)
	}
	fmt.Fprintln(out, "  registered: "+gwServerName)
	return nil
}

// registerServers resolves + guards + builds + runs the `sbx mcp add` commands
// for the requested local stdio servers. With no requested names it registers
// every entry in the resolved profile's cfg.MCP (gog via its special path, every
// other name as `pix-host mcp <name>`). It fails with a clear, actionable
// message rather than registering a junk command (op/gog missing, gog account
// unset). When sbx is absent it prints exactly what it WOULD run instead of
// crashing. hostResolver locates pix-host (injected so tests stay
// hermetic).
func registerServers(cfg *config.Config, env shellEnv, out io.Writer,
	requested []string, hostResolver func() (string, error), containers map[string]packContainer) error {

	names := requested
	if len(names) == 0 {
		names = append(names, cfg.MCP...)
	}
	if len(names) == 0 {
		fmt.Fprintln(out, "Nothing to register: no local stdio servers requested or in config mcp.")
		fmt.Fprintln(out, "Enable one first, e.g.:  pix config set mcp "+gwServerName)
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
	// (`pix-host mcp --list`). gog is always a valid local special case even
	// though the bridge never lists it. Partition the requested names into gog,
	// confirmed-local servers, and remote gateway-catalog servers to SKIP.
	localSet, localKnown := localMCPNames(env, hostResolver)
	wantGog := false
	var localServers []string
	var containerServers []string // pack CONTAINER/REMOTE integrations (--local --url manifest, or --url remote)
	var skippedUnknown []string   // non-gog names skipped because the local set is unknown
	for _, n := range names {
		switch {
		case n == gwServerName:
			wantGog = true
		case containers[n].Manifest != "":
			// Manifest container: registered by --local --url, not a host --command,
			// so it doesn't depend on the pix-host local-name set.
			containerServers = append(containerServers, n)
		case containers[n].RemoteURL != "":
			// Remote container: the pack carries the endpoint URL, so we register it
			// ourselves via `sbx mcp add --url` (OAuth'd host-side). Previously these
			// gateway-catalog names were SKIPPED — the pack now wires them directly.
			containerServers = append(containerServers, n)
		case containers[n].Image != "":
			// Image container: an op-run-wrapped `docker run` — behaves like a local
			// stdio server (host-registered, op-refs-backed), just a different cmd.
			localServers = append(localServers, n)
		case !localKnown:
			// FAIL CLOSED: the local-name list could NOT be established
			// (pix-host unresolved or `mcp --list` failed). We must NOT assume
			// an unknown name is local — that would register a gateway-catalog name
			// (e.g. notion) as `pix-host mcp notion`. Skip every non-gog name
			// with an actionable warning and fail the command below.
			fmt.Fprintf(out, "  %s: cannot determine local MCP servers "+
				"(pix-host mcp --list failed); skipping %s; re-run after building pix-host\n", n, n)
			skippedUnknown = append(skippedUnknown, n)
		case !localSet[n]:
			// Not gog and not a local stdio server -> a remote gateway-catalog
			// server. It is attached a different way; do not register it as local.
			fmt.Fprintf(out, "  %s: gateway-catalog server, not locally registered\n", n)
		default:
			// Confirmed local: it is in the pix-host `mcp --list` set.
			localServers = append(localServers, n)
		}
	}

	// skippedErr is non-nil when a requested non-gog name was skipped because the
	// local set could not be established. It is folded into the final error so the
	// command exits non-zero rather than reporting a silent success.
	var skippedErr error
	if len(skippedUnknown) > 0 {
		skippedErr = fmt.Errorf("could not determine local MCP servers "+
			"(pix-host mcp --list failed); skipped %s; build pix-host, then re-run",
			strings.Join(skippedUnknown, ", "))
	}

	// The final registration order: local servers, then container servers, then
	// gog (if requested).
	var finalNames []string
	finalNames = append(finalNames, localServers...)
	finalNames = append(finalNames, containerServers...)
	if wantGog {
		finalNames = append(finalNames, gwServerName)
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
			// A confirmed non-gog local stdio server (slack, or another registered
			// local name) can actually use op-refs. Best-effort: seed a template op-refs.env at
			// the absolute XDG path so the user has a concrete file to fill in later,
			// and note that we registered bare rather than failing.
			refsPath := defaultOpRefsPath(env)
			// ONE seeder: route through config.SeedOpRefsAt so the template + 0700 dir
			// / 0600 file + no-clobber rule is identical to `pix setup`'s seeding.
			if created, err := config.SeedOpRefsAt(refsPath); err == nil && created {
				fmt.Fprintf(out, "seeded a template op-refs.env at %s\n", refsPath)
			}
			fmt.Fprintf(out, "note: no op-refs.env found; registered %s directly (bare, no 1Password); "+
				"add creds to %s if a server needs them\n",
				strings.Join(finalNames, ", "), refsPath)
		} else if wantGog {
			// gog-only: gog authenticates via its own OAuth grant, never op-refs, so
			// do NOT seed op-refs.env or mention it. Register bare. The grant
			// guidance is the GUIDED command only — gog is a LOCAL stdio MCP, so
			// neither native `sbx mcp auth` (remote catalog OAuth) nor a raw legacy
			// direct-login recipe is ever printed.
			fmt.Fprintln(out, "note: registered gog directly (bare); gog authenticates via OAuth — wire it: pix gworkspace setup")
		}
		// container-only: nothing to seed — container creds are Docker-side, not op-refs.
	}

	if wantGog {
		gogPath, err := lookPath("gog")
		if err != nil {
			return fmt.Errorf("gog is requested but gog not found — " + gwInstallCmd)
		}
		account := strings.TrimSpace(cfg.GogAccount)
		if account == "" {
			return fmt.Errorf("gog is requested but no account is set — " +
				"run: pix config set google_workspace_account <you@example.com>")
		}
		reg.gog = gogPath
		reg.account = account
	}

	if len(localServers) > 0 {
		hb, err := hostResolver()
		if err != nil {
			return fmt.Errorf("pix-host not found (needed for non-gog servers): %v", err)
		}
		reg.hostBin = hb
	}

	_, sbxErr := lookPath("sbx")
	sbxOK := sbxErr == nil
	if !sbxOK {
		fmt.Fprintln(out, "sbx not on PATH; here is what WOULD be registered (run these on the host):")
	}

	// Accumulate per-server failures so `pix mcp register` exits non-zero on
	// ANY failure, while still attempting every server and printing each result.
	var regErrs []error
	for _, n := range finalNames {
		args := reg.addArgs(n)
		if !sbxOK {
			fmt.Fprintf(out, "  sbx %s\n", strings.Join(args, " "))
			continue
		}
		if remoteURL := containers[n].RemoteURL; remoteURL != "" && remoteMCPRegistrationCurrent(env, n, remoteURL) {
			if !env.quiet {
				fmt.Fprintf(out, "  already registered: %s\n", n)
			}
			continue
		}
		var err error
		if containers[n].RemoteURL != "" && env.runInteractive != nil {
			// `sbx mcp add --url` may perform OAuth and keep a localhost callback
			// listener alive while the browser completes. A bounded probe kills that
			// listener, leaving the browser at ERR_CONNECTION_REFUSED. Remote MCP
			// registration is an explicitly interactive mutation; let it inherit the
			// terminal and run to completion. Read-only status checks remain bounded.
			if env.quiet && env.runInteractiveQuiet != nil {
				fmt.Fprintf(out, "  Authorize %s in your browser…\n", n)
				err = env.runInteractiveQuiet("sbx", args...)
			} else {
				err = env.runInteractive("sbx", args...)
			}
		} else if env.probe != nil {
			_, timedOut, probeErr := env.probe("sbx", args...)
			err = probeErr
			if timedOut {
				err = fmt.Errorf("timed out")
			}
		} else {
			_, err = env.run("sbx", args...)
		}
		if err != nil {
			fmt.Fprintf(out, "  FAILED to register: %s (%v)\n", n, err)
			regErrs = append(regErrs, fmt.Errorf("%s: %v", n, err))
		} else {
			if !env.quiet {
				fmt.Fprintf(out, "  registered: %s\n", n)
			}
		}
	}

	if sbxOK {
		if reg.opRefs != "" && !env.quiet {
			fmt.Fprintf(out, "Each wrapped server resolves its creds from %s via op run at gateway spawn.\n", reg.opRefs)
		}
	} else {
		fmt.Fprintln(out, "note: install Docker Sandboxes (sbx) to register: https://docs.docker.com/ai/sandboxes")
	}
	if len(regErrs) > 0 {
		return errors.Join(fmt.Errorf("%d server(s) failed to register: %w", len(regErrs), errors.Join(regErrs...)), skippedErr)
	}
	if !sbxOK {
		// `register` PROMISED to register these servers and did not (nothing was
		// exec'd, nothing is registered with the gateway) — exit non-zero
		// (errSbxUnavailable -> exitServiceDown) rather than a silent success
		// just because the would-run lines above printed cleanly.
		return errors.Join(errSbxUnavailable, skippedErr)
	}
	return skippedErr
}

// remoteMCPRegistrationCurrent prevents an idempotent setup rerun from
// reopening OAuth. It skips only when sbx's inspected definition contains the
// exact endpoint the pack declares; a changed or unreadable definition is
// registered again.
func remoteMCPRegistrationCurrent(env shellEnv, name, endpoint string) bool {
	for _, verb := range []string{"inspect", "get"} {
		out, timedOut, err := probeRun(env, "sbx", "mcp", verb, name)
		if err == nil && !timedOut && strings.Contains(out, endpoint) {
			return true
		}
	}
	return false
}

// allPreloadedMCP returns, order-preserving and de-duplicated, every non-empty
// name in `servers` — the full set to attach EAGERLY at create (emitted to sbx
// as --static-mcp; their tools sit in context from the start).
//
// S01: there is no eager/lazy split any more. Every configured server (plus
// every pack integration's server — see applyPackToLaunch) preloads at CREATE,
// regardless of kind (local stdio or remote gateway-catalog): once a server is
// registered (`pix mcp register` / `sbx mcp add`) it's behind the local
// gateway, so local vs remote was never the axis here — the retired
// mcp_static/mcp_dynamic knobs were the only thing that ever kept a server out
// of this set, and they're gone (see config.RetiredKeys). This function is now
// pure list hygiene: dedupe + drop empties, order preserved.
func allPreloadedMCP(servers []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range servers {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// localMCPNames asks the pix-host binary which MCP servers it can serve
// locally (`pix-host mcp --list`), the source of truth for local vs remote
// registration. It returns the set of names and whether the list was obtained.
// A missing binary or a failed call returns (nil,false); the caller then FAILS
// CLOSED — it registers only gog (a known special case) and SKIPS every other
// requested name rather than risk registering a remote gateway-catalog name as a
// local pix-host subcommand.
func localMCPNames(env shellEnv, hostResolver func() (string, error)) (map[string]bool, bool) {
	if env.run == nil || hostResolver == nil {
		return nil, false
	}
	hb, err := hostResolver()
	if err != nil || hb == "" {
		return nil, false
	}
	// BOUNDED: a hung `pix-host mcp --list` degrades to an unknown local
	// set (callers fail closed), never a wedged caller.
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

// defaultOpRefsPath computes the absolute XDG op-refs.env path from the injected
// env (mirrors config.OpRefsPath but stays hermetic under test): $PIX_CONFIG
// dir, else $XDG_CONFIG_HOME/pix, else ~/.config/pix — all + op-refs.env.
// This is the path repo-less hosts must create, so every user-facing message and
// the seeder reference it (never a meaningless repo-relative config/op-refs.env).
func defaultOpRefsPath(env shellEnv) string {
	if env.getenv != nil {
		if p := env.getenv("PIX_CONFIG"); p != "" {
			return filepath.Join(filepath.Dir(p), "op-refs.env")
		}
		if xdg := env.getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "pix", "op-refs.env")
		}
	}
	if env.homeDir != nil {
		if home := env.homeDir(); home != "" {
			return filepath.Join(home, ".config", "pix", "op-refs.env")
		}
	}
	return config.OpRefsPath()
}
