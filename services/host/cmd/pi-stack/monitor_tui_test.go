package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
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

	// Same sandbox (the body shows one sandbox at a time now — sandboxes
	// are tabs), two sessions sharing turnId/toolId.
	reqA := mkProviderRequest("1", "opus-4-8", "hash-a", 1000, 1, 100, nil)
	reqA.SandboxID, reqA.SessionID = "sbx", "sess-a"
	reqB := mkProviderRequest("1", "sonnet-5", "hash-b", 2000, 2, 200, nil)
	reqB.SandboxID, reqB.SessionID = "sbx", "sess-b"

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
	reqA2.SandboxID, reqA2.SessionID = "sbx", "sess-a"
	m = feed(t, m, reqA2)
	if !strings.Contains(m.View(), "sys=1000B(unchanged)") {
		t.Errorf("session A's own repeated hash not marked unchanged, got:\n%s", m.View())
	}

	// Session B's turn 1 tool_start (toolId "t1") and session A's own
	// tool_start with the SAME toolId must render as two separate rows.
	toolA := mkToolStart("1", "t1", "builtin", "bash-a", "args-a", "ha")
	toolA.SandboxID, toolA.SessionID = "sbx", "sess-a"
	toolB := mkToolStart("1", "t1", "builtin", "bash-b", "args-b", "hb")
	toolB.SandboxID, toolB.SessionID = "sbx", "sess-b"
	m = feed(t, m, toolA)
	m = feed(t, m, toolB)
	view = m.View()
	if !strings.Contains(view, "bash-a") || !strings.Contains(view, "bash-b") {
		t.Fatalf("expected both sessions' same-toolId tool rows, got:\n%s", view)
	}

	// tool_end on session A's t1 must mutate only session A's row.
	toolEndA := mkToolEnd("1", "t1", true, 100, "ok", "rha", 50)
	toolEndA.SandboxID, toolEndA.SessionID = "sbx", "sess-a"
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

func TestMonitorTUI_CursorMovesWithArrowsAndEmacsKeys(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 5) // row-0 .. row-4, newest (row-4) selected by default (follow)

	if got := m.selectedRowID(); !strings.Contains(got, "4") {
		t.Fatalf("default selection = %q, want the newest row (contains 4)", got)
	}

	// ctrl+p / up move the cursor up (older).
	m = key(t, m, tea.KeyMsg{Type: tea.KeyCtrlP})
	if idx := selectedIdx(m); idx != 3 {
		t.Fatalf("after ctrl+p: selected index = %d, want 3", idx)
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if idx := selectedIdx(m); idx != 2 {
		t.Fatalf("after up: selected index = %d, want 2", idx)
	}

	// ctrl+n / down move it back down.
	m = key(t, m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if idx := selectedIdx(m); idx != 3 {
		t.Fatalf("after ctrl+n: selected index = %d, want 3", idx)
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if idx := selectedIdx(m); idx != 4 {
		t.Fatalf("after down: selected index = %d, want 4 (back at the bottom)", idx)
	}
}

// j and k are deliberately NOT nav keys (the user explicitly doesn't want
// them clobbering vi muscle memory near filter/other future letter keys):
// pressed outside filter mode they must be complete no-ops, never moving
// the cursor or touching follow.
func TestMonitorTUI_JAndKAreNotNavKeys(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 5) // row-0 .. row-4, newest (row-4) selected by default (follow)

	before := selectedIdx(m)
	if !m.follow {
		t.Fatalf("setup: expected follow to be attached")
	}

	m = key(t, m, runeKey("k"))
	if idx := selectedIdx(m); idx != before {
		t.Fatalf("after k: selected index = %d, want unchanged %d (k must be a no-op)", idx, before)
	}
	if !m.follow {
		t.Fatalf("k detached follow — it must be a no-op")
	}

	m = key(t, m, runeKey("j"))
	if idx := selectedIdx(m); idx != before {
		t.Fatalf("after j: selected index = %d, want unchanged %d (j must be a no-op)", idx, before)
	}
	if !m.follow {
		t.Fatalf("j detached follow — it must be a no-op")
	}
}

func TestMonitorTUI_SelectedRowIsHighlighted(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 3)
	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp}) // select row-1 (middle)

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
	// Line 0 is the primary SESSION NODE — navigable, so the cursor lands
	// ON it (selectedRowID is empty there; the node is what's selected).
	lines := m.bodyLayoutLines()
	if cur := m.clampedCursor(lines); cur != 0 || !strings.HasPrefix(lines[cur].nodeID, sessionNodePrefix) {
		t.Fatalf("after g: cursor line %d (nodeID %q), want the session node at line 0", cur, lines[cur].nodeID)
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
	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp})
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

	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp}) // move up: detach
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

	m = key(t, m, runeKey("g"))                   // cursor to the top (session node), detach
	m = key(t, m, tea.KeyMsg{Type: tea.KeyDown})  // the request row header
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

// Bug 2: expanding a response row must show detail (status/stop/usage),
// not be a silent no-op.
func TestMonitorTUI_ExpandResponseRowShowsDetail(t *testing.T) {
	m := NewModel(TUIConfig{})
	resp := mkProviderResponse("7", 200, "tool_use", &monitor.UsageSummary{InputTokens: 100, OutputTokens: 50, TotalTokens: 150})
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

// Fix 7: row headers carry a ▸/▾ expand affordance. (Node headers always
// carry a ▾ by default, so the assertion scopes to the ROW's own line.)
func TestMonitorTUI_ExpandCaretOnRowHeaders(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded", "enrich"))

	rowLine := func(m Model) string {
		for _, l := range m.bodyLayoutLines() {
			if l.isHeader && l.rowID != "" {
				return l.text
			}
		}
		t.Fatalf("no row header line in layout")
		return ""
	}
	if !strings.Contains(rowLine(m), "▸") {
		t.Fatalf("collapsed row header missing ▸ caret, got: %q", rowLine(m))
	}
	m = key(t, m, runeKey(" "))
	if !strings.Contains(rowLine(m), "▾") {
		t.Errorf("expanded row header missing ▾ caret, got: %q", rowLine(m))
	}
	m = key(t, m, runeKey(" "))
	if strings.Contains(rowLine(m), "▾") {
		t.Errorf("collapsed-again row header still shows ▾, got: %q", rowLine(m))
	}
}

// Line-cursor follow semantics: stepping down to the LAST body line
// re-attaches follow; collapsing a row snaps the cursor to its header.
func TestMonitorTUI_LineCursorFollowReattachAndCollapseSnap(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = nRows(t, m, 3)

	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp}) // detach, cursor on row-1's line
	if m.follow {
		t.Fatalf("k did not detach follow")
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyDown}) // back to the last line: re-attach
	if !m.follow {
		t.Errorf("stepping down to the last body line did not re-attach follow")
	}

	// Expand the middle row, walk into its detail, collapse from there:
	// the cursor must snap to that row's header line.
	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp})   // cursor on row-1 header (detached)
	m = key(t, m, runeKey(" "))                  // expand row-1
	m = key(t, m, tea.KeyMsg{Type: tea.KeyDown}) // step onto row-1's detail line
	// nRows rows are ctx events seq 0/1/2; row-1's id ends in ":1" (the
	// seq), which distinguishes it from ":0"/":2" — a bare Contains(":1")
	// would match every row's "ctx:1:" turn segment.
	if got := m.selectedRowID(); !strings.HasSuffix(got, ":1") {
		t.Fatalf("cursor's owning row = %q, want row-1's id (suffix :1)", got)
	}
	m = key(t, m, runeKey(" ")) // collapse from the detail line
	lines := m.bodyLayoutLines()
	cur := m.clampedCursor(lines)
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
		m = key(t, m, tea.KeyMsg{Type: tea.KeyUp}) // detach, a few lines above the bottom
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

	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp}) // detach onto ctx-below-a's header
	if m.follow {
		t.Fatalf("cursor-up did not detach follow")
	}
	selBefore := m.selectedRowID()
	lines := m.bodyLayoutLines()
	lineBefore := lines[m.clampedCursor(lines)].text
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
	if got := lines[m.clampedCursor(lines)].text; got != lineBefore {
		t.Errorf("cursor line content changed under insertion above:\nbefore: %q\nafter:  %q", lineBefore, got)
	}
}

// --- finding 3: empty-state navigation never detaches follow ---

func TestMonitorTUI_EmptyStateNavKeepsFollow(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = resize(t, m, 80, 12)

	navMsgs := []tea.Msg{
		tea.KeyMsg{Type: tea.KeyUp},
		tea.KeyMsg{Type: tea.KeyCtrlP},
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
	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp}) // select the wide row so the styled/selected line is covered too

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

// (c) Expanding a response shows the FULL assistant reply first, then the
// — diagnostics — section (status/usage) — status/usage appear ONLY in
// the expand.
func TestMonitorTUI_ExpandedResponseReplyBeforeDiagnostics(t *testing.T) {
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
	m = feed(t, m, resp)

	// status absent while collapsed.
	if strings.Contains(m.View(), "status 200") {
		t.Fatalf("diagnostics visible before expand, got:\n%s", m.View())
	}

	m = key(t, m, runeKey("f")) // showFull on
	m = key(t, m, runeKey(" ")) // expand (following: cursor on the response row)

	view := m.View()
	iReply := strings.Index(view, "It spans multiple lines.")
	iDiag := strings.Index(view, "— diagnostics —")
	iStatus := strings.Index(view, "status 200")
	if iReply < 0 {
		t.Fatalf("expanded response missing the full assistant reply, got:\n%s", view)
	}
	if iDiag < 0 || iStatus < 0 {
		t.Fatalf("expanded response missing diagnostics/status (%d/%d), got:\n%s", iDiag, iStatus, view)
	}
	if !(iReply < iDiag && iDiag < iStatus) {
		t.Errorf("expanded response order wrong: reply=%d diagnostics=%d status=%d, got:\n%s",
			iReply, iDiag, iStatus, view)
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

// --- collapsible tree: sandbox -> session -> conversation ---

// treeShape flattens the layout into a compact []string signature
// (node:<nodeID> / row:<rowID>, header lines only) so ordering and
// contiguity assertions read clearly.
func treeShape(m Model) []string {
	var out []string
	for _, l := range m.bodyLayoutLines() {
		switch {
		case l.nodeID != "":
			out = append(out, "node:"+l.nodeID)
		case l.isHeader:
			out = append(out, "row:"+l.rowID)
		}
	}
	return out
}

func assertShape(t *testing.T, m Model, want []string) {
	t.Helper()
	got := treeShape(m)
	if len(got) != len(want) {
		t.Fatalf("tree shape = %v,\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tree shape[%d] = %q, want %q\nfull: %v", i, got[i], want[i], got)
		}
	}
}

// sessionNodeLine returns the layout text of the node header for session.
func sessionNodeLine(t *testing.T, m Model, session string) string {
	t.Helper()
	for _, l := range m.bodyLayoutLines() {
		if l.nodeID == sessionNodeID(session) {
			return l.text
		}
	}
	t.Fatalf("no session node line for %q in:\n%s", session, m.View())
	return ""
}

// mkSandboxCtx builds a context event tagged with a sandbox + session.
func mkSandboxCtx(sandbox, session string, seq uint64, detail string) monitor.ContextEvent {
	e := mkContextEvent("1", seq, "skill_loaded", detail)
	e.SandboxID, e.SessionID = sandbox, session
	return e
}

// (a) Two sessions in one sandbox: primary session (first-seen, depth 0)
// with its events -> child session node with its events — each session's
// events CONTIGUOUS and chronological, never interleaved, even though the
// events ARRIVED interleaved. There is NO sandbox node level any more —
// sandboxes are tabs.
func TestMonitorTUI_TreeGroupsInterleavedSessionsContiguously(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkSandboxCtx("pi-stack-dev", "sess-aaaa1111", 1, "a-first"))
	m = feed(t, m, mkSandboxCtx("pi-stack-dev", "sess-bbbb2222", 2, "b-first"))
	m = feed(t, m, mkSandboxCtx("pi-stack-dev", "sess-aaaa1111", 3, "a-second"))
	m = feed(t, m, mkSandboxCtx("pi-stack-dev", "sess-bbbb2222", 4, "b-second"))

	assertShape(t, m, []string{
		"node:" + sessionNodeID("pi-stack-dev/sess-aaaa1111"), // primary = first-seen
		"row:pi-stack-dev/sess-aaaa1111/ctx:1:1",
		"row:pi-stack-dev/sess-aaaa1111/ctx:1:3",
		"node:" + sessionNodeID("pi-stack-dev/sess-bbbb2222"), // child session
		"row:pi-stack-dev/sess-bbbb2222/ctx:1:2",
		"row:pi-stack-dev/sess-bbbb2222/ctx:1:4",
	})

	// Labels + 2-space-per-depth indent (layout text — the timestamp
	// column and cursor gutter are prepended later by renderBodyLines).
	lines := m.bodyLayoutLines()
	if !strings.HasPrefix(lines[0].text, "\u25be aaaa1111") {
		t.Errorf("primary session node = %q, want depth-0 `\u25be aaaa1111…`", lines[0].text)
	}
	if !strings.HasPrefix(lines[1].text, "  \u25b8 ") {
		t.Errorf("primary event row = %q, want depth-1 indent", lines[1].text)
	}
	if !strings.HasPrefix(lines[3].text, "  \u25be bbbb2222") {
		t.Errorf("child session node = %q, want depth-1 `  \u25be bbbb2222…`", lines[3].text)
	}
	if !strings.HasPrefix(lines[4].text, "    \u25b8 ") {
		t.Errorf("child event row = %q, want depth-2 indent", lines[4].text)
	}
}

// (b) Collapsing the primary session node hides its events but leaves the
// child session nodes (and their events) visible; collapsing a child
// hides its events too.
func TestMonitorTUI_CollapseSessionNodes(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkSandboxCtx("pi-stack-dev", "sess-aaaa1111", 1, "a-first"))
	m = feed(t, m, mkSandboxCtx("pi-stack-dev", "sess-bbbb2222", 2, "b-first"))
	m = feed(t, m, mkSandboxCtx("pi-stack-dev", "sess-aaaa1111", 3, "a-second"))
	m = feed(t, m, mkSandboxCtx("pi-stack-dev", "sess-bbbb2222", 4, "b-second"))

	primaryNode := sessionNodeID("pi-stack-dev/sess-aaaa1111")
	childNode := sessionNodeID("pi-stack-dev/sess-bbbb2222")

	// Collapse the PRIMARY session: g lands on its node (line 0), enter
	// toggles — its events hide, the child session node AND the child's
	// events stay visible.
	m = key(t, m, runeKey("g"))
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	assertShape(t, m, []string{
		"node:" + primaryNode,
		"node:" + childNode,
		"row:pi-stack-dev/sess-bbbb2222/ctx:1:2",
		"row:pi-stack-dev/sess-bbbb2222/ctx:1:4",
	})
	if !strings.Contains(m.bodyLayoutLines()[0].text, "\u25b8") {
		t.Errorf("collapsed primary node missing \u25b8 caret: %q", m.bodyLayoutLines()[0].text)
	}

	// Enter again re-expands the whole subtree.
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := len(treeShape(m)); got != 6 {
		t.Fatalf("re-expanded tree has %d header lines, want 6: %v", got, treeShape(m))
	}

	// Collapse the CHILD session: its events hide too. After the
	// re-expand the cursor snapped back to the primary node (line 0);
	// the child node sits below the primary's two event rows.
	for i := 0; i < 3; i++ {
		m = key(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	assertShape(t, m, []string{
		"node:" + primaryNode,
		"row:pi-stack-dev/sess-aaaa1111/ctx:1:1",
		"row:pi-stack-dev/sess-aaaa1111/ctx:1:3",
		"node:" + childNode,
	})
}

// (c) enter/space is CONTEXT-SENSITIVE: on an event row it toggles the
// row's payload expand (never node collapse); on a node header it toggles
// collapse (never payload expand).
func TestMonitorTUI_EnterContextSensitiveRowVsNode(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkContextEvent("1", 1, "skill_loaded", "enrich"))
	rowID := m.rows[0].id

	// Follow puts the cursor on the event row: enter = payload expand.
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.expanded[rowID] {
		t.Fatalf("enter on an event row did not expand its payload")
	}
	if len(m.collapsed) != 0 {
		t.Fatalf("enter on an event row touched node collapse state: %v", m.collapsed)
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.expanded[rowID] {
		t.Fatalf("second enter did not collapse the row payload")
	}

	// Enter on a NODE header toggles collapse, never payload expand.
	m = key(t, m, runeKey("g")) // primary session node (line 0)
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.collapsed[sessionNodeID("/")] {
		t.Fatalf("enter on the session node did not collapse it: %v", m.collapsed)
	}
	if m.expanded[rowID] {
		t.Fatalf("enter on a node expanded a row payload")
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.collapsed[sessionNodeID("/")] {
		t.Fatalf("second enter did not re-expand the session node")
	}
}

// (d) follow tracks the newest EVENT line even when the tree places it
// mid-layout; when its session is collapsed, follow lands on the nearest
// visible ancestor node — never force-expanding it.
func TestMonitorTUI_FollowTracksNewestEventInTree(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkSandboxCtx("sbx", "sess-aaaa1111", 1, "a-first"))
	m = feed(t, m, mkSandboxCtx("sbx", "sess-bbbb2222", 2, "b-first"))
	m = feed(t, m, mkSandboxCtx("sbx", "sess-aaaa1111", 3, "a-second"))

	// Newest event (a-second) belongs to the PRIMARY session, whose block
	// renders ABOVE the child session's — follow must sit on ITS line,
	// mid-layout, not on the last layout line.
	if !m.follow {
		t.Fatalf("expected follow attached")
	}
	if got := m.selectedRowID(); got != "sbx/sess-aaaa1111/ctx:1:3" {
		t.Fatalf("follow selected %q, want the newest event row", got)
	}
	lines := m.bodyLayoutLines()
	if cur := m.clampedCursor(lines); cur == len(lines)-1 {
		t.Fatalf("follow line should be mid-layout (child session renders below), got the last line")
	}

	// Collapse the primary session node (up, up onto it — nodes are
	// navigable), then feed another event for it: follow lands on the
	// session node (nearest visible ancestor), the event stays hidden.
	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp}) // a-first row
	m = key(t, m, tea.KeyMsg{Type: tea.KeyUp}) // primary session node
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.collapsed[sessionNodeID("sbx/sess-aaaa1111")] {
		t.Fatalf("enter on the primary session node did not collapse it")
	}
	if !m.follow {
		t.Fatalf("collapsing the node holding the newest event should leave follow on it (ancestor)")
	}
	m = feed(t, m, mkSandboxCtx("sbx", "sess-aaaa1111", 4, "a-third"))
	lines = m.bodyLayoutLines()
	cur := m.clampedCursor(lines)
	if lines[cur].nodeID != sessionNodeID("sbx/sess-aaaa1111") {
		t.Fatalf("follow line = %+v, want the collapsed session's node header", lines[cur])
	}
	if strings.Contains(m.View(), "a-third") {
		t.Fatalf("collapsed session's new event rendered anyway (force-expand?):\n%s", m.View())
	}
}

// --- sandbox TAB BAR: one tab per sandbox, body = active sandbox only ---

// (e/a) A second sandbox becomes a TAB, not a top-level node: the tab bar
// renders with the active (first-seen) tab bracketed, and the body shows
// ONLY the active sandbox's session tree.
func TestMonitorTUI_TabBarRendersAndBodyShowsActiveSandboxOnly(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkSandboxCtx("box-one", "sess-1", 1, "one-a"))
	m = feed(t, m, mkSandboxCtx("box-two", "sess-2", 2, "two-a"))
	m = feed(t, m, mkSandboxCtx("box-one", "sess-1", 3, "one-b"))

	if m.activeSandbox != "box-one" {
		t.Fatalf("activeSandbox = %q, want the first-seen box-one", m.activeSandbox)
	}
	view := m.View()
	// The tab bar: active tab bracketed (color-independent marker), the
	// background one plain (with its unread • — it got an event while
	// box-one was active).
	if !strings.Contains(view, "[box-one]") {
		t.Errorf("tab bar missing bracketed active tab, got:\n%s", view)
	}
	if !strings.Contains(view, "box-two\u2022") {
		t.Errorf("tab bar missing background tab with unread marker, got:\n%s", view)
	}

	// The BODY is box-one's session tree only — no sandbox node line, no
	// box-two rows.
	assertShape(t, m, []string{
		"node:" + sessionNodeID("box-one/sess-1"),
		"row:box-one/sess-1/ctx:1:1",
		"row:box-one/sess-1/ctx:1:3",
	})
	if !strings.Contains(view, "one-a") || !strings.Contains(view, "one-b") {
		t.Errorf("active sandbox's rows missing from the body, got:\n%s", view)
	}
	if strings.Contains(view, "two-a") {
		t.Errorf("background sandbox's rows leaked into the body, got:\n%s", view)
	}
}

// (b) `tab` switches the active sandbox (wrapping) and the body swaps to
// the other sandbox's sessions; shift+tab and the ]/[ aliases cycle too.
func TestMonitorTUI_TabKeySwitchesActiveSandbox(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkSandboxCtx("box-one", "sess-1", 1, "one-a"))
	m = feed(t, m, mkSandboxCtx("box-two", "sess-2", 2, "two-a"))

	m = key(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.activeSandbox != "box-two" {
		t.Fatalf("after tab: activeSandbox = %q, want box-two", m.activeSandbox)
	}
	if !m.follow {
		t.Errorf("switching tabs did not re-attach follow")
	}
	view := m.View()
	if !strings.Contains(view, "[box-two]") {
		t.Errorf("tab bar active bracket did not move, got:\n%s", view)
	}
	if !strings.Contains(view, "two-a") || strings.Contains(view, "one-a") {
		t.Errorf("body did not swap to box-two's sessions, got:\n%s", view)
	}
	assertShape(t, m, []string{
		"node:" + sessionNodeID("box-two/sess-2"),
		"row:box-two/sess-2/ctx:1:2",
	})

	// tab again wraps around to the first tab.
	m = key(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.activeSandbox != "box-one" {
		t.Fatalf("tab did not wrap around: activeSandbox = %q", m.activeSandbox)
	}
	// shift+tab wraps backward.
	m = key(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.activeSandbox != "box-two" {
		t.Fatalf("shift+tab did not cycle back: activeSandbox = %q", m.activeSandbox)
	}
	// ] / [ aliases.
	m = key(t, m, runeKey("]"))
	if m.activeSandbox != "box-one" {
		t.Fatalf("] did not cycle forward: activeSandbox = %q", m.activeSandbox)
	}
	m = key(t, m, runeKey("["))
	if m.activeSandbox != "box-two" {
		t.Fatalf("[ did not cycle backward: activeSandbox = %q", m.activeSandbox)
	}
}

// (c) A new event in a BACKGROUND sandbox sets its unread marker but does
// not move the active body/cursor; switching to it clears the marker.
func TestMonitorTUI_BackgroundSandboxEventSetsUnreadNotView(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkSandboxCtx("box-one", "sess-1", 1, "one-a"))
	m = feed(t, m, mkSandboxCtx("box-two", "sess-2", 2, "two-a"))
	if m.sandboxUnread["box-two"] != true {
		t.Fatalf("background sandbox's first event did not set unread")
	}

	selBefore := m.selectedRowID()
	bodyBefore := treeShape(m)

	// Another background event: unread stays lit, active body unmoved.
	m = feed(t, m, mkSandboxCtx("box-two", "sess-2", 3, "two-b"))
	if !m.sandboxUnread["box-two"] {
		t.Fatalf("background event did not keep unread set")
	}
	if got := m.selectedRowID(); got != selBefore {
		t.Errorf("background event moved the cursor: before=%q after=%q", selBefore, got)
	}
	after := treeShape(m)
	if len(after) != len(bodyBefore) {
		t.Fatalf("background event changed the active body: before=%v after=%v", bodyBefore, after)
	}
	for i := range after {
		if after[i] != bodyBefore[i] {
			t.Fatalf("background event changed the active body: before=%v after=%v", bodyBefore, after)
		}
	}
	if strings.Contains(m.View(), "two-b") {
		t.Errorf("background sandbox's new event rendered in the active view:\n%s", m.View())
	}

	// Switching to it clears the marker (and shows the rows).
	m = key(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.sandboxUnread["box-two"] {
		t.Errorf("switching to the sandbox did not clear its unread marker")
	}
	view := m.View()
	if strings.Contains(view, "box-two\u2022") {
		t.Errorf("tab bar still shows the unread marker after switching, got:\n%s", view)
	}
	if !strings.Contains(view, "two-b") {
		t.Errorf("switched-to sandbox's newest event not shown (follow re-attach), got:\n%s", view)
	}
}

// (d) A single sandbox renders NO tab bar; its name lives in the title.
func TestMonitorTUI_SingleSandboxNoTabBarNameInTitle(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkSandboxCtx("solo-box", "sess-1", 1, "solo-a"))

	view := m.View()
	if strings.Contains(view, "[solo-box]") {
		t.Errorf("single sandbox rendered a tab bar entry, got:\n%s", view)
	}
	if !strings.Contains(view, "pi-stack monitor  solo-box  events=1") {
		t.Errorf("title line missing the single sandbox's name, got:\n%s", view)
	}
	// Chrome accounting agrees: no tab line.
	if _, tabs, _, _ := m.chrome(); tabs {
		t.Errorf("chrome() wants a tab bar with a single sandbox")
	}
}

// (e) Digit keys jump straight to the Nth sandbox tab; out-of-range
// digits are no-ops. While typing a filter, digits are filter text.
func TestMonitorTUI_DigitKeyJumpsToSandbox(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, mkSandboxCtx("box-one", "sess-1", 1, "one-a"))
	m = feed(t, m, mkSandboxCtx("box-two", "sess-2", 2, "two-a"))
	m = feed(t, m, mkSandboxCtx("box-three", "sess-3", 3, "three-a"))

	m = key(t, m, runeKey("3"))
	if m.activeSandbox != "box-three" {
		t.Fatalf("digit 3 did not jump to the 3rd sandbox: activeSandbox = %q", m.activeSandbox)
	}
	if !strings.Contains(m.View(), "three-a") {
		t.Errorf("body did not swap to the jumped-to sandbox, got:\n%s", m.View())
	}
	m = key(t, m, runeKey("9")) // no 9th tab: no-op
	if m.activeSandbox != "box-three" {
		t.Fatalf("out-of-range digit changed the active sandbox: %q", m.activeSandbox)
	}
	m = key(t, m, runeKey("1"))
	if m.activeSandbox != "box-one" {
		t.Fatalf("digit 1 did not jump back to the 1st sandbox: %q", m.activeSandbox)
	}

	// Digits typed while filtering are filter text, never a tab jump.
	m = key(t, m, runeKey("/"))
	m = key(t, m, runeKey("2"))
	if m.activeSandbox != "box-one" {
		t.Fatalf("digit while filtering switched tabs: %q", m.activeSandbox)
	}
	if m.filterInput != "2" {
		t.Fatalf("filterInput = %q, want %q", m.filterInput, "2")
	}
}

// A single sandbox + single session still renders as the session tree —
// consistent and labeled, with NO sandbox line (the sandbox name lives in
// the title line when there's only one).
func TestMonitorTUI_SingleSessionStillRendersTree(t *testing.T) {
	m := NewModel(TUIConfig{})
	// Disable the (on-by-default) timestamp column so its blank prefix
	// doesn't shift the indent assertions below.
	m = key(t, m, runeKey("T"))
	m = nRows(t, m, 3) // all rows share the (empty) default sandbox/session

	lines := m.bodyLayoutLines()
	if lines[0].nodeID != sessionNodeID("/") || !strings.HasPrefix(lines[0].text, "\u25be -") {
		t.Fatalf("line 0 = %+v, want the (unnamed) session node at depth 0", lines[0])
	}
	for _, l := range lines[1:] {
		if !l.isHeader || !strings.HasPrefix(l.text, "  \u25b8 ") {
			t.Fatalf("event row not at depth 1: %+v", l)
		}
	}
	// The (local) sandbox name is in the title line, not the body.
	if !strings.Contains(m.headerLine(), "pi-stack monitor  (local)  events=3") {
		t.Errorf("single-sandbox title missing the sandbox name, got: %q", m.headerLine())
	}
}

// --- session node carries per-session model + first-user-prompt title ---

// TestMonitorTUI_SessionNodeShowsModelAndFirstUserPromptTitle locks in the
// enriched session node label: it must show the session's model, and a
// title taken from the FIRST user-triggered request only — a later prompt
// in the same session must never overwrite it, and a tool_result-triggered
// request (the tool's own output replayed to the model, not something the
// user typed) must never set it at all.
func TestMonitorTUI_SessionNodeShowsModelAndFirstUserPromptTitle(t *testing.T) {
	m := NewModel(TUIConfig{})

	first := mkProviderRequest("1", "claude-opus-4-8", "h1", 1000, 1, 100,
		[]monitor.MessageSummary{{Role: "user", Bytes: 10, Hash: "m1", Preview: "can you send a test ping to gemini"}})
	first.Trigger = "user"
	m = feed(t, m, first)

	line := sessionNodeLine(t, m, "/")
	if !strings.Contains(line, "claude-opus-4-8") {
		t.Fatalf("session node missing the session's model, got: %q", line)
	}
	if !strings.Contains(line, "\u201ccan you send a test ping to gemini\u201d") {
		t.Fatalf("session node missing the first user prompt as its title, got: %q", line)
	}

	// A tool_result-triggered request must never set (or change) the title.
	toolReq := mkProviderRequest("2", "claude-opus-4-8", "h1", 1000, 1, 100,
		[]monitor.MessageSummary{{Role: "tool", Bytes: 20, Hash: "m2", Preview: "tool result text"}})
	toolReq.Trigger = "tool_result"
	m = feed(t, m, toolReq)

	// A later user prompt must not overwrite the FIRST one.
	later := mkProviderRequest("3", "claude-opus-4-8", "h1", 1000, 1, 100,
		[]monitor.MessageSummary{{Role: "user", Bytes: 10, Hash: "m3", Preview: "a completely different later ask"}})
	later.Trigger = "user"
	m = feed(t, m, later)

	line = sessionNodeLine(t, m, "/")
	if !strings.Contains(line, "\u201ccan you send a test ping to gemini\u201d") {
		t.Fatalf("session node title changed away from the FIRST user prompt, got: %q", line)
	}
	if strings.Contains(line, "tool result text") || strings.Contains(line, "a completely different later ask") {
		t.Fatalf("session node title picked up a non-first/tool_result request, got: %q", line)
	}
}

// TestMonitorTUI_SessionNodePerSessionModelDiffers proves the model in the
// node label is per-SESSION, not a single global last-seen value: a child
// session (e.g. a `pi -p` provider call) running a different model from
// the main session must show ITS OWN model in its own node label.
func TestMonitorTUI_SessionNodePerSessionModelDiffers(t *testing.T) {
	m := NewModel(TUIConfig{})

	parent := mkProviderRequest("1", "claude-opus-4-8", "hp", 1000, 1, 100, nil)
	parent.SandboxID, parent.SessionID = "sbx", "sess-parent"
	child := mkProviderRequest("1", "gemini-2.5-flash", "hc", 500, 0, 50, nil)
	child.SandboxID, child.SessionID = "sbx", "sess-child"

	m = feed(t, m, parent)
	m = feed(t, m, child)

	if got := sessionNodeLine(t, m, "sbx/sess-parent"); !strings.Contains(got, "s-parent \u00b7 claude-opus-4-8") {
		t.Fatalf("parent session node missing its own model, got: %q", got)
	}
	if got := sessionNodeLine(t, m, "sbx/sess-child"); !strings.Contains(got, "ss-child \u00b7 gemini-2.5-flash") {
		t.Fatalf("child session node missing its own (different) model, got: %q", got)
	}
}

// Collapse state is evicted with a node's last retained row, exactly like
// prevSysHash/sessionModel/sessionTitle (R4-2 class), so a long-running
// monitor never accumulates one collapsed entry per node ever collapsed.
// The sandbox TAB state (sandboxOrder/sandboxUnread/activeSandbox) is
// cleaned up the same way: an evicted sandbox loses its tab, and an
// evicted ACTIVE sandbox falls back to the first remaining tab.
func TestMonitorTUI_CollapsedStateEvictedWithLastRow(t *testing.T) {
	m := NewModel(TUIConfig{})
	first := mkProviderRequest("1", "opus", "h", 100, 0, 10, nil)
	first.SandboxID, first.SessionID = "sbx-0", "sess-0"
	m = feed(t, m, first)
	m.collapsed[sessionNodeID("sbx-0/sess-0")] = true
	if m.activeSandbox != "sbx-0" {
		t.Fatalf("activeSandbox = %q, want the first-seen sbx-0", m.activeSandbox)
	}

	for i := 1; i <= maxRows+100; i++ {
		req := mkProviderRequest("1", "opus", "h", 100, 0, 10, nil)
		req.SandboxID, req.SessionID = fmt.Sprintf("sbx-%d", i), fmt.Sprintf("sess-%d", i)
		m = feed(t, m, req)
	}

	if _, ok := m.collapsed[sessionNodeID("sbx-0/sess-0")]; ok {
		t.Errorf("collapsed still holds the evicted session's node entry")
	}
	if len(m.sandboxRowCount) != maxRows {
		t.Errorf("len(sandboxRowCount) = %d, want %d (one live sandbox per retained row)", len(m.sandboxRowCount), maxRows)
	}
	// Tab state is bounded by live sandboxes too, and the evicted active
	// tab fell back to the (new) first tab.
	if len(m.sandboxOrder) != maxRows {
		t.Errorf("len(sandboxOrder) = %d, want %d (one tab per live sandbox)", len(m.sandboxOrder), maxRows)
	}
	if m.activeSandbox != m.sandboxOrder[0] {
		t.Errorf("activeSandbox = %q, want fallback to the first remaining tab %q", m.activeSandbox, m.sandboxOrder[0])
	}
	if _, ok := m.sandboxUnread["sbx-0"]; ok {
		t.Errorf("sandboxUnread still holds the evicted sandbox's entry")
	}

	// A LIVE node's collapse state survives eviction churn.
	live := fmt.Sprintf("sbx-%d/sess-%d", maxRows+100, maxRows+100)
	m.collapsed[sessionNodeID(live)] = true
	m = feed(t, m, mkSandboxCtx("sbx-extra", "sess-extra", 9, "evict-one-more"))
	if !m.collapsed[sessionNodeID(live)] {
		t.Errorf("a live session's collapse state was dropped by unrelated eviction")
	}
}

// --- tool-result continuation requests are hidden from the feed ---

// TestMonitorTUI_ToolResultTriggeredRequestHidden locks in the
// transcript-shape fix: in a multi-step tool conversation, each tool
// result is fed back to the model as the next turn's provider_request —
// but that request row would just duplicate the tool row's `→ ok ...`
// line right above it. A provider_request with Trigger="tool_result"
// must not appear in visibleRows (or the rendered feed) at all, while an
// ordinary Trigger="user" request row still does, and the tool-result
// turn's own response row still renders normally (assistant rows are
// never hidden, only the redundant request row is).
func TestMonitorTUI_ToolResultTriggeredRequestHidden(t *testing.T) {
	m := NewModel(TUIConfig{})

	// Turn 1: a real user prompt kicks things off.
	userReq := mkProviderRequest("1", "opus-4-8", "h1", 1000, 1, 100,
		[]monitor.MessageSummary{{Role: "user", Bytes: 10, Hash: "m1", Preview: "do the thing"}})
	userReq.Trigger = "user"
	m = feed(t, m, userReq)
	m = feed(t, m, mkProviderResponse("1", 200, "tool_use", nil))
	m = feed(t, m, mkToolStart("1", "call_1", "builtin", "read", "path=foo", "a1"))
	m = feed(t, m, mkToolEnd("1", "call_1", true, 128, "ok", "r1", 50))

	// Turn 2: the tool's own result is fed back as this turn's request —
	// Trigger="tool_result" — and must be hidden.
	toolReq := mkProviderRequest("2", "opus-4-8", "h1", 1000, 1, 100,
		[]monitor.MessageSummary{{Role: "tool", Bytes: 128, Hash: "m2", Preview: "ok"}})
	toolReq.Trigger = "tool_result"
	m = feed(t, m, toolReq)
	m = feed(t, m, mkProviderResponse("2", 200, "end_turn", nil))

	rows := m.visibleRows()
	for _, r := range rows {
		if r.kind == rowKindRequest && r.trigger == "tool_result" {
			t.Fatalf("tool_result-triggered request row is visible: turn=%s", r.turnID)
		}
	}

	var sawUserReq, sawTurn2Resp bool
	for _, r := range rows {
		if r.kind == rowKindRequest && r.turnID == "1" && r.trigger == "user" {
			sawUserReq = true
		}
		if r.kind == rowKindResponse && r.turnID == "2" {
			sawTurn2Resp = true
		}
	}
	if !sawUserReq {
		t.Errorf("turn 1's user-triggered request row should be visible")
	}
	if !sawTurn2Resp {
		t.Errorf("turn 2's response row should still render even though its request was hidden")
	}

	// Also confirmed at the View level: the hidden turn's tool-result
	// preview text never appears as a request-row headline.
	view := m.View()
	if strings.Contains(view, "tool     \u201cok\u201d") {
		t.Errorf("hidden tool_result request row rendered in the feed, got:\n%s", view)
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
	// Top of the tree is the primary session node (navigable) — the
	// cursor lands on it.
	lines := m.bodyLayoutLines()
	if cur := m.clampedCursor(lines); cur != 0 || !strings.HasPrefix(lines[cur].nodeID, sessionNodePrefix) {
		t.Fatalf("after alt+<: cursor line %d, want the session node at line 0", cur)
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

// --- SESSION FOCUS (solo) mode: `s` ---

// sessionOfRow looks up the session key that owns id in m.rows — a small
// helper so focus tests can assert "which session does the selected row
// belong to" without duplicating the lookup inline.
func sessionOfRow(m Model, id string) string {
	for _, r := range m.rows {
		if r.id == id {
			return r.session
		}
	}
	return ""
}

func TestMonitorTUI_FocusSoloesSelectedSessionAndBackAgain(t *testing.T) {
	m := NewModel(TUIConfig{})

	// One sandbox, two sessions (focus solos a SESSION within the active
	// sandbox tab — cross-sandbox is what the tab bar is for now).
	reqA1 := mkProviderRequest("1", "opus-4-8", "hash-a", 100, 0, 10, nil)
	reqA1.SandboxID, reqA1.SessionID = "sbx", "sess-a"
	reqB1 := mkProviderRequest("1", "sonnet-5", "hash-b", 100, 0, 10, nil)
	reqB1.SandboxID, reqB1.SessionID = "sbx", "sess-b"
	reqA2 := mkProviderRequest("2", "opus-4-8", "hash-a", 100, 0, 10, nil)
	reqA2.SandboxID, reqA2.SessionID = "sbx", "sess-a"
	reqB2 := mkProviderRequest("2", "sonnet-5", "hash-b", 100, 0, 10, nil)
	reqB2.SandboxID, reqB2.SessionID = "sbx", "sess-b"

	// Interleaved arrival: A, B, A, B — the exact "concurrent sessions
	// interleave" shape the focus feature exists to collapse.
	m = feed(t, m, reqA1)
	m = feed(t, m, reqB1)
	m = feed(t, m, reqA2)
	m = feed(t, m, reqB2)

	if got := len(m.visibleRows()); got != 4 {
		t.Fatalf("want 4 rows before focus, got %d", got)
	}

	// Land the cursor on session A's row: follow starts on the newest
	// event (session B's turn 2); session A's whole subtree renders ABOVE
	// session B's, so walking up crosses B's rows and B's session node
	// header (a navigable line) before landing on session A's rows.
	for i := 0; i < 4; i++ {
		m = key(t, m, tea.KeyMsg{Type: tea.KeyUp})
	}
	if sess := sessionOfRow(m, m.selectedRowID()); sess != "sbx/sess-a" {
		t.Fatalf("cursor not on session A's row before focus, got session %q (row %q)", sess, m.selectedRowID())
	}

	m = key(t, m, runeKey("s"))
	if m.focusSession != "sbx/sess-a" {
		t.Fatalf("focusSession = %q, want sbx/sess-a", m.focusSession)
	}
	rows := m.visibleRows()
	if len(rows) != 2 {
		t.Fatalf("want only session A's 2 rows visible when focused, got %d", len(rows))
	}
	for _, r := range rows {
		if r.session != "sbx/sess-a" {
			t.Fatalf("visibleRows leaked a non-focused session row: %+v", r)
		}
	}
	view := m.View()
	if !strings.Contains(view, "opus-4-8") {
		t.Fatalf("focused view missing session A's model, got:\n%s", view)
	}
	if strings.Contains(view, "sonnet-5") {
		t.Fatalf("focused view leaked session B's rows, got:\n%s", view)
	}
	// SOLO render: only the focused session's node — no sibling session
	// nodes.
	var nodes int
	for _, l := range m.bodyLayoutLines() {
		if l.nodeID != "" {
			nodes++
		}
	}
	if nodes != 1 {
		t.Fatalf("solo view: want exactly the focused session's node line, got %d node lines", nodes)
	}
	if !strings.Contains(view, "focus sbx/sess-a") {
		t.Fatalf("top bar missing focus context, got:\n%s", view)
	}

	// A new event for the OTHER session while focused must stay filtered
	// and must not steal follow (visibleRows still only session A's rows).
	reqB3 := mkProviderRequest("3", "sonnet-5", "hash-b", 100, 0, 10, nil)
	reqB3.SandboxID, reqB3.SessionID = "sbx", "sess-b"
	m = feed(t, m, reqB3)
	if got := len(m.visibleRows()); got != 2 {
		t.Fatalf("want still only 2 rows visible (session B's new event hidden), got %d", got)
	}
	if strings.Contains(m.View(), "sonnet-5") {
		t.Fatalf("new event for the unfocused session leaked into the view:\n%s", m.View())
	}

	// `s` again toggles focus OFF: every row returns, including the one
	// that arrived while focused.
	m = key(t, m, runeKey("s"))
	if m.focusSession != "" {
		t.Fatalf("focusSession = %q, want \"\" after second s", m.focusSession)
	}
	if got := len(m.visibleRows()); got != 5 {
		t.Fatalf("want all 5 rows back after unfocus, got %d", got)
	}
	if !strings.Contains(m.View(), "sonnet-5") {
		t.Fatalf("unfocused view missing session B's model, got:\n%s", m.View())
	}
}

// TestMonitorTUI_FocusNoOpOnUntiedLine confirms `s` is a no-op when the
// cursor sits on an untied line (the empty-state hint) rather than a real
// row — there is nothing to focus on.
func TestMonitorTUI_FocusNoOpOnUntiedLine(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = key(t, m, runeKey("s"))
	if m.focusSession != "" {
		t.Fatalf("focusSession = %q, want \"\" (no-op on the empty feed)", m.focusSession)
	}
}

// TestMonitorTUI_EscClearsFocus covers the nice-to-have: `esc` outside
// filtering/help also clears an active focus.
func TestMonitorTUI_EscClearsFocus(t *testing.T) {
	m := NewModel(TUIConfig{})
	req := mkProviderRequest("1", "opus-4-8", "h1", 100, 0, 10, nil)
	req.SandboxID, req.SessionID = "sbx-a", "sess-a"
	m = feed(t, m, req)
	m = key(t, m, runeKey("s"))
	if m.focusSession == "" {
		t.Fatalf("expected focus to be set before esc")
	}
	m = key(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focusSession != "" {
		t.Fatalf("focusSession = %q, want \"\" after esc", m.focusSession)
	}
}

// TestMonitorTUI_FocusFooterAndHelpAdvertiseState covers the footer's
// compact focus segment and the help overlay's `s` line.
func TestMonitorTUI_FocusFooterAndHelpAdvertiseState(t *testing.T) {
	m := NewModel(TUIConfig{})
	if !strings.Contains(m.footerLine(), "s:focus=off") {
		t.Fatalf("footer missing unfocused state, got: %s", m.footerLine())
	}
	req := mkProviderRequest("1", "opus-4-8", "h1", 100, 0, 10, nil)
	req.SandboxID, req.SessionID = "sbx-a", "sess-a"
	m = feed(t, m, req)
	m = key(t, m, runeKey("s"))
	if !strings.Contains(m.footerLine(), "[focus]") {
		t.Fatalf("footer missing focused state, got: %s", m.footerLine())
	}

	m.showHelp = true
	m.cursor, m.scrollTop, m.follow = 0, 0, false
	if !strings.Contains(m.View(), "focus/unfocus the selected session") {
		t.Fatalf("help overlay missing the `s` line, got:\n%s", m.View())
	}
}

// TestMonitorTUI_FocusEmptyStateWhenSessionHasNoRowsInView covers the edge
// case where the focused session's rows are no longer in the visible set
// (e.g. all evicted) — focus stays on, and a dedicated empty-state line
// tells the user how to get back to the full feed.
func TestMonitorTUI_FocusEmptyStateWhenSessionHasNoRowsInView(t *testing.T) {
	m := NewModel(TUIConfig{})
	req := mkProviderRequest("1", "opus-4-8", "h1", 100, 0, 10, nil)
	req.SandboxID, req.SessionID = "sbx-a", "sess-a"
	m = feed(t, m, req)
	// Focus a session with no rows at all (simulating "the focused
	// session's rows are all evicted") directly rather than churning
	// maxRows rows through eviction.
	m.focusSession = "sbx-a/gone"
	view := m.View()
	if !strings.Contains(view, "focused session has no events in view") {
		t.Fatalf("expected the focused-empty hint, got:\n%s", view)
	}
	if !strings.Contains(view, "press s to unfocus") {
		t.Fatalf("expected the unfocus hint, got:\n%s", view)
	}
}

// --- timestamps (`T`) + model latency ---

// TestMonitorTUI_ShowTimestampsOnByDefaultRendersLocalTime feeds a row with
// a known envelope ts and asserts the rendered HH:MM:SS (local time) shows
// up in View() with showTimestamps left at its default (on).
func TestMonitorTUI_ShowTimestampsOnByDefaultRendersLocalTime(t *testing.T) {
	m := NewModel(TUIConfig{})
	if !m.showTimestamps {
		t.Fatalf("showTimestamps should default to true")
	}
	ts := time.Date(2024, 3, 14, 9, 5, 32, 0, time.Local).UnixMilli()
	req := mkProviderRequest("1", "opus-4-8", "h1", 100, 0, 10, nil)
	req.TS = ts
	m = feed(t, m, req)

	want := time.UnixMilli(ts).Format("15:04:05")
	if !strings.Contains(m.View(), want) {
		t.Fatalf("View() missing formatted timestamp %q, got:\n%s", want, m.View())
	}
}

// TestMonitorTUI_ToggleTimestampsRemovesColumn covers `T`: pressing it
// once (default on) hides the HH:MM:SS column; pressing it again restores
// it.
func TestMonitorTUI_ToggleTimestampsRemovesColumn(t *testing.T) {
	m := NewModel(TUIConfig{})
	ts := time.Date(2024, 3, 14, 9, 5, 32, 0, time.Local).UnixMilli()
	req := mkProviderRequest("1", "opus-4-8", "h1", 100, 0, 10, nil)
	req.TS = ts
	m = feed(t, m, req)

	want := time.UnixMilli(ts).Format("15:04:05")
	if !strings.Contains(m.View(), want) {
		t.Fatalf("View() missing formatted timestamp before toggle, got:\n%s", m.View())
	}
	if !strings.Contains(m.footerLine(), "T:time=on") {
		t.Fatalf("footer missing T:time=on, got: %s", m.footerLine())
	}

	m = key(t, m, runeKey("T"))
	if m.showTimestamps {
		t.Fatalf("showTimestamps still true after toggling T")
	}
	if strings.Contains(m.View(), want) {
		t.Fatalf("View() still shows timestamp after T toggled off, got:\n%s", m.View())
	}
	if !strings.Contains(m.footerLine(), "T:time=off") {
		t.Fatalf("footer missing T:time=off, got: %s", m.footerLine())
	}

	// Toggle back on: the column returns.
	m = key(t, m, runeKey("T"))
	if !strings.Contains(m.View(), want) {
		t.Fatalf("View() missing timestamp after toggling T back on, got:\n%s", m.View())
	}
}

// TestMonitorTUI_HelpLineAdvertisesTimestampToggle covers the `T   toggle
// timestamps` help line.
func TestMonitorTUI_HelpLineAdvertisesTimestampToggle(t *testing.T) {
	m := NewModel(TUIConfig{})
	m.showHelp = true
	m.cursor, m.scrollTop, m.follow = 0, 0, false
	if !strings.Contains(m.View(), "toggle timestamps") {
		t.Fatalf("help overlay missing the `T` line, got:\n%s", m.View())
	}
}

// TestMonitorTUI_ResponseLatencyComputedFromRequestTS feeds a request at
// ts=T and its matching response at ts=T+1300 and asserts the response row
// shows the humanDuration-formatted "1.3s" latency.
func TestMonitorTUI_ResponseLatencyComputedFromRequestTS(t *testing.T) {
	m := NewModel(TUIConfig{})
	const baseTS int64 = 1_700_000_000_000

	req := mkProviderRequest("1", "opus-4-8", "h1", 100, 0, 10, nil)
	req.TS = baseTS
	m = feed(t, m, req)

	resp := mkProviderResponse("1", 200, "stop", &monitor.UsageSummary{InputTokens: 2, OutputTokens: 17})
	resp.TS = baseTS + 1300
	m = feed(t, m, resp)

	view := m.View()
	if !strings.Contains(view, "1.3s") {
		t.Fatalf("View() missing computed latency 1.3s, got:\n%s", view)
	}
	// Sanity: the usage segment this latency is appended after is still
	// there (the requirement is ADD, not replace).
	if !strings.Contains(view, "in 2 out 17") {
		t.Fatalf("View() missing usage segment alongside latency, got:\n%s", view)
	}
}

// TestMonitorTUI_ResponseLatencyOmittedWhenRequestTSUnknown covers the
// unknown case: a response for a turn with no recorded request TS (e.g.
// the TUI attached mid-turn, or a synthetic event that never set ts) shows
// no latency segment at all — never "0ms" or a bogus value.
func TestMonitorTUI_ResponseLatencyOmittedWhenRequestTSUnknown(t *testing.T) {
	m := NewModel(TUIConfig{})
	resp := mkProviderResponse("1", 200, "stop", &monitor.UsageSummary{InputTokens: 2, OutputTokens: 17})
	resp.TS = 1_700_000_000_000
	m = feed(t, m, resp)

	idx, ok := m.rowIndex[m.rows[0].id]
	if !ok {
		t.Fatalf("expected the response row to be indexed")
	}
	if got := m.rows[idx].latencyMs; got != 0 {
		t.Fatalf("latencyMs = %d, want 0 (no matching request ts)", got)
	}
	view := m.View()
	if strings.Contains(view, "ms") || strings.Contains(view, "0s") {
		t.Fatalf("View() rendered a bogus latency with no matching request ts, got:\n%s", view)
	}
}

// --- humanDuration unit cases ---

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		ms   int
		want string
	}{
		{0, "0ms"},
		{820, "820ms"},
		{999, "999ms"},
		{1300, "1.3s"},
		{4300, "4.3s"},
		{62000, "1m2s"},
	}
	for _, c := range cases {
		if got := humanDuration(c.ms); got != c.want {
			t.Errorf("humanDuration(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

// --- spawn correlation: child pi sessions nest under the tool that ran them ---

// mkTurnStart builds a turn_start tagged with sandbox/session/ts — the
// typical FIRST event a freshly-booted child pi session emits, carrying
// the model the correlation heuristic keys on.
func mkTurnStart(sandbox, session, turnID, model string, ts int64) monitor.TurnStart {
	e := monitor.TurnStart{Model: model}
	e.SandboxID, e.SessionID, e.TurnID, e.TS = sandbox, session, turnID, ts
	return e
}

// spawnTool builds a bash tool_start (the `pi -p ...` spawner shape)
// tagged with sandbox/session/ts.
func spawnTool(sandbox, session, toolID, args string, ts int64) monitor.ToolStart {
	e := mkToolStart("1", toolID, "builtin", "bash", args, "ah-"+toolID)
	e.SandboxID, e.SessionID, e.TS = sandbox, session, ts
	return e
}

// spawnCtx builds a context event row tagged with sandbox/session/ts.
func spawnCtx(sandbox, session string, seq uint64, detail string, ts int64) monitor.ContextEvent {
	e := mkContextEvent("1", seq, "skill_loaded", detail)
	e.SandboxID, e.SessionID, e.TS = sandbox, session, ts
	return e
}

// (a) A primary session runs a bash tool whose args contain the child's
// model id; the child session then boots. The child session node must
// render nested immediately UNDER that tool row — in place in the
// conversation, before the primary's later rows — one level deeper, not
// piled at the end of the primary session.
func TestMonitorTUI_ChildSessionNestsUnderSpawningToolRow(t *testing.T) {
	m := NewModel(TUIConfig{})
	req := mkProviderRequest("1", "claude-opus-4-8", "h1", 100, 1, 10,
		[]monitor.MessageSummary{{Role: "user", Bytes: 5, Hash: "m1", Preview: "go"}})
	req.SandboxID, req.SessionID = "sbx", "sess-primary1"
	req.TS = 1000
	m = feed(t, m, req)
	m = feed(t, m, spawnTool("sbx", "sess-primary1", "call_1", `pi -p --model gemini-3.6-flash "summarize"`, 2000))
	// A later PRIMARY event, so "nested under the tool" is distinguishable
	// from "dumped at the end".
	m = feed(t, m, spawnCtx("sbx", "sess-primary1", 9, "later-primary-event", 2500))

	// Child session boots: first event is turn_start carrying the model
	// (with a provider/ prefix the matcher must strip), then an event row.
	m = feed(t, m, mkTurnStart("sbx", "sess-child111", "1", "google/gemini-3.6-flash", 3000))
	m = feed(t, m, spawnCtx("sbx", "sess-child111", 1, "child-event", 3100))

	if got := m.sessionParentRow["sbx/sess-child111"]; got != "sbx/sess-primary1/1/tool:call_1" {
		t.Fatalf("sessionParentRow = %q, want the spawning tool row id", got)
	}
	assertShape(t, m, []string{
		"node:" + sessionNodeID("sbx/sess-primary1"),
		"row:sbx/sess-primary1/1:req",
		"row:sbx/sess-primary1/1/tool:call_1",
		"node:" + sessionNodeID("sbx/sess-child111"), // nested right under the tool
		"row:sbx/sess-child111/ctx:1:1",
		"row:sbx/sess-primary1/ctx:1:9", // primary flow continues after
	})
	// Depths: tool row depth 1 -> child session node depth 2 -> child
	// event depth 3 (2 spaces per depth in the layout text).
	lines := m.bodyLayoutLines()
	if !strings.HasPrefix(lines[2].text, "  \u25b8") {
		t.Errorf("tool row = %q, want depth-1 indent", lines[2].text)
	}
	if !strings.HasPrefix(lines[3].text, "    \u25be child111") {
		t.Errorf("child session node = %q, want depth-2 `    \u25be child111…`", lines[3].text)
	}
	if !strings.HasPrefix(lines[4].text, "      \u25b8") {
		t.Errorf("child event row = %q, want depth-3 indent", lines[4].text)
	}
}

// (b) Three parallel `pi -p` tool rows with different --model args, three
// child sessions: each child nests under the tool whose args contain ITS
// model id (provider/ prefix stripped, case-insensitive), not merely the
// most recent tool.
func TestMonitorTUI_ParallelChildSessionsNestUnderMatchingTools(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, spawnTool("sbx", "sess-primary1", "call_g", `pi -p --model gemini-3.6-flash "a"`, 1000))
	m = feed(t, m, spawnTool("sbx", "sess-primary1", "call_o", `pi -p --model gpt-5.6-sol "b"`, 1001))
	m = feed(t, m, spawnTool("sbx", "sess-primary1", "call_q", `pi -p --model qwen3:8b "c"`, 1002))

	// Children boot out of order relative to their tools.
	m = feed(t, m, mkTurnStart("sbx", "sess-childqqq", "1", "ollama/qwen3:8b", 2000))
	m = feed(t, m, spawnCtx("sbx", "sess-childqqq", 1, "q-event", 2001))
	m = feed(t, m, mkTurnStart("sbx", "sess-childggg", "1", "google/gemini-3.6-flash", 2100))
	m = feed(t, m, spawnCtx("sbx", "sess-childggg", 1, "g-event", 2101))
	m = feed(t, m, mkTurnStart("sbx", "sess-childooo", "1", "openai/gpt-5.6-sol", 2200))
	m = feed(t, m, spawnCtx("sbx", "sess-childooo", 1, "o-event", 2201))

	assertShape(t, m, []string{
		"node:" + sessionNodeID("sbx/sess-primary1"),
		"row:sbx/sess-primary1/1/tool:call_g",
		"node:" + sessionNodeID("sbx/sess-childggg"),
		"row:sbx/sess-childggg/ctx:1:1",
		"row:sbx/sess-primary1/1/tool:call_o",
		"node:" + sessionNodeID("sbx/sess-childooo"),
		"row:sbx/sess-childooo/ctx:1:1",
		"row:sbx/sess-primary1/1/tool:call_q",
		"node:" + sessionNodeID("sbx/sess-childqqq"),
		"row:sbx/sess-childqqq/ctx:1:1",
	})
}

// (c) A child session with NO matching tool row (model matches nothing,
// and the only tool's window closed long before the child booted) must
// fall back to the old behavior: a primary-level child node at the end —
// never a wild guess.
func TestMonitorTUI_ChildSessionNoMatchFallsBackToPrimaryLevel(t *testing.T) {
	m := NewModel(TUIConfig{})
	tool := spawnTool("sbx", "sess-primary1", "call_1", `ls -la`, 1000)
	m = feed(t, m, tool)
	end := mkToolEnd("1", "call_1", true, 10, "done", "rh", 500)
	end.SandboxID, end.SessionID = "sbx", "sess-primary1"
	end.TS = 1500
	m = feed(t, m, end) // window [1000, 1500] — closed well before the child

	m = feed(t, m, mkTurnStart("sbx", "sess-child111", "1", "mystery-model", 5000))
	m = feed(t, m, spawnCtx("sbx", "sess-child111", 1, "child-event", 5001))

	if got := m.sessionParentRow["sbx/sess-child111"]; got != "" {
		t.Fatalf("sessionParentRow = %q, want \"\" (no plausible spawner)", got)
	}
	assertShape(t, m, []string{
		"node:" + sessionNodeID("sbx/sess-primary1"),
		"row:sbx/sess-primary1/1/tool:call_1",
		"node:" + sessionNodeID("sbx/sess-child111"), // primary-level child, at the end
		"row:sbx/sess-child111/ctx:1:1",
	})
	// Depth 1 node, depth 2 events — the pre-existing child layout.
	lines := m.bodyLayoutLines()
	if !strings.HasPrefix(lines[2].text, "  \u25be child111") {
		t.Errorf("fallback child node = %q, want depth-1 indent", lines[2].text)
	}
}

// (d) Evicting the parent tool row (maxRows churn) must leave the child
// session rendering as a primary-level child node — no crash, no orphaned
// nesting under a row that no longer exists.
func TestMonitorTUI_EvictedParentToolRowChildFallsBack(t *testing.T) {
	m := NewModel(TUIConfig{})
	m = feed(t, m, spawnTool("sbx", "sess-primary1", "call_1", `pi -p --model gemini-3.6-flash "x"`, 1000))
	// Fill the primary up to exactly maxRows retained rows.
	for i := 0; i < maxRows-1; i++ {
		m = feed(t, m, spawnCtx("sbx", "sess-primary1", uint64(10+i), fmt.Sprintf("filler-%d", i), int64(2000+i)))
	}
	// Child boots while the tool row is still retained: correlation lands.
	m = feed(t, m, mkTurnStart("sbx", "sess-child111", "1", "google/gemini-3.6-flash", 900000))
	if got := m.sessionParentRow["sbx/sess-child111"]; got != "sbx/sess-primary1/1/tool:call_1" {
		t.Fatalf("sessionParentRow = %q, want the tool row id before eviction", got)
	}
	// The child's first ROW pushes the count past maxRows and evicts the
	// oldest row — the parent tool row.
	m = feed(t, m, spawnCtx("sbx", "sess-child111", 1, "child-event", 900001))
	if _, ok := m.rowIndex["sbx/sess-primary1/1/tool:call_1"]; ok {
		t.Fatalf("parent tool row still retained; test needs it evicted")
	}

	shape := treeShape(m)
	if len(shape) != maxRows+2 { // primary node + (maxRows-1) fillers + child node + child row
		t.Fatalf("tree shape has %d header lines, want %d", len(shape), maxRows+2)
	}
	if want := "node:" + sessionNodeID("sbx/sess-child111"); shape[len(shape)-2] != want {
		t.Fatalf("shape[-2] = %q, want fallback child node %q", shape[len(shape)-2], want)
	}
	if want := "row:sbx/sess-child111/ctx:1:1"; shape[len(shape)-1] != want {
		t.Fatalf("shape[-1] = %q, want child event row %q", shape[len(shape)-1], want)
	}
	// View() must render without panicking, with the child at depth 1.
	_ = m.View()
	lines := m.bodyLayoutLines()
	if !strings.HasPrefix(lines[len(lines)-2].text, "  \u25be child111") {
		t.Errorf("fallback child node = %q, want depth-1 indent", lines[len(lines)-2].text)
	}
}
