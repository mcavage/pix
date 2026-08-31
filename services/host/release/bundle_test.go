package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"pix/host/release"
)

const digestA = "sha256:" + "aa11" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

// shippedManifestJSON renders exactly what scripts/release/emit-manifest.mjs
// writes, so a drift between the two shapes fails here instead of on a
// user's first `pix setup`.
func shippedManifestJSON(version, agent, memory, runtimeDigest string) []byte {
	doc := map[string]any{
		"schemaVersion": 1,
		"version":       version,
		"artifacts": map[string]any{
			"pix-agent":  map[string]string{"digest": agent},
			"pix-memory": map[string]string{"digest": memory},
			"runtime":    map[string]string{"digest": runtimeDigest},
		},
		"kitRevision": "0123456789abcdef0123456789abcdef01234567",
		"generatedAt": "2024-01-01T00:00:00.000Z",
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// runtimeArchive builds a gzip tar with the canonical runtime/<version>/
// layout plus whatever extra entries a test wants, and returns its bytes and
// sha256 digest.
func runtimeArchive(t *testing.T, version string, extra func(tw *tar.Writer)) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	prefix := "runtime/" + version + "/"
	writeDir := func(name string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(name, content string, mode int64) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	writeDir(prefix)
	writeDir(prefix + "skills/")
	writeFile(prefix+"skills/plan/SKILL.md", "# plan\n", 0o644)
	writeDir(prefix + "agents/")
	writeFile(prefix+"agents/deep.md", "deep\n", 0o644)
	writeFile(prefix+"pi/settings.json", "{\"shipped\":true}\n", 0o644)
	writeFile(prefix+"manifest.json", "{\"schemaVersion\":1}\n", 0o644)
	if extra != nil {
		extra(tw)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), "sha256:" + hex.EncodeToString(sum[:])
}

// installTree writes a fake installation: a "pix" binary plus the adjacent
// release-manifest.json and pix-runtime-<version>.tar.gz.
func installTree(t *testing.T, version string, mutate func(dir string, archive []byte, digest string)) string {
	t.Helper()
	dir := t.TempDir()
	archive, digest := runtimeArchive(t, version, nil)
	if err := os.WriteFile(filepath.Join(dir, "pix"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, release.BundleManifestFile), shippedManifestJSON(version, digestA, digestA, digest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, release.RuntimeArchiveName(version)), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(dir, archive, digest)
	}
	return dir
}

func TestParseBundleManifestReadsTheShippedEnvelope(t *testing.T) {
	m, err := release.ParseBundleManifest(shippedManifestJSON("1.2.3", digestA, digestA, digestA))
	if err != nil {
		t.Fatalf("ParseBundleManifest: %v", err)
	}
	if m.Version != "1.2.3" || m.PixAgentDigest != digestA || m.PixMemoryDigest != digestA || m.RuntimeDigest != digestA {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if m.KitRevision == "" {
		t.Fatal("kit revision must survive the translation")
	}
	if (*m == release.Manifest{}) {
		t.Fatal("a parsed bundle manifest must be nonzero")
	}
}

func TestParseBundleManifestRejectsBadDocuments(t *testing.T) {
	cases := map[string][]byte{
		"unsupported schema": []byte(`{"schemaVersion":2,"version":"1.2.3","artifacts":{"pix-agent":{"digest":"` + digestA + `"},"pix-memory":{"digest":"` + digestA + `"},"runtime":{"digest":"` + digestA + `"}},"kitRevision":"abc1234","generatedAt":"x"}`),
		"mutable tag":        shippedManifestJSON("1.2.3", "latest", digestA, digestA),
		"unknown field":      []byte(`{"schemaVersion":1,"version":"1.2.3","surprise":true}`),
		"not json":           []byte("nope"),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := release.ParseBundleManifest(doc); err == nil {
				t.Fatal("expected a parse/validate failure")
			}
		})
	}
}

func TestDiscoverBundleResolvesASymlinkedInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink install shape is POSIX-only")
	}
	dir := installTree(t, "1.2.3", nil)
	binDir := t.TempDir()
	link := filepath.Join(binDir, "pix")
	if err := os.Symlink(filepath.Join(dir, "pix"), link); err != nil {
		t.Fatal(err)
	}
	// The dev install shape: ~/.local/bin/pix -> out/pix, bundle in out/.
	b, err := release.DiscoverBundle(func() (string, error) { return link, nil })
	if err != nil {
		t.Fatalf("DiscoverBundle through a symlink: %v", err)
	}
	if b.Dir != mustEval(t, dir) {
		t.Fatalf("bundle dir = %q, want the resolved binary's dir %q", b.Dir, dir)
	}
	if err := b.VerifyArchive(); err != nil {
		t.Fatalf("VerifyArchive: %v", err)
	}
}

func mustEval(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestDiscoverBundleNamesTheExactInstallRemedy(t *testing.T) {
	empty := t.TempDir()
	exe := filepath.Join(empty, "pix")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := release.DiscoverBundle(func() (string, error) { return exe, nil })
	if err == nil {
		t.Fatal("a bundle-less installation must fail")
	}
	for _, want := range []string{release.BundleManifestFile, "install.sh", "make install"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name %q, got: %v", want, err)
		}
	}
}

func TestLoadBundleRequiresTheVersionedArchive(t *testing.T) {
	dir := installTree(t, "1.2.3", func(dir string, _ []byte, _ string) {
		if err := os.Remove(filepath.Join(dir, release.RuntimeArchiveName("1.2.3"))); err != nil {
			t.Fatal(err)
		}
	})
	_, err := release.LoadBundle(dir)
	if err == nil || !strings.Contains(err.Error(), release.RuntimeArchiveName("1.2.3")) {
		t.Fatalf("missing archive must be named in the error, got: %v", err)
	}
}

func TestVerifyArchiveRefusesADigestMismatch(t *testing.T) {
	dir := installTree(t, "1.2.3", func(dir string, archive []byte, _ string) {
		corrupt := append(append([]byte{}, archive...), 0x00)
		if err := os.WriteFile(filepath.Join(dir, release.RuntimeArchiveName("1.2.3")), corrupt, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	b, err := release.LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	err = b.VerifyArchive()
	if err == nil {
		t.Fatal("a corrupted archive must fail verification")
	}
	if !strings.Contains(err.Error(), "runtime_digest") {
		t.Fatalf("mismatch error must show both digests, got: %v", err)
	}
	// And nothing may be installed off an unverified archive.
	home := t.TempDir()
	if _, err := release.InstallRuntime(home, *b); err == nil {
		t.Fatal("InstallRuntime must refuse an archive that fails verification")
	}
	if _, err := os.Stat(release.RuntimeDir(home, "1.2.3")); !os.IsNotExist(err) {
		t.Fatalf("nothing may be extracted from an unverified archive: %v", err)
	}
}

func ExampleRuntimeArchiveName() {
	fmt.Println(release.RuntimeArchiveName("2.0.0"))
	// Output: pix-runtime-2.0.0.tar.gz
}
