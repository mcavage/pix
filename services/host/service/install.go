// install.go implements the MANAGED LOGIN SERVICE (docs/design/
// serve-lifecycle.md §2): `pix serve install` / `serve uninstall` register
// `pix-host serve` as a launchd LaunchAgent (macOS only — see install_other.go).
// Every argv choice lives here, unit-tested through an injected runner; the
// platform files bind only the real runner, uid and $HOME.

package service

import (
	"bytes"
	"context"
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

	"pix/host/cli"
	"pix/host/config"
	"pix/host/launcher"
)

// LaunchdLabel is the LaunchAgent label (and plist basename). Exported rather
// than copied for health's launchd probe: probing a label the installer never
// writes is worse than not probing at all.
const LaunchdLabel = "com.pix.serve"

// The embedded template is the SINGLE SOURCE OF TRUTH for the generated plist.
//
//go:embed templates/com.pix.serve.plist.tmpl
var plistTemplate string

// envKV is one install-time environment override rendered into the generated
// unit (H6), in a deterministic order.
type envKV struct {
	Key   string
	Value string
}

// capturedServeEnvVars is the allowlist of daemon-relevant env vars captured at
// `serve install` time (beyond the always-rendered PIX_CONFIG): they decide which
// config/store/ports the daemon reads, and launchd starts it with a bare env.
var capturedServeEnvVars = []string{
	"XDG_CONFIG_HOME",
	"MEMORY_DB",
	"MEMORY_PORT",
	"OLLAMA_HOST",
}

// capturedServeEnv resolves the env block rendered into the managed unit (H6).
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

// validateUnitValue rejects values no plist can carry safely: newlines/control
// chars inject fresh plist elements no quoting can contain (H7).
func validateUnitValue(what, v string) error {
	for _, r := range v {
		if r == '\n' || r == '\r' || (r < 0x20 && r != '\t') {
			return fmt.Errorf("%s contains a control character and cannot be rendered into a service unit: %q", what, v)
		}
	}
	return nil
}

// xmlEscape escapes a value for a plist <string> (H7): text/template does NOT
// XML-escape, so a `</string>` in $HOME or the binary path would otherwise inject
// plist structure launchd happily parses.
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
	t, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, esc); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// cmdRunner is the injected exec seam that makes every install/uninstall argv
// sequence unit-testable.
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

// launchdPaths derives the launchd plist path from $HOME. The serve log is
// config.ServeLogPath(), one unified log across lazy and managed starts.
func launchdPaths(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

// launchdInstall renders + writes the plist and bootstraps it.
func launchdInstall(run cmdRunner, fs installFS, uid int, home, hostBin string, env []envKV, out io.Writer) error {
	plistPath := launchdPaths(home)
	logPath := config.ServeLogPath()
	rendered, err := renderPlist(plistData{
		HostBin: hostBin, Home: home, LogPath: logPath, Label: LaunchdLabel, Env: env,
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
	target := fmt.Sprintf("gui/%d/%s", uid, LaunchdLabel)
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
		LaunchdLabel, logPath)
	return nil
}

// launchdUninstall bootouts the agent (ignoring not-loaded) and removes the plist.
func launchdUninstall(run cmdRunner, fs installFS, uid int, home string, out io.Writer) error {
	plistPath := launchdPaths(home)
	_, _ = run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, LaunchdLabel))
	if err := fs.remove(plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(out, "removed managed service. run `pix serve install` to re-enable, or `pix serve` for foreground.")
	return nil
}

// launchdActive: launchctl print exits 0 for a loaded service.
func launchdActive(run cmdRunner, uid int) bool {
	_, err := run("launchctl", "print", fmt.Sprintf("gui/%d/%s", uid, LaunchdLabel))
	return err == nil
}

// launchdRestart kickstarts the managed unit in place: `-k` kills + restarts
// while keeping RunAtLoad/KeepAlive, the "reload config" primitive.
func launchdRestart(run cmdRunner, uid int) error {
	_, err := run("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, LaunchdLabel))
	return err
}

// launchdStop boots the agent OUT of its domain so KeepAlive stops respawning it,
// WITHOUT removing the plist (unlike launchdUninstall): a bare SIGTERM would be
// respawned instantly, the classic "I stopped it but it came right back" bug.
// It stays installed and returns at next login.
func launchdStop(run cmdRunner, uid int, out io.Writer) error {
	if _, err := run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, LaunchdLabel)); err != nil {
		return err
	}
	fmt.Fprintln(out, "stopped the managed pix service (launchd). It stays installed and returns at next login; start it now with `pix serve install`, or remove it with `pix serve uninstall`.")
	return nil
}

// --- shared entry points ------------------------------------------------------

// resolvedHostBinary is launcher.FindHostBinary + EvalSymlinks: launchd has a
// minimal PATH and needs the REAL absolute path (a ~/.local/bin symlink into a
// repo's out/ dir would break when the repo moves).
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
// KeepAlive unit installed over an already-running daemon collides on ports +
// store lock and crash-loops while install claims success. A foreground daemon
// is the user's to stop; a lazy one we started, we stop.
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

// verifyManagedInstallHealth is the post-install verification step (round 2, H8):
// a swallowed config.Load() failure used to let install report success for a unit
// that could never start. Success words are earned by a probe, or not at all.
func verifyManagedInstallHealth(cfg *config.Config, cfgErr error, st serveStarter, out io.Writer) bool {
	if cfgErr != nil {
		fmt.Fprintf(out, "warning: installed managed service, but could not verify it started: config.toml failed to load (%v). It will not start until this is fixed — edit config.toml, then check with `pix serve status`.\n", cfgErr)
		return false
	}
	return reportManagedServeHealth(st.dial, requiredServePorts(st, cfg, nil),
		time.Now, time.Sleep, 10*time.Second, out)
}

// reportManagedServeHealth verifies (bounded) that the freshly-installed managed
// service actually came up — its required ports answer — and reports HONESTLY
// when it did not (H5: "installed" must not paper over a crash-loop).
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

// runManagedVerb is the shared front door of `serve install`/`uninstall`: help
// on out, a hard refusal of any argument on errW (neither takes one), then the
// verb. Each answer is a RETURNED cli.SilentError carrying the exit code for a
// message already printed, so this package ends no process.
func runManagedVerb(verb string, argv []string, out, errW io.Writer, do func() error) error {
	if cli.WantsHelp(argv) {
		fmt.Fprint(out, Description)
		return nil
	}
	if len(argv) > 0 {
		fmt.Fprintf(errW, "pix serve %s: unexpected argument %q\n\n%s", verb, argv[0], Description)
		return cli.SilentError{Code: 2}
	}
	if err := do(); err != nil {
		fmt.Fprintf(errW, "pix serve %s: %v\n", verb, err)
		return cli.SilentError{Code: 1}
	}
	return nil
}

// RunInstall is `serve install` (platform dispatch: install_{darwin,other}.go).
func RunInstall(out, errW io.Writer, argv []string) error {
	return runManagedVerb("install", argv, out, errW, func() error {
		rl := DefaultReloader(errW)
		if err := preInstallGuard(rl.mode, rl.Stop, out); err != nil {
			return err
		}
		if err := platformServeInstall(out); err != nil {
			return err
		}
		// Bounded post-install verification; the warning carries the next step.
		cfg, cfgErr := config.Load()
		verifyManagedInstallHealth(cfg, cfgErr, DefaultStarter(errW), out)
		return nil // an unhealthy start is a warning, not a failed install
	})
}

// Install is the non-exiting install seam: `pix setup`'s provision loop needs an
// apply that RETURNS its failure (the loop records it and re-checks).
func Install(out io.Writer) error { return platformServeInstall(out) }

// RunUninstall is the `serve uninstall` entry point, on the same contract.
func RunUninstall(out, errW io.Writer, argv []string) error {
	return runManagedVerb("uninstall", argv, out, errW, func() error { return platformServeUninstall(out) })
}

// ctlTimeout bounds every service-control child. A VARIABLE so a test can
// shrink it; nothing else may write it.
var ctlTimeout = 20 * time.Second

// realCmdRunner is the concrete exec shim; the argv sequences around it are what
// the tests prove.
//
// BOUNDED, deliberately. `launchctl kickstart -k` kills the running daemon and
// waits for it to die, so a `pix-host serve` that is itself wedged shutting down
// takes the caller with it — and the caller is `propagateConfig`, a post-commit
// side effect whose whole contract is to warn and move on. `pix pack use` hung
// there with nothing printed after registration.
//
// WaitDelay matters as much as the deadline: without it, killing the child
// still leaves CombinedOutput blocked on a pipe an inherited grandchild holds
// open, which is the same hang wearing a different hat.
func realCmdRunner(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ctlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("`%s %s` did not finish within %s",
			name, strings.Join(args, " "), ctlTimeout)
	}
	return string(out), err
}
