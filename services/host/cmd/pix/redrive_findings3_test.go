package main

// redrive_findings3_test.go — rereview redrive findings 3/4 + DX JSON
// finding 2:
//
//  3: status must read each discovered sandbox's receipt even when the
//     current cfg/pack MCP intent is empty — a receipt-only transient name
//     (a `run --pack` mix-in, or a since-switched pack's historical
//     integration) must still surface as a per-sandbox row (human + --json),
//     while the host-global summary (which only ever reflects current
//     cfg/pack intent) correctly stays empty.
//  4: doctor tracks sbx-on-PATH separately from a successful `sbx secret
//     ls`. When sbx is on PATH but the secret probe fails, `sbx mcp ls` must
//     still be attempted — the MCP/gog groups get the on-path truth (not the
//     secret-probe's success/failure) as their "sbx present" signal, so they
//     can render ready/todo instead of falsely degrading to "sbx
//     unavailable". Providers stay unverifiable (that probe genuinely
//     failed); PATH-absent still reads absent everywhere.
//  DX JSON #2: verdict=ready must mean verified working. A note-only check
//     must carry a TRUTHFUL verdict (ready for a confirmed positive fact,
//     unverifiable for "cannot verify"/"not configured"/anything else) —
//     result() must not blanket-override to ready just because note is set.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"pix/host/config"
)

// --- finding 3: per-sandbox MCP rows with an empty current intent ----------

func TestStatusMCPRowsEmptyCfgReceiptPreloaded(t *testing.T) {
	cfg := &config.Config{} // empty cfg.MCP, no pack — currentIntent is empty
	env, stateDir := statusMCPEnv(t, "pix-proj running /home/u/proj\n", "notion\n")
	if err := writeCreateReceipt(stateDir, "pix-proj", "", []string{"notion"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	st := gatherStatus(cfg, "default", env)

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
	if r.Name != "notion" || r.State != mcpJoinPreloaded || r.Sandbox != "pix-proj" {
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
	if err := appendLoadReceipt(stateDir, "pix-proj", "slack", receiptClock); err != nil {
		t.Fatal(err)
	}
	st := gatherStatus(cfg, "default", env)

	if len(st.MCPServers) != 0 {
		t.Errorf("host-global MCPServers = %+v, want empty", st.MCPServers)
	}
	if len(st.MCPRows) != 1 || st.MCPRows[0].Name != "slack" || st.MCPRows[0].State != mcpJoinLoaded {
		t.Fatalf("MCPRows = %+v, want 1 loaded slack row", st.MCPRows)
	}
}

// TestStatusMCPRowsEmptyCfgNoSandboxes: empty cfg.MCP AND zero discovered
// sandboxes must still degrade cleanly — no rows, no panic, host-global
// summary empty.
func TestStatusMCPRowsEmptyCfgNoSandboxes(t *testing.T) {
	cfg := &config.Config{}
	env, _ := statusMCPEnv(t, "other-box running\n", "")
	st := gatherStatus(cfg, "default", env)
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
func TestRunDoctor_SecretFailureMcpSuccess(t *testing.T) {
	const hostBin = "/usr/local/bin/pix-host"
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		hostBin: hostBin,
		output: map[string]string{
			// "sbx secret ls" deliberately ABSENT -> probeRun/run errors.
			"sbx mcp ls":                 "notion\n",
			"sbx mcp auth status notion": "notion: authorized\n",
			hostBin + " mcp --list":      "google-workspace\n", // notion is not local
		},
	}
	r := runDoctor(cfg, f.env())

	if r.sbxAbsent {
		t.Fatal("sbx IS on PATH — a failing `sbx secret ls` must not set sbxAbsent")
	}

	var modelKey check
	found := false
	for _, g := range r.groups {
		for _, c := range g.checks {
			if c.label == "model key" {
				modelKey, found = c, true
			}
		}
	}
	if !found {
		t.Fatal("model key check not found")
	}
	if modelKey.result() != verdictUnverifiable {
		t.Errorf("model key verdict = %q, want unverifiable (secret ls failed)", modelKey.result())
	}

	var notion check
	found = false
	for _, g := range r.groups {
		for _, c := range g.checks {
			if c.label == "notion" {
				notion, found = c, true
			}
		}
	}
	if !found {
		t.Fatal("notion check not found")
	}
	if notion.result() != verdictReady {
		t.Errorf("notion verdict = %q, want ready (mcp ls + auth status both succeeded)", notion.result())
	}
	if strings.Contains(strings.ToLower(notion.detail), "sbx unavailable") ||
		strings.Contains(strings.ToLower(notion.detail), "gateway") {
		t.Errorf("notion must not read as sbx-unavailable when only the SECRET probe failed: %+v", notion)
	}
}

// TestRunDoctor_SecretAndPathBothAbsent: converse control — sbx genuinely off
// PATH must still degrade both providers AND mcp/gog to their sbx-absent
// messaging (finding 4 must not weaken the true-absent case).
func TestRunDoctor_SecretAndPathBothAbsent(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}
	f := fakeEnv{present: map[string]bool{}}
	r := runDoctor(cfg, f.env())
	if !r.sbxAbsent {
		t.Fatal("sbx off PATH must set sbxAbsent")
	}
	for _, g := range r.groups {
		for _, c := range g.checks {
			if c.label == "notion" {
				if c.result() != verdictUnverifiable {
					t.Errorf("notion with sbx absent = %+v, want unverifiable", c)
				}
				if !strings.Contains(c.detail, "sbx unavailable") {
					t.Errorf("notion detail should say sbx unavailable, got %q", c.detail)
				}
			}
		}
	}
}

// --- DX JSON finding 2: verdict=ready must mean verified working -----------

// TestCheckResult_NoteDoesNotForceReady pins the core fix: result() reads the
// EXPLICIT verdict on a note check; it must not blanket-override to ready.
func TestCheckResult_NoteDoesNotForceReady(t *testing.T) {
	unset := check{label: "n", note: true, detail: "some annotation"}
	if got := unset.result(); got != verdictUnverifiable {
		t.Errorf("note with unset verdict = %q, want unverifiable (fail-safe default)", got)
	}
	// state() still renders as the info glyph regardless — a note never
	// claims ✓/✗/⚠, it's presentational — but the underlying VERDICT (JSON)
	// must be truthful.
	if unset.state() != stateInfo {
		t.Errorf("note state = %v, want stateInfo (presentation unaffected)", unset.state())
	}
	positive := check{label: "n", note: true, verdict: verdictReady, detail: "set"}
	if got := positive.result(); got != verdictReady {
		t.Errorf("note with explicit verdictReady = %q, want ready", got)
	}
	if positive.state() != stateInfo {
		t.Errorf("positive note state = %v, want stateInfo", positive.state())
	}
	// A note explicitly marked unverifiable must read that way too — the
	// point of the fix.
	negative := check{label: "n", note: true, verdict: verdictUnverifiable, detail: "cannot verify (sbx unavailable here)"}
	if got := negative.result(); got != verdictUnverifiable {
		t.Errorf("note with explicit verdictUnverifiable = %q, want unverifiable", got)
	}
}

// TestProviderInfoCheck_TruthfulVerdict: providerInfoCheck (the finding's
// named repro) must set ready only for a confirmed-set key, and unverifiable
// for both "cannot verify" and "not configured" — never a blanket ready.
func TestProviderInfoCheck_TruthfulVerdict(t *testing.T) {
	if c := providerInfoCheck("anthropic", "", false); c.result() != verdictUnverifiable {
		t.Errorf("cannot-verify provider info = %+v, want unverifiable", c)
	}
	if c := providerInfoCheck("anthropic", "anthropic\n", true); c.result() != verdictReady {
		t.Errorf("set provider info = %+v, want ready", c)
	}
	if c := providerInfoCheck("anthropic", "openai\n", true); c.result() != verdictUnverifiable {
		t.Errorf("not-configured provider info = %+v, want unverifiable", c)
	}
}

// TestDoctorInvariant_NoReadyEvidenceClaimsUnverified is the invariant test:
// across a battery of doctor states (cold, warm, secret-failure, absent
// account/CLI, ollama not installed, etc.), no check that reports verdict
// ready may carry evidence/detail suggesting it could not actually be
// verified. This is the exact bug class DX JSON finding 2 named: note:true
// forcing ready over "cannot verify"/"not configured" language.
func TestDoctorInvariant_NoReadyEvidenceClaimsUnverified(t *testing.T) {
	banned := []string{"cannot verify", "could not verify", "unavailable", "not configured", "missing", "not installed", "not present", "not found"}
	scan := func(t *testing.T, r *report) {
		t.Helper()
		for _, g := range r.groups {
			for _, c := range g.checks {
				if c.result() != verdictReady {
					continue
				}
				hay := strings.ToLower(c.evidenceString() + " " + c.detail)
				for _, b := range banned {
					if strings.Contains(hay, b) {
						t.Errorf("group %q check %q is verdict=ready but evidence/detail says %q: detail=%q evidence=%q",
							g.title, c.label, b, c.detail, c.evidence)
					}
				}
			}
		}
	}

	// Cold: everything absent.
	scan(t, runDoctor(defaultCfg(), fakeEnv{present: map[string]bool{}}.env()))

	// Warm-ish: sbx present, secrets set, mcp registered, ollama absent.
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	warm := fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			"sbx mcp ls":    "slack\n",
		},
	}
	scan(t, runDoctor(cfg, warm.env()))

	// Secret probe fails, mcp succeeds (this task's own finding-4 fixture).
	notionCfg := defaultCfg()
	notionCfg.MCP = []string{"notion"}
	scan(t, runDoctor(notionCfg, fakeEnv{
		present: map[string]bool{"sbx": true},
		hostBin: "/usr/local/bin/pix-host",
		output: map[string]string{
			"sbx mcp ls":                         "notion\n",
			"sbx mcp auth status notion":         "notion: authorized\n",
			"/usr/local/bin/pix-host mcp --list": "google-workspace\n",
		},
	}.env()))

	// No credentialed host MCP servers -> the 1Password "not needed" note.
	scan(t, runDoctor(defaultCfg(), fakeEnv{present: map[string]bool{}}.env()))
}

// TestDoctorJSON_NoteVerdictSerializesTruthfully: the --json payload must
// carry the SAME truthful verdict on a note check, not a blanket "ready".
func TestDoctorJSON_NoteVerdictSerializesTruthfully(t *testing.T) {
	r := &report{groups: []group{{title: "g", checks: []check{
		{label: "a", note: true, verdict: verdictUnverifiable, detail: "not configured"},
		{label: "b", note: true, verdict: verdictReady, detail: "set"},
	}}}}
	v := r.jsonView("")
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
	if r.outstanding() != 0 || r.blocking() {
		t.Errorf("notes must never count as outstanding or block: outstanding=%d blocking=%v", r.outstanding(), r.blocking())
	}
}
