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
func opRefsFileEnv() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		IsFileFn: func(p string) bool {
			st, err := os.Stat(p)
			return err == nil && !st.IsDir()
		},
	}}
}

func writeRefs(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY=op://v/i/f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFindOpRefs_PixHomeIsTheOnlyCandidate pins QA F5's close-out: the
// resolver looks at <PIX_HOME>/op-refs.env and nowhere else. $PIX_CONFIG, a
// repo checkout's config/op-refs.env, and ~/.config/pix are deleted, not
// deprioritized, so a stray file at any of them is invisible here.
func TestFindOpRefs_PixHomeIsTheOnlyCandidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	env := opRefsFileEnv()

	if got := FindOpRefs(env); got != "" {
		t.Fatalf("FindOpRefs with no file = %q, want \"\" (cannot verify)", got)
	}

	// A file at every DELETED candidate must still resolve to nothing.
	other := t.TempDir()
	writeRefs(t, filepath.Join(other, "op-refs.env"))
	t.Setenv("PIX_CONFIG", filepath.Join(other, "config.toml"))
	t.Setenv("XDG_CONFIG_HOME", other)
	if got := FindOpRefs(env); got != "" {
		t.Errorf("FindOpRefs honored a retired candidate: %q", got)
	}

	want := filepath.Join(home, "op-refs.env")
	writeRefs(t, want)
	got := FindOpRefs(env)
	if got != want {
		t.Errorf("FindOpRefs = %q, want the PIX_HOME file %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolved op-refs must be absolute, got %q", got)
	}
	if p := DefaultOpRefsPath(); p != want {
		t.Errorf("DefaultOpRefsPath = %q, want %q", p, want)
	}
}
