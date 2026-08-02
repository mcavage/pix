// setup_keys_hostmode_test.go — subject is setupProvisionKeys, a cmd/pix
// workflow, so the test lives here even though the state it inspects belongs
// to the secret capability.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// A ref found ONLY in hostmode.env (secret.CurrentOpRef's cross-file lookup) must be
// backfilled into op-refs.env by setupProvisionKeys itself; if that write
// fails, setup fails outright — even when sbx can't be probed at all (the old
// bug: the ignored backfill let a fail-open final probe mask a real write
// failure).
func TestSetupProvisionKeys_HasRefOnlyInHostMode_UnwritableOpRefsFailsEvenSbxUnavailable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	files := map[string]string{
		// op-refs.env absent; hostmode.env carries all three refs.
		"/cfg/pix/hostmode.env": "ANTHROPIC_API_KEY=op://v/anthropic/key\n" +
			"OPENAI_API_KEY=op://v/openai/key\nGEMINI_API_KEY=op://v/gemini/key\n",
	}
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/cfg"
		}
		return ""
	}, ReadFileFn: func(p string) (string, error) {
		if v, ok := files[p]; ok {
			return v, nil
		}
		return "", os.ErrNotExist
	}, WriteFileFn: func(p string, d []byte, m os.FileMode) error {
		if strings.HasSuffix(p, "op-refs.env") {
			return os.ErrPermission // op-refs.env is unwritable
		}
		files[p] = string(d)
		return nil
	}, LookPathFn: func(name string) (string, error) {
		if name == "sbx" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}, RunFn: func(name string, args ...string) (string, error) {
		if name == "op" && len(args) >= 1 && args[0] == "--version" {
			return "2.0", nil
		}
		if name == "op" && len(args) >= 1 && args[0] == "account" {
			return "acct", nil
		}
		if name == "op" && len(args) >= 1 && args[0] == "read" {
			return "sk-val", nil
		}
		return "", nil
	}}}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, true, false) {
		t.Fatal("an unwritable op-refs.env must fail setup even when sbx can't be probed at all")
	}
	if !strings.Contains(out.String(), "op-refs.env") {
		t.Errorf("must explain the op-refs.env write failure, got:\n%s", out.String())
	}
}
