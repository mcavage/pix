package env

// commit_test.go — Wave C security L1: every `pix env` registry/default
// mutation (add, use, forget) serializes on one launcher-owned lock
// (config.EnvRegistryLockPath, STATE dir) and commits against a FRESH
// under-lock reload of config.toml, so two processes interleaving across a
// prompt (or simply across load->save) can never lost-update each other.
// These tests simulate "another pix process" by loading, mutating, and
// saving an independent *config.Config against the same $PIX_CONFIG file:
// exactly the state a second process leaves behind.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"pix/host/cli"
	"pix/host/config"
)

// otherProcessCommit models a concurrent pix process's completed
// read-modify-write: a fresh load of the live file, one mutation, one save.
func otherProcessCommit(t *testing.T, mutate func(*config.Config) error) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := mutate(cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func tier0Root(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	return root
}

func freshConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// ── add: disjoint concurrent adds preserve both ───────────────────────────

func TestAdd_CommitPreservesConcurrentDisjointRegistration(t *testing.T) {
	tempConfigAndState(t)
	cfgA := loadConfig(t) // loaded BEFORE the other process commits

	otherProcessCommit(t, func(c *config.Config) error {
		_, err := c.AddEnvironment("other", tier0Root(t))
		return err
	})

	if _, err := Add(cfgA, "mine", tier0Root(t), AddOptions{Yes: true, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	fresh := freshConfig(t)
	if _, ok := fresh.Environments["mine"]; !ok {
		t.Error("mine must be registered after Add")
	}
	if _, ok := fresh.Environments["other"]; !ok {
		t.Error("the other process's concurrent registration was lost (lost update)")
	}
}

// ── add: a concurrent repoint of the SAME name refuses deterministically ──

func TestAdd_ConcurrentRepointDuringPromptRefusesDeterministically(t *testing.T) {
	tempConfigAndState(t)
	cfgA := loadConfig(t)

	myRoot := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", myRoot)

	var theirCanon string
	in := &mutateOnFirstRead{
		mutate: func() {
			otherProcessCommit(t, func(c *config.Config) error {
				canon, err := c.AddEnvironment("work", tier0Root(t))
				theirCanon = canon
				return err
			})
		},
		r: strings.NewReader("y\n"),
	}

	var out bytes.Buffer
	res, err := Add(cfgA, "work", myRoot, AddOptions{TTY: true, In: in, Out: &out, LookPath: noBareLookPath})
	if theirCanon == "" {
		t.Fatal("test setup error: the other process's registration never committed")
	}
	if err == nil {
		t.Fatal("Add must refuse when the same name was registered to a different root during the prompt")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}

	fresh := freshConfig(t)
	if got := fresh.Environments["work"]; got != theirCanon {
		t.Errorf("work = %q, want the other process's registration %q preserved", got, theirCanon)
	}
}

// ── add: the same name -> the SAME root committed concurrently is fine ────

func TestAdd_ConcurrentSameRootRegistrationIsIdempotent(t *testing.T) {
	tempConfigAndState(t)
	cfgA := loadConfig(t)

	myRoot := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", myRoot)

	in := &mutateOnFirstRead{
		mutate: func() {
			otherProcessCommit(t, func(c *config.Config) error {
				_, err := c.AddEnvironment("work", myRoot)
				return err
			})
		},
		r: strings.NewReader("y\n"),
	}

	if _, err := Add(cfgA, "work", myRoot, AddOptions{TTY: true, In: in, Out: &bytes.Buffer{}, LookPath: noBareLookPath}); err != nil {
		t.Fatalf("Add of an identically-registered name/root must succeed, got: %v", err)
	}
}

// ── use: commits fresh, preserving a concurrent registration ─────────────

func TestUse_CommitPreservesConcurrentRegistration(t *testing.T) {
	tempConfigAndState(t)
	cfgSetup := loadConfig(t)
	if _, err := Register(cfgSetup, "home", tier0Root(t)); err != nil {
		t.Fatal(err)
	}
	if err := cfgSetup.Save(); err != nil {
		t.Fatal(err)
	}

	cfgA := freshConfig(t) // stale after the next commit
	otherProcessCommit(t, func(c *config.Config) error {
		_, err := c.AddEnvironment("late", tier0Root(t))
		return err
	})

	if err := Use(cfgA, "home", noBareLookPath); err != nil {
		t.Fatalf("Use: %v", err)
	}

	fresh := freshConfig(t)
	if fresh.Environment != "home" {
		t.Errorf("environment = %q, want %q persisted by Use itself", fresh.Environment, "home")
	}
	if _, ok := fresh.Environments["late"]; !ok {
		t.Error("the other process's concurrent registration was lost (lost update)")
	}
	if cfgA.Environment != "home" {
		t.Errorf("passed cfg not synchronized: Environment = %q, want %q", cfgA.Environment, "home")
	}
}

// ── use: a name forgotten concurrently refuses against FRESH state ───────

func TestUse_RefusesWhenNameForgottenConcurrently(t *testing.T) {
	tempConfigAndState(t)
	cfgSetup := loadConfig(t)
	if _, err := Register(cfgSetup, "home", tier0Root(t)); err != nil {
		t.Fatal(err)
	}
	if err := cfgSetup.Save(); err != nil {
		t.Fatal(err)
	}

	cfgA := freshConfig(t)
	otherProcessCommit(t, func(c *config.Config) error {
		c.RemoveEnvironment("home")
		return nil
	})

	err := Use(cfgA, "home", noBareLookPath)
	if err == nil {
		t.Fatal("Use must refuse a name the live config no longer registers")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	fresh := freshConfig(t)
	if fresh.Environment != "" {
		t.Errorf("environment = %q, want no default set by a refused Use", fresh.Environment)
	}
}

// ── forget: commits fresh, preserving a concurrent registration ──────────

func TestForget_CommitPreservesConcurrentRegistration(t *testing.T) {
	tempConfigAndState(t)
	cfgSetup := loadConfig(t)
	if _, err := Register(cfgSetup, "home", tier0Root(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(cfgSetup, "doomed", tier0Root(t)); err != nil {
		t.Fatal(err)
	}
	if err := cfgSetup.Save(); err != nil {
		t.Fatal(err)
	}

	cfgA := freshConfig(t)
	otherProcessCommit(t, func(c *config.Config) error {
		_, err := c.AddEnvironment("late", tier0Root(t))
		return err
	})

	if _, err := Forget(cfgA, "doomed", nil); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	fresh := freshConfig(t)
	if _, ok := fresh.Environments["doomed"]; ok {
		t.Error("doomed must be unregistered after Forget")
	}
	if _, ok := fresh.Environments["home"]; !ok {
		t.Error("home must survive Forget")
	}
	if _, ok := fresh.Environments["late"]; !ok {
		t.Error("the other process's concurrent registration was lost (lost update)")
	}
	if _, ok := cfgA.Environments["doomed"]; ok {
		t.Error("passed cfg not synchronized: doomed still registered in memory")
	}
}

// ── forget: a concurrent `use` making NAME the default refuses fresh ─────

func TestForget_RefusesWhenConcurrentUseMadeItTheDefault(t *testing.T) {
	tempConfigAndState(t)
	cfgSetup := loadConfig(t)
	if _, err := Register(cfgSetup, "a", tier0Root(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(cfgSetup, "b", tier0Root(t)); err != nil {
		t.Fatal(err)
	}
	if err := cfgSetup.Save(); err != nil {
		t.Fatal(err)
	}

	cfgA := freshConfig(t)
	otherProcessCommit(t, func(c *config.Config) error {
		return c.UseEnvironment("a")
	})

	_, err := Forget(cfgA, "a", nil)
	if err == nil {
		t.Fatal("Forget must refuse a name a concurrent `use` just made the default")
	}
	var target *ForgetCurrentDefaultError
	if !errors.As(err, &target) {
		t.Errorf("error = %T (%v), want *ForgetCurrentDefaultError", err, err)
	}
	fresh := freshConfig(t)
	if fresh.Environment != "a" {
		t.Errorf("environment = %q, want the other process's default %q preserved", fresh.Environment, "a")
	}
	if _, ok := fresh.Environments["a"]; !ok {
		t.Error("a refused Forget must leave the registration in place")
	}
}

// ── commit: the ENTIRE fresh config syncs back, not just env fields ─────

// TestCommit_SyncsEntireConfigIncludingUnrelatedFields is finding A3: a
// concurrent process's change to a field commitEnvRegistryMutation does not
// own (OllamaBridgeModel here, standing in for any non-env key) must still be
// visible on the caller's cfg after an env commit, and a LATER cfg.Save()
// from that same caller must not revert it — that is the lost-update this
// finding closes, one level up from the env-registry fields themselves.
func TestCommit_SyncsEntireConfigIncludingUnrelatedFields(t *testing.T) {
	tempConfigAndState(t)
	cfgA := loadConfig(t) // stale snapshot: OllamaBridgeModel still unset here

	otherProcessCommit(t, func(c *config.Config) error {
		c.OllamaBridgeModel = "strategy"
		return nil
	})

	if _, err := Add(cfgA, "mine", tier0Root(t), AddOptions{Yes: true, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if cfgA.OllamaBridgeModel != "strategy" {
		t.Errorf("cfgA.OllamaBridgeModel = %q, want the concurrent unrelated field %q synced back after commit", cfgA.OllamaBridgeModel, "strategy")
	}

	// A later caller Save() of the now-synced cfgA must not revert the
	// concurrent unrelated field: it was already folded in, so re-saving is a
	// no-op for OllamaBridgeModel, not a regression back to the stale value.
	if err := cfgA.Save(); err != nil {
		t.Fatal(err)
	}
	fresh := freshConfig(t)
	if fresh.OllamaBridgeModel != "strategy" {
		t.Errorf("a later caller Save() reverted the unrelated concurrent field: OllamaBridgeModel = %q, want %q", fresh.OllamaBridgeModel, "strategy")
	}
}

// ── parallel: N concurrent disjoint adds all survive (run with -race) ─────

func TestEnvMutations_ParallelDisjointAddsAllSurvive(t *testing.T) {
	tempConfigAndState(t)

	const n = 6
	roots := make([]string, n)
	for i := range roots {
		roots[i] = tier0Root(t)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg, err := config.Load()
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = Add(cfg, fmt.Sprintf("env-%d", i), roots[i], AddOptions{Yes: true, Out: &bytes.Buffer{}})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Add env-%d: %v", i, err)
		}
	}

	fresh := freshConfig(t)
	for i := 0; i < n; i++ {
		if _, ok := fresh.Environments[fmt.Sprintf("env-%d", i)]; !ok {
			t.Errorf("env-%d was lost to a concurrent writer", i)
		}
	}

	// The serialization lock is launcher-owned STATE, never config-dir:
	// renaming the config dir aside (pix reset) can never strand it.
	lock := config.EnvRegistryLockPath()
	if !strings.HasPrefix(lock, filepath.Join(os.Getenv("XDG_STATE_HOME"), "pix")) {
		t.Errorf("EnvRegistryLockPath = %q, want it under $XDG_STATE_HOME/pix", lock)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("lock file %s: %v (every env mutation must have serialized on it)", lock, err)
	}
}

// ── sentinel: no env writer saves outside the locked commit path ─────────

// countSaveCalls parses file and counts CALLS whose selector is .Save() —
// an AST count, never a substring scan, so doc comments that legitimately
// discuss Save() can never trip it (nor hide a real call).
func countSaveCalls(t *testing.T, file string) int {
	t.Helper()
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	n := 0
	ast.Inspect(node, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Save" {
			n++
		}
		return true
	})
	return n
}

// TestCountSaveCalls_SelfTest is finding A2's planted-violation proof for
// the .Save()-call AST scanner: a throwaway file with a KNOWN number of
// .Save() call expressions (0, 1, 2, and one that only MENTIONS Save() in a
// comment/doc string, which must not count) must report exactly that count.
func TestCountSaveCalls_SelfTest(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"zero", "package planted\nfunc f() {}\n", 0},
		{"one", "package planted\nfunc f(c interface{ Save() error }) { c.Save() }\n", 1},
		{"two", "package planted\nfunc f(a, b interface{ Save() error }) { a.Save(); b.Save() }\n", 2},
		{"comment mention does not count", "package planted\n// this calls Save() but only in prose\nfunc f() {}\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".go")
			if err := os.WriteFile(path, []byte(c.src), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := countSaveCalls(t, path); got != c.want {
				t.Errorf("countSaveCalls(%s) = %d, want %d", c.name, got, c.want)
			}
		})
	}
}

func TestEnvRegistryWriters_SaveOnlyInsideLockedCommit(t *testing.T) {
	for _, f := range []string{"add.go", "use.go", "forget.go"} {
		if got := countSaveCalls(t, f); got != 0 {
			t.Errorf("%s calls .Save() directly (%d call(s)); every registry/default commit must go through commit.go's locked helper", f, got)
		}
	}
	if got := countSaveCalls(t, "commit.go"); got != 1 {
		t.Errorf("commit.go must hold exactly ONE .Save() call (the locked commit), got %d", got)
	}
	src, err := os.ReadFile("commit.go")
	if err != nil {
		t.Fatalf("commit.go (the ONE locked commit path) must exist: %v", err)
	}
	for _, want := range []string{"WithLock", "EnvRegistryLockPath", "config.Load()"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("commit.go must contain %q (lock + fresh under-lock reload)", want)
		}
	}
}
