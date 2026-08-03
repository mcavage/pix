// Moved from cmd/pix: the subject is a doctor internal.
package doctor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/readiness"
	"pix/host/sys/systest"
	"pix/host/workflow/reset"
	"pix/host/workspace"
	"strings"
	"testing"
)

func TestGogAttachCheckUsesReceiptJoin(t *testing.T) {
	cfg := &config.Config{MCP: []string{config.GWServerName}}
	const box = "pix-proj"
	const ws = "/home/u/proj"

	receiptCtx := func(t *testing.T, preloaded []string) mcpSandboxContext {
		t.Helper()
		stateDir := t.TempDir()
		mustCreateReceipt(t, stateDir, box, ws, preloaded)
		receipt, status, err := workspace.ReadMCPReceipt(stateDir, box)
		if err != nil || status != workspace.MCPStateOK {
			t.Fatalf("status=%v err=%v", status, err)
		}
		return mcpSandboxContext{mode: mcpAttachReceipt, sandbox: box, ws: ws, receipt: receipt, status: status}
	}

	t.Run("configured AFTER create -> registered-not-attached TODO, never ready", func(t *testing.T) {
		// The sandbox's complete valid receipt has NO gog entry: cfg naming
		// gog is intent, not attachment — a verified optional TODO with the
		// exact live-attach command.
		c := gogAttachCheck(cfg, receiptCtx(t, []string{"slack"}), mcp.McpRegYes)
		if c.Result() != readiness.VerdictTodo {
			t.Fatalf("check = %+v, want a verified registered-not-attached todo", c)
		}
		if want := "pix mcp load google-workspace " + ws; c.Todo != want {
			t.Fatalf("todo = %q, want %q", c.Todo, want)
		}
	})

	t.Run("receipted preload -> ready", func(t *testing.T) {
		c := gogAttachCheck(cfg, receiptCtx(t, []string{config.GWServerName}), mcp.McpRegYes)
		if c.Result() != readiness.VerdictReady || !strings.Contains(c.Detail, "preloaded by pix at create") {
			t.Fatalf("check = %+v, want ready from the receipt's preload claim", c)
		}
	})

	t.Run("no sandbox context -> config membership is intent, never ready", func(t *testing.T) {
		c := gogAttachCheck(cfg, noCtx, mcp.McpRegYes)
		if c.Result() == readiness.VerdictReady {
			t.Fatalf("check = %+v — config membership alone must never render ready", c)
		}
		if !c.Note || !strings.Contains(c.Detail, "intent") {
			t.Fatalf("check = %+v, want an intent-labeled note", c)
		}
	})
}

// --- finding 4: reset --sbx clears receipts on positive removal only --------

func TestResetSbxClearsReceiptsOnPositiveRemovalOnly(t *testing.T) {
	stateDir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return stateDir, nil })
	mustCreateReceipt(t, stateDir, "pix-ok", "/w/ok", []string{"slack"})
	mustCreateReceipt(t, stateDir, "pix-bad", "/w/bad", []string{"slack"})

	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		switch key {
		case "sbx ls":
			return "pix-ok running\npix-bad running\n", nil
		case "sbx rm -f pix-ok":
			return "", nil
		case "sbx rm -f pix-bad":
			return "", fmt.Errorf("sandbox busy")
		}
		return "", nil
	}}}
	var out bytes.Buffer
	reset.ExecuteSbxReset(reset.Actions{RemoveSandboxes: true}, env, &out)

	// Positive success -> receipt cleared via the hardened helper.
	if _, status, _ := workspace.ReadMCPReceipt(stateDir, "pix-ok"); status != workspace.MCPStateAbsent {
		t.Errorf("pix-ok receipt status = %v, want absent after a successful rm", status)
	}
	// Failed removal -> receipt RETAINED (evidence discarded only on proof).
	if _, status, _ := workspace.ReadMCPReceipt(stateDir, "pix-bad"); status != workspace.MCPStateOK {
		t.Errorf("pix-bad receipt status = %v, want retained (ok) after a failed rm", status)
	}
}

// --- finding 5: status headline vs unverifiable MCP rows ---------------------

// statusReceiptEnv is fakeStatusEnv with the receipt stateDir seam wired and a
// single running pix box, providers ready.

// statusReceiptEnv is fakeStatusEnv with the receipt stateDir seam wired and a
// single running pix box, providers ready.
func statusReceiptEnv(t *testing.T, stateDir string) hostenv.Env {
	t.Helper()
	env := fakeStatusEnv()
	systest.Of(env.System).StateDirFn = func() (string, error) { return stateDir, nil }
	base := systest.Of(env.System).RunFn
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == "sbx ls" {
			return "NAME STATUS\npix-proj running\n", nil
		}
		if key == "sbx mcp ls" {
			return "google-workspace\n", nil
		}
		return base(name, args...)
	}
	return env
}

func TestStatusHeadlineUnverifiableRows(t *testing.T) {
	cfg := &config.Config{MCP: []string{config.GWServerName}}

	t.Run("provider-ready + valid preload receipt -> all systems go", func(t *testing.T) {
		stateDir := t.TempDir()
		mustCreateReceipt(t, stateDir, "pix-proj", "/w/proj", []string{config.GWServerName})
		env := statusReceiptEnv(t, stateDir)
		st := GatherStatus(cfg, "default", env)
		if len(st.Todos) != 0 {
			t.Fatalf("todos = %v, want none", st.Todos)
		}
		var out bytes.Buffer
		RenderStatus(cfg, "default", env, &out, false)
		if !strings.Contains(out.String(), "all systems go") {
			t.Errorf("verified-ready rows should keep the green headline:\n%s", out.String())
		}
	})

	for name, plant := range map[string]func(t *testing.T, stateDir string){
		"corrupt receipt": func(t *testing.T, stateDir string) {
			dir := filepath.Join(stateDir, "sandboxes", "pix-proj")
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
			st := GatherStatus(cfg, "default", env)
			if len(st.Todos) != 0 {
				t.Fatalf("an unverifiable row must not fabricate a TODO, got %v", st.Todos)
			}
			// JSON stays the row truth: the row itself reads unverifiable.
			found := false
			for _, r := range st.MCPRows {
				if r.Name == config.GWServerName && r.Sandbox == "pix-proj" && r.State == mcp.McpJoinUnverifiable {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected an unverifiable gog row, got %+v", st.MCPRows)
			}
			var out bytes.Buffer
			RenderStatus(cfg, "default", env, &out, false)
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
