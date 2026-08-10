package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestLoadAbsentReturnsDefaults(t *testing.T) {
	// Point at a path that does not exist.
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "nope.toml"))

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() absent file: got err %v, want nil", err)
	}
	if want := []string{}; len(c.Services) != len(want) {
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
google_workspace_account = "you@example.com"

[kits]
stack = ["mixin-a", "mixin-b"]

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
	t.Setenv("PIX_CONFIG", path)

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
	if len(c.Kits.Stack) != 2 || c.Kits.Stack[1] != "mixin-b" {
		t.Errorf("Kits.Stack = %v", c.Kits.Stack)
	}
	if len(c.Skills.Paths) != 1 || c.Skills.Paths[0] != "/tmp/skills" {
		t.Errorf("Skills.Paths = %v", c.Skills.Paths)
	}
	// [plugins.*] is RETIRED (U07d): the declarations above decode without
	// error but are swept INERT — every slot answers builtin (no path, no sha,
	// nothing launchable), and each declared slot surfaces through
	mem := c.Plugin("memory")
	if mem.Impl != BuiltinImpl || mem.Path != "" || mem.SHA != "" || mem.Port != 0 {
		t.Errorf("Plugin(memory) = %+v, want inert builtin (plugins.* is retired)", mem)
	}
	if got := c.Plugin("slack"); got.Impl != BuiltinImpl {
		t.Errorf("Plugin(slack).Impl = %q, want %q", got.Impl, BuiltinImpl)
	}
	retired := c.UnknownKeys()
	for _, want := range []string{"plugins.memory", "plugins.slack"} {
		found := false
		for _, k := range retired {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("UnknownKeys() = %v, want it to include %q (a silently discarded plugin declaration)", retired, want)
		}
	}
}

// TestUnknownSlackTableTolerated: a leftover `[slack]` table from a
// pre-externalization config.toml decodes without error, does not fail the
// load, and is REPORTED as an unknown key so a caller can surface it -
func TestUnknownSlackTableTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const toml = "[slack]\nclient_id = \"123.456\"\nredirect_uri = \"https://example.com/cb\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !slices.Contains(c.UnknownKeys(), "slack") {
		t.Errorf("UnknownKeys() = %v, want it to include the ignored slack table", c.UnknownKeys())
	}
}
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
	if got, want := c.UnknownKeys(), []string{"mcp_dynamic", "mcp_static"}; !stringSlicesEqual(got, want) {
		t.Errorf("UnknownKeys() = %v, want %v", got, want)
	}
	if len(c.MCP) != 1 || c.MCP[0] != "slack" {
		t.Errorf("MCP = %v, want [slack] (live key still decodes)", c.MCP)
	}

	t.Setenv("PIX_CONFIG", path)
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
	if got := reloaded.UnknownKeys(); len(got) != 0 {
		t.Errorf("reloaded UnknownKeys() = %v, want none", got)
	}
}
func TestLoadAbsentReportsNoRetiredOrUnknownKeys(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "nope.toml"))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %v, want none", got)
	}
}

// TestSaveAtomic_WriteFailureLeavesPriorFileIntact (packs-v2 review finding
// #4): Save writes to a temp file + atomic rename rather than truncating
// config.toml in place, so a failed write never leaves the prior file
func TestSaveAtomic_WriteFailureLeavesPriorFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", path)

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
	t.Setenv("PIX_CONFIG", path)

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
	if !c.AddService("broker") {
		t.Error("AddService(broker): want changed=true")
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
	if !contains(got.Services, "broker") {
		t.Errorf("Services = %v, want it to contain broker", got.Services)
	}

	// Remove mutators.
	if !got.RemoveMCP("gog") {
		t.Error("RemoveMCP(gog): want changed=true")
	}
	if got.RemoveMCP("gog") {
		t.Error("RemoveMCP(gog) twice: want changed=false")
	}
	if !got.RemoveService("broker") {
		t.Error("RemoveService(broker): want changed=true")
	}
}

// TestKnowledgeBundlesKeyIsRetired: an older config.toml carrying
// knowledge_bundles (the built-in OKF knowledge service, retired W2 U03A)
// must still Load cleanly — tolerated, never a hard error — and be reported
func TestKnowledgeBundlesKeyIsRetired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("knowledge_bundles = [\"/kb/acme\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", path)

	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("Load must tolerate a retired knowledge_bundles key, got: %v", err)
	}
	if !slices.Contains(c.UnknownKeys(), "knowledge_bundles") {
		t.Errorf("UnknownKeys() = %v, want knowledge_bundles reported", c.UnknownKeys())
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

// TestServePidPath resolves serve.pid under the STATE dir (ephemeral runtime
// state, a sibling of serve.log), honoring $XDG_STATE_HOME, so the host writer
// and the launcher reader always agree on the location.
func TestServePidPath(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	want := filepath.Join(xdg, "pix", "serve.pid")
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
// else ~/.local/share/pix, and every durable default derives from it.
func TestDataDirLayout(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("MEMORY_DB", "")

	d, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := filepath.Join(xdg, "pix"); d != want {
		t.Errorf("DataDir = %q, want %q", d, want)
	}
	if got, want := MemoryDBPath(), filepath.Join(xdg, "pix", "memory", "memory.db"); got != want {
		t.Errorf("MemoryDBPath = %q, want %q", got, want)
	}

	// Env overrides win over the derived default.
	t.Setenv("MEMORY_DB", "/custom/mem.db")
	if got := MemoryDBPath(); got != "/custom/mem.db" {
		t.Errorf("MemoryDBPath with MEMORY_DB = %q, want /custom/mem.db", got)
	}
}

// TestDataDirDefaultHome checks the ~/.local/share/pix fallback when
// XDG_DATA_HOME is unset (uses HOME).
func TestDataDirDefaultHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", home)
	d, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := filepath.Join(home, ".local", "share", "pix"); d != want {
		t.Errorf("DataDir = %q, want %q", d, want)
	}
}

// TestUnknownKeysReportsATypo is the case the old retired-key allowlist could
// never cover: a MISTYPED key. It is not in any curated list, it decodes into
// nothing, and before this it was dropped in silence — so the setting simply
// never took effect and pix said nothing about why.
func TestUnknownKeysReportsATypo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// One real key (so the file is otherwise valid), one typo, one nested typo.
	const body = "run_intent = \"code\"\nmemory_watchr_model = \"x\"\n\n[inference]\nbackendz = 1\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err) // an unknown key must never fail the load
	}
	if c.RunIntent != "code" {
		t.Errorf("RunIntent = %q, want the recognized key to still apply", c.RunIntent)
	}
	for _, want := range []string{"memory_watchr_model", "inference.backendz"} {
		if !slices.Contains(c.UnknownKeys(), want) {
			t.Errorf("UnknownKeys() = %v, want it to include %q", c.UnknownKeys(), want)
		}
	}
}

// TestUnknownKeysEmptyForACleanConfig: the warning must stay silent on a file
// that is entirely understood, or it becomes noise everyone learns to ignore.
func TestUnknownKeysEmptyForACleanConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("run_intent = \"code\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := c.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %v, want none for a fully recognized config", got)
	}
}
