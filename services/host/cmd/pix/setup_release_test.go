// setup_release_test.go — the PRODUCTION boundary test for `pix setup`'s
// release-bundle step. It drives setupCmd.run (the command's own body, the
// same one Run calls) with fake external effects, so what is proven is the
// real sequencing: discover the bundle beside the binary, verify it, install
// the runtime under PIX_HOME, record a NONZERO manifest, and only then
// reconcile Docker and the Gateway.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/envinfo"
	"pix/host/release"
	"pix/host/workflow/provision"
)

type setupFakeDocker struct {
	calls    []string
	imageErr map[string]error
}

func (f *setupFakeDocker) Run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case args[0] == "inspect":
		return "Error: No such object", errors.New("exit status 1")
	case args[0] == "image" && args[1] == "inspect":
		if err, ok := f.imageErr[args[2]]; ok {
			return "no such image", err
		}
		return "[]", nil
	case args[0] == "pull":
		if err, ok := f.imageErr[args[1]]; ok {
			return "pull failed", err
		}
		return "pulled", nil
	default:
		return "id123", nil
	}
}

type setupFakeProber struct{}

func (setupFakeProber) Probe(string) error { return nil }

type setupFakeMCP struct{ url string }

func (m *setupFakeMCP) EnsureMemoryRemote(name, url string) (provision.MCPRegistrationState, error) {
	m.url = url
	return provision.MCPRegistrationAdded, nil
}

type setupFakePrereqs struct{ missing string }

func (p setupFakePrereqs) Check(name string, args ...string) (string, error) {
	if name == p.missing {
		return "", errors.New("executable file not found in $PATH")
	}
	return name + " ok", nil
}

// fakeInstallDir writes a complete installed release bundle: a `pix` binary
// with release-manifest.json and pix-runtime-<version>.tar.gz beside it.
func fakeInstallDir(t *testing.T, version string) (dir string, manifest release.Manifest) {
	t.Helper()
	dir = t.TempDir()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	prefix := "runtime/" + version + "/"
	for name, body := range map[string]string{
		prefix + "skills/plan/SKILL.md": "# plan\n",
		prefix + "agents/deep.md":       "deep\n",
		prefix + "pi/settings.json":     "{\"shipped\":true}\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	runtimeDigest := "sha256:" + hex.EncodeToString(sum[:])
	imageDigest := "sha256:" + strings.Repeat("c", 64)

	doc := `{
  "schemaVersion": 1,
  "version": "` + version + `",
  "artifacts": {
    "pix-agent": { "digest": "` + imageDigest + `" },
    "pix-memory": { "digest": "` + imageDigest + `" },
    "runtime": { "digest": "` + runtimeDigest + `" }
  },
  "kitRevision": "0123456789abcdef0123456789abcdef01234567",
  "generatedAt": "2024-01-01T00:00:00.000Z"
}
`
	write := func(name string, data []byte, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("pix", []byte("#!/bin/sh\n"), 0o755)
	write(release.BundleManifestFile, []byte(doc), 0o644)
	write(release.RuntimeArchiveName(version), buf.Bytes(), 0o644)

	return dir, release.Manifest{
		Version:         version,
		PixAgentDigest:  imageDigest,
		PixMemoryDigest: imageDigest,
		RuntimeDigest:   runtimeDigest,
		KitRevision:     "0123456789abcdef0123456789abcdef01234567",
	}
}

func setupSeamsFor(t *testing.T, dir string, docker *setupFakeDocker, mcp *setupFakeMCP) setupSeams {
	t.Helper()
	return setupSeams{
		DiscoverBundle: func() (*release.Bundle, error) {
			// The REAL discovery path, pointed at a fake install tree
			// through its injected locator: this is what production runs,
			// not a stand-in that returns a hand-built bundle.
			return release.DiscoverBundle(func() (string, error) { return filepath.Join(dir, "pix"), nil })
		},
		Prereqs:         setupFakePrereqs{},
		ContainerRunner: docker,
		Prober:          setupFakeProber{},
		MCP:             mcp,
	}
}

func TestSetupInstallsTheReleaseBundleAndRecordsANonzeroManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	dir, want := fakeInstallDir(t, "2.0.0")
	docker := &setupFakeDocker{}
	mcp := &setupFakeMCP{}

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := (&setupCmd{}).run(d, setupSeamsFor(t, dir, docker, mcp)); err != nil {
		t.Fatalf("pix setup: %v\n%s%s", err, out.String(), errb.String())
	}

	// 1. A NONZERO manifest was recorded, and it is the shipped one.
	got, err := release.LoadInstalled(home)
	if err != nil || got == nil {
		t.Fatalf("LoadInstalled: %v (manifest %v)", err, got)
	}
	if *got != want {
		t.Fatalf("recorded manifest = %+v, want %+v", *got, want)
	}

	// 2. The runtime archive is installed under PIX_HOME/runtime/<version>.
	for _, rel := range []string{"skills/plan/SKILL.md", "agents/deep.md", "pi/settings.json"} {
		if _, err := os.Stat(filepath.Join(release.RuntimeDir(home, "2.0.0"), rel)); err != nil {
			t.Fatalf("runtime is missing %s: %v", rel, err)
		}
	}

	// 3. The pix-memory container was reconciled against the manifest's
	//    digest, not a tag and not a stale prior record.
	wantRef := provision.MemoryImageRef(want)
	if !strings.Contains(strings.Join(docker.calls, "\n"), wantRef) {
		t.Fatalf("docker was never asked for the release-pinned image %s; calls:\n%s", wantRef, strings.Join(docker.calls, "\n"))
	}
	for _, ref := range []string{provision.AgentImageRef(want), wantRef} {
		if !strings.Contains(strings.Join(docker.calls, "\n"), "image inspect "+ref) {
			t.Fatalf("setup never verified image %s; calls:\n%s", ref, strings.Join(docker.calls, "\n"))
		}
	}

	// 4. A host with no environment got exactly one runnable default.
	for _, rel := range []string{"envs/default/.sbxenv.yaml", "envs/default/pix.toml"} {
		if _, err := os.Stat(filepath.Join(home, rel)); err != nil {
			t.Fatalf("default environment is missing %s: %v", rel, err)
		}
	}
	if mcp.url == "" {
		t.Fatal("the reserved pix-memory MCP name was never registered")
	}
	if !strings.Contains(out.String(), "runtime installed") {
		t.Fatalf("setup must report what it installed; got:\n%s", out.String())
	}
}

func TestSetupRefusesAnIncompleteInstallationBeforeTouchingDocker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	dir, _ := fakeInstallDir(t, "2.0.0")

	for _, tc := range []struct {
		name    string
		break_  func()
		wantSub string
	}{
		{"missing manifest", func() { os.Remove(filepath.Join(dir, release.BundleManifestFile)) }, "release-manifest.json"},
		{"corrupt archive", func() {
			os.WriteFile(filepath.Join(dir, release.RuntimeArchiveName("2.0.0")), []byte("not the archive"), 0o644)
		}, "does not match the release manifest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ = fakeInstallDir(t, "2.0.0")
			tc.break_()
			docker := &setupFakeDocker{}
			mcp := &setupFakeMCP{}
			var out, errb bytes.Buffer
			d := &cli.Deps{Out: &out, Err: &errb}

			err := (&setupCmd{}).run(d, setupSeamsFor(t, dir, docker, mcp))
			if err == nil {
				t.Fatal("an incomplete installation must fail setup")
			}
			if !strings.Contains(err.Error(), tc.wantSub) || !strings.Contains(err.Error(), "make install") {
				t.Fatalf("error must name the defect and the exact remedy, got: %v", err)
			}
			if len(docker.calls) != 0 {
				t.Fatalf("Docker was mutated before the bundle was proven: %v", docker.calls)
			}
			if mcp.url != "" {
				t.Fatal("the Gateway was mutated before the bundle was proven")
			}
			if _, err := os.Stat(filepath.Join(home, "state", "release.json")); !os.IsNotExist(err) {
				t.Fatalf("no release may be recorded off an unproven bundle: %v", err)
			}
		})
	}
}

func TestSetupRefusesAMissingPrerequisiteBeforeMutating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	dir, _ := fakeInstallDir(t, "2.0.0")
	docker := &setupFakeDocker{}
	mcp := &setupFakeMCP{}
	seams := setupSeamsFor(t, dir, docker, mcp)
	seams.Prereqs = setupFakePrereqs{missing: "sbx"}

	var out, errb bytes.Buffer
	err := (&setupCmd{}).run(&cli.Deps{Out: &out, Err: &errb}, seams)
	if err == nil || !strings.Contains(err.Error(), "sbx is required") {
		t.Fatalf("a missing prerequisite must fail with its remedy, got: %v", err)
	}
	if len(docker.calls) != 0 {
		t.Fatalf("Docker was mutated despite an unmet prerequisite: %v", docker.calls)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "release.json")); !os.IsNotExist(err) {
		t.Fatal("nothing may be recorded when a prerequisite is unmet")
	}
}

func TestSetupNeverOverwritesAnExistingEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	mine := filepath.Join(home, "envs", "mine")
	if err := os.MkdirAll(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	authored := "schemaVersion: \"1\"\nagent: pix\n# mine\n"
	if err := os.WriteFile(filepath.Join(mine, ".sbxenv.yaml"), []byte(authored), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, _ := fakeInstallDir(t, "2.0.0")

	var out, errb bytes.Buffer
	if err := (&setupCmd{}).run(&cli.Deps{Out: &out, Err: &errb}, setupSeamsFor(t, dir, &setupFakeDocker{}, &setupFakeMCP{})); err != nil {
		t.Fatalf("pix setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "envs", "default")); !os.IsNotExist(err) {
		t.Fatal("setup created a default environment on a host that already had one")
	}
	got, err := os.ReadFile(filepath.Join(mine, ".sbxenv.yaml"))
	if err != nil || string(got) != authored {
		t.Fatalf("an authored environment was rewritten: %q (%v)", got, err)
	}
}

// The default environment setup writes must actually parse as the native
// document plus a valid thin sidecar — a "minimal runnable environment" that
// does not load is not one.
func TestDefaultEnvironmentParses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	dir, _ := fakeInstallDir(t, "2.0.0")
	var out, errb bytes.Buffer
	if err := (&setupCmd{}).run(&cli.Deps{Out: &out, Err: &errb}, setupSeamsFor(t, dir, &setupFakeDocker{}, &setupFakeMCP{})); err != nil {
		t.Fatalf("pix setup: %v", err)
	}
	root := filepath.Join(home, "envs", "default")
	if _, err := envinfo.Parse(filepath.Join(root, ".sbxenv.yaml")); err != nil {
		t.Fatalf("the default .sbxenv.yaml does not parse: %v", err)
	}
	if _, err := envinfo.ParseSidecar(filepath.Join(root, "pix.toml")); err != nil {
		t.Fatalf("the default pix.toml does not parse: %v", err)
	}
}

var _ = container.Name
