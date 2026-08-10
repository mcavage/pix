package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sort"
	"strings"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys"
)

// CatalogServer is one shipped public MCP catalog entry: a name plus the
// remote endpoint `sbx mcp add NAME --url URL` registers it under.
type CatalogServer struct {
	Name string
	URL  string
}

// McpCatalog is a convenience LOOKUP TABLE: hosted servers whose endpoint pix
// already knows, so `pix mcp add notion` does not make you go find the URL
// again. It is not a bundle, a recommendation, or a set anything registers
// wholesale: `pix mcp bundle` (which registered all of these in one step) was
// deleted, because shipping three named SaaS vendors as a first-class CLI verb
// is a personal preference wearing a public API's clothes. Any name absent from
// here is registered exactly the same way, with an explicit --url.
var McpCatalog = []CatalogServer{
	{Name: "notion", URL: "https://mcp.notion.com/mcp"},
	{Name: "atlassian", URL: "https://mcp.atlassian.com/v1/mcp"},
	{Name: "granola", URL: "https://mcp.granola.ai/mcp"},
}

// McpCatalogNames is the SINGLE source of truth for "is this a name pix knows
// the endpoint for", DERIVED from McpCatalog so the two can never disagree.
// classifyMCPServer's remote-vs-custom split reuses this set, so a
// plausible-looking but unknown name (e.g. "linear") is never treated as one
// pix can register unaided — that would print a repair command that silently
// cannot work.
var McpCatalogNames = catalogNameSet(McpCatalog)

func catalogNameSet(catalog []CatalogServer) map[string]bool {
	names := make(map[string]bool, len(catalog))
	for _, c := range catalog {
		names[c.Name] = true
	}
	return names
}

// Credentials is everything registration needs to know about the host's
// 1Password setup. It is a PARAMETER, not something this package resolves:
// answering these questions is the secret capability's job, and a capability
// may not call a sibling, so the composition root asks secret and passes the
// answers down.
type Credentials struct {
	OpPath     string // resolved `op` binary ("" = not installed)
	OpRefsPath string // an EXISTING op-refs.env ("" = none found)
	SeedPath   string // where a template op-refs.env would be seeded
	GogKeyring bool   // GOG_KEYRING_PASSWORD is present as a FILLED op:// ref
}

// ErrSbxUnavailable is the sentinel every mcp subcommand that PROMISES an
// operation (register/load/auth/bundle) returns when sbx isn't on PATH,
// instead of silently exiting 0 after only printing what it would have run.
// It maps to rpc.ExitServiceDown (3) — the same "evidence/dependency
// unavailable" code `pix memory`/`secret` use. Read-only `pix mcp ls` is held
// to the SAME contract: the caller asked for gateway state and got none, so a
// truthful exit code beats a quiet success implying "zero servers".
var ErrSbxUnavailable = fmt.Errorf("sbx not on PATH")

// McpWouldRun prints the exact host command a user can run manually (the
// recovery path every mutating mcp subcommand must preserve verbatim) and
// returns ErrSbxUnavailable so the caller exits non-zero. This is an ERROR
// report (exit 3), so it goes to errW/stderr — a caller piping stdout (e.g.
// `pix mcp ls | jq`) must never see it mixed into what looks like output.
func McpWouldRun(errW io.Writer, args ...string) error {
	fmt.Fprintf(errW, "sbx not on PATH; would run: sbx %s (run it on the host)\n", strings.Join(args, " "))
	return ErrSbxUnavailable
}

// The exit dispatcher that used to live here (ExitMcpVerb, which mapped these
// errors to os.Exit from inside the capability) is gone: every `pix mcp` verb
// returns its error to cmd/pix, whose mcpExit wrapper does the same mapping at
// the one layer allowed to end the process. Two mappers meant two places for
// "what does exit 3 mean" to drift.

// RunSbxMcpCore is the ONE passthrough every `pix mcp` verb that simply
// forwards to sbx runs through (bundle, auth, ls): lookPath is injected so a
// test can force the sbx-absent branch hermetically (no PATH manipulation, no
// subprocess) and assert ErrSbxUnavailable + the printed recovery command
// without ever exec'ing anything. Having one body is the point — three copies
// is how the verbs drifted into phrasing the same failure differently.
func RunSbxMcpCore(lookPath func(string) (string, error), out io.Writer, in io.Reader, errW io.Writer, args []string) error {
	if _, err := lookPath("sbx"); err != nil {
		return McpWouldRun(errW, args...)
	}
	cmd := exec.Command("sbx", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errW
	return cmd.Run()
}

// RunMcpLsCore is runMcpLs's testable core (see RunSbxMcpCore). It exits 3 when
// the listing is unavailable, per the ErrSbxUnavailable policy above: a caller
// cannot tell "zero servers" from "couldn't ask" otherwise.
//
// `sbx mcp ls` reports HOST registration only, never what is attached to the
// sandbox running right now, so mcpLsAttachmentNote is appended after a
// plain-text listing — and skipped for a machine-readable format (any args,
// e.g. a future `-o json`) so a script never has to filter out prose.
func RunMcpLsCore(lookPath func(string) (string, error), out io.Writer, in io.Reader, errW io.Writer, extraArgs ...string) error {
	if err := RunSbxMcpCore(lookPath, out, in, errW, append([]string{"mcp", "ls"}, extraArgs...)); err != nil {
		return err
	}
	if len(extraArgs) == 0 {
		fmt.Fprint(out, mcpLsAttachmentNote)
	}
	return nil
}

// mcpLsAttachmentNote is the disclaimer `pix mcp ls` prints after a successful
// listing (see RunMcpLsCore). It deliberately does NOT point to `pix status`/
// `pix doctor` as an attachment authority: neither can see inside a live
// session either (health/mcp.go's attachmentCaveat says so in its own words),
// so sending a reader there to learn "what's live" would just relocate the
// same unanswerable question. The two REAL options are named instead.
const mcpLsAttachmentNote = "\nNote: this is the gateway's HOST registration list, not what's attached to\n" +
	"your current sandbox — `pix status`/`pix doctor` can't see inside a live\n" +
	"session either. A sandbox picks up everything registered when it starts, so\n" +
	"`pix rm <box>` then `pix run` is how a running one catches up.\n"

// McpRegistrar carries the resolved ABSOLUTE paths + account needed to build a
// `sbx mcp add` command. The gateway daemon's PATH may not include op/gog, so
// every binary is registered by absolute path (matching the Makefile).
type McpRegistrar struct {
	Op      string // absolute op (1Password CLI)
	OpRefs  string // absolute config/op-refs.env
	Gog     string // absolute gog (only needed to register gog)
	Account string // gog --account value
	HostBin string // absolute pix-host (for slack + other host subcommands)
	// GogUseOp is true only for gog's explicit file-keyring topology, where
	// GOG_KEYRING_PASSWORD is an op:// ref. Normal macOS OAuth lives in gog's
	// own keychain and must stay bare even when unrelated pack servers use op.
	GogUseOp bool
	// containers maps a server name to its pack CONTAINER/REMOTE spec (Manifest,
	// Image, or RemoteURL). A Manifest name registers via `--local --url` (gateway
	// resolves the OCI image; creds Docker-side; never op-run wrapped). An Image
	// name registers as an op-run-wrapped `docker run <image>` (creds from op-refs
	// forwarded via -e), exactly like a local stdio server otherwise. A RemoteURL
	// name registers via `--url` (a remote MCP endpoint the gateway OAuths
	// host-side; no op-run wrap).
	containers map[string]config.MCPContainer
	// LegacyPositionalURL flips a manifest/remote container's URL argument
	// from the current --url FLAG grammar to the legacy POSITIONAL grammar
	// (`mcp add name --local <manifest>` / `mcp add name <url>`, no --url).
	// It is decided ONCE, up front, by a read-only `sbx mcp add --help` probe
	// (see detectLegacyPositionalURL) — never by retrying a failed remote-URL
	// registration, which can trigger an interactive OAuth grant a second
	// attempt must not repeat.
	LegacyPositionalURL bool
}

// GogHardenedArgv builds the EXACT hardened gog invocation used both when
// registering gog with the sbx gateway (McpRegistrar.ServerCmd) and when
// probing it directly — a single definition so a direct probe can never
// silently drift from what actually gets registered. gogBin is normally the
// canonical PATH-resolved gog binary.
func GogHardenedArgv(gogBin, account string) []string {
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
func (m McpRegistrar) ServerCmd(name string) []string {
	// Image container: the bare command is `docker run -i --rm -e <KEY>… <image>`.
	// AddArgs op-run wraps it (when op-refs is present), so op resolves each KEY
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
	case config.GWServerName:
		return GogHardenedArgv(m.Gog, m.Account)
	default:
		// slack + any other local stdio server is a pix-host subcommand.
		return []string{m.HostBin, "mcp", name}
	}
}

// AddArgs builds the `sbx mcp add <name> …` argv for one server. When op-refs is
// present (m.opRefs != "") the command is wrapped in
// `op run --no-masking --env-file=<refs> -- <cmd…>` so creds resolve from
// 1Password at gateway spawn time (needed for slack's token + gog's headless
// keyring password). When op-refs is ABSENT every local server is registered
// DIRECTLY as a bare command — 1Password is optional, so a no-creds server (a
// future `pio`) still registers, and a creds server just runs without injected
// creds until an op-refs.env is added (harmless: the op-run wrapper is a no-op
// for a server that needs no creds).
func (m McpRegistrar) AddArgs(name string) []string {
	// Manifest container: register the OCI server by manifest, run locally by the
	// gateway via Docker. No op-run wrap — its creds are provided Docker-side
	// (declared in the server's server.json), never through op-refs. (Image
	// containers fall through to serverCmd + the op-run wrapper below.)
	if c := m.containers[name]; c.Manifest != "" {
		if m.LegacyPositionalURL {
			return []string{"mcp", "add", name, "--local", c.Manifest}
		}
		return []string{"mcp", "add", name, "--local", "--url", c.Manifest}
	}
	// Remote container: register the remote MCP endpoint by URL. No --local (it's a
	// remote HTTP server, not an OCI image the gateway runs) and no op-run wrap —
	// OAuth is discovered + handled host-side by the gateway on first use.
	if c := m.containers[name]; c.RemoteURL != "" {
		if m.LegacyPositionalURL {
			return []string{"mcp", "add", name, c.RemoteURL}
		}
		return []string{"mcp", "add", name, "--url", c.RemoteURL}
	}
	argv := m.ExecArgv(name)
	args := []string{"mcp", "add", name, "--command", argv[0]}
	for _, c := range argv[1:] {
		args = append(args, "--args", c)
	}
	return args
}

// detectLegacyPositionalURL is the bounded, side-effect-free FEATURE-DETECTION
// probe that decides which `sbx mcp add` URL grammar this host's installed sbx
// speaks, BEFORE ever registering a manifest/remote container. It runs the
// read-only `sbx mcp add --help` (bounded by env.RunTimed, capped output) and
// looks for a documented --url flag.
//
// A help call that fails, times out, or prints nothing recognizable NEVER
// flips behavior: it returns false (the current --url-flag grammar, this
// host's default and the one the public sbx v0.38 grammar documents) exactly
// as if detection had not run at all. Only help text that positively omits
// --url from its own flag listing switches to the legacy positional grammar.
func detectLegacyPositionalURL(env hostenv.Env) bool {
	out, timedOut, err := env.RunTimed("sbx", "mcp", "add", "--help")
	if timedOut || err != nil || strings.TrimSpace(out) == "" {
		return false
	}
	return !strings.Contains(out, "--url")
}

// runSbxCaptured execs `sbx <args...>` with stdout and stderr captured into
// SEPARATE buffers, so RunSbxGrammarFallback can still classify a failure
// from their combined text (sys.IsUsageMismatch does not care which stream a
// parser wrote its complaint to) while reporting each stream to its own
// destination once a final attempt is chosen.
func runSbxCaptured(args []string) (stdout, stderr string, err error) {
	cmd := exec.Command("sbx", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// catalogEntryState is what a single bounded `sbx mcp ls` (plus, when the
// name is present, one inspect/get probe) can PROVE about one shipped
// catalog entry's current registration — the tri-state the direct add/rm
// fallback acts on so neither ever guesses at ownership.
type catalogEntryState int

const (
	// catalogEntryAbsent: `sbx mcp ls` positively lacks this name.
	catalogEntryAbsent catalogEntryState = iota
	// catalogEntryMatches: registered at EXACTLY the shipped catalog URL —
	// ours, safe to leave alone (add) or remove (rm).
	catalogEntryMatches
	// catalogEntryCustom: the name is registered, but its inspected endpoint
	// differs from the shipped URL, or no endpoint could be found at all
	// (a different KIND of server entirely, e.g. a local command). Never
	// ours to overwrite or remove.
	catalogEntryCustom
)

// classifyCatalogEntry resolves one catalog entry's state from an
// already-fetched successful `sbx mcp ls` (mcpOut) plus, only when the name
// is present there, one further inspect/get probe to compare endpoints. It
// returns an error ONLY on an operational inspect failure (both `inspect`
// and `get` failed) — never a guess dressed up as a classification.
func classifyCatalogEntry(mcpOut, name, url string) (catalogEntryState, error) {
	if McpRegEvidenceFrom(mcpOut, true, name) == McpRegNo {
		return catalogEntryAbsent, nil
	}
	match, err := catalogEntryMatchesShippedURL(name, url)
	if err != nil {
		return catalogEntryCustom, err
	}
	if match {
		return catalogEntryMatches, nil
	}
	return catalogEntryCustom, nil
}

// catalogEntryMatchesShippedURL inspects an ALREADY-PRESENT registration
// (via `sbx mcp inspect NAME`, falling back to `sbx mcp get NAME` exactly
// like remoteMCPRegistrationCurrent) and reports whether its canonical
// endpoint is EXACTLY url. It returns an error only when BOTH inspection
// verbs fail operationally — that failure must fail the caller closed, never
// be read as "no endpoint found" (which would misclassify a real custom
// registration as absent-equivalent and license overwriting it).
func catalogEntryMatchesShippedURL(name, url string) (bool, error) {
	for _, verb := range []string{"inspect", "get"} {
		stdout, _, err := runSbxCaptured([]string{"mcp", verb, name})
		if err == nil {
			return outputContainsCanonicalEndpoint(stdout, url), nil
		}
	}
	return false, fmt.Errorf("could not inspect existing registration (`sbx mcp inspect|get %s` both failed)", name)
}

// catalogLsEvidenceOrFailClosed runs the ONE bounded `sbx mcp ls` every
// direct catalog fallback (add or rm) fetches up front, so every catalog
// entry is classified against a single consistent snapshot rather than a
// fresh (and possibly inconsistent) listing per name. A failed listing is an
// operational failure, not evidence of absence: fail closed rather than risk
// treating an unreadable registration as fair game to add over or leave
// unremoved.
func catalogLsEvidenceOrFailClosed() (string, error) {
	stdout, stderr, err := runSbxCaptured([]string{"mcp", "ls"})
	if err != nil {
		return "", fmt.Errorf("checking existing registrations first (`sbx mcp ls`): %v: %s",
			err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// AddRemoteServers registers each hosted server ONE AT A TIME via direct
// `sbx mcp add NAME --url URL`. It first
// fetches registration evidence ONCE (catalogLsEvidenceOrFailClosed) and
// classifies every entry against it before touching anything:
//
//   - absent (catalogEntryAbsent): add it.
//   - registered at the exact shipped URL (catalogEntryMatches): leave it
//     unchanged — already ours, nothing to do.
//   - registered under the same name but a different endpoint or kind
//     (catalogEntryCustom): FAIL CLOSED without overwriting it — a caller's
//     own "notion" server (say) must never be silently replaced by the URL pix
//     happens to know for that name.
//
// It stops at the FIRST failure of any kind (a real add failure, an
// unreadable inspection, or a positively custom entry) — a partial success is
// never masked by continuing to the next entry — and reports every add
// attempt's own streams as they happen, in catalog order, so a caller sees
// exactly which entries registered before a failure.
func AddRemoteServers(out, errW io.Writer, catalog []CatalogServer) error {
	// sbx-absent is ErrSbxUnavailable (exit 3) with the command a user would run
	// by hand, NOT a generic failure: a verb that promises a registration must
	// never exit 1 with an exec error when the real answer is "the tool that
	// does this is not installed". Checked before the evidence probe, whose own
	// failure means something different (sbx is here and could not answer).
	if _, err := exec.LookPath("sbx"); err != nil {
		args := []string{"mcp", "add"}
		if len(catalog) > 0 {
			args = append(args, catalog[0].Name, "--url", catalog[0].URL)
		}
		return McpWouldRun(errW, args...)
	}
	mcpOut, err := catalogLsEvidenceOrFailClosed()
	if err != nil {
		return err
	}
	for _, c := range catalog {
		state, err := classifyCatalogEntry(mcpOut, c.Name, c.URL)
		if err != nil {
			return fmt.Errorf("%s: %v", c.Name, err)
		}
		switch state {
		case catalogEntryMatches:
			fmt.Fprintf(out, "  already registered: %s\n", c.Name)
		case catalogEntryCustom:
			return fmt.Errorf("%s: already registered with a different endpoint or kind than the shipped "+
				"catalog entry (%s); refusing to overwrite it — remove it manually first if you want the catalog default",
				c.Name, c.URL)
		default: // catalogEntryAbsent
			stdout, stderr, err := runSbxCaptured([]string{"mcp", "add", c.Name, "--url", c.URL})
			fmt.Fprint(out, stdout)
			fmt.Fprint(errW, stderr)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// ExecArgv returns the EXACT, literal command line the sbx gateway will exec to
// spawn server `name` — serverCmd's bare invocation, wrapped in
// `op run --no-masking --env-file=<refs> -- <cmd…>` when m.opRefs is present so
// creds resolve from 1Password at gateway spawn time, or returned bare when
// m.opRefs is empty (1Password is optional). This is the single source of
// truth for "what will actually run": AddArgs re-encodes it into sbx's
// --command/--args form, and GogRegisteredArgv calls it directly so a probe of
// the real headless path can never drift from what gets registered. Container
// (manifest/remote) servers never route through here — AddArgs short-circuits
// them above.
func (m McpRegistrar) ExecArgv(name string) []string {
	cmd := m.ServerCmd(name)
	if m.OpRefs == "" || (name == config.GWServerName && !m.GogUseOp) {
		return cmd
	}
	// The ONE op-run wrapper grammar pix generates.
	return append([]string{m.Op, "run", "--no-masking", "--env-file=" + m.OpRefs, "--"}, cmd...)
}

// RegisterServers resolves + guards + builds + runs the `sbx mcp add` commands
// for the requested local stdio servers. With no requested names it registers
// every entry in the resolved profile's cfg.MCP (gog via its special path, every
// other name as `pix-host mcp <name>`). It fails with a clear, actionable
// message rather than registering a junk command (op/gog missing, gog account
// unset). When sbx is absent it prints exactly what it WOULD run instead of
// crashing. hostResolver locates pix-host (injected so tests stay
// hermetic).
func RegisterServers(cfg *config.Config, env hostenv.Env, out io.Writer,
	requested []string, hostResolver func() (string, error), containers map[string]config.MCPContainer,
	creds Credentials) error {

	names := requested
	if len(names) == 0 {
		names = append(names, cfg.MCP...)
	}
	if len(names) == 0 {
		fmt.Fprintln(out, "Nothing to register: no local stdio servers requested or in config mcp.")
		fmt.Fprintln(out, "Enable one first, e.g.:  pix config set mcp "+config.GWServerName)
		return nil
	}

	// `sbx mcp add` registers against sbx's local data-plane gateway, which is
	// always available (no SBX_MCP_URL needed on nightly) — so there's no gateway
	// precondition to check here anymore.

	// Nil-safe lookPath: a partially-populated hostenv.Env (some tests set only
	// env.Run) must degrade to "binary not found" rather than panic — the same
	// posture LocalMCPNames takes for a nil env.Run. Every op/gog/sbx lookup below
	// goes through this.
	lookPath := env.LookPath

	// The set of names this host can serve locally is the source of truth
	// (`pix-host mcp --list`). gog is always a valid local special case even
	// though the bridge never lists it. Partition the requested names into gog,
	// confirmed-local servers, and remote gateway-catalog servers to SKIP.
	localSet, localKnown := LocalMCPNames(env, hostResolver)
	wantGog := false
	var localServers []string
	var containerServers []string // pack CONTAINER/REMOTE integrations (--local --url manifest, or --url remote)
	var skippedUnknown []string   // non-gog names skipped because the local set is unknown
	for _, n := range names {
		switch {
		case n == config.GWServerName:
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
		finalNames = append(finalNames, config.GWServerName)
	}
	if len(finalNames) == 0 {
		if skippedErr != nil {
			return skippedErr
		}
		fmt.Fprintln(out, "Nothing to register locally: every configured mcp name is a remote gateway-catalog server.")
		return nil
	}

	// op-refs is the file of op:// refs the wrapper resolves at spawn; when both
	// op and op-refs are present we wrap credentialed local servers in `op run`.
	// Normal gog OAuth remains bare unless the refs file explicitly contains
	// GOG_KEYRING_PASSWORD. When either is absent we register BARE: a no-creds
	// server registers fine, and a creds server runs uncredentialed until an
	// op-refs.env is added — never a hard failure.
	OpPath, OpRefs := creds.OpPath, creds.OpRefsPath
	opReady := OpPath != "" && OpRefs != ""

	reg := McpRegistrar{containers: containers}
	if len(containerServers) > 0 {
		// Decide the manifest/remote URL grammar ONCE, up front, by a read-only
		// help probe — never by retrying a failed registration. A remote
		// container's `mcp add` can trigger an interactive OAuth grant, and
		// retrying that with a different argv after a failed first attempt
		// risks a second, unwanted device-code flow; deciding the grammar
		// before ever registering anything avoids that entirely.
		reg.LegacyPositionalURL = detectLegacyPositionalURL(env)
	}
	if opReady {
		reg.Op = OpPath
		reg.OpRefs = OpRefs
	}
	if wantGog {
		reg.GogUseOp = opReady && creds.GogKeyring
	}

	if !opReady {
		if len(localServers) > 0 {
			// A confirmed non-gog local stdio server (slack, or another registered
			// local name) can actually use op-refs. Best-effort: seed a template op-refs.env at
			// the absolute XDG path so the user has a concrete file to fill in later,
			// and note that we registered bare rather than failing.
			refsPath := creds.SeedPath
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
			// do NOT seed op-refs.env or mention it. Register bare. gog is a LOCAL
			// stdio MCP with no built-in guided setup (that wizard was retired —
			// see docs/design/gworkspace-externalization.md): authorize the account
			// with the external `gog` CLI directly, never native `sbx mcp auth`
			// (remote catalog OAuth) or a raw legacy direct-login recipe.
			fmt.Fprintln(out, "note: registered gog directly (bare); gog authenticates via its own OAuth grant — run the gog CLI's own auth command if it needs (re)authorizing")
		}
		// container-only: nothing to seed — container creds are Docker-side, not op-refs.
	}

	if wantGog {
		gogPath, err := lookPath("gog")
		if err != nil {
			return fmt.Errorf("gog is requested but gog not found — " + config.GWInstallCmd)
		}
		account := strings.TrimSpace(cfg.GogAccount)
		if account == "" {
			return fmt.Errorf("gog is requested but no account is set — " +
				"run: pix config set google_workspace_account <you@example.com>")
		}
		reg.Gog = gogPath
		reg.Account = account
	}

	if len(localServers) > 0 {
		hb, err := hostResolver()
		if err != nil {
			return fmt.Errorf("pix-host not found (needed for non-gog servers): %v", err)
		}
		reg.HostBin = hb
	}

	_, sbxErr := lookPath("sbx")
	sbxOK := sbxErr == nil
	if !sbxOK {
		fmt.Fprintln(out, "sbx not on PATH; here is what WOULD be registered (run these on the host):")
	}

	// Accumulate per-server failures so `pix mcp add` exits non-zero on
	// ANY failure, while still attempting every server and printing each result.
	var regErrs []error
	for _, n := range finalNames {
		args := reg.AddArgs(n)
		if !sbxOK {
			fmt.Fprintf(out, "  sbx %s\n", strings.Join(args, " "))
			continue
		}
		if remoteURL := containers[n].RemoteURL; remoteURL != "" && remoteMCPRegistrationCurrent(env, n, remoteURL) {
			switch remoteMCPAuthorizationState(env, n) {
			case CatalogMCPReady:
				if !env.Quiet {
					fmt.Fprintf(out, "  already registered: %s\n", n)
				}
				continue
			case CatalogMCPUnauthorized:
				fmt.Fprintf(out, "  Authorize %s in your browser…\n", n)
				if authErr := runInteractiveSbx(env, "mcp", "auth", n); authErr != nil {
					regErrs = append(regErrs, fmt.Errorf("%s: authorization failed: %v", n, authErr))
					continue
				}
				if remoteMCPAuthorizationState(env, n) != CatalogMCPReady {
					regErrs = append(regErrs, fmt.Errorf("%s: authorization completed but could not be verified", n))
				}
				continue
			case CatalogMCPDenied:
				regErrs = append(regErrs, fmt.Errorf("%s: authorization denied by policy", n))
				continue
			default:
				regErrs = append(regErrs, fmt.Errorf("%s: registration exists but authorization could not be verified", n))
				continue
			}
		}
		var err error
		if containers[n].RemoteURL != "" {
			// Remote MCP registration is an explicitly interactive mutation (see
			// runInteractiveSbx); read-only status checks stay bounded.
			if env.Quiet {
				fmt.Fprintf(out, "  Authorize %s in your browser…\n", n)
			}
			err = runInteractiveSbx(env, args...)
		} else {
			// One branch, not two: the bounded seam is always present, so the
			// "fall back to the plain runner" arm this used to carry is gone.
			_, timedOut, probeErr := env.RunTimed("sbx", args...)
			err = probeErr
			if timedOut {
				err = fmt.Errorf("timed out")
			}
		}
		if err != nil {
			fmt.Fprintf(out, "  FAILED to register: %s (%v)\n", n, err)
			regErrs = append(regErrs, fmt.Errorf("%s: %v", n, err))
		} else {
			if !env.Quiet {
				fmt.Fprintf(out, "  registered: %s\n", n)
			}
		}
	}

	if sbxOK {
		wrapped := false
		if reg.Op != "" && reg.OpRefs != "" {
			for _, name := range finalNames {
				argv := reg.ExecArgv(name)
				if len(argv) > 1 && argv[0] == reg.Op && argv[1] == "run" {
					wrapped = true
					break
				}
			}
		}
		if wrapped && !env.Quiet {
			fmt.Fprintf(out, "Each wrapped server resolves its creds from %s via op run at gateway spawn.\n", reg.OpRefs)
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
		// (ErrSbxUnavailable -> rpc.ExitServiceDown) rather than a silent success
		// just because the would-run lines above printed cleanly.
		return errors.Join(ErrSbxUnavailable, skippedErr)
	}
	return skippedErr
}

// runInteractiveSbx runs an sbx mutation that may open a browser and keep a
// localhost OAuth callback listener alive: it inherits the terminal and runs
// to completion, never a bounded probe (a probe's kill leaves the browser at
// ERR_CONNECTION_REFUSED). The quiet variant is the same contract with sbx's
// own chatter suppressed — one helper so registration and repair can never
// pick different runners for the same interactive mutation.
func runInteractiveSbx(env hostenv.Env, args ...string) error {
	if env.Quiet {
		return env.RunInteractiveQuiet("sbx", args...)
	}
	return env.RunInteractive("sbx", args...)
}

// remoteMCPRegistrationCurrent prevents an idempotent setup rerun from
// reopening OAuth. It skips only when sbx's inspected definition contains the
// exact endpoint the pack declares; a changed or unreadable definition is
// registered again.
func remoteMCPRegistrationCurrent(env hostenv.Env, name, endpoint string) bool {
	for _, verb := range []string{"inspect", "get"} {
		out, timedOut, err := env.RunTimed("sbx", "mcp", verb, name)
		if err == nil && !timedOut && outputContainsCanonicalEndpoint(out, endpoint) {
			return true
		}
	}
	return false
}

// outputContainsCanonicalEndpoint accepts only URL/endpoint fields (or a bare
// URL line) whose parsed canonical URL equals want. Arbitrary JSON strings and
// substrings are not identity evidence.
func outputContainsCanonicalEndpoint(out, want string) bool {
	wantURL, ok := canonicalMCPEndpoint(want)
	if !ok {
		return false
	}
	var decoded any
	if json.Unmarshal([]byte(out), &decoded) == nil {
		found := map[string]bool{}
		jsonCollectCanonicalEndpoints(decoded, found)
		return len(found) == 1 && found[wantURL]
	}
	found := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if got, valid := canonicalMCPEndpoint(strings.Trim(line, `"'`)); valid {
			found[got] = true
		}
		if i := strings.Index(line, ":"); i >= 0 {
			key := normalizeEndpointField(line[:i])
			v := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
			if endpointField(key) {
				if got, valid := canonicalMCPEndpoint(v); valid {
					found[got] = true
				}
			}
		}
	}
	return len(found) == 1 && found[wantURL]
}

func jsonCollectCanonicalEndpoints(v any, found map[string]bool) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			jsonCollectCanonicalEndpoints(item, found)
		}
	case map[string]any:
		for key, item := range x {
			if endpointField(normalizeEndpointField(key)) {
				if raw, ok := item.(string); ok {
					if got, valid := canonicalMCPEndpoint(raw); valid {
						found[got] = true
					}
				}
			}
			jsonCollectCanonicalEndpoints(item, found)
		}
	}
}

func normalizeEndpointField(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.NewReplacer("_", "", "-", "", ".", "").Replace(key)
}

func endpointField(key string) bool {
	switch key {
	case "url", "endpoint", "remoteurl", "serverurl":
		return true
	default:
		return false
	}
}

func canonicalMCPEndpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.User != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Fragment != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host
	if port != "" {
		u.Host += ":" + port
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = u.Query().Encode()
	u.RawFragment = ""
	return u.String(), true
}

// AllPreloadedMCP returns, order-preserving and de-duplicated, every non-empty
// name in `servers` — the full set to attach EAGERLY at create (emitted to sbx
// as --static-mcp; their tools sit in context from the start). There is no
// eager/lazy split: every configured server, and every pack integration's
// server, preloads at CREATE regardless of kind, so this is pure list hygiene.
func AllPreloadedMCP(servers []string) []string {
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

// LocalMCPNames asks the pix-host binary which MCP servers it can serve
// locally (`pix-host mcp --list`), the source of truth for local vs remote
// registration. It returns the set of names and whether the list was obtained.
// A missing binary or a failed call returns (nil,false); the caller then FAILS
// CLOSED — it registers only gog (a known special case) and SKIPS every other
// requested name rather than risk registering a remote gateway-catalog name as a
// local pix-host subcommand.
func LocalMCPNames(env hostenv.Env, hostResolver func() (string, error)) (map[string]bool, bool) {
	if hostResolver == nil {
		return nil, false
	}
	hb, err := hostResolver()
	if err != nil || hb == "" {
		return nil, false
	}
	// BOUNDED: a hung `pix-host mcp --list` degrades to an unknown local
	// set (callers fail closed), never a wedged caller.
	out, timedOut, err := env.RunTimed(hb, "mcp", "--list")
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

// mcpAuthResult is the outcome McpAuthStatus classifies a `sbx mcp auth
// status <name>` probe into. mcpAuthUnknown covers output doctor cannot
// confidently parse as either a pass or a fail — it must never guess (a
// misread failure would recommend a repair command that doesn't apply, and a
// misread success would silently hide a real auth gap).
type mcpAuthResult int

// McpAuthStatus parses `sbx mcp auth status <name>` output (name-scoped: sbx
// prints only this server's state) into the tri-state above. It is
// deliberately lenient about exact wording (this is a passthrough to sbx, not
// a format pix controls — see runMcpAuth) but conservative about
// ambiguity: a negative phrase anywhere wins over a positive one, and neither
// present at all is unknown rather than a guess.
func McpAuthStatus(out string) mcpAuthResult {
	lower := strings.ToLower(out)
	for _, neg := range []string{"not authenticated", "unauthenticated", "not authorized", "unauthorized", "needs auth", "not logged in", "expired", "no token", "401"} {
		if strings.Contains(lower, neg) {
			return McpAuthFailed
		}
	}
	for _, pos := range []string{"authenticated", "authorized", "logged in", " ok", "\tok"} {
		if strings.Contains(lower, pos) {
			return McpAuthOK
		}
	}
	if strings.TrimSpace(lower) == "ok" {
		return McpAuthOK
	}
	return mcpAuthUnknown
}

const (
	mcpAuthUnknown mcpAuthResult = iota
	McpAuthOK
	McpAuthFailed
)

// catalogMCPReadiness classifies one catalog remote's launch readiness.
type catalogMCPReadiness int

const (
	CatalogMCPReady        catalogMCPReadiness = iota // registered + authorized
	CatalogMCPUnregistered                            // a successful listing positively lacks it
	CatalogMCPUnauthorized                            // registered, auth positively missing/expired
	CatalogMCPDenied                                  // registered, EXPLICIT policy denial
	catalogMCPUnverifiable                            // probe failed/timed out — unknown, never a guess
)

// CatalogMCPState resolves one catalog name's readiness from the shared
// registration evidence (McpRegEvidenceFrom over an already-fetched bounded
// `sbx mcp ls`) plus a bounded native `sbx mcp auth status <name>` probe —
// the SAME classification doctor's mcpRemoteAuthCheck applies, so the gate
// and doctor can never disagree about what "auth-ready" means.
func CatalogMCPState(env hostenv.Env, mcpOut string, mcpOK bool, name string) catalogMCPReadiness {
	switch McpRegEvidenceFrom(mcpOut, mcpOK, name) {
	case McpRegNo:
		return CatalogMCPUnregistered
	case McpRegUnknown:
		return catalogMCPUnverifiable
	}
	return remoteMCPAuthorizationState(env, name)
}

// remoteMCPAuthorizationState classifies the native sbx OAuth status for an
// already-registered remote. Keeping this separate lets pack activation repair
// a positively unauthorized registration in the same interactive transaction,
// without reopening OAuth when the credential is already healthy.
func remoteMCPAuthorizationState(env hostenv.Env, name string) catalogMCPReadiness {
	out, timedOut, err := env.RunTimed("sbx", "mcp", "auth", "status", name)
	if timedOut {
		return catalogMCPUnverifiable
	}
	// EXPLICIT denial signals win regardless of exit code: a policy denial is
	// a positive refusal, not a credential gap.
	if sys.ClassifyProbeFailure(out, err) == sys.ProbeDenied {
		return CatalogMCPDenied
	}
	if err != nil {
		// The native status parser recognizes credential-specific evidence such
		// as "expired" and "not logged in" that a generic process-failure
		// classifier cannot enumerate. Preserve that positive evidence even when
		// sbx correctly exits nonzero for the missing/expired credential.
		if McpAuthStatus(out) == McpAuthFailed || sys.ClassifyProbeFailure(out, err) == sys.ProbeAuthTodo {
			return CatalogMCPUnauthorized
		}
		return catalogMCPUnverifiable
	}
	switch McpAuthStatus(out) {
	case McpAuthOK:
		return CatalogMCPReady
	case McpAuthFailed:
		return CatalogMCPUnauthorized
	default: // mcpAuthUnknown
		return catalogMCPUnverifiable
	}
}

// KnownRemoteURL reports the endpoint pix already knows for name, if any. The
// ONE reader of McpCatalog outside classification: `pix mcp add <name>` uses it
// to skip asking for a URL it could have looked up.
func KnownRemoteURL(name string) (string, bool) {
	for _, c := range McpCatalog {
		if c.Name == name {
			return c.URL, true
		}
	}
	return "", false
}
