package mcp

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

// --- real fixtures, replacing the retired hostenv/hostenvtest package -------
//
// RegisterServers only ever touches env.LookPath, env.RunTimed, env.
// RunInteractive(Quiet) and env.Quiet (verified against mcp.go directly — it
// never calls Getenv/IsFile/HomeDir/ReadFile), so a fixture here needs only
// two primitives: a PATH-isolated bin dir a test can drop a real "sbx"/"gog"
// executable into, and a real file a hostResolver can point at for pix-host.
// Every probe below execs the real thing; nothing is a call-keyed double.

// binDir returns a fresh, PATH-isolated directory: with PATH replaced (not
// appended) an unwritten binary is genuinely ABSENT, regardless of what the
// host running these tests happens to have installed.
func binDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	return dir
}

// installBin writes a REAL executable named name into dir. Invoked with argv,
// it answers with the entry in output keyed by the space-joined argv (exactly
// what env.RunTimed(name, argv...) will be called with) and exits 0; any
// other invocation exits 1 — an undeclared command fails the test loudly,
// the same contract the retired shared fixture documented.
func installBin(t *testing.T, dir, name string, output map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncase \"$*\" in\n")
	for args, out := range output {
		fmt.Fprintf(&b, "%s)\nprintf %%s %s\nexit 0\n;;\n", shQuote(args), shQuote(out))
	}
	b.WriteString("*) exit 1 ;;\nesac\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// shQuote single-quotes s for embedding in a POSIX shell script, so neither
// glob metacharacters in a case pattern nor shell syntax in canned output can
// leak out of the literal string a test declared.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// realEnv is the real System every fixture above wires into.
func realEnv() hostenv.Env { return hostenv.Env{System: sys.Real{}} }

// hostStub is a hostResolver that returns a fixed path (or an error).
func hostStub(path string, err error) func() (string, error) {
	return func() (string, error) { return path, err }
}

// gogRegistrar returns a registrar with fixed absolutes for builder tests.
func gogRegistrar() McpRegistrar {
	return McpRegistrar{
		Op:       "/usr/bin/op",
		OpRefs:   "/abs/config/op-refs.env",
		Gog:      "/usr/bin/gog",
		Account:  "me@x.com",
		HostBin:  "/usr/bin/pix-host",
		GogUseOp: true,
	}
}

// TestAddArgs_gog builds the hardened gog `sbx mcp add` command (mirrors the
// Makefile mcp-register line).
func TestAddArgs_Gog(t *testing.T) {
	args := gogRegistrar().AddArgs(config.GWServerName)
	if !contains(args, []string{"mcp", "add", config.GWServerName, "--command", "/usr/bin/op"}) {
		t.Errorf("missing add-command prefix in %v", args)
	}
	// op-run wrapper.
	if !contains(args, []string{"--args", "run", "--args", "--no-masking",
		"--args", "--env-file=/abs/config/op-refs.env", "--args", "--"}) {
		t.Errorf("missing op-run wrapper in %v", args)
	}
	// gog binary + hardened flags.
	if !contains(args, []string{"--args", "/usr/bin/gog", "--args", "--account", "--args", "me@x.com"}) {
		t.Errorf("missing gog account args in %v", args)
	}
	for _, flag := range []string{"--gmail-no-send", "--wrap-untrusted", "--readonly", "read"} {
		if !contains(args, []string{"--args", flag}) {
			t.Errorf("missing hardened flag %q in %v", flag, args)
		}
	}
}

// TestAddArgs_Slack builds the pix-host subcommand form (op-run wrapped).
func TestAddArgs_Slack(t *testing.T) {
	args := gogRegistrar().AddArgs("slack")
	if !contains(args, []string{"--args", "/usr/bin/pix-host", "--args", "mcp", "--args", "slack"}) {
		t.Errorf("slack should register pix-host mcp slack, got %v", args)
	}
}

// TestAddArgs_LocalServer builds the pix-host subcommand form for an
// arbitrary local stdio server like "pio": it registers as
// `pix-host mcp pio`, exactly like slack, via the serverCmd default.
func TestAddArgs_LocalServer(t *testing.T) {
	args := gogRegistrar().AddArgs("pio")
	if !contains(args, []string{"mcp", "add", "pio", "--command", "/usr/bin/op"}) {
		t.Errorf("missing add-command prefix for pio in %v", args)
	}
	if !contains(args, []string{"--args", "/usr/bin/pix-host", "--args", "mcp", "--args", "pio"}) {
		t.Errorf("pio should register pix-host mcp pio, got %v", args)
	}
}

// TestAddArgs_GogBare: with NO op-refs (opRefs=""), gog registers DIRECTLY as a
// bare command — no `op run` wrapper. 1Password is optional for gog.
func TestAddArgs_GogBare(t *testing.T) {
	reg := gogRegistrar()
	reg.OpRefs = "" // no op-refs -> bare
	args := reg.AddArgs(config.GWServerName)
	if !contains(args, []string{"mcp", "add", config.GWServerName, "--command", "/usr/bin/gog"}) {
		t.Errorf("bare gog should register --command /usr/bin/gog, got %v", args)
	}
	for _, a := range args {
		if a == "/usr/bin/op" || a == "run" {
			t.Errorf("bare gog must not wrap in op run, got %v", args)
		}
	}
	// Hardened flags survive in the bare form.
	if !contains(args, []string{"--args", "--account", "--args", "me@x.com"}) {
		t.Errorf("bare gog missing account args in %v", args)
	}
	for _, flag := range []string{"--gmail-no-send", "--wrap-untrusted", "--readonly", "read"} {
		if !contains(args, []string{"--args", flag}) {
			t.Errorf("bare gog missing hardened flag %q in %v", flag, args)
		}
	}
}

// TestRegisterServers_GogNoOpRefsBare: gateway on, op + op-refs ABSENT, gog
// present + account set -> gog registers DIRECTLY (bare command, no op wrapper)
// with the OAuth note. gog authenticates via its own OAuth grant, never
// op-refs, so the note must NOT mention op-refs.
func TestRegisterServers_GogNoOpRefsBare(t *testing.T) {
	dir := binDir(t)
	installBin(t, dir, "gog", nil) // present, never executed by RegisterServers
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, []string{config.GWServerName}, hostStub("", nil), nil, Credentials{}); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "registered gog directly") {
		t.Errorf("expected the bare-gog note, got:\n%s", out)
	}
	if strings.Contains(out, "op-refs.env") {
		t.Errorf("gog-only note must NOT mention op-refs (gog uses OAuth), got:\n%s", out)
	}
	// The would-run command must be the bare gog command, not an op wrapper.
	if !strings.Contains(out, "sbx mcp add google-workspace --command "+filepath.Join(dir, "gog")) {
		t.Errorf("expected a bare gog would-run command, got:\n%s", out)
	}
}

// TestRegisterServers_GogOnlyNoSeed (R5-1/R5-2): a gog-only clean state with op
// NOT resolvable must NOT create op-refs.env at the XDG path and must NOT print a
// "seeded" line. gog authenticates via its own OAuth grant, never op-refs, so
// seeding one would be actively wrong.
func TestRegisterServers_GogOnlyNoSeed(t *testing.T) {
	dir := binDir(t)
	installBin(t, dir, "gog", nil) // op absent -> not resolvable
	home := t.TempDir()
	cfg := defaultCfg()
	cfg.MCP = []string{config.GWServerName}
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, nil, hostStub("/usr/bin/pix-host", nil), nil, Credentials{}); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got: %v", err)
	}
	seeded := filepath.Join(home, ".config", "pix", "op-refs.env")
	if _, err := os.Stat(seeded); !os.IsNotExist(err) {
		t.Errorf("gog-only must NOT seed op-refs.env at %s (err=%v)", seeded, err)
	}
	out := buf.String()
	if strings.Contains(out, "seeded") {
		t.Errorf("gog-only must NOT print a seeded line, got:\n%s", out)
	}
	if strings.Contains(out, "op-refs.env") {
		t.Errorf("gog-only note must NOT mention op-refs, got:\n%s", out)
	}
}

// TestRegisterServers_SlackNoOpRefsBare: slack with op absent + op-refs absent no
// longer errors — it registers BARE (no op-run wrapper), so a no-creds server
// registers without 1Password. sbx absent -> the would-run command is printed.
func TestRegisterServers_SlackNoOpRefsBare(t *testing.T) {
	dir := binDir(t) // no op, no sbx
	hostBin := installBin(t, dir, "pix-host", map[string]string{"mcp --list": "slack\n"})
	cfg := defaultCfg()
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, []string{"slack"}, hostStub(hostBin, nil), nil, Credentials{}); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "registered slack directly (bare, no 1Password)") {
		t.Errorf("expected the bare-registration note for slack, got:\n%s", out)
	}
	// Bare command: --command <pix-host>, no op-run wrapper.
	if !strings.Contains(out, "sbx mcp add slack --command "+hostBin+" --args mcp --args slack") {
		t.Errorf("expected a bare slack would-run command, got:\n%s", out)
	}
	if strings.Contains(out, "op") && strings.Contains(out, "run --no-masking") {
		t.Errorf("bare slack must not wrap in op run, got:\n%s", out)
	}
}

// TestRegisterServers_SlackOpRefsAbsentSeeds: slack with op present but op-refs
// ABSENT -> seed a template at the absolute XDG path (via the ONE seeder,
// config.SeedOpRefsAt) and register BARE (no error), noting the seeded path.
func TestRegisterServers_SlackOpRefsAbsentSeeds(t *testing.T) {
	dir := binDir(t)
	hostBin := installBin(t, dir, "pix-host", map[string]string{"mcp --list": "slack\n"})
	home := t.TempDir()
	cfg := defaultCfg()
	var buf bytes.Buffer
	// op present, no op-refs found: the caller still says where one WOULD go,
	// which is what makes seeding possible without this package resolving paths.
	seeded := filepath.Join(home, ".config", "pix", "op-refs.env")
	creds := Credentials{OpPath: "/usr/bin/op", SeedPath: seeded}
	if err := RegisterServers(cfg, realEnv(), &buf, []string{"slack"}, hostStub(hostBin, nil), nil, creds); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got: %v", err)
	}
	info, err := os.Stat(seeded)
	if err != nil {
		t.Fatalf("expected the template seeded at %s: %v", seeded, err)
	}
	// The ONE seeder writes 0600 (owner-only).
	if info.Mode().Perm() != 0o600 {
		t.Errorf("seeded op-refs.env mode = %o, want 600", info.Mode().Perm())
	}
	if !strings.Contains(buf.String(), "seeded a template op-refs.env") {
		t.Errorf("expected the seeded-template note, got:\n%s", buf.String())
	}
}

// TestRegisterServers_RemoteSkipped: a cfg.MCP entry that is NEITHER gog NOR a
// local stdio server (per `pix-host mcp --list`) is a remote gateway-
// catalog server: it is SKIPPED with an info line, never registered as local.
// This gate fails if the local-vs-remote guard is removed (notion would then be
// wrongly registered).
func TestRegisterServers_RemoteSkipped(t *testing.T) {
	dir := binDir(t)                                                                      // no sbx -> would-run printed
	hostBin := installBin(t, dir, "pix-host", map[string]string{"mcp --list": "slack\n"}) // notion is NOT local
	cfg := defaultCfg()
	cfg.MCP = []string{"slack", "notion"}
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, nil, hostStub(hostBin, nil), nil, Credentials{}); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "notion: gateway-catalog server, not locally registered") {
		t.Errorf("expected notion to be skipped as a gateway-catalog server, got:\n%s", out)
	}
	if strings.Contains(out, "sbx mcp add notion") {
		t.Errorf("notion (remote) must NOT be registered as local, got:\n%s", out)
	}
	if !strings.Contains(out, "sbx mcp add slack") {
		t.Errorf("slack (local) should still be registered, got:\n%s", out)
	}
}

// TestRegisterServers_RemoteWithURLRegistered: a remote gateway-catalog name that
// the pack carries a URL for (containers[name].RemoteURL set) is NOT skipped — it
// is registered via `sbx mcp add <name> --url <url>`, with no --local and no
// op-run wrapper. This is the pack-self-registers-remotes path; it fails if the
// RemoteURL branch is dropped and meetings/notion/etc. fall back to the skip line.
func TestRegisterServers_RemoteWithURLRegistered(t *testing.T) {
	dir := binDir(t)                                                                      // no sbx -> would-run printed
	hostBin := installBin(t, dir, "pix-host", map[string]string{"mcp --list": "slack\n"}) // meetings is NOT a local server
	cfg := defaultCfg()
	cfg.MCP = []string{"meetings"}
	containers := map[string]config.MCPContainer{"meetings": {RemoteURL: "https://app.trymeetings.com/mcp"}}
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, nil, hostStub(hostBin, nil), containers, Credentials{}); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sbx mcp add meetings --url https://app.trymeetings.com/mcp") {
		t.Errorf("expected meetings registered via --url, got:\n%s", out)
	}
	if strings.Contains(out, "gateway-catalog server, not locally registered") {
		t.Errorf("meetings with a pack URL must NOT be skipped, got:\n%s", out)
	}
	if strings.Contains(out, "--local") || strings.Contains(out, "--command") {
		t.Errorf("remote-url server must not use --local/--command, got:\n%s", out)
	}
}

// The next three tests pin which of sbx's two mutation seams a remote-URL
// registration uses — RunInteractive (never killed mid-OAuth) vs the bounded
// RunTimed probe. That is a routing/seam-choice property, not a real
// subprocess's behavior, so it stays on the narrow, local systest.Fake
// (sys.System's ONE sanctioned test double) rather than a shell fixture.

func TestRegisterServers_RemoteURLUsesInteractiveRunner(t *testing.T) {
	var interactive []string
	env := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		RunInteractiveFn: func(name string, args ...string) error {
			interactive = append([]string{name}, args...)
			return nil
		},
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			// `sbx mcp add --help` is the bounded, read-only grammar-detection
			// probe (detectLegacyPositionalURL) RegisterServers runs BEFORE any
			// registration — it carries no server name and no --url, so it
			// cannot be mistaken for the real mutation this test guards.
			if name == "sbx" && len(args) >= 3 && args[1] == "add" && args[2] == "--help" {
				return "", false, nil
			}
			if name == "sbx" && len(args) >= 2 && args[1] == "add" {
				t.Fatalf("remote OAuth registration was sent through the bounded probe: %s %s", name, strings.Join(args, " "))
			}
			if name == "sbx" {
				return "", false, errors.New("not registered")
			}
			return "slack\n", false, nil
		},
	}}
	cfg := defaultCfg()
	cfg.MCP = []string{"meetings"}
	containers := map[string]config.MCPContainer{"meetings": {RemoteURL: "https://app.trymeetings.com/mcp"}}
	var out bytes.Buffer
	if err := RegisterServers(cfg, env, &out, nil, hostStub("/usr/bin/pix-host", nil), containers, Credentials{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(interactive, " "); got != "sbx mcp add meetings --url https://app.trymeetings.com/mcp" {
		t.Fatalf("interactive registration = %q", got)
	}
}

func TestRegisterServers_CurrentRemoteDoesNotReopenOAuth(t *testing.T) {
	endpoint := "https://app.trymeetings.com/mcp"
	env := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			command := strings.Join(args, " ")
			if name == "sbx" && command == "mcp inspect meetings" {
				return "URL: " + endpoint, false, nil
			}
			if name == "sbx" && command == "mcp auth status meetings" {
				return "meetings: authorized", false, nil
			}
			return "slack\n", false, nil
		},
		RunInteractiveFn: func(string, ...string) error {
			t.Fatal("an unchanged registered remote must not reopen OAuth")
			return nil
		},
	}}
	cfg := defaultCfg()
	cfg.MCP = []string{"meetings"}
	if err := RegisterServers(cfg, env, &bytes.Buffer{}, nil, hostStub("/usr/bin/pix-host", nil),
		map[string]config.MCPContainer{"meetings": {RemoteURL: endpoint}}, Credentials{}); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterServers_CurrentUnauthorizedRemoteRepairsOAuthOnce(t *testing.T) {
	endpoint := "https://app.trymeetings.com/mcp"
	authorized := false
	var interactive []string
	env := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			switch strings.Join(args, " ") {
			case "mcp inspect meetings":
				return `{"url":"` + endpoint + `"}`, false, nil
			case "mcp auth status meetings":
				if authorized {
					return "meetings: authorized", false, nil
				}
				return "meetings: not authenticated", false, nil
			default:
				return "slack\n", false, nil
			}
		},
		RunInteractiveFn: func(name string, args ...string) error {
			interactive = append([]string{name}, args...)
			authorized = true
			return nil
		},
	}}
	cfg := defaultCfg()
	cfg.MCP = []string{"meetings"}
	if err := RegisterServers(cfg, env, &bytes.Buffer{}, nil, hostStub("/usr/bin/pix-host", nil),
		map[string]config.MCPContainer{"meetings": {RemoteURL: endpoint}}, Credentials{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(interactive, " "); got != "sbx mcp auth meetings" {
		t.Fatalf("repair command = %q", got)
	}
}

func TestRemoteMCPRegistrationCurrentRejectsEndpointSubstring(t *testing.T) {
	want := "https://expected.example/mcp"
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return `{"url":"https://evil.example/?next=https://expected.example/mcp"}`, false, nil
	}}}
	if remoteMCPRegistrationCurrent(env, "meetings", want) {
		t.Fatal("an endpoint embedded inside another URL must not count as the registered endpoint")
	}
}

func TestRemoteMCPRegistrationCurrentRequiresEndpointField(t *testing.T) {
	want := "https://expected.example/mcp?a=1&b=2"
	for _, payload := range []string{
		`{"url":"https://evil.example/mcp","note":"https://expected.example/mcp?a=1&b=2"}`,
		`{"callback_url":"https://expected.example/mcp?a=1&b=2"}`,
		`{"url":"https://evil.example/mcp","nested":{"endpoint":"https://expected.example/mcp?a=1&b=2"}}`,
		`url: https://evil.example/?next=https://expected.example/mcp?a=1&b=2`,
	} {
		env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) { return payload, false, nil }}}
		if remoteMCPRegistrationCurrent(env, "meetings", want) {
			t.Fatalf("non-endpoint evidence was trusted: %s", payload)
		}
	}
}

func TestRemoteMCPRegistrationCurrentCanonicalExactMatch(t *testing.T) {
	want := "https://expected.example/mcp?b=2&a=1"
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return `{"server":{"remote_url":"HTTPS://EXPECTED.EXAMPLE:443/mcp?a=1&b=2"}}`, false, nil
	}}}
	if !remoteMCPRegistrationCurrent(env, "meetings", want) {
		t.Fatal("canonically identical endpoint field was not recognized")
	}
}

// TestRegisterServers_LocalSetUnknownFailClosed: when the local-name list can't
// be established (pix-host unresolved / `mcp --list` fails), a non-gog name
// must FAIL CLOSED — NOT be registered as a local pix-host subcommand — and
// RegisterServers must return a non-nil error so the command exits non-zero.
// This gate fails if the old fall-back-to-local behavior returns.
func TestRegisterServers_LocalSetUnknownFailClosed(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"} // a gateway-catalog name, NOT a local server
	var buf bytes.Buffer
	// hostStub fails: pix-host cannot be resolved, so the local set is unknown.
	err := RegisterServers(cfg, realEnv(), &buf, nil, hostStub("", errors.New("pix-host not found")), nil, Credentials{})
	if err == nil {
		t.Fatal("expected a non-nil error when the local set is unknown, got nil (silent success)")
	}
	if !strings.Contains(err.Error(), "could not determine local MCP servers") {
		t.Errorf("expected a fail-closed error, got %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "sbx mcp add notion") {
		t.Errorf("notion must NOT be registered as local when the local set is unknown, got:\n%s", out)
	}
	if !strings.Contains(out, "cannot determine local MCP servers") {
		t.Errorf("expected the fail-closed skip warning, got:\n%s", out)
	}
}

// TestRegisterServers_FailuresReturnError: when `sbx mcp add` fails for a
// server, register attempts it, prints the failure, AND returns a non-nil error
// so `pix mcp register` exits non-zero.
func TestRegisterServers_FailuresReturnError(t *testing.T) {
	dir := binDir(t)
	installBin(t, dir, "gog", nil)
	installBin(t, dir, "sbx", nil) // present but no matching output -> every add "fails"
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	err := RegisterServers(cfg, realEnv(), &buf, []string{config.GWServerName}, hostStub("", nil), nil, Credentials{})
	if err == nil || !strings.Contains(err.Error(), "failed to register") || !strings.Contains(err.Error(), config.GWServerName) {
		t.Errorf("expected a joined registration error mentioning gog, got %v", err)
	}
	if !strings.Contains(buf.String(), "FAILED to register: "+config.GWServerName) {
		t.Errorf("expected the per-server failure line, got:\n%s", buf.String())
	}
}

// TestRegisterServers_GogNotFound: op present, gog requested but absent.
func TestRegisterServers_GogNotFound(t *testing.T) {
	binDir(t) // op present is irrelevant here (never LookPath'd); gog stays absent
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	err := RegisterServers(cfg, realEnv(), &buf, []string{config.GWServerName}, hostStub("", nil), nil, Credentials{})
	if err == nil || !strings.Contains(err.Error(), "brew install openclaw/tap/gogcli") {
		t.Errorf("expected a gog-not-found guard, got %v", err)
	}
}

// TestRegisterServers_GogAccountUnset: op+gog present but no account -> guide the
// config-set command (never file editing).
func TestRegisterServers_GogAccountUnset(t *testing.T) {
	dir := binDir(t)
	installBin(t, dir, "gog", nil)
	cfg := defaultCfg() // GogAccount empty
	var buf bytes.Buffer
	err := RegisterServers(cfg, realEnv(), &buf, []string{config.GWServerName}, hostStub("", nil), nil, Credentials{})
	if err == nil || !strings.Contains(err.Error(), "pix config set google_workspace_account") {
		t.Errorf("expected the config-set google_workspace_account guide, got %v", err)
	}
}

// TestRegisterServers_SbxAbsentPrintsWouldRun: everything resolves but sbx is
// absent -> print the would-run command instead of crashing.
func TestRegisterServers_SbxAbsentPrintsWouldRun(t *testing.T) {
	dir := binDir(t) // no sbx
	installBin(t, dir, "gog", nil)
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, []string{config.GWServerName}, hostStub("", nil), nil, Credentials{}); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sbx mcp add google-workspace") || !strings.Contains(out, "me@x.com") {
		t.Errorf("expected a would-run command, got:\n%s", out)
	}
	if !strings.Contains(out, "install Docker Sandboxes") {
		t.Errorf("expected the sbx-absent note, got:\n%s", out)
	}
}

// TestRegisterServers_Registers: everything present + sbx runs -> registered.
func TestRegisterServers_Registers(t *testing.T) {
	dir := binDir(t)
	gogBin := installBin(t, dir, "gog", nil)
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	// Provide success output for the exact sbx call the registrar builds. GogUseOp
	// stays false (Credentials{} below -> opReady false), so this is the bare form
	// regardless of OpRefs — mirrored here by leaving it unset too.
	reg := McpRegistrar{Gog: gogBin, Account: "me@x.com"}
	key := strings.Join(reg.AddArgs(config.GWServerName), " ")
	if strings.Contains(key, "--command "+gogBin+" --command") || strings.HasPrefix(key, "run") {
		t.Fatalf("unrelated op-refs must not wrap normal gog OAuth: %s", key)
	}
	installBin(t, dir, "sbx", map[string]string{key: "ok"})
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, []string{config.GWServerName}, hostStub("", nil), nil, Credentials{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "registered: google-workspace") {
		t.Errorf("expected registered: google-workspace, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "Each wrapped server") {
		t.Errorf("bare gog registration claimed to use an op wrapper:\n%s", buf.String())
	}
}

// TestRegisterServers_DefaultsToConfigMCP: with no names, every entry in cfg.MCP
// is registered as a local stdio server. A non-gog name like "pio" registers as
// `pix-host mcp pio` (sbx absent -> would-run is printed).
func TestRegisterServers_DefaultsToConfigMCP(t *testing.T) {
	dir := binDir(t) // no sbx -> would-run printed
	hostBin := installBin(t, dir, "pix-host", map[string]string{"mcp --list": "pio\n"})
	cfg := defaultCfg()
	cfg.MCP = []string{"pio"} // an arbitrary local stdio server
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, nil, hostStub(hostBin, nil), nil, Credentials{}); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sbx mcp add pio") ||
		!strings.Contains(out, hostBin+" --args mcp --args pio") {
		t.Errorf("expected pio registered as pix-host mcp pio, got:\n%s", out)
	}
}

// TestRegisterServers_EmptyConfig: with no names and an empty cfg.MCP, there is
// nothing to register.
func TestRegisterServers_EmptyConfig(t *testing.T) {
	binDir(t)
	cfg := defaultCfg()
	cfg.MCP = nil
	var buf bytes.Buffer
	if err := RegisterServers(cfg, realEnv(), &buf, nil, hostStub("", nil), nil, Credentials{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "Nothing to register") {
		t.Errorf("expected nothing-to-register for an empty mcp set, got:\n%s", buf.String())
	}
}

// TestMcpRegistrar_ContainerAddArgs pins the pack CONTAINER integration path:
// a name with a manifest registers via `--local --url <manifest>` and is NOT
// op-run wrapped (its creds are Docker-side), while a normal local server still
// gets the op-run wrapper when op-refs is present.
func TestMcpRegistrar_ContainerAddArgs(t *testing.T) {
	reg := McpRegistrar{
		Op:      "/usr/bin/op",
		OpRefs:  "/abs/op-refs.env",
		HostBin: "/usr/local/bin/pix-host",
		containers: map[string]config.MCPContainer{
			"notion-ish": {Manifest: "https://example.com/mcp/x/server.json"},
			"hr":         {Image: "hr-mcp:0.0.1", EnvKeys: []string{"HR_API_KEY"}, EnvValues: map[string]string{"HR_COMPANY_DOMAIN": "acme"}},
			"meetings":   {RemoteURL: "https://app.trymeetings.com/mcp"},
		},
	}

	// Manifest container: --local --url, NOT op-run wrapped.
	got := strings.Join(reg.AddArgs("notion-ish"), " ")
	want := "mcp add notion-ish --local --url https://example.com/mcp/x/server.json"
	if got != want {
		t.Fatalf("manifest AddArgs:\n got: %s\nwant: %s", got, want)
	}

	// Remote container: --url (remote endpoint), NO --local and NOT op-run wrapped
	// (OAuth is handled host-side by the gateway).
	rem := strings.Join(reg.AddArgs("meetings"), " ")
	if rem != "mcp add meetings --url https://app.trymeetings.com/mcp" {
		t.Fatalf("remote-url AddArgs:\n got: %s\nwant: mcp add meetings --url https://app.trymeetings.com/mcp", rem)
	}
	if strings.Contains(rem, "--local") || strings.Contains(rem, "--command") {
		t.Fatalf("remote-url container must not use --local/--command, got:\n%s", rem)
	}

	// Image container: op-run-wrapped `docker run -i --rm -e KEY… <image>`.
	img := strings.Join(reg.AddArgs("hr"), " ")
	for _, must := range []string{
		"mcp add hr --command /usr/bin/op",
		"--args run", "--args --env-file=/abs/op-refs.env", "--args --",
		"--args docker --args run --args -i --args --rm",
		"--args -e --args HR_API_KEY --args -e --args HR_COMPANY_DOMAIN=acme",
		"--args hr-mcp:0.0.1",
	} {
		if !strings.Contains(img, must) {
			t.Fatalf("image AddArgs missing %q in:\n%s", must, img)
		}
	}
	if strings.Contains(img, "--local") {
		t.Fatalf("image container must not use --local, got:\n%s", img)
	}

	// A non-container name (slack) must still be op-run wrapped, not --local.
	slack := reg.AddArgs("slack")
	if len(slack) < 5 || slack[3] != "--command" || slack[4] != "/usr/bin/op" {
		t.Fatalf("non-container server must be op-run wrapped, got: %v", slack)
	}
	if strings.Contains(strings.Join(slack, " "), "--local") {
		t.Fatalf("non-container server must not use --local, got: %v", slack)
	}
}

// The gog-only-wraps-for-an-explicit-keyring-ref behavior (BuildGogRegistrar)
// used to back the built-in `pix gworkspace setup` wizard's pre-registration
// snapshot and had its own test here. That wizard is retired; the same gate
// (opReady && creds.GogKeyring) is now inlined directly in RegisterServers
// and exercised through it — see TestRegisterServers_GogNoOpRefsBare and
// TestRegisterServers_Registers below.

// --- DX finding 6: sbx-absent errors go to stderr, not stdout --------------

// TestRunMcpLsCore_SbxAbsentWritesRecoveryToStderr: `mcp ls` promises a
// listing (exit 3 on ErrSbxUnavailable, see RunMcpLsCore's doc). The
// recovery command it prints is an error report, not a listing row — a
// caller piping stdout (`pix mcp ls | jq`) must see NOTHING there, and the
// exact "would run" command on stderr where a human actually looks for it.
func TestRunMcpLsCore_SbxAbsentWritesRecoveryToStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	absent := func(string) (string, error) { return "", errors.New("not found") }
	err := RunMcpLsCore(absent, &out, strings.NewReader(""), &errOut)
	if !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("mcp ls must write nothing to stdout on sbx-absent, got stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "would run: sbx mcp ls") {
		t.Errorf("mcp ls must print the exact recovery command on stderr, got stderr=%q", errOut.String())
	}
}

// TestMcpLsAttachmentNote_NeverClaimsStatusOrDoctorShowLiveness: `pix status`/
// `pix doctor` cannot see inside a running session either (health/mcp.go's
// own attachmentCaveat says so), so the note must not send a reader there to
// learn "what's live" — that would just relocate the same unanswerable
// question. It must instead name the two commands that actually DO something:
// `pix mcp load` (attach now) and recreating the sandbox (preload at create).
func TestMcpLsAttachmentNote_NeverClaimsStatusOrDoctorShowLiveness(t *testing.T) {
	for _, bad := range []string{"for what's live", "see `pix status`", "see `pix doctor`"} {
		if strings.Contains(mcpLsAttachmentNote, bad) {
			t.Errorf("mcpLsAttachmentNote contains %q, implying status/doctor are an attachment authority they are not", bad)
		}
	}
	for _, want := range []string{"pix status", "pix doctor", "pix mcp load <name>", "pix rm <box>", "pix run"} {
		if !strings.Contains(mcpLsAttachmentNote, want) {
			t.Errorf("mcpLsAttachmentNote missing %q:\n%s", want, mcpLsAttachmentNote)
		}
	}
}
