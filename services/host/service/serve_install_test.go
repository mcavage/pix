package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/launcher"
	"pix/host/rpc"
)

// recRunner records every command invocation and answers from a script.
type recRunner struct {
	calls []string
	fail  map[string]error  // command-prefix -> error
	out   map[string]string // command-prefix -> stdout
}

func (r *recRunner) run(name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, line)
	for prefix, err := range r.fail {
		if strings.HasPrefix(line, prefix) {
			return "", err
		}
	}
	for prefix, o := range r.out {
		if strings.HasPrefix(line, prefix) {
			return o, nil
		}
	}
	return "", nil
}

// recFS records writes/removes without touching the disk.
type recFS struct {
	written map[string]string
	perms   map[string]os.FileMode
	removed []string
}

func newRecFS() *recFS {
	return &recFS{written: map[string]string{}, perms: map[string]os.FileMode{}}
}

func (f *recFS) fs() installFS {
	return installFS{
		mkdirAll: func(string, os.FileMode) error { return nil },
		writeFile: func(path string, data []byte, perm os.FileMode) error {
			f.written[path] = string(data)
			f.perms[path] = perm
			return nil
		},
		remove: func(path string) error { f.removed = append(f.removed, path); return nil },
	}
}

// TestRenderPlist: the generated plist has no CHANGEME left and carries the
// real paths — golden-ish assertions on the rendered output, no launchctl.
func TestRenderPlist(t *testing.T) {
	logPath := config.ServeLogPath()
	got, err := renderPlist(plistData{
		HostBin: "/home/u/.local/bin/pix-host",
		Home:    "/home/u",
		LogPath: logPath,
		Label:   LaunchdLabel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "CHANGEME") || strings.Contains(got, "{{") {
		t.Errorf("unrendered placeholder in plist:\n%s", got)
	}
	for _, want := range []string{
		"<string>com.pix.serve</string>",
		"<string>/home/u/.local/bin/pix-host</string>",
		"<string>serve</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q:\n%s", want, got)
		}
	}
	// StandardOutPath AND StandardErrorPath must both be config.ServeLogPath()
	// — one unified serve log, no more ~/Library/Logs split files.
	if n := strings.Count(got, "<string>"+logPath+"</string>"); n != 2 {
		t.Errorf("want StandardOutPath and StandardErrorPath both = %q (2 occurrences), got %d in:\n%s", logPath, n, got)
	}
	if strings.Contains(got, "Library/Logs") {
		t.Errorf("plist still references ~/Library/Logs:\n%s", got)
	}
}

// TestLaunchdInstall: plist written to the right path (0644) and launchctl
// invoked in the right order: bootout (idempotence) -> bootstrap -> kickstart.
func TestLaunchdInstall(t *testing.T) {
	r := &recRunner{}
	f := newRecFS()
	var out bytes.Buffer
	if err := launchdInstall(r.run, f.fs(), 501, "/Users/u", "/opt/pix-host", nil, &out); err != nil {
		t.Fatal(err)
	}
	plistPath := "/Users/u/Library/LaunchAgents/com.pix.serve.plist"
	if _, ok := f.written[plistPath]; !ok {
		t.Fatalf("plist not written to %s (wrote %v)", plistPath, f.written)
	}
	if f.perms[plistPath] != 0o644 {
		t.Errorf("plist perms = %o, want 0644", f.perms[plistPath])
	}
	want := []string{
		"launchctl bootout gui/501/com.pix.serve",
		"launchctl bootstrap gui/501 " + plistPath,
		"launchctl kickstart -k gui/501/com.pix.serve",
	}
	if len(r.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
	for i := range want {
		if r.calls[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, r.calls[i], want[i])
		}
	}
	if !strings.Contains(out.String(), "installed managed service com.pix.serve") ||
		!strings.Contains(out.String(), "logs: "+config.ServeLogPath()) {
		t.Errorf("install message = %q", out.String())
	}
}

// bootstrap failing on an old macOS falls back to the deprecated load -w.
func TestLaunchdInstallFallsBackToLoad(t *testing.T) {
	r := &recRunner{fail: map[string]error{"launchctl bootstrap": fmt.Errorf("not supported")}}
	f := newRecFS()
	if err := launchdInstall(r.run, f.fs(), 501, "/Users/u", "/opt/pix-host", nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.calls, "\n")
	if !strings.Contains(joined, "launchctl load -w /Users/u/Library/LaunchAgents/com.pix.serve.plist") {
		t.Errorf("no load -w fallback in %v", r.calls)
	}
}

// Both bootstrap AND the fallback failing is a real error.
func TestLaunchdInstallBothFail(t *testing.T) {
	r := &recRunner{fail: map[string]error{
		"launchctl bootstrap": fmt.Errorf("nope"),
		"launchctl load":      fmt.Errorf("also nope"),
	}}
	if err := launchdInstall(r.run, newRecFS().fs(), 501, "/Users/u", "/opt/hb", nil, &bytes.Buffer{}); err == nil {
		t.Fatal("want error when bootstrap and load -w both fail")
	}
}

func TestLaunchdUninstall(t *testing.T) {
	r := &recRunner{fail: map[string]error{"launchctl bootout": fmt.Errorf("not loaded")}} // ignored
	f := newRecFS()
	var out bytes.Buffer
	if err := launchdUninstall(r.run, f.fs(), 501, "/Users/u", &out); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 1 || !strings.HasSuffix(f.removed[0], "com.pix.serve.plist") {
		t.Errorf("removed = %v", f.removed)
	}
	if !strings.Contains(out.String(), "removed managed service") {
		t.Errorf("uninstall message = %q", out.String())
	}
}

func TestLaunchdActiveAndRestart(t *testing.T) {
	r := &recRunner{}
	if !launchdActive(r.run, 501) {
		t.Error("print exit 0 should mean active")
	}
	r2 := &recRunner{fail: map[string]error{"launchctl print": fmt.Errorf("not found")}}
	if launchdActive(r2.run, 501) {
		t.Error("print failure should mean inactive")
	}
	r3 := &recRunner{}
	if err := launchdRestart(r3.run, 501); err != nil {
		t.Fatal(err)
	}
	if r3.calls[0] != "launchctl kickstart -k gui/501/com.pix.serve" {
		t.Errorf("restart argv = %v", r3.calls)
	}
}

// --- H7: template injection --------------------------------------------------

// A hostile HostBin/Home containing plist structure must be XML-escaped, not
// interpolated raw (text/template does not escape XML).
func TestRenderPlistEscapesXMLMetachars(t *testing.T) {
	evil := `/tmp/x</string><key>ProgramArguments</key>`
	got, err := renderPlist(plistData{
		HostBin: evil,
		Home:    "/home/u",
		LogPath: `/logs/a&b"c'd/out.log`, Label: LaunchdLabel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, evil) {
		t.Errorf("raw XML metachars survived into the plist:\n%s", got)
	}
	if !strings.Contains(got, "&lt;/string&gt;&lt;key&gt;") {
		t.Errorf("HostBin not XML-escaped:\n%s", got)
	}
	if !strings.Contains(got, "/logs/a&amp;b&#34;c&#39;d/out.log") {
		t.Errorf("LogPath not XML-escaped:\n%s", got)
	}
}

// Values that no escaping can carry (newlines/control chars) are refused loudly.
func TestRenderPlistRejectsControlChars(t *testing.T) {
	_, err := renderPlist(plistData{
		HostBin: "/bin/x\n<key>Injected</key>",
		Home:    "/home/u", LogPath: "/l/o", Label: LaunchdLabel,
	})
	if err == nil {
		t.Fatal("newline in HostBin accepted")
	}
}

// --- H6: install-time env capture ---------------------------------------------

func TestCapturedServeEnv(t *testing.T) {
	t.Setenv("PIX_CONFIG", "/custom/place/config.toml")
	fake := map[string]string{
		"MEMORY_PORT": "21435",
		"OLLAMA_HOST": "http://127.0.0.1:11434",
	}
	env := capturedServeEnv(func(k string) string { return fake[k] })
	if len(env) != 3 {
		t.Fatalf("env = %v, want PIX_CONFIG + 2 set vars", env)
	}
	if env[0].Key != "PIX_CONFIG" || env[0].Value != "/custom/place/config.toml" {
		t.Errorf("env[0] = %v, want the absolute PIX_CONFIG pin", env[0])
	}
	joined := fmt.Sprint(env)
	for _, want := range []string{"MEMORY_PORT 21435", "OLLAMA_HOST http://127.0.0.1:11434"} {
		if !strings.Contains(joined, want) {
			t.Errorf("captured env missing %q: %v", want, env)
		}
	}
	for _, absent := range []string{"MEMORY_DB", "KNOWLEDGE_PORT", "XDG_CONFIG_HOME"} {
		if strings.Contains(joined, absent) {
			t.Errorf("unset var %q captured: %v", absent, env)
		}
	}
}

// A custom PIX_CONFIG + port override lands in the rendered plist.
func TestRenderPlistCarriesCapturedEnv(t *testing.T) {
	got, err := renderPlist(plistData{
		HostBin: "/opt/pix-host", Home: "/Users/u",
		LogPath: "/l/o", Label: LaunchdLabel,
		Env: []envKV{
			{Key: "PIX_CONFIG", Value: "/custom/config.toml"},
			{Key: "MEMORY_PORT", Value: "21435"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>PIX_CONFIG</key>",
		"<string>/custom/config.toml</string>",
		"<key>MEMORY_PORT</key>",
		"<string>21435</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing env line %q:\n%s", want, got)
		}
	}
}

// --- H5: install over a running daemon ----------------------------------------

func TestPreInstallGuardForegroundRefuses(t *testing.T) {
	stopCalled := false
	err := preInstallGuard(
		func() serveMode { return serveForeground },
		func(io.Writer) (bool, error) { stopCalled = true; return true, nil },
		io.Discard)
	if err == nil || !strings.Contains(err.Error(), "foreground") {
		t.Fatalf("err = %v, want a foreground refusal with instructions", err)
	}
	if stopCalled {
		t.Error("guard tried to stop a FOREGROUND daemon (must never)")
	}
}

func TestPreInstallGuardLazyStopsFirst(t *testing.T) {
	stopCalled := false
	var out bytes.Buffer
	err := preInstallGuard(
		func() serveMode { return serveLazy },
		func(io.Writer) (bool, error) { stopCalled = true; return true, nil },
		&out)
	if err != nil {
		t.Fatalf("lazy guard: %v", err)
	}
	if !stopCalled {
		t.Error("lazy daemon not stopped before install")
	}
	if !strings.Contains(out.String(), "stopping the background") {
		t.Errorf("no legible stop message: %q", out.String())
	}
}

func TestPreInstallGuardLazyStopRefusedFails(t *testing.T) {
	err := preInstallGuard(
		func() serveMode { return serveLazy },
		func(io.Writer) (bool, error) { return false, nil },
		io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not stopped") {
		t.Fatalf("err = %v, want an honest not-stopped error", err)
	}
}

func TestPreInstallGuardManagedAndDownProceed(t *testing.T) {
	for _, mode := range []serveMode{serveManaged, serveDown} {
		stopCalled := false
		err := preInstallGuard(
			func() serveMode { return mode },
			func(io.Writer) (bool, error) { stopCalled = true; return true, nil },
			io.Discard)
		if err != nil {
			t.Errorf("mode %v: err = %v, want nil (proceed)", mode, err)
		}
		if stopCalled {
			t.Errorf("mode %v: stop invoked, want untouched", mode)
		}
	}
}

// currentIdentityProbe is the fake, non-nil test double every test below
// injects instead of nil (architect round 2: nil no longer skips the identity
// check, so a caller that only cares about TCP-liveness must inject a probe
// that trivially matches, not omit one).
func currentIdentityProbe(int) (rpc.ServiceIdentity, error) {
	return rpc.ServiceIdentity{Name: rpc.MemoryName, Version: launcher.Version, Ready: true}, nil
}

// Post-install verification: honest success and honest failure, bounded by an
// injected clock (no wall-time wait).
func TestReportManagedServeHealth(t *testing.T) {
	ports := []servePortSpec{{"memory", 11435}}

	// Comes up after a few polls -> true + "up" line.
	var out bytes.Buffer
	clock := time.Time{}
	dialsLeft := 3
	ok := reportManagedServeHealth(
		func(int) bool { dialsLeft--; return dialsLeft <= 0 },
		ports,
		func() time.Time { return clock },
		func(d time.Duration) { clock = clock.Add(d) },
		10*time.Second, currentIdentityProbe, &out)
	if !ok || !strings.Contains(out.String(), "managed service is up") {
		t.Errorf("healthy: ok=%v out=%q", ok, out.String())
	}

	// Never up -> false + honest warning naming the budget.
	out.Reset()
	clock = time.Time{}
	ok = reportManagedServeHealth(
		func(int) bool { return false },
		ports,
		func() time.Time { return clock },
		func(d time.Duration) { clock = clock.Add(d) },
		10*time.Second, currentIdentityProbe, &out)
	if ok {
		t.Error("never-up reported healthy")
	}
	if !strings.Contains(out.String(), "did not answer within 10s") {
		t.Errorf("timeout warning missing/dishonest: %q", out.String())
	}

	// No enabled ports -> vacuously healthy, silent.
	out.Reset()
	if !reportManagedServeHealth(func(int) bool { return false }, nil,
		func() time.Time { return clock }, func(time.Duration) {}, time.Second, currentIdentityProbe, &out) {
		t.Error("no ports should be vacuously healthy")
	}
}

// TestReportManagedServeHealth_IdentityVerifiedSuccess is the U3-lifecycle
// convergent case: TCP is up AND the injected probe answers as the CURRENT
// version — exactly one verified success line, nothing else.
func TestReportManagedServeHealth_IdentityVerifiedSuccess(t *testing.T) {
	ports := []servePortSpec{{"memory", 11435}}
	var out bytes.Buffer
	probeCalls := 0
	probe := func(int) (rpc.ServiceIdentity, error) {
		probeCalls++
		return rpc.ServiceIdentity{Name: rpc.MemoryName, Version: launcher.Version, Ready: true}, nil
	}
	ok := reportManagedServeHealth(func(int) bool { return true }, ports,
		func() time.Time { return time.Time{} }, func(time.Duration) {}, time.Second, probe, &out)
	if !ok {
		t.Fatalf("expected verified success, got failure: %q", out.String())
	}
	if probeCalls != 1 {
		t.Errorf("expected exactly one identity probe (at most one attempt), got %d", probeCalls)
	}
	if got := out.String(); !strings.Contains(got, "managed service is up") || strings.Contains(got, "warning") {
		t.Errorf("expected exactly one clean success line, got %q", got)
	}
}

// TestReportManagedServeHealth_IdentityNeverConverges_WarnsNeverUpdated is the
// U3-lifecycle non-convergent case: TCP mechanically comes up, but the daemon
// behind it answers as the OLD version forever. This must WARN with the
// actual/expected versions and the exact recovery command, and must NEVER
// print "updated" — that word is the literal bug this unit closes.
func TestReportManagedServeHealth_IdentityNeverConverges_WarnsNeverUpdated(t *testing.T) {
	ports := []servePortSpec{{"memory", 11435}}
	probe := func(int) (rpc.ServiceIdentity, error) {
		return rpc.ServiceIdentity{Name: rpc.MemoryName, Version: "0.1.7", Ready: true}, nil
	}
	// Calling it repeatedly must never converge and must still terminate at its
	// OWN deadline (identity verification is now folded into the health-wait, so
	// a mismatch keeps polling instead of failing on the first sample — this
	// proves it still bails out once the fake clock, advanced only by the
	// injected sleep, crosses the timeout; no wall-clock wait, no internal loop
	// that outlives the deadline).
	for i := 0; i < 5; i++ {
		var out bytes.Buffer
		clock := time.Time{}
		ok := reportManagedServeHealth(func(int) bool { return true }, ports,
			func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) },
			time.Second, probe, &out)
		if ok {
			t.Fatalf("call %d: identity mismatch reported healthy", i)
		}
		msg := out.String()
		if strings.Contains(msg, "updated") {
			t.Fatalf("call %d: must never print \"updated\" without a verified new version, got %q", i, msg)
		}
		if !strings.Contains(msg, "0.1.7") || !strings.Contains(msg, launcher.Version) {
			t.Fatalf("call %d: warning must name actual (0.1.7) and expected (%s) versions, got %q", i, launcher.Version, msg)
		}
		if !strings.Contains(msg, "pix serve stop && pix serve install") {
			t.Fatalf("call %d: warning must name the exact recovery command, got %q", i, msg)
		}
	}
}

// TestReportManagedServeHealth_OldThenNewDrain_ConvergesToSuccess is the
// old-then-new drain regression (architect round 2): a `launchctl kickstart
// -k` mid-restart can leave the OUTGOING process still answering, stale, for
// a moment while the incoming one is binding the same port. Folding identity
// verification into the health-wait means a mismatch on the first few polls
// must NOT short-circuit into a warning — it must keep polling and report the
// LATER, converged, correct-version answer as a clean success.
func TestReportManagedServeHealth_OldThenNewDrain_ConvergesToSuccess(t *testing.T) {
	ports := []servePortSpec{{"memory", 11435}}
	polls := 0
	probe := func(int) (rpc.ServiceIdentity, error) {
		polls++
		if polls <= 2 {
			// old process, still draining
			return rpc.ServiceIdentity{Name: rpc.MemoryName, Version: "0.1.7", Ready: true}, nil
		}
		// new process has taken the port
		return rpc.ServiceIdentity{Name: rpc.MemoryName, Version: launcher.Version, Ready: true}, nil
	}
	var out bytes.Buffer
	clock := time.Time{}
	ok := reportManagedServeHealth(func(int) bool { return true }, ports,
		func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) },
		10*time.Second, probe, &out)
	if !ok {
		t.Fatalf("drain sequence never reported healthy: %q", out.String())
	}
	msg := out.String()
	if strings.Contains(msg, "warning") || strings.Contains(msg, "0.1.7") {
		t.Errorf("a converged drain must print a clean success, no trace of the transient old version, got %q", msg)
	}
	if !strings.Contains(msg, "managed service is up") {
		t.Errorf("missing success line: %q", msg)
	}
	if polls < 3 {
		t.Errorf("expected the loop to poll past the stale answer, got %d poll(s)", polls)
	}
}

// TestReportManagedServeHealth_VersionCorrectButNotReady_WarnsWithReason is
// the version-correct-but-not-ready regression (architect round 2): the
// binary IS current, but the unit itself is not ready (still warming up, or
// genuinely degraded) at the deadline — never worded as "did not update",
// since the version is right.
func TestReportManagedServeHealth_VersionCorrectButNotReady_WarnsWithReason(t *testing.T) {
	ports := []servePortSpec{{"memory", 11435}}
	probe := func(int) (rpc.ServiceIdentity, error) {
		return rpc.ServiceIdentity{Name: rpc.MemoryName, Version: launcher.Version, Ready: false,
			DegradedReason: "unit backoff: plugin exited"}, nil
	}
	var out bytes.Buffer
	clock := time.Time{}
	ok := reportManagedServeHealth(func(int) bool { return true }, ports,
		func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) },
		time.Second, probe, &out)
	if ok {
		t.Fatal("a not-ready unit reported healthy")
	}
	msg := out.String()
	if strings.Contains(msg, "did not update") || strings.Contains(msg, "updated") {
		t.Errorf("a version-correct, not-ready unit must never be worded as a version mismatch, got %q", msg)
	}
	if !strings.Contains(msg, "not ready") || !strings.Contains(msg, "unit backoff: plugin exited") {
		t.Errorf("warning must carry the unit's own not-ready reason, got %q", msg)
	}
	if !strings.Contains(msg, "pix serve stop && pix serve install") {
		t.Errorf("warning must name the exact recovery command, got %q", msg)
	}
}

// TestReportManagedServeHealth_ProbeError_KeepsWaitingThenWarnsWithLastError
// proves a probe error (architect round 2, e.g. an unreadable identity
// payload mid-restart) is also a KEEP-WAITING signal, not an immediate
// failure — and that the eventual warning is the honest, LAST OBSERVED probe
// error rather than a generic port-timeout message that would misdirect the
// user (the port IS up; it is the identity call that never resolved).
func TestReportManagedServeHealth_ProbeError_KeepsWaitingThenWarnsWithLastError(t *testing.T) {
	ports := []servePortSpec{{"memory", 11435}}
	probeErr := fmt.Errorf("unreadable identity payload: unexpected EOF")
	calls := 0
	probe := func(int) (rpc.ServiceIdentity, error) {
		calls++
		return rpc.ServiceIdentity{}, probeErr
	}
	var out bytes.Buffer
	clock := time.Time{}
	ok := reportManagedServeHealth(func(int) bool { return true }, ports,
		func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) },
		time.Second, probe, &out)
	if ok {
		t.Fatal("a perpetually erroring probe reported healthy")
	}
	if calls < 2 {
		t.Errorf("expected the probe error to be retried across polls, got %d call(s)", calls)
	}
	msg := out.String()
	if !strings.Contains(msg, "unreadable identity payload") {
		t.Errorf("warning must carry the last observed probe error, got %q", msg)
	}
	if strings.Contains(msg, "did not answer within") {
		t.Errorf("a probe error must not be reworded as a generic port timeout (the port IS up), got %q", msg)
	}
}

// TestVerifyServeIdentity_ForeignNameNeverTouched: a port answering with a
// different service name is reported as a mismatch (so the warning fires and
// success is withheld) but never conflated with "our own daemon is stale".
func TestVerifyServeIdentity_ForeignNameNeverTouched(t *testing.T) {
	ports := []servePortSpec{{"memory", 11435}}
	probe := func(int) (rpc.ServiceIdentity, error) {
		return rpc.ServiceIdentity{Name: "someone-else", Version: "9.9.9"}, nil
	}
	mismatches := verifyServeIdentity(probe, ports, launcher.Version)
	if len(mismatches) != 1 {
		t.Fatalf("expected one mismatch, got %v", mismatches)
	}
	if !strings.Contains(mismatches[0].String(), "someone-else") {
		t.Errorf("mismatch message should name the foreign service, got %q", mismatches[0])
	}
}

// TestNilProberSeamRemovedFromInstallGo is the sentinel for the removed
// nil-prober weakening seam (architect round 2): verifyServeIdentity used to
// treat a nil probe as "skip the identity check entirely" — a
// production-reachable way to silently disable the one thing this file exists
// to do. There is no such skip any more: the one production call
// (RunInstall) always passes rpc.IdentityProbe, and every test injects a fake
// probe (a test double), never nil. Mirrors the grep-based sentinel pattern
// in serve_upgrade_test.go for the deleted read-side restart machinery.
func TestNilProberSeamRemovedFromInstallGo(t *testing.T) {
	b, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatalf("read install.go: %v", err)
	}
	if strings.Contains(string(b), "if probe == nil") {
		t.Error("install.go must never again skip identity verification for a nil probe — inject a fake probe test double instead")
	}
}

// Round 2 (H8): a config.Load() failure must be reported as a verification
// FAILURE, not silently skipped. The old code ran health verification only
// `if err == nil`, so a malformed config.toml printed "installed managed
func TestVerifyManagedInstallHealthConfigLoadFailure(t *testing.T) {
	var out bytes.Buffer
	st := serveStarter{dial: func(int) bool {
		t.Fatal("must not probe ports when config failed to load")
		return false
	}}
	probeCalled := false
	probe := func(int) (rpc.ServiceIdentity, error) {
		probeCalled = true
		return currentIdentityProbe(0)
	}
	ok := verifyManagedInstallHealth(nil, fmt.Errorf("malformed TOML at line 3"), st, probe, &out)
	if probeCalled {
		t.Error("must not probe identity when config failed to load")
	}
	if ok {
		t.Error("config-load failure reported healthy")
	}
	msg := out.String()
	if !strings.Contains(msg, "malformed TOML at line 3") {
		t.Errorf("underlying config error not surfaced: %q", msg)
	}
	if !strings.Contains(msg, "will not start") {
		t.Errorf("verification failure not reported honestly: %q", msg)
	}
}

// A successfully loaded config still runs the real health probe.
func TestVerifyManagedInstallHealthConfigLoadSuccess(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.Config{Services: []string{"memory"}}
	st := serveStarter{dial: func(int) bool { return true }, getenv: func(string) string { return "" }}
	if !verifyManagedInstallHealth(cfg, nil, st, currentIdentityProbe, &out) {
		t.Errorf("expected healthy: %q", out.String())
	}
	if !strings.Contains(out.String(), "managed service is up") {
		t.Errorf("missing up message: %q", out.String())
	}
}

// --- round 2 (H8): plist XML-comment hazard ----------------------------------

// A home directory containing `--` must still produce a VALID plist: XML
// text-escaping does not make `--` legal inside a `<!-- -->` comment, so the
// old template (which interpolated OutLog/ErrLog — both under Home — directly
func TestRenderPlistHomeWithDoubleDashProducesValidXML(t *testing.T) {
	home := "/Users/alice--work"
	got, err := renderPlist(plistData{
		HostBin: "/opt/pix-host",
		Home:    home,
		LogPath: config.ServeLogPath(),
		Label:   LaunchdLabel,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?s)<!--.*?-->`).FindAllString(got, -1) {
		body := strings.TrimSuffix(strings.TrimPrefix(m, "<!--"), "-->")
		if strings.Contains(body, "--") {
			t.Errorf("comment contains a literal `--`, which is invalid XML: %s", m)
		}
	}
	dec := xml.NewDecoder(strings.NewReader(got))
	for {
		if _, err := dec.Token(); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, got)
		}
	}
}

// TestSupervisedPathIsStableAcrossAnUpgrade is the regression for a permanently
// broken LaunchAgent.
//
// resolvedHostBinary used to EvalSymlinks. Homebrew's /opt/homebrew/bin/pix-host
// points into a VERSIONED Cellar directory, so the plist got
// …/Cellar/pix/<version>/… baked in, and the next `brew upgrade` deleted that
// directory. launchd then keeps a job it can never spawn (`last exit code = 78:
// EX_CONFIG`, parked in `spawn scheduled`) — and `launchctl kickstart -k`, which
// pix runs after every pack change, blocks forever on it. That was three
// separate "pix setup hangs" on a real host before anyone read the plist.
//
// The fixture is the exact Homebrew shape: a stable symlink into a versioned
// directory. What a supervisor is handed must be the symlink.
func TestSupervisedPathIsStableAcrossAnUpgrade(t *testing.T) {
	dir := t.TempDir()
	versioned := filepath.Join(dir, "Cellar", "pix", "0.1.44", "bin")
	if err := os.MkdirAll(versioned, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(versioned, "pix-host")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stableDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stableDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(stableDir, "pix-host")
	if err := os.Symlink(target, stable); err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(stable); err != nil || resolved == stable {
		t.Fatalf("fixture is wrong: %q did not resolve elsewhere (%v)", stable, err)
	}

	// The transform resolvedHostBinary applies to whatever FindHostBinary
	// returned: absolute, and NOTHING that follows the symlink.
	got := stable
	if abs, err := filepath.Abs(got); err == nil {
		got = abs
	}
	if got != stable {
		t.Errorf("supervised path = %q, want the stable %q", got, stable)
	}
	if strings.Contains(got, "Cellar") {
		t.Errorf("supervised path %q names a versioned directory the package manager deletes on upgrade", got)
	}
}
