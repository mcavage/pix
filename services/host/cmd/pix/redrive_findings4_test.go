package main

// redrive_findings4_test.go — the final ship-review redrive (findings 1–5):
//
//  1. custom sandbox names: the create receipt records the canonical
//     Workspace, and the hardened workspace->sandbox resolver
//     (workspaceresolve.go) finds a custom-named box again from its DIR —
//     unique mapping wins, clean no-mapping falls back to the derived
//     default, ambiguous/corrupt NEVER targets an arbitrary box;
//  2. gog attachment comes from the shared receipt-backed join row, never
//     from config membership alone;
//  3. doctor's registered-not-attached is a verified optional TODO with the
//     exact `pix mcp load NAME <workspace>` (pinned in
//     doctor_mcp_test.go's TestMCPAttachmentFromReceipt);
//  4. `pix reset --sbx` clears each positively-removed sandbox's
//     receipt (and retains it on failure);
//  5. status's headline never reads "all systems go" over an unverifiable
//     MCP row.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pix/host/mcp"
	"pix/host/workspace"
)

// --- finding 1: the hardened workspace->sandbox resolver ---------------------

// mustCreateReceipt writes a full create receipt for sandbox with the given
// canonical workspace, via the production writer.
func mustCreateReceipt(t *testing.T, stateDir, sandbox, ws string, preloaded []string) {
	t.Helper()
	if err := workspace.WriteCreateReceipt(stateDir, sandbox, ws, preloaded, receiptClock); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWorkspaceSandbox_CustomNameMapping(t *testing.T) {
	stateDir := t.TempDir()
	ws := t.TempDir() // a REAL dir so canonicalization is exact
	canon := workspace.CanonicalPath(ws)
	mustCreateReceipt(t, stateDir, "pix-demo", canon, []string{"slack"})

	res := workspace.ResolveSandbox(stateDir, ws)
	if res.Outcome != workspace.SandboxMapped || res.Sandbox != "pix-demo" {
		t.Fatalf("resolution = %+v, want mapped -> pix-demo", res)
	}
}

func TestResolveWorkspaceSandbox_NoMappingFallsBackToDerived(t *testing.T) {
	stateDir := t.TempDir()
	ws := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("empty store", func(t *testing.T) {
		res := workspace.ResolveSandbox(stateDir, ws)
		if res.Outcome != workspace.SandboxDefault || res.Sandbox != workspace.DeriveSandboxName(ws) {
			t.Fatalf("resolution = %+v, want default -> %s", res, workspace.DeriveSandboxName(ws))
		}
	})

	t.Run("old sandbox: receipt without a ws field", func(t *testing.T) {
		// A receipt written before the Workspace field (or by an older
		// binary) maps nothing — the clean fallback is the derived name.
		mustCreateReceipt(t, stateDir, workspace.DeriveSandboxName(ws), "", []string{"slack"})
		res := workspace.ResolveSandbox(stateDir, ws)
		if res.Outcome != workspace.SandboxDefault || res.Sandbox != workspace.DeriveSandboxName(ws) {
			t.Fatalf("resolution = %+v, want default -> %s", res, workspace.DeriveSandboxName(ws))
		}
	})
}

func TestResolveWorkspaceSandbox_PathCanonicalization(t *testing.T) {
	stateDir := t.TempDir()
	ws := t.TempDir()
	canon := workspace.CanonicalPath(ws)
	mustCreateReceipt(t, stateDir, "pix-demo", canon, nil)

	// Every non-canonical spelling of the same directory must resolve to the
	// SAME mapping: a trailing slash, a ./ hop, and a parent round-trip.
	base := filepath.Base(ws)
	for _, spelling := range []string{
		ws + string(filepath.Separator),
		filepath.Join(ws, "."),
		filepath.Join(ws, "..", base),
	} {
		res := workspace.ResolveSandbox(stateDir, spelling)
		if res.Outcome != workspace.SandboxMapped || res.Sandbox != "pix-demo" {
			t.Errorf("spelling %q: resolution = %+v, want mapped -> pix-demo", spelling, res)
		}
	}
}

func TestResolveWorkspaceSandbox_AmbiguousRefuses(t *testing.T) {
	stateDir := t.TempDir()
	ws := t.TempDir()
	canon := workspace.CanonicalPath(ws)
	mustCreateReceipt(t, stateDir, "pix-a", canon, nil)
	mustCreateReceipt(t, stateDir, "pix-b", canon, nil)

	res := workspace.ResolveSandbox(stateDir, ws)
	if res.Outcome != workspace.SandboxAmbiguous {
		t.Fatalf("resolution = %+v, want ambiguous", res)
	}
	if res.Sandbox != "" {
		t.Fatalf("an ambiguous resolution must target NO sandbox, got %q", res.Sandbox)
	}
}

func TestResolveWorkspaceSandbox_TamperedStoreRefuses(t *testing.T) {
	ws := t.TempDir()
	canon := workspace.CanonicalPath(ws)

	writeRaw := func(t *testing.T, stateDir, box, contents string) {
		t.Helper()
		dir := filepath.Join(stateDir, "sandboxes", box)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("identity-mismatched receipt (moved between dirs)", func(t *testing.T) {
		stateDir := t.TempDir()
		// A receipt claiming the workspace, planted under a DIFFERENT box's
		// directory — the classic tamper. It must never resolve, and its
		// presence must poison the "no mapping" conclusion too.
		writeRaw(t, stateDir, "pix-demo",
			`{"schema":1,"sandbox":"pix-other","workspace":`+jsonStr(canon)+`}`)
		res := workspace.ResolveSandbox(stateDir, ws)
		if res.Outcome != workspace.WorkspaceSandboxUntrusted || res.Sandbox != "" {
			t.Fatalf("resolution = %+v, want untrusted with no target", res)
		}
	})

	t.Run("corrupt receipt anywhere fails closed", func(t *testing.T) {
		stateDir := t.TempDir()
		// Even with a clean unique match present, an unreadable receipt could
		// BE the competing mapping — untrusted, never "unique".
		mustCreateReceipt(t, stateDir, "pix-demo", canon, nil)
		writeRaw(t, stateDir, "pix-unrelated", "not json{")
		res := workspace.ResolveSandbox(stateDir, ws)
		if res.Outcome != workspace.WorkspaceSandboxUntrusted || res.Sandbox != "" {
			t.Fatalf("resolution = %+v, want untrusted with no target", res)
		}
	})

	t.Run("symlinked receipt directory fails closed", func(t *testing.T) {
		stateDir := t.TempDir()
		real := t.TempDir()
		if err := os.MkdirAll(filepath.Join(stateDir, "sandboxes"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(stateDir, "sandboxes", "pix-link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		res := workspace.ResolveSandbox(stateDir, ws)
		if res.Outcome != workspace.WorkspaceSandboxUntrusted {
			t.Fatalf("resolution = %+v, want untrusted (symlinked dir)", res)
		}
	})
}

// jsonStr marshals s as a JSON string literal for raw fixture assembly.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- finding 1: the create receipt records the canonical workspace ----------

func TestCreateReceiptRecordsWorkspace(t *testing.T) {
	stateDir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return stateDir, nil })
	withCreatePollSeams(t, probeAlways(sbxRunning), time.Millisecond, time.Second)

	ws := t.TempDir()
	canon := workspace.CanonicalPath(ws)
	if err := execSbxRunAndRecordCreate(trueCmd(t), true, "pix-demo", canon, []string{"slack"}); err != nil {
		t.Fatal(err)
	}
	r, status, err := workspace.ReadMCPReceipt(stateDir, "pix-demo")
	if err != nil || status != workspace.MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if r.Workspace != canon {
		t.Fatalf("receipt ws = %q, want %q", r.Workspace, canon)
	}
	// And it round-trips through the resolver: the exact flow behind
	// `run --name pix-demo` then `mcp load slack <workspace>`.
	if got, err := mcp.ResolveMcpLoadSandbox(ws); err != nil || got != "pix-demo" {
		t.Fatalf("mcp.ResolveMcpLoadSandbox = %q, %v; want pix-demo", got, err)
	}
}

func TestResolveMcpLoadSandbox_RefusalPaths(t *testing.T) {
	stateDir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return stateDir, nil })
	ws := t.TempDir()
	canon := workspace.CanonicalPath(ws)

	// Default fallback first (no receipts): the derived name, no error.
	if got, err := mcp.ResolveMcpLoadSandbox(ws); err != nil || got != workspace.DeriveSandboxName(ws) {
		t.Fatalf("clean fallback = %q, %v; want %s", got, err, workspace.DeriveSandboxName(ws))
	}

	// Ambiguous: two receipts claim the workspace -> refuse, never a target.
	mustCreateReceipt(t, stateDir, "pix-a", canon, nil)
	mustCreateReceipt(t, stateDir, "pix-b", canon, nil)
	if got, err := mcp.ResolveMcpLoadSandbox(ws); err == nil {
		t.Fatalf("ambiguous mapping must refuse, resolved %q", got)
	}

	// Unresolvable state dir: refuse (cannot prove no mapping exists).
	withSandboxMCPStateDirFn(t, func() (string, error) { return "", fmt.Errorf("no state dir") })
	if got, err := mcp.ResolveMcpLoadSandbox(ws); err == nil {
		t.Fatalf("unresolvable state dir must refuse, resolved %q", got)
	}
}

// --- finding 1: doctor's workspace context uses the same resolver -----------

// --- finding 2: gog attachment is the shared receipt-backed join row --------
