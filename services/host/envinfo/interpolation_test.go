package envinfo_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pix/host/envinfo"
)

func TestInterpolation_DefinedVarSurfacedByNameAndDestination(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
env:
  PIX_MEMORY_SCOPE: "${PIX_MEMORY_SCOPE}"
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Interpolations) != 1 {
		t.Fatalf("Interpolations = %+v, want 1", tr.Interpolations)
	}
	got := tr.Interpolations[0]
	if got.Var != "PIX_MEMORY_SCOPE" {
		t.Errorf("Var = %q, want PIX_MEMORY_SCOPE", got.Var)
	}
	if got.KeyPath != "env.PIX_MEMORY_SCOPE" {
		t.Errorf("KeyPath = %q, want env.PIX_MEMORY_SCOPE", got.KeyPath)
	}
	if got.Default != nil {
		t.Errorf("Default = %v, want nil (no default authored)", *got.Default)
	}
	// The literal expression stays in the Env node untouched, never resolved.
	if tr.Env[0].Value != "${PIX_MEMORY_SCOPE}" {
		t.Errorf("Env node value = %q, want the unresolved literal expression", tr.Env[0].Value)
	}
}

func TestInterpolation_MissingWithDefault(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
env:
  FOO: "${SOME_VAR:-fallback-value}"
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Interpolations) != 1 {
		t.Fatalf("Interpolations = %+v, want 1", tr.Interpolations)
	}
	got := tr.Interpolations[0]
	if got.Var != "SOME_VAR" {
		t.Errorf("Var = %q, want SOME_VAR", got.Var)
	}
	if got.Default == nil || *got.Default != "fallback-value" {
		t.Errorf("Default = %v, want fallback-value", got.Default)
	}
}

func TestInterpolation_SandboxOptionsScanned(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
sandboxOptions:
  memory: "${MEM_LIMIT:-16g}"
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Interpolations) != 1 || tr.Interpolations[0].KeyPath != "sandboxOptions.memory" {
		t.Fatalf("Interpolations = %+v", tr.Interpolations)
	}
}

func TestInterpolation_MultipleReferencesInOneValueAllSurfaced(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
env:
  COMBINED: "${FIRST}-${SECOND:-second-default}"
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Interpolations) != 2 {
		t.Fatalf("Interpolations = %+v, want 2", tr.Interpolations)
	}
	if tr.Interpolations[0].Var != "FIRST" || tr.Interpolations[1].Var != "SECOND" {
		t.Errorf("Vars = %q, %q", tr.Interpolations[0].Var, tr.Interpolations[1].Var)
	}
}

func TestInterpolation_NoReferencesWhenPlainValue(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
env:
  PLAIN: "just-a-value"
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Interpolations) != 0 {
		t.Fatalf("Interpolations = %+v, want none", tr.Interpolations)
	}
}

// TestInterpolationTypeCarriesNoResolvedValue is a structural guard for
// docs/design/environments.md §9.1's Story 1 obligation: the type itself
// must have no field a resolved secret could ever occupy, not merely "the
// code happens not to set one today".
func TestInterpolationTypeCarriesNoResolvedValue(t *testing.T) {
	typ := reflect.TypeOf(envinfo.Interpolation{})
	want := map[string]bool{"Var": true, "Default": true, "KeyPath": true}
	if typ.NumField() != len(want) {
		t.Fatalf("Interpolation has %d fields, want exactly %v", typ.NumField(), want)
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !want[name] {
			t.Errorf("Interpolation has unexpected field %q; only Var/Default/KeyPath may exist (no resolved-value field)", name)
		}
	}
}

// TestNoHostEnvironmentResolution is a grep-based fitness function: this
// package must never call os.Getenv, os.LookupEnv, or os.Environ in
// production code. Surfacing an authored ${VAR} reference must never
// require, and must never tempt a future edit into, actually resolving it
// (docs/design/environments.md §9.1).
func TestNoHostEnvironmentResolution(t *testing.T) {
	forbidden := []string{"os.Getenv(", "os.LookupEnv(", "os.Environ("}
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, f := range forbidden {
			if strings.Contains(string(content), f) {
				t.Errorf("%s calls %s: envinfo must never resolve a host environment variable, only surface the reference", path, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
