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
//  4: the same embedded-repo-path mistake as finding 1 also leaked into
//     `pix agent new`'s next-steps, `pix agent reassess --model`'s
//     guidance, and the removed `pix evals` message.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/routing"
	"pix/host/rpc"
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

// TestAgentNew_NextStepsPointAtLiveScorecardPath: `agent new`'s "hand-add its
// scores to ..." next-step must be the live override path, not the repo's
// embedded defaults file (which doesn't exist outside a checkout).
func TestAgentNew_NextStepsPointAtLiveScorecardPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ROUTING_DIR", t.TempDir())
	if err := os.MkdirAll("agents", 0o755); err != nil {
		t.Fatal(err)
	}

	old := os.Stdout
	rp, wp, _ := os.Pipe()
	os.Stdout = wp
	mustRunAgent(t, "new", "redrive-dx-probe")
	_ = wp.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rp)

	out := buf.String()
	if !strings.Contains(out, routing.ScorecardPath()) {
		t.Errorf("agent new next-steps must point at %s, got:\n%s", routing.ScorecardPath(), out)
	}
	if strings.Contains(out, "services/host/routing/defaults") {
		t.Errorf("agent new next-steps must not point at the embedded repo source, got:\n%s", out)
	}
}

// TestAgentReassessModel_PointsAtLiveScorecardPath: `agent reassess --model X`
// exits 2 with guidance (measurement was removed); that guidance must be the
// live override path, not the repo's embedded defaults file. Run in a
// subprocess since agentReassess calls os.Exit(2) on this path.
func TestAgentReassessModel_PointsAtLiveScorecardPath(t *testing.T) {
	if os.Getenv("PIX_DXFIX_REASSESS") == "1" {
		mustRunAgent(t, "reassess", "--model", "anthropic/claude-haiku-4-5")
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run", "TestAgentReassessModel_PointsAtLiveScorecardPath")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PIX_DXFIX_REASSESS=1",
		"ROUTING_DIR="+filepath.Join(dir, "routing"),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("expected an ExitError, got %v (Output: %s)", runErr, out.String())
	}
	if ee.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2; output:\n%s", ee.ExitCode(), out.String())
	}
	wantPath := filepath.Join(dir, "routing", "scorecard.json")
	if !strings.Contains(out.String(), wantPath) {
		t.Errorf("expected the live scorecard path %q in guidance, got:\n%s", wantPath, out.String())
	}
	if strings.Contains(out.String(), "services/host/routing/defaults") {
		t.Errorf("agent reassess guidance must not point at the embedded repo source, got:\n%s", out.String())
	}
}

// --- finding 3: mcp verbs that promise an operation must not exit 0 --------

// TestRunMcpLs_AbsentSbxExitsServiceDown: read-only `mcp ls` also exits 3 when
// sbx is unreachable — the documented policy decision (see mcp.ErrSbxUnavailable)
// so a caller can tell "zero servers" from "couldn't ask".
func TestRunMcpLs_AbsentSbxExitsServiceDown(t *testing.T) {
	if os.Getenv("PIX_DXFIX_MCP_LS") == "1" {
		runMcpLs()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestRunMcpLs_AbsentSbxExitsServiceDown")
	cmd.Env = append(envWithout(os.Environ(), "PATH"),
		"PIX_DXFIX_MCP_LS=1",
		"PATH="+t.TempDir(),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("expected an ExitError, got %v (Output: %s)", runErr, out.String())
	}
	if ee.ExitCode() != rpc.ExitServiceDown {
		t.Errorf("exit code = %d, want %d (rpc.ExitServiceDown); output:\n%s", ee.ExitCode(), rpc.ExitServiceDown, out.String())
	}
	if !strings.Contains(out.String(), "would run: sbx mcp ls") {
		t.Errorf("expected the exact recovery command, got:\n%s", out.String())
	}
}

// TestRunMcpAuth_AbsentSbxExitsServiceDown: `mcp auth` promises an OAuth
// operation; sbx-absent must not exit 0.
func TestRunMcpAuth_AbsentSbxExitsServiceDown(t *testing.T) {
	if os.Getenv("PIX_DXFIX_MCP_AUTH") == "1" {
		runMcpAuth([]string{"--all"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestRunMcpAuth_AbsentSbxExitsServiceDown")
	cmd.Env = append(envWithout(os.Environ(), "PATH"),
		"PIX_DXFIX_MCP_AUTH=1",
		"PATH="+t.TempDir(),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("expected an ExitError, got %v (Output: %s)", runErr, out.String())
	}
	if ee.ExitCode() != rpc.ExitServiceDown {
		t.Errorf("exit code = %d, want %d (rpc.ExitServiceDown); output:\n%s", ee.ExitCode(), rpc.ExitServiceDown, out.String())
	}
	if !strings.Contains(out.String(), "would run: sbx mcp auth --all") {
		t.Errorf("expected the exact recovery command, got:\n%s", out.String())
	}
}

// TestRunMcpBundle_AbsentSbxExitsServiceDown: `mcp bundle` promises registering
// the catalog bundle; sbx-absent must not exit 0.
func TestRunMcpBundle_AbsentSbxExitsServiceDown(t *testing.T) {
	if os.Getenv("PIX_DXFIX_MCP_BUNDLE") == "1" {
		runMcpBundle(nil)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestRunMcpBundle_AbsentSbxExitsServiceDown")
	cmd.Env = append(envWithout(os.Environ(), "PATH"),
		"PIX_DXFIX_MCP_BUNDLE=1",
		"PATH="+t.TempDir(),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("expected an ExitError, got %v (Output: %s)", runErr, out.String())
	}
	if ee.ExitCode() != rpc.ExitServiceDown {
		t.Errorf("exit code = %d, want %d (rpc.ExitServiceDown); output:\n%s", ee.ExitCode(), rpc.ExitServiceDown, out.String())
	}
	if !strings.Contains(out.String(), "would run: sbx mcp bundle add") {
		t.Errorf("expected the exact recovery command, got:\n%s", out.String())
	}
}

// TestRunMcpRegister_AbsentSbxExitsServiceDownNoConfigMutation: `mcp register`
// promises to register servers with the gateway; sbx-absent must exit 3, print
// the exact would-run commands, and leave config.toml byte-for-byte untouched
// (mcp.RegisterServers never writes config — only `pix config set` may).
func TestRunMcpRegister_AbsentSbxExitsServiceDownNoConfigMutation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	const cfgContent = "mcp = [\"slack\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// A fake pix-host next to nothing real: `mcp --list` reports slack as
	// locally servable, so registration reaches the sbx-absent branch instead
	// of failing closed on an unresolvable local-name set.
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeHost := filepath.Join(binDir, "pix-host")
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo dev; exit 0; fi\nif [ \"$1\" = \"mcp\" ] && [ \"$2\" = \"--list\" ]; then echo slack; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(fakeHost, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if os.Getenv("PIX_DXFIX_MCP_REGISTER") == "1" {
		runMcpRegister(nil)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run", "TestRunMcpRegister_AbsentSbxExitsServiceDownNoConfigMutation")
	cmd.Env = append(envWithout(os.Environ(), "PATH"),
		"PIX_DXFIX_MCP_REGISTER=1",
		"PIX_CONFIG="+cfgPath,
		"PATH="+binDir, // pix-host resolvable; sbx is not
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("expected an ExitError, got %v (Output: %s)", runErr, out.String())
	}
	if ee.ExitCode() != rpc.ExitServiceDown {
		t.Errorf("exit code = %d, want %d (rpc.ExitServiceDown); output:\n%s", ee.ExitCode(), rpc.ExitServiceDown, out.String())
	}
	if !strings.Contains(out.String(), "sbx mcp add slack") {
		t.Errorf("expected the exact would-run registration command, got:\n%s", out.String())
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != cfgContent {
		t.Errorf("config.toml was mutated by a failed `mcp register`:\nbefore:\n%s\nafter:\n%s", cfgContent, after)
	}
}
