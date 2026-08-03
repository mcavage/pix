package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
	"pix/host/workflow/setup"
	"pix/host/workspace"
)

// --- config gate: host.enabled / host.autonomy (config_cli test style) ---

// TestApplyConfigChange_HostEnabled: default false; set requires an explicit
// boolean; unset resets to false; the value round-trips through Save/Load.
func TestApplyConfigChange_HostEnabled(t *testing.T) {
	t.Setenv("PIX_CONFIG", t.TempDir()+"/config.toml")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Enabled {
		t.Fatal("host.enabled must default to FALSE (the gate is non-negotiable)")
	}

	sum, err := setup.ApplyConfigChange(cfg, false, "host.enabled", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Host.Enabled || !strings.Contains(sum, "true") {
		t.Errorf("set host.enabled true: cfg=%v summary=%q", cfg.Host.Enabled, sum)
	}

	// Round-trip: the enabled gate survives Save/Load.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Host.Enabled {
		t.Error("host.enabled did not survive Save/Load")
	}

	// A non-boolean is rejected, never inferred.
	if _, err := setup.ApplyConfigChange(cfg, false, "host.enabled", []string{"yes-please"}); err == nil {
		t.Error("expected an error for a non-boolean host.enabled value")
	}
	if _, err := setup.ApplyConfigChange(cfg, false, "host.enabled", nil); err == nil {
		t.Error("expected an arity error for set host.enabled with no value")
	}

	// unset resets the gate to off.
	if _, err := setup.ApplyConfigChange(cfg, true, "host.enabled", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Enabled {
		t.Error("unset host.enabled: want false")
	}
}

// TestApplyConfigChange_HostAutonomy: reserved key stores + clears a string.
func TestApplyConfigChange_HostAutonomy(t *testing.T) {
	cfg := defaultCfg()
	sum, err := setup.ApplyConfigChange(cfg, false, "host.autonomy", []string{"strict"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Autonomy != "strict" || !strings.Contains(sum, "strict") {
		t.Errorf("set host.autonomy: cfg=%q summary=%q", cfg.Host.Autonomy, sum)
	}
	if _, err := setup.ApplyConfigChange(cfg, true, "host.autonomy", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Autonomy != "" {
		t.Errorf("unset host.autonomy: cfg=%q, want empty", cfg.Host.Autonomy)
	}
}

// TestConfigValue_HostKeys: `config get` renders both host keys.
func TestConfigValue_HostKeys(t *testing.T) {
	cfg := defaultCfg()
	if v, err := setup.ConfigValue(cfg, "host.enabled"); err != nil || v != "false" {
		t.Errorf("get host.enabled = %q,%v, want false,nil", v, err)
	}
	cfg.Host.Enabled = true
	cfg.Host.Autonomy = "strict"
	if v, _ := setup.ConfigValue(cfg, "host.enabled"); v != "true" {
		t.Errorf("get host.enabled = %q, want true", v)
	}
	if v, _ := setup.ConfigValue(cfg, "host.autonomy"); v != "strict" {
		t.Errorf("get host.autonomy = %q, want strict", v)
	}
}

// --- parser: `host` has its own subcommand parser ---

// TestParseHostArgs_SetupIsNotADir: a bare `setup` is the provision subcommand,
// never parsed as a workspace directory.
func TestParseHostArgs_SetupIsNotADir(t *testing.T) {
	sub, _, err := launch.ParseHostArgs([]string{"setup"})
	if err != nil || sub != "setup" {
		t.Fatalf("launch.ParseHostArgs(setup) = %q, %v; want subcommand setup", sub, err)
	}
	// Extra args after setup are a usage error.
	if _, _, err := launch.ParseHostArgs([]string{"setup", "extra"}); err == nil {
		t.Error("expected an error for `host setup extra`")
	}
}

// TestParseHostArgs_Launch: DIR + --model + passthrough parse; the default
// workspace is "."; unknown flags and extra positionals error.
func TestParseHostArgs_Launch(t *testing.T) {
	dir := t.TempDir()
	sub, o, err := launch.ParseHostArgs([]string{dir, "--model", "anthropic/claude-sonnet-5", "--", "-p", "hi"})
	if err != nil || sub != "" {
		t.Fatalf("launch.ParseHostArgs launch: sub=%q err=%v", sub, err)
	}
	if o.Workspace != dir || o.Model != "anthropic/claude-sonnet-5" {
		t.Errorf("opts = %+v", o)
	}
	if strings.Join(o.Passthrough, " ") != "-p hi" {
		t.Errorf("passthrough = %v", o.Passthrough)
	}

	if _, o, err := launch.ParseHostArgs(nil); err != nil || o.Workspace != "." {
		t.Errorf("default workspace: %+v, %v", o, err)
	}
	if _, _, err := launch.ParseHostArgs([]string{"--frob"}); err == nil {
		t.Error("expected unknown-flag error")
	}
	if _, _, err := launch.ParseHostArgs([]string{dir, dir}); err == nil {
		t.Error("expected extra-positional error")
	}
	if _, _, err := launch.ParseHostArgs([]string{"-h"}); err != cli.ErrHelpRequested {
		t.Errorf("-h: err = %v, want cli.ErrHelpRequested", err)
	}
	// A nonexistent dir is rejected (shared launch.ValidateRunWorkspace contract).
	if _, _, err := launch.ParseHostArgs([]string{"no-such-dir-xyz"}); err == nil {
		t.Error("expected an error for a nonexistent workspace")
	}
}

// TestParseHostArgs_RefusesGuardDisablingPassthrough: the launcher appends the
// passthrough AFTER `-e host-guard.ts`, so any flag that disables or displaces
// extensions would launch pi UNGUARDED. launch.ParseHostArgs must refuse them — both
// the `--flag value` and `--flag=value` spellings — while normal pi args pass.
func TestParseHostArgs_RefusesGuardDisablingPassthrough(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range [][]string{
		{"--no-extensions"},
		{"--extensions", "none"},
		{"--extensions=none"},
		{"-e", "/tmp/evil.ts"},
		{"-e=/tmp/evil.ts"},
		{"--extension", "/tmp/evil.ts"},
		{"--extension=/tmp/evil.ts"},
		{"-p", "hi", "--no-extensions"}, // buried later in the passthrough
	} {
		argv := append([]string{dir, "--"}, bad...)
		if _, _, err := launch.ParseHostArgs(argv); err == nil {
			t.Errorf("launch.ParseHostArgs(%v) = nil, want a guard-displacement refusal", argv)
		}
	}
	// The pure checker refuses the same set directly (the seam runHostLaunch
	// relies on) …
	if err := launch.CheckHostPassthrough([]string{"--no-extensions"}); err == nil {
		t.Error("launch.CheckHostPassthrough(--no-extensions) = nil, want error")
	}
	// … and benign passthrough still parses.
	if _, o, err := launch.ParseHostArgs([]string{dir, "--", "-p", "hi", "--thinking", "high"}); err != nil {
		t.Fatalf("benign passthrough refused: %v", err)
	} else if strings.Join(o.Passthrough, " ") != "-p hi --thinking high" {
		t.Errorf("passthrough = %v", o.Passthrough)
	}
}

// --- workspace refusals (Phase-1 security blocker) ---

func canon(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestValidateHostWorkspace_Refusals: $HOME, /, /etc (and below), inside a
// secret dir, and a workspace directly containing one are all refused; a plain
// project dir passes.
func TestValidateHostWorkspace_Refusals(t *testing.T) {
	home := canon(t, t.TempDir())

	for _, ws := range []string{"/", "/etc", "/etc/ssh", home} {
		if err := launch.ValidateHostWorkspace(ws, home); err == nil {
			t.Errorf("launch.ValidateHostWorkspace(%q) = nil, want refusal", ws)
		}
	}

	// Inside a secret dir under home.
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(filepath.Join(ssh, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{ssh, filepath.Join(ssh, "sub")} {
		if err := launch.ValidateHostWorkspace(ws, home); err == nil {
			t.Errorf("launch.ValidateHostWorkspace(%q) = nil, want refusal (inside secrets)", ws)
		}
	}

	// A workspace that directly CONTAINS a secret dir.
	holder := filepath.Join(home, "proj-with-keys")
	if err := os.MkdirAll(filepath.Join(holder, ".aws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := launch.ValidateHostWorkspace(holder, home); err == nil {
		t.Error("workspace containing .aws must be refused")
	}

	// A normal project dir is fine.
	ok := filepath.Join(home, "dev", "proj")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := launch.ValidateHostWorkspace(ok, home); err != nil {
		t.Errorf("launch.ValidateHostWorkspace(%q) = %v, want nil", ok, err)
	}
}

// TestValidateHostWorkspace_SymlinkedSecretDir: ~/.ssh being a SYMLINK must
// still refuse a workspace at (or under) its real target. The workspace arrives
// EvalSymlinks-resolved, so the secret dir must be canonicalized too — a
// lexical $HOME/.ssh never prefix-matches the resolved real path.
func TestValidateHostWorkspace_SymlinkedSecretDir(t *testing.T) {
	home := canon(t, t.TempDir())
	realSSH := canon(t, t.TempDir()) // outside $HOME, like a dotfiles store
	if err := os.Symlink(realSSH, filepath.Join(home, ".ssh")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := launch.ValidateHostWorkspace(realSSH, home); err == nil {
		t.Error("workspace at the real target of a symlinked ~/.ssh must be refused")
	}
	sub := filepath.Join(realSSH, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := launch.ValidateHostWorkspace(sub, home); err == nil {
		t.Error("workspace under the real target of a symlinked ~/.ssh must be refused")
	}
}

// TestValidateHostWorkspace_NestedSecretEntry: a workspace AT $HOME/.config
// with $HOME/.config/gcloud beneath it must be refused. The old check joined
// the full relative entry onto the workspace ($HOME/.config/.config/gcloud) and
// wrongly accepted it.
func TestValidateHostWorkspace_NestedSecretEntry(t *testing.T) {
	home := canon(t, t.TempDir())
	cfgDir := filepath.Join(home, ".config")
	if err := os.MkdirAll(filepath.Join(cfgDir, "gcloud"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := launch.ValidateHostWorkspace(cfgDir, home); err == nil {
		t.Error("workspace at $HOME/.config containing gcloud must be refused")
	}
	// Without any secret dir beneath it, $HOME/.config itself is acceptable.
	home2 := canon(t, t.TempDir())
	cfg2 := filepath.Join(home2, ".config")
	if err := os.MkdirAll(cfg2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := launch.ValidateHostWorkspace(cfg2, home2); err != nil {
		t.Errorf("empty $HOME/.config workspace refused: %v", err)
	}
}

// TestResolveHostWorkspace_SymlinkCannotDefeatCheck: a symlink pointing at the
// home directory must be refused — the check runs on the EvalSymlinks-resolved
// real path, not the lexical one (the /tmp/link-to-home attack from the design
// doc).
func TestResolveHostWorkspace_SymlinkCannotDefeatCheck(t *testing.T) {
	home := canon(t, t.TempDir())
	t.Setenv("HOME", home)

	other := t.TempDir()
	link := filepath.Join(other, "link-to-home")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := launch.ResolveHostWorkspace(link); err == nil {
		t.Fatal("a symlink to $HOME must not defeat the workspace refusal")
	}

	// And the resolved path of a legitimate workspace comes back canonical.
	proj := filepath.Join(home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := launch.ResolveHostWorkspace(proj)
	if err != nil {
		t.Fatalf("launch.ResolveHostWorkspace(proj): %v", err)
	}
	if got != canon(t, proj) {
		t.Errorf("resolved = %q, want canonical %q", got, canon(t, proj))
	}
}

// --- env contract + argv (what the TS extensions rely on) ---

// TestHostChildEnv_Contract pins the EXACT env contract the TS side matches
// (ollama-bridge host bypass, subagent kill switch, service URLs). Changing a
// name or value here is a cross-repo breaking change — update the TS
// extensions in the same commit.
func TestHostChildEnv_Contract(t *testing.T) {
	t.Setenv("MEMORY_PORT", "")
	t.Setenv("KNOWLEDGE_PORT", "")
	env := launch.HostChildEnv("/state/host-agent", "")
	want := []string{
		"PI_CODING_AGENT_DIR=/state/host-agent",
		"MEMORY_URL=http://127.0.0.1:11435",
		"KNOWLEDGE_URL=http://127.0.0.1:11436",
		"OLLAMA_HOSTMODE=1",
		"OLLAMA_URL=http://127.0.0.1:11434/v1",
		"PI_SUBAGENT_DISABLED=1",
		"PI_SUBAGENT_MAX_DEPTH=0",
		// F3: pack host wrappers on the child PATH — host mode ONLY.
		"PATH=" + pack.HostPackBinDir() + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Errorf("host env contract drifted:\n got: %v\nwant: %v", env, want)
	}
}

// TestHostChildEnv_PortOverrideAndBridgeModel: MEMORY_URL/KNOWLEDGE_URL honor
// the same MEMORY_PORT/KNOWLEDGE_PORT overrides the services honor, and the
// configured ollama_bridge_model rides along as OLLAMA_BRIDGE_MODEL (absent
// when unset — the bridge then uses its own default).
func TestHostChildEnv_PortOverrideAndBridgeModel(t *testing.T) {
	t.Setenv("MEMORY_PORT", "21435")
	t.Setenv("KNOWLEDGE_PORT", "21436")
	env := strings.Join(launch.HostChildEnv("/sa", "qwen3.5:9b"), "\n")
	for _, want := range []string{
		"MEMORY_URL=http://127.0.0.1:21435",
		"KNOWLEDGE_URL=http://127.0.0.1:21436",
		"OLLAMA_BRIDGE_MODEL=qwen3.5:9b",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("launch.HostChildEnv missing %q in:\n%s", want, env)
		}
	}
	if strings.Contains(strings.Join(launch.HostChildEnv("/sa", "  "), "\n"), "OLLAMA_BRIDGE_MODEL") {
		t.Error("blank bridge model must not export OLLAMA_BRIDGE_MODEL")
	}
}

// TestProvisionHostAgentDir_HarnessFiles: setup symlinks the harness dirs AND
// the root harness files (capabilities.json — capability-routing reads it from
// $PI_CODING_AGENT_DIR; routing.json — subagents.ts intent→model; keybindings)
// but never mcp.json (sbx gateway; host mode doesn't use it).
func TestProvisionHostAgentDir_HarnessFiles(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	for _, d := range []string{"skills", "agents"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"capabilities.json", "routing.json", "keybindings.json", "mcp.json"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if err := launch.ProvisionHostAgentDir(root, dir, devnull); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"skills", "agents", "capabilities.json", "routing.json", "keybindings.json"} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: want symlink into the checkout, got %v/%v", name, fi, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "mcp.json")); !os.IsNotExist(err) {
		t.Error("mcp.json must NOT be provisioned (sbx gateway only)")
	}
	// settings.json + host-context.md written; idempotent re-run.
	for _, f := range []string{"settings.json", "host-context.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not provisioned: %v", f, err)
		}
	}
	if err := launch.ProvisionHostAgentDir(root, dir, devnull); err != nil {
		t.Errorf("provision is not idempotent: %v", err)
	}
}

// TestHostPinnedPiPackage_MatchesDockerfile: the "install pi" hints must name
// the ACTUAL pinned version. Cross-check the constant against the Dockerfile
// ARG when the checkout is reachable.
func TestHostPinnedPiPackage_MatchesDockerfile(t *testing.T) {
	const prefix = "@earendil-works/pi-coding-agent@"
	if !strings.HasPrefix(launch.HostPinnedPiPackage, prefix) {
		t.Fatalf("launch.HostPinnedPiPackage = %q, want %s<version>", launch.HostPinnedPiPackage, prefix)
	}
	if v := strings.TrimPrefix(launch.HostPinnedPiPackage, prefix); v == "" || v == "latest" {
		t.Fatalf("launch.HostPinnedPiPackage must pin a real version, got %q", v)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Skip("no cwd")
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err == nil {
			if !strings.Contains(string(b), "ARG PI_PACKAGE="+launch.HostPinnedPiPackage) {
				t.Errorf("launch.HostPinnedPiPackage %q drifted from the Dockerfile ARG PI_PACKAGE — bump both together", launch.HostPinnedPiPackage)
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("no Dockerfile found (not running in a checkout)")
}

func TestHostProvisioned_RequiresPinnedPiVersion(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := workspace.HostAgentDir()
	if err := os.MkdirAll(filepath.Join(dir, "extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", filepath.Join("extensions", "host-guard.ts")} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fakePiBin(t)
	if launch.HostProvisioned() {
		t.Error("host mode must not be provisioned without the curated extension marker")
	}
	if err := os.WriteFile(filepath.Join(dir, launch.HostPiExtensionsLockFile), []byte(launch.HostPiExtensionsMarker()), 0o644); err != nil {
		t.Fatal(err)
	}
	fakePiBinVersion(t, "0.0.0")
	if launch.HostProvisioned() {
		t.Error("host mode must not be provisioned with a stale pi core")
	}
	fakePiBin(t)
	if !launch.HostProvisioned() {
		t.Error("host mode must be provisioned when harness files, curated extensions, and pinned pi exist")
	}
}

// TestBuildHostArgs: the guard extension and host preamble are ALWAYS on the
// argv (Phase-1 blockers), sessions live outside the checkout, and model +
// passthrough ride along.
func TestBuildHostArgs(t *testing.T) {
	args := launch.BuildHostArgs("/sa", "PREAMBLE", "/personal/skills", launch.HostOpts{Model: "m", Passthrough: []string{"-p", "hi"}})
	j := strings.Join(args, " ")
	for _, want := range []string{
		"--session-dir /sa/sessions",
		"-e /sa/extensions/host-guard.ts",
		"--append-system-prompt PREAMBLE",
		"--skill /personal/skills",
		"--model m",
		"-p hi",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("host argv missing %q in %q", want, j)
		}
	}
}

// TestHostAgentDir_HonorsXDGStateHome mirrors the task.go contract.
func TestHostAgentDir_HonorsXDGStateHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	if got, want := workspace.HostAgentDir(), filepath.Join(tmp, "pix", "host-agent"); got != want {
		t.Errorf("workspace.HostAgentDir = %q, want %q", got, want)
	}
}

// --- UX: gate message + banner ---

// TestHostGateMessage names the exact enable command and the danger, and the
// banner carries the required warning line.
func TestHostGateMessage(t *testing.T) {
	msg := launch.HostGateMessage()
	for _, want := range []string{
		"pix host setup",
		"no sandbox",
		"no network fence",
		"real credentials",
		"NOT a security boundary",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("gate message missing %q", want)
		}
	}
	b := launch.HostBanner(false)
	if !strings.Contains(b, "HOST MODE — commands run on YOUR machine. No sandbox, no network fence, real credentials. Ctrl-C to abort.") {
		t.Errorf("banner text drifted: %q", b)
	}
	if strings.Contains(b, "\x1b[") {
		t.Error("non-TTY banner must not carry ANSI color")
	}
	if !strings.Contains(launch.HostBanner(true), "\x1b[1;31m") {
		t.Error("TTY banner should be red")
	}
}

// TestHostInHelpTiers: `host` is a known verb with usage, listed in the EXPERT
// tier (--all) but NEVER in the Core helpText (keeping it off the easy path).
func TestHostInHelpTiers(t *testing.T) {
	if !knownVerbs["host"] {
		t.Error("host missing from knownVerbs")
	}
	if _, ok := verbUsage("host"); !ok {
		t.Error("verbUsage(host) should exist")
	}
	if !strings.Contains(helpAllText, "host [DIR]") {
		t.Error("help --all must list host")
	}
	if strings.Contains(helpText, "host [DIR]") {
		t.Error("Core helpText must NOT list host (expert tier only)")
	}
}

// --- FIX 1: idempotent pi-extension install (launch.InstallHostPiExtensions) ---

// fakePiBin puts an executable `pi` shim on PATH (and restores the original
// PATH via t.Cleanup) that logs every `install npm:<pkg>` call to a file, so
// tests can assert whether/how many times it ran. failOn (may be empty) marks
// package names whose install exits non-zero.
func fakePiBin(t *testing.T, failOn ...string) (logPath string) {
	t.Helper()
	return fakePiBinVersion(t, strings.TrimPrefix(launch.HostPinnedPiPackage, "@earendil-works/pi-coding-agent@"), failOn...)
}

func fakePiBinVersion(t *testing.T, version string, failOn ...string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("if [ \"$1\" = \"--version\" ]; then echo " + version + "; exit 0; fi\n")
	script.WriteString("echo \"$@\" >> " + logPath + "\n")
	for _, f := range failOn {
		script.WriteString("case \"$2\" in *" + f + "*) echo boom >&2; exit 1;; esac\n")
	}
	script.WriteString("exit 0\n")
	pi := filepath.Join(dir, "pi")
	if err := os.WriteFile(pi, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// A marker matching the current launch.HostPiPackages set skips the whole install:
// `pi` is never invoked, and the "already installed" line is printed.
func TestInstallHostPiExtensions_SkipsWhenMarkerMatches(t *testing.T) {
	dir := t.TempDir()
	logPath := fakePiBin(t)
	if err := os.WriteFile(filepath.Join(dir, launch.HostPiExtensionsLockFile), []byte(launch.HostPiExtensionsMarker()), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	failed := launch.InstallHostPiExtensions(&out, dir)
	if len(failed) != 0 {
		t.Errorf("want no failures on a skipped install, got %v", failed)
	}
	if !strings.Contains(out.String(), "already installed") {
		t.Errorf("must print the skip line, got:\n%s", out.String())
	}
	b, err := os.ReadFile(logPath)
	if err != nil || len(b) != 0 {
		t.Errorf("pi must never be invoked when the marker matches, log: %q, err=%v", string(b), err)
	}
}

// No marker: every package installs (with a visible per-package progress
// line so it's never a silent hang), and the marker is written afterward.
func TestInstallHostPiExtensions_InstallsAndWritesMarker(t *testing.T) {
	dir := t.TempDir()
	logPath := fakePiBin(t)
	var out strings.Builder
	failed := launch.InstallHostPiExtensions(&out, dir)
	if len(failed) != 0 {
		t.Fatalf("want no failures, got %v", failed)
	}
	for i, p := range launch.HostPiPackages {
		want := fmt.Sprintf("installing pi extension %s (%d/%d)...", p, i+1, len(launch.HostPiPackages))
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing progress line %q in:\n%s", want, out.String())
		}
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range launch.HostPiPackages {
		if !strings.Contains(string(logged), "npm:"+p) {
			t.Errorf("pi was never invoked for %s: log=%q", p, string(logged))
		}
	}
	marker, err := os.ReadFile(filepath.Join(dir, launch.HostPiExtensionsLockFile))
	if err != nil || string(marker) != launch.HostPiExtensionsMarker() {
		t.Errorf("marker not written correctly: %q (err=%v)", string(marker), err)
	}
}

// A stale marker (package list changed since it was written) busts the
// idempotency check and re-installs, refreshing the marker to the new set.
func TestInstallHostPiExtensions_StaleMarkerBustsAndRefreshes(t *testing.T) {
	dir := t.TempDir()
	logPath := fakePiBin(t)
	if err := os.WriteFile(filepath.Join(dir, launch.HostPiExtensionsLockFile), []byte("pi-plan@0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	failed := launch.InstallHostPiExtensions(&out, dir)
	if len(failed) != 0 {
		t.Fatalf("want no failures, got %v", failed)
	}
	if strings.Contains(out.String(), "already installed") {
		t.Errorf("a stale marker must NOT skip the install, got:\n%s", out.String())
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "npm:"+launch.HostPiPackages[0]) {
		t.Errorf("stale marker must trigger a real (re)install, log=%q", string(logged))
	}
	marker, err := os.ReadFile(filepath.Join(dir, launch.HostPiExtensionsLockFile))
	if err != nil || string(marker) != launch.HostPiExtensionsMarker() {
		t.Errorf("marker not refreshed to the current set: %q (err=%v)", string(marker), err)
	}
}

// A failed package install is reported (name + captured output) and the
// marker is NEVER written when anything failed — so the next run retries.
func TestInstallHostPiExtensions_FailureReportedMarkerNotWritten(t *testing.T) {
	dir := t.TempDir()
	failing := launch.HostPiPackages[2] // pi-manage-todo-list@0.4.0
	fakePiBin(t, strings.SplitN(failing, "@", 2)[0])
	var out strings.Builder
	failed := launch.InstallHostPiExtensions(&out, dir)
	if len(failed) != 1 || failed[0] != failing {
		t.Fatalf("want exactly %q to fail, got %v", failing, failed)
	}
	if !strings.Contains(out.String(), "✗ "+failing) {
		t.Errorf("failure must be reported by name, got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, launch.HostPiExtensionsLockFile)); err == nil {
		t.Error("marker must NOT be written when any package failed to install")
	}
}

// A stale core pi is rejected before extension installs can create a
// core/extension compatibility mismatch. The error includes the exact upgrade.
func TestInstallHostPiExtensions_RejectsStalePi(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, launch.HostPiExtensionsLockFile), []byte(launch.HostPiExtensionsMarker()), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := fakePiBinVersion(t, "0.0.0")
	var out strings.Builder
	failed := launch.InstallHostPiExtensions(&out, dir)
	if len(failed) != len(launch.HostPiPackages) {
		t.Fatalf("want all %d packages reported failed, got %d: %v", len(launch.HostPiPackages), len(failed), failed)
	}
	wantVersion := strings.TrimPrefix(launch.HostPinnedPiPackage, "@earendil-works/pi-coding-agent@")
	if !strings.Contains(out.String(), "found pi \"0.0.0\", need \""+wantVersion+"\"") || !strings.Contains(out.String(), "npm install -g "+launch.HostPinnedPiPackage) {
		t.Errorf("must explain the stale core and exact upgrade, got:\n%s", out.String())
	}
	logged, err := os.ReadFile(logPath)
	if err != nil || len(logged) != 0 {
		t.Errorf("stale pi must not install extensions, log: %q, err=%v", string(logged), err)
	}
}

func TestCheckHostPiVersion_TimesOut(t *testing.T) {
	dir := t.TempDir()
	pi := filepath.Join(dir, "pi")
	if err := os.WriteFile(pi, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A short injected bound: the invariant is that the deadline FIRES and is
	// reported, which is observable in 50ms exactly as well as in two seconds.
	const bound = 50 * time.Millisecond
	started := time.Now()
	err := launch.CheckHostPiVersionWithin(pi, bound)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("hanging pi error = %v, want bounded timeout", err)
	}
	if elapsed > bound+2*time.Second {
		t.Fatalf("hanging pi probe took %s, want near %s", elapsed, bound)
	}
	// And the production path is still bounded at all: an unbounded default
	// would make the injection meaningless.
	if launch.HostPiVersionProbeTimeout <= 0 {
		t.Fatal("launch.HostPiVersionProbeTimeout must be a positive bound")
	}
}

// Missing `pi` on PATH: every package is reported as failed and the marker
// check never runs (no marker written, no crash).
func TestInstallHostPiExtensions_NoPiOnPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir) // a PATH with nothing on it
	var out strings.Builder
	failed := launch.InstallHostPiExtensions(&out, dir)
	if len(failed) != len(launch.HostPiPackages) {
		t.Errorf("want all %d packages reported failed, got %d: %v", len(launch.HostPiPackages), len(failed), failed)
	}
	if !strings.Contains(out.String(), "`pi` not found on PATH") {
		t.Errorf("must explain the missing binary, got:\n%s", out.String())
	}
}

// Symlink safety: a symlinked marker is never followed for a read (treated as
// absent/untrusted) or a write (the symlink is removed, never written through,
// and the link target is left untouched).
func TestHostPiExtensionsMarker_SymlinkSafe(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte(launch.HostPiExtensionsMarker()), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, launch.HostPiExtensionsLockFile)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if launch.HostPiExtensionsInstalled(dir) {
		t.Error("a symlinked marker must never be trusted, even with matching content")
	}
	if err := launch.WriteHostPiExtensionsMarker(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("marker must be replaced with a REAL file after writing, got %v/%v", fi, err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != launch.HostPiExtensionsMarker() {
		t.Errorf("symlink target must be untouched by the write: %q, err=%v", string(b), err)
	}
}
