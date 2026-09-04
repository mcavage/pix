package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/container"
	"pix/host/hostenv"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/sys/systest"
	"pix/host/workflow/provision"
	"pix/host/workflow/reset"
)

// memoryEmbedEnvAt fakes an ollama daemon at OLLAMA_HOST for
// memoryContainerEnv's own detection call.
func memoryEmbedEnvAt(t *testing.T, body string) hostenv.Env {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	fake := &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		GetenvFn:   func(name string) string { return u.Host },
	}
	return hostenv.Env{System: fake}
}

// TestMemoryContainerEnv_LocalOllamaTranslatesToHostDockerInternal is the
// bug this pass fixes: the pix-memory CONTAINER's own 127.0.0.1 is itself,
// never the host, so a bare copy of the host's default loopback endpoint
// would silently point the container at nothing.
func TestMemoryContainerEnv_LocalOllamaTranslatesToHostDockerInternal(t *testing.T) {
	env := memoryEmbedEnvAt(t, `{"models":[]}`)
	envVars, hosts := memoryContainerEnv(&config.Config{MemoryEmbedModel: "nomic-embed-text"}, env)
	if !strings.HasPrefix(envVars["OLLAMA_HOST"], "host.docker.internal:") {
		t.Errorf("OLLAMA_HOST = %q, want a host.docker.internal translation", envVars["OLLAMA_HOST"])
	}
	if envVars["MEMORY_EMBED_MODEL"] != "nomic-embed-text" {
		t.Errorf("MEMORY_EMBED_MODEL = %q", envVars["MEMORY_EMBED_MODEL"])
	}
	found := false
	for _, h := range hosts {
		if h == "host.docker.internal:host-gateway" {
			found = true
		}
	}
	if !found {
		t.Errorf("ExtraHosts = %v, want the host-gateway mapping for Linux Docker", hosts)
	}
}

// TestMemoryContainerEnv_RemoteOllamaPassesThroughUnchanged proves a remote
// endpoint (a shared team daemon, or a proxied Ollama Cloud account) is
// never rewritten and never gets the host-gateway mapping — that address is
// already reachable from a container exactly as it is from this host.
func TestMemoryContainerEnv_RemoteOllamaPassesThroughUnchanged(t *testing.T) {
	fake := &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		GetenvFn:   func(name string) string { return "team-ollama.internal:11434" },
	}
	envVars, hosts := memoryContainerEnv(&config.Config{MemoryEmbedModel: "nomic-embed-text"}, hostenv.Env{System: fake})
	if envVars["OLLAMA_HOST"] != "team-ollama.internal:11434" {
		t.Errorf("OLLAMA_HOST = %q, want the remote endpoint unchanged", envVars["OLLAMA_HOST"])
	}
	if len(hosts) != 0 {
		t.Errorf("ExtraHosts = %v, want none for a remote endpoint", hosts)
	}
}

// TestMemoryContainerEnv_NilSystemIsAGuardedNoOp: a bare setupSeams{} (no
// Env wired) must never panic and must never make a network call — it just
// gets no OLLAMA_HOST at all, with MEMORY_EMBED_MODEL still defaulted.
func TestMemoryContainerEnv_NilSystemIsAGuardedNoOp(t *testing.T) {
	envVars, hosts := memoryContainerEnv(nil, hostenv.Env{})
	if envVars["MEMORY_EMBED_MODEL"] != config.DefaultMemoryEmbedModel {
		t.Errorf("MEMORY_EMBED_MODEL = %q, want the default %q", envVars["MEMORY_EMBED_MODEL"], config.DefaultMemoryEmbedModel)
	}
	if _, ok := envVars["OLLAMA_HOST"]; ok {
		t.Error("OLLAMA_HOST must not be set when no env seam is wired")
	}
	if len(hosts) != 0 {
		t.Errorf("ExtraHosts = %v, want none", hosts)
	}
}

func TestHomeContainerSpecUsesCanonicalReleaseImageReference(t *testing.T) {
	home := pixhome.New(t.TempDir())
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := release.Manifest{Version: "1.2.3", PixAgentDigest: digest, PixMemoryDigest: digest, RuntimeDigest: digest, KitRevision: "abcdef1"}
	if err := release.SaveInstalled(home.Home, manifest); err != nil {
		t.Fatal(err)
	}
	if got, want := homeContainerSpec(home).Image, provision.MemoryImageRef(manifest); got != want {
		t.Fatalf("container image = %q, want canonical release reference %q", got, want)
	}
}

// TestHomeContainerSpec_TwoHomesYieldDistinctScopedNames is the Wave B U4
// TDD anchor at the actual production caller: two different PIX_HOME roots
// must resolve to two different, non-legacy pix-memory container names.
func TestHomeContainerSpec_TwoHomesYieldDistinctScopedNames(t *testing.T) {
	homeA := pixhome.New(t.TempDir())
	homeB := pixhome.New(t.TempDir())
	nameA := homeContainerSpec(homeA).ContainerName
	nameB := homeContainerSpec(homeB).ContainerName
	if nameA == "" || nameB == "" {
		t.Fatalf("expected non-empty scoped names, got %q and %q", nameA, nameB)
	}
	if nameA == nameB {
		t.Fatalf("two different PIX_HOME roots must yield distinct container names, both got %q", nameA)
	}
	if nameA == container.Name || nameB == container.Name {
		t.Fatalf("a scoped container name must never equal the bare legacy name %q", container.Name)
	}
	if !strings.HasPrefix(nameA, "pix-memory-") || !strings.HasPrefix(nameB, "pix-memory-") {
		t.Fatalf("expected pix-memory-<stack id> shaped names, got %q and %q", nameA, nameB)
	}
	// StackID/Home must be stamped on the spec too, since Reconcile's foreign-
	// owner check reads them, not just the ContainerName string.
	if homeContainerSpec(homeA).StackID == "" || homeContainerSpec(homeA).StackID == homeContainerSpec(homeB).StackID {
		t.Fatalf("expected distinct, non-empty StackID per home")
	}
}

// TestBuiltinMCPFactsUsesScopedNames proves run_env.go's own builtinMCPFacts
// resolves THIS PIX_HOME's own scoped names, never a bare legacy fallback,
// and that two different PIX_HOMEs diverge exactly the way
// TestHomeContainerSpec_TwoHomesYieldDistinctScopedNames proves for the
// container name.
// TestHomeMemoryProber_CarriesThePersistedToken is the doctor-side wiring
// regression for the reported false "pix-memory unhealthy" report: once
// `pix setup` has generated this home's bearer token, `pix doctor`'s own
// Prober must carry it — a bare, tokenless container.HTTPProber{} against a
// real auth-requiring pix-memory container is exactly what produced "mcp
// initialize at http://127.0.0.1:<port>/mcp returns 401".
func TestHomeMemoryProber_CarriesThePersistedToken(t *testing.T) {
	home := pixhome.New(t.TempDir())
	tok, err := container.EnsureMemoryAuthToken(home)
	if err != nil {
		t.Fatalf("EnsureMemoryAuthToken: %v", err)
	}
	if got := homeMemoryProber(home).Token; got != tok {
		t.Fatalf("homeMemoryProber(home).Token = %q, want the persisted token %q", got, tok)
	}
}

// TestHomeMemoryProber_NoTokenYetIsUnauthenticated proves the pre-`pix
// setup` posture stays exactly what it always was: no token file yet means
// no token attached, matching homeContainerSpec's own AuthTokenFile
// ("points at a file that may not exist yet") posture — never a crash and
// never a fabricated value.
func TestHomeMemoryProber_NoTokenYetIsUnauthenticated(t *testing.T) {
	home := pixhome.New(t.TempDir())
	if got := homeMemoryProber(home).Token; got != "" {
		t.Fatalf("homeMemoryProber(home).Token = %q, want empty before pix setup has run", got)
	}
}

func TestBuiltinMCPFactsUsesScopedNames(t *testing.T) {
	homeA := t.TempDir()
	t.Setenv("PIX_HOME", homeA)
	factsA := builtinMCPFacts()
	if factsA.MemoryName == "" || factsA.SessionName == "" {
		t.Fatalf("expected resolved scoped names, got %+v", factsA)
	}
	if factsA.MemoryName == "pix-memory" || factsA.SessionName == "pix-session" {
		t.Fatalf("builtinMCPFacts must never fall back to the bare legacy names, got %+v", factsA)
	}

	homeB := t.TempDir()
	t.Setenv("PIX_HOME", homeB)
	factsB := builtinMCPFacts()
	if factsA.MemoryName == factsB.MemoryName || factsA.SessionName == factsB.SessionName {
		t.Fatalf("two different PIX_HOME roots must diverge, both got %+v", factsA)
	}
}

func installSbxRegistrarFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sbx")
	script := "#!/bin/sh\nd='" + dir + "'\n" + body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

type sequenceProber struct {
	calls int
	fails int
}

func (p *sequenceProber) Probe(string) error {
	p.calls++
	if p.calls <= p.fails {
		return fmt.Errorf("not ready")
	}
	return nil
}

func TestProductionSetupUsesFullMCPStartupProbe(t *testing.T) {
	p, ok := productionSetupSeams().Prober.(startupProber)
	if !ok {
		t.Fatalf("setup prober = %T, want startupProber", productionSetupSeams().Prober)
	}
	if _, ok := p.Inner.(container.HTTPProber); !ok {
		t.Fatalf("startup inner prober = %T, want container.HTTPProber", p.Inner)
	}
}

// TestStartupProberWithTokenReachesTheRealHTTPProber proves
// container.WithToken(startupProber{...}, tok) authenticates the actual
// container.HTTPProber it wraps — the exact seam provision.Setup uses to
// hand its own generated token through to `pix setup`'s startup-retry
// prober, resolved long after productionSetupSeams() built it tokenless.
func TestStartupProberWithTokenReachesTheRealHTTPProber(t *testing.T) {
	p := startupProber{Inner: container.HTTPProber{}}
	authenticated := container.WithToken(p, "tok-123")
	sp, ok := authenticated.(startupProber)
	if !ok {
		t.Fatalf("WithToken returned %T, want startupProber", authenticated)
	}
	hp, ok := sp.Inner.(container.HTTPProber)
	if !ok {
		t.Fatalf("startupProber.Inner = %T, want container.HTTPProber", sp.Inner)
	}
	if hp.Token != "tok-123" {
		t.Fatalf("inner HTTPProber.Token = %q, want %q", hp.Token, "tok-123")
	}
}

func TestStartupProberRefusesNilInner(t *testing.T) {
	if err := (startupProber{}).Probe("http://127.0.0.1:18080"); err == nil {
		t.Fatal("expected nil inner prober to return an error")
	}
}

func TestStartupProberRetriesUntilMemoryIsReady(t *testing.T) {
	inner := &sequenceProber{fails: 2}
	notices := 0
	p := startupProber{Inner: inner, Timeout: time.Second, Interval: time.Millisecond, OnRetry: func(time.Duration) { notices++ }}
	if err := p.Probe("http://127.0.0.1:18080"); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if inner.calls != 3 {
		t.Fatalf("calls = %d, want 3", inner.calls)
	}
	if notices != 1 {
		t.Fatalf("retry notices = %d, want exactly 1", notices)
	}
}

func TestStartupProberStopsAtDeadline(t *testing.T) {
	inner := &sequenceProber{fails: 100}
	p := startupProber{Inner: inner, Timeout: 10 * time.Millisecond, Interval: time.Millisecond}
	if err := p.Probe("http://127.0.0.1:18080"); err == nil {
		t.Fatal("expected readiness timeout")
	}
	if inner.calls < 2 || inner.calls > 20 {
		t.Fatalf("calls = %d, want bounded retries", inner.calls)
	}
}

func TestSbxMemoryRegistrarExplicitlyAllowsOwnedLoopbackEndpoint(t *testing.T) {
	dir := installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then exit 0; fi
printf '%s\n' "$@" > "$d/argv"
`)
	receipt := filepath.Join(dir, "argv")

	const endpoint = "http://127.0.0.1:18080/mcp?token=secret"
	state, err := (sbxMemoryRegistrar{}).EnsureMemoryRemote("pix-memory", endpoint)
	if err != nil {
		t.Fatalf("EnsureMemoryRemote: %v", err)
	}
	if state != provision.MCPRegistrationAdded {
		t.Fatalf("state = %v, want added", state)
	}
	data, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	want := []string{"mcp", "add", "pix-memory", "--url", endpoint, "--skip-ssrf-check", "--skip-auth"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sbx argv = %#v, want %#v", got, want)
	}
}

// TestAddMemoryRemoteUsesHyphenatedSkipAuthFlag pins the exact `sbx mcp add`
// auth-skip spelling. sbx once rejected the earlier `--skip_auth` (underscored)
// spelling outright, which silently broke every fresh memory registration on
// current sbx builds; this test exists so a future respelling of either flag
// fails a unit test instead of a live `pix setup`.
func TestAddMemoryRemoteUsesHyphenatedSkipAuthFlag(t *testing.T) {
	dir := installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then exit 0; fi
printf '%s\n' "$@" > "$d/argv"
`)
	const endpoint = "http://127.0.0.1:18080/mcp?token=secret"
	state, err := (sbxMemoryRegistrar{}).EnsureMemoryRemote("pix-memory", endpoint)
	if err != nil {
		t.Fatalf("EnsureMemoryRemote: %v", err)
	}
	if state != provision.MCPRegistrationAdded {
		t.Fatalf("state = %v, want added", state)
	}
	data, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Fields(string(data))
	if !slices.Contains(argv, "--skip-auth") {
		t.Fatalf("sbx argv = %#v, want it to contain --skip-auth", argv)
	}
	if slices.Contains(argv, "--skip_auth") {
		t.Fatalf("sbx argv = %#v, still uses the rejected underscored --skip_auth spelling", argv)
	}
}

// TestMemorySourceNeverReintroducesUnderscoredSkipAuth is a compatibility
// guard at the source-text level: even a caller this test file doesn't know
// about (a future addMemoryRemote rewrite, a copy-pasted helper) cannot slip
// the rejected `--skip_auth` spelling back in without failing here.
func TestMemorySourceNeverReintroducesUnderscoredSkipAuth(t *testing.T) {
	src, err := os.ReadFile("homeadapters.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "--skip_auth") {
		t.Fatal("homeadapters.go contains the rejected --skip_auth flag spelling; sbx requires --skip-auth")
	}
}

func TestSbxMemoryRegistrarReplacesMismatchedOwnedScopedEndpoint(t *testing.T) {
	const scopedName = "pix-memory-0123456789abcdef"
	dir := installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then echo "`+scopedName+` remote"; exit 0; fi
if [ "$1 $2" = "mcp inspect" ]; then echo '{"url":"http://127.0.0.1:9999/mcp"}'; exit 0; fi
if [ "$1 $2" = "mcp rm" ]; then printf '%s\n' "$@" > "$d/rmargv"; exit 0; fi
if [ "$1 $2" = "mcp add" ]; then printf '%s\n' "$@" > "$d/addargv"; exit 0; fi
exit 1
`)
	state, err := (sbxMemoryRegistrar{}).EnsureMemoryRemote(scopedName, "http://127.0.0.1:18080/mcp")
	if err != nil || state != provision.MCPRegistrationAdded {
		t.Fatalf("EnsureMemoryRemote = (%v, %v), want re-added", state, err)
	}
	for _, f := range []string{"rmargv", "addargv"} {
		if _, statErr := os.Stat(filepath.Join(dir, f)); statErr != nil {
			t.Errorf("expected sbx %s during owned registration reconcile: %v", strings.TrimSuffix(f, "argv"), statErr)
		}
	}
}

// TestSbxMemoryRegistrarPresentWhenEndpointMatches proves a same scoped name
// registered at the SAME endpoint is reported present-VERIFIED (not re-added,
// not refused): the one already-fine answer, and a read-back earned it.
func TestSbxMemoryRegistrarPresentWhenEndpointMatches(t *testing.T) {
	const scopedName = "pix-memory-0123456789abcdef"
	installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then echo "`+scopedName+` remote"; exit 0; fi
if [ "$1 $2" = "mcp inspect" ]; then echo '{"url":"http://127.0.0.1:18080/mcp"}'; exit 0; fi
exit 1
`)
	state, err := (sbxMemoryRegistrar{}).EnsureMemoryRemote(scopedName, "http://127.0.0.1:18080/mcp")
	if err != nil {
		t.Fatalf("EnsureMemoryRemote: %v", err)
	}
	if state != provision.MCPRegistrationPresentVerified {
		t.Fatalf("state = %v, want present-verified", state)
	}
}

// A scoped name is owned by this PIX_HOME. When sbx cannot read its endpoint,
// remove and re-add that exact name so setup can earn a current registration.
func TestSbxMemoryRegistrarReplacesUnverifiedOwnedRegistration(t *testing.T) {
	const scopedName = "pix-memory-0123456789abcdef"
	dir := installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then echo "`+scopedName+` remote"; exit 0; fi
if [ "$1 $2" = "mcp add" ]; then printf '%s\n' "$@" > "$d/addargv"; exit 0; fi
if [ "$1 $2" = "mcp rm" ]; then printf '%s\n' "$@" > "$d/rmargv"; exit 0; fi
exit 1
`)
	state, err := (sbxMemoryRegistrar{}).EnsureMemoryRemote(scopedName, "http://127.0.0.1:18080/mcp")
	if err != nil {
		t.Fatalf("EnsureMemoryRemote: %v", err)
	}
	if state != provision.MCPRegistrationAdded {
		t.Fatalf("state = %v, want re-added", state)
	}
	for _, f := range []string{"addargv", "rmargv"} {
		if _, statErr := os.Stat(filepath.Join(dir, f)); statErr != nil {
			t.Errorf("expected sbx %s during owned registration reconcile: %v", strings.TrimSuffix(f, "argv"), statErr)
		}
	}
}

// TestSetupReportsUnverifiedRegistrationAsNotReady wires the state through to
// the surface a user sees: `pix setup` names the registration, says the
// endpoint could not be read, states that nothing was touched, gives the
// exact commands, and does NOT print the ready line.
func TestSetupReportsUnverifiedRegistrationAsNotReady(t *testing.T) {
	const scopedName = "pix-memory-0123456789abcdef"
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	res := provision.Result{
		MCPRegistered: true,
		MCPState:      provision.MCPRegistrationPresentUnverified,
		MCPName:       scopedName,
	}
	renderSetupResult(d, pixhome.New(t.TempDir()), res, false)
	got := out.String()
	for _, want := range []string{
		scopedName,
		"could not be read",
		"nothing overwritten or removed",
		"sbx mcp rm " + scopedName,
		"pix setup: not ready.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("setup output is missing %q:\n%s", want, got)
		}
	}
	// "read back and matched" is the verified branch's own wording; the bare
	// word "verified" is not searchable here because the temp PIX_HOME path
	// carries this test's own name.
	for _, forbidden := range []string{"pix setup: ready", "read back and matched"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("setup output claims %q for a registration nothing probed:\n%s", forbidden, got)
		}
	}
	if res.Ready() {
		t.Error("Ready() = true for an unverifiable registration")
	}
}

// TestSbxMemoryDeregistrar_RemovesOnlyWhenPresent proves the reset-side
// production adapter proves presence before ever calling `mcp rm`, and
// reports absence honestly rather than guessing.
func TestSbxMemoryDeregistrar_RemovesOnlyWhenPresent(t *testing.T) {
	const scopedName = "pix-memory-0123456789abcdef"
	dir := installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then echo "`+scopedName+` remote"; exit 0; fi
if [ "$1 $2" = "mcp rm" ]; then printf '%s\n' "$@" > "$d/rmargv"; exit 0; fi
exit 1
`)
	state, err := (sbxMemoryDeregistrar{}).RemoveIfRegistered(scopedName)
	if err != nil {
		t.Fatalf("RemoveIfRegistered: %v", err)
	}
	if state != reset.MCPRemovalRemoved {
		t.Fatalf("state = %v, want Removed", state)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "rmargv")); statErr != nil {
		t.Fatal("expected `sbx mcp rm` to have run")
	}

	// A name absent from `mcp ls` must never reach `mcp rm` at all.
	installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then echo unrelated-name; exit 0; fi
exit 1
`)
	state, err = (sbxMemoryDeregistrar{}).RemoveIfRegistered(scopedName)
	if err != nil {
		t.Fatalf("RemoveIfRegistered: %v", err)
	}
	if state != reset.MCPRemovalAbsent {
		t.Fatalf("state = %v, want Absent", state)
	}
}

func TestSbxMemoryRegistrarReportsFailureWithoutLeakingToken(t *testing.T) {
	const token = "0123456789abcdef"
	installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then exit 0; fi
printf 'SSRF guard refused loopback; rejected URL %s\n' "$5" >&2
exit 1
`)
	_, err := (sbxMemoryRegistrar{}).EnsureMemoryRemote("pix-memory", "http://127.0.0.1:18080/mcp?token="+token)
	if err == nil {
		t.Fatal("EnsureMemoryRemote succeeded, want failure")
	}
	got := err.Error()
	if strings.Contains(got, token) {
		t.Fatalf("error leaked token: %s", got)
	}
	if !strings.Contains(got, container.RedactedTokenPlaceholder) || !strings.Contains(got, "SSRF guard refused loopback") {
		t.Fatalf("error = %q, want redacted sbx stderr", got)
	}
}
