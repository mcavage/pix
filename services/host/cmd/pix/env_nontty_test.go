// env_nontty_test.go — E1.13's non-TTY sweep (F16/AC-65): every `pix env`
// verb, the hidden `rm` pointer, and bare `pix env`, dispatched with
// Interactive == false and an empty stdin, each run under a wall-clock
// timeout so a regression that starts reading real stdin fails LOUD
// (a timed-out goroutine, never a wedged `go test`) instead of just hanging
// the suite. Every one of these dispatch calls is synchronous, in-process
// Go over an in-memory strings.Reader, so none of them can genuinely block
// today — the timeout is the regression guard, not a demonstration that
// something here currently blocks.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/config"
)

// runWithTimeout dispatches argv and fails the test if it has not returned
// within d — the mechanical proof half of F16 ("no hang"), independent of
// whatever exit code comes back.
func runWithTimeout(t *testing.T, argv []string, deps *cli.Deps, d time.Duration) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- dispatch(argv, deps) }()
	select {
	case code := <-done:
		return code
	case <-time.After(d):
		t.Fatalf("dispatch(%v) did not return within %s (reads stdin? hangs?)", argv, d)
		return -1
	}
}

// nonTTYDeps is a fresh, non-interactive *cli.Deps over a scratch
// $PIX_CONFIG/$XDG_STATE_HOME: Interactive defaults false (cli.Deps' own
// zero value) and In is an already-EOF reader, so a bufio.Scanner.Scan()
// anywhere on this path returns immediately rather than blocking.
func nonTTYDeps(t *testing.T) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("")}, &out, &errb
}

func copyFixtureDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func registerNonTTYTier0(t *testing.T, name string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment(name, root); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func registerNonTTYTier1(t *testing.T, name string) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	copyFixtureDir(t, filepath.Join("..", "..", "workflow", "env", "testdata", "hostexec-fixture"), root)
	if _, err := cfg.AddEnvironment(name, root); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestEnvNonTTY_EverySurfaceTerminatesWithADefinedExitCode is F16/AC-65:
// bare `env`, all seven verbs with minimal argv, the hidden `rm` pointer,
// a missing-required-arg shape per verb, and the three TTY-sensitive verbs
// (edit/review/add) all resolve to a defined exit code without hanging.
func TestEnvNonTTY_EverySurfaceTerminatesWithADefinedExitCode(t *testing.T) {
	type tc struct {
		name  string
		argv  []string
		setup func(t *testing.T) []string // may rewrite argv (e.g. inject a scratch source path)
	}
	cases := []tc{
		{name: "bare env", argv: []string{"env"}},
		{name: "env ls", argv: []string{"env", "ls"}},
		{name: "env add missing NAME", argv: []string{"env", "add"}},
		{name: "env add zero-path tier0", argv: []string{"env", "add", "home"}},
		{
			name: "env add tier1 non-interactive (TTY-sensitive)",
			argv: []string{"env", "add", "work"},
			setup: func(t *testing.T) []string {
				src := t.TempDir()
				copyFixtureDir(t, filepath.Join("..", "..", "workflow", "env", "testdata", "hostexec-fixture"), src)
				return []string{"env", "add", "work", src}
			},
		},
		{name: "env use missing NAME", argv: []string{"env", "use"}},
		{name: "env use unknown", argv: []string{"env", "use", "ghost"}},
		{name: "env show no args", argv: []string{"env", "show"}},
		{name: "env show unknown", argv: []string{"env", "show", "ghost"}},
		{name: "env edit missing NAME", argv: []string{"env", "edit"}},
		{
			name: "env edit no target, registered (TTY-sensitive)",
			argv: []string{"env", "edit", "work"},
			setup: func(t *testing.T) []string { registerNonTTYTier0(t, "work"); return nil },
		},
		{name: "env review missing NAME", argv: []string{"env", "review"}},
		{
			name: "env review registered tier1 (TTY-sensitive)",
			argv: []string{"env", "review", "work"},
			setup: func(t *testing.T) []string { registerNonTTYTier1(t, "work"); return nil },
		},
		{name: "env forget missing NAME", argv: []string{"env", "forget"}},
		{name: "env forget unknown", argv: []string{"env", "forget", "ghost"}},
		{name: "env rm bare", argv: []string{"env", "rm"}},
		{name: "env rm with name and flags", argv: []string{"env", "rm", "home", "--force"}},
		{name: "env unknown verb", argv: []string{"env", "bogus"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, out, errb := nonTTYDeps(t)
			argv := c.argv
			if c.setup != nil {
				if rewritten := c.setup(t); rewritten != nil {
					argv = rewritten
				}
			}

			code := runWithTimeout(t, argv, d, 5*time.Second)
			if code < 0 || code == 3 || code > 8 {
				t.Errorf("dispatch(%v) = %d, want a defined, bounded exit code", argv, code)
			}
			t.Logf("dispatch(%v) = %d (stdout %dB, stderr %dB)", argv, code, out.Len(), errb.Len())
		})
	}
}
