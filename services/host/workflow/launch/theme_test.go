package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadThemePreference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	active := filepath.Join(home, "context", "themes", "active")
	if err := os.MkdirAll(filepath.Dir(active), 0o700); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		contents string
		want     string
	}{
		"fixed":     {contents: "nord\n", want: "nord"},
		"automatic": {contents: "solarized-light/solarized-dark\n", want: ""},
		"spaces":    {contents: "not a theme\n", want: ""},
		"traversal": {contents: "../nord\n", want: ""},
		"too many":  {contents: "one/two/three\n", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(active, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := ReadThemePreference(); got != tc.want {
				t.Fatalf("ReadThemePreference() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadThemePreferenceIgnoresSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	themes := filepath.Join(home, "context", "themes")
	if err := os.MkdirAll(themes, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "selected")
	if err := os.WriteFile(target, []byte("nord\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(themes, "active")); err != nil {
		t.Fatal(err)
	}
	if got := ReadThemePreference(); got != "" {
		t.Fatalf("ReadThemePreference() followed symlink and returned %q", got)
	}
}

func TestBuildPiInvocationCarriesThemeOnEveryLaunch(t *testing.T) {
	got := BuildPiInvocation(nil, RunOpts{Theme: "catppuccin-mocha"})
	want := []string{"--session-dir", ".pi-sessions", "--use-theme", "catppuccin-mocha"}
	if len(got) != len(want) {
		t.Fatalf("BuildPiInvocation() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BuildPiInvocation() = %#v, want %#v", got, want)
		}
	}
	reattach := BuildReattachArgs(RunOpts{Name: "pix-test", Theme: "catppuccin-mocha"})
	wantReattach := []string{"run", "--name", "pix-test", "--", "--session-dir", ".pi-sessions", "--use-theme", "catppuccin-mocha"}
	if len(reattach) != len(wantReattach) {
		t.Fatalf("BuildReattachArgs() = %#v, want %#v", reattach, wantReattach)
	}
	for i := range wantReattach {
		if reattach[i] != wantReattach[i] {
			t.Fatalf("BuildReattachArgs() = %#v, want %#v", reattach, wantReattach)
		}
	}
	host := BuildPiInvocation(nil, RunOpts{Theme: "host"})
	wantHost := []string{"--session-dir", ".pi-sessions", "--no-themes", "--theme", shippedHostThemePath, "--use-theme", "host"}
	if len(host) != len(wantHost) {
		t.Fatalf("BuildPiInvocation(host) = %#v, want %#v", host, wantHost)
	}
	for i := range wantHost {
		if host[i] != wantHost[i] {
			t.Fatalf("BuildPiInvocation(host) = %#v, want %#v", host, wantHost)
		}
	}
}
