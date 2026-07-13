package config

import (
	"os"
	"path/filepath"
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
	if want := []string{"memory", "gws"}; len(c.Services) != len(want) || c.Services[0] != want[0] || c.Services[1] != want[1] {
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
services = ["memory", "gws", "warehouse"]
mcp = ["slack"]
memory_watcher_model = "custom-watcher"
memory_embed_model = "custom-embed"

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
	if len(c.Services) != 3 || c.Services[2] != "warehouse" {
		t.Errorf("Services = %v", c.Services)
	}
	if len(c.MCP) != 1 || c.MCP[0] != "slack" {
		t.Errorf("MCP = %v", c.MCP)
	}
	if c.MemoryWatcherModel != "custom-watcher" || c.MemoryEmbedModel != "custom-embed" {
		t.Errorf("models = %q/%q", c.MemoryWatcherModel, c.MemoryEmbedModel)
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
