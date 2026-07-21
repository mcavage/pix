package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"pi-stack/host/monitor"
)

// mkEvent* helpers build synthetic monitor.Event values. The embedded
// Envelope field (`env`) is unexported inside package monitor, so we can't
// set it via a keyed composite literal from here — but its EXPORTED
// promoted fields (TurnID, Seq, ...) are assignable via plain field
// assignment (Go embedding promotion doesn't require the embedding field's
// own name to be exported), which is all these tests need.

func mkProviderRequest(turnID, model, sysHash string, sysBytes, toolCount, estTokens int, newMsgs []monitor.MessageSummary) monitor.ProviderRequest {
	e := monitor.ProviderRequest{
		Model: model,
		Summary: monitor.RequestSummary{
			SystemPromptHash:  sysHash,
			SystemPromptBytes: sysBytes,
			NewMessages:       newMsgs,
			ToolCount:         toolCount,
			EstTokens:         estTokens,
		},
	}
	e.TurnID = turnID
	return e
}

func mkProviderResponse(turnID string, status int, stopReason string, usage *monitor.UsageSummary) monitor.ProviderResponse {
	e := monitor.ProviderResponse{Status: status, StopReason: stopReason, Usage: usage}
	e.TurnID = turnID
	return e
}

func mkToolStart(turnID, toolID, source, name, argsSummary, argsHash string) monitor.ToolStart {
	e := monitor.ToolStart{ToolID: toolID, Source: source, Name: name, ArgsSummary: argsSummary, ArgsHash: argsHash}
	e.TurnID = turnID
	return e
}

func mkToolEnd(turnID, toolID string, ok bool, resultBytes int, resultSummary, resultHash string, durationMs int) monitor.ToolEnd {
	e := monitor.ToolEnd{ToolID: toolID, OK: ok, ResultBytes: resultBytes, ResultSummary: resultSummary, ResultHash: resultHash, DurationMs: durationMs}
	e.TurnID = turnID
	return e
}

func mkContextEvent(turnID string, seq uint64, ctxKind, detail string) monitor.ContextEvent {
	e := monitor.ContextEvent{CtxKind: ctxKind, Detail: detail}
	e.TurnID = turnID
	e.Seq = seq
	return e
}

// feed runs one event through Update via the real eventMsg path (exactly
// what the tea.Cmd bridge produces), returning the updated Model. It never
// touches a real tea.Program — Update/View stay pure and testable per
// architecture.md 3.B.
func feed(t *testing.T, m Model, e monitor.Event) Model {
	t.Helper()
	next, _ := m.Update(eventMsg{event: e})
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return nm
}

func key(t *testing.T, m Model, k tea.KeyMsg) Model {
	t.Helper()
	next, _ := m.Update(k)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return nm
}

func runeKey(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// --- provider_request row: sys bytes + msg delta + tool count ---

func TestMonitorTUI_ProviderRequestRow(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkProviderRequest("12", "opus-4-8", "hash-a", 41984, 14, 38000,
		[]monitor.MessageSummary{{Role: "user", Bytes: 40, Hash: "m1", Preview: "hi"}}))

	view := m.View()
	if !strings.Contains(view, "turn 12") {
		t.Errorf("View() missing turn id, got:\n%s", view)
	}
	if !strings.Contains(view, "opus-4-8") {
		t.Errorf("View() missing model, got:\n%s", view)
	}
	if !strings.Contains(view, "sys=41.0KB") {
		t.Errorf("View() missing sys bytes, got:\n%s", view)
	}
	if !strings.Contains(view, "msgs=+1") {
		t.Errorf("View() missing msg delta, got:\n%s", view)
	}
	if !strings.Contains(view, "tools=14") {
		t.Errorf("View() missing tool count, got:\n%s", view)
	}
	// First time this hash is seen: not "(unchanged)".
	if strings.Contains(view, "(unchanged)") {
		t.Errorf("View() marked first-seen sys hash as unchanged, got:\n%s", view)
	}
}

func TestMonitorTUI_ProviderRequestRow_UnchangedSysHash(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkProviderRequest("1", "opus-4-8", "same-hash", 1000, 1, 100, nil))
	m = feed(t, m, mkProviderRequest("2", "opus-4-8", "same-hash", 1000, 1, 100, nil))

	view := m.View()
	if !strings.Contains(view, "sys=1000B(unchanged)") {
		t.Errorf("View() missing unchanged sys marker on second turn, got:\n%s", view)
	}
	// Turn 1's row should still show up too (not overwritten).
	if !strings.Contains(view, "turn 1 ") {
		t.Errorf("View() missing turn 1 row, got:\n%s", view)
	}
}

// --- toggling showFull expands blob text via a fake Blob func ---

func TestMonitorTUI_ShowFullExpandsBlobText(t *testing.T) {
	blob := monitor.Blob{Hash: "sys-hash-1", Bytes: 20, Text: "SYSTEM PROMPT BODY TEXT"}
	fakeBlob := func(hash string) (monitor.Blob, bool) {
		if hash == blob.Hash {
			return blob, true
		}
		return monitor.Blob{}, false
	}
	m := NewModel(TUIConfig{Blob: fakeBlob})
	m = feed(t, m, mkProviderRequest("5", "sonnet-5", blob.Hash, 20, 0, 10, nil))

	// Not expanded yet: body text must not appear.
	if strings.Contains(m.View(), blob.Text) {
		t.Fatalf("blob text visible before row was expanded")
	}

	// space expands the (only, most recent) row.
	m = key(t, m, runeKey(" "))
	// showFull still off: body must not appear even though the row is expanded.
	if strings.Contains(m.View(), blob.Text) {
		t.Fatalf("blob text visible with showFull off, got:\n%s", m.View())
	}

	// f toggles showFull on; now expanded + full => body text shows.
	m = key(t, m, runeKey("f"))
	view := m.View()
	if !strings.Contains(view, blob.Text) {
		t.Errorf("View() missing expanded blob text, got:\n%s", view)
	}
}

func TestMonitorTUI_ShowFullMissingBlob(t *testing.T) {
	fakeBlob := func(string) (monitor.Blob, bool) { return monitor.Blob{}, false }
	m := NewModel(TUIConfig{Blob: fakeBlob})
	m = feed(t, m, mkProviderRequest("5", "sonnet-5", "gone-hash", 20, 0, 10, nil))
	m = key(t, m, runeKey("f"))
	m = key(t, m, runeKey(" "))

	view := m.View()
	if !strings.Contains(view, "(body not captured)") {
		t.Errorf("View() missing '(body not captured)' fallback, got:\n%s", view)
	}
}

// --- f:full + expand resolves message bodies and the tool schema (R2-6) ---

func TestMonitorTUI_ShowFullResolvesMessageAndToolSchemaBodies(t *testing.T) {
	msgBlob := monitor.Blob{Hash: "msg-hash-1", Bytes: 11, Text: "FULL MESSAGE BODY"}
	schemaBlob := monitor.Blob{Hash: "schema-hash-1", Bytes: 30, Text: "TOOL SCHEMA JSON BODY"}
	fakeBlob := func(hash string) (monitor.Blob, bool) {
		switch hash {
		case msgBlob.Hash:
			return msgBlob, true
		case schemaBlob.Hash:
			return schemaBlob, true
		}
		return monitor.Blob{}, false
	}
	m := NewModel(TUIConfig{Blob: fakeBlob})
	req := mkProviderRequest("11", "opus-4-8", "sys-hash", 500, 3, 1000,
		[]monitor.MessageSummary{{Role: "user", Bytes: 18, Hash: msgBlob.Hash, Preview: "hi there"}})
	req.Summary.ToolSchemaHash = schemaBlob.Hash
	m = feed(t, m, req)

	// Before the expanding+showFull Update, neither body must appear.
	if strings.Contains(m.View(), msgBlob.Text) || strings.Contains(m.View(), schemaBlob.Text) {
		t.Fatalf("full bodies visible before expand+showFull, got:\n%s", m.View())
	}

	m = key(t, m, runeKey("f")) // showFull on
	m = key(t, m, runeKey(" ")) // expand — this Update resolves the blobs

	view := m.View()
	if !strings.Contains(view, msgBlob.Text) {
		t.Errorf("View() missing resolved message body, got:\n%s", view)
	}
	if !strings.Contains(view, schemaBlob.Text) {
		t.Errorf("View() missing resolved tool-schema body, got:\n%s", view)
	}
}

// --- `/` filter hides non-matching rows ---

func TestMonitorTUI_FilterHidesNonMatchingRows(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("1", "t1", "builtin", "bash", "go test ./...", "a1"))
	m = feed(t, m, mkToolEnd("1", "t1", true, 2150, "ok", "r1", 4300))
	m = feed(t, m, mkToolStart("1", "t2", "mcp:slack", "slack_post", "channel:#eng", "a2"))
	m = feed(t, m, mkToolEnd("1", "t2", true, 118, "ok", "r2", 600))

	// Before filtering both tool rows are visible.
	before := m.View()
	if !strings.Contains(before, "bash") || !strings.Contains(before, "slack_post") {
		t.Fatalf("expected both tool rows before filtering, got:\n%s", before)
	}

	m = key(t, m, runeKey("/"))
	for _, r := range "bash" {
		m = key(t, m, runeKey(string(r)))
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	after := m.View()
	if !strings.Contains(after, "bash") {
		t.Errorf("filtered View() missing matching row, got:\n%s", after)
	}
	if strings.Contains(after, "slack_post") {
		t.Errorf("filtered View() still shows non-matching row, got:\n%s", after)
	}
}

// --- space expands a row ---

func TestMonitorTUI_SpaceExpandsRow(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkProviderRequest("9", "sonnet-5", "h1", 500, 2, 1000,
		[]monitor.MessageSummary{{Role: "user", Bytes: 12, Hash: "mh1", Preview: "hello there"}}))

	collapsed := m.View()
	if strings.Contains(collapsed, "hello there") {
		t.Fatalf("new-message detail visible before expanding, got:\n%s", collapsed)
	}

	m = key(t, m, runeKey(" "))
	expanded := m.View()
	if !strings.Contains(expanded, "hello there") {
		t.Errorf("expanded View() missing new-message preview, got:\n%s", expanded)
	}

	// space again collapses it back.
	m = key(t, m, runeKey(" "))
	if strings.Contains(m.View(), "hello there") {
		t.Errorf("row still expanded after second space, got:\n%s", m.View())
	}
}

// --- same session, different turns, same toolId don't collide (R2-2) ---

func TestMonitorTUI_ToolRowsKeyedByTurnID(t *testing.T) {
	m := NewModel(TUIConfig{})
	// Same session, DIFFERENT turns, SAME toolId (a provider reusing e.g.
	// "call_1" across turns) must render as two distinct rows, not one
	// overwriting the other.
	m = feed(t, m, mkToolStart("1", "call_1", "builtin", "bash-turn1", "args1", "a1"))
	m = feed(t, m, mkToolStart("2", "call_1", "builtin", "bash-turn2", "args2", "a2"))

	if len(m.rows) != 2 {
		t.Fatalf("want 2 distinct rows for the same toolId across 2 turns, got %d", len(m.rows))
	}
	view := m.View()
	if !strings.Contains(view, "bash-turn1") || !strings.Contains(view, "bash-turn2") {
		t.Fatalf("expected both turns' same-toolId tool rows, got:\n%s", view)
	}

	// A late tool_end for turn 1's call_1 must update ONLY turn 1's row.
	m = feed(t, m, mkToolEnd("1", "call_1", true, 500, "ok", "r1", 200))
	view = m.View()

	var line1, line2 string
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "bash-turn1") {
			line1 = l
		}
		if strings.Contains(l, "bash-turn2") {
			line2 = l
		}
	}
	if !strings.Contains(line1, "ok") {
		t.Errorf("turn 1's row missing tool_end result, got line:\n%s", line1)
	}
	if strings.Contains(line2, "ok ") {
		t.Errorf("turn 2's row picked up turn 1's tool_end result, got line:\n%s", line2)
	}
	if len(m.rows) != 2 {
		t.Errorf("tool_end must mutate an existing row in place, not append a third, got %d rows", len(m.rows))
	}
}

// --- tool_start / tool_end render (merged into one row) ---

func TestMonitorTUI_ToolStartThenEndRender(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("3", "t1", "builtin", "bash", "go test ./...", "argshash"))

	pending := m.View()
	if !strings.Contains(pending, "tool") || !strings.Contains(pending, "bash") || !strings.Contains(pending, "builtin") {
		t.Fatalf("pending tool row missing expected fields, got:\n%s", pending)
	}
	if strings.Contains(pending, "ok ") {
		t.Errorf("tool row shows a result before tool_end arrived, got:\n%s", pending)
	}

	m = feed(t, m, mkToolEnd("3", "t1", true, 2150, "ok", "resulthash", 4300))
	done := m.View()
	if !strings.Contains(done, "ok") {
		t.Errorf("completed tool row missing ok status, got:\n%s", done)
	}
	if !strings.Contains(done, "2.1KB") {
		t.Errorf("completed tool row missing result bytes, got:\n%s", done)
	}
	if !strings.Contains(done, "4.3s") {
		t.Errorf("completed tool row missing duration, got:\n%s", done)
	}
	// Still exactly one tool row — tool_end mutated in place, didn't append.
	visible := m.visibleRows()
	if len(visible) != 1 {
		t.Errorf("want 1 merged tool row, got %d", len(visible))
	}
}

func TestMonitorTUI_ToolEndFailure(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("1", "t1", "builtin", "bash", "false", "a"))
	m = feed(t, m, mkToolEnd("1", "t1", false, 0, "exit 1", "r", 100))

	view := m.View()
	if !strings.Contains(view, "FAIL") {
		t.Errorf("View() missing FAIL marker for a failed tool, got:\n%s", view)
	}
}

// --- toggles gate visibility ---

func TestMonitorTUI_ToggleToolsHidesToolRows(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("1", "t1", "builtin", "bash", "go test", "a"))

	if !strings.Contains(m.View(), "bash") {
		t.Fatalf("expected tool row visible by default")
	}
	m = key(t, m, runeKey("t"))
	if strings.Contains(m.View(), "bash") {
		t.Errorf("tool row still visible after toggling showTools off, got:\n%s", m.View())
	}
}

func TestMonitorTUI_ToggleMCPHidesOnlyMCPTools(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("1", "t1", "builtin", "bash", "go test", "a"))
	m = feed(t, m, mkToolStart("1", "t2", "mcp:slack", "slack_post", "channel", "b"))

	m = key(t, m, runeKey("p")) // showMCP off
	view := m.View()
	if !strings.Contains(view, "bash") {
		t.Errorf("builtin tool hidden after toggling MCP off, got:\n%s", view)
	}
	if strings.Contains(view, "slack_post") {
		t.Errorf("mcp tool still visible after toggling MCP off, got:\n%s", view)
	}
}

// --- context_event rendering + showThinking gate ---

func TestMonitorTUI_ContextEventRendersAndThinkingGated(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded", "enrich"))
	m = feed(t, m, mkContextEvent("1", 2, "thinking_level", "high"))

	view := m.View()
	if !strings.Contains(view, "skill_loaded") || !strings.Contains(view, "enrich") {
		t.Errorf("View() missing context_event row, got:\n%s", view)
	}
	if strings.Contains(view, "thinking_level") {
		t.Errorf("thinking_level row visible with showThinking off by default, got:\n%s", view)
	}

	m = key(t, m, runeKey("x"))
	if !strings.Contains(m.View(), "thinking_level") {
		t.Errorf("thinking_level row missing after toggling showThinking on, got:\n%s", m.View())
	}
}

// --- quit ---

func TestMonitorTUI_QuitKeys(t *testing.T) {
	m := NewModel(TUIConfig{})
	if _, cmd := m.Update(runeKey("q")); cmd == nil {
		t.Errorf("q key: want tea.Quit cmd, got nil")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Errorf("ctrl+c: want tea.Quit cmd, got nil")
	}
}

// --- filter input mode: q types instead of quitting; ctrl+c still quits ---

func TestMonitorTUI_FilterModeLettersDontTriggerToggles(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("1", "t1", "builtin", "bash", "go test", "a"))
	m = key(t, m, runeKey("/"))
	m = key(t, m, runeKey("t")) // would normally toggle showTools off

	if !m.showTools {
		t.Errorf("showTools toggled off while typing a filter; 't' should have been filter text")
	}
	if m.filterInput != "t" {
		t.Errorf("filterInput = %q, want %q", m.filterInput, "t")
	}
}

// --- Init/RunTUI shape sanity (no real program spun up) ---

func TestMonitorTUI_InitReturnsNilWithoutEventsChannel(t *testing.T) {
	m := NewModel(TUIConfig{})
	if cmd := m.Init(); cmd != nil {
		t.Errorf("Init() with nil Events channel: want nil Cmd")
	}
}

func TestMonitorTUI_InitReturnsCmdWithEventsChannel(t *testing.T) {
	ch := make(chan monitor.Event)
	m := NewModel(TUIConfig{Events: ch})
	if cmd := m.Init(); cmd == nil {
		t.Errorf("Init() with a real Events channel: want a non-nil Cmd")
	}
}

// --- no events yet ---

func TestMonitorTUI_EmptyState(t *testing.T) {
	m := NewModel(TUIConfig{Filter: "mybox"})
	view := m.View()
	if !strings.Contains(view, "(no events yet)") {
		t.Errorf("View() missing empty-state message, got:\n%s", view)
	}
	if !strings.Contains(view, "mybox") {
		t.Errorf("View() missing sandbox filter label in header, got:\n%s", view)
	}
	// DX-2b: the empty state must hint that a stale (pre-monitor) sandbox
	// image is the likely reason nothing is showing up yet.
	if !strings.Contains(view, "monitor-enabled sandbox") || !strings.Contains(view, "make load") {
		t.Errorf("View() missing stale-image hint, got:\n%s", view)
	}
	// With TUIConfig.Port unset (0), the hint falls back to monitor.DefaultPort.
	if !strings.Contains(view, fmt.Sprintf(":%d", monitor.DefaultPort)) {
		t.Errorf("View() empty-state hint missing default port :%d, got:\n%s", monitor.DefaultPort, view)
	}
}

// TestMonitorTUI_EmptyStateUsesConfiguredPort proves the empty-state hint
// names the hub's ACTUAL bound port (--port N), not just the default.
func TestMonitorTUI_EmptyStateUsesConfiguredPort(t *testing.T) {
	m := NewModel(TUIConfig{Port: 9999})
	view := m.View()
	if !strings.Contains(view, ":9999") {
		t.Errorf("View() empty-state hint missing configured port :9999, got:\n%s", view)
	}
}

// --- ANSI/control-char injection is stripped (review finding R1-8) ---

func TestMonitorTUI_SanitizesAnsiInjection(t *testing.T) {
	m := NewModel(TUIConfig{})
	// OSC-52 clipboard write ("YQ==" base64-decodes to "a") + a CSI color
	// sequence + a cursor-move CSI, all embedded in otherwise-normal fields.
	osc52 := "\x1b]52;c;YQ==\x07"
	csi := "\x1b[31mRED\x1b[0m"
	m = feed(t, m, mkToolStart("1", "t1", "builtin"+osc52, "bash"+csi, "args"+csi+osc52, "a"))
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded"+csi, "detail"+osc52+csi))

	view := m.View()
	// Decode as runes, not raw bytes: a valid multi-byte UTF-8 sequence
	// (e.g. the … ellipsis) has continuation bytes in 0x80-0xBF, which
	// would false-positive a byte-wise C1 scan.
	for _, r := range view {
		if r == 0x1b {
			t.Fatalf("View() contains a raw ESC rune, got:\n%q", view)
		}
		if r < 0x20 && r != '\n' {
			t.Fatalf("View() contains raw control rune %#x, got:\n%q", r, view)
		}
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("View() contains raw C1 control rune %#x, got:\n%q", r, view)
		}
	}
	// The harmless payload text around the injected sequences should still
	// render — sanitizing must not eat the whole field, just the escapes.
	if !strings.Contains(view, "bash") || !strings.Contains(view, "RED") {
		t.Errorf("View() over-sanitized, lost plain text, got:\n%s", view)
	}
}

func TestMonitorTUI_SanitizeTextKeepNewlines(t *testing.T) {
	if got := sanitizeText("line1\nline2", true); got != "line1\nline2" {
		t.Errorf("sanitizeText(keepNewlines=true) = %q, want newline preserved", got)
	}
	if got := sanitizeText("line1\nline2", false); got != "line1 line2" {
		t.Errorf("sanitizeText(keepNewlines=false) = %q, want newline replaced with space", got)
	}
}

// --- distinct sessions with identical turnId/toolId don't collide (R1-7) ---

func TestMonitorTUI_DistinctSessionsDontCollide(t *testing.T) {
	m := NewModel(TUIConfig{})

	reqA := mkProviderRequest("1", "opus-4-8", "hash-a", 1000, 1, 100, nil)
	reqA.SandboxID, reqA.SessionID = "sbx-a", "sess-a"
	reqB := mkProviderRequest("1", "sonnet-5", "hash-b", 2000, 2, 200, nil)
	reqB.SandboxID, reqB.SessionID = "sbx-b", "sess-b"

	m = feed(t, m, reqA)
	m = feed(t, m, reqB)

	if len(m.rows) != 2 {
		t.Fatalf("want 2 distinct rows for 2 sessions sharing turnId=1, got %d", len(m.rows))
	}
	view := m.View()
	if !strings.Contains(view, "opus-4-8") || !strings.Contains(view, "sonnet-5") {
		t.Fatalf("View() missing one of the two sessions' rows, got:\n%s", view)
	}

	// A second request on session A with the SAME hash as its own first
	// request should read as unchanged; it must not be affected by
	// session B's different hash having been seen in between.
	reqA2 := mkProviderRequest("2", "opus-4-8", "hash-a", 1000, 1, 100, nil)
	reqA2.SandboxID, reqA2.SessionID = "sbx-a", "sess-a"
	m = feed(t, m, reqA2)
	if !strings.Contains(m.View(), "sys=1000B(unchanged)") {
		t.Errorf("session A's own repeated hash not marked unchanged, got:\n%s", m.View())
	}

	// Session B's turn 1 tool_start (toolId "t1") and session A's own
	// tool_start with the SAME toolId must render as two separate rows.
	toolA := mkToolStart("1", "t1", "builtin", "bash-a", "args-a", "ha")
	toolA.SandboxID, toolA.SessionID = "sbx-a", "sess-a"
	toolB := mkToolStart("1", "t1", "builtin", "bash-b", "args-b", "hb")
	toolB.SandboxID, toolB.SessionID = "sbx-b", "sess-b"
	m = feed(t, m, toolA)
	m = feed(t, m, toolB)
	view = m.View()
	if !strings.Contains(view, "bash-a") || !strings.Contains(view, "bash-b") {
		t.Fatalf("expected both sessions' same-toolId tool rows, got:\n%s", view)
	}

	// tool_end on session A's t1 must mutate only session A's row.
	toolEndA := mkToolEnd("1", "t1", true, 100, "ok", "rha", 50)
	toolEndA.SandboxID, toolEndA.SessionID = "sbx-a", "sess-a"
	m = feed(t, m, toolEndA)
	view = m.View()
	if !strings.Contains(view, "bash-b") {
		t.Fatalf("session B's pending tool row disappeared, got:\n%s", view)
	}
	// Session B's row must still show as pending (no result yet).
	lines := strings.Split(view, "\n")
	var bLine string
	for _, l := range lines {
		if strings.Contains(l, "bash-b") {
			bLine = l
		}
	}
	if strings.Contains(bLine, "ok ") {
		t.Errorf("session B's row picked up session A's tool_end result, got line:\n%s", bLine)
	}
}

// --- blob resolution happens in Update, never in View (R1-12) ---

func TestMonitorTUI_BlobResolvedOnlyByUpdate(t *testing.T) {
	calls := 0
	blob := monitor.Blob{Hash: "h1", Bytes: 10, Text: "FULL BODY"}
	fakeBlob := func(hash string) (monitor.Blob, bool) {
		calls++
		if hash == blob.Hash {
			return blob, true
		}
		return monitor.Blob{}, false
	}
	m := NewModel(TUIConfig{Blob: fakeBlob})
	m = key(t, m, runeKey("f")) // showFull on up front
	m = feed(t, m, mkProviderRequest("1", "opus-4-8", blob.Hash, 10, 0, 10, nil))

	// Calling View() repeatedly before expanding must never call cfg.Blob
	// and must never show the body.
	before := calls
	for i := 0; i < 5; i++ {
		if strings.Contains(m.View(), blob.Text) {
			t.Fatalf("blob text visible before row was expanded")
		}
	}
	if calls != before {
		t.Errorf("View() called cfg.Blob %d times; View must be pure and never call it", calls-before)
	}

	// The expanding Update (space) is what resolves it.
	m = key(t, m, runeKey(" "))
	afterUpdateCalls := calls
	if afterUpdateCalls == before {
		t.Fatalf("expanding Update never called cfg.Blob")
	}

	// Now View() must show the already-resolved text WITHOUT calling
	// cfg.Blob again, however many times it's rendered.
	for i := 0; i < 5; i++ {
		view := m.View()
		if !strings.Contains(view, blob.Text) {
			t.Fatalf("expanded View() missing resolved blob text, got:\n%s", view)
		}
	}
	if calls != afterUpdateCalls {
		t.Errorf("View() called cfg.Blob after the resolving Update (%d extra calls); View must render stored state only", calls-afterUpdateCalls)
	}
}

// --- row growth is bounded (R1-13) ---

func TestMonitorTUI_RowsBoundedAndOldestEvicted(t *testing.T) {
	m := NewModel(TUIConfig{})
	const n = maxRows + 500
	for i := 0; i < n; i++ {
		m = feed(t, m, mkContextEvent("1", uint64(i), "skill_loaded", fmt.Sprintf("skill-%d", i)))
	}
	if len(m.rows) != maxRows {
		t.Fatalf("len(m.rows) = %d, want capped at maxRows=%d", len(m.rows), maxRows)
	}
	if len(m.rowIndex) != maxRows {
		t.Errorf("len(m.rowIndex) = %d, want %d (evicted rows must be removed from the index too)", len(m.rowIndex), maxRows)
	}
	// The oldest rows (skill-0 .. skill-499) must be gone; the newest
	// (skill-(n-1)) must remain.
	view := m.View()
	if strings.Contains(view, "skill-0\n") || strings.Contains(view, "skill-0 ") {
		t.Errorf("oldest row (skill-0) was not evicted, got:\n%s", view)
	}
	if !strings.Contains(view, fmt.Sprintf("skill-%d", n-1)) {
		t.Errorf("newest row (skill-%d) missing after eviction, got:\n%s", n-1, view)
	}
}

// --- prevSysHash is bounded to live sessions (review finding R4-2) ---

// A long-running monitor watching many distinct sessions (the default
// `monitor` watches every sandbox/session at once) must not accumulate one
// prevSysHash entry per session forever. Once a session's rows are all
// evicted by evictOldRows (maxRows exceeded), that session's prevSysHash
// entry must go with them, so prevSysHash stays bounded by the number of
// LIVE (row-retaining) sessions rather than growing with total distinct
// sessions ever seen.
func TestMonitorTUI_PrevSysHashBoundedByLiveSessions(t *testing.T) {
	m := NewModel(TUIConfig{})
	const n = maxRows + 500
	for i := 0; i < n; i++ {
		req := mkProviderRequest("1", "opus-4-8", fmt.Sprintf("hash-%d", i), 1000, 1, 100, nil)
		req.SandboxID, req.SessionID = "sbx", fmt.Sprintf("sess-%d", i)
		m = feed(t, m, req)
	}

	// One row per session (n sessions, one request each), capped at
	// maxRows by evictOldRows — same invariant as
	// TestMonitorTUI_RowsBoundedAndOldestEvicted.
	if len(m.rows) != maxRows {
		t.Fatalf("len(m.rows) = %d, want capped at maxRows=%d", len(m.rows), maxRows)
	}

	// The fix under test: prevSysHash must NOT hold one entry per distinct
	// session ever seen (that would be n = maxRows+500, the R4-2 leak).
	// It must instead be bounded by the number of sessions that still have
	// a retained row — exactly maxRows here, since each session has
	// exactly one row.
	if len(m.prevSysHash) != maxRows {
		t.Fatalf("len(m.prevSysHash) = %d, want bounded at maxRows=%d (not growing with total sessions seen=%d)", len(m.prevSysHash), maxRows, n)
	}
	if len(m.sessionRowCount) != maxRows {
		t.Fatalf("len(m.sessionRowCount) = %d, want %d (one live session per retained row)", len(m.sessionRowCount), maxRows)
	}

	// An evicted session's prevSysHash entry must actually be gone, not
	// just uncounted.
	if _, ok := m.prevSysHash["sbx/sess-0"]; ok {
		t.Errorf("prevSysHash still holds evicted session sbx/sess-0's entry")
	}

	// A session that STILL has a retained row (the most recent one) must
	// keep working delta computation: a second request with the SAME hash
	// on that live session must still read as "(unchanged)".
	liveSess := fmt.Sprintf("sess-%d", n-1)
	repeat := mkProviderRequest("2", "opus-4-8", fmt.Sprintf("hash-%d", n-1), 1000, 1, 100, nil)
	repeat.SandboxID, repeat.SessionID = "sbx", liveSess
	m = feed(t, m, repeat)
	if !strings.Contains(m.View(), "sys=1000B(unchanged)") {
		t.Errorf("live session's repeated hash not marked unchanged after eviction of older sessions, got:\n%s", m.View())
	}
}

// --- retained memory is bounded to expanded rows (review finding R3-2b) ---

// Collapsing a row must clear its resolved full-body text (not just hide
// it): the stored sysPromptText/newMessageTexts/toolSchemaText fields must
// go back to their zero value so the large body can be GC'd, rather than
// living on the row for the rest of the process.
func TestMonitorTUI_CollapseClearsResolvedBody(t *testing.T) {
	bigBody := strings.Repeat("X", 5000)
	msgBody := strings.Repeat("Y", 3000)
	schemaBody := strings.Repeat("Z", 2000)
	blobs := map[string]monitor.Blob{
		"sys-hash":    {Hash: "sys-hash", Bytes: len(bigBody), Text: bigBody},
		"msg-hash":    {Hash: "msg-hash", Bytes: len(msgBody), Text: msgBody},
		"schema-hash": {Hash: "schema-hash", Bytes: len(schemaBody), Text: schemaBody},
	}
	fakeBlob := func(hash string) (monitor.Blob, bool) {
		bl, ok := blobs[hash]
		return bl, ok
	}
	m := NewModel(TUIConfig{Blob: fakeBlob})
	m = key(t, m, runeKey("f")) // showFull on
	req := mkProviderRequest("1", "opus-4-8", "sys-hash", len(bigBody), 1, 100,
		[]monitor.MessageSummary{{Role: "user", Bytes: len(msgBody), Hash: "msg-hash", Preview: "hi"}})
	req.Summary.ToolSchemaHash = "schema-hash"
	m = feed(t, m, req)

	// Expand: the body should now be resolved and stored on the row.
	m = key(t, m, runeKey(" "))
	id := m.rows[0].id
	row := m.rows[m.rowIndex[id]]
	if row.sysPromptText != bigBody {
		t.Fatalf("sysPromptText not resolved after expand, got %q", row.sysPromptText)
	}
	if len(row.newMessageTexts) != 1 || row.newMessageTexts[0] != msgBody {
		t.Fatalf("newMessageTexts not resolved after expand, got %v", row.newMessageTexts)
	}
	if row.toolSchemaText != schemaBody {
		t.Fatalf("toolSchemaText not resolved after expand, got %q", row.toolSchemaText)
	}
	// The small summary fields (preview, byte counts) must still be intact.
	if row.newMessages[0].Preview != "hi" {
		t.Fatalf("newMessages summary lost, got %v", row.newMessages)
	}

	// Collapse: the large resolved bodies must be cleared back to empty.
	m = key(t, m, runeKey(" "))
	row = m.rows[m.rowIndex[id]]
	if row.sysPromptText != "" {
		t.Errorf("sysPromptText retained after collapse, len=%d", len(row.sysPromptText))
	}
	if len(row.newMessageTexts) != 0 {
		t.Errorf("newMessageTexts retained after collapse, got %v", row.newMessageTexts)
	}
	if row.toolSchemaText != "" {
		t.Errorf("toolSchemaText retained after collapse, len=%d", len(row.toolSchemaText))
	}
	// Summary fields survive the collapse — only the resolved bodies clear.
	if row.newMessages[0].Preview != "hi" {
		t.Errorf("newMessages summary lost after collapse, got %v", row.newMessages)
	}
	if row.sysHash != "sys-hash" {
		t.Errorf("sysHash lost after collapse, got %q", row.sysHash)
	}
}

// Toggling showFull off must clear every row's resolved body, not just the
// row a subsequent collapse would touch — with showFull off nothing is
// rendered, so nothing should still be retained either.
func TestMonitorTUI_ShowFullOffClearsAllResolvedBodies(t *testing.T) {
	body1 := strings.Repeat("A", 4000)
	body2 := strings.Repeat("B", 4000)
	blobs := map[string]monitor.Blob{"h1": {Hash: "h1", Text: body1}, "h2": {Hash: "h2", Text: body2}}
	fakeBlob := func(hash string) (monitor.Blob, bool) {
		bl, ok := blobs[hash]
		return bl, ok
	}
	m := NewModel(TUIConfig{Blob: fakeBlob})
	m = key(t, m, runeKey("f")) // showFull on
	m = feed(t, m, mkProviderRequest("1", "opus-4-8", "h1", len(body1), 0, 10, nil))
	m = key(t, m, runeKey(" ")) // expand row 1 -> resolves body1
	m = feed(t, m, mkProviderRequest("2", "sonnet-5", "h2", len(body2), 0, 10, nil))

	// Row 2 isn't expanded, so "expand the last visible row" now targets
	// row 2 instead: expand it too so both rows carry a resolved body.
	m = key(t, m, runeKey(" "))

	id1, id2 := m.rows[0].id, m.rows[1].id
	if m.rows[m.rowIndex[id1]].sysPromptText != body1 {
		t.Fatalf("row 1 body not resolved before toggling showFull off")
	}
	if m.rows[m.rowIndex[id2]].sysPromptText != body2 {
		t.Fatalf("row 2 body not resolved before toggling showFull off")
	}

	// f toggles showFull OFF: both rows' resolved bodies must clear.
	m = key(t, m, runeKey("f"))
	if got := m.rows[m.rowIndex[id1]].sysPromptText; got != "" {
		t.Errorf("row 1 sysPromptText retained after showFull off, len=%d", len(got))
	}
	if got := m.rows[m.rowIndex[id2]].sysPromptText; got != "" {
		t.Errorf("row 2 sysPromptText retained after showFull off, len=%d", len(got))
	}
}

// Re-expanding a collapsed row must re-resolve via cfg.Blob rather than
// leaving it empty (the cleared state on collapse must be transient, not
// permanent).
func TestMonitorTUI_ReExpandReResolvesBlob(t *testing.T) {
	calls := 0
	blob := monitor.Blob{Hash: "h1", Text: "REPEATED SYSTEM PROMPT"}
	fakeBlob := func(hash string) (monitor.Blob, bool) {
		calls++
		if hash == blob.Hash {
			return blob, true
		}
		return monitor.Blob{}, false
	}
	m := NewModel(TUIConfig{Blob: fakeBlob})
	m = key(t, m, runeKey("f")) // showFull on
	m = feed(t, m, mkProviderRequest("1", "opus-4-8", blob.Hash, 100, 0, 10, nil))

	m = key(t, m, runeKey(" ")) // expand -> resolves
	first := calls
	if first == 0 {
		t.Fatalf("expand never called cfg.Blob")
	}
	if !strings.Contains(m.View(), blob.Text) {
		t.Fatalf("expanded View() missing resolved body, got:\n%s", m.View())
	}

	m = key(t, m, runeKey(" ")) // collapse -> clears
	id := m.rows[0].id
	if m.rows[m.rowIndex[id]].sysPromptText != "" {
		t.Fatalf("sysPromptText not cleared on collapse")
	}

	m = key(t, m, runeKey(" ")) // re-expand -> must re-resolve, not reuse stale empty state
	if calls <= first {
		t.Errorf("re-expanding did not call cfg.Blob again: calls before=%d after=%d", first, calls)
	}
	if !strings.Contains(m.View(), blob.Text) {
		t.Errorf("re-expanded View() missing re-resolved body, got:\n%s", m.View())
	}
}

// --- provider_response row ---

func TestMonitorTUI_ProviderResponseRow(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkProviderResponse("7", 200, "tool_use", &monitor.UsageSummary{InputTokens: 37900, OutputTokens: 512, TotalTokens: 38412}))
	view := m.View()
	if !strings.Contains(view, "resp 200") {
		t.Errorf("View() missing response status, got:\n%s", view)
	}
	if !strings.Contains(view, "stop=tool_use") {
		t.Errorf("View() missing stop reason, got:\n%s", view)
	}
	if !strings.Contains(view, "37.9k") {
		t.Errorf("View() missing input token count, got:\n%s", view)
	}
}
