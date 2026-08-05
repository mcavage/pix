// serve_install.go implements the MANAGED LOGIN SERVICE (docs/design/
// serve-lifecycle.md §2): `pix serve install` / `serve uninstall` register
// `pix-host serve` as a launchd LaunchAgent — pix's host lifecycle is macOS
// only, the Docker Desktop model, opt-in beside the default lazy auto-start.
//
// This file is testable on any OS (the rendering, install/uninstall step
// sequences, and message formatting are pure functions over an injected
// command runner + fs ops), but only actually WIRED on darwin. The tiny
// real-exec dispatch lives in serve_install_darwin.go; every other GOOS gets
// the single ErrUnsupportedHost stub in serve_install_other.go.

package service

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/launcher"
)

// serveLaunchdLabel is the LaunchAgent label (and plist basename).
const serveLaunchdLabel = "com.pix.serve"

// LaunchdLabel is serveLaunchdLabel for the one caller outside this package:
// health's launchd probe, which has to name the label it asks launchctl
// about. Exported rather than copied, because a doctor that probes a label
// the installer does not write is worse than no probe at all.
const LaunchdLabel = serveLaunchdLabel

// The embedded template is the SINGLE SOURCE OF TRUTH for the generated
// plist (the old scripts/macos CHANGEME plist is superseded — go:embed
// cannot reach outside the module, so the template lives here and the script
// now delegates to `pix serve install`).
//
//go:embed templates/com.pix.serve.plist.tmpl
var plistTemplate string

// envKV is one install-time environment override rendered into the generated
// unit (H6). Order is preserved so the rendered files are deterministic.
type envKV struct {
	Key   string
	Value string
}

// capturedServeEnvVars is the documented allowlist of daemon-relevant env vars
// captured at `serve install` time (beyond the always-rendered
// PIX_CONFIG): they change which config/store/ports the daemon reads, so
// a launcher that runs with them set MUST install a daemon that sees the same
// values — otherwise config propagation "restarts" a daemon that never reads
// the launcher's config. Kept in sync with the man page's `serve install`
// section.
var capturedServeEnvVars = []string{
	"XDG_CONFIG_HOME",
	"MEMORY_DB",
	"MEMORY_PORT",
	"OLLAMA_HOST",
}

// capturedServeEnv resolves the env block rendered into the managed unit (H6):
// always an ABSOLUTE PIX_CONFIG pinned to the launcher's resolved
// config.Path() (so launcher and daemon can never read different configs),
// plus each capturedServeEnvVars entry that is set at install time.
func capturedServeEnv(getenv func(string) string) []envKV {
	cfgPath := config.Path()
	if abs, err := filepath.Abs(cfgPath); err == nil {
		cfgPath = abs
	}
	out := []envKV{{Key: "PIX_CONFIG", Value: cfgPath}}
	for _, k := range capturedServeEnvVars {
		if v := getenv(k); v != "" {
			out = append(out, envKV{Key: k, Value: v})
		}
	}
	return out
}

// plistData fills the launchd template.
type plistData struct {
	HostBin string  // absolute, symlink-resolved path to pix-host
	Home    string  // os.UserHomeDir()
	LogPath string  // config.ServeLogPath() — StandardOutPath AND StandardErrorPath both point here (one unified serve log across lazy + managed launchd)
	Label   string  // com.pix.serve
	Env     []envKV // install-time env overrides (H6)
}

// validateUnitValue rejects values no generated plist can carry safely:
// newlines/control chars would inject fresh plist elements no quoting can
// contain (H7). Loud error beats silent mangling.
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
// itself is config.ServeLogPath() — unified across lazy auto-start and the
// managed launchd form — so it no longer derives from $HOME here.
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
	fmt.Fprintln(out, "removed managed service. run `pix serve install` to re-enable, or `pix serve` for foreground.")
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
// returns at next login, or immediately via `pix serve install`. This is
// the only way to actually stop a KeepAlive agent — a bare SIGTERM to the pid is
// undone by launchd within a second.
func launchdStop(run cmdRunner, uid int, out io.Writer) error {
	if _, err := run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, serveLaunchdLabel)); err != nil {
		return err
	}
	fmt.Fprintln(out, "stopped the managed pix service (launchd). It stays installed and returns at next login; start it now with `pix serve install`, or remove it with `pix serve uninstall`.")
	return nil
}

// --- shared entry points ------------------------------------------------------

// resolvedHostBinary is launcher.FindHostBinary + EvalSymlinks: launchd needs the
// REAL absolute path (a ~/.local/bin symlink into a repo's out/ dir would break
// when the repo moves, and launchd has a minimal PATH).
func resolvedHostBinary() (string, error) {
	bin, err := launcher.FindHostBinary()
	if err != nil {
		return "", fmt.Errorf("pix-host not found — run `make install` first")
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
		return fmt.Errorf("a foreground `pix serve` is running — stop it (Ctrl-C in its terminal, or `pix serve stop`) and re-run `pix serve install`")
	case serveLazy:
		fmt.Fprintln(out, "stopping the background (lazy-started) pix services before installing the managed service…")
		stopped, err := stop(out)
		if err != nil {
			return fmt.Errorf("could not stop the background pix services: %v — stop them (`pix serve stop`) and re-run", err)
		}
		if !stopped {
			return fmt.Errorf("the background pix services were not stopped — stop them (`pix serve stop`) and re-run `pix serve install`")
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
		fmt.Fprintf(out, "warning: installed managed service, but could not verify it started: config.toml failed to load (%v). It will not start until this is fixed — edit config.toml, then check with `pix serve status`.\n", cfgErr)
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
			fmt.Fprintf(out, "warning: the managed service was installed but its services (%s) did not answer within %s — it may be failing to start; check the logs above and `pix serve status`.\n",
				describeServePorts(ports), timeout)
			return false
		}
		sleep(200 * time.Millisecond)
	}
}

// RunInstall is the `serve install` entry point (platform dispatch is in
// serve_install_{darwin,linux,other}.go).
func RunInstall(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(Usage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix serve install: unexpected argument %q\n\n%s", argv[0], Usage)
		os.Exit(2)
	}
	rl := DefaultReloader()
	if err := preInstallGuard(rl.mode, rl.Stop, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix serve install: %v\n", err)
		os.Exit(1)
	}
	if err := platformServeInstall(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix serve install: %v\n", err)
		os.Exit(1)
	}
	// Bounded post-install verification: report honestly if the unit did not
	// come up, or if we could not even check because config.toml won't load
	// (exit 0 either way — the install itself succeeded; the warning + pointers
	// are the honest signal).
	cfg, cfgErr := config.Load()
	verifyManagedInstallHealth(cfg, cfgErr, DefaultStarter(), os.Stdout)
}

// RunUninstall is the `serve uninstall` entry point.
func RunUninstall(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(Usage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix serve uninstall: unexpected argument %q\n\n%s", argv[0], Usage)
		os.Exit(2)
	}
	if err := platformServeUninstall(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix serve uninstall: %v\n", err)
		os.Exit(1)
	}
}

// realCmdRunner is the concrete exec shim (kept thin like DefaultCtl's
// syscalls; the argv sequences around it are what the tests prove).
func realCmdRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
