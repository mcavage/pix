package inference

import (
	"os"
	"path/filepath"
	"testing"
)

// catalog_source_test.go — the inspection-surface half of "eliminate magic
// model fallback visibility": DescribeCatalogSource must tell a caller
// EXACTLY which catalog LoadCatalog will read (an override on disk, else
// the binary's embedded default) and where the release-materialized,
// on-disk copy of that embedded default lives, without itself becoming a
// second source of truth LoadCatalog does not also honor.

func TestRuntimeCatalogPath_IsUnderHomeRuntimeVersion(t *testing.T) {
	got := RuntimeCatalogPath("/home/u/.pix", "1.2.3")
	want := filepath.Join("/home/u/.pix", "runtime", "1.2.3", "models.json")
	if got != want {
		t.Fatalf("RuntimeCatalogPath = %q, want %q", got, want)
	}
}

func TestDescribeCatalogSource_EmbeddedWhenNoOverrideOnDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_MODEL_CATALOG", "")

	info := DescribeCatalogSource(home, "9.9.9")
	if info.Source != "embedded" {
		t.Fatalf("Source = %q, want %q", info.Source, "embedded")
	}
	if info.OverridePath != "" {
		t.Fatalf("OverridePath = %q, want empty when nothing is configured on disk", info.OverridePath)
	}
	wantRuntime := filepath.Join(home, "runtime", "9.9.9", "models.json")
	if info.RuntimePath != wantRuntime {
		t.Fatalf("RuntimePath = %q, want %q", info.RuntimePath, wantRuntime)
	}
	if info.RuntimePathExists {
		t.Fatal("RuntimePathExists = true, want false: nothing was installed at that path")
	}
}

func TestDescribeCatalogSource_RuntimePathExistsWhenInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_MODEL_CATALOG", "")

	runtimeDir := filepath.Join(home, "runtime", "9.9.9")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "models.json"), []byte(`{"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	info := DescribeCatalogSource(home, "9.9.9")
	if !info.RuntimePathExists {
		t.Fatal("RuntimePathExists = false, want true: the file is right there")
	}
	if info.Source != "embedded" {
		t.Fatalf("Source = %q, want %q: DescribeCatalogSource must not start READING from the runtime path", info.Source, "embedded")
	}
}

func TestDescribeCatalogSource_OverrideWinsWhenPresentOnDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	override := filepath.Join(t.TempDir(), "custom-models.json")
	if err := os.WriteFile(override, []byte(`{"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_MODEL_CATALOG", override)

	info := DescribeCatalogSource(home, "9.9.9")
	if info.Source != "override" {
		t.Fatalf("Source = %q, want %q", info.Source, "override")
	}
	if info.OverridePath != override {
		t.Fatalf("OverridePath = %q, want %q", info.OverridePath, override)
	}
}

func TestDescribeCatalogSource_MissingOverrideFileIsNotAnOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_MODEL_CATALOG", filepath.Join(t.TempDir(), "does-not-exist.json"))

	info := DescribeCatalogSource(home, "9.9.9")
	if info.Source != "embedded" {
		t.Fatalf("Source = %q, want %q: a $PIX_MODEL_CATALOG pointing at a missing file is not an active override", info.Source, "embedded")
	}
}
