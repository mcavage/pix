package supervise

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hostpath_test.go — the launchd environment, which is the ONLY environment real
// users run serve in, and the one my foreground testing did not cover.

// launchdPATH is what launchd actually hands a login service. Every host binary
// in this system lives somewhere else.
const launchdPATH = "/usr/bin:/bin:/usr/sbin:/sbin"

// TestResolveHostCommandFindsAUserInstalledBinaryUnderLaunchd is the regression.
// A daemon declared with `command` resolved in a developer's shell and failed
// permanently under the managed service, because ~/.local/bin is not on the PATH
// launchd provides.
func TestResolveHostCommandFindsAUserInstalledBinaryUnderLaunchd(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "pix-test-daemon")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", launchdPATH)

	got, err := resolveHostCommand("pix-test-daemon")
	if err != nil {
		t.Fatalf("a binary in ~/.local/bin must resolve under launchd's PATH: %v", err)
	}
	if got != exe {
		t.Errorf("resolved %q, want %q", got, exe)
	}
}

// TestResolveHostCommandIgnoresANonExecutable: a same-named data file must not
// be mistaken for the binary, or the daemon fails with a confusing exec error
// instead of "not found".
func TestResolveHostCommandIgnoresANonExecutable(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "pix-test-notexec"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", launchdPATH)

	if _, err := resolveHostCommand("pix-test-notexec"); err == nil {
		t.Error("a non-executable file was accepted as the daemon binary")
	}
}

// TestResolveHostCommandErrorNamesEveryPlaceItLooked: "is not on PATH" is
// actively misleading under launchd, where PATH is four system directories and
// the user's interactive PATH does contain the binary. The message has to say
// where it looked so the answer is actionable.
func TestResolveHostCommandErrorNamesEveryPlaceItLooked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", launchdPATH)

	_, err := resolveHostCommand("pix-definitely-absent-binary")
	if err == nil {
		t.Fatal("an absent binary must not resolve")
	}
	msg := err.Error()
	for _, want := range []string{
		"pix-definitely-absent-binary",
		launchdPATH,
		filepath.Join(home, ".local", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error must name %q so a user knows where to install it; got: %v", want, err)
		}
	}
}

// TestResolveHostCommandRefusesAPath: `command` is a bare name by contract. A
// path would be a second, unpinned spelling of UnitSpec.Path, dodging the sha
// requirement that form carries.
func TestResolveHostCommandRefusesAPath(t *testing.T) {
	for _, name := range []string{"/usr/bin/true", "./local-thing", "sub/dir/thing"} {
		if _, err := resolveHostCommand(name); err == nil {
			t.Errorf("%q was accepted; command must be a bare binary name", name)
		}
	}
}

// TestWithHostBinPathLetsADaemonFindItsTools is the second half of the same bug,
// and the more dangerous half: a daemon inheriting launchd's PATH starts, binds,
// and answers its health check while being unable to find the vendor CLI it
// shells out to. Every row is green and the capability is broken.
func TestWithHostBinPathLetsADaemonFindItsTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := withHostBinPath([]string{"PATH=" + launchdPATH, "OTHER=keep"})
	var path string
	for _, kv := range got {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	for _, want := range append([]string{launchdPATH}, hostBinDirs()...) {
		if !strings.Contains(path, want) {
			t.Errorf("child PATH %q is missing %q", path, want)
		}
	}
	// Everything else in the environment is untouched.
	if len(got) != 2 || got[1] != "OTHER=keep" {
		t.Errorf("withHostBinPath altered more than PATH: %v", got)
	}
}

// TestWithHostBinPathRespectsTheAllowlist: a unit that did not ask for PATH must
// not be handed one. The allowlist is the entire contract for what a supervised
// unit can see, and widening it here would make it a suggestion.
func TestWithHostBinPathRespectsTheAllowlist(t *testing.T) {
	got := withHostBinPath([]string{"HOME=/x"})
	for _, kv := range got {
		if strings.HasPrefix(kv, "PATH=") {
			t.Fatalf("PATH was injected into a unit that never allowed it: %v", got)
		}
	}
}

// TestWithHostBinPathDoesNotDuplicate: a developer's PATH already has these
// directories, and a PATH that repeats them grows on every generation.
func TestWithHostBinPathDoesNotDuplicate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := filepath.Join(home, ".local", "bin")
	got := withHostBinPath([]string{"PATH=" + local + ":/opt/homebrew/bin:/usr/bin"})
	path := strings.TrimPrefix(got[0], "PATH=")
	if n := strings.Count(path, local); n != 1 {
		t.Errorf("%q appears %d times in %q, want 1", local, n, path)
	}
	if n := strings.Count(path, "/opt/homebrew/bin"); n != 1 {
		t.Errorf("/opt/homebrew/bin appears %d times in %q, want 1", n, path)
	}
}

// TestDaemonStartsUnderLaunchdPATH is the end-to-end version: the whole launch
// path, with the environment a managed service actually gets.
func TestDaemonStartsUnderLaunchdPATH(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDaemon(t, bin, "launchd-proxy", 0)
	t.Setenv("HOME", home)
	t.Setenv("PATH", launchdPATH)

	tr, _ := daemonTree(t, func(b *Budgets) { b.Handshake = 10 * time.Second })
	port := freePort(t)
	if err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "launchd-proxy", Kind: "daemon", Command: "launchd-proxy",
			Argv: []string{"--port", fmt.Sprint(port)}, EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	}); err != nil {
		t.Fatalf("a user-installed daemon must start under launchd's PATH: %v", err)
	}
	if st := daemonStatus(t, tr, "launchd-proxy"); st.State != UnitRunning || !st.HealthOK {
		t.Errorf("state=%s health=%v, want running+healthy", st.State, st.HealthOK)
	}
}
