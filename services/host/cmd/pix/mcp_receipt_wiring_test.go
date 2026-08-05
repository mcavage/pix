// mcp_receipt_wiring_test.go — S03: wiring the sandboxmcpstate receipt (S02)
// into `pix run`'s create path and `pix mcp load`. Covers:
//
//   - success/failure ordering (never write before the underlying sbx exec
//     succeeds)
//   - reattach writes nothing; a definite create/replace writes and (on
//     replace) clears loads
//   - `mcp load` appends only on a successful attach; an absent sbx or a
//     failed load writes nothing
//   - a receipt-write failure after a successful operation is reported
//     distinctly and non-zero, never silently swallowed or claimed clean
//   - exact sandbox derivation / exact state-dir path (no doubled "pix")
//   - the call-site guard: only run.go writes create receipts, only mcp.go
//     writes load receipts
package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/config"
	"pix/host/mcp"
	"pix/host/rpc"
	"pix/host/sandbox"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
	"pix/host/workspace"
	"slices"
	"strings"
	"testing"
	"time"
)

// withSandboxMCPStateDirFn overrides the seam for the duration of the test.
func withSandboxMCPStateDirFn(t *testing.T, fn func() (string, error)) {
	t.Helper()
	old := workspace.MCPStateDirFn
	workspace.MCPStateDirFn = fn
	t.Cleanup(func() { workspace.MCPStateDirFn = old })
}

// trueCmd / falseCmd give launch.ExecSbxRunAndRecordCreate / mcp.ExecSbxMcpLoadAndRecord a
// real, fast, portable *exec.Cmd standing in for `sbx ...` without needing sbx
// on PATH at all — only its exit status matters to these wrappers.
func trueCmd(t *testing.T) *exec.Cmd  { t.Helper(); return exec.Command("true") }
func falseCmd(t *testing.T) *exec.Cmd { t.Helper(); return exec.Command("false") }

// withCreatePollSeams installs a fast, deterministic creation-evidence poll
// (probe + interval + timeout) for the duration of the test — no real `sbx
// ls`, no real half-second sleeps.
func withCreatePollSeams(t *testing.T, probe func(name string) sandbox.State, interval, timeout time.Duration) {
	t.Helper()
	oldProbe, oldInt, oldTO := launch.SandboxAppearProbeFn, launch.SandboxAppearPollInterval, launch.SandboxAppearPollTimeout
	launch.SandboxAppearProbeFn, launch.SandboxAppearPollInterval, launch.SandboxAppearPollTimeout = probe, interval, timeout
	t.Cleanup(func() {
		launch.SandboxAppearProbeFn, launch.SandboxAppearPollInterval, launch.SandboxAppearPollTimeout = oldProbe, oldInt, oldTO
	})
}

// probeAlways is a creation-evidence probe pinned to one state.
func probeAlways(st sandbox.State) func(string) sandbox.State {
	return func(string) sandbox.State { return st }
}

// --- launch.ExecSbxRunAndRecordCreate: ordering + gating ---------------------------

// A failed exec with no creation evidence must never write a receipt,
// regardless of writeReceipt.
func TestExecSbxRunAndRecordCreate_FailedExecWritesNoReceipt(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	withCreatePollSeams(t, probeAlways(launch.SbxAbsent), time.Millisecond, 5*time.Second)

	err := launch.ExecSbxRunAndRecordCreate(falseCmd(t), true, "pix-fail", "", []string{"slack"})
	if err == nil {
		t.Fatal("want an error propagated from the failed exec")
	}
	var rerr *workspace.ReceiptRecordError
	if errors.As(err, &rerr) {
		t.Fatalf("a failed exec must surface its OWN error, not a workspace.ReceiptRecordError: %v", err)
	}
	if _, status, _ := workspace.ReadMCPReceipt(dir, "pix-fail"); status != workspace.MCPStateAbsent {
		t.Fatalf("status = %v, want absent (no receipt on a failed exec)", status)
	}
}

// A successful exec with writeReceipt=false (a plain re-attach) writes
// nothing — the reattach contract.
func TestExecSbxRunAndRecordCreate_ReattachWritesNothing(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	if err := launch.ExecSbxRunAndRecordCreate(trueCmd(t), false, "pix-reattach", "", []string{"slack"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, status, _ := workspace.ReadMCPReceipt(dir, "pix-reattach"); status != workspace.MCPStateAbsent {
		t.Fatalf("status = %v, want absent (a reattach must write no receipt)", status)
	}
}

// A successful exec with writeReceipt=true (a definite create) writes the
// receipt with the exact preloaded set passed in.
func TestExecSbxRunAndRecordCreate_CreateWritesExactPreloadedSet(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	withCreatePollSeams(t, probeAlways(launch.SbxRunning), time.Millisecond, 5*time.Second)

	preloaded := []string{"slack", "gog", "notion"}
	if err := launch.ExecSbxRunAndRecordCreate(trueCmd(t), true, "pix-create", "", preloaded); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, status, err := workspace.ReadMCPReceipt(dir, "pix-create")
	if err != nil || status != workspace.MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Preloaded) != len(preloaded) {
		t.Fatalf("Preloaded = %v, want %v", r.Preloaded, preloaded)
	}
	for i, name := range preloaded {
		if r.Preloaded[i] != name {
			t.Fatalf("Preloaded = %v, want %v (order preserved)", r.Preloaded, preloaded)
		}
	}
	if len(r.Loads) != 0 {
		t.Fatalf("Loads = %v, want none on a fresh create", r.Loads)
	}
}

// --replace on a sandbox that already has a receipt (with loads) REWRITES the
// whole receipt and clears loads — the same replace contract
// TestWriteCreateReceiptReplaceResetsLoads pins at the sandboxmcpstate layer,
// exercised here through the actual wiring call.
func TestExecSbxRunAndRecordCreate_ReplaceRewritesAndClearsLoads(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	withCreatePollSeams(t, probeAlways(launch.SbxRunning), time.Millisecond, 5*time.Second)

	sandbox := "pix-replace-wire"
	if err := launch.ExecSbxRunAndRecordCreate(trueCmd(t), true, sandbox, "", []string{"slack"}); err != nil {
		t.Fatal(err)
	}
	if err := workspace.AppendLoadReceipt(dir, sandbox, "notion", nil); err != nil {
		t.Fatal(err)
	}
	// A --replace re-run: launch.DefinitelyCreating is true again (state doesn't
	// matter to the wrapper — the caller already decided), new preloaded set.
	if err := launch.ExecSbxRunAndRecordCreate(trueCmd(t), true, sandbox, "", []string{config.GWServerName}); err != nil {
		t.Fatal(err)
	}
	r, status, err := workspace.ReadMCPReceipt(dir, sandbox)
	if err != nil || status != workspace.MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Preloaded) != 1 || r.Preloaded[0] != config.GWServerName {
		t.Errorf("Preloaded = %v, want [gog] after replace", r.Preloaded)
	}
	if len(r.Loads) != 0 {
		t.Errorf("Loads = %v, want cleared after replace", r.Loads)
	}
}

// A receipt-write failure after a successful exec is reported as a DISTINCT
// *workspace.ReceiptRecordError, never confused with an exec failure, and never a
// silent success.
func TestExecSbxRunAndRecordCreate_ReceiptWriteFailureIsDistinctError(t *testing.T) {
	withSandboxMCPStateDirFn(t, func() (string, error) { return "", errors.New("boom: state dir unresolvable") })
	withCreatePollSeams(t, probeAlways(launch.SbxRunning), time.Millisecond, 5*time.Second)

	err := launch.ExecSbxRunAndRecordCreate(trueCmd(t), true, "pix-recerr", "", []string{"slack"})
	if err == nil {
		t.Fatal("want an error when the receipt write fails")
	}
	var rerr *workspace.ReceiptRecordError
	if !errors.As(err, &rerr) {
		t.Fatalf("want a *workspace.ReceiptRecordError, got %T: %v", err, err)
	}
	if rerr.Op != "create" || rerr.Sandbox != "pix-recerr" {
		t.Errorf("workspace.ReceiptRecordError = %+v, want op=create sandbox=pix-recerr", rerr)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error message should carry through the underlying cause, got %q", err.Error())
	}
}

// Decision-table mirror of launch.DefinitelyCreating (mirrors
// TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe's pattern, applied
// to the create-receipt gate this time): the caller (run.go) computes
// writeReceipt := launch.DefinitelyCreating(state, replace) — an inconclusive
// launch.SbxUnknown probe never writes, a positive absent/replace-on-known-state
// always does, matching run.go's actual gate byte for byte.
func TestCreateReceiptGate_MirrorsDefinitelyCreating(t *testing.T) {
	cases := []struct {
		State   sandbox.State
		replace bool
		want    bool
	}{
		{launch.SbxAbsent, false, true},
		{launch.SbxUnknown, false, false},
		{launch.SbxRunning, false, false},
		{launch.SbxStopped, false, false},
		{launch.SbxUnknown, true, false},
		{launch.SbxAbsent, true, true},
		{launch.SbxRunning, true, true},
		{launch.SbxStopped, true, true},
	}
	withCreatePollSeams(t, probeAlways(launch.SbxRunning), time.Millisecond, 5*time.Second)
	for _, tc := range cases {
		dir := t.TempDir()
		withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
		sandbox := "pix-gate"
		// Seed an existing receipt so a non-write case is verifiably untouched.
		if err := workspace.WriteCreateReceipt(dir, sandbox, "", []string{"existing"}, nil); err != nil {
			t.Fatal(err)
		}

		writeReceipt := launch.DefinitelyCreating(tc.State, tc.replace)
		if writeReceipt != tc.want {
			t.Fatalf("launch.DefinitelyCreating(%v,%v) = %v, want %v", tc.State, tc.replace, writeReceipt, tc.want)
		}
		if err := launch.ExecSbxRunAndRecordCreate(trueCmd(t), writeReceipt, sandbox, "", []string{"fresh"}); err != nil {
			t.Fatal(err)
		}
		r, _, err := workspace.ReadMCPReceipt(dir, sandbox)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want {
			if len(r.Preloaded) != 1 || r.Preloaded[0] != "fresh" {
				t.Errorf("state=%v replace=%v: want the fresh preloaded set written, got %v", tc.State, tc.replace, r.Preloaded)
			}
		} else {
			if len(r.Preloaded) != 1 || r.Preloaded[0] != "existing" {
				t.Errorf("state=%v replace=%v: want the existing receipt left untouched, got %v", tc.State, tc.replace, r.Preloaded)
			}
		}
	}
}

// --- exact state-dir path (no doubled "pix") ---------------------------

// launch.RecordCreateReceipt (the real production seam, workspace.MCPStateDirFn at its
// default) must land at exactly
// <XDG_STATE_HOME>/pix/sandboxes/<sandbox>/mcp.json — config.StateDir()
// already returns .../pix, so workspace.MCPStateRoot must NOT prepend
// another "pix" segment.
func TestRecordCreateReceipt_ExactStateDirPath(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)

	if err := launch.RecordCreateReceipt("pix-pathcheck", "", []string{"slack"}, true); err != nil {
		t.Fatalf("launch.RecordCreateReceipt: %v", err)
	}
	want := filepath.Join(xdg, "pix", "sandboxes", "pix-pathcheck", "mcp.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected receipt at %s: %v", want, err)
	}
	// And NOT doubled anywhere else.
	doubled := filepath.Join(xdg, "pix", "pix", "sandboxes", "pix-pathcheck", "mcp.json")
	if _, err := os.Stat(doubled); err == nil {
		t.Fatalf("receipt written at a DOUBLED pix path: %s", doubled)
	}
}

// --- mcp.ExecSbxMcpLoadAndRecord: ordering + gating -----------------------------

func TestExecSbxMcpLoadAndRecord_FailedLoadWritesNoReceipt(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	err := mcp.ExecSbxMcpLoadAndRecord(falseCmd(t), "pix-loadfail", "slack")
	if err == nil {
		t.Fatal("want an error propagated from the failed load")
	}
	var rerr *workspace.ReceiptRecordError
	if errors.As(err, &rerr) {
		t.Fatalf("a failed load must surface its OWN error, not a workspace.ReceiptRecordError: %v", err)
	}
	if _, status, _ := workspace.ReadMCPReceipt(dir, "pix-loadfail"); status != workspace.MCPStateAbsent {
		t.Fatalf("status = %v, want absent (no receipt on a failed load)", status)
	}
}

func TestExecSbxMcpLoadAndRecord_SuccessAppendsLoad(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	sandbox := "pix-loadok"
	if err := mcp.ExecSbxMcpLoadAndRecord(trueCmd(t), sandbox, "slack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, status, err := workspace.ReadMCPReceipt(dir, sandbox)
	if err != nil || status != workspace.MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Loads) != 1 || r.Loads[0].Name != "slack" {
		t.Fatalf("Loads = %+v, want [slack]", r.Loads)
	}
}

func TestExecSbxMcpLoadAndRecord_ReceiptWriteFailureIsDistinctError(t *testing.T) {
	withSandboxMCPStateDirFn(t, func() (string, error) { return "", errors.New("boom: no state dir") })

	err := mcp.ExecSbxMcpLoadAndRecord(trueCmd(t), "pix-loadrecerr", "slack")
	if err == nil {
		t.Fatal("want an error when the receipt write fails")
	}
	var rerr *workspace.ReceiptRecordError
	if !errors.As(err, &rerr) {
		t.Fatalf("want a *workspace.ReceiptRecordError, got %T: %v", err, err)
	}
	if rerr.Op != "mcp load" || rerr.Sandbox != "pix-loadrecerr" || rerr.Name != "slack" {
		t.Errorf("workspace.ReceiptRecordError = %+v, want op=mcp load sandbox=pix-loadrecerr name=slack", rerr)
	}
}

// runMcpLoad's "sbx absent" branch returns before constructing any *exec.Cmd
// at all, so it never reaches mcp.ExecSbxMcpLoadAndRecord — no receipt is ever
// written. A command that PROMISES an attach must not exit 0 having done
// nothing (finding: no-sbx behavior), so this now exits rpc.ExitServiceDown (3)
// instead of returning — proven in a subprocess since runMcpLoad calls
// os.Exit on this path.
func TestRunMcpLoad_AbsentSbxExitsServiceDownWritesNoReceipt(t *testing.T) {
	stateHome := t.TempDir()
	ws := t.TempDir()

	if os.Getenv("PIX_MCP_LOAD_ABSENT_SBX") == "1" {
		runMcpLoad([]string{"slack", ws})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run", "TestRunMcpLoad_AbsentSbxExitsServiceDownWritesNoReceipt")
	cmd.Env = append(envWithout(os.Environ(), "PATH"),
		"PIX_MCP_LOAD_ABSENT_SBX=1",
		"XDG_STATE_HOME="+stateHome,
		"PATH="+t.TempDir(), // nothing on PATH, including no sbx
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
	sandbox := workspace.DeriveSandboxName(ws)
	want := "would run: sbx mcp load slack --sandbox " + sandbox
	if !strings.Contains(out.String(), want) {
		t.Errorf("expected the exact recovery command %q, got:\n%s", want, out.String())
	}

	stateDir := filepath.Join(stateHome, "pix")
	if _, status, _ := workspace.ReadMCPReceipt(stateDir, sandbox); status != workspace.MCPStateAbsent {
		t.Fatalf("status = %v, want absent (sbx-absent branch must write no receipt)", status)
	}
}

// envWithout drops the named keys from an environment slice (os.Environ()
// shape), so a subprocess test can override PATH exactly once instead of
// relying on duplicate-key resolution order, which differs by platform.
func envWithout(base []string, keys ...string) []string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i >= 0 && drop[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// --- exact sandbox derivation ------------------------------------------------

// The sandbox identity a load receipt is keyed under must be EXACTLY what
// workspace.DeriveSandboxName(workspace) derives — the same name `sbx mcp load --sandbox`
// targets — not a re-derivation that could drift (e.g. from cwd instead of the
// given DIR).
func TestRecordMcpLoadReceipt_ExactSandboxDerivation(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	ws := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox := workspace.DeriveSandboxName(ws)
	if err := mcp.RecordMcpLoadReceipt(sandbox, "slack"); err != nil {
		t.Fatal(err)
	}
	// Read back under the SAME derivation — proves the receipt is keyed by the
	// exact name `sbx mcp load --sandbox` would have targeted, e.g.
	// "pix-my-project", not by cwd or the raw DIR string.
	if !strings.HasSuffix(sandbox, "my-project") {
		t.Fatalf("precondition: workspace.DeriveSandboxName(%q) = %q, expected it to end in my-project", ws, sandbox)
	}
	r, status, err := workspace.ReadMCPReceipt(dir, workspace.DeriveSandboxName(ws))
	if err != nil || status != workspace.MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Loads) != 1 || r.Loads[0].Name != "slack" {
		t.Fatalf("Loads = %+v", r.Loads)
	}
}

// --- pack integrations fold into the emitted static set AND the receipt ----

// S03 item 4: an active pack's integration MCP servers already fold into
// cfg.MCP via launch.ApplyPackToLaunch (S01); this pins that the run.go COMPUTATION
// run.go actually performs (mcp.AllPreloadedMCP(cfg.MCP+o.MCP) -> o.StaticMCP,
// emitted as --static-mcp by launch.BuildSbxArgs) and the CREATE RECEIPT agree
// exactly on that same set, for both a persisted (`pack use`d) integration and
// a transient --pack override never persisted to cfg.MCP.
func TestPackIntegrations_FoldIntoStaticSetAndReceipt(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "transient-pack")
	mustWritePack(t, root, pack.Manifest{Name: "transient", Schema: 1, Integrations: []pack.Integration{
		{Name: "Fastmail", MCP: "fastmail"},
		{Name: "Notion", MCP: "notion"},
	}})

	cfg := defaultCfg()
	cfg.MCP = []string{"slack"} // an already-persisted, non-pack server
	o := launch.RunOpts{Pack: root}
	if _, err := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil)); err != nil {
		t.Fatalf("launch.ApplyPackToLaunch: %v", err)
	}

	// The exact computation run.go performs before building sbx args / writing
	// the receipt.
	o.StaticMCP = mcp.AllPreloadedMCP(append(append([]string(nil), cfg.MCP...), o.MCP...))

	for _, want := range []string{"slack", "fastmail", "notion"} {
		if !slices.Contains(o.StaticMCP, want) {
			t.Errorf("o.StaticMCP = %v, want it to contain %q", o.StaticMCP, want)
		}
	}

	// Emitted --static-mcp flags mirror o.StaticMCP exactly.
	args := launch.BuildSbxArgs(cfg, o, "0.0.99")
	for _, want := range o.StaticMCP {
		if !contains(args, []string{"--static-mcp", want}) {
			t.Errorf("launch.BuildSbxArgs args %v missing --static-mcp %s", args, want)
		}
	}

	// The receipt, once written, carries the SAME set byte-for-byte.
	stateDir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return stateDir, nil })
	if err := launch.RecordCreateReceipt("pix-packfold", "", o.StaticMCP, true); err != nil {
		t.Fatal(err)
	}
	r, status, err := workspace.ReadMCPReceipt(stateDir, "pix-packfold")
	if err != nil || status != workspace.MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Preloaded) != len(o.StaticMCP) {
		t.Fatalf("Preloaded = %v, want exactly %v", r.Preloaded, o.StaticMCP)
	}
	for i, name := range o.StaticMCP {
		if r.Preloaded[i] != name {
			t.Fatalf("Preloaded = %v, want exactly %v (order preserved)", r.Preloaded, o.StaticMCP)
		}
	}
}

// --- call-site guard: only run.go / mcp.go may write a receipt -------------

// S03 item 3: "no automatic load anywhere" is enforced structurally — the only
// call sites that may invoke workspace.WriteCreateReceipt are run.go (the create path)
// and sandboxmcpstate.go (its own definition + doc comment), and the only call
// sites for workspace.AppendLoadReceipt are mcp.go (the explicit `mcp load` path) and
// sandboxmcpstate.go. A future call site fabricating a receipt anywhere else
// (an "automatic load" on session start, a background watcher, ...) fails this
// test rather than being discovered later via a wrong doctor/status report.
func TestMCPReceiptCallSitesAreGuarded(t *testing.T) {
	assertOnlyCalledFrom(t, "workspace.WriteCreateReceipt(", []string{"workflow/launch/run.go"})
	assertOnlyCalledFrom(t, "workspace.CommitCreateReceipt(", []string{"workflow/launch/run.go"})
	assertOnlyCalledFrom(t, "workspace.AppendLoadReceipt(", []string{"mcp/mcp.go"})
	// The create LIFECYCLE (pre-clear + start + evidence poll + commit + wait)
	// has exactly two owners: run.go (pix run) and task.go (task new) —
	// both launch paths MUST share the corrected lifecycle, and nothing else
	// may fabricate one.
	// Three sites: the definition, task's launch, and `pix run`'s argv seam --
	// runRun performs the launch itself, so it is a legitimate caller.
	assertOnlyCalledFrom(t, "ExecSbxRunAndRecordCreate(", []string{
		"workflow/launch/run.go", "workflow/launch/task.go", "cmd/pix/run_cmd.go"})
	assertOnlyCalledFrom(t, "workspace.ClearMCPReceipt(", []string{"workflow/launch/run.go"})
	// Receipt removal is tied to LAUNCHER sandbox removal only: pix rm
	// (sandbox.go), replace pre-remove (run.go), task teardown/prepare
	// (task.go), and `pix reset --sbx`'s positive per-sandbox removals
	// (reset.go).
	assertOnlyCalledFrom(t, "workspace.ClearRemovedReceipt(", []string{"workflow/launch/run.go", "workflow/launch/sandbox.go", "workflow/launch/task.go", "workflow/reset/reset.go"})
}

// C: task_cmd.go must actually route a task's sandbox launch through the
// SAME shared create lifecycle `pix run` uses — a task-created sandbox
// records the same receipt. Story06 removed task.go's own launch path
// (launchTask/ExecSbxRunAndRecordCreate duplicated here); `task new` and
// `task run` now delegate straight to runRun (run_cmd.go), which already
// goes through ExecSbxRunAndRecordCreate, so the guarantee is that neither
// verb ever launches a sandbox any other way.
func TestTaskLaunchUsesSharedCreateLifecycle(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("task_cmd.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if strings.Count(src, "runRun(") < 2 {
		t.Fatal("task_cmd.go must launch a task's sandbox by delegating to runRun (both `task new` and `task run`/--task), not a separate exec.Command(\"sbx\", ...) path")
	}
	if strings.Contains(src, "exec.Command(\"sbx\"") {
		t.Fatal("task_cmd.go must not shell out to sbx directly for a sandbox LAUNCH; that duplicates run.go's create-receipt lifecycle")
	}
}

func assertOnlyCalledFrom(t *testing.T, needle string, allowed []string) {
	t.Helper()
	// Scan the WHOLE module, not just this directory. When mcp.go moved into
	// its own package this globbed "*.go" and stopped seeing it — a guard that
	// silently covers less than it claims is worse than one that fails, so it
	// now walks from services/host and matches allowed entries as path
	// suffixes ("mcp/mcp.go"), which also survives the next extraction.
	var files []string
	root := filepath.Join("..", "..") // the test runs in cmd/pix; the module root is two up
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// NB: the root itself is "..", whose Name() starts with a dot — skipping
		// on that prefix without excluding the root walks nothing at all.
		if d.IsDir() && path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			rel, _ := filepath.Rel(root, path)
			files = append(files, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	allow := map[string]bool{}
	for _, a := range allowed {
		allow[a] = true
	}
	found := false
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), needle) {
			continue
		}
		found = true
		if !allow[f] && !allow[filepath.Base(f)] {
			t.Errorf("%s calls %s but is not an allowed call site (allowed: %v)", f, needle, allowed)
		}
	}
	if !found {
		t.Fatalf("guard is stale: %s was not found in ANY source file", needle)
	}
}
