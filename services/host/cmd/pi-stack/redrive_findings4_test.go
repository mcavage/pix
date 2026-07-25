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
//     exact `pi-stack mcp load NAME <workspace>` (pinned in
//     doctor_mcp_test.go's TestMCPAttachmentFromReceipt);
//  4. `pi-stack reset --sbx` clears each positively-removed sandbox's
//     receipt (and retains it on failure);
//  5. status's headline never reads "all systems go" over an unverifiable
//     MCP row.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-stack/host/config"
)

// --- finding 1: the hardened workspace->sandbox resolver ---------------------

// mustCreateReceipt writes a full create receipt for sandbox with the given
// canonical workspace, via the production writer.
func mustCreateReceipt(t *testing.T, stateDir, sandbox, workspace string, preloaded []string) {
	t.Helper()
	if err := writeCreateReceipt(stateDir, sandbox, workspace, preloaded, receiptClock); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWorkspaceSandbox_CustomNameMapping(t *testing.T) {
	stateDir := t.TempDir()
	ws := t.TempDir() // a REAL dir so canonicalization is exact
	canon := canonicalWorkspacePath(ws)
	mustCreateReceipt(t, stateDir, "pi-stack-demo", canon, []string{"slack"})

	res := resolveWorkspaceSandbox(stateDir, ws)
	if res.Outcome != workspaceSandboxMapped || res.Sandbox != "pi-stack-demo" {
		t.Fatalf("resolution = %+v, want mapped -> pi-stack-demo", res)
	}
}

func TestResolveWorkspaceSandbox_NoMappingFallsBackToDerived(t *testing.T) {
	stateDir := t.TempDir()
	ws := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("empty store", func(t *testing.T) {
		res := resolveWorkspaceSandbox(stateDir, ws)
		if res.Outcome != workspaceSandboxDefault || res.Sandbox != deriveSandboxName(ws) {
			t.Fatalf("resolution = %+v, want default -> %s", res, deriveSandboxName(ws))
		}
	})

	t.Run("old sandbox: receipt without a workspace field", func(t *testing.T) {
		// A receipt written before the Workspace field (or by an older
		// binary) maps nothing — the clean fallback is the derived name.
		mustCreateReceipt(t, stateDir, deriveSandboxName(ws), "", []string{"slack"})
		res := resolveWorkspaceSandbox(stateDir, ws)
		if res.Outcome != workspaceSandboxDefault || res.Sandbox != deriveSandboxName(ws) {
			t.Fatalf("resolution = %+v, want default -> %s", res, deriveSandboxName(ws))
		}
	})
}

func TestResolveWorkspaceSandbox_PathCanonicalization(t *testing.T) {
	stateDir := t.TempDir()
	ws := t.TempDir()
	canon := canonicalWorkspacePath(ws)
	mustCreateReceipt(t, stateDir, "pi-stack-demo", canon, nil)

	// Every non-canonical spelling of the same directory must resolve to the
	// SAME mapping: a trailing slash, a ./ hop, and a parent round-trip.
	base := filepath.Base(ws)
	for _, spelling := range []string{
		ws + string(filepath.Separator),
		filepath.Join(ws, "."),
		filepath.Join(ws, "..", base),
	} {
		res := resolveWorkspaceSandbox(stateDir, spelling)
		if res.Outcome != workspaceSandboxMapped || res.Sandbox != "pi-stack-demo" {
			t.Errorf("spelling %q: resolution = %+v, want mapped -> pi-stack-demo", spelling, res)
		}
	}
}

func TestResolveWorkspaceSandbox_AmbiguousRefuses(t *testing.T) {
	stateDir := t.TempDir()
	ws := t.TempDir()
	canon := canonicalWorkspacePath(ws)
	mustCreateReceipt(t, stateDir, "pi-stack-a", canon, nil)
	mustCreateReceipt(t, stateDir, "pi-stack-b", canon, nil)

	res := resolveWorkspaceSandbox(stateDir, ws)
	if res.Outcome != workspaceSandboxAmbiguous {
		t.Fatalf("resolution = %+v, want ambiguous", res)
	}
	if res.Sandbox != "" {
		t.Fatalf("an ambiguous resolution must target NO sandbox, got %q", res.Sandbox)
	}
}

func TestResolveWorkspaceSandbox_TamperedStoreRefuses(t *testing.T) {
	ws := t.TempDir()
	canon := canonicalWorkspacePath(ws)

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
		writeRaw(t, stateDir, "pi-stack-demo",
			`{"schema":1,"sandbox":"pi-stack-other","workspace":`+jsonStr(canon)+`}`)
		res := resolveWorkspaceSandbox(stateDir, ws)
		if res.Outcome != workspaceSandboxUntrusted || res.Sandbox != "" {
			t.Fatalf("resolution = %+v, want untrusted with no target", res)
		}
	})

	t.Run("corrupt receipt anywhere fails closed", func(t *testing.T) {
		stateDir := t.TempDir()
		// Even with a clean unique match present, an unreadable receipt could
		// BE the competing mapping — untrusted, never "unique".
		mustCreateReceipt(t, stateDir, "pi-stack-demo", canon, nil)
		writeRaw(t, stateDir, "pi-stack-unrelated", "not json{")
		res := resolveWorkspaceSandbox(stateDir, ws)
		if res.Outcome != workspaceSandboxUntrusted || res.Sandbox != "" {
			t.Fatalf("resolution = %+v, want untrusted with no target", res)
		}
	})

	t.Run("symlinked receipt directory fails closed", func(t *testing.T) {
		stateDir := t.TempDir()
		real := t.TempDir()
		if err := os.MkdirAll(filepath.Join(stateDir, "sandboxes"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(stateDir, "sandboxes", "pi-stack-link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		res := resolveWorkspaceSandbox(stateDir, ws)
		if res.Outcome != workspaceSandboxUntrusted {
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
	canon := canonicalWorkspacePath(ws)
	if err := execSbxRunAndRecordCreate(trueCmd(t), true, "pi-stack-demo", canon, []string{"slack"}); err != nil {
		t.Fatal(err)
	}
	r, status, err := readSandboxMCPReceipt(stateDir, "pi-stack-demo")
	if err != nil || status != sandboxMCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if r.Workspace != canon {
		t.Fatalf("receipt workspace = %q, want %q", r.Workspace, canon)
	}
	// And it round-trips through the resolver: the exact flow behind
	// `run --name pi-stack-demo` then `mcp load slack <ws>`.
	if got, err := resolveMcpLoadSandbox(ws); err != nil || got != "pi-stack-demo" {
		t.Fatalf("resolveMcpLoadSandbox = %q, %v; want pi-stack-demo", got, err)
	}
}

func TestResolveMcpLoadSandbox_RefusalPaths(t *testing.T) {
	stateDir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return stateDir, nil })
	ws := t.TempDir()
	canon := canonicalWorkspacePath(ws)

	// Default fallback first (no receipts): the derived name, no error.
	if got, err := resolveMcpLoadSandbox(ws); err != nil || got != deriveSandboxName(ws) {
		t.Fatalf("clean fallback = %q, %v; want %s", got, err, deriveSandboxName(ws))
	}

	// Ambiguous: two receipts claim the workspace -> refuse, never a target.
	mustCreateReceipt(t, stateDir, "pi-stack-a", canon, nil)
	mustCreateReceipt(t, stateDir, "pi-stack-b", canon, nil)
	if got, err := resolveMcpLoadSandbox(ws); err == nil {
		t.Fatalf("ambiguous mapping must refuse, resolved %q", got)
	}

	// Unresolvable state dir: refuse (cannot prove no mapping exists).
	withSandboxMCPStateDirFn(t, func() (string, error) { return "", fmt.Errorf("no state dir") })
	if got, err := resolveMcpLoadSandbox(ws); err == nil {
		t.Fatalf("unresolvable state dir must refuse, resolved %q", got)
	}
}

// --- finding 1: doctor's workspace context uses the same resolver -----------

func TestDoctorContextResolvesCustomSandboxName(t *testing.T) {
	ws := t.TempDir()
	canon := canonicalWorkspacePath(ws)
	stateDir := t.TempDir()
	f := mcpFake()
	f.output["sbx ls"] = "pi-stack-demo  running  " + canon + "\n"
	env := f.env()
	env.getwd = func() (string, error) { return ws, nil }
	env.stateDir = func() (string, error) { return stateDir, nil }
	mustCreateReceipt(t, stateDir, "pi-stack-demo", canon, []string{"slack"})

	ctx := resolveMCPSandboxContext(env)
	if ctx.mode != mcpAttachReceipt || ctx.sandbox != "pi-stack-demo" {
		t.Fatalf("ctx = %+v, want receipt mode on pi-stack-demo (the mapped custom name)", ctx)
	}
	if ctx.workspace != canon {
		t.Fatalf("ctx.workspace = %q, want %q", ctx.workspace, canon)
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, ctx)
	c := findCheck(t, g, "slack attachment")
	if c.result() != verdictReady || !strings.Contains(c.detail, "pi-stack-demo") {
		t.Fatalf("attachment = %+v, want ready on the custom-named sandbox", c)
	}
}

func TestDoctorContextAmbiguousMappingIsUnverifiable(t *testing.T) {
	ws := t.TempDir()
	canon := canonicalWorkspacePath(ws)
	stateDir := t.TempDir()
	f := mcpFake()
	f.output["sbx ls"] = "pi-stack-a  running  " + canon + "\n"
	env := f.env()
	env.getwd = func() (string, error) { return ws, nil }
	env.stateDir = func() (string, error) { return stateDir, nil }
	mustCreateReceipt(t, stateDir, "pi-stack-a", canon, nil)
	mustCreateReceipt(t, stateDir, "pi-stack-b", canon, nil)

	ctx := resolveMCPSandboxContext(env)
	if ctx.mode != mcpAttachNone || ctx.note == "" {
		t.Fatalf("ctx = %+v, want no-attachment mode with an explanatory note", ctx)
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, ctx)
	if hasCheck(g, "slack attachment") {
		t.Fatalf("an ambiguous mapping must never emit a per-sandbox attachment check: %+v", g)
	}
	note := findCheck(t, g, "attachment")
	if note.result() != verdictUnverifiable || !strings.Contains(note.detail, "mapping unresolvable") {
		t.Fatalf("attachment note = %+v, want unverifiable naming the mapping problem", note)
	}
}

// --- finding 2: gog attachment is the shared receipt-backed join row --------

func TestGogAttachCheckUsesReceiptJoin(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}}
	const box = "pi-stack-proj"
	const ws = "/home/u/proj"

	receiptCtx := func(t *testing.T, preloaded []string) mcpSandboxContext {
		t.Helper()
		stateDir := t.TempDir()
		mustCreateReceipt(t, stateDir, box, ws, preloaded)
		receipt, status, err := readSandboxMCPReceipt(stateDir, box)
		if err != nil || status != sandboxMCPStateOK {
			t.Fatalf("status=%v err=%v", status, err)
		}
		return mcpSandboxContext{mode: mcpAttachReceipt, sandbox: box, workspace: ws, receipt: receipt, status: status}
	}

	t.Run("configured AFTER create -> registered-not-attached TODO, never ready", func(t *testing.T) {
		// The sandbox's complete valid receipt has NO gog entry: cfg naming
		// gog is intent, not attachment — a verified optional TODO with the
		// exact live-attach command.
		c := gogAttachCheck(cfg, receiptCtx(t, []string{"slack"}), mcpRegYes)
		if c.result() != verdictTodo {
			t.Fatalf("check = %+v, want a verified registered-not-attached todo", c)
		}
		if want := "pi-stack mcp load gog " + ws; c.todo != want {
			t.Fatalf("todo = %q, want %q", c.todo, want)
		}
	})

	t.Run("receipted preload -> ready", func(t *testing.T) {
		c := gogAttachCheck(cfg, receiptCtx(t, []string{"gog"}), mcpRegYes)
		if c.result() != verdictReady || !strings.Contains(c.detail, "preloaded by pi-stack at create") {
			t.Fatalf("check = %+v, want ready from the receipt's preload claim", c)
		}
	})

	t.Run("no sandbox context -> config membership is intent, never ready", func(t *testing.T) {
		c := gogAttachCheck(cfg, noCtx, mcpRegYes)
		if c.result() == verdictReady {
			t.Fatalf("check = %+v — config membership alone must never render ready", c)
		}
		if !c.note || !strings.Contains(c.detail, "intent") {
			t.Fatalf("check = %+v, want an intent-labeled note", c)
		}
	})
}

// --- finding 4: reset --sbx clears receipts on positive removal only --------

func TestResetSbxClearsReceiptsOnPositiveRemovalOnly(t *testing.T) {
	stateDir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return stateDir, nil })
	mustCreateReceipt(t, stateDir, "pi-stack-ok", "/w/ok", []string{"slack"})
	mustCreateReceipt(t, stateDir, "pi-stack-bad", "/w/bad", []string{"slack"})

	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			switch key {
			case "sbx ls":
				return "pi-stack-ok running\npi-stack-bad running\n", nil
			case "sbx rm -f pi-stack-ok":
				return "", nil
			case "sbx rm -f pi-stack-bad":
				return "", fmt.Errorf("sandbox busy")
			}
			return "", nil
		},
	}
	var out bytes.Buffer
	executeSbxReset(resetActions{RemoveSandboxes: true}, env, &out)

	// Positive success -> receipt cleared via the hardened helper.
	if _, status, _ := readSandboxMCPReceipt(stateDir, "pi-stack-ok"); status != sandboxMCPStateAbsent {
		t.Errorf("pi-stack-ok receipt status = %v, want absent after a successful rm", status)
	}
	// Failed removal -> receipt RETAINED (evidence discarded only on proof).
	if _, status, _ := readSandboxMCPReceipt(stateDir, "pi-stack-bad"); status != sandboxMCPStateOK {
		t.Errorf("pi-stack-bad receipt status = %v, want retained (ok) after a failed rm", status)
	}
}

// --- finding 5: status headline vs unverifiable MCP rows ---------------------

// statusReceiptEnv is fakeStatusEnv with the receipt stateDir seam wired and a
// single running pi-stack box, providers ready.
func statusReceiptEnv(t *testing.T, stateDir string) shellEnv {
	t.Helper()
	env := fakeStatusEnv()
	env.stateDir = func() (string, error) { return stateDir, nil }
	base := env.run
	env.run = func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == "sbx ls" {
			return "NAME STATUS\npi-stack-proj running\n", nil
		}
		if key == "sbx mcp ls" {
			return "gog\n", nil
		}
		return base(name, args...)
	}
	return env
}

func TestStatusHeadlineUnverifiableRows(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}}

	t.Run("provider-ready + valid preload receipt -> all systems go", func(t *testing.T) {
		stateDir := t.TempDir()
		mustCreateReceipt(t, stateDir, "pi-stack-proj", "/w/proj", []string{"gog"})
		env := statusReceiptEnv(t, stateDir)
		st := gatherStatus(cfg, "default", env)
		if len(st.Todos) != 0 {
			t.Fatalf("todos = %v, want none", st.Todos)
		}
		var out bytes.Buffer
		renderStatus(cfg, "default", env, &out, false)
		if !strings.Contains(out.String(), "all systems go") {
			t.Errorf("verified-ready rows should keep the green headline:\n%s", out.String())
		}
	})

	for name, plant := range map[string]func(t *testing.T, stateDir string){
		"corrupt receipt": func(t *testing.T, stateDir string) {
			dir := filepath.Join(stateDir, "sandboxes", "pi-stack-proj")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("not json{"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"absent receipt": func(t *testing.T, stateDir string) { /* nothing recorded */ },
	} {
		t.Run(name+" -> unverifiable row blocks all-systems-go without a false TODO", func(t *testing.T) {
			stateDir := t.TempDir()
			plant(t, stateDir)
			env := statusReceiptEnv(t, stateDir)
			st := gatherStatus(cfg, "default", env)
			if len(st.Todos) != 0 {
				t.Fatalf("an unverifiable row must not fabricate a TODO, got %v", st.Todos)
			}
			// JSON stays the row truth: the row itself reads unverifiable.
			found := false
			for _, r := range st.MCPRows {
				if r.Name == "gog" && r.Sandbox == "pi-stack-proj" && r.State == mcpJoinUnverifiable {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected an unverifiable gog row, got %+v", st.MCPRows)
			}
			var out bytes.Buffer
			renderStatus(cfg, "default", env, &out, false)
			s := out.String()
			if strings.Contains(s, "all systems go") {
				t.Errorf("unverifiable rows must prevent the all-systems-go headline:\n%s", s)
			}
			if !strings.Contains(s, "unverifiable") || !strings.Contains(s, "nothing outstanding") {
				t.Errorf("headline should say some checks are unverifiable without calling them failed:\n%s", s)
			}
		})
	}
}
