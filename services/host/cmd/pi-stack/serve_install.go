// serve_install.go implements the MANAGED LOGIN SERVICE (docs/design/
// serve-lifecycle.md §2): `pi-stack serve install` / `serve uninstall` register
// `pi-stack-host serve` as a launchd LaunchAgent (macOS) or a systemd --user
// unit (Linux), so the services start at login and auto-restart — the Docker
// Desktop model, opt-in beside the default lazy auto-start.
//
// This file is CROSS-PLATFORM: the rendering, install/uninstall step sequences,
// and message formatting are all pure functions over an injected command runner
// + fs ops, unit-tested on any OS. Only the tiny real-exec dispatch lives in
// the build-tagged serve_install_{darwin,linux,other}.go files.

package main

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"pi-stack/host/config"
)

// serveLaunchdLabel is the LaunchAgent label (and plist basename).
const serveLaunchdLabel = "com.pi-stack.serve"

// serveSystemdUnit is the systemd --user unit name.
const serveSystemdUnit = "pi-stack-serve.service"

// The embedded templates are the SINGLE SOURCE OF TRUTH for the generated
// unit files (the old scripts/macos CHANGEME plist is superseded — go:embed
// cannot reach outside the module, so the template lives here and the script
// now delegates to `pi-stack serve install`).
//
//go:embed templates/com.pi-stack.serve.plist.tmpl
var plistTemplate string

//go:embed templates/pi-stack-serve.service.tmpl
var unitTemplate string

// envKV is one install-time environment override rendered into the generated
// unit (H6). Order is preserved so the rendered files are deterministic.
type envKV struct {
	Key   string
	Value string
}

// capturedServeEnvVars is the documented allowlist of daemon-relevant env vars
// captured at `serve install` time (beyond the always-rendered
// PI_STACK_CONFIG): they change which config/store/ports the daemon reads, so
// a launcher that runs with them set MUST install a daemon that sees the same
// values — otherwise config propagation "restarts" a daemon that never reads
// the launcher's config. Kept in sync with the man page's `serve install`
// section.
var capturedServeEnvVars = []string{
	"XDG_CONFIG_HOME",
	"MEMORY_DB",
	"MEMORY_PORT",
	"KNOWLEDGE_PORT",
	"OLLAMA_HOST",
}

// capturedServeEnv resolves the env block rendered into the managed unit (H6):
// always an ABSOLUTE PI_STACK_CONFIG pinned to the launcher's resolved
// config.Path() (so launcher and daemon can never read different configs),
// plus each capturedServeEnvVars entry that is set at install time.
func capturedServeEnv(getenv func(string) string) []envKV {
	cfgPath := config.Path()
	if abs, err := filepath.Abs(cfgPath); err == nil {
		cfgPath = abs
	}
	out := []envKV{{Key: "PI_STACK_CONFIG", Value: cfgPath}}
	for _, k := range capturedServeEnvVars {
		if v := getenv(k); v != "" {
			out = append(out, envKV{Key: k, Value: v})
		}
	}
	return out
}

// plistData fills the launchd template.
type plistData struct {
	HostBin string  // absolute, symlink-resolved path to pi-stack-host
	Home    string  // os.UserHomeDir()
	LogPath string  // config.ServeLogPath() — StandardOutPath AND StandardErrorPath both point here (one unified serve log across lazy + managed launchd/systemd)
	Label   string  // com.pi-stack.serve
	Env     []envKV // install-time env overrides (H6)
}

// unitData fills the systemd template.
type unitData struct {
	HostBin string
	LogPath string  // config.ServeLogPath() — StandardOutput/StandardError append: target (same unified serve log)
	Env     []envKV // install-time env overrides (H6)
}

// validateUnitValue rejects values no generated unit can carry safely:
// newlines/control chars would inject fresh plist elements or systemd
// directives no quoting can contain (H7). Loud error beats silent mangling.
func validateUnitValue(what, v string) error {
	for _, r := range v {
		if r == '\n' || r == '\r' || (r < 0x20 && r != '\t') {
			return fmt.Errorf("%s contains a control character and cannot be rendered into a service unit: %q", what, v)
		}
	}
	return nil
}

// xmlEscape escapes a value for use inside a plist <string> element (H7):
// text/template does NOT XML-escape, so a `</string>` embedded in $HOME or the
// binary path would otherwise inject plist structure launchd happily parses.
func xmlEscape(v string) string {
	var buf bytes.Buffer
	// xml.EscapeText never fails on a bytes.Buffer.
	_ = xml.EscapeText(&buf, []byte(v))
	return buf.String()
}

// renderPlist renders the launchd plist from the embedded template, validating
// then XML-escaping every interpolated value (H7).
func renderPlist(d plistData) (string, error) {
	for _, f := range []struct{ what, v string }{
		{"host binary path", d.HostBin}, {"home", d.Home},
		{"log path", d.LogPath}, {"label", d.Label},
	} {
		if err := validateUnitValue(f.what, f.v); err != nil {
			return "", err
		}
	}
	esc := plistData{
		HostBin: xmlEscape(d.HostBin),
		Home:    xmlEscape(d.Home),
		LogPath: xmlEscape(d.LogPath),
		Label:   xmlEscape(d.Label),
	}
	for _, kv := range d.Env {
		if err := validateUnitValue("env "+kv.Key, kv.Value); err != nil {
			return "", err
		}
		esc.Env = append(esc.Env, envKV{Key: xmlEscape(kv.Key), Value: xmlEscape(kv.Value)})
	}
	return renderTemplate("plist", plistTemplate, esc)
}

// systemdQuote renders a value as one systemd-quoted string: double-quoted with
// backslash and double-quote escaped, so an ExecStart binary path containing
// spaces stays ONE argv element instead of splitting (H7). Round 2 (H8) also
// escapes the two characters systemd itself expands AFTER unit parsing: `%`
// (specifier expansion — a literal path like /home/user%id must render `%%`)
// and `$` (variable expansion in ExecStart=/Environment=, so a literal `$` must
// render `$$` or it could be read as the start of a reference). Neither
// introduces a character the OTHER escape steps below would need to re-quote.
func systemdQuote(v string) string {
	v = strings.ReplaceAll(v, "%", "%%")
	v = strings.ReplaceAll(v, "$", "$$")
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
}

// systemdEscapePercent escapes `%` for a value used OUTSIDE ExecStart/
// Environment (StandardOutput=append:<path> is not quoted/word-split the way
// ExecStart is, so systemdQuote's quote-wrapping does not apply — but `%` is
// still expanded as a unit specifier wherever it appears in a unit value).
func systemdEscapePercent(v string) string {
	return strings.ReplaceAll(v, "%", "%%")
}

// renderUnit renders the systemd unit from the embedded template, validating
// values and quoting the ExecStart path + Environment entries (H7).
func renderUnit(d unitData) (string, error) {
	if err := validateUnitValue("host binary path", d.HostBin); err != nil {
		return "", err
	}
	if err := validateUnitValue("log path", d.LogPath); err != nil {
		return "", err
	}
	esc := unitData{HostBin: systemdQuote(d.HostBin), LogPath: systemdEscapePercent(d.LogPath)}
	for _, kv := range d.Env {
		if err := validateUnitValue("env "+kv.Key, kv.Value); err != nil {
			return "", err
		}
		// One quoted KEY=value token per systemd Environment= assignment.
		esc.Env = append(esc.Env, envKV{Key: kv.Key, Value: systemdQuote(kv.Key + "=" + kv.Value)})
	}
	return renderTemplate("unit", unitTemplate, esc)
}

func renderTemplate(name, tmpl string, data any) (string, error) {
	t, err := template.New(name).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// cmdRunner runs one external command and returns its combined output. It is
// the injected exec seam so install/uninstall argv sequences are unit-testable.
type cmdRunner func(name string, args ...string) (string, error)

// installFS bundles the injected filesystem ops the installers need.
type installFS struct {
	mkdirAll  func(path string, perm os.FileMode) error
	writeFile func(path string, data []byte, perm os.FileMode) error
	remove    func(path string) error
}

func realInstallFS() installFS {
	return installFS{mkdirAll: os.MkdirAll, writeFile: os.WriteFile, remove: os.Remove}
}

// --- launchd (macOS) ---------------------------------------------------------

// launchdPaths derives the launchd plist path from $HOME. The serve log
// itself is config.ServeLogPath() — unified across lazy auto-start and both
// managed forms (launchd, systemd) — so it no longer derives from $HOME here.
func launchdPaths(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", serveLaunchdLabel+".plist")
}

// launchdInstall renders + writes the plist and bootstraps it. Steps (all argv
// choices unit-tested via the injected runner):
//  1. bootout any loaded copy first (idempotent re-install; "not loaded" ignored)
//  2. launchctl bootstrap gui/<uid> <plist> (modern surface), falling back to
//     the deprecated `load -w` on old macOS
//  3. kickstart -k to start it NOW (RunAtLoad covers reboots)
func launchdInstall(run cmdRunner, fs installFS, uid int, home, hostBin string, env []envKV, out io.Writer) error {
	plistPath := launchdPaths(home)
	logPath := config.ServeLogPath()
	rendered, err := renderPlist(plistData{
		HostBin: hostBin, Home: home, LogPath: logPath, Label: serveLaunchdLabel, Env: env,
	})
	if err != nil {
		return err
	}
	if err := fs.mkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}
	if err := fs.mkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	if err := fs.writeFile(plistPath, []byte(rendered), 0o644); err != nil {
		return err
	}
	target := fmt.Sprintf("gui/%d/%s", uid, serveLaunchdLabel)
	domain := fmt.Sprintf("gui/%d", uid)
	// Idempotent re-install: unload any loaded copy; "not loaded" errors ignored.
	_, _ = run("launchctl", "bootout", target)
	if _, err := run("launchctl", "bootstrap", domain, plistPath); err != nil {
		// Old-macOS fallback.
		if _, lerr := run("launchctl", "load", "-w", plistPath); lerr != nil {
			return fmt.Errorf("launchctl bootstrap failed (%v) and load -w fallback failed (%v)", err, lerr)
		}
	}
	// Start it now; RunAtLoad usually already did, so a kickstart failure is not fatal.
	_, _ = run("launchctl", "kickstart", "-k", target)
	fmt.Fprintf(out, "installed managed service %s (starts at login, auto-restarts). logs: %s\n",
		serveLaunchdLabel, logPath)
	return nil
}

// launchdUninstall bootouts the agent (ignoring not-loaded) and removes the plist.
func launchdUninstall(run cmdRunner, fs installFS, uid int, home string, out io.Writer) error {
	plistPath := launchdPaths(home)
	_, _ = run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, serveLaunchdLabel))
	if err := fs.remove(plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(out, "removed managed service. run `pi-stack serve install` to re-enable, or `pi-stack serve` for foreground.")
	return nil
}

// launchdActive reports whether the LaunchAgent is loaded (launchctl print
// exits 0 for a loaded service).
func launchdActive(run cmdRunner, uid int) bool {
	_, err := run("launchctl", "print", fmt.Sprintf("gui/%d/%s", uid, serveLaunchdLabel))
	return err == nil
}

// launchdRestart kickstarts the managed unit in place (`-k` kills + restarts
// while keeping RunAtLoad/KeepAlive — the "reload config" primitive).
func launchdRestart(run cmdRunner, uid int) error {
	_, err := run("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, serveLaunchdLabel))
	return err
}

// launchdStop boots the agent OUT of its domain so KeepAlive stops respawning it,
// WITHOUT removing the plist (unlike launchdUninstall). It stays installed and
// returns at next login, or immediately via `pi-stack serve install`. This is
// the only way to actually stop a KeepAlive agent — a bare SIGTERM to the pid is
// undone by launchd within a second.
func launchdStop(run cmdRunner, uid int, out io.Writer) error {
	if _, err := run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, serveLaunchdLabel)); err != nil {
		return err
	}
	fmt.Fprintln(out, "stopped the managed pi-stack service (launchd). It stays installed and returns at next login; start it now with `pi-stack serve install`, or remove it with `pi-stack serve uninstall`.")
	return nil
}

// --- systemd --user (Linux) --------------------------------------------------

// systemdUnitPath is ~/.config/systemd/user/pi-stack-serve.service.
func systemdUnitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", serveSystemdUnit)
}

// errNoSystemd is the clean degrade on non-systemd distros.
var errNoSystemd = fmt.Errorf("no systemd --user found; use lazy auto-start (default) or run `pi-stack serve` yourself")

// systemdInstall writes the unit and enables it now.
func systemdInstall(run cmdRunner, fs installFS, home, hostBin string, env []envKV, out io.Writer) error {
	if _, err := run("systemctl", "--user", "--version"); err != nil {
		return errNoSystemd
	}
	logPath := config.ServeLogPath()
	rendered, err := renderUnit(unitData{HostBin: hostBin, LogPath: logPath, Env: env})
	if err != nil {
		return err
	}
	unitPath := systemdUnitPath(home)
	if err := fs.mkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	if err := fs.mkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	if err := fs.writeFile(unitPath, []byte(rendered), 0o644); err != nil {
		return err
	}
	if _, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := run("systemctl", "--user", "enable", "--now", serveSystemdUnit); err != nil {
		return err
	}
	fmt.Fprintf(out, "installed managed service %s (starts at login, auto-restarts). logs: %s\n", serveSystemdUnit, logPath)
	return nil
}

// systemdUninstall disables the unit and removes it.
func systemdUninstall(run cmdRunner, fs installFS, home string, out io.Writer) error {
	_, _ = run("systemctl", "--user", "disable", "--now", serveSystemdUnit)
	if err := fs.remove(systemdUnitPath(home)); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = run("systemctl", "--user", "daemon-reload")
	fmt.Fprintln(out, "removed managed service. run `pi-stack serve install` to re-enable, or `pi-stack serve` for foreground.")
	return nil
}

// systemdActive reports whether the unit is active.
// systemdStop stops the running unit WITHOUT disabling it (unlike
// systemdUninstall), so it stays enabled and returns at next login; a bare
// SIGTERM to the pid is otherwise undone by Restart=. Re-run now with
// `systemctl --user start` or `pi-stack serve install`.
func systemdStop(run cmdRunner, out io.Writer) error {
	if _, err := run("systemctl", "--user", "stop", serveSystemdUnit); err != nil {
		return err
	}
	fmt.Fprintf(out, "stopped the managed pi-stack service (%s). It stays installed and returns at next login; start it now with `pi-stack serve install`, or remove it with `pi-stack serve uninstall`.\n", serveSystemdUnit)
	return nil
}

func systemdActive(run cmdRunner) bool {
	got, err := run("systemctl", "--user", "is-active", serveSystemdUnit)
	return err == nil && strings.TrimSpace(got) == "active"
}

// systemdRestart restarts the unit.
func systemdRestart(run cmdRunner) error {
	_, err := run("systemctl", "--user", "restart", serveSystemdUnit)
	return err
}

// --- shared entry points ------------------------------------------------------

// resolvedHostBinary is findHostBinary + EvalSymlinks: launchd/systemd need the
// REAL absolute path (a ~/.local/bin symlink into a repo's out/ dir would break
// when the repo moves, and launchd has a minimal PATH).
func resolvedHostBinary() (string, error) {
	bin, err := findHostBinary()
	if err != nil {
		return "", fmt.Errorf("pi-stack-host not found — run `make install` first")
	}
	if resolved, rerr := filepath.EvalSymlinks(bin); rerr == nil {
		bin = resolved
	}
	if abs, aerr := filepath.Abs(bin); aerr == nil {
		bin = abs
	}
	return bin, nil
}

// preInstallGuard clears the ground before a managed-service install (H5): a
// KeepAlive/Restart=always unit installed over an already-running daemon
// collides on ports + store lock and crash-loops while install reports
// success. A VERIFIED lazy daemon is stopped (it is ours to manage); a
// FOREGROUND daemon is REFUSED with instructions (never kill a process the
// user is watching); managed (idempotent re-install) and down proceed.
func preInstallGuard(mode func() serveMode, stop func(io.Writer) (bool, error), out io.Writer) error {
	switch mode() {
	case serveForeground:
		return fmt.Errorf("a foreground `pi-stack serve` is running — stop it (Ctrl-C in its terminal, or `pi-stack serve stop`) and re-run `pi-stack serve install`")
	case serveLazy:
		fmt.Fprintln(out, "stopping the background (lazy-started) pi-stack services before installing the managed service…")
		stopped, err := stop(out)
		if err != nil {
			return fmt.Errorf("could not stop the background pi-stack services: %v — stop them (`pi-stack serve stop`) and re-run", err)
		}
		if !stopped {
			return fmt.Errorf("the background pi-stack services were not stopped — stop them (`pi-stack serve stop`) and re-run `pi-stack serve install`")
		}
	}
	return nil
}

// managedHealthTimeout bounds the post-install health verification.
const managedHealthTimeout = 10 * time.Second

// verifyManagedInstallHealth is the post-install verification step (round 2,
// H8): a config.Load() failure used to be silently swallowed (`if err == nil`
// guarded the whole check, and the else branch did nothing) so a malformed
// config.toml printed "installed managed service" while health verification
// was skipped entirely and the unit crash-loops in the background. A load
// failure is now itself a verification FAILURE, reported honestly — the
// install step already succeeded (the unit is on disk and enabled), but it
// will not start until config.toml is fixed, and we say so instead of
// claiming health we never checked. Returns whether health was verified.
func verifyManagedInstallHealth(cfg *config.Config, cfgErr error, st serveStarter, out io.Writer) bool {
	if cfgErr != nil {
		fmt.Fprintf(out, "warning: installed managed service, but could not verify it started: config.toml failed to load (%v). It will not start until this is fixed — edit config.toml, then check with `pi-stack serve status`.\n", cfgErr)
		return false
	}
	return reportManagedServeHealth(st.dial, requiredServePorts(st, cfg, nil),
		time.Now, time.Sleep, managedHealthTimeout, out)
}

// reportManagedServeHealth verifies (bounded) that the freshly-installed
// managed service actually came up — its required service ports answer — and
// reports HONESTLY when it did not (H5: "installed" must not paper over a
// crash-looping unit). Returns whether the services became healthy.
func reportManagedServeHealth(dial func(int) bool, ports []servePortSpec,
	now func() time.Time, sleep func(time.Duration), timeout time.Duration, out io.Writer) bool {
	if len(ports) == 0 {
		return true // nothing enabled to probe
	}
	deadline := now().Add(timeout)
	for {
		up := true
		for _, p := range ports {
			if !dial(p.port) {
				up = false
				break
			}
		}
		if up {
			fmt.Fprintf(out, "managed service is up (%s)\n", describeServePorts(ports))
			return true
		}
		if !now().Before(deadline) {
			fmt.Fprintf(out, "warning: the managed service was installed but its services (%s) did not answer within %s — it may be failing to start; check the logs above and `pi-stack serve status`.\n",
				describeServePorts(ports), timeout)
			return false
		}
		sleep(200 * time.Millisecond)
	}
}

// runServeInstall is the `serve install` entry point (platform dispatch is in
// serve_install_{darwin,linux,other}.go).
func runServeInstall(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(serveUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pi-stack serve install: unexpected argument %q\n\n%s", argv[0], serveUsage)
		os.Exit(2)
	}
	rl := defaultServeReloader()
	if err := preInstallGuard(rl.mode, rl.stopServe, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack serve install: %v\n", err)
		os.Exit(1)
	}
	if err := platformServeInstall(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack serve install: %v\n", err)
		os.Exit(1)
	}
	// Bounded post-install verification: report honestly if the unit did not
	// come up, or if we could not even check because config.toml won't load
	// (exit 0 either way — the install itself succeeded; the warning + pointers
	// are the honest signal).
	cfg, cfgErr := config.Load()
	verifyManagedInstallHealth(cfg, cfgErr, defaultServeStarter(), os.Stdout)
}

// runServeUninstall is the `serve uninstall` entry point.
func runServeUninstall(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(serveUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pi-stack serve uninstall: unexpected argument %q\n\n%s", argv[0], serveUsage)
		os.Exit(2)
	}
	if err := platformServeUninstall(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack serve uninstall: %v\n", err)
		os.Exit(1)
	}
}

// realCmdRunner is the concrete exec shim (kept thin like defaultServeCtl's
// syscalls; the argv sequences around it are what the tests prove).
func realCmdRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
