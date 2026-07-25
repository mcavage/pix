package main

// status_mcp_s09_test.go — S09: truthful per-sandbox MCP status. The rows come
// from the SAME shared join path doctor uses (mcpjoin.go), backed by the
// launcher receipt (sandboxmcpstate.go) — never a guess, never an sbx
// per-sandbox inspect API (sbx has none: `sbx mcp get <name>` shows only the
// registered definition).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// statusMCPEnv is a hermetic shellEnv for the per-sandbox row tests: sbx on
// PATH, canned `sbx ls` / `sbx mcp ls` output, and a t.TempDir()-backed
// launcher state dir for receipts.
func statusMCPEnv(t *testing.T, sbxLs, mcpLs string) (shellEnv, string) {
	t.Helper()
	stateDir := t.TempDir()
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			switch strings.Join(append([]string{name}, args...), " ") {
			case "sbx ls":
				return sbxLs, nil
			case "sbx mcp ls":
				return mcpLs, nil
			case "sbx secret ls":
				return "anthropic\nopenai\ngoogle\ngithub\n", nil
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
		stateDir: func() (string, error) { return stateDir, nil },
	}
	return env, stateDir
}

func rowsFor(st statusReport, sandbox string) map[string]mcpSandboxRow {
	out := map[string]mcpSandboxRow{}
	for _, r := range st.MCPRows {
		if r.Sandbox == sandbox {
			out[r.Name] = r
		}
	}
	return out
}

// TestStatusMCPRowsAllFiveStates: one status snapshot exercises every join
// state across two sandboxes with DISTINCT receipts — preloaded, loaded,
// registered-not-attached, not-registered, and unverifiable (corrupt receipt).
func TestStatusMCPRowsAllFiveStates(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack", "notion", "linear"}}
	env, stateDir := statusMCPEnv(t,
		"NAME STATE DIR\npi-stack-proj running /home/u/proj\npi-stack-bad running /home/u/bad\n",
		"gog\nslack\nnotion\n") // linear positively not registered
	if err := writeCreateReceipt(stateDir, "pi-stack-proj", []string{"gog"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	if err := appendLoadReceipt(stateDir, "pi-stack-proj", "slack", receiptClock); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(stateDir, "sandboxes", "pi-stack-bad")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "mcp.json"), []byte("not json{"), 0o600); err != nil {
		t.Fatal(err)
	}

	st := gatherStatus(cfg, "default", env)
	if len(st.MCPRows) != 8 {
		t.Fatalf("MCPRows = %+v, want 4 servers x 2 sandboxes", st.MCPRows)
	}
	proj := rowsFor(st, "pi-stack-proj")
	for name, want := range map[string]string{
		"gog":    mcpJoinPreloaded,
		"slack":  mcpJoinLoaded,
		"notion": mcpJoinRegisteredNotAttached,
		"linear": mcpJoinNotRegistered,
	} {
		if proj[name].State != want {
			t.Errorf("pi-stack-proj %s state = %q, want %q", name, proj[name].State, want)
		}
	}
	bad := rowsFor(st, "pi-stack-bad")
	for _, name := range []string{"gog", "slack", "notion"} {
		r := bad[name]
		if r.State != mcpJoinUnverifiable || !strings.Contains(r.Evidence, "receipt corrupt") {
			t.Errorf("pi-stack-bad %s = %+v, want unverifiable on a corrupt receipt", name, r)
		}
	}
	// A distinct receipt per sandbox: proj's loaded slack must NOT leak into bad.
	if bad["slack"].State == mcpJoinLoaded {
		t.Errorf("pi-stack-bad slack = %+v — leaked pi-stack-proj's receipt", bad["slack"])
	}
	// Registration tri-state carried on every row.
	if proj["linear"].Registered != "no" || proj["gog"].Registered != "yes" {
		t.Errorf("registered tri-state wrong: linear=%q gog=%q", proj["linear"].Registered, proj["gog"].Registered)
	}
}

// TestStatusMCPRowsIdentityMismatch: a receipt for a DIFFERENT sandbox in this
// sandbox's directory is never trusted — unverifiable, not attached.
func TestStatusMCPRowsIdentityMismatch(t *testing.T) {
	cfg := &config.Config{MCP: []string{"slack"}}
	env, stateDir := statusMCPEnv(t, "pi-stack-proj running /home/u/proj\n", "slack\n")
	dir := filepath.Join(stateDir, "sandboxes", "pi-stack-proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stolen := `{"schema":1,"sandbox":"someone-else","preloaded":["slack"]}`
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(stolen), 0o600); err != nil {
		t.Fatal(err)
	}
	st := gatherStatus(cfg, "default", env)
	if len(st.MCPRows) != 1 {
		t.Fatalf("MCPRows = %+v, want 1", st.MCPRows)
	}
	r := st.MCPRows[0]
	if r.State != mcpJoinUnverifiable || !strings.Contains(r.Evidence, "identity-mismatch") {
		t.Errorf("row = %+v, want unverifiable naming identity-mismatch", r)
	}
}

// TestStatusMCPNotRegisteredDominatesStaleReceipt: a valid preload receipt for
// a server the gateway no longer registers renders not-registered (with the
// stale claim as evidence) plus the type-correct register guidance TODO.
func TestStatusMCPNotRegisteredDominatesStaleReceipt(t *testing.T) {
	cfg := &config.Config{MCP: []string{"slack"}}
	env, stateDir := statusMCPEnv(t, "pi-stack-proj running /home/u/proj\n", "gog\n") // slack deregistered
	env.hostBinary = func() (string, error) { return "/usr/local/bin/pi-stack-host", nil }
	env.probe = func(name string, args ...string) (string, bool, error) { return "slack\n", false, nil }
	if err := writeCreateReceipt(stateDir, "pi-stack-proj", []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	st := gatherStatus(cfg, "default", env)
	r := st.MCPRows[0]
	if r.State != mcpJoinNotRegistered || !strings.Contains(r.Evidence, "stale receipt claims preloaded") {
		t.Errorf("row = %+v, want not-registered dominating the stale receipt", r)
	}
	found := false
	for _, tdo := range st.Todos {
		if tdo == "pi-stack mcp register slack" {
			found = true
		}
	}
	if !found {
		t.Errorf("want the type-correct register TODO, got %v", st.Todos)
	}
}

// TestStatusMCPLoadTodoExactCommand: a registered-not-attached row gets the
// exact live-attach command — with the sandbox's dir when `sbx ls` showed it,
// the [DIR] placeholder otherwise.
func TestStatusMCPLoadTodoExactCommand(t *testing.T) {
	cfg := &config.Config{MCP: []string{"notion"}}

	t.Run("dir known", func(t *testing.T) {
		env, stateDir := statusMCPEnv(t, "pi-stack-proj running /home/u/proj\n", "notion\n")
		if err := writeCreateReceipt(stateDir, "pi-stack-proj", nil, receiptClock); err != nil {
			t.Fatal(err)
		}
		st := gatherStatus(cfg, "default", env)
		if !containsStr(st.Todos, "pi-stack mcp load notion /home/u/proj") {
			t.Errorf("want exact `pi-stack mcp load notion /home/u/proj` TODO, got %v", st.Todos)
		}
	})

	t.Run("dir unknown", func(t *testing.T) {
		env, stateDir := statusMCPEnv(t, "pi-stack-proj running\n", "notion\n")
		if err := writeCreateReceipt(stateDir, "pi-stack-proj", nil, receiptClock); err != nil {
			t.Fatal(err)
		}
		st := gatherStatus(cfg, "default", env)
		if !containsStr(st.Todos, "pi-stack mcp load notion [DIR]") {
			t.Errorf("want `pi-stack mcp load notion [DIR]` TODO, got %v", st.Todos)
		}
	})

	t.Run("unverifiable rows never add a load TODO", func(t *testing.T) {
		env, _ := statusMCPEnv(t, "pi-stack-proj running /home/u/proj\n", "notion\n") // no receipt at all
		st := gatherStatus(cfg, "default", env)
		for _, tdo := range st.Todos {
			if strings.Contains(tdo, "mcp load") {
				t.Errorf("unverifiable attachment must not surface a load TODO: %q", tdo)
			}
		}
		r := st.MCPRows[0]
		if r.State != mcpJoinUnverifiable ||
			!strings.Contains(r.Evidence, "pi-stack mcp load notion") ||
			!strings.Contains(r.Evidence, "pi-stack run --replace") {
			t.Errorf("row = %+v, want unverifiable with load/--replace guidance as EVIDENCE", r)
		}
	})
}

// TestStatusMCPDiscoveryUnavailableNotNoSandboxes: sbx on PATH but `sbx ls`
// failing must render unverifiable rows — never as if there were zero
// sandboxes (and never a receipt-backed claim without a discovered sandbox).
func TestStatusMCPDiscoveryUnavailableNotNoSandboxes(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack"}}
	env, _ := statusMCPEnv(t, "", "gog\nslack\n")
	inner := env.run
	env.run = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) == 1 && args[0] == "ls" {
			return "", fmt.Errorf("sbx daemon down")
		}
		return inner(name, args...)
	}
	st := gatherStatus(cfg, "default", env)
	if len(st.Sandboxes) != 0 {
		t.Fatalf("sandboxes = %+v, want none parsed", st.Sandboxes)
	}
	if len(st.MCPRows) != 2 {
		t.Fatalf("MCPRows = %+v, want one unverifiable row per configured server", st.MCPRows)
	}
	for _, r := range st.MCPRows {
		if r.State != mcpJoinUnverifiable || r.Sandbox != "" {
			t.Errorf("row = %+v, want unverifiable with no sandbox claim", r)
		}
		if !strings.Contains(r.Evidence, "sandbox discovery unavailable") {
			t.Errorf("evidence should name the discovery gap: %q", r.Evidence)
		}
		// Registration was still verifiable (its own listing succeeded).
		if r.Registered != "yes" {
			t.Errorf("registered = %q, want yes (mcp ls succeeded independently)", r.Registered)
		}
	}
	var out bytes.Buffer
	st.render(&out)
	if !strings.Contains(out.String(), "(sandboxes unknown)") {
		t.Errorf("human render should show sandboxes as unknown, not absent:\n%s", out.String())
	}
}

// TestStatusHostGlobalNoAttachmentClaim: with no pi-stack sandbox discovered,
// the host-global MCP summary states registration + preload intent only —
// no attachment vocabulary anywhere.
func TestStatusHostGlobalNoAttachmentClaim(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}}
	env, _ := statusMCPEnv(t, "other-box running\n", "gog\n") // zero pi-stack boxes
	st := gatherStatus(cfg, "default", env)
	if len(st.MCPRows) != 0 {
		t.Fatalf("MCPRows = %+v, want none (discovery succeeded, zero pi-stack sandboxes)", st.MCPRows)
	}
	var out bytes.Buffer
	st.render(&out)
	s := out.String()
	if !strings.Contains(s, "preloads at sandbox create") {
		t.Errorf("host-global summary must state the preload intent:\n%s", s)
	}
	for _, banned := range []string{"attach", "Attach"} {
		if strings.Contains(s, banned) {
			t.Errorf("host-global output must never claim attachment (%q):\n%s", banned, s)
		}
	}
}

// TestStatusMCPRowsJSONGolden pins the additive --json row schema:
// {name,registered,sandbox,state,evidence} with the registration tri-state as
// a string. A schema drift breaks this golden on purpose.
func TestStatusMCPRowsJSONGolden(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack", "notion", "linear"}}
	env, stateDir := statusMCPEnv(t, "pi-stack-proj running /home/u/proj\n", "gog\nslack\nnotion\n")
	if err := writeCreateReceipt(stateDir, "pi-stack-proj", []string{"gog"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	if err := appendLoadReceipt(stateDir, "pi-stack-proj", "slack", receiptClock); err != nil {
		t.Fatal(err)
	}
	st := gatherStatus(cfg, "default", env)
	got, err := json.MarshalIndent(st.MCPRows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden := `[
  {
    "name": "gog",
    "registered": "yes",
    "sandbox": "pi-stack-proj",
    "state": "preloaded",
    "evidence": "preloaded by pi-stack at create"
  },
  {
    "name": "slack",
    "registered": "yes",
    "sandbox": "pi-stack-proj",
    "state": "loaded",
    "evidence": "loaded by pi-stack"
  },
  {
    "name": "notion",
    "registered": "yes",
    "sandbox": "pi-stack-proj",
    "state": "registered-not-attached",
    "evidence": "no receipt entry; attach live with ` + "`pi-stack mcp load notion`" + ` or recreate with ` + "`pi-stack run --replace`" + `"
  },
  {
    "name": "linear",
    "registered": "no",
    "sandbox": "pi-stack-proj",
    "state": "not-registered",
    "evidence": "not in ` + "`sbx mcp ls`" + `"
  }
]`
	if string(got) != golden {
		t.Errorf("mcp_sandbox_rows JSON drifted:\n--- got ---\n%s\n--- want ---\n%s", got, golden)
	}
}

// TestStatusNoRetiredMCPVocabulary is the grep guard: neither the status/join
// SOURCE nor a representative rendered output may resurrect the retired
// attach-on-run / dynamic-discovery / mcp-find vocabulary.
func TestStatusNoRetiredMCPVocabulary(t *testing.T) {
	banned := []string{"attach_on_run", "attach-on-run", "mcp-find", "dynamic"}
	for _, src := range []string{"status.go", "mcpjoin.go"} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		lower := strings.ToLower(string(b))
		for _, bad := range banned {
			if strings.Contains(lower, bad) {
				t.Errorf("%s carries retired vocabulary %q", src, bad)
			}
		}
	}
	cfg := &config.Config{MCP: []string{"gog", "slack", "notion", "linear"}}
	env, stateDir := statusMCPEnv(t, "pi-stack-proj running /home/u/proj\n", "gog\nslack\nnotion\n")
	if err := writeCreateReceipt(stateDir, "pi-stack-proj", []string{"gog"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	for _, jsonOut := range []bool{false, true} {
		var out bytes.Buffer
		renderStatus(cfg, "default", env, &out, jsonOut)
		lower := strings.ToLower(out.String())
		for _, bad := range banned {
			if strings.Contains(lower, bad) {
				t.Errorf("rendered output (json=%v) carries retired vocabulary %q:\n%s", jsonOut, bad, out.String())
			}
		}
	}
}
