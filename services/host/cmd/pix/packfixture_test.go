// packfixture_test.go — the two pack fixtures cmd/pix tests still need. Copies,
// not exports: a test helper that writes a pack on disk and one that pretends
// git succeeded are three lines each, and exporting them would put test
// scaffolding in the pack package's public API for no gain.
package main

import (
	"os"
	"path/filepath"
	"pix/host/packinfo"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
	"pix/host/workflow/pack"
)

func mustWritePack(t *testing.T, root string, m packinfo.Manifest) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pack.WriteManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

// fakeGitEnv records git invocations and pretends they succeed.
func fakeGitEnv(calls *[]string) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if calls != nil {
			*calls = append(*calls, name+" "+strings.Join(args, " "))
		}
		return "", nil
	}}}
}

var _ = filepath.Join

// phase2HostPack writes a pack with one host proxy wrapper and returns its
// root. XDG_STATE_HOME must already point at a temp dir. A copy of pack's own
// fixture: it is test-only, so exporting it would put scaffolding in pack's
// public API.
func phase2HostPack(t *testing.T, dir, name, wrapper string) string {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", wrapper), []byte("#!/bin/sh\necho "+wrapper+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, root, packinfo.Manifest{Name: name, Schema: 1,
		Proxies: []packinfo.PackProxy{{Name: wrapper, Host: true}}})
	return root
}

// writeFile is a three-line test write. reset has its own copy; sharing one
// across a package boundary costs more than the duplication.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
