// mcp_receipt_wiring_test.go — S03: wiring the sandboxmcpstate receipt (S02)
// into `pi-stack run`'s create path and `pi-stack mcp load`. Covers:
//
//   - success/failure ordering (never write before the underlying sbx exec
//     succeeds)
//   - reattach writes nothing; a definite create/replace writes and (on
//     replace) clears loads
//   - `mcp load` appends only on a successful attach; an absent sbx or a
//     failed load writes nothing
//   - a receipt-write failure after a successful operation is reported
//     distinctly and non-zero, never silently swallowed or claimed clean
//   - exact sandbox derivation / exact state-dir path (no doubled "pi-stack")
//   - the call-site guard: only run.go writes create receipts, only mcp.go
//     writes load receipts
package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withSandboxMCPStateDirFn overrides the seam for the duration of the test.
func withSandboxMCPStateDirFn(t *testing.T, fn func() (string, error)) {
	t.Helper()
	old := sandboxMCPStateDirFn
	sandboxMCPStateDirFn = fn
	t.Cleanup(func() { sandboxMCPStateDirFn = old })
}

// trueCmd / falseCmd give execSbxRunAndRecordCreate / execSbxMcpLoadAndRecord a
// real, fast, portable *exec.Cmd standing in for `sbx ...` without needing sbx
// on PATH at all — only its exit status matters to these wrappers.
func trueCmd(t *testing.T) *exec.Cmd  { t.Helper(); return exec.Command("true") }
func falseCmd(t *testing.T) *exec.Cmd { t.Helper(); return exec.Command("false") }

// --- execSbxRunAndRecordCreate: ordering + gating ---------------------------

// A failed exec must never write a receipt, regardless of writeReceipt.
func TestExecSbxRunAndRecordCreate_FailedExecWritesNoReceipt(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	err := execSbxRunAndRecordCreate(falseCmd(t), true, "pi-stack-fail", []string{"slack"})
	if err == nil {
		t.Fatal("want an error propagated from the failed exec")
	}
	var rerr *receiptRecordError
	if errors.As(err, &rerr) {
		t.Fatalf("a failed exec must surface its OWN error, not a receiptRecordError: %v", err)
	}
	if _, status, _ := readSandboxMCPReceipt(dir, "pi-stack-fail"); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent (no receipt on a failed exec)", status)
	}
}

// A successful exec with writeReceipt=false (a plain re-attach) writes
// nothing — the reattach contract.
func TestExecSbxRunAndRecordCreate_ReattachWritesNothing(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	if err := execSbxRunAndRecordCreate(trueCmd(t), false, "pi-stack-reattach", []string{"slack"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, status, _ := readSandboxMCPReceipt(dir, "pi-stack-reattach"); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent (a reattach must write no receipt)", status)
	}
}

// A successful exec with writeReceipt=true (a definite create) writes the
// receipt with the exact preloaded set passed in.
func TestExecSbxRunAndRecordCreate_CreateWritesExactPreloadedSet(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	preloaded := []string{"slack", "gog", "notion"}
	if err := execSbxRunAndRecordCreate(trueCmd(t), true, "pi-stack-create", preloaded); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, status, err := readSandboxMCPReceipt(dir, "pi-stack-create")
	if err != nil || status != sandboxMCPStateOK {
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

	sandbox := "pi-stack-replace-wire"
	if err := execSbxRunAndRecordCreate(trueCmd(t), true, sandbox, []string{"slack"}); err != nil {
		t.Fatal(err)
	}
	if err := appendLoadReceipt(dir, sandbox, "notion", nil); err != nil {
		t.Fatal(err)
	}
	// A --replace re-run: definitelyCreating is true again (state doesn't
	// matter to the wrapper — the caller already decided), new preloaded set.
	if err := execSbxRunAndRecordCreate(trueCmd(t), true, sandbox, []string{"gog"}); err != nil {
		t.Fatal(err)
	}
	r, status, err := readSandboxMCPReceipt(dir, sandbox)
	if err != nil || status != sandboxMCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Preloaded) != 1 || r.Preloaded[0] != "gog" {
		t.Errorf("Preloaded = %v, want [gog] after replace", r.Preloaded)
	}
	if len(r.Loads) != 0 {
		t.Errorf("Loads = %v, want cleared after replace", r.Loads)
	}
}

// A receipt-write failure after a successful exec is reported as a DISTINCT
// *receiptRecordError, never confused with an exec failure, and never a
// silent success.
func TestExecSbxRunAndRecordCreate_ReceiptWriteFailureIsDistinctError(t *testing.T) {
	withSandboxMCPStateDirFn(t, func() (string, error) { return "", errors.New("boom: state dir unresolvable") })

	err := execSbxRunAndRecordCreate(trueCmd(t), true, "pi-stack-recerr", []string{"slack"})
	if err == nil {
		t.Fatal("want an error when the receipt write fails")
	}
	var rerr *receiptRecordError
	if !errors.As(err, &rerr) {
		t.Fatalf("want a *receiptRecordError, got %T: %v", err, err)
	}
	if rerr.op != "create" || rerr.sandbox != "pi-stack-recerr" {
		t.Errorf("receiptRecordError = %+v, want op=create sandbox=pi-stack-recerr", rerr)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error message should carry through the underlying cause, got %q", err.Error())
	}
}

// Decision-table mirror of definitelyCreating (mirrors
// TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe's pattern, applied
// to the create-receipt gate this time): the caller (run.go) computes
// writeReceipt := definitelyCreating(state, replace) — an inconclusive
// sbxUnknown probe never writes, a positive absent/replace-on-known-state
// always does, matching run.go's actual gate byte for byte.
func TestCreateReceiptGate_MirrorsDefinitelyCreating(t *testing.T) {
	cases := []struct {
		state   sbxState
		replace bool
		want    bool
	}{
		{sbxAbsent, false, true},
		{sbxUnknown, false, false},
		{sbxRunning, false, false},
		{sbxStopped, false, false},
		{sbxUnknown, true, false},
		{sbxAbsent, true, true},
		{sbxRunning, true, true},
		{sbxStopped, true, true},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
		sandbox := "pi-stack-gate"
		// Seed an existing receipt so a non-write case is verifiably untouched.
		if err := writeCreateReceipt(dir, sandbox, []string{"existing"}, nil); err != nil {
			t.Fatal(err)
		}

		writeReceipt := definitelyCreating(tc.state, tc.replace)
		if writeReceipt != tc.want {
			t.Fatalf("definitelyCreating(%v,%v) = %v, want %v", tc.state, tc.replace, writeReceipt, tc.want)
		}
		if err := execSbxRunAndRecordCreate(trueCmd(t), writeReceipt, sandbox, []string{"fresh"}); err != nil {
			t.Fatal(err)
		}
		r, _, err := readSandboxMCPReceipt(dir, sandbox)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want {
			if len(r.Preloaded) != 1 || r.Preloaded[0] != "fresh" {
				t.Errorf("state=%v replace=%v: want the fresh preloaded set written, got %v", tc.state, tc.replace, r.Preloaded)
			}
		} else {
			if len(r.Preloaded) != 1 || r.Preloaded[0] != "existing" {
				t.Errorf("state=%v replace=%v: want the existing receipt left untouched, got %v", tc.state, tc.replace, r.Preloaded)
			}
		}
	}
}

// --- exact state-dir path (no doubled "pi-stack") ---------------------------

// recordCreateReceipt (the real production seam, sandboxMCPStateDirFn at its
// default) must land at exactly
// <XDG_STATE_HOME>/pi-stack/sandboxes/<sandbox>/mcp.json — config.StateDir()
// already returns .../pi-stack, so sandboxMCPStateRoot must NOT prepend
// another "pi-stack" segment.
func TestRecordCreateReceipt_ExactStateDirPath(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)

	if err := recordCreateReceipt("pi-stack-pathcheck", []string{"slack"}); err != nil {
		t.Fatalf("recordCreateReceipt: %v", err)
	}
	want := filepath.Join(xdg, "pi-stack", "sandboxes", "pi-stack-pathcheck", "mcp.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected receipt at %s: %v", want, err)
	}
	// And NOT doubled anywhere else.
	doubled := filepath.Join(xdg, "pi-stack", "pi-stack", "sandboxes", "pi-stack-pathcheck", "mcp.json")
	if _, err := os.Stat(doubled); err == nil {
		t.Fatalf("receipt written at a DOUBLED pi-stack path: %s", doubled)
	}
}

// --- execSbxMcpLoadAndRecord: ordering + gating -----------------------------

func TestExecSbxMcpLoadAndRecord_FailedLoadWritesNoReceipt(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	err := execSbxMcpLoadAndRecord(falseCmd(t), "pi-stack-loadfail", "slack")
	if err == nil {
		t.Fatal("want an error propagated from the failed load")
	}
	var rerr *receiptRecordError
	if errors.As(err, &rerr) {
		t.Fatalf("a failed load must surface its OWN error, not a receiptRecordError: %v", err)
	}
	if _, status, _ := readSandboxMCPReceipt(dir, "pi-stack-loadfail"); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent (no receipt on a failed load)", status)
	}
}

func TestExecSbxMcpLoadAndRecord_SuccessAppendsLoad(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	sandbox := "pi-stack-loadok"
	if err := execSbxMcpLoadAndRecord(trueCmd(t), sandbox, "slack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, status, err := readSandboxMCPReceipt(dir, sandbox)
	if err != nil || status != sandboxMCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Loads) != 1 || r.Loads[0].Name != "slack" {
		t.Fatalf("Loads = %+v, want [slack]", r.Loads)
	}
}

func TestExecSbxMcpLoadAndRecord_ReceiptWriteFailureIsDistinctError(t *testing.T) {
	withSandboxMCPStateDirFn(t, func() (string, error) { return "", errors.New("boom: no state dir") })

	err := execSbxMcpLoadAndRecord(trueCmd(t), "pi-stack-loadrecerr", "slack")
	if err == nil {
		t.Fatal("want an error when the receipt write fails")
	}
	var rerr *receiptRecordError
	if !errors.As(err, &rerr) {
		t.Fatalf("want a *receiptRecordError, got %T: %v", err, err)
	}
	if rerr.op != "mcp load" || rerr.sandbox != "pi-stack-loadrecerr" || rerr.name != "slack" {
		t.Errorf("receiptRecordError = %+v, want op=mcp load sandbox=pi-stack-loadrecerr name=slack", rerr)
	}
}

// runMcpLoad's "sbx absent" branch returns before constructing any *exec.Cmd
// at all, so it never reaches execSbxMcpLoadAndRecord — no receipt is ever
// written, and it never calls os.Exit (safe to call in-process).
func TestRunMcpLoad_AbsentSbxWritesNoReceiptAndDoesNotExit(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	t.Setenv("PATH", t.TempDir()) // nothing on PATH, including no sbx

	ws := t.TempDir()
	runMcpLoad([]string{"slack", ws}) // must return, not exit

	sandbox := deriveSandboxName(ws)
	if _, status, _ := readSandboxMCPReceipt(dir, sandbox); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent (sbx-absent branch must write nothing)", status)
	}
}

// --- exact sandbox derivation ------------------------------------------------

// The sandbox identity a load receipt is keyed under must be EXACTLY what
// deriveSandboxName(ws) derives — the same name `sbx mcp load --sandbox`
// targets — not a re-derivation that could drift (e.g. from cwd instead of the
// given DIR).
func TestRecordMcpLoadReceipt_ExactSandboxDerivation(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })

	ws := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox := deriveSandboxName(ws)
	if err := recordMcpLoadReceipt(sandbox, "slack"); err != nil {
		t.Fatal(err)
	}
	// Read back under the SAME derivation — proves the receipt is keyed by the
	// exact name `sbx mcp load --sandbox` would have targeted, e.g.
	// "pi-stack-my-project", not by cwd or the raw DIR string.
	if !strings.HasSuffix(sandbox, "my-project") {
		t.Fatalf("precondition: deriveSandboxName(%q) = %q, expected it to end in my-project", ws, sandbox)
	}
	r, status, err := readSandboxMCPReceipt(dir, deriveSandboxName(ws))
	if err != nil || status != sandboxMCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Loads) != 1 || r.Loads[0].Name != "slack" {
		t.Fatalf("Loads = %+v", r.Loads)
	}
}

// --- pack integrations fold into the emitted static set AND the receipt ----

// S03 item 4: an active pack's integration MCP servers already fold into
// cfg.MCP via applyPackToLaunch (S01); this pins that the run.go COMPUTATION
// run.go actually performs (allPreloadedMCP(cfg.MCP+o.MCP) -> o.StaticMCP,
// emitted as --static-mcp by buildSbxArgs) and the CREATE RECEIPT agree
// exactly on that same set, for both a persisted (`pack use`d) integration and
// a transient --pack override never persisted to cfg.MCP.
func TestPackIntegrations_FoldIntoStaticSetAndReceipt(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "transient-pack")
	mustWritePack(t, root, packManifest{Name: "transient", Schema: 1, Integrations: []packIntegration{
		{Name: "Fastmail", MCP: "fastmail"},
		{Name: "Notion", MCP: "notion"},
	}})

	cfg := defaultCfg()
	cfg.MCP = []string{"slack"} // an already-persisted, non-pack server
	o := runOpts{Pack: root}
	if _, err := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); err != nil {
		t.Fatalf("applyPackToLaunch: %v", err)
	}

	// The exact computation run.go performs before building sbx args / writing
	// the receipt.
	o.StaticMCP = allPreloadedMCP(append(append([]string(nil), cfg.MCP...), o.MCP...))

	for _, want := range []string{"slack", "fastmail", "notion"} {
		if !containsStr(o.StaticMCP, want) {
			t.Errorf("o.StaticMCP = %v, want it to contain %q", o.StaticMCP, want)
		}
	}

	// Emitted --static-mcp flags mirror o.StaticMCP exactly.
	args := buildSbxArgs(cfg, o, "0.0.99")
	for _, want := range o.StaticMCP {
		if !contains(args, []string{"--static-mcp", want}) {
			t.Errorf("buildSbxArgs args %v missing --static-mcp %s", args, want)
		}
	}

	// The receipt, once written, carries the SAME set byte-for-byte.
	stateDir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return stateDir, nil })
	if err := recordCreateReceipt("pi-stack-packfold", o.StaticMCP); err != nil {
		t.Fatal(err)
	}
	r, status, err := readSandboxMCPReceipt(stateDir, "pi-stack-packfold")
	if err != nil || status != sandboxMCPStateOK {
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
// call sites that may invoke writeCreateReceipt are run.go (the create path)
// and sandboxmcpstate.go (its own definition + doc comment), and the only call
// sites for appendLoadReceipt are mcp.go (the explicit `mcp load` path) and
// sandboxmcpstate.go. A future call site fabricating a receipt anywhere else
// (an "automatic load" on session start, a background watcher, ...) fails this
// test rather than being discovered later via a wrong doctor/status report.
func TestMCPReceiptCallSitesAreGuarded(t *testing.T) {
	assertOnlyCalledFrom(t, "writeCreateReceipt(", []string{"run.go", "sandboxmcpstate.go"})
	assertOnlyCalledFrom(t, "appendLoadReceipt(", []string{"mcp.go", "sandboxmcpstate.go"})
}

func assertOnlyCalledFrom(t *testing.T, needle string, allowed []string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allow := map[string]bool{}
	for _, a := range allowed {
		allow[a] = true
	}
	found := false
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue // tests are allowed to call these directly to set up fixtures
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), needle) {
			found = true
			if !allow[filepath.Base(f)] {
				t.Errorf("%s calls %s but is not an allowed call site (allowed: %v)", f, needle, allowed)
			}
		}
	}
	if !found {
		t.Fatalf("guard is stale: %s was not found in ANY source file", needle)
	}
}
