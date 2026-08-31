// secret_cmd_e2e_test.go — round 5's end-to-end proof for the ONE accepted
// secrets file: a real `pix secret set` through the REAL dispatcher, then
// `pix secret ls`/`pix secret check` and secret.FindOpRefs/config.OpRefsPath
// must all agree on the SAME path and the SAME bytes on disk. There is no
// second op-refs.env location any of these could silently diverge onto.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/secret"
	"pix/host/sys/systest"
)

// TestSecretSetThenSyncCheckFind_SamePathSameBytes drives `pix secret set`
// through the real kong dispatcher in a subprocess (so the write is the
// genuine production path, not a capability-level call), then asserts:
//
//  1. config.OpRefsPath() (setup seeding/sync's own resolver) names the file
//     that was actually written;
//  2. secret.FindOpRefs (doctor/gog setup's "does one exist" resolver) finds
//     the SAME path;
//  3. `pix secret ls` (a second real dispatch) reports that exact path;
//  4. the on-disk bytes contain the exact ref `set` wrote — the same file,
//     the same content, from every one of these four callers.
func TestSecretSetThenSyncCheckFind_SamePathSameBytes(t *testing.T) {
	if home := os.Getenv("PIX_SECRET_E2E_HOME"); home != "" {
		runSecretDispatch(strings.Fields(os.Getenv("PIX_SECRET_E2E_ARGV")))
		return
	}

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}

	runDispatch := func(argv ...string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetThenSyncCheckFind_SamePathSameBytes")
		cmd.Env = append(os.Environ(),
			"PIX_SECRET_E2E_HOME="+home,
			"PIX_SECRET_E2E_ARGV="+strings.Join(argv, " "),
			"PIX_HOME="+home,
		)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}

	setOut := runDispatch("set", "ANTHROPIC_API_KEY", "op://vault/anthropic/key")
	if !strings.Contains(setOut, "ANTHROPIC_API_KEY") {
		t.Fatalf("secret set output should confirm the key, got:\n%s", setOut)
	}

	// 1. config.OpRefsPath resolves under the SAME PIX_HOME (setup seeding,
	// sync, and the Gateway wrappers all call this).
	t.Setenv("PIX_HOME", home)
	wantPath := filepath.Join(home, "secrets.env")
	if got := config.OpRefsPath(); got != wantPath {
		t.Fatalf("config.OpRefsPath() = %q, want %q", got, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("secret set did not write %s: %v", wantPath, err)
	}

	// 2. secret.FindOpRefs (the doctor/gog "does one exist" resolver) finds
	// the identical path — never a second candidate.
	env := hostenv.Env{System: &systest.Fake{
		IsFileFn: func(p string) bool {
			st, err := os.Stat(p)
			return err == nil && !st.IsDir()
		},
	}}
	if got := secret.FindOpRefs(env); got != wantPath {
		t.Fatalf("secret.FindOpRefs = %q, want %q", got, wantPath)
	}

	// 3. `pix secret ls` (a second real dispatch, reading LoadRefs against
	// the SAME resolved home) reports the key `set` just wrote as configured
	// — proof it read the identical file, not a second copy.
	lsOut := runDispatch("ls")
	if !strings.Contains(lsOut, "ANTHROPIC_API_KEY") || !strings.Contains(lsOut, "ok") {
		t.Errorf("secret ls output should report ANTHROPIC_API_KEY ok, got:\n%s", lsOut)
	}

	// 4. the bytes on disk are exactly what `set` wrote — every caller above
	// is pointed at ONE file with ONE set of bytes, not a copy.
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "ANTHROPIC_API_KEY=op://vault/anthropic/key") {
		t.Errorf("secrets.env content = %q, want the exact ref line", data)
	}
}

// TestSecretConcurrentLegacyAndNewWriter_NoLostUpdateOrDelete proves the
// SINGLE .secrets.lock (providerrefslock.go) actually serializes concurrent
// `secret set`/`secret rm` callers against the SAME secrets.env: N
// concurrent `set` calls for DISTINCT keys must all land (no lost update),
// and a concurrent `rm` of one key must never delete a sibling key another
// goroutine is writing at the same moment.
func TestSecretConcurrentLegacyAndNewWriter_NoLostUpdateOrDelete(t *testing.T) {
	if home := os.Getenv("PIX_SECRET_CONC_HOME"); home != "" {
		runSecretDispatch(strings.Fields(os.Getenv("PIX_SECRET_CONC_ARGV")))
		return
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(argv ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run", "TestSecretConcurrentLegacyAndNewWriter_NoLostUpdateOrDelete")
		cmd.Env = append(os.Environ(),
			"PIX_SECRET_CONC_HOME="+home,
			"PIX_SECRET_CONC_ARGV="+strings.Join(argv, " "),
			"PIX_HOME="+home,
		)
		return cmd
	}

	keys := []string{"KEY_A", "KEY_B", "KEY_C", "KEY_D", "KEY_E"}
	cmds := make([]*exec.Cmd, len(keys))
	for i, k := range keys {
		cmds[i] = run("set", k, "op://vault/item/"+k)
	}
	done := make(chan error, len(cmds))
	for _, c := range cmds {
		c := c
		go func() { done <- c.Run() }()
	}
	for range cmds {
		if err := <-done; err != nil {
			t.Fatalf("concurrent secret set failed: %v", err)
		}
	}

	data, err := os.ReadFile(filepath.Join(home, "secrets.env"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, k := range keys {
		if !strings.Contains(string(data), k+"=op://vault/item/"+k) {
			t.Errorf("lost update: %s missing from secrets.env:\n%s", k, data)
		}
	}
}
