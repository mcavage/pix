package env

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/cli"
	"pix/host/envinfo"
	"pix/host/pixhome"
)

// TestLoadHome_BothFilesParsed mirrors TestLoad_BothFilesParsed (load_test.go)
// against a pixhome-resolved Selected instead of a config.Environments
// registry entry: the v2 selection model has no registry to register
// against at all (home.go's own doc comment).
func TestLoadHome_BothFilesParsed(t *testing.T) {
	home := pixhome.New(t.TempDir())
	root := home.EnvironmentDir("home")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// A LOCAL skill dir under the environment's own root: LoadHome always
	// implicitly adds root to the skill-validation workspace set (mirroring
	// Load's own contract), so this needs no caller-supplied effective mount.
	skillsDir := filepath.Join(root, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	writeEnvFile(t, root, "pix.toml", minimalSidecarWithSkills(skillsDir))

	sel, err := ResolveIn(home, "home")
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}

	got, err := LoadHome(sel, nil, nil)
	if err != nil {
		t.Fatalf("LoadHome: %v", err)
	}
	if got.Document == nil || got.Document.SchemaVersion != "1" || got.Document.Agent != "pix" {
		t.Fatalf("Document = %+v", got.Document)
	}
	if got.Sidecar == nil || len(got.Sidecar.Pi.Skills) != 1 || got.Sidecar.Pi.Skills[0] != skillsDir {
		t.Fatalf("Sidecar = %+v", got.Sidecar)
	}
	if got.Tree == nil {
		t.Fatal("Tree is nil, want the pre-composition tree")
	}
	if got.Root != sel.Root {
		t.Errorf("Root = %q, want %q", got.Root, sel.Root)
	}
	if got.Name != "home" {
		t.Errorf("Name = %q, want home", got.Name)
	}
}

// TestLoadHome_MissingRequiredFile proves the required-native-file refusal
// survives the pixhome cutover unchanged.
func TestLoadHome_MissingRequiredFile(t *testing.T) {
	home := pixhome.New(t.TempDir())
	root := home.EnvironmentDir("home")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// ResolveIn requires the directory to exist but does not require
	// .sbxenv.yaml; LoadHome is what refuses a directory with no native
	// document.
	sel, err := ResolveIn(home, "home")
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	_, err = LoadHome(sel, nil, nil)
	var missing *MissingRequiredFileError
	if !errors.As(err, &missing) {
		t.Fatalf("expected MissingRequiredFileError, got %v", err)
	}
}

// TestLoadHome_RefusesReservedMCPName proves an authored server named
// pix-memory or pix-session refuses before anything else composes further
// — the one v2-only check LoadHome adds beyond Load's own contract.
func TestLoadHome_RefusesReservedMCPName(t *testing.T) {
	home := pixhome.New(t.TempDir())
	root := home.EnvironmentDir("home")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", "schemaVersion: \"1\"\nagent: pix\n\nmcp:\n  servers:\n    - name: "+envinfo.MCPMemoryName+"\n      url: https://example.com/mcp\n")

	sel, err := ResolveIn(home, "home")
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	_, err = LoadHome(sel, nil, nil)
	var collision *envinfo.ReservedMCPCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("an authored %q server must refuse, got %v", envinfo.MCPMemoryName, err)
	}
	var usage cli.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("the refusal must be a usage error (exit 2), got %T", err)
	}
}

// TestLoadHome_RefusesContainment proves the same writable-workspace
// containment rule Load enforces still applies when Root comes from a
// pixhome Selected rather than a config registry entry.
func TestLoadHome_RefusesContainment(t *testing.T) {
	parent := t.TempDir()
	home := pixhome.New(filepath.Join(parent, "pixhome"))
	if err := os.MkdirAll(home.Envs, 0o700); err != nil {
		t.Fatal(err)
	}
	root := home.EnvironmentDir("home")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)

	sel, err := ResolveIn(home, "home")
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	// A caller-supplied writable mount that happens to be an ancestor of
	// root must be refused.
	_, err = LoadHome(sel, EffectiveMounts{{Path: parent}}, nil)
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("expected a containment refusal, got %v", err)
	}
}
