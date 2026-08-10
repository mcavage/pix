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
