package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/pixhome"
)

// env_cmd_l1_test.go — L1 (security re-review), proven at the REAL command
// boundary: `dispatch(["env", ..., "--effective"], ...)`, the exact entry
// point a user or a script hits, never RenderEffectiveDocument called
// directly. A canary token is planted on disk exactly where `pix setup`
// would leave a real one; the captured stdout is scanned for it the same
// way a reviewer would scan a leaked log.
const envCmdCanaryToken = "CANARY-TOKEN-DO-NOT-LEAK-cli-9d21"

func envCmdTestHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "pixhome")
	p := pixhome.New(home)
	if err := os.MkdirAll(p.StateMemory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(container.MemoryAuthTokenPath(p), []byte(envCmdCanaryToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestEnvShowEffective_NeverPrintsTheRealMemoryToken(t *testing.T) {
	home := envCmdTestHome(t)
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}

	code := dispatch([]string{"env", "show", "--effective"}, d)

	if code != 0 {
		t.Fatalf("dispatch exit = %d, want 0; stderr=%s", code, errb.String())
	}
	got := out.String()
	if strings.Contains(got, envCmdCanaryToken) {
		t.Fatalf("`pix env show --effective` leaked the real memory token:\n%s", got)
	}
	if !strings.Contains(got, "token="+container.RedactedTokenPlaceholder) {
		t.Fatalf("`pix env show --effective` did not show the redacted marker; got:\n%s", got)
	}
}

// TestEnvShow_JSONAndPlain_NeverPrintTheRealMemoryToken is the broader
// canary scan L1 asks for: neither `pix env show` form composes the
// pix-memory built-in today (it is added only at effective-compile time),
// but a future regression that started folding it into the trust
// BOM/show output would be caught here rather than discovered live.
func TestEnvShow_JSONAndPlain_NeverPrintTheRealMemoryToken(t *testing.T) {
	home := envCmdTestHome(t)
	envRoot := filepath.Join(home, "envs", "work")
	if err := os.MkdirAll(envRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envRoot, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	for _, args := range [][]string{
		{"env", "show", "work"},
		{"env", "show", "work", "--json"},
		{"env", "list"},
		{"env", "list", "--json"},
	} {
		var out, errb bytes.Buffer
		d := &cli.Deps{Out: &out, Err: &errb}
		code := dispatch(args, d)
		if code != 0 {
			t.Fatalf("dispatch(%v) exit = %d, stderr=%s", args, code, errb.String())
		}
		if strings.Contains(out.String(), envCmdCanaryToken) {
			t.Fatalf("dispatch(%v) leaked the real memory token:\n%s", args, out.String())
		}
	}
}
