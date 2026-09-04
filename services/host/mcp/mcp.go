package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"

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
const mcpLsAttachmentNote = "\nNote: this is the gateway's HOST registration list. A ✓ here means REGISTERED,\n" +
	"not working — the gateway does not check whether a server can authenticate or\n" +
	"even whether its command still exists. `pix doctor` checks that.\n" +
	"\nIt is also not what's attached to your current sandbox. Neither `pix status`\n" +
	"nor `pix doctor` can see inside a live session, so neither can answer that. A\n" +
	"sandbox picks up everything registered when it starts, so `pix rm <box>` then\n" +
	"`pix run` is how a running one catches up.\n"

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

// VerifyExistingEndpoint inspects an ALREADY-PRESENT registration under name
// (via `sbx mcp inspect NAME`, falling back to `sbx mcp get NAME`) and
// reports whether its canonical endpoint equals want. verified is false when
// NEITHER verb produced a readable answer — the caller must then treat the
// registration as unverifiable (this host genuinely cannot tell), never as a
// match or a mismatch. This is the exported form of
// catalogEntryMatchesShippedURL's own inspect/get probe, for a caller (e.g.
// cmd/pix's pix-memory registrar) outside this package that needs the same
// evidence without importing the catalog-specific classification around it.
func VerifyExistingEndpoint(name, want string) (matches bool, verified bool) {
	for _, verb := range []string{"inspect", "get"} {
		stdout, _, err := runSbxCaptured([]string{"mcp", verb, name})
		if err == nil {
			return outputContainsCanonicalEndpoint(stdout, want), true
		}
	}
	return false, false
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

// OpRunWrap is the ONE op-run wrapper grammar pix generates. It exists as a
// shared function, not an inline string, because two callers must produce
// byte-identical commands: registration (what the gateway will spawn) and
// doctor's health probe (what we claim to have verified).
//
// That equality is the whole point. The failure this codebase was rebuilt
// around is a credential that works in your terminal and not in the gateway's
// environment — so a probe that does not go through this wrapper proves
// nothing about the thing it claims to check. Returns argv unchanged when
// 1Password is not configured, which is a legitimate no-credential setup.
func OpRunWrap(opPath, opRefs string, argv []string) []string {
	if opPath == "" || opRefs == "" || len(argv) == 0 {
		return argv
	}
	return append([]string{opPath, "run", "--no-masking", "--env-file=" + opRefs, "--"}, argv...)
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

// LocalMCPNames is GONE, and its absence is the point. It asked `pix-host mcp
// --list` which servers this binary could serve, so that a name could be
// classified local-vs-remote at runtime. That list has been empty since the
// last built-in server was externalized, which made the classifier an
// unanswerable question every caller then had to fail closed around — a whole
// subsystem (the bridge, the resolver threading, the unknown-set fallbacks)
// serving a set that is always empty. Servers are declared by the active
// pack's manifest now, so classification is a map lookup on data a reviewer
// already consented to, with no subprocess and no ambient host state.

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
	// "does not require OAuth" is a DEFINITE answer, and it must be read before
	// the negatives below -- it contains "not", and a server that needs no OAuth
	// is not an unauthenticated one. Without this case the answer fell through to
	// unknown, which is how `pix doctor` reported a permanent `?` for two servers
	// sbx had described perfectly clearly: slack and google-docs-create take
	// their credentials from the gateway's op-refs injection, so there is no
	// OAuth to have. A row that can only ever be `?` teaches you to stop reading
	// the glyph.
	for _, na := range []string{"does not require oauth", "no oauth required", "oauth not required"} {
		if strings.Contains(lower, na) {
			return McpAuthNotRequired
		}
	}
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
	// McpAuthNotRequired is "asked and answered: this server has no OAuth".
	// Distinct from McpAuthOK because it is a different FACT, and distinct from
	// unknown because it is not an absence of information.
	McpAuthNotRequired
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
