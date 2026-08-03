// Moved from cmd/pix: the subject is a doctor internal.
package doctor

import (
	"bytes"
	"encoding/json"
	"pix/host/config"
	"pix/host/mcp"
	"pix/host/readiness"
	"pix/host/workspace"
	"strings"
	"testing"
)

func TestStatusMCPRowsEmptyCfgReceiptPreloaded(t *testing.T) {
	cfg := &config.Config{} // empty cfg.MCP, no pack — currentIntent is empty
	env, stateDir := statusMCPEnv(t, "pix-proj running /home/u/proj\n", "notion\n")
	if err := workspace.WriteCreateReceipt(stateDir, "pix-proj", "", []string{"notion"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	st := GatherStatus(cfg, "default", env)

	if len(st.MCP) != 0 {
		t.Errorf("current-intent MCP = %v, want empty (nothing configured)", st.MCP)
	}
	if len(st.MCPServers) != 0 {
		t.Errorf("host-global MCPServers = %+v, want empty (current cfg/pack intent is empty)", st.MCPServers)
	}
	if len(st.MCPRows) != 1 {
		t.Fatalf("MCPRows = %+v, want 1 receipt-only row even with empty cfg.MCP", st.MCPRows)
	}
	r := st.MCPRows[0]
	if r.Name != "notion" || r.State != mcp.McpJoinPreloaded || r.Sandbox != "pix-proj" {
		t.Errorf("row = %+v, want preloaded notion on pix-proj", r)
	}
	if !strings.Contains(r.Evidence, "sandbox provenance only") {
		t.Errorf("evidence should label notion as sandbox provenance (not current intent): %q", r.Evidence)
	}

	var human bytes.Buffer
	st.render(&human)
	if !strings.Contains(human.String(), "1 ready") || !strings.Contains(human.String(), "available in 1 sandbox") {
		t.Errorf("human render must summarize the receipt-only integration:\n%s", human.String())
	}
	got, err := json.Marshal(st.MCPRows)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"notion"`) {
		t.Errorf("json mcp_sandbox_rows must carry notion: %s", got)
	}
}

func TestStatusMCPRowsEmptyCfgReceiptLoaded(t *testing.T) {
	cfg := &config.Config{}
	env, stateDir := statusMCPEnv(t, "pix-proj running /home/u/proj\n", "slack\n")
	if err := workspace.AppendLoadReceipt(stateDir, "pix-proj", "slack", receiptClock); err != nil {
		t.Fatal(err)
	}
	st := GatherStatus(cfg, "default", env)

	if len(st.MCPServers) != 0 {
		t.Errorf("host-global MCPServers = %+v, want empty", st.MCPServers)
	}
	if len(st.MCPRows) != 1 || st.MCPRows[0].Name != "slack" || st.MCPRows[0].State != mcp.McpJoinLoaded {
		t.Fatalf("MCPRows = %+v, want 1 loaded slack row", st.MCPRows)
	}
}

// TestStatusMCPRowsEmptyCfgNoSandboxes: empty cfg.MCP AND zero discovered
// sandboxes must still degrade cleanly — no rows, no panic, host-global
// summary empty.

// TestStatusMCPRowsEmptyCfgNoSandboxes: empty cfg.MCP AND zero discovered
// sandboxes must still degrade cleanly — no rows, no panic, host-global
// summary empty.
func TestStatusMCPRowsEmptyCfgNoSandboxes(t *testing.T) {
	cfg := &config.Config{}
	env, _ := statusMCPEnv(t, "other-box running\n", "")
	st := GatherStatus(cfg, "default", env)
	if len(st.MCPRows) != 0 {
		t.Errorf("MCPRows = %+v, want none (nothing configured, no pix sandboxes)", st.MCPRows)
	}
	if len(st.MCPServers) != 0 {
		t.Errorf("MCPServers = %+v, want empty", st.MCPServers)
	}
}

// --- finding 4: sbx-on-PATH tracked independently of `sbx secret ls` -------

// TestRunDoctor_SecretFailureMcpSuccess: `sbx secret ls` fails but sbx IS on
// PATH and `sbx mcp ls` succeeds. Providers stays unverifiable (its own probe
// genuinely failed); the MCP group must still render from the successful
// `sbx mcp ls` — never falsely degrade to "sbx unavailable".

// TestCheckResult_NoteDoesNotForceReady pins the core fix: result() reads the
// EXPLICIT verdict on a note check; it must not blanket-override to ready.
func TestCheckResult_NoteDoesNotForceReady(t *testing.T) {
	unset := readiness.Check{Label: "n", Note: true, Detail: "some annotation"}
	if got := unset.Result(); got != readiness.VerdictUnverifiable {
		t.Errorf("note with unset verdict = %q, want unverifiable (fail-safe default)", got)
	}
	// state() still renders as the info glyph regardless — a note never
	// claims ✓/✗/⚠, it's presentational — but the underlying VERDICT (JSON)
	// must be truthful.
	if unset.State() != readiness.StateInfo {
		t.Errorf("note state = %v, want readiness.StateInfo (presentation unaffected)", unset.State())
	}
	positive := readiness.Check{Label: "n", Note: true, Verdict: readiness.VerdictReady, Detail: "set"}
	if got := positive.Result(); got != readiness.VerdictReady {
		t.Errorf("note with explicit readiness.VerdictReady = %q, want ready", got)
	}
	if positive.State() != readiness.StateInfo {
		t.Errorf("positive note state = %v, want readiness.StateInfo", positive.State())
	}
	// A note explicitly marked unverifiable must read that way too — the
	// point of the fix.
	negative := readiness.Check{Label: "n", Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "cannot verify (sbx unavailable here)"}
	if got := negative.Result(); got != readiness.VerdictUnverifiable {
		t.Errorf("note with explicit readiness.VerdictUnverifiable = %q, want unverifiable", got)
	}
}

// TestProviderInfoCheck_TruthfulVerdict: providerInfoCheck (the finding's
// named repro) must set ready only for a confirmed-set key, and unverifiable
// for both "cannot verify" and "not configured" — never a blanket ready.

// TestProviderInfoCheck_TruthfulVerdict: providerInfoCheck (the finding's
// named repro) must set ready only for a confirmed-set key, and unverifiable
// for both "cannot verify" and "not configured" — never a blanket ready.
func TestProviderInfoCheck_TruthfulVerdict(t *testing.T) {
	if c := providerInfoCheck("anthropic", "", false); c.Result() != readiness.VerdictUnverifiable {
		t.Errorf("cannot-verify provider info = %+v, want unverifiable", c)
	}
	if c := providerInfoCheck("anthropic", "anthropic\n", true); c.Result() != readiness.VerdictReady {
		t.Errorf("set provider info = %+v, want ready", c)
	}
	if c := providerInfoCheck("anthropic", "openai\n", true); c.Result() != readiness.VerdictUnverifiable {
		t.Errorf("not-configured provider info = %+v, want unverifiable", c)
	}
}

// TestDoctorInvariant_NoReadyEvidenceClaimsUnverified is the invariant test:
// across a battery of doctor states (cold, warm, secret-failure, absent
// account/CLI, ollama not installed, etc.), no check that reports verdict
// ready may carry evidence/detail suggesting it could not actually be
// verified. This is the exact bug class DX JSON finding 2 named: note:true
// forcing ready over "cannot verify"/"not configured" language.

// TestDoctorJSON_NoteVerdictSerializesTruthfully: the --json payload must
// carry the SAME truthful verdict on a note check, not a blanket "ready".
func TestDoctorJSON_NoteVerdictSerializesTruthfully(t *testing.T) {
	r := &readiness.Report{Groups: []readiness.Group{{Title: "g", Checks: []readiness.Check{
		{Label: "a", Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "not configured"},
		{Label: "b", Note: true, Verdict: readiness.VerdictReady, Detail: "set"},
	}}}}
	v := JsonView(r, "")
	byLabel := map[string]doctorCheckJSON{}
	for _, c := range v.Groups[0].Checks {
		byLabel[c.Label] = c
	}
	if byLabel["a"].Verdict != "unverifiable" {
		t.Errorf(`note "a" verdict = %q, want "unverifiable"`, byLabel["a"].Verdict)
	}
	if byLabel["a"].State != "info" {
		t.Errorf(`note "a" state = %q, want "info" (presentation unaffected)`, byLabel["a"].State)
	}
	if byLabel["b"].Verdict != "ready" {
		t.Errorf(`note "b" verdict = %q, want "ready"`, byLabel["b"].Verdict)
	}
	// Notes never block/count as outstanding regardless of verdict.
	if r.Outstanding() != 0 || r.Blocking() {
		t.Errorf("notes must never count as outstanding or block: outstanding=%d blocking=%v", r.Outstanding(), r.Blocking())
	}
}
