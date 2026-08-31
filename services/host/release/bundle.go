// bundle.go — the INSTALLED release bundle: the `release-manifest.json` and
// `pix-runtime-<version>.tar.gz` that ship next to the `pix` binary itself
// (docs/design/pix-v2-architecture.md §3, §12: "CI ... builds the one `pix`
// binary, assembles runtime content, ... and emits the release manifest").
//
// Two shapes exist on purpose and must not be confused:
//
//   - the SHIPPED document (scripts/release/emit-manifest.mjs): a versioned
//     envelope with nested `artifacts` and a `generatedAt` stamp, which is
//     what release CI writes and what `pix setup` reads off disk here;
//   - the RECORDED document (state.go): the flat internal Manifest this
//     module validates, compares, and stores at <home>/state/release.json.
//
// ParseBundleManifest is the one translation between them, so nothing else
// in the launcher has to know the shipped envelope's shape.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// BundleManifestFile is the release manifest's file name as published: it
// sits in the SAME directory as the installed `pix` binary.
const BundleManifestFile = "release-manifest.json"

// RuntimeArchiveName is the runtime archive's published file name for a
// version — the same name `make runtime-archive` writes into out/.
func RuntimeArchiveName(version string) string {
	return "pix-runtime-" + version + ".tar.gz"
}

// Bundle is a discovered, parsed, on-disk release bundle.
type Bundle struct {
	// Dir is the directory the bundle was found in (the resolved binary's
	// directory, symlinks followed).
	Dir string
	// ManifestPath and ArchivePath are absolute paths to the two files.
	ManifestPath string
	ArchivePath  string
	// Manifest is the parsed, validated manifest.
	Manifest Manifest
}

// Locator reports the path of the running executable. os.Executable is the
// production implementation; a test injects one pointing at a fake install
// tree, which is the whole reason this is a seam rather than a direct call.
type Locator func() (string, error)

// DefaultLocator is the production Locator.
var DefaultLocator Locator = os.Executable

// installRemedy is the ONE exact remedy every bundle failure prints. It is a
// concrete command, not "reinstall pix": a user who hits this has a partial
// installation and needs to know precisely what to run (safety invariant 12
// — a failure names one owner and one exact next action).
const installRemedy = "Install a complete release (binary + release-manifest.json + runtime archive):\n" +
	"  curl -fsSL https://raw.githubusercontent.com/mcavage/pix/main/install.sh | sh\n" +
	"or, from a source checkout:  make install"

// DiscoverBundle finds the release bundle adjacent to the RESOLVED
// executable. The resolution step is load-bearing: `make install` symlinks
// ~/.local/bin/pix at out/pix, and the bundle lives beside the real binary
// in out/, not beside the symlink.
func DiscoverBundle(locate Locator) (*Bundle, error) {
	if locate == nil {
		locate = DefaultLocator
	}
	exe, err := locate()
	if err != nil {
		return nil, fmt.Errorf("locate the running pix binary: %w\n%s", err, installRemedy)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w\n%s", exe, err, installRemedy)
	}
	dir := filepath.Dir(resolved)
	return LoadBundle(dir)
}

// LoadBundle reads and validates the bundle in dir. It is separate from
// DiscoverBundle so a caller that already knows the directory (a test, a
// future `--bundle` flag) skips the executable resolution entirely.
func LoadBundle(dir string) (*Bundle, error) {
	manifestPath := filepath.Join(dir, BundleManifestFile)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no release manifest next to the pix binary: %s is missing\n%s", manifestPath, installRemedy)
		}
		return nil, fmt.Errorf("read %s: %w\n%s", manifestPath, err, installRemedy)
	}
	m, err := ParseBundleManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", manifestPath, err, installRemedy)
	}
	archivePath := filepath.Join(dir, RuntimeArchiveName(m.Version))
	info, err := os.Stat(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("release manifest %s pins version %s, but its runtime archive %s is missing\n%s",
				manifestPath, m.Version, archivePath, installRemedy)
		}
		return nil, fmt.Errorf("stat %s: %w\n%s", archivePath, err, installRemedy)
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %s)\n%s", archivePath, info.Mode(), installRemedy)
	}
	return &Bundle{
		Dir:          dir,
		ManifestPath: manifestPath,
		ArchivePath:  archivePath,
		Manifest:     *m,
	}, nil
}

// shippedManifest is the published envelope emit-manifest.mjs writes.
type shippedManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
	Artifacts     struct {
		Agent   shippedArtifact `json:"pix-agent"`
		Memory  shippedArtifact `json:"pix-memory"`
		Runtime shippedArtifact `json:"runtime"`
	} `json:"artifacts"`
	KitRevision string `json:"kitRevision"`
	GeneratedAt string `json:"generatedAt"`
}

type shippedArtifact struct {
	Digest string `json:"digest"`
}

// BundleSchemaVersion is the only shipped envelope version this launcher
// understands. A future schema must fail loudly here rather than be
// half-read: setup gates Docker and Gateway mutation on this document.
const BundleSchemaVersion = 1

// ParseBundleManifest decodes the SHIPPED release-manifest.json and returns
// the equivalent internal Manifest, fully validated (every digest immutable,
// version and kit revision present).
func ParseBundleManifest(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var s shippedManifest
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse release manifest: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse release manifest: trailing data after the document")
	}
	if s.SchemaVersion != BundleSchemaVersion {
		return nil, fmt.Errorf("parse release manifest: unsupported schemaVersion %d (this pix understands %d)", s.SchemaVersion, BundleSchemaVersion)
	}
	m := Manifest{
		Version:         s.Version,
		PixAgentDigest:  s.Artifacts.Agent.Digest,
		PixMemoryDigest: s.Artifacts.Memory.Digest,
		RuntimeDigest:   s.Artifacts.Runtime.Digest,
		KitRevision:     s.KitRevision,
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// VerifyArchive streams the runtime archive and checks its sha256 against
// the manifest's runtime_digest. A mismatch is fatal and never repaired:
// setup has no way to tell a truncated download from a tampered one, and
// both have the same fix.
func (b Bundle) VerifyArchive() error {
	sum, err := sha256File(b.ArchivePath)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, installRemedy)
	}
	if sum != b.Manifest.RuntimeDigest {
		return fmt.Errorf("runtime archive %s does not match the release manifest:\n  manifest runtime_digest: %s\n  archive sha256:          %s\n%s",
			b.ArchivePath, b.Manifest.RuntimeDigest, sum, installRemedy)
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
