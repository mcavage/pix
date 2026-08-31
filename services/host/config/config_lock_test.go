package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// config_lock_test.go pins round 5's config collapse: config.Config is the
// SOLE <PIX_HOME>/config.toml schema, and every named writer (SetMemoryPort-
// shaped callers like container's allocator, SetDefaultEnvironment) must
// load-modify-save the FULL schema under the one config lock — never a
// second schema that persists only the fields IT knows about and silently
// drops VersionPin/Inference/whatever a sibling writer already set.

func seedFullSchema(t *testing.T, path string) *Config {
	t.Helper()
	c := &Config{
		VersionPin: "9.9.9",
		Inference: InferenceConfig{
			Backends: map[string]InferenceBackend{
				"anthropic": {Driver: "native", Auth: "sbx-session"},
			},
		},
	}
	if err := SaveTo(path, c); err != nil {
		t.Fatalf("seed SaveTo: %v", err)
	}
	return c
}

// TestSetupMemoryPortThenEnvDefault_PreservesPortInferenceVersion: a caller
// that allocates a memory port (the shape container.AllocateMemoryPortLocked
// uses) followed by `pix env default NAME` (SetDefaultEnvironmentAt) must
// never lose VersionPin/Inference set by an earlier `pix setup` — the exact
// data loss the retired per-home Machine struct caused by overwriting
// config.toml with only its own two fields.
func TestSetupMemoryPortThenEnvDefault_PreservesPortInferenceVersion(t *testing.T) {
	dir := t.TempDir()
	path := PathAt(dir)
	seedFullSchema(t, path)

	if err := WithLockAt(dir, func(c *Config) error {
		c.MemoryPort = 18123
		return nil
	}); err != nil {
		t.Fatalf("allocate memory port: %v", err)
	}
	if err := SetDefaultEnvironmentAt(dir, "work"); err != nil {
		t.Fatalf("SetDefaultEnvironmentAt: %v", err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got.VersionPin != "9.9.9" {
		t.Errorf("VersionPin = %q, want preserved 9.9.9", got.VersionPin)
	}
	if _, ok := got.Inference.Backends["anthropic"]; !ok {
		t.Errorf("Inference.Backends[anthropic] dropped: %+v", got.Inference.Backends)
	}
	if got.MemoryPort != 18123 {
		t.Errorf("MemoryPort = %d, want 18123", got.MemoryPort)
	}
	if got.DefaultEnvironment != "work" {
		t.Errorf("DefaultEnvironment = %q, want work", got.DefaultEnvironment)
	}
}

// TestEnvDefaultThenSetupMemoryPort_ReversedOrderPreservesDefault is the
// mirror: writing the default FIRST, then allocating a port, must not lose
// the default either — the fix must not just happen to work for one order.
func TestEnvDefaultThenSetupMemoryPort_ReversedOrderPreservesDefault(t *testing.T) {
	dir := t.TempDir()
	path := PathAt(dir)
	seedFullSchema(t, path)

	if err := SetDefaultEnvironmentAt(dir, "home"); err != nil {
		t.Fatalf("SetDefaultEnvironmentAt: %v", err)
	}
	if err := WithLockAt(dir, func(c *Config) error {
		c.MemoryPort = 18456
		return nil
	}); err != nil {
		t.Fatalf("allocate memory port: %v", err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got.DefaultEnvironment != "home" {
		t.Errorf("DefaultEnvironment = %q, want home (must survive a later writer)", got.DefaultEnvironment)
	}
	if got.MemoryPort != 18456 {
		t.Errorf("MemoryPort = %d, want 18456", got.MemoryPort)
	}
	if got.VersionPin != "9.9.9" {
		t.Errorf("VersionPin = %q, want preserved 9.9.9", got.VersionPin)
	}
	if _, ok := got.Inference.Backends["anthropic"]; !ok {
		t.Errorf("Inference.Backends[anthropic] dropped: %+v", got.Inference.Backends)
	}
}

// TestWithLockAt_ConcurrentWritersNoLostUpdate: N goroutines each increment
// a counter field (MemoryPort, repurposed as a counter here — any field
// works) by exactly 1 under WithLockAt. Without a real lock serializing the
// read-modify-write, concurrent goroutines racing Load-then-Save lose
// updates; the final value must equal N exactly.
func TestWithLockAt_ConcurrentWritersNoLostUpdate(t *testing.T) {
	dir := t.TempDir()
	const n = 40
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = WithLockAt(dir, func(c *Config) error {
				c.MemoryPort++
				return nil
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WithLockAt[%d]: %v", i, err)
		}
	}
	got, err := LoadFrom(PathAt(dir))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got.MemoryPort != n {
		t.Errorf("MemoryPort = %d, want %d (a lost update means the lock is not exclusive)", got.MemoryPort, n)
	}
}

// TestWithLockAt_ConcurrentMixedWritersNoLostUpdate: concurrent
// SetDefaultEnvironmentAt callers and MemoryPort-incrementing callers
// against the SAME PIX_HOME must never lose EITHER kind of update — the
// mixed-writer shape `pix env default` racing `pix setup`'s port allocation
// would actually produce.
func TestWithLockAt_ConcurrentMixedWritersNoLostUpdate(t *testing.T) {
	dir := t.TempDir()
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n * 2)
	errs := make([]error, n*2)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = WithLockAt(dir, func(c *Config) error {
				c.MemoryPort++
				return nil
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			errs[n+i] = SetDefaultEnvironmentAt(dir, "env-stable")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer[%d]: %v", i, err)
		}
	}
	got, err := LoadFrom(PathAt(dir))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got.MemoryPort != n {
		t.Errorf("MemoryPort = %d, want %d (lost update against a mixed writer)", got.MemoryPort, n)
	}
	if got.DefaultEnvironment != "env-stable" {
		t.Errorf("DefaultEnvironment = %q, want env-stable", got.DefaultEnvironment)
	}
}

// TestSave_AtomicNeverPartial proves Save/SaveTo never leaves a truncated
// config.toml visible mid-write: a reader racing a writer sees either the
// old complete file or the new complete file, never a half-written one
// (the same same-directory temp+rename guarantee AtomicWriteInDir gives
// every other hardened writer in this tree).
func TestSave_AtomicNeverPartial(t *testing.T) {
	dir := t.TempDir()
	path := PathAt(dir)
	if err := SaveTo(path, &Config{VersionPin: "1.0.0"}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || e.Name() != "config.toml" {
			// A leftover temp file would mean cleanup failed; config.toml
			// itself is the only file this test seeded.
			if e.Name() != "config.toml" {
				t.Errorf("unexpected leftover file: %s", e.Name())
			}
		}
	}
}
