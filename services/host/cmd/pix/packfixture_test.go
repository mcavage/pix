// packfixture_test.go — the two pack fixtures cmd/pix tests still need. Copies,
// not exports: a test helper that writes a pack on disk and one that pretends
// git succeeded are three lines each, and exporting them would put test
// scaffolding in the pack package's public API for no gain.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
	"pix/host/workflow/pack"
)

func mustWritePack(t *testing.T, root string, m pack.Manifest) {
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
	mustWritePack(t, root, pack.Manifest{Name: name, Schema: 1,
		Proxies: []pack.PackProxy{{Name: wrapper, Host: true}}})
	return root
}

// brokenPackLock makes writePackLock(root, ...) fail deterministically: the
// destination pack.lock is a non-empty DIRECTORY, so the atomic tmp+rename in
// writePackLock fails (rename onto a directory), while everything else in the
// pack root (pack.toml, bin/) stays perfectly readable/writable.
func brokenPackLock(t *testing.T, root string) {
	t.Helper()
	lockDir := pack.PackLockPath(root)
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
