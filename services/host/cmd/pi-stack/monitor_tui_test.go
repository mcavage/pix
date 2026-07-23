package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

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

// The conversation-first rework put the user-message PREVIEW on the summary
// line itself (it is the headline now), so "expand shows something new" is
// asserted via expand-only content: the per-message detail line and the
// `— diagnostics —` section marker.
func TestMonitorTUI_SpaceExpandsRow(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkProviderRequest("9", "sonnet-5", "h1", 500, 2, 1000,
		[]monitor.MessageSummary{{Role: "user", Bytes: 12, Hash: "mh1", Preview: "hello there"}}))

	collapsed := m.View()
	if strings.Contains(collapsed, "msg user") || strings.Contains(collapsed, "\u2014 diagnostics \u2014") {
		t.Fatalf("expand-only detail visible before expanding, got:\n%s", collapsed)
	}
	// The preview itself IS the collapsed headline now.
	if !strings.Contains(collapsed, "hello there") {
		t.Fatalf("collapsed summary missing the user message preview, got:\n%s", collapsed)
	}

	m = key(t, m, runeKey(" "))
	expanded := m.View()
	if !strings.Contains(expanded, "msg user") || !strings.Contains(expanded, "\u2014 diagnostics \u2014") {
		t.Errorf("expanded View() missing message detail / diagnostics section, got:\n%s", expanded)
	}

	// space again collapses it back.
	m = key(t, m, runeKey(" "))
	if strings.Contains(m.View(), "msg user") || strings.Contains(m.View(), "\u2014 diagnostics \u2014") {
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

// --- requirement 1: alt-screen + size handling ---

// windowMsg builds a tea.WindowSizeMsg feed helper (like key/feed above,
// but for the resize message Update now handles).
func resize(t *testing.T, m Model, w, h int) Model {
	t.Helper()
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return nm
}

func TestMonitorTUI_WindowSizeMsgStoresDimensions(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = resize(t, m, 120, 40)
	if m.width != 120 || m.height != 40 {
		t.Fatalf("width/height = %d/%d, want 120/40", m.width, m.height)
	}
}

// nRows feeds n distinct context_event rows (cheap, one line each, no
// expand) into m so tests can build a feed long enough to need clamping.
func nRows(t *testing.T, m Model, n int) Model {
	t.Helper()
	for i := 0; i < n; i++ {
		m = feed(t, m, mkContextEvent("1", uint64(i), "skill_loaded", fmt.Sprintf("row-%d", i)))
	}
	return m
}

// --- requirement 2: cursor navigation (line-granular) ---

// selectedIdx resolves the row that owns the cursor's line to its index in
// visibleRows() — with every row collapsed (one line each) this equals the
// line index, which is what the nav tests below assert on.
func selectedIdx(m Model) int {
	id := m.selectedRowID()
	for i, r := range m.visibleRows() {
		if r.id == id {
			return i
		}
	}
	return -1
}

func TestMonitorTUI_CursorMovesWithJKAndArrows(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 5) // row-0 .. row-4, newest (row-4) selected by default (follow)

	if got := m.selectedRowID(); !strings.Contains(got, "4") {
		t.Fatalf("default selection = %q, want the newest row (contains 4)", got)
	}

	// k / up move the cursor up (older).
	m = key(t, m, runeKey("k"))
	if idx := selectedIdx(m); idx != 3 {
		t.Fatalf("after k: selected index = %d, want 3", idx)
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if idx := selectedIdx(m); idx != 2 {
		t.Fatalf("after up: selected index = %d, want 2", idx)
	}

	// j / down move it back down.
	m = key(t, m, runeKey("j"))
	if idx := selectedIdx(m); idx != 3 {
		t.Fatalf("after j: selected index = %d, want 3", idx)
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if idx := selectedIdx(m); idx != 4 {
		t.Fatalf("after down: selected index = %d, want 4 (back at the bottom)", idx)
	}
}

func TestMonitorTUI_SelectedRowIsHighlighted(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 3)
	m = key(t, m, runeKey("k")) // select row-1 (middle)

	view := m.View()
	var selectedLine, otherLine string
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "row-1") {
			selectedLine = l
		}
		if strings.Contains(l, "row-2") {
			otherLine = l
		}
	}
	if selectedLine == "" || otherLine == "" {
		t.Fatalf("missing expected rows in view:\n%s", view)
	}
	// The selected row's line carries a "> " cursor marker (plus
	// lipgloss reverse/bold styling, which only emits ANSI codes on a
	// real color terminal — termenv reports NoColor in this non-tty test
	// process, so the marker is the one signal guaranteed to be visible
	// either way); the unselected row's line gets the blank two-space
	// gutter instead.
	if !strings.Contains(selectedLine, "> ") {
		t.Errorf("selected row line missing cursor marker, got: %q", selectedLine)
	}
	if strings.Contains(otherLine, "> ") {
		t.Errorf("unselected row line unexpectedly carries the cursor marker, got: %q", otherLine)
	}
}

func TestMonitorTUI_GHomeAndGEnd(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 5)

	m = key(t, m, runeKey("g"))
	if idx := selectedIdx(m); idx != 0 {
		t.Fatalf("after g: selected index = %d, want 0", idx)
	}
	if m.follow {
		t.Errorf("after g: follow should be detached")
	}

	m = key(t, m, runeKey("G"))
	if idx := selectedIdx(m); idx != 4 {
		t.Fatalf("after G: selected index = %d, want 4", idx)
	}
	if !m.follow {
		t.Errorf("after G: follow should be re-attached")
	}
}

func TestMonitorTUI_PageUpPageDown(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 30)
	m = resize(t, m, 80, 15) // small bodyHeight so a page is a real step

	start := selectedIdx(m)
	m = key(t, m, tea.KeyMsg{Type: tea.KeyPgUp})
	afterUp := selectedIdx(m)
	if afterUp >= start {
		t.Fatalf("PgUp did not move the cursor up: before=%d after=%d", start, afterUp)
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	afterDown := selectedIdx(m)
	if afterDown <= afterUp {
		t.Fatalf("PgDn did not move the cursor down: afterUp=%d afterDown=%d", afterUp, afterDown)
	}
}

// --- requirement 2 (headline ask): enter/space expand the SELECTED row ---

func TestMonitorTUI_EnterExpandsSelectedRowNotLast(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkProviderRequest("1", "opus-4-8", "h1", 500, 0, 1000,
		[]monitor.MessageSummary{{Role: "user", Bytes: 12, Hash: "mh1", Preview: "older row preview"}}))
	m = feed(t, m, mkProviderRequest("2", "opus-4-8", "h2", 500, 0, 1000,
		[]monitor.MessageSummary{{Role: "user", Bytes: 12, Hash: "mh2", Preview: "newer row preview"}}))

	// Select the OLDER row (turn 1), not the last/newest one.
	m = key(t, m, runeKey("k"))
	if idx := selectedIdx(m); idx != 0 {
		t.Fatalf("selected index = %d, want 0 (the older row)", idx)
	}

	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	// Previews live on the summary lines now (conversation-first), so
	// "which row expanded" is asserted on expand-only content: exactly one
	// diagnostics section, owned by the OLDER row's detail lines.
	view := m.View()
	if got := strings.Count(view, "\u2014 diagnostics \u2014"); got != 1 {
		t.Fatalf("want exactly 1 expanded row (1 diagnostics section), got %d:\n%s", got, view)
	}
	olderID, newerID := m.rows[0].id, m.rows[1].id
	if !m.expanded[olderID] {
		t.Errorf("enter did not expand the selected (older) row")
	}
	if m.expanded[newerID] {
		t.Errorf("enter expanded the NEWER row instead of the selected older one, got:\n%s", view)
	}

	// space toggles the same selected row back closed.
	m = key(t, m, runeKey(" "))
	if strings.Contains(m.View(), "\u2014 diagnostics \u2014") {
		t.Errorf("space did not collapse the selected row, got:\n%s", m.View())
	}
}

// --- requirement 4: height clamps the body instead of dumping every row ---

func TestMonitorTUI_HeightClampsLineCount(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 200)
	m = resize(t, m, 100, 20)

	view := m.View()
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) > 20 {
		t.Fatalf("View() emitted %d lines for height=20, want <= 20 (unbounded dump)", len(lines))
	}
	// Still following: the newest row (row-199) must be visible without
	// any navigation.
	if !strings.Contains(view, "row-199") {
		t.Errorf("clamped View() missing newest row, got:\n%s", view)
	}
	// And the flood of older rows must NOT all be dumped just because
	// they exist.
	if strings.Contains(view, "row-0\n") {
		t.Errorf("clamped View() dumped the oldest row despite height=20, got:\n%s", view)
	}
}

func TestMonitorTUI_HeightClampsFullPayloadDump(t *testing.T) {
	// A big schema/body (well beyond a 20-line terminal) must not spew
	// past the clamped frame just because f:full is on.
	bigSchema := strings.Repeat("SCHEMA-LINE\n", 500)
	fakeBlob := func(hash string) (monitor.Blob, bool) {
		if hash == "schema-hash" {
			return monitor.Blob{Hash: hash, Text: bigSchema}, true
		}
		return monitor.Blob{}, false
	}
	m := NewModel(TUIConfig{Blob: fakeBlob})
	m = resize(t, m, 100, 20)
	m = key(t, m, runeKey("f")) // showFull on
	req := mkProviderRequest("1", "opus-4-8", "sys-hash", 10, 1, 10, nil)
	req.Summary.ToolSchemaHash = "schema-hash"
	m = feed(t, m, req)
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // expand -> resolves the huge blob

	view := m.View()
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) > 20 {
		t.Fatalf("expanded f:full View() emitted %d lines for height=20, want <= 20 (unbounded schema dump)", len(lines))
	}
}

// --- requirement 3: follow mode ---

func TestMonitorTUI_FollowKeepsNewestVisible(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = resize(t, m, 100, 10)
	m = nRows(t, m, 50)

	view := m.View()
	if !strings.Contains(view, "row-49") {
		t.Fatalf("follow mode did not keep the newest row visible, got:\n%s", view)
	}
	if !strings.Contains(view, "[following]") {
		t.Errorf("View() missing [following] indicator, got:\n%s", view)
	}
}

func TestMonitorTUI_MovingCursorUpDetachesFollow(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = resize(t, m, 100, 10)
	m = nRows(t, m, 50)

	m = key(t, m, runeKey("k")) // move up: detach
	if m.follow {
		t.Fatalf("moving the cursor up did not detach follow")
	}
	if !strings.Contains(m.View(), "[paused]") {
		t.Errorf("View() missing [paused] indicator after detaching, got:\n%s", m.View())
	}
	selectedBefore := m.selectedRowID()

	// A brand-new event must NOT jump the view back to the bottom while
	// detached.
	m = feed(t, m, mkContextEvent("1", 999, "skill_loaded", "row-new"))
	if got := m.selectedRowID(); got != selectedBefore {
		t.Fatalf("a new event changed the selection while follow was detached: before=%q after=%q", selectedBefore, got)
	}
	view := m.View()
	if strings.Contains(view, "row-new") {
		t.Errorf("detached follow still scrolled to a brand-new row, got:\n%s", view)
	}

	// G re-attaches and jumps back to (the now newest) bottom row.
	m = key(t, m, runeKey("G"))
	if !m.follow {
		t.Fatalf("G did not re-attach follow")
	}
	if !strings.Contains(m.View(), "row-new") {
		t.Errorf("after G, the newest row is not visible, got:\n%s", m.View())
	}
}

// --- requirement 5: help overlay ---

func TestMonitorTUI_HelpOverlayReplacesBody(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("1", "t1", "builtin", "bash", "go test", "a"))

	before := m.View()
	if !strings.Contains(before, "bash") {
		t.Fatalf("expected the tool row visible before opening help, got:\n%s", before)
	}

	m = key(t, m, runeKey("?"))
	if !m.showHelp {
		t.Fatalf("? did not open the help overlay")
	}
	helpView := m.View()
	if !strings.Contains(helpView, "keys") || !strings.Contains(helpView, "q, ctrl+c") {
		t.Errorf("help overlay missing expected content, got:\n%s", helpView)
	}
	if strings.Contains(helpView, "bash") {
		t.Errorf("help overlay did not replace the body (row still visible), got:\n%s", helpView)
	}

	// esc closes it; the row feed is back.
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.showHelp {
		t.Fatalf("esc did not close the help overlay")
	}
	if !strings.Contains(m.View(), "bash") {
		t.Errorf("body not restored after closing help, got:\n%s", m.View())
	}

	// ? also closes it (toggle).
	m = key(t, m, runeKey("?"))
	m = key(t, m, runeKey("?"))
	if m.showHelp {
		t.Fatalf("second ? did not close the overlay")
	}
}

func TestMonitorTUI_HelpOverlaySwallowsOtherKeys(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = key(t, m, runeKey("?"))
	m = key(t, m, runeKey("f")) // must NOT toggle showFull while help is open
	if m.showFull {
		t.Errorf("a key pressed while the help overlay is open leaked through to a toggle")
	}
}

// --- line-cursor rework: the audit's confirmed bugs stay fixed ---

// Bug 1 (headline): a long expanded payload must be readable by scrolling
// THROUGH it line by line, with the frame still clamped to the terminal
// height.
func TestMonitorTUI_LineCursorScrollsThroughExpandedPayload(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		sb.WriteString(fmt.Sprintf("payload line %02d\n", i))
	}
	blob := monitor.Blob{Hash: "sys-h", Bytes: sb.Len(), Text: sb.String()}
	m := NewModel(TUIConfig{Blob: func(h string) (monitor.Blob, bool) {
		if h == blob.Hash {
			return blob, true
		}
		return monitor.Blob{}, false
	}})
	m = resize(t, m, 100, 20)
	m = key(t, m, runeKey("f")) // showFull on
	m = feed(t, m, mkProviderRequest("1", "opus-4-8", blob.Hash, sb.Len(), 0, 10, nil))
	m = feed(t, m, mkContextEvent("1", 2, "skill_loaded", "later-row"))

	m = key(t, m, runeKey("g"))                   // cursor to the request header (top), detach
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // expand it

	frameLines := func(v string) int { return len(strings.Split(strings.TrimRight(v, "\n"), "\n")) }

	view := m.View()
	if !strings.Contains(view, "payload line 00") {
		t.Fatalf("expanded payload's first lines not visible, got:\n%s", view)
	}
	if strings.Contains(view, "payload line 30") {
		t.Fatalf("line 30 already visible before scrolling (no clamp?), got:\n%s", view)
	}
	if n := frameLines(view); n > 20 {
		t.Fatalf("View() emitted %d lines for height=20, want <= 20", n)
	}

	// down scrolls THROUGH the payload: later lines become visible.
	for i := 0; i < 35; i++ {
		m = key(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	view = m.View()
	if !strings.Contains(view, "payload line 30") {
		t.Errorf("after 35x down, payload line 30 still not visible (cannot scroll through payload), got:\n%s", view)
	}
	if n := frameLines(view); n > 20 {
		t.Errorf("View() emitted %d lines for height=20 while scrolled into a payload, want <= 20", n)
	}
}

// Bug 2: expanding a response row must show detail (status/stop/usage +
// headers), not be a silent no-op.
func TestMonitorTUI_ExpandResponseRowShowsDetail(t *testing.T) {
	m := NewModel(TUIConfig{})
	resp := mkProviderResponse("7", 200, "tool_use", &monitor.UsageSummary{InputTokens: 100, OutputTokens: 50, TotalTokens: 150})
	resp.Headers = map[string]string{"x-request-id": "abc-123"}
	m = feed(t, m, resp)

	before := m.View()
	m = key(t, m, runeKey(" ")) // following: cursor is on the response row
	after := m.View()
	if len(strings.Split(after, "\n")) <= len(strings.Split(before, "\n")) {
		t.Fatalf("expanding a response row added no lines (silent no-op), got:\n%s", after)
	}
	if !strings.Contains(after, "status 200") || !strings.Contains(after, "stop=tool_use") {
		t.Errorf("response detail missing status/stop, got:\n%s", after)
	}
	if !strings.Contains(after, "in=100") || !strings.Contains(after, "out=50") || !strings.Contains(after, "total=150") {
		t.Errorf("response detail missing usage tokens, got:\n%s", after)
	}
	// Headers are behind the `h` toggle now (default off).
	if strings.Contains(after, "x-request-id") {
		t.Errorf("response detail shows headers with h:headers=off, got:\n%s", after)
	}
	m = key(t, m, runeKey("h"))
	if !strings.Contains(m.View(), "x-request-id: abc-123") {
		t.Errorf("response detail missing headers after h, got:\n%s", m.View())
	}
}

// Bug 2 (other half): expanding a context row must show detail too.
func TestMonitorTUI_ExpandContextRowShowsDetail(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded", "enrich"))

	m = key(t, m, runeKey(" "))
	view := m.View()
	if !strings.Contains(view, "skill_loaded: enrich") {
		t.Errorf("expanding a context row produced no detail line, got:\n%s", view)
	}
}

// Bug 3: a multi-line blob body must render as multiple physical
// bodyLines — none containing '\n', none exceeding the terminal width.
func TestMonitorTUI_MultiLineBodySplitAndTruncated(t *testing.T) {
	body := "short first line\n" + strings.Repeat("W", 200) + "\nlast line marker"
	blob := monitor.Blob{Hash: "h", Bytes: len(body), Text: body}
	m := NewModel(TUIConfig{Blob: func(h string) (monitor.Blob, bool) {
		if h == blob.Hash {
			return blob, true
		}
		return monitor.Blob{}, false
	}})
	const width = 40
	// Tall enough that nothing scrolls out: this test is about WIDTH
	// clamping only. (Height 0 no longer means "unbounded" once a real
	// WindowSizeMsg has arrived — a sized zero-height terminal renders
	// nothing, see TestMonitorTUI_TinyHeightsClampTotalOutput.)
	m = resize(t, m, width, 200)
	m = key(t, m, runeKey("f"))
	m = feed(t, m, mkProviderRequest("1", "opus-4-8", blob.Hash, len(body), 0, 10, nil))
	m = key(t, m, runeKey(" ")) // expand

	for i, l := range m.renderBodyLines() {
		if strings.Contains(l.text, "\n") {
			t.Errorf("body line %d contains an embedded newline: %q", i, l.text)
		}
		if n := utf8.RuneCountInString(l.text); n > width {
			t.Errorf("body line %d is %d runes, want <= width=%d: %q", i, n, width, l.text)
		}
	}
	view := m.View()
	if !strings.Contains(view, "short first line") {
		t.Errorf("first physical line of the body missing, got:\n%s", view)
	}
	if !strings.Contains(view, "last line marker") {
		t.Errorf("last physical line of the body missing, got:\n%s", view)
	}
	if strings.Contains(view, strings.Repeat("W", width)) {
		t.Errorf("long physical line not truncated to width, got:\n%s", view)
	}
}

// Bug 4: the help overlay renders from its own top (never inherits the row
// view's scrollTop) and is scrollable when taller than the viewport.
func TestMonitorTUI_HelpOverlayStartsAtTopAndScrolls(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = resize(t, m, 100, 12)
	m = nRows(t, m, 50) // following: scrollTop is deep into the feed

	m = key(t, m, runeKey("?"))
	view := m.View()
	if !strings.Contains(view, "pi-stack monitor — keys") {
		t.Fatalf("help overlay clipped at the top (title missing), got:\n%s", view)
	}
	if n := len(strings.Split(strings.TrimRight(view, "\n"), "\n")); n > 12 {
		t.Fatalf("help View() emitted %d lines for height=12, want <= 12", n)
	}

	// The same line nav scrolls help: End bottom-anchors, revealing the
	// last help line that was clipped at open.
	if strings.Contains(view, "q, ctrl+c") {
		t.Fatalf("help fits the viewport; test needs it taller to prove scrolling")
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(m.View(), "q, ctrl+c") {
		t.Errorf("End did not scroll help to its bottom, got:\n%s", m.View())
	}

	// Closing help restores the row feed (follow re-attached: newest row).
	m = key(t, m, runeKey("?"))
	if m.showHelp {
		t.Fatalf("second ? did not close help")
	}
	if !m.follow {
		t.Errorf("closing help did not re-attach follow")
	}
	if !strings.Contains(m.View(), "row-49") {
		t.Errorf("row feed not restored (newest row missing) after closing help, got:\n%s", m.View())
	}
}

// Fix 7: row headers carry a ▸/▾ expand affordance.
func TestMonitorTUI_ExpandCaretOnRowHeaders(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded", "enrich"))

	if !strings.Contains(m.View(), "▸") {
		t.Fatalf("collapsed row header missing ▸ caret, got:\n%s", m.View())
	}
	m = key(t, m, runeKey(" "))
	if !strings.Contains(m.View(), "▾") {
		t.Errorf("expanded row header missing ▾ caret, got:\n%s", m.View())
	}
	m = key(t, m, runeKey(" "))
	if strings.Contains(m.View(), "▾") {
		t.Errorf("collapsed-again row header still shows ▾, got:\n%s", m.View())
	}
}

// Line-cursor follow semantics: stepping down to the LAST body line
// re-attaches follow; collapsing a row snaps the cursor to its header.
func TestMonitorTUI_LineCursorFollowReattachAndCollapseSnap(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 3)

	m = key(t, m, runeKey("k")) // detach, cursor on row-1's line
	if m.follow {
		t.Fatalf("k did not detach follow")
	}
	m = key(t, m, runeKey("j")) // back to the last line: re-attach
	if !m.follow {
		t.Errorf("stepping down to the last body line did not re-attach follow")
	}

	// Expand the middle row, walk into its detail, collapse from there:
	// the cursor must snap to that row's header line.
	m = key(t, m, runeKey("k")) // cursor on row-1 header (detached)
	m = key(t, m, runeKey(" ")) // expand row-1
	m = key(t, m, runeKey("j")) // step onto row-1's detail line
	// nRows rows are ctx events seq 0/1/2; row-1's id ends in ":1" (the
	// seq), which distinguishes it from ":0"/":2" — a bare Contains(":1")
	// would match every row's "ctx:1:" turn segment.
	if got := m.selectedRowID(); !strings.HasSuffix(got, ":1") {
		t.Fatalf("cursor's owning row = %q, want row-1's id (suffix :1)", got)
	}
	m = key(t, m, runeKey(" ")) // collapse from the detail line
	lines := m.bodyLayoutLines()
	cur := m.clampedCursor(len(lines))
	if !lines[cur].isHeader || !strings.HasSuffix(lines[cur].rowID, ":1") {
		t.Errorf("after collapse, cursor not on the collapsed row's header: line=%+v", lines[cur])
	}
}

// --- finding 1: ToolID is sanitized before it is ever DISPLAYED ---

// assertNoControlRunes fails if v contains a raw ESC or any C0/C1 control
// rune other than the '\n' line separators View itself emits (rune-wise,
// not byte-wise: multi-byte UTF-8 continuation bytes live in 0x80-0xBF and
// would false-positive a byte scan).
func assertNoControlRunes(t *testing.T, v string) {
	t.Helper()
	for _, r := range v {
		if r == 0x1b {
			t.Fatalf("View() contains a raw ESC rune, got:\n%q", v)
		}
		if r < 0x20 && r != '\n' {
			t.Fatalf("View() contains raw control rune %#x, got:\n%q", r, v)
		}
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("View() contains raw C1 control rune %#x, got:\n%q", r, v)
		}
	}
}

// A tool row's expanded detail renders its ToolID (`id=...`), so an ingest
// event carrying an OSC-52/CSI/control-byte ToolID must not reach the
// terminal raw when the row expands — on either the tool_start path or the
// standalone tool_end (no matching start) path.
func TestMonitorTUI_ToolIDSanitizedOnExpand(t *testing.T) {
	dirty := "call" + "\x1b]52;c;YQ==\x07" + "\x1b[31m" + "\n\x01\u009b" + "-1"

	// tool_start path.
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("1", dirty, "builtin", "bash", "args", "ah"))
	m = key(t, m, runeKey(" ")) // expand (following: cursor on the tool row)
	view := m.View()
	assertNoControlRunes(t, view)
	// Not over-sanitized: the plain text around the escapes survives.
	if !strings.Contains(view, "id=call") {
		t.Errorf("expanded tool detail lost the ToolID's plain text, got:\n%s", view)
	}

	// standalone tool_end path (attached mid-tool-call, no start row).
	m2 := NewModel(TUIConfig{})
	m2 = feed(t, m2, mkToolEnd("2", dirty, true, 10, "ok", "rh", 5))
	m2 = key(t, m2, runeKey(" ")) // expand
	view2 := m2.View()
	assertNoControlRunes(t, view2)
	if !strings.Contains(view2, "id=call") {
		t.Errorf("standalone tool_end detail lost the ToolID's plain text, got:\n%s", view2)
	}
}

// --- finding 2: detached cursor/scroll stay anchored to the same ROW ---

// Eviction drops lines off the TOP; a detached (paused) cursor/scrollTop
// held as raw line indices would then silently walk forward through the
// feed. The anchor remap must keep the SAME row selected and the visible
// window on the same content.
func TestMonitorTUI_DetachedCursorStableUnderEviction(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, maxRows) // fill to the cap (cheap: pre-size, still following)
	m = resize(t, m, 80, 10)
	for i := 0; i < 5; i++ {
		m = key(t, m, runeKey("k")) // detach, a few lines above the bottom
	}
	if m.follow {
		t.Fatalf("cursor-up did not detach follow")
	}
	selBefore := m.selectedRowID()
	viewBefore := m.View()

	// Every one of these evicts one row off the top (len already == maxRows).
	for i := 0; i < 50; i++ {
		m = feed(t, m, mkContextEvent("1", uint64(maxRows+i), "skill_loaded", fmt.Sprintf("row-%d", maxRows+i)))
	}

	if got := m.selectedRowID(); got != selBefore {
		t.Errorf("eviction moved the paused selection: before=%q after=%q", selBefore, got)
	}
	if got := m.View(); got != viewBefore {
		t.Errorf("paused view drifted under eviction:\nbefore:\n%s\nafter:\n%s", viewBefore, got)
	}
	if m.follow {
		t.Errorf("eviction re-attached follow")
	}
}

// A tool_end landing on an expanded pending tool row ABOVE the cursor
// inserts detail lines above it — the detached cursor must keep pointing
// at the same row/line content, not slide up onto the inserted line.
func TestMonitorTUI_DetachedCursorStableUnderInsertionAbove(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkToolStart("1", "t1", "builtin", "bash", "go test ./...", "ah"))
	m = key(t, m, runeKey(" ")) // expand the pending tool row (still following)
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded", "ctx-below-a"))
	m = feed(t, m, mkContextEvent("1", 2, "skill_loaded", "ctx-below-b"))

	m = key(t, m, runeKey("k")) // detach onto ctx-below-a's header
	if m.follow {
		t.Fatalf("cursor-up did not detach follow")
	}
	selBefore := m.selectedRowID()
	lines := m.bodyLayoutLines()
	lineBefore := lines[m.clampedCursor(len(lines))].text
	if !strings.Contains(lineBefore, "ctx-below-a") {
		t.Fatalf("test setup: cursor not on ctx-below-a, got line %q", lineBefore)
	}

	// tool_end mutates the expanded tool row above the cursor, adding a
	// "result:" detail line — every line index below it shifts by one.
	m = feed(t, m, mkToolEnd("1", "t1", true, 100, "result-summary", "rh", 50))

	if got := m.selectedRowID(); got != selBefore {
		t.Errorf("insertion above moved the paused selection: before=%q after=%q", selBefore, got)
	}
	lines = m.bodyLayoutLines()
	if got := lines[m.clampedCursor(len(lines))].text; got != lineBefore {
		t.Errorf("cursor line content changed under insertion above:\nbefore: %q\nafter:  %q", lineBefore, got)
	}
}

// --- finding 3: empty-state navigation never detaches follow ---

func TestMonitorTUI_EmptyStateNavKeepsFollow(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = resize(t, m, 80, 12)

	navMsgs := []tea.Msg{
		tea.KeyMsg{Type: tea.KeyUp},
		runeKey("k"),
		tea.KeyMsg{Type: tea.KeyHome},
		tea.KeyMsg{Type: tea.KeyPgUp},
		tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp},
	}
	for _, msg := range navMsgs {
		next, _ := m.Update(msg)
		m = next.(Model)
		if !m.follow {
			t.Fatalf("nav %T(%v) on the empty state detached follow", msg, msg)
		}
	}

	// The first arriving events must therefore be shown, live.
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded", "first-real-row"))
	view := m.View()
	if !strings.Contains(view, "first-real-row") {
		t.Errorf("first event not visible after empty-state nav, got:\n%s", view)
	}
	if !strings.Contains(view, "[following]") {
		t.Errorf("monitor not following after empty-state nav, got:\n%s", view)
	}
}

// --- finding 4: width is measured in display CELLS, not runes ---

func TestMonitorTUI_WideRunesClampedToDisplayWidth(t *testing.T) {
	const width = 40
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded", strings.Repeat("界", 100)))
	m = feed(t, m, mkContextEvent("1", 2, "skill_loaded", "🎉🎉🎉"+strings.Repeat("界", 50)))
	m = resize(t, m, width, 20)
	m = key(t, m, runeKey("k")) // select the wide row so the styled/selected line is covered too

	for i, l := range strings.Split(m.View(), "\n") {
		if w := runewidth.StringWidth(l); w > width {
			t.Errorf("rendered line %d is %d display cells, want <= %d: %q", i, w, width, l)
		}
	}

	// Direct truncateLine check: cell budget respected, ellipsis marks it.
	got := truncateLine(strings.Repeat("界", 30), 21)
	if w := runewidth.StringWidth(got); w > 21 {
		t.Errorf("truncateLine display width = %d, want <= 21: %q", w, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated wide line missing ellipsis: %q", got)
	}
}

// --- finding 5: sized tiny/zero heights clamp TOTAL output ---

func TestMonitorTUI_TinyHeightsClampTotalOutput(t *testing.T) {
	base := NewModel(TUIConfig{})
	base = nRows(t, base, 50)

	withFilter := base
	withFilter = key(t, withFilter, runeKey("/"))
	for _, r := range "row" {
		withFilter = key(t, withFilter, runeKey(string(r)))
	}
	withFilter = key(t, withFilter, tea.KeyMsg{Type: tea.KeyEnter})

	for _, tc := range []struct {
		name string
		m    Model
	}{{"no-filter", base}, {"with-filter", withFilter}} {
		for h := 0; h <= 3; h++ {
			m := resize(t, tc.m, 80, h)
			view := m.View()
			if h == 0 {
				// A real zero-height resize renders NOTHING — height 0
				// must never be mistaken for the pre-size "unbounded"
				// sentinel and dump the whole retained feed.
				if view != "" {
					t.Errorf("%s height=0: want empty render, got %d bytes:\n%s", tc.name, len(view), view)
				}
				continue
			}
			lines := strings.Split(view, "\n")
			if len(lines) > h {
				t.Errorf("%s height=%d: rendered %d lines, want <= %d:\n%s", tc.name, h, len(lines), h, view)
			}
		}
	}

	// Sanity: at height 3 with no filter there IS room for one body line,
	// and follow shows the newest row in it.
	m := resize(t, base, 80, 3)
	if !strings.Contains(m.View(), "row-49") {
		t.Errorf("height=3 (no filter): newest row missing from the single body line, got:\n%s", m.View())
	}
}

// --- finding 6: multiline context detail expands to multiple lines ---

func TestMonitorTUI_ContextDetailMultilineExpands(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkContextEvent("1", 1, "compaction", "first line\nsecond line detail\nthird line detail"))

	// Collapsed: the row summary is the single-line flattened copy.
	collapsed := m.View()
	if !strings.Contains(collapsed, "first line second line detail third line detail") {
		t.Fatalf("row summary not flattened to one line, got:\n%s", collapsed)
	}

	m = key(t, m, runeKey(" ")) // expand
	view := m.View()
	if !strings.Contains(view, "compaction: first line") {
		t.Errorf("expanded context detail missing kind-prefixed first line, got:\n%s", view)
	}
	var sawSecond, sawThird bool
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "second line detail") && !strings.Contains(l, "first line") {
			sawSecond = true
		}
		if strings.Contains(l, "third line detail") && !strings.Contains(l, "second line") {
			sawThird = true
		}
	}
	if !sawSecond || !sawThird {
		t.Errorf("multiline context detail not split into physical lines (second=%v third=%v), got:\n%s", sawSecond, sawThird, view)
	}
}

// --- conversation-first rework: the feed reads like a transcript ---

// mkAssistantResponse builds a provider_response carrying the new
// assistant-output fields (TextPreview/TextBytes/TextHash/ToolCalls).
func mkAssistantResponse(turnID string, status int, stopReason string, usage *monitor.UsageSummary, preview, textHash string, textBytes int, toolCalls []string) monitor.ProviderResponse {
	e := mkProviderResponse(turnID, status, stopReason, usage)
	e.TextPreview = preview
	e.TextHash = textHash
	e.TextBytes = textBytes
	e.ToolCalls = toolCalls
	return e
}

// summaryLine returns the (single) rendered header line of the row whose
// summary contains marker — the collapsed one-liner, not any expanded
// detail line.
func summaryLine(t *testing.T, m Model, marker string) string {
	t.Helper()
	for _, l := range m.bodyLayoutLines() {
		if l.isHeader && strings.Contains(l.text, marker) {
			return l.text
		}
	}
	t.Fatalf("no row summary line contains %q in:\n%s", marker, m.View())
	return ""
}

// (a) The response row's SUMMARY leads with what the assistant said and
// the tools it called — never raw HTTP header text (live feedback: "HTTP
// headers show instead of the actual high-level LLM data").
func TestMonitorTUI_ResponseSummaryLeadsWithAssistantReply(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = resize(t, m, 200, 30) // sized: prove the headline survives a real bounded frame
	resp := mkAssistantResponse("3", 200, "tool_use",
		&monitor.UsageSummary{InputTokens: 37900, OutputTokens: 512, TotalTokens: 38412},
		"The bug is in reconcileScroll; fixing it now.", "text-h1", 1800,
		[]string{"bash", "read"})
	resp.Headers = map[string]string{"x-request-id": "abc-123", "content-type": "application/json"}
	m = feed(t, m, resp)

	line := summaryLine(t, m, "resp 200")
	if !strings.Contains(line, "assistant") {
		t.Errorf("response summary missing the assistant role label, got: %q", line)
	}
	if !strings.Contains(line, "The bug is in reconcileScroll") {
		t.Errorf("response summary missing the assistant text preview, got: %q", line)
	}
	if !strings.Contains(line, "→ bash, read") {
		t.Errorf("response summary missing tool-call names, got: %q", line)
	}
	// The reply must LEAD: preview before the demoted status/usage suffix.
	if strings.Index(line, "The bug is") > strings.Index(line, "resp 200") {
		t.Errorf("response summary leads with diagnostics instead of the reply, got: %q", line)
	}
	// Headers never appear on the summary line (nor anywhere collapsed).
	if strings.Contains(m.View(), "x-request-id") || strings.Contains(m.View(), "application/json") {
		t.Errorf("raw HTTP header text visible on the collapsed feed, got:\n%s", m.View())
	}
}

// (b) The request row's SUMMARY leads with the newest user message — the
// prompt is the headline, model/tokens/sys-bytes are a demoted suffix.
func TestMonitorTUI_RequestSummaryLeadsWithUserMessage(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkProviderRequest("12", "opus-4-8", "h1", 41984, 14, 38000,
		[]monitor.MessageSummary{{Role: "user", Bytes: 40, Hash: "m1", Preview: "why is the build red?"}}))

	line := summaryLine(t, m, "turn 12")
	if !strings.Contains(line, "user") || !strings.Contains(line, "why is the build red?") {
		t.Fatalf("request summary missing user label/preview, got: %q", line)
	}
	if strings.Index(line, "why is the build red?") > strings.Index(line, "opus-4-8") {
		t.Errorf("request summary leads with diagnostics instead of the prompt, got: %q", line)
	}

	// A tool_result-driven turn is labeled as such, not as "user".
	m = feed(t, m, mkProviderRequest("13", "opus-4-8", "h1", 41984, 14, 39000,
		[]monitor.MessageSummary{{Role: "toolResult", Bytes: 900, Hash: "m2", Preview: "exit 0"}}))
	line = summaryLine(t, m, "turn 13")
	if !strings.Contains(line, "(tool result)") {
		t.Errorf("tool_result-driven request row not labeled (tool result), got: %q", line)
	}
}

// (c)+(d) Expanding a response shows the FULL assistant reply first, then
// the — diagnostics — section, with HTTP headers dead last — and headers
// appear ONLY in the expand.
func TestMonitorTUI_ExpandedResponseReplyBeforeDiagnosticsHeadersLast(t *testing.T) {
	fullReply := "Here is the full assistant answer.\nIt spans multiple lines.\nDone."
	fakeBlob := func(h string) (monitor.Blob, bool) {
		if h == "text-h9" {
			return monitor.Blob{Hash: h, Bytes: len(fullReply), Text: fullReply}, true
		}
		return monitor.Blob{}, false
	}
	m := NewModel(TUIConfig{Blob: fakeBlob})
	m = resize(t, m, 200, 40) // bounded frame: ordering must hold in a real sized window
	resp := mkAssistantResponse("4", 200, "end_turn",
		&monitor.UsageSummary{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		"Here is the full assistant answer.", "text-h9", len(fullReply), nil)
	resp.Headers = map[string]string{"x-request-id": "hdr-xyz"}
	m = feed(t, m, resp)

	// Headers absent while collapsed (d).
	if strings.Contains(m.View(), "hdr-xyz") {
		t.Fatalf("header text visible before expand, got:\n%s", m.View())
	}

	m = key(t, m, runeKey("f")) // showFull on
	m = key(t, m, runeKey("h")) // headers on (hidden by default)
	m = key(t, m, runeKey(" ")) // expand (following: cursor on the response row)

	view := m.View()
	iReply := strings.Index(view, "It spans multiple lines.")
	iDiag := strings.Index(view, "— diagnostics —")
	iStatus := strings.Index(view, "status 200")
	iHdr := strings.Index(view, "hdr-xyz")
	if iReply < 0 {
		t.Fatalf("expanded response missing the full assistant reply, got:\n%s", view)
	}
	if iDiag < 0 || iStatus < 0 || iHdr < 0 {
		t.Fatalf("expanded response missing diagnostics/status/headers (%d/%d/%d), got:\n%s", iDiag, iStatus, iHdr, view)
	}
	if !(iReply < iDiag && iDiag < iStatus && iStatus < iHdr) {
		t.Errorf("expanded response order wrong: reply=%d diagnostics=%d status=%d headers=%d, got:\n%s",
			iReply, iDiag, iStatus, iHdr, view)
	}
}

// With showFull OFF, the expanded response still puts the assistant
// PREVIEW before the diagnostics section (conversation first at every
// fidelity level), and the new event-derived fields are sanitized.
func TestMonitorTUI_ExpandedResponsePreviewFirstAndSanitized(t *testing.T) {
	osc52 := "\x1b]52;c;YQ==\x07"
	csi := "\x1b[31m"
	resp := mkAssistantResponse("5", 200, "tool_use", nil,
		"dirty"+osc52+"preview"+csi, "", 40, []string{"ba" + csi + "sh", "re" + osc52 + "ad"})
	m := NewModel(TUIConfig{})
	m = feed(t, m, resp)
	m = key(t, m, runeKey(" ")) // expand

	view := m.View()
	assertNoControlRunes(t, view)
	if !strings.Contains(view, "dirtypreview") {
		t.Errorf("sanitizing ate the preview's plain text, got:\n%s", view)
	}
	if !strings.Contains(view, "bash") || !strings.Contains(view, "read") {
		t.Errorf("sanitizing ate the tool-call names, got:\n%s", view)
	}
	iPrev := strings.Index(view, "dirtypreview")
	iDiag := strings.Index(view, "— diagnostics —")
	if iDiag < 0 || iPrev < 0 || iPrev > iDiag {
		t.Errorf("preview (%d) must precede diagnostics (%d), got:\n%s", iPrev, iDiag, view)
	}
}

// A request row's expand shows the full prompt body(ies) BEFORE its
// diagnostics section (model/system-prompt/tool schema).
func TestMonitorTUI_ExpandedRequestPromptBeforeDiagnostics(t *testing.T) {
	msgBody := "full user prompt body line one\nand line two"
	sysBody := "SYSTEM PROMPT CONTENTS"
	blobs := map[string]monitor.Blob{
		"mh": {Hash: "mh", Bytes: len(msgBody), Text: msgBody},
		"sh": {Hash: "sh", Bytes: len(sysBody), Text: sysBody},
	}
	m := NewModel(TUIConfig{Blob: func(h string) (monitor.Blob, bool) { b, ok := blobs[h]; return b, ok }})
	m = feed(t, m, mkProviderRequest("8", "opus-4-8", "sh", len(sysBody), 0, 500,
		[]monitor.MessageSummary{{Role: "user", Bytes: len(msgBody), Hash: "mh", Preview: "full user prompt body…"}}))
	m = key(t, m, runeKey("f"))
	m = key(t, m, runeKey(" "))

	view := m.View()
	iBody := strings.Index(view, "and line two")
	iDiag := strings.Index(view, "— diagnostics —")
	iSys := strings.Index(view, sysBody)
	if iBody < 0 || iDiag < 0 || iSys < 0 {
		t.Fatalf("expanded request missing body/diagnostics/system prompt (%d/%d/%d), got:\n%s", iBody, iDiag, iSys, view)
	}
	if !(iBody < iDiag && iDiag < iSys) {
		t.Errorf("expanded request order wrong: body=%d diagnostics=%d sys=%d, got:\n%s", iBody, iDiag, iSys, view)
	}
}

// --- session grouping (live feedback: 2 sandboxes = undifferentiated mess) ---

// Two interleaved sessions: group headers appear before each run, rows are
// indented under them, and the line cursor skips the non-selectable
// header lines.
func TestMonitorTUI_GroupHeadersTwoSessions(t *testing.T) {
	m := NewModel(TUIConfig{})

	reqA := mkProviderRequest("1", "opus-a", "ha", 1000, 1, 100, nil)
	reqA.SandboxID, reqA.SessionID = "sbx-alpha", "sess-aaaa1111"
	ctxA := mkContextEvent("1", 5, "skill_loaded", "row-a2")
	ctxA.SandboxID, ctxA.SessionID = "sbx-alpha", "sess-aaaa1111"
	reqB := mkProviderRequest("1", "sonnet-b", "hb", 2000, 2, 200, nil)
	reqB.SandboxID, reqB.SessionID = "sbx-beta", "sess-bbbb2222"
	ctxB := mkContextEvent("1", 6, "skill_loaded", "row-b2")
	ctxB.SandboxID, ctxB.SessionID = "sbx-beta", "sess-bbbb2222"

	m = feed(t, m, reqA)
	m = feed(t, m, ctxA)
	m = feed(t, m, reqB)
	m = feed(t, m, ctxB)

	view := m.View()
	// Group headers: rule + label, sandbox id + short (last-8) session id.
	if !strings.Contains(view, "── sandbox sbx-alpha  ·  session aaaa1111 ──") {
		t.Fatalf("missing session A group header, got:\n%s", view)
	}
	if !strings.Contains(view, "── sandbox sbx-beta  ·  session bbbb2222 ──") {
		t.Fatalf("missing session B group header, got:\n%s", view)
	}
	// Headers sit at column 0 (no gutter, no indent).
	var headerLines int
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "── sandbox ") {
			headerLines++
			if !strings.HasPrefix(l, "──") {
				t.Errorf("group header not at column 0: %q", l)
			}
		}
	}
	if headerLines != 2 {
		t.Errorf("want 2 group header lines, got %d in:\n%s", headerLines, view)
	}
	// Every row line is indented under its header (gutter + 4-space
	// indent before the caret).
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "▸") && !strings.Contains(l, "    ▸") {
			t.Errorf("row line not indented in grouped view: %q", l)
		}
	}

	// Cursor skips headers: from the bottom (ctxB row), two ups cross
	// session B's header and land on session A's ctx row — never on a
	// header (selectedRowID would be "").
	m = key(t, m, runeKey("k")) // -> reqB row
	if id := m.selectedRowID(); !strings.Contains(id, "sess-bbbb2222") || !strings.Contains(id, ":req") {
		t.Fatalf("after k: selected %q, want session B's request row", id)
	}
	m = key(t, m, runeKey("k")) // header skipped -> ctxA row
	if id := m.selectedRowID(); !strings.Contains(id, "sess-aaaa1111") || !strings.Contains(id, "ctx") {
		t.Fatalf("after k,k: selected %q, want session A's ctx row (header must be skipped)", id)
	}

	// g jumps to the top and lands on the first ROW (line 0 is a group
	// header, non-selectable).
	m = key(t, m, runeKey("g"))
	if id := m.selectedRowID(); !strings.Contains(id, "sess-aaaa1111") || !strings.Contains(id, ":req") {
		t.Fatalf("after g: selected %q, want session A's request row", id)
	}
	// Another up: nowhere above but the header — cursor stays on the row.
	m = key(t, m, runeKey("k"))
	if id := m.selectedRowID(); !strings.Contains(id, ":req") || !strings.Contains(id, "sess-aaaa1111") {
		t.Fatalf("up from first row landed on %q, want it to stay on the row", id)
	}
}

// Single-session feed: NO group header, NO indent — flat, exactly like the
// pre-grouping rendering.
func TestMonitorTUI_SingleSessionRendersFlat(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 3) // all rows share the (empty) default session

	view := m.View()
	if strings.Contains(view, "──") || strings.Contains(view, "(local)") {
		t.Fatalf("single-session feed rendered a group header, got:\n%s", view)
	}
	if strings.Contains(view, "    ▸") {
		t.Fatalf("single-session feed indented its rows, got:\n%s", view)
	}
	// The flat two-space gutter + caret shape is intact.
	if !strings.Contains(view, "▸ ") {
		t.Fatalf("row carets missing, got:\n%s", view)
	}
}

// --- `h`: HTTP headers hidden by default, toggled on for req + resp ---

func TestMonitorTUI_HeaderToggleShowsRequestAndResponseHeaders(t *testing.T) {
	m := NewModel(TUIConfig{})

	req := mkProviderRequest("9", "opus-4-8", "h1", 1000, 1, 100, nil)
	req.Headers = map[string]string{"x-req-trace": "req-va\x1b[31mlue"}
	m = feed(t, m, req)
	m = key(t, m, runeKey(" ")) // expand request (following: cursor on it)

	resp := mkProviderResponse("9", 200, "end_turn", nil)
	resp.Headers = map[string]string{"x-resp-id": "resp-value"}
	m = feed(t, m, resp)
	m = key(t, m, runeKey(" ")) // expand response (follow moved cursor to it)

	view := m.View()
	if strings.Contains(view, "hdr ") || strings.Contains(view, "x-req-trace") || strings.Contains(view, "x-resp-id") {
		t.Fatalf("headers visible while h:headers=off (the default), got:\n%s", view)
	}
	if !strings.Contains(view, "h:headers=off") {
		t.Errorf("footer missing h:headers=off, got:\n%s", view)
	}

	m = key(t, m, runeKey("h"))
	view = m.View()
	if !strings.Contains(view, "hdr x-req-trace: req-value") {
		t.Errorf("request headers missing (or unsanitized) after h, got:\n%s", view)
	}
	if !strings.Contains(view, "hdr x-resp-id: resp-value") {
		t.Errorf("response headers missing after h, got:\n%s", view)
	}
	if !strings.Contains(view, "h:headers=on") {
		t.Errorf("footer missing h:headers=on, got:\n%s", view)
	}
	assertNoControlRunes(t, view)

	m = key(t, m, runeKey("h")) // toggle back off
	if strings.Contains(m.View(), "hdr ") {
		t.Errorf("headers still visible after toggling h back off, got:\n%s", m.View())
	}
}

// --- emacs keybindings + harmless left/right arrows ---

func TestMonitorTUI_EmacsKeybindings(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 6) // row-0..row-5, follow on (idx 5)

	m = key(t, m, tea.KeyMsg{Type: tea.KeyCtrlP})
	if idx := selectedIdx(m); idx != 4 {
		t.Fatalf("after ctrl+p: selected index = %d, want 4", idx)
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if idx := selectedIdx(m); idx != 5 {
		t.Fatalf("after ctrl+n: selected index = %d, want 5", idx)
	}

	m = key(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("<"), Alt: true})
	if idx := selectedIdx(m); idx != 0 {
		t.Fatalf("after alt+<: selected index = %d, want 0", idx)
	}
	if m.follow {
		t.Errorf("after alt+<: follow should be detached")
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(">"), Alt: true})
	if idx := selectedIdx(m); idx != 5 {
		t.Fatalf("after alt+>: selected index = %d, want 5", idx)
	}
	if !m.follow {
		t.Errorf("after alt+>: follow should be re-attached")
	}

	// Paging: ctrl+v down, alt+v up (a real page needs a sized frame).
	m = resize(t, m, 80, 8)
	m = nRows(t, m, 24) // 30 rows total
	start := selectedIdx(m)
	m = key(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v"), Alt: true})
	afterUp := selectedIdx(m)
	if afterUp >= start {
		t.Fatalf("alt+v did not page up: before=%d after=%d", start, afterUp)
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyCtrlV})
	afterDown := selectedIdx(m)
	if afterDown <= afterUp {
		t.Fatalf("ctrl+v did not page down: afterUp=%d afterDown=%d", afterUp, afterDown)
	}

	// Left/right arrows: harmless no-ops — no movement, no state flip.
	before := selectedIdx(m)
	m = key(t, m, tea.KeyMsg{Type: tea.KeyLeft})
	m = key(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if idx := selectedIdx(m); idx != before {
		t.Errorf("left/right moved the cursor: before=%d after=%d", before, idx)
	}
}
