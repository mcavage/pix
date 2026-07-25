package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadAbsentReturnsDefaults(t *testing.T) {
	// Point at a path that does not exist.
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "nope.toml"))

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() absent file: got err %v, want nil", err)
	}
	if want := []string{"memory"}; len(c.Services) != len(want) || c.Services[0] != want[0] {
		t.Errorf("Services = %v, want %v", c.Services, want)
	}
	if c.MemoryWatcherModel != DefaultMemoryWatcherModel {
		t.Errorf("MemoryWatcherModel = %q, want %q", c.MemoryWatcherModel, DefaultMemoryWatcherModel)
	}
	if c.MemoryEmbedModel != DefaultMemoryEmbedModel {
		t.Errorf("MemoryEmbedModel = %q, want %q", c.MemoryEmbedModel, DefaultMemoryEmbedModel)
	}
	if got := c.Plugin("anything"); got.Impl != BuiltinImpl {
		t.Errorf("Plugin(absent).Impl = %q, want %q", got.Impl, BuiltinImpl)
	}
}

func TestLoadDecodesTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
version_pin = "0.0.1"
services = ["memory", "warehouse"]
mcp = ["slack"]
memory_watcher_model = "custom-watcher"
memory_embed_model = "custom-embed"
gog_account = "you@example.com"

[kits]
stack = ["overlay-a", "overlay-b"]

[skills]
paths = ["/tmp/skills"]

[plugins.memory]
impl = "external"
path = "/opt/mem"
sha  = "deadbeef"
port = 9000

[plugins.slack]
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", path)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() decode: %v", err)
	}
	if c.VersionPin != "0.0.1" {
		t.Errorf("VersionPin = %q, want 0.0.1", c.VersionPin)
	}
	if len(c.Services) != 2 || c.Services[1] != "warehouse" {
		t.Errorf("Services = %v", c.Services)
	}
	if len(c.MCP) != 1 || c.MCP[0] != "slack" {
		t.Errorf("MCP = %v", c.MCP)
	}
	if c.MemoryWatcherModel != "custom-watcher" || c.MemoryEmbedModel != "custom-embed" {
		t.Errorf("models = %q/%q", c.MemoryWatcherModel, c.MemoryEmbedModel)
	}
	if c.GogAccount != "you@example.com" {
		t.Errorf("GogAccount = %q, want you@example.com", c.GogAccount)
	}
	if len(c.Kits.Stack) != 2 || c.Kits.Stack[1] != "overlay-b" {
		t.Errorf("Kits.Stack = %v", c.Kits.Stack)
	}
	if len(c.Skills.Paths) != 1 || c.Skills.Paths[0] != "/tmp/skills" {
		t.Errorf("Skills.Paths = %v", c.Skills.Paths)
	}
	mem := c.Plugin("memory")
	if mem.Impl != "external" || mem.Path != "/opt/mem" || mem.SHA != "deadbeef" || mem.Port != 9000 {
		t.Errorf("Plugin(memory) = %+v", mem)
	}
	// A slot with no impl set defaults to builtin.
	if got := c.Plugin("slack"); got.Impl != BuiltinImpl {
		t.Errorf("Plugin(slack).Impl = %q, want %q", got.Impl, BuiltinImpl)
	}
}

func TestSeedCreatesThenRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")

	created, err := Seed(path)
	if err != nil {
		t.Fatalf("Seed() first: %v", err)
	}
	if !created {
		t.Errorf("Seed() first: created = false, want true")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Seed() did not write file: %v", err)
	}

	// Second call must not clobber.
	before, _ := os.ReadFile(path)
	created, err = Seed(path)
	if err != nil {
		t.Fatalf("Seed() second: %v", err)
	}
	if created {
		t.Errorf("Seed() second: created = true, want false (must not clobber)")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("Seed() second: file was modified")
	}

	// The seeded file must decode cleanly.
	t.Setenv("PI_STACK_CONFIG", path)
	if _, err := Load(); err != nil {
		t.Errorf("Load() seeded file: %v", err)
	}
}

// TestSeedOpRefsAtNoClobberAndDirPerms covers F5: SeedOpRefsAt is atomic
// no-clobber (an existing file is never truncated) and it tightens an existing
// 0755 config dir to 0700.
func TestSeedOpRefsAtNoClobberAndDirPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "op-refs.env")

	created, err := SeedOpRefsAt(path)
	if err != nil {
		t.Fatalf("SeedOpRefsAt first: %v", err)
	}
	if !created {
		t.Errorf("SeedOpRefsAt first: created = false, want true")
	}
	// An existing 0755 dir must be tightened to 0700.
	if fi, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir perms = %04o, want 0700", fi.Mode().Perm())
	}
	// File must be 0600.
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat file: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("file perms = %04o, want 0600", fi.Mode().Perm())
	}

	// Populate the file with user content, then seed again: it must NOT truncate.
	const userContent = "SLACK_TOKEN=op://Private/Slack/credential\n"
	if err := os.WriteFile(path, []byte(userContent), 0o600); err != nil {
		t.Fatalf("write user content: %v", err)
	}
	created, err = SeedOpRefsAt(path)
	if err != nil {
		t.Fatalf("SeedOpRefsAt second: %v", err)
	}
	if created {
		t.Errorf("SeedOpRefsAt second: created = true, want false (must not clobber)")
	}
	if got, _ := os.ReadFile(path); string(got) != userContent {
		t.Errorf("SeedOpRefsAt second truncated/modified existing file: %q", string(got))
	}
}

// TestSeedOpRefsAtConcurrent asserts that many concurrent seeders never truncate
// a populated op-refs.env: exactly one reports created, and the others leave the
// content intact.
func TestSeedOpRefsAtConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "op-refs.env")
	const n = 16
	var wg sync.WaitGroup
	createdCount := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			c, err := SeedOpRefsAt(path)
			if err != nil {
				t.Errorf("SeedOpRefsAt goroutine %d: %v", i, err)
			}
			createdCount[i] = c
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range createdCount {
		if c {
			created++
		}
	}
	if created != 1 {
		t.Errorf("exactly one seeder should report created, got %d", created)
	}
	// The file must equal the template exactly (never a truncated/partial write).
	if got, _ := os.ReadFile(path); string(got) != OpRefsTemplate {
		t.Errorf("seeded file does not equal OpRefsTemplate after concurrent seed")
	}
}

// TestOpRefsTemplateHasNoActiveRefs covers F1 at the source: the seed template
// must have ZERO active (uncommented) KEY=VALUE lines.
func TestOpRefsTemplateHasNoActiveRefs(t *testing.T) {
	for _, ln := range strings.Split(OpRefsTemplate, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.IndexByte(trimmed, '=') > 0 {
			t.Errorf("OpRefsTemplate has an active (uncommented) entry: %q", ln)
		}
	}
}

// TestRetiredKeysReportedAndNeverReemitted covers S01: mcp_static/mcp_dynamic
// were retired (all configured/pack MCP servers now preload at sandbox
// CREATE — no more eager/lazy split), but a config.toml written by an older
// pi-stack still has them. Load must not hard-fail, must surface them via
// RetiredKeys (not silently swallow them into UnknownKeys, which would read
// as "you made a typo"), and Save must never re-emit them — there is no
// field for them to round-trip through.
func TestRetiredKeysReportedAndNeverReemitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := `
mcp = ["slack"]
mcp_static = ["slack"]
mcp_dynamic = ["notion"]
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got, want := c.RetiredKeys(), []string{"mcp_dynamic", "mcp_static"}; !stringSlicesEqual(got, want) {
		t.Errorf("RetiredKeys() = %v, want %v", got, want)
	}
	if got := c.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %v, want none (retired keys are not unknown)", got)
	}
	if len(c.MCP) != 1 || c.MCP[0] != "slack" {
		t.Errorf("MCP = %v, want [slack] (live key still decodes)", c.MCP)
	}

	t.Setenv("PI_STACK_CONFIG", path)
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "mcp_static") || strings.Contains(string(saved), "mcp_dynamic") {
		t.Errorf("saved config still contains a retired key:\n%s", saved)
	}
	// Reloading the saved file reports no retired keys — the migration is
	// one-shot.
	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.RetiredKeys(); len(got) != 0 {
		t.Errorf("reloaded RetiredKeys() = %v, want none", got)
	}
}

// TestUnknownKeysSeparateFromRetired: a genuinely unrecognized key (typo or a
// field from a future version) is reported via UnknownKeys, not RetiredKeys —
// the two must never be conflated.
func TestUnknownKeysSeparateFromRetired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := `
mcp_static = ["slack"]
totally_made_up_key = "oops"
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got, want := c.RetiredKeys(), []string{"mcp_static"}; !stringSlicesEqual(got, want) {
		t.Errorf("RetiredKeys() = %v, want %v", got, want)
	}
	if got, want := c.UnknownKeys(), []string{"totally_made_up_key"}; !stringSlicesEqual(got, want) {
		t.Errorf("UnknownKeys() = %v, want %v", got, want)
	}
}

// TestLoadAbsentReportsNoRetiredOrUnknownKeys: a fresh install (no file) has
// nothing undecoded to report.
func TestLoadAbsentReportsNoRetiredOrUnknownKeys(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "nope.toml"))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RetiredKeys(); len(got) != 0 {
		t.Errorf("RetiredKeys() = %v, want none", got)
	}
	if got := c.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %v, want none", got)
	}
}

// TestSaveAtomic_WriteFailureLeavesPriorFileIntact (packs-v2 review finding
// #4): Save writes to a temp file + atomic rename rather than truncating
// config.toml in place, so a failed write never leaves the prior file
// half-written. Simulate a write failure by revoking write access to the
// config dir AFTER a first successful save, so the temp-file create itself
// fails.
func TestSaveAtomic_WriteFailureLeavesPriorFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", path)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.SetGogAccount("first@example.com")
	if err := c.Save(); err != nil {
		t.Fatalf("first Save(): %v", err)
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}

	// Revoke write access to the dir so os.CreateTemp fails inside Save.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // let TempDir clean up

	c.SetGogAccount("second@example.com")
	if err := c.Save(); err == nil {
		t.Fatal("expected Save() to fail with a read-only config dir")
	}

	_ = os.Chmod(dir, 0o700) // restore before reading
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config after failed Save: %v", err)
	}
	if string(got) != string(orig) {
		t.Errorf("a failed Save() modified the prior config file:\nbefore:\n%s\nafter:\n%s", orig, got)
	}
	// And no stray temp file was left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after failed Save: %s", e.Name())
		}
	}
}

func TestSaveAndMutators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	t.Setenv("PI_STACK_CONFIG", path)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.SetGogAccount("  you@example.com ") // trimmed
	if !c.AddMCP("gog") {
		t.Error("AddMCP(gog): want changed=true")
	}
	if c.AddMCP("gog") {
		t.Error("AddMCP(gog) twice: want changed=false (no duplicate)")
	}
	if !c.AddService("knowledge") {
		t.Error("AddService(knowledge): want changed=true")
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	// Machine-managed file mode is 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("saved config mode = %o, want 600", perm)
	}

	// Round-trip: reload gets the mutated values.
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.GogAccount != "you@example.com" {
		t.Errorf("GogAccount = %q, want you@example.com", got.GogAccount)
	}
	if len(got.MCP) != 1 || got.MCP[0] != "gog" {
		t.Errorf("MCP = %v, want [gog]", got.MCP)
	}
	if !contains(got.Services, "knowledge") {
		t.Errorf("Services = %v, want it to contain knowledge", got.Services)
	}

	// Remove mutators.
	if !got.RemoveMCP("gog") {
		t.Error("RemoveMCP(gog): want changed=true")
	}
	if got.RemoveMCP("gog") {
		t.Error("RemoveMCP(gog) twice: want changed=false")
	}
	if !got.RemoveService("knowledge") {
		t.Error("RemoveService(knowledge): want changed=true")
	}
}

// TestKnowledgeBundleMutators covers add/remove: idempotent, deduped,
// canonicalized to an absolute path, and preserved across a Save/Load round-trip.
func TestKnowledgeBundleMutators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	t.Setenv("PI_STACK_CONFIG", path)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	// A relative path is canonicalized to an absolute one.
	if !c.AddKnowledgeBundle("bundles/okf") {
		t.Error("AddKnowledgeBundle: want changed=true")
	}
	if len(c.KnowledgeBundles) != 1 || !filepath.IsAbs(c.KnowledgeBundles[0]) {
		t.Errorf("KnowledgeBundles = %v, want a single abs path", c.KnowledgeBundles)
	}
	abs, _ := filepath.Abs("bundles/okf")
	if c.KnowledgeBundles[0] != abs {
		t.Errorf("KnowledgeBundles[0] = %q, want %q", c.KnowledgeBundles[0], abs)
	}

	// Adding the same bundle (relative or with surrounding space) is a no-op.
	if c.AddKnowledgeBundle("bundles/okf") {
		t.Error("AddKnowledgeBundle twice: want changed=false (dedupe)")
	}
	if c.AddKnowledgeBundle("  bundles/okf  ") {
		t.Error("AddKnowledgeBundle trimmed dup: want changed=false")
	}
	if c.AddKnowledgeBundle("") {
		t.Error("AddKnowledgeBundle empty: want changed=false")
	}
	if len(c.KnowledgeBundles) != 1 {
		t.Errorf("KnowledgeBundles = %v, want exactly one entry", c.KnowledgeBundles)
	}

	if err := c.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.KnowledgeBundles) != 1 || got.KnowledgeBundles[0] != abs {
		t.Errorf("round-trip KnowledgeBundles = %v, want [%q]", got.KnowledgeBundles, abs)
	}

	// Remove by the same relative path the bundle was added with.
	if !got.RemoveKnowledgeBundle("bundles/okf") {
		t.Error("RemoveKnowledgeBundle: want changed=true")
	}
	if got.RemoveKnowledgeBundle("bundles/okf") {
		t.Error("RemoveKnowledgeBundle twice: want changed=false")
	}
	if len(got.KnowledgeBundles) != 0 {
		t.Errorf("KnowledgeBundles = %v, want empty after remove", got.KnowledgeBundles)
	}
}

// TestKnowledgeBundleCanonicalizationMatchesStore is the F6 guard: config's
// canonicalizeBundlePath must resolve a symlinked spelling to the SAME id as the
// real path (abs -> EvalSymlinks -> Clean), so a bundle added via a symlink
// dedupes against the real path and can be removed by either spelling.
func TestKnowledgeBundleCanonicalizationMatchesStore(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link-to-bundle")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// The real path and the symlinked spelling must canonicalize identically.
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalizeBundlePath(link); got != resolved {
		t.Fatalf("canonicalizeBundlePath(symlink) = %q, want %q (real path)", got, resolved)
	}
	if got := canonicalizeBundlePath(real); got != resolved {
		t.Fatalf("canonicalizeBundlePath(real) = %q, want %q", got, resolved)
	}

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Add via the real path, then adding via the symlink must dedupe (same id).
	if !c.AddKnowledgeBundle(real) {
		t.Fatal("AddKnowledgeBundle(real): want changed=true")
	}
	if c.AddKnowledgeBundle(link) {
		t.Fatal("AddKnowledgeBundle(symlink): want changed=false (dedupe against real path)")
	}
	if len(c.KnowledgeBundles) != 1 || c.KnowledgeBundles[0] != resolved {
		t.Fatalf("KnowledgeBundles = %v, want [%q]", c.KnowledgeBundles, resolved)
	}
	// Remove by the OTHER spelling (symlink) still removes the entry.
	if !c.RemoveKnowledgeBundle(link) {
		t.Fatal("RemoveKnowledgeBundle(symlink): want changed=true (remove by either spelling)")
	}
	if len(c.KnowledgeBundles) != 0 {
		t.Fatalf("KnowledgeBundles = %v, want empty after remove", c.KnowledgeBundles)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestEnsureToken(t *testing.T) {
	dir := t.TempDir()
	// TokenPath derives from configDir; PI_STACK_CONFIG's parent is the dir.
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))

	tok1, err := EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() first: %v", err)
	}
	if tok1 == "" {
		t.Fatal("EnsureToken() minted empty token")
	}

	tok2, err := EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() second: %v", err)
	}
	if tok1 != tok2 {
		t.Errorf("EnsureToken() not idempotent: %q != %q", tok1, tok2)
	}

	info, err := os.Stat(TokenPath())
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}

	// ReadToken returns the same value.
	got, err := ReadToken()
	if err != nil {
		t.Fatalf("ReadToken(): %v", err)
	}
	if got != tok1 {
		t.Errorf("ReadToken() = %q, want %q", got, tok1)
	}
}

// TestEnsureTokenConcurrent proves the first-run race is closed: N goroutines
// racing on a fresh config dir must all return the SAME token. Before the
// O_CREATE|O_EXCL election, concurrent first-runs could each mint a different
// value and the last writer would win, so the host and the VM could end up with
// different tokens and auth would fail.
func TestEnsureTokenConcurrent(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	toks := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once to maximize the race
			toks[i], errs[i] = EnsureToken()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureToken() goroutine %d: %v", i, err)
		}
		if toks[i] == "" {
			t.Fatalf("EnsureToken() goroutine %d: empty token", i)
		}
	}
	want := toks[0]
	for i, got := range toks {
		if got != want {
			t.Fatalf("EnsureToken() goroutine %d = %q, want %q (tokens diverged under concurrency)", i, got, want)
		}
	}

	// The persisted file must match the agreed token.
	got, err := ReadToken()
	if err != nil {
		t.Fatalf("ReadToken() after race: %v", err)
	}
	if got != want {
		t.Errorf("ReadToken() = %q, want %q", got, want)
	}
}

func TestReadTokenAbsent(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	if _, err := ReadToken(); err == nil {
		t.Error("ReadToken() absent: got nil err, want error")
	}
}

// TestServePidPath resolves serve.pid under the STATE dir (ephemeral runtime
// state, a sibling of serve.log), honoring $XDG_STATE_HOME, so the host writer
// and the launcher reader always agree on the location — and so `pi-stack reset`
// (which moves the CONFIG dir aside) never orphans a running daemon's pidfile.
func TestServePidPath(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	want := filepath.Join(xdg, "pi-stack", "serve.pid")
	if got := ServePidPath(); got != want {
		t.Errorf("ServePidPath() = %q, want %q", got, want)
	}
	// It must be a sibling of serve.log (the state dir), NOT the config dir.
	if filepath.Dir(ServePidPath()) != filepath.Dir(ServeLogPath()) {
		t.Errorf("ServePidPath dir %q != state dir %q", filepath.Dir(ServePidPath()), filepath.Dir(ServeLogPath()))
	}
	if filepath.Dir(ServePidPath()) == filepath.Dir(Path()) {
		t.Errorf("ServePidPath must NOT live in the config dir %q", filepath.Dir(Path()))
	}
}

// TestDataDirLayout locks the XDG data-root resolution: $XDG_DATA_HOME wins,
// else ~/.local/share/pi-stack, and every durable default derives from it.
func TestDataDirLayout(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("MEMORY_DB", "")
	t.Setenv("KNOWLEDGE_DB", "")

	d, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := filepath.Join(xdg, "pi-stack"); d != want {
		t.Errorf("DataDir = %q, want %q", d, want)
	}
	if got, want := MemoryDBPath(), filepath.Join(xdg, "pi-stack", "memory", "memory.db"); got != want {
		t.Errorf("MemoryDBPath = %q, want %q", got, want)
	}
	if got, want := KnowledgeDBPath(), filepath.Join(xdg, "pi-stack", "knowledge", "knowledge.db"); got != want {
		t.Errorf("KnowledgeDBPath = %q, want %q", got, want)
	}
	if got, want := BackupsDir(), filepath.Join(xdg, "pi-stack", "backups"); got != want {
		t.Errorf("BackupsDir = %q, want %q", got, want)
	}

	// Env overrides win over the derived default.
	t.Setenv("MEMORY_DB", "/custom/mem.db")
	if got := MemoryDBPath(); got != "/custom/mem.db" {
		t.Errorf("MemoryDBPath with MEMORY_DB = %q, want /custom/mem.db", got)
	}
}

// TestDataDirDefaultHome checks the ~/.local/share/pi-stack fallback when
// XDG_DATA_HOME is unset (uses HOME).
func TestDataDirDefaultHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", home)
	d, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := filepath.Join(home, ".local", "share", "pi-stack"); d != want {
		t.Errorf("DataDir = %q, want %q", d, want)
	}
}
