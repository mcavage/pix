package main

// redrive_dxfix_test.go — DX-consultant redrive findings 1/3/4:
//
//  1: `pix route --help` (now `pix models --help`) pointed a consumer at the embedded repo source
//     (services/host/routing/defaults/scorecard.json) for the override file
//     they should hand-edit. That path only exists inside a pix repo
//     checkout and means nothing on a consumer machine — the consumer-facing
//     recovery path must be the live override dir
//     (~/.local/share/pix/routing/, honoring $ROUTING_DIR/$XDG_DATA_HOME);
//     a mention of the maintainer's embedded repo source is fine ONLY when
//     explicitly labeled as such.
//  3: `pix mcp register/load/auth/bundle` (and, by the same policy,
//     read-only `mcp ls`) printed "would run: ..." and exited 0 when sbx
//     isn't on PATH — a command that PROMISES an operation must not report a
//     silent success. They now return/exit with mcp.ErrSbxUnavailable ->
//     rpc.ExitServiceDown (3), the SAME "dependency unavailable" code
//     `pix memory`/`secret` already use, while still printing the exact
//     recovery command verbatim and never mutating a receipt or config file.
//  4: the same embedded-repo-path mistake as finding 1 also leaked into the
//     (now retired) `pix agent new`'s next-steps and `pix agent reassess
//     --model`'s guidance, and the removed `pix evals` message; the sweep test
//     below still guards the source it flagged.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/packinfo"
	"pix/host/routing"
	"pix/host/rpc"
	"pix/host/sys"
)

// modelsHelp renders `pix models --help` the way a user reads it: through the
// root parser, off the same tags that parse the verb.
func modelsHelp(t *testing.T) string {
	t.Helper()
	d, out, _ := rootDeps()
	if err := runRootParse([]string{"models", "--help"}, d); err != nil {
		t.Fatalf("models --help: %v", err)
	}
	return out.String()
}

// --- finding 1: route/models help is repo-less for the consumer -----------

// TestRouteUsage_ConsumerPathsAreLiveOverrideDir proves `models --help` points
// at the REAL resolved override paths (honoring $ROUTING_DIR), never a
// hardcoded guess and never the repo's embedded default source. (Test name
// kept from the `route` era; docs/design/models-cli.md renamed the verb, not
// this file's history.)
func TestRouteUsage_ConsumerPathsAreLiveOverrideDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROUTING_DIR", dir)

	usage := modelsHelp(t)

	wantModels := filepath.Join(dir, "models.json")
	wantScorecard := filepath.Join(dir, "scorecard.json")
	if !strings.Contains(usage, wantModels) {
		t.Errorf("route usage must point at the live override path %q, got:\n%s", wantModels, usage)
	}
	if !strings.Contains(usage, wantScorecard) {
		t.Errorf("route usage must point at the live override path %q, got:\n%s", wantScorecard, usage)
	}
	if strings.Contains(usage, routing.Dir()) && routing.Dir() != dir {
		t.Errorf("route usage resolved a stale routing dir, not the injected $ROUTING_DIR")
	}
}

// TestRouteUsage_EmbeddedRepoSourceIsLabeledMaintainerOnly: the ONE remaining
// mention of the repo's embedded default source (for a pix maintainer
// editing shipped defaults, not a personal override) must be explicitly
// labeled — never presented as if it were the consumer's recovery path.
func TestRouteUsage_EmbeddedRepoSourceIsLabeledMaintainerOnly(t *testing.T) {
	usage := modelsHelp(t)
	idx := strings.Index(usage, "services/host/routing/defaults")
	if idx < 0 {
		return // no mention at all is also fine
	}
	window := usage[:idx]
	if !strings.Contains(window, "repo checkout") && !strings.Contains(strings.ToLower(window), "maintainer") {
		t.Errorf("embedded repo source mention must be labeled maintainer-only, got:\n%s", usage)
	}
}

// --- finding 4: sweep for the same mistake elsewhere -----------------------

// TestNoUnlabeledRepoRoutingDefaultsInSource is the regression guard for the
// sweep: any mention of services/host/routing/defaults ANYWHERE in this
// package's non-test source must sit within an explicitly maintainer-labeled
// window, never bare as if it were a consumer's file to edit.
func TestNoUnlabeledRepoRoutingDefaultsInSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const needle = "services/host/routing/defaults"
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for i := 0; ; {
			idx := strings.Index(src[i:], needle)
			if idx < 0 {
				break
			}
			pos := i + idx
			start := pos - 250
			if start < 0 {
				start = 0
			}
			end := pos + len(needle) + 100
			if end > len(src) {
				end = len(src)
			}
			window := strings.ToLower(src[start:end])
			if !strings.Contains(window, "repo checkout") && !strings.Contains(window, "maintainer") {
				t.Errorf("%s: unlabeled repo-relative routing path at byte %d — label it maintainer-only or point at routing.ScorecardPath()/ModelsPath() instead:\n%s", f, pos, src[start:end])
			}
			i = pos + len(needle)
		}
	}
}

// --- finding 3: mcp verbs that promise an operation must not exit 0 --------

// runTypedMcp drives one `pix mcp` subcommand through its typed tree with sbx
// absent from PATH, and returns the exit code the launcher would produce plus
// the combined output. These cases used to re-exec the test binary because the
// seam called os.Exit deep inside itself; the command returns an error now, so
// the exit code is an ordinary assertion.
func runTypedMcp(t *testing.T, argv []string, extraEnv map[string]string) (int, string) {
	t.Helper()
	t.Setenv("PATH", t.TempDir()) // nothing on PATH, including no sbx
	for k, v := range extraEnv {
		t.Setenv(k, v)
	}
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("")}
	err := cli.RunRoot[mcpCmd]("pix mcp", "", "", argv, d)
	return cli.ExitCode(err), out.String()
}

// wantServiceDown asserts the honest-unknown contract shared by every mcp verb
// that promises an operation: exit 3, and the exact command to run by hand.
func wantServiceDown(t *testing.T, code int, out, wantCmd string) {
	t.Helper()
	if code != rpc.ExitServiceDown {
		t.Errorf("exit code = %d, want %d (rpc.ExitServiceDown); output:\n%s", code, rpc.ExitServiceDown, out)
	}
	if !strings.Contains(out, wantCmd) {
		t.Errorf("expected the exact recovery command %q, got:\n%s", wantCmd, out)
	}
}

// TestRunMcpLs_AbsentSbxExitsServiceDown: read-only `mcp ls` also exits 3 when
// sbx is unreachable — the documented policy decision (see mcp.ErrSbxUnavailable)
// so a caller can tell "zero servers" from "couldn't ask".
func TestRunMcpLs_AbsentSbxExitsServiceDown(t *testing.T) {
	code, out := runTypedMcp(t, []string{"ls"}, nil)
	wantServiceDown(t, code, out, "would run: sbx mcp ls")
}

// TestRunMcpAuth_AbsentSbxExitsServiceDown: `mcp auth` promises an OAuth
// operation; sbx-absent must not exit 0.
func TestRunMcpAuth_AbsentSbxExitsServiceDown(t *testing.T) {
	code, out := runTypedMcp(t, []string{"auth", "--all"}, nil)
	wantServiceDown(t, code, out, "would run: sbx mcp auth --all")
}

// TestRunMcpAddURL_AbsentSbxExitsServiceDown: `mcp add --url` promises to
// register a hosted server; sbx-absent must not exit 0.
func TestRunMcpAddURL_AbsentSbxExitsServiceDown(t *testing.T) {
	code, out := runTypedMcp(t, []string{"add", "acme", "--url", "https://mcp.acme.test/mcp"}, nil)
	wantServiceDown(t, code, out, "would run: sbx mcp add acme --url https://mcp.acme.test/mcp")
}

// TestRunMcpAdd_AbsentSbxExitsServiceDownNoConfigMutation: `mcp add` (bare, the
// builder path) promises to register servers with the gateway; sbx-absent must
// exit 3, print the exact would-run commands, and leave config.toml
// byte-for-byte untouched (mcp.RegisterServers never writes config — only `pix
// config set` may).
func TestRunMcpAdd_AbsentSbxExitsServiceDownNoConfigMutation(t *testing.T) {
	dir := t.TempDir()

	// A server only exists because a pack declares it, so the fixture activates
	// a pack declaring slack as a host command. Its binary IS on PATH (an
	// unresolvable command is its own hard failure, tested elsewhere), leaving
	// the missing sbx as the only thing that can go wrong.
	packRoot := filepath.Join(dir, "pack")
	mustWritePack(t, packRoot, packinfo.Manifest{Name: "acme", Schema: 1,
		Integrations: []packinfo.Integration{{Name: "Slack", MCP: "slack", Command: "slackmcp"}}})
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "slackmcp"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.toml")
	cfgContent := "mcp = [\"slack\"]\npack = " + strconv.Quote(packRoot) + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runTypedMcp(t, []string{"add"}, map[string]string{
		"PIX_CONFIG": cfgPath,
		"PATH":       binDir, // the server's command resolves; sbx does not
	})
	wantServiceDown(t, code, out, "sbx mcp add slack")

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != cfgContent {
		t.Errorf("config.toml mutated by `mcp add`:\n got: %q\nwant: %q", string(after), cfgContent)
	}
}
