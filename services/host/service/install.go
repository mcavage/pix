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
	"pix/host/rpc"
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

// resolvedHostBinary is the ABSOLUTE path a supervisor should launch, and it
// deliberately does NOT resolve symlinks.
//
// It used to EvalSymlinks, on the theory that launchd's minimal PATH needs "the
// REAL path". Launchd needs an absolute path, which a symlink already is — and
// resolving one defeats the exact indirection package managers exist to
// provide. Homebrew's /opt/homebrew/bin/pix-host points into a VERSIONED Cellar
// directory, so resolving baked `…/Cellar/pix/0.1.44/bin/pix-host` into the
// plist, and the next `brew upgrade` deleted that directory.
//
// The failure is silent and permanent. launchd keeps the job, cannot spawn it
// (`last exit code = 78: EX_CONFIG`), and parks in `spawn scheduled` — while
// `launchctl kickstart -k`, which pix runs after every pack change, BLOCKS
// FOREVER on a job that can never start. Measured on a real host: an agent
// pinned to 0.1.44 with only 0.1.54 installed, three separate "pix setup hangs"
// over two days, and 22 seconds of silence per run once the wait was bounded.
//
// A dangling symlink is no worse unresolved: if the target moves, both forms
// break, and only this one survives an upgrade that keeps the symlink correct.
func resolvedHostBinary() (string, error) {
	bin, err := launcher.FindHostBinary()
	if err != nil {
		return "", fmt.Errorf("pix-host not found — run `make install` first")
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

// verifyManagedInstallHealth is the post-install verification step (round 2, H8;
// identity check added U3-lifecycle): a swallowed config.Load() failure used to
// let install report success for a unit that could never start. Success words
// are earned by a probe, or not at all. probe is rpc.IdentityProbe in
// production, ALWAYS — there is no nil-skips-the-check seam here (architect
// round 2): a nil probe was a production-reachable way to silently disable the
// one thing this file exists to do, so it is gone; tests inject a fake probe
// (a test double) instead.
func verifyManagedInstallHealth(cfg *config.Config, cfgErr error, st serveStarter, probe rpc.IdentityProber, out io.Writer) bool {
	if cfgErr != nil {
		fmt.Fprintf(out, "warning: installed managed service, but could not verify it started: config.toml failed to load (%v). It will not start until this is fixed — edit config.toml, then check with `pix serve status`.\n", cfgErr)
		return false
	}
	return reportManagedServeHealth(st.dial, requiredServePorts(st, cfg, nil),
		time.Now, time.Sleep, 10*time.Second, probe, out)
}

// serveIdentityNames maps a servePortSpec's name to the identity name that
// service's `identity` RPC method answers with (rpc.identity.go). Only
// services with a known identity are version-checked here; anything else stays
// TCP-liveness-only, unchanged from before U3-lifecycle.
var serveIdentityNames = map[string]string{"memory": rpc.MemoryName}

// identityMismatch names ONE required service that is up on TCP but whose
// application identity does not (yet) prove it is the CURRENT, READY binary —
// the exact gap a listening port cannot close on its own. Exactly one of
// {err set, notReady, a version mismatch} applies. String() is the message a
// nonconverging repair must show: actual vs expected version (or the
// unit's own not-ready reason), plus the exact recovery command, never the
// word "updated" (U3-lifecycle).
type identityMismatch struct {
	service, want, got string
	err                error // set when the identity call itself failed (down/unreachable/malformed name)
	// notReady is the version-correct-but-not-ready case (architect round 2):
	// the binary IS current, but the unit itself says it is not up yet (still
	// warming up, or genuinely degraded) — a different gap from a stale
	// version, and worded as such rather than folded into "did not update".
	notReady       bool
	degradedReason string
}

func (m identityMismatch) String() string {
	switch {
	case m.err != nil:
		return fmt.Sprintf("%s answered its port but not its identity check (%v) — expected version %s. Run: pix serve stop && pix serve install",
			m.service, m.err, m.want)
	case m.notReady:
		detail := "not ready"
		if m.degradedReason != "" {
			detail = "not ready: " + m.degradedReason
		}
		return fmt.Sprintf("%s is up and reports the current version (%s) but is %s. Run: pix serve stop && pix serve install",
			m.service, m.want, detail)
	default:
		return fmt.Sprintf("%s is up but reports version %s, not %s — the running binary did not update. Run: pix serve stop && pix serve install",
			m.service, m.got, m.want)
	}
}

// verifyServeIdentity is the CONVERGENT reconciliation seam (U3-lifecycle): it
// makes exactly ONE identity probe per required port PER CALL (no retry loop
// of its own — that budget belongs to the caller's health-wait, which calls
// this once per poll) and reports every service whose reported name, version,
// or readiness does not (yet) prove it is the current, up binary. A listening
// port is not evidence that the RIGHT binary is behind it, and a matching
// version is not evidence it has finished starting; this is the check that
// closes both gaps, called ONLY from the explicit start path
// (verifyManagedInstallHealth / `pix serve start`⁄`install`), never from the
// read-side EnsureUp every `pix run`/`pix memory …` call makes.
//
// probe must never be nil (architect round 2): there is no skip-the-check
// seam here on purpose — a nil probe is a caller bug, not a supported way to
// waive identity verification, so it panics loudly at the call site instead
// of silently reporting success. Every production call passes
// rpc.IdentityProbe; a test that wants TCP-liveness-only coverage injects a
// fake probe answering the current name/version/ready, a test double, never
// nil.
func verifyServeIdentity(probe rpc.IdentityProber, ports []servePortSpec, wantVersion string) []identityMismatch {
	var mismatches []identityMismatch
	for _, p := range ports {
		want, ok := serveIdentityNames[p.name]
		if !ok {
			continue
		}
		id, err := probe(p.port)
		switch {
		case err != nil:
			mismatches = append(mismatches, identityMismatch{service: p.name, want: wantVersion, err: err})
		case id.Name != want:
			mismatches = append(mismatches, identityMismatch{service: p.name, want: wantVersion,
				err: fmt.Errorf("port answers as %q, not %q", id.Name, want)})
		case id.Version != wantVersion:
			got := id.Version
			if got == "" {
				got = "unknown (pre-version daemon)"
			}
			mismatches = append(mismatches, identityMismatch{service: p.name, want: wantVersion, got: got})
		case !id.Ready:
			// Version-correct-but-not-ready (architect round 2): the binary IS
			// current, so this is never worded as "did not update".
			mismatches = append(mismatches, identityMismatch{service: p.name, want: wantVersion,
				notReady: true, degradedReason: id.DegradedReason})
		}
	}
	return mismatches
}

// reportManagedServeHealth verifies (bounded) that the freshly-installed managed
// service actually came up — its required ports answer, AND (architect round
// 2) its application identity confirms the CURRENT, READY binary is behind
// them — and reports HONESTLY when it did not (H5: "installed" must not paper
// over a crash-loop).
//
// Identity verification is FOLDED INTO the same bounded poll as the TCP wait,
// not a one-shot check bolted on after it: a mismatch, a probe error, or a
// not-ready unit all mean KEEP POLLING, exactly like a port that has not
// opened yet — because both an old-then-new drain (the outgoing process keeps
// answering, stale, for a moment while the incoming one binds the port) and a
// brand-new binary that has not finished warming up look identical to a
// nonconverging failure on the FIRST poll. Only the DEADLINE may turn a
// mismatch into a warning, and that warning must show the LAST OBSERVED
// actual/expected state, not force a snap judgment on the very first sample —
// which is what let a launchd `kickstart -k` mid-restart print a false
// "did not update" warning while the new process was still binding the port.
// Success ("managed service is up") is earned only once BOTH Ready and a
// matching name/version are true; a mechanically-successful restart that
// still answers as the OLD version, or answers current-but-not-ready forever,
// is a warning naming what was last seen + the exact recovery command, never
// success and never the word "updated".
func reportManagedServeHealth(dial func(int) bool, ports []servePortSpec,
	now func() time.Time, sleep func(time.Duration), timeout time.Duration,
	probe rpc.IdentityProber, out io.Writer) bool {
	if len(ports) == 0 {
		return true // nothing enabled to probe
	}
	deadline := now().Add(timeout)
	var lastMismatches []identityMismatch // last OBSERVED identity gap, for an honest deadline warning
	for {
		up := true
		for _, p := range ports {
			if !dial(p.port) {
				up = false
				break
			}
		}
		if up {
			if mismatches := verifyServeIdentity(probe, ports, launcher.Version); len(mismatches) > 0 {
				lastMismatches = mismatches // keep polling; see the doc comment above
			} else {
				fmt.Fprintf(out, "managed service is up (%s)\n", describeServePorts(ports))
				return true
			}
		}
		if !now().Before(deadline) {
			if len(lastMismatches) > 0 {
				for _, m := range lastMismatches {
					fmt.Fprintf(out, "warning: %s\n", m)
				}
			} else {
				fmt.Fprintf(out, "warning: the managed service was installed but its services (%s) did not answer within %s — it may be failing to start; check the logs above and `pix serve status`.\n",
					describeServePorts(ports), timeout)
			}
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
		// Bounded post-install verification, now including application identity
		// (U3-lifecycle): the warning carries the next step, success is earned by
		// a verified probe.
		cfg, cfgErr := config.Load()
		verifyManagedInstallHealth(cfg, cfgErr, DefaultStarter(errW), rpc.IdentityProbe, out)
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
