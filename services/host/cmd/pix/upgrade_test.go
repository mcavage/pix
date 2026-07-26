package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseUpgradeArgs(t *testing.T) {
	o, err := parseUpgradeArgs(nil)
	if err != nil || o.Check || o.Force || o.Version != "" {
		t.Errorf("no args = (%+v,%v), want a zero opts", o, err)
	}
	if o, err = parseUpgradeArgs([]string{"--check"}); err != nil || !o.Check {
		t.Errorf("--check = (%+v,%v)", o, err)
	}
	// Both spellings, and a leading v is accepted for symmetry with the tags.
	for _, argv := range [][]string{{"--version", "0.1.4"}, {"--version=0.1.4"}, {"--version", "v0.1.4"}} {
		o, err := parseUpgradeArgs(argv)
		if err != nil || o.Version != "0.1.4" {
			t.Errorf("%v = (%q,%v), want 0.1.4", argv, o.Version, err)
		}
	}
	// A non-release value must be refused up front rather than producing a 404
	// against a download URL that was never going to exist.
	for _, bad := range []string{"main", "latest", "0.1", "0.1.4-rc1", ""} {
		if _, err := parseUpgradeArgs([]string{"--version", bad}); err == nil {
			t.Errorf("--version %q should be rejected", bad)
		}
	}
	if _, err := parseUpgradeArgs([]string{"--nope"}); err == nil {
		t.Error("unknown flag should error")
	}
	if _, err := parseUpgradeArgs([]string{"--version"}); err == nil {
		t.Error("--version with no value should error")
	}
}

func TestPlanUpgrade(t *testing.T) {
	cases := []struct {
		name             string
		current, latest  string
		o                upgradeOpts
		wantTarget       string
		wantErr          bool
		wantMsgContains  string
		wantNoopWithMsg  bool
		wantDowngradeMsg bool
	}{
		{
			name: "plain upgrade", current: "0.1.0", latest: "0.1.4",
			wantTarget: "0.1.4", wantMsgContains: "Upgrading pix 0.1.0 -> 0.1.4",
		},
		{
			name: "already latest is a no-op, not an error", current: "0.1.4", latest: "0.1.4",
			wantNoopWithMsg: true, wantMsgContains: "already the latest",
		},
		{
			name: "--force reinstalls the same version", current: "0.1.4", latest: "0.1.4",
			o: upgradeOpts{Force: true}, wantTarget: "0.1.4",
		},
		{
			name: "--version overrides latest", current: "0.1.0", latest: "0.1.4",
			o: upgradeOpts{Version: "0.1.2"}, wantTarget: "0.1.2",
		},
		{
			name: "--version backwards is labelled a downgrade", current: "0.1.4", latest: "0.1.4",
			o: upgradeOpts{Version: "0.1.0"}, wantTarget: "0.1.0", wantDowngradeMsg: true,
		},
		{
			name: "unresolvable latest is an error, not a silent no-op", current: "0.1.0", latest: "",
			wantErr: true, wantMsgContains: "",
		},
		{
			name: "--version still works offline", current: "0.1.0", latest: "",
			o: upgradeOpts{Version: "0.1.4"}, wantTarget: "0.1.4",
		},
		{
			name: "a local build is refused", current: "0.1.4+local", latest: "0.1.4",
			wantErr: true,
		},
		{
			name: "a dev build is refused", current: "dev", latest: "0.1.4",
			wantErr: true,
		},
		{
			name: "--force overrides the local-build refusal", current: "0.1.4+local", latest: "0.1.4",
			o: upgradeOpts{Force: true}, wantTarget: "0.1.4",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planUpgrade(c.current, c.latest, c.o)
			if c.wantErr {
				if p.Err == nil {
					t.Fatalf("want an error, got target=%q msg=%q", p.Target, p.Message)
				}
				return
			}
			if p.Err != nil {
				t.Fatalf("unexpected error: %v", p.Err)
			}
			if p.Target != c.wantTarget {
				t.Errorf("target = %q, want %q", p.Target, c.wantTarget)
			}
			if c.wantNoopWithMsg && p.Target != "" {
				t.Errorf("expected a no-op, got target %q", p.Target)
			}
			if c.wantMsgContains != "" && !strings.Contains(p.Message, c.wantMsgContains) {
				t.Errorf("message %q missing %q", p.Message, c.wantMsgContains)
			}
			if c.wantDowngradeMsg && !strings.Contains(p.Message, "DOWNGRADING") {
				t.Errorf("a backwards move must say so, got %q", p.Message)
			}
		})
	}
}

// The refusal must name the escape hatch, or someone with a local build is just
// stuck wondering why upgrade does nothing.
func TestPlanUpgrade_LocalBuildRefusalIsActionable(t *testing.T) {
	p := planUpgrade("0.1.4+local", "0.1.4", upgradeOpts{})
	if p.Err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"local build", "make install", "--force"} {
		if !strings.Contains(p.Err.Error(), want) {
			t.Errorf("refusal %q missing %q", p.Err, want)
		}
	}
}

func TestOlderVersion(t *testing.T) {
	older := [][2]string{{"0.1.0", "0.1.4"}, {"0.1.9", "0.2.0"}, {"0.9.9", "1.0.0"}, {"1.2.3", "1.10.0"}}
	for _, c := range older {
		if !olderVersion(c[0], c[1]) {
			t.Errorf("olderVersion(%q,%q) = false, want true", c[0], c[1])
		}
	}
	notOlder := [][2]string{{"0.1.4", "0.1.0"}, {"0.1.4", "0.1.4"}, {"1.0.0", "0.9.9"}, {"1.10.0", "1.2.3"}}
	for _, c := range notOlder {
		if olderVersion(c[0], c[1]) {
			t.Errorf("olderVersion(%q,%q) = true, want false", c[0], c[1])
		}
	}
}

func TestUpgradeInstallerURL_PinsTheTargetTag(t *testing.T) {
	got := upgradeInstallerURL("0.1.4")
	if !strings.Contains(got, "/v0.1.4/install.sh") {
		t.Errorf("installer URL %q must be pinned to the target tag, not main", got)
	}
	if strings.Contains(got, "/main/") {
		t.Errorf("installer URL %q must not track main", got)
	}
}

// execInstaller must run the fetched script with the version pinned and the
// prefix pointed at the running binary's directory — the two things that make
// this different from the README's curl|sh.
func TestExecInstaller_PinsVersionAndPrefix(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env.txt")
	script := "#!/bin/sh\nprintf 'V=%s P=%s\\n' \"$PIX_VERSION\" \"$PIX_PREFIX\" > " + out + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(script))
	}))
	defer srv.Close()

	prefix := t.TempDir()
	if err := execInstallerFrom(srv.URL, "0.1.4", prefix, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("execInstaller: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(b)), "V=0.1.4 P="+prefix; got != want {
		t.Errorf("installer env = %q, want %q", got, want)
	}
}

func TestExecInstaller_FailuresDoNotClaimSuccess(t *testing.T) {
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer missing.Close()
	err := execInstallerFrom(missing.URL, "9.9.9", t.TempDir(), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a 404 installer must be an error")
	}
	if !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("error %q should name the version that could not be found", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\nexit 3\n"))
	}))
	defer failing.Close()
	if err := execInstallerFrom(failing.URL, "0.1.4", t.TempDir(), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("a non-zero installer exit must surface as an error")
	}
}

// installPrefix must point at the directory of the RUNNING binary, so an
// upgrade replaces this install instead of planting a second copy elsewhere.
func TestInstallPrefix_IsTheRunningBinarysDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	// No `pix` on PATH in a test binary, so this takes the executable-dir branch.
	if got, want := installPrefix(), filepath.Dir(exe); got != want {
		t.Errorf("installPrefix() = %q, want %q", got, want)
	}
}

// installPrefix must hand install.sh the path spelled the way $PATH spells it.
// Resolving symlinks makes the script's own `command -v pix` = $PIX_PREFIX/pix
// checks compare unequal under a symlinked ancestor (/tmp -> /private/tmp on
// macOS, a symlinked $HOME on Linux), and the upgrade dies claiming a collision
// with the exact path it is installing to. Caught by running it for real.
func TestInstallPrefix_KeepsThePathSpelling(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "bin")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// A "pix" that is genuinely this test binary, reached through the symlink.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	if err := os.Symlink(exe, filepath.Join(target, "pix")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	t.Setenv("PATH", link+string(os.PathListSeparator)+os.Getenv("PATH"))

	if got := installPrefix(); got != link {
		t.Errorf("installPrefix() = %q, want the PATH spelling %q (not its realpath %q)", got, link, target)
	}
}

// When the pix on PATH is a DIFFERENT binary, that is a real collision and
// install.sh must be the one to report it — don't silently install over it.
func TestInstallPrefix_ForeignPixOnPathIsNotAdopted(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "pix")
	if err := os.WriteFile(other, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	if got, want := installPrefix(), filepath.Dir(exe); got != want {
		t.Errorf("installPrefix() = %q, want %q (an unrelated pix on PATH must not be adopted)", got, want)
	}
}
