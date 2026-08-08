package secret

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// realFileEnv answers IsFile from the real filesystem: these tests write real
// files into a temp dir, because "which path does the resolver find" is a
// question about the disk, not about a stub.
func opRefsFileEnv(getenv func(string) string, home, cwd string) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		GetenvFn:  getenv,
		HomeDirFn: func() string { return home },
		GetwdFn:   func() (string, error) { return cwd, nil },
		IsFileFn: func(p string) bool {
			st, err := os.Stat(p)
			return err == nil && !st.IsDir()
		},
	}}
}

func writeRefs(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY=op://v/i/f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFindOpRefs moved here with its subject (it was stranded in the doctor
// workflow, whose report used to print the resolved path). It uses REAL files
// in a temp dir rather than a stat fake: the question is what the resolver
// finds on disk.
func TestFindOpRefs(t *testing.T) {
	cfgDir := t.TempDir()
	writeRefs(t, filepath.Join(cfgDir, "op-refs.env"))
	empty := t.TempDir()
	env := opRefsFileEnv(func(k string) string {
		return map[string]string{"PIX_CONFIG": filepath.Join(cfgDir, "config.toml")}[k]
	}, "", empty)
	if got, want := FindOpRefs(env), filepath.Join(cfgDir, "op-refs.env"); got != want {
		t.Errorf("expected the PIX_CONFIG-dir op-refs %q, got %q", want, got)
	}

	home := t.TempDir()
	writeRefs(t, filepath.Join(home, ".config", "pix", "op-refs.env"))
	env2 := opRefsFileEnv(func(string) string { return "" }, home, t.TempDir())
	got := FindOpRefs(env2)
	if want := filepath.Join(home, ".config", "pix", "op-refs.env"); got != want {
		t.Errorf("expected the home-dir op-refs fallback %q, got %q", want, got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolved op-refs must be absolute, got %q", got)
	}
}
