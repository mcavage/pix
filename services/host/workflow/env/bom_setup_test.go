package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bom_setup_test.go proves the `[[setup]]` hook surface is REVIEWED, not
// merely runnable: it is Tier1 (so the default-No gate fires), every one of
// its facts moves the fingerprint (so no field can be swapped after
// acceptance), and an executable that cannot be content-identified fails
// the whole bill closed.

// setupHookEnv writes an executable hook script under the environment root
// and returns the loaded *Environment plus that root.
func setupHookEnv(t *testing.T, sidecar, scriptBody string) *Environment {
	t.Helper()
	env := loadTestEnv(t, "work", minimalSbxenv, sidecar)
	if scriptBody != "" {
		if err := os.WriteFile(filepath.Join(env.Root, "setup-tool"), []byte(scriptBody), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

const setupSidecar = `schema = 1

[[setup]]
id = "tool"
command = "./setup-tool"
check_args = ["check"]
apply_args = ["install"]
required = true
kind = "install"
`

func TestComputeBoM_SetupHookIsTier1AndFingerprinted(t *testing.T) {
	env := setupHookEnv(t, setupSidecar, "#!/bin/sh\nexit 0\n")
	bom, err := ComputeBoM(env, nil, nil)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if len(bom.SetupHooks) != 1 {
		t.Fatalf("want 1 setup hook in the bill, got %+v", bom.SetupHooks)
	}
	h := bom.SetupHooks[0]
	if h.ID != "tool" || h.Kind != "install" || !h.Required {
		t.Fatalf("hook fact wrong: %+v", h)
	}
	if h.SHA == "" {
		t.Fatal("a setup hook must carry the resolved executable's content hash")
	}
	if h.Resolved != filepath.Join(env.Root, "setup-tool") {
		t.Fatalf("resolved = %q, want the env-root-relative path", h.Resolved)
	}
	if !bom.Tier1() {
		t.Fatal("an environment that runs a setup hook on this host must be Tier1 (default-No review)")
	}
}

// Every fingerprinted hook fact must MOVE the fingerprint: a reviewer who
// accepted "check" must not silently get "install --force" later.
func TestFingerprint_EverySetupHookFactIsSensitive(t *testing.T) {
	base := setupHookEnv(t, setupSidecar, "#!/bin/sh\nexit 0\n")
	baseBoM, err := ComputeBoM(base, nil, nil)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	baseFP, err := Fingerprint(baseBoM)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	mutations := map[string]func(*SetupHookFact){
		"id":         func(h *SetupHookFact) { h.ID = "other" },
		"command":    func(h *SetupHookFact) { h.Command = "./other-tool" },
		"sha":        func(h *SetupHookFact) { h.SHA = strings.Repeat("0", 64) },
		"check argv": func(h *SetupHookFact) { h.CheckArgs = []string{"check", "--force"} },
		"apply argv": func(h *SetupHookFact) { h.ApplyArgs = []string{"install", "--sudo"} },
		"kind":       func(h *SetupHookFact) { h.Kind = "auth" },
		"required":   func(h *SetupHookFact) { h.Required = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			mutated := baseBoM
			hooks := append([]SetupHookFact(nil), baseBoM.SetupHooks...)
			mutate(&hooks[0])
			mutated.SetupHooks = hooks
			fp, err := Fingerprint(mutated)
			if err != nil {
				t.Fatalf("Fingerprint: %v", err)
			}
			if fp == baseFP {
				t.Fatalf("changing a hook's %s did not change the fingerprint; it could be swapped after acceptance", name)
			}
		})
	}

	// The resolved absolute path is deliberately NOT fingerprinted: the
	// same reviewed content at a different checkout must not re-gate.
	moved := baseBoM
	hooks := append([]SetupHookFact(nil), baseBoM.SetupHooks...)
	hooks[0].Resolved = "/somewhere/else/setup-tool"
	moved.SetupHooks = hooks
	fp, err := Fingerprint(moved)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp != baseFP {
		t.Fatal("the host-local resolved path must not be part of the fingerprint (content identity is)")
	}
}

func TestComputeBoM_UnfingerprintableSetupHookFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		script string
		mode   os.FileMode
		want   string
	}{
		{"missing executable", "", 0, "no such file"},
		{"not executable", "#!/bin/sh\nexit 0\n", 0o644, "not executable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := loadTestEnv(t, "work", minimalSbxenv, setupSidecar)
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(env.Root, "setup-tool"), []byte(tc.script), tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			_, err := ComputeBoM(env, nil, nil)
			if err == nil {
				t.Fatal("a setup hook whose executable cannot be identified must fail the bill closed")
			}
			if !strings.Contains(err.Error(), "setup hook \"tool\"") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q must name the hook and the reason %q", err, tc.want)
			}
		})
	}
}

func TestComputeBoM_SymlinkedSetupHookIsRefused(t *testing.T) {
	env := loadTestEnv(t, "work", minimalSbxenv, setupSidecar)
	real := filepath.Join(t.TempDir(), "real-tool")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(env.Root, "setup-tool")); err != nil {
		t.Fatal(err)
	}
	_, err := ComputeBoM(env, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked hook command must be refused, got %v", err)
	}
}

// An ABSOLUTE command is allowed only because it is fingerprinted the same
// way a relative one is: same proof, same hash, no PATH lookup anywhere.
func TestComputeBoM_AbsoluteCommandIsFingerprintedNotTrusted(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "abs-tool")
	if err := os.WriteFile(abs, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := loadTestEnv(t, "work", minimalSbxenv, `schema = 1

[[setup]]
id = "abs"
command = "`+abs+`"
check_args = ["check"]
apply_args = ["install"]
`)
	bom, err := ComputeBoM(env, nil, nil)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if bom.SetupHooks[0].SHA == "" || bom.SetupHooks[0].Resolved != abs {
		t.Fatalf("an absolute hook command must resolve to itself and be hashed: %+v", bom.SetupHooks[0])
	}
}
