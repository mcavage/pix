package main

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
		Label:   serveLaunchdLabel,
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

func TestRenderUnit(t *testing.T) {
	logPath := config.ServeLogPath()
	got, err := renderUnit(unitData{HostBin: "/usr/local/bin/pix-host", LogPath: logPath})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ExecStart=\"/usr/local/bin/pix-host\" serve") {
		t.Errorf("unit missing ExecStart:\n%s", got)
	}
	if !strings.Contains(got, "Restart=always") || !strings.Contains(got, "WantedBy=default.target") {
		t.Errorf("unit missing restart/install directives:\n%s", got)
	}
	// Logging goes to the SAME unified file, not journald.
	if !strings.Contains(got, "StandardOutput=append:"+logPath) || !strings.Contains(got, "StandardError=append:"+logPath) {
		t.Errorf("unit missing StandardOutput/StandardError=append:%s:\n%s", logPath, got)
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

// TestSystemdInstall: unit written and systemctl sequence issued.
func TestSystemdInstall(t *testing.T) {
	r := &recRunner{}
	f := newRecFS()
	var out bytes.Buffer
	if err := systemdInstall(r.run, f.fs(), "/home/u", "/usr/bin/pix-host", nil, &out); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join("/home/u", ".config", "systemd", "user", "pix-serve.service")
	if _, ok := f.written[unitPath]; !ok {
		t.Fatalf("unit not written to %s", unitPath)
	}
	joined := strings.Join(r.calls, "\n")
	for _, want := range []string{
		"systemctl --user --version",
		"systemctl --user daemon-reload",
		"systemctl --user enable --now pix-serve.service",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, r.calls)
		}
	}
	if !strings.Contains(out.String(), "logs: "+config.ServeLogPath()) {
		t.Errorf("install message must point at the unified serve log: %q", out.String())
	}
}

// Non-systemd distro degrades to the explicit message, writing nothing.
func TestSystemdInstallNoSystemd(t *testing.T) {
	r := &recRunner{fail: map[string]error{"systemctl": fmt.Errorf("exec: not found")}}
	f := newRecFS()
	err := systemdInstall(r.run, f.fs(), "/home/u", "/usr/bin/pix-host", nil, &bytes.Buffer{})
	if err != errNoSystemd {
		t.Fatalf("err = %v, want errNoSystemd", err)
	}
	if len(f.written) != 0 {
		t.Errorf("unit written despite missing systemd: %v", f.written)
	}
}

func TestSystemdUninstall(t *testing.T) {
	r := &recRunner{}
	f := newRecFS()
	var out bytes.Buffer
	if err := systemdUninstall(r.run, f.fs(), "/home/u", &out); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.calls, "\n")
	if !strings.Contains(joined, "systemctl --user disable --now pix-serve.service") ||
		!strings.Contains(joined, "systemctl --user daemon-reload") {
		t.Errorf("uninstall argv = %v", r.calls)
	}
	if len(f.removed) != 1 {
		t.Errorf("removed = %v", f.removed)
	}
}

func TestSystemdActive(t *testing.T) {
	r := &recRunner{out: map[string]string{"systemctl --user is-active": "active\n"}}
	if !systemdActive(r.run) {
		t.Error("active output should mean active")
	}
	r2 := &recRunner{out: map[string]string{"systemctl --user is-active": "inactive\n"}}
	if systemdActive(r2.run) {
		t.Error("inactive output should mean inactive")
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
		LogPath: `/logs/a&b"c'd/out.log`, Label: serveLaunchdLabel,
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
		Home:    "/home/u", LogPath: "/l/o", Label: serveLaunchdLabel,
	})
	if err == nil {
		t.Fatal("newline in HostBin accepted")
	}
}

// A HostBin with a space (and a quote) must stay ONE systemd argv element:
// quoted ExecStart, embedded quotes escaped.
func TestRenderUnitQuotesHostileExecStart(t *testing.T) {
	got, err := renderUnit(unitData{HostBin: `/Users/My Name/bin/pi"stack-host`})
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="/Users/My Name/bin/pi\"stack-host" serve`
	if !strings.Contains(got, want) {
		t.Errorf("unit ExecStart not safely quoted, want %q in:\n%s", want, got)
	}
}

func TestRenderUnitRejectsControlChars(t *testing.T) {
	if _, err := renderUnit(unitData{HostBin: "/bin/x\nExecStartPre=/bin/evil"}); err == nil {
		t.Fatal("newline in HostBin accepted (systemd directive injection)")
	}
	if _, err := renderUnit(unitData{HostBin: "/bin/x", Env: []envKV{{Key: "MEMORY_DB", Value: "a\nEnvironment=EVIL=1"}}}); err == nil {
		t.Fatal("newline in env value accepted")
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
		LogPath: "/l/o", Label: serveLaunchdLabel,
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

// …and in the rendered systemd unit.
func TestRenderUnitCarriesCapturedEnv(t *testing.T) {
	got, err := renderUnit(unitData{
		HostBin: "/usr/bin/pix-host",
		LogPath: "/l/serve.log",
		Env: []envKV{
			{Key: "PIX_CONFIG", Value: "/custom/config.toml"},
			{Key: "KNOWLEDGE_PORT", Value: "21436"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Environment="PIX_CONFIG=/custom/config.toml"`,
		`Environment="KNOWLEDGE_PORT=21436"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q:\n%s", want, got)
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
		10*time.Second, &out)
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
		10*time.Second, &out)
	if ok {
		t.Error("never-up reported healthy")
	}
	if !strings.Contains(out.String(), "did not answer within 10s") {
		t.Errorf("timeout warning missing/dishonest: %q", out.String())
	}

	// No enabled ports -> vacuously healthy, silent.
	out.Reset()
	if !reportManagedServeHealth(func(int) bool { return false }, nil,
		func() time.Time { return clock }, func(time.Duration) {}, time.Second, &out) {
		t.Error("no ports should be vacuously healthy")
	}
}

// Round 2 (H8): a config.Load() failure must be reported as a verification
// FAILURE, not silently skipped. The old code ran health verification only
// `if err == nil`, so a malformed config.toml printed "installed managed
// service" while the check silently no-oped and the unit crash-looped in the
// background with no honest signal.
func TestVerifyManagedInstallHealthConfigLoadFailure(t *testing.T) {
	var out bytes.Buffer
	st := serveStarter{dial: func(int) bool {
		t.Fatal("must not probe ports when config failed to load")
		return false
	}}
	ok := verifyManagedInstallHealth(nil, fmt.Errorf("malformed TOML at line 3"), st, &out)
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
	if !verifyManagedInstallHealth(cfg, nil, st, &out) {
		t.Errorf("expected healthy: %q", out.String())
	}
	if !strings.Contains(out.String(), "managed service is up") {
		t.Errorf("missing up message: %q", out.String())
	}
}

// --- round 2 (H8): systemd `%`/`$` expansion + plist XML-comment hazard ------

func TestSystemdQuoteEscapesPercentAndDollar(t *testing.T) {
	got := systemdQuote("/home/user%id/$HOME/pix-host")
	want := `"/home/user%%id/$$HOME/pix-host"`
	if got != want {
		t.Errorf("systemdQuote = %q, want %q", got, want)
	}
}

// A literal `%` in the rendered ExecStart must render `%%` — systemd expands
// unescaped `%` as a unit specifier, so a real path like /home/user%id would
// otherwise be silently mangled at daemon-reload time.
func TestRenderUnitEscapesPercentInHostBin(t *testing.T) {
	got, err := renderUnit(unitData{HostBin: "/home/user%id/pix-host"})
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="/home/user%%id/pix-host" serve`
	if !strings.Contains(got, want) {
		t.Errorf("unit ExecStart missing escaped %%, want %q in:\n%s", want, got)
	}
}

// A home directory containing `--` must still produce a VALID plist: XML
// text-escaping does not make `--` legal inside a `<!-- -->` comment, so the
// old template (which interpolated OutLog/ErrLog — both under Home — directly
// into its top comment) broke for exactly this home. The fix keeps every
// user-interpolated value out of comments entirely.
func TestRenderPlistHomeWithDoubleDashProducesValidXML(t *testing.T) {
	home := "/Users/alice--work"
	got, err := renderPlist(plistData{
		HostBin: "/opt/pix-host",
		Home:    home,
		LogPath: config.ServeLogPath(),
		Label:   serveLaunchdLabel,
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
