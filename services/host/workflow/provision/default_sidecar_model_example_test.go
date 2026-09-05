package provision

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"pix/host/inference"
	"pix/host/pixhome"
)

// default_sidecar_model_example_test.go — the scaffolded default pix.toml's
// commented-out [models].main example must name a model that ACTUALLY EXISTS
// in the shipped catalog (never a stale id a catalog refresh silently
// orphaned) and must describe the real fallback rule (the shipped default
// for a configured provider, resolveRunModel's own behavior — never Pi's own
// native default, which safety invariant 10 explicitly forbids falling back
// to). A comment that names a dead model or a wrong rule is exactly the
// "invisible input read as magic" failure this feature exists to end.

var modelsMainExampleRE = regexp.MustCompile(`(?m)^#\s*main\s*=\s*"([^"]+)"`)

func TestDefaultSidecar_ModelExampleIsARealCatalogModel(t *testing.T) {
	home := pixhome.New(t.TempDir())
	if _, err := EnsureDefaultEnvironment(home, testManifest()); err != nil {
		t.Fatalf("EnsureDefaultEnvironment: %v", err)
	}
	sidecarPath := filepath.Join(home.EnvironmentDir(DefaultEnvironmentName), "pix.toml")
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("read %s: %v", sidecarPath, err)
	}
	content := string(raw)

	m := modelsMainExampleRE.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("no commented `# main = \"...\"` example found in the generated pix.toml:\n%s", content)
	}
	exampleModel := m[1]
	if !inference.IsQualifiedID(exampleModel) {
		t.Fatalf("example model %q is not a qualified provider/id", exampleModel)
	}

	catalog, err := inference.LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	got, ok := catalog.Get(exampleModel)
	if !ok {
		t.Fatalf("example model %q does not exist in the shipped catalog; pix.toml's comment has gone stale", exampleModel)
	}
	if !got.Available || got.Local {
		t.Fatalf("example model %q must be an available, non-local catalog model; got %+v", exampleModel, got)
	}

	// The explanation must describe the REAL rule, never Pi's own stale
	// native default (safety invariant 10; resolveRunModel's own doc
	// comment: "prevents Pi's own stale native default from taking over").
	if strings.Contains(content, "Pi's own default") || strings.Contains(content, "Pi's default") {
		t.Fatalf("pix.toml's [models] comment still claims an empty main falls back to Pi's own default, which safety invariant 10 forbids:\n%s", content)
	}
}
