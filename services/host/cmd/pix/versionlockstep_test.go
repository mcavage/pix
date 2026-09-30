package main

// versionlockstep_test.go — a focused, single-purpose lockstep check: the
// three version-bearing files (Makefile VERSION, pi-kit/pix/pix.yaml image tag,
// package.json version) must always carry the exact same version string. CI
// (.github/workflows/publish.yml) stamps all three together on every push to
// main; a hand-bump of only one (or a mismatched manual edit) desyncs the
// pinned-tag invariant a consumer's `sbx run --kit` relies on. This test
// fails with the three concrete values so the mismatch is obvious at a
// glance, not just "versions disagree".

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// repoRootForVersionLockstep walks up from the test's working directory to
// the repo root (identified by the presence of both Makefile and
// pi-kit/pix/pix.yaml). Skips when run outside a full checkout.
func repoRootForVersionLockstep(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "Makefile")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "pi-kit", "pix", "pix.yaml")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no repo root with Makefile + pi-kit/pix/pix.yaml above the test dir")
		}
		dir = parent
	}
}

// TestReleaseVersionsLockstep asserts Makefile VERSION, pi-kit/pix/pix.yaml's
// pinned image tag, and package.json's version all agree, reporting each
// concrete value on failure.
func TestReleaseVersionsLockstep(t *testing.T) {
	root := repoRootForVersionLockstep(t)

	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	spec, err := os.ReadFile(filepath.Join(root, "pi-kit", "pix", "pix.yaml"))
	if err != nil {
		t.Fatalf("read pi-kit/pix/pix.yaml: %v", err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "pi-kit", "pix", "pix.dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}

	mkMatch := regexp.MustCompile(`(?m)^VERSION\s*\?=\s*(\S+)`).FindSubmatch(makefile)
	if mkMatch == nil {
		t.Fatal("Makefile: no `VERSION ?= <version>` line found")
	}
	specMatch := regexp.MustCompile(`(?m)^version:\s*["']?([^"'\s]+)`).FindSubmatch(spec)
	if specMatch == nil {
		t.Fatal(`pi-kit/pix/pix.yaml: no pinned version`)
	}
	baseMatch := regexp.MustCompile(`(?m)^ARG PIX_AGENT_IMAGE=docker\.io/[^\s]*/pix-agent:([^\s]+)`).FindSubmatch(dockerfile)
	if baseMatch == nil {
		t.Fatal("pix.dockerfile: no pinned pix-agent base image")
	}
	pkgMatch := regexp.MustCompile(`"version"\s*:\s*"([^"]+)"`).FindSubmatch(pkg)
	if pkgMatch == nil {
		t.Fatal(`package.json: no "version": "<version>" field found`)
	}

	mkVersion := string(mkMatch[1])
	specVersion := string(specMatch[1])
	pkgVersion := string(pkgMatch[1])

	if mkVersion != specVersion || specVersion != pkgVersion || specVersion != string(baseMatch[1]) {
		t.Errorf(
			"release version lockstep broken: Makefile VERSION=%q, pi-kit/pix/pix.yaml image tag=%q, package.json version=%q — all three must match exactly",
			mkVersion, specVersion, pkgVersion,
		)
	}

	if specVersion == "latest" {
		t.Error(`pi-kit/pix/pix.yaml image tag must be pinned to a concrete version, never "latest"`)
	}
}
