// monitor_tui.go — Unit B of `pi-stack monitor` (see
// .pi-agent/deliver/monitor/architecture.md Section 1 Unit B, Section 3.B).
//
// This is the bubbletea TUI half: it renders a live-follow view of the
// monitor.Hub's event stream (Section 3.A) in-process, over a plain Go
// channel — no HTTP/SSE round-trip (that seam is `monitor.Hub.Subscribe()`).
//
// Ownership: this file + monitor_tui_test.go ONLY. It does not touch
// monitor.go, main.go, help.go, or the monitor/ package (those are Unit A/C).
//
// Contract with Unit C (monitor.go, not owned here): C constructs a Hub,
// calls hub.Subscribe() for the Events channel and passes hub.Blob for the
// Blob func, then calls RunTUI(cfg). Everything below is exactly the API
// pinned in architecture.md Section 3.B — no more, no less.
package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"pi-stack/host/monitor"
)

// maxRows bounds retained rows so a long-running `pi-stack monitor` session
// doesn't grow rows/rowIndex/expanded for the life of the process (review
// finding R1-13: unbounded row growth). A few thousand rows is already far
// more scrollback than any terminal shows at once.
const maxRows = 4000

// TUIConfig wires the TUI to the hub. Events is the in-process subscriber
// channel from hub.Subscribe(); Blob is hub.Blob (in-process cache lookup,
// no HTTP); Filter is the sandbox name/id filter label shown in the header
// (the actual event filtering already happened hub-side — this is display
// only).
type TUIConfig struct {
	Events <-chan monitor.Event
	Blob   func(hash string) (monitor.Blob, bool)
	Filter string

	// Port is the hub's bound port, shown in the empty-state hint below so
	// a user staring at "(no events yet)" knows which port to expect a
	// monitor-enabled sandbox to reach. 0 falls back to monitor.DefaultPort
	// (unit C always sets this from the resolved --port flag).
	Port int
}

// rowKind groups a rendered row by shape. It is distinct from monitor.Kind
// because a tool row combines a tool_start and its later tool_end into ONE
// row (mutated in place when tool_end arrives), matching the design doc's
// single-line `tool ... → ok 2.1KB 4.3s` rendering — there is no 1:1 mapping
// from wire event kind to row kind for tools.
type rowKind int

const (
	rowKindRequest rowKind = iota
	rowKindResponse
	rowKindTool
	rowKindContext
)

// tuiRow is one rendered line (plus optional expanded detail) in the feed.
// Rows are keyed by `id` — a composite `sandboxId/sessionId/turnId` (plus
// toolId for tool rows, review finding R1-7) rather than bare turnId/toolId,
// because turnId resets to "1" every session and the default `monitor`
// watches every sandbox at once: two different sessions' turn 1 (or two
// sessions that happen to reuse the same toolId) would otherwise collide
// and overwrite each other. The composite key lets tool_end still find and
// mutate the tool_start row it completes, and lets `space` toggle a
// specific row's expanded state, without cross-session bleed.
type tuiRow struct {
	id      string
	session string // sessionKey(env) this row belongs to — see Model.sessionRowCount
	turnID  string
	kind    rowKind

	// request
	model        string
	sysHash      string
	sysBytes     int
	sysUnchanged bool
	msgDelta     int
	newMessages  []monitor.MessageSummary
	toolCount    int
	toolNames    []string
	mcpToolNames []string
	estTokens    int

	// response
	status     int
	stopReason string
	usage      *monitor.UsageSummary

	// tool (start fields + end fields merged into one row)
	toolID        string
	source        string
	name          string
	argsSummary   string
	argsHash      string
	toolDone      bool
	ok            bool
	resultBytes   int
	resultSummary string
	resultHash    string
	durationMs    int

	// context
	ctxKind string
	detail  string

	// resolved (and sanitized) blob text for the expanded detail view,
	// stored here by Update — never fetched from View (review finding
	// R1-12: View must be a pure function of Model state, not of the
	// hub's mutable blob cache). Populated only while the row is BOTH
	// expanded (m.expanded[id]) and showFull is on, and cleared back to
	// "" the moment either stops being true (review finding R3-2b: a
	// full body retained on every one of up to maxRows rows for the
	// row's whole lifetime — even after the row is collapsed — could
	// reach multiple GB; bounding retention to what's actually on screen
	// keeps it to the handful of rows the user has expanded at once).
	// See clearRowBlobs/clearAllResolvedBlobs and resolveRowBlobs.
	sysPromptText string
	argsText      string
	resultText    string

	// full-payload contract (review finding R2-6): resolved bodies for
	// each of the request row's newMessages (parallel to newMessages,
	// resolved by nm.Hash) and for the tool schema sent this turn
	// (resolved by toolSchemaHash) — populated by resolveRowBlobs, same
	// retention rule (and same clearing on collapse/showFull-off, R3-2b)
	// as sysPromptText/argsText/resultText: never fetched from View.
	newMessageTexts []string
	toolSchemaHash  string
	toolSchemaText  string
}

// eventMsg / eventsClosedMsg bridge the plain `<-chan monitor.Event` into
// bubbletea's message loop (architecture.md 3.B wiring note): a tea.Cmd
// blocks on one channel receive and returns it as a tea.Msg; Update
// re-issues the same Cmd after each event so the read loop continues for the
// life of the program.
type eventMsg struct{ event monitor.Event }
type eventsClosedMsg struct{}

// Model is the bubbletea model. View() is a PURE function of this state —
// no I/O, no channel reads, no Blob calls beyond what's already wired via
// cfg.Blob (also side-effect-free from the TUI's point of view: it's an
// in-memory cache lookup) — so tests can feed synthetic events through
// Update and assert on View()'s output without a real terminal or hub.
type Model struct {
	cfg TUIConfig

	rows     []tuiRow
	rowIndex map[string]int // row id -> index into rows, for tool_end merge + expand lookup

	turnModel string // last seen turn_start.Model, shown in the header
	// prevSysHash is compared against each provider_request's
	// SystemPromptHash to render "(unchanged)" vs "(new)" — keyed by
	// sessionKey(env) (sandboxId+"/"+sessionId), NOT global, because the
	// default `monitor` watches every sandbox/session at once and a
	// single shared value would let one session's hash mark another
	// session's first-seen prompt as "(unchanged)" (review finding R1-7).
	prevSysHash map[string]string

	// sessionRowCount tracks how many currently-retained rows belong to
	// each session (sessionKey(env)), so evictOldRows can tell when a
	// session's LAST row is gone and it's safe to drop that session's
	// prevSysHash entry too. Without this, prevSysHash accumulates one
	// entry per distinct session forever — even after every row for that
	// session has been evicted from rows/rowIndex/expanded — because
	// nothing ever removes it (review finding R4-2, same class as
	// R1-13). Incremented in upsertRow when a NEW row is inserted (never
	// on an in-place overwrite/mutate, which doesn't change row count),
	// decremented in evictOldRows for each dropped row.
	sessionRowCount map[string]int

	expanded map[string]bool // row id -> expanded, toggled by `space`

	filtering   bool // `/` was pressed; subsequent runes build filterInput
	filterInput string
	filter      string // committed filter (Enter); substring match on the rendered row line

	showFull     bool // f
	showModel    bool // m
	showTools    bool // t
	showMCP      bool // p
	showThinking bool // x
	showContext  bool // c
}

// NewModel constructs a Model with the design doc's default toggle state:
// model/tool/mcp/context rows visible, full payloads/thinking off (see
// docs/design/monitor.md: "off = summaries", "off by default, behind a
// toggle").
func NewModel(cfg TUIConfig) Model {
	return Model{
		cfg:             cfg,
		rowIndex:        make(map[string]int),
		prevSysHash:     make(map[string]string),
		sessionRowCount: make(map[string]int),
		expanded:        make(map[string]bool),
		showModel:       true,
		showTools:       true,
		showMCP:         true,
		showContext:     true,
	}
}

// Init starts the event-read loop (Section 3.B: "re-issued on each Update").
func (m Model) Init() tea.Cmd {
	if m.cfg.Events == nil {
		return nil
	}
	return waitForEvent(m.cfg.Events)
}

// waitForEvent returns a tea.Cmd that blocks on one channel receive.
func waitForEvent(events <-chan monitor.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-events
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg{event: e}
	}
}

// Update applies one message: an event from the hub, or a keypress.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case eventMsg:
		m.applyEvent(msg.event)
		return m, waitForEvent(m.cfg.Events)
	case eventsClosedMsg:
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	default:
		return m, nil
	}
}

// handleKey implements the toggle/filter/expand/quit keymap from
// architecture.md 3.B. `ctrl+c` always quits, even while typing a filter (it
// is a control signal, never filter text). `q` only quits outside filter
// input mode, so a filter string containing "q" can actually be typed.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.filtering {
		switch key {
		case "enter":
			m.filtering = false
			m.filter = m.filterInput
		case "esc":
			m.filtering = false
			m.filterInput = ""
		case "backspace":
			if m.filterInput != "" {
				r := []rune(m.filterInput)
				m.filterInput = string(r[:len(r)-1])
			}
		default:
			// A single printable rune (letters, digits, punctuation, space)
			// extends the filter text; anything else (arrow keys, tab, …)
			// is ignored rather than mis-typed into the filter.
			if utf8.RuneCountInString(key) == 1 {
				m.filterInput += key
			}
		}
		return m, nil
	}
	switch key {
	case "q":
		return m, tea.Quit
	case "f":
		m.showFull = !m.showFull
		if m.showFull {
			m.refreshExpandedBlobs()
		} else {
			// Nothing renders full bodies with showFull off, so nothing
			// should keep retaining them either (review finding R3-2b).
			m.clearAllResolvedBlobs()
		}
	case "m":
		m.showModel = !m.showModel
	case "t":
		m.showTools = !m.showTools
	case "p":
		m.showMCP = !m.showMCP
	case "x":
		m.showThinking = !m.showThinking
	case "c":
		m.showContext = !m.showContext
	case "/":
		m.filtering = true
		m.filterInput = ""
	case " ":
		m.toggleExpandLast()
	}
	return m, nil
}

// toggleExpandLast expands/collapses the most recent VISIBLE row — "current
// turn pinned" per the design doc. There is no up/down row cursor in the
// architecture.md 3.B keymap, so `space` acts on the row the user is
// currently looking at: whatever is newest under the active filter/toggles.
func (m *Model) toggleExpandLast() {
	rows := m.visibleRows()
	if len(rows) == 0 {
		return
	}
	id := rows[len(rows)-1].id
	m.expanded[id] = !m.expanded[id]
	if m.expanded[id] {
		if m.showFull {
			m.resolveRowBlobs(id)
		}
	} else {
		// Collapsing: drop this row's resolved full-body text so it can
		// be GC'd rather than retained for the rest of the row's
		// lifetime (review finding R3-2b). The small summary fields
		// (hashes, previews, byte counts) are untouched.
		m.clearRowBlobs(id)
	}
}

// applyEvent updates row state from one decoded monitor.Event. It type
// switches on the concrete Go type (matching monitor's "interface +
// concrete structs" decision, architecture.md 2.3) rather than Kind(), so
// hand-built synthetic events in tests need not set the embedded envelope's
// Kind field correctly.
func (m *Model) applyEvent(e monitor.Event) {
	env := e.Envelope()
	sess := sessionKey(env)
	switch ev := e.(type) {
	case monitor.TurnStart:
		m.turnModel = sanitizeText(ev.Model, false)

	case monitor.ProviderRequest:
		hash := ev.Summary.SystemPromptHash
		prevHash := m.prevSysHash[sess]
		unchanged := hash != "" && prevHash != "" && hash == prevHash
		if hash != "" {
			m.prevSysHash[sess] = hash
		}
		id := sess + "/" + env.TurnID + ":req"
		m.upsertRow(tuiRow{
			id:             id,
			session:        sess,
			turnID:         sanitizeText(env.TurnID, false),
			kind:           rowKindRequest,
			model:          sanitizeText(ev.Model, false),
			sysHash:        hash,
			sysBytes:       ev.Summary.SystemPromptBytes,
			sysUnchanged:   unchanged,
			msgDelta:       len(ev.Summary.NewMessages),
			newMessages:    sanitizeMessages(ev.Summary.NewMessages),
			toolCount:      ev.Summary.ToolCount,
			toolNames:      sanitizeStrings(ev.Summary.ToolNames),
			mcpToolNames:   sanitizeStrings(ev.Summary.McpToolNames),
			estTokens:      ev.Summary.EstTokens,
			toolSchemaHash: ev.Summary.ToolSchemaHash,
		})
		// Only re-resolve while the row is both expanded AND showFull is
		// on (review finding R3-2b) — resolving-but-not-displaying would
		// retain a full body for nothing.
		if m.expanded[id] && m.showFull {
			m.resolveRowBlobs(id)
		}

	case monitor.ProviderResponse:
		id := sess + "/" + env.TurnID + ":resp"
		m.upsertRow(tuiRow{
			id:         id,
			session:    sess,
			turnID:     sanitizeText(env.TurnID, false),
			kind:       rowKindResponse,
			status:     ev.Status,
			stopReason: sanitizeText(ev.StopReason, false),
			usage:      ev.Usage,
		})

	case monitor.ToolStart:
		// Keyed by turnId+toolId (review finding R2-2), not toolId alone: a
		// provider can reuse a toolId (e.g. "call_1") across two turns in
		// the SAME session, and without turnId in the key the second
		// turn's tool_start would overwrite the first turn's row, and a
		// late tool_end would mutate the wrong turn's row.
		id := sess + "/" + env.TurnID + "/tool:" + ev.ToolID
		m.upsertRow(tuiRow{
			id:          id,
			session:     sess,
			turnID:      sanitizeText(env.TurnID, false),
			kind:        rowKindTool,
			toolID:      ev.ToolID,
			source:      sanitizeText(ev.Source, false),
			name:        sanitizeText(ev.Name, false),
			argsSummary: sanitizeText(ev.ArgsSummary, false),
			argsHash:    ev.ArgsHash,
		})
		// See the ProviderRequest case above: gated on showFull too (R3-2b).
		if m.expanded[id] && m.showFull {
			m.resolveRowBlobs(id)
		}

	case monitor.ToolEnd:
		// Same turnId+toolId key as ToolStart (review finding R2-2).
		id := sess + "/" + env.TurnID + "/tool:" + ev.ToolID
		if idx, ok := m.rowIndex[id]; ok {
			r := m.rows[idx]
			r.ok = ev.OK
			r.resultBytes = ev.ResultBytes
			r.resultSummary = sanitizeText(ev.ResultSummary, false)
			r.resultHash = ev.ResultHash
			r.durationMs = ev.DurationMs
			r.toolDone = true
			m.rows[idx] = r
		} else {
			// tool_end with no matching tool_start (e.g. TUI attached
			// mid-tool-call): render it standalone rather than drop it.
			m.upsertRow(tuiRow{
				id:            id,
				session:       sess,
				turnID:        sanitizeText(env.TurnID, false),
				kind:          rowKindTool,
				toolID:        ev.ToolID,
				ok:            ev.OK,
				resultBytes:   ev.ResultBytes,
				resultSummary: sanitizeText(ev.ResultSummary, false),
				resultHash:    ev.ResultHash,
				durationMs:    ev.DurationMs,
				toolDone:      true,
			})
		}
		// The result hash/text only exists once tool_end lands, so a row the
		// user already expanded (pending, on the tool_start) needs its
		// blob text refreshed now rather than staying "(body not
		// captured)" forever — but again only while showFull is actually on
		// (R3-2b).
		if m.expanded[id] && m.showFull {
			m.resolveRowBlobs(id)
		}

	case monitor.ContextEvent:
		// Seq disambiguates multiple context events within the same turn
		// (there is no natural sub-id like tool_id for this kind).
		m.upsertRow(tuiRow{
			id:      fmt.Sprintf("%s/ctx:%s:%d", sess, env.TurnID, env.Seq),
			session: sess,
			turnID:  sanitizeText(env.TurnID, false),
			kind:    rowKindContext,
			ctxKind: sanitizeText(ev.CtxKind, false),
			detail:  sanitizeText(ev.Detail, false),
		})
	}
}

// sessionKey returns the composite (sandbox, session) key used to scope row
// identity and prevSysHash so two different sandboxes' or sessions' state
// never collide (review finding R1-7) — turnId and toolId are only unique
// WITHIN one session (turnId restarts at "1" every session, and the default
// `monitor` watches every sandbox at once).
func sessionKey(env monitor.Envelope) string {
	return env.SandboxID + "/" + env.SessionID
}

// sanitizeText strips everything a malicious /ingest event could use to
// drive the host terminal (review finding R1-8): ANSI escape sequences —
// CSI (`ESC [ ... final-byte`) and OSC (`ESC ] ... BEL` or `ESC ] ... ESC
// \`, e.g. an OSC-52 clipboard write) — and every remaining C0/C1 control
// byte (0x00-0x1F, 0x7F-0x9F). \r is dropped outright (a classic
// overwrite-the-line trick); \t becomes a space; \n becomes a space too
// UNLESS keepNewlines is true, which callers set only for a full multi-line
// blob body about to be rendered across several lines (never for a
// single-line summary field, where an embedded newline could otherwise
// forge an extra row in the feed). Applied to every event-derived string
// before it is stored on a row, so nothing unsanitized ever reaches View.
func sanitizeText(s string, keepNewlines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			i += ansiSeqLen(s[i:])
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			// Invalid UTF-8 byte: drop it rather than emit garbage.
			i++
			continue
		}
		switch {
		case r == '\r':
			// drop: a bare CR is a line-overwrite trick, not content.
		case r == '\n':
			if keepNewlines {
				b.WriteRune(r)
			} else {
				b.WriteByte(' ')
			}
		case r == '\t':
			b.WriteByte(' ')
		case r <= 0x1f, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// remaining C0/C1 control chars: drop.
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// ansiSeqLen returns the length (in bytes, from s[0], which must be ESC
// 0x1b) of a recognized ANSI escape sequence, so sanitizeText can skip it
// wholesale rather than leaving its guts to be filtered byte-by-byte (CSI
// parameter bytes like `31` or `52;c;YQ==` are ordinary printable
// characters that would otherwise pass straight through). Recognizes CSI
// (`ESC [ params... final-byte 0x40-0x7E`) and OSC (`ESC ] ... BEL` or
// `ESC ] ... ESC \`, the OSC-52 clipboard-write shape). Any other
// two-character ESC sequence (e.g. `ESC c`) is treated generically as two
// bytes. An unterminated CSI/OSC consumes the rest of the string — safe,
// since there is nothing after it but more of the same untrusted payload.
func ansiSeqLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b {
		return 1
	}
	switch s[1] {
	case '[':
		i := 2
		for i < len(s) {
			if c := s[i]; c >= 0x40 && c <= 0x7e {
				return i + 1
			}
			i++
		}
		return i
	case ']':
		i := 2
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			i++
		}
		return i
	default:
		return 2
	}
}

// sanitizeStrings maps sanitizeText (single-line: keepNewlines=false) over
// a slice, preserving nil vs. empty so an unset ToolNames/McpToolNames
// stays unset rather than becoming a spurious empty (non-nil) slice.
func sanitizeStrings(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = sanitizeText(s, false)
	}
	return out
}

// sanitizeMessages sanitizes every event-derived string field of a
// provider_request's NewMessages (Role and Preview — Bytes/Hash are
// server-computed, not attacker text) before they're stored on a row.
func sanitizeMessages(msgs []monitor.MessageSummary) []monitor.MessageSummary {
	if msgs == nil {
		return nil
	}
	out := make([]monitor.MessageSummary, len(msgs))
	for i, mm := range msgs {
		out[i] = monitor.MessageSummary{
			Role:    sanitizeText(mm.Role, false),
			Bytes:   mm.Bytes,
			Hash:    mm.Hash,
			Preview: sanitizeText(mm.Preview, false),
		}
	}
	return out
}

// upsertRow inserts a new row or overwrites an existing one in place (same
// id), preserving row order (position of first appearance), then evicts the
// oldest rows if the cap was just exceeded (review finding R1-13).
func (m *Model) upsertRow(row tuiRow) {
	if idx, ok := m.rowIndex[row.id]; ok {
		// In-place overwrite/mutate: row count for this session is
		// unchanged, so sessionRowCount is untouched (only insertion below
		// changes how many rows a session has retained).
		m.rows[idx] = row
		return
	}
	m.rowIndex[row.id] = len(m.rows)
	m.rows = append(m.rows, row)
	if row.session != "" {
		m.sessionRowCount[row.session]++
	}
	m.evictOldRows()
}

// evictOldRows drops the oldest rows once len(m.rows) exceeds maxRows,
// removing their rowIndex and expanded entries too so neither map leaks —
// a row's resolved blob text lives on the tuiRow itself, so it is dropped
// for free along with the row (review finding R1-13). It also decrements
// sessionRowCount for each dropped row's session, and once a session's
// count hits zero — meaning no retained row belongs to it any more —
// deletes both sessionRowCount and prevSysHash for that session (review
// finding R4-2: prevSysHash otherwise keeps one entry per distinct session
// forever, even long after every row for that session is gone). A live
// session (one that still has at least one retained row) is never touched,
// so delta ((unchanged) vs (new)) computation for it keeps working.
func (m *Model) evictOldRows() {
	if len(m.rows) <= maxRows {
		return
	}
	drop := len(m.rows) - maxRows
	for _, r := range m.rows[:drop] {
		delete(m.rowIndex, r.id)
		delete(m.expanded, r.id)
		if r.session == "" {
			continue
		}
		m.sessionRowCount[r.session]--
		if m.sessionRowCount[r.session] <= 0 {
			delete(m.sessionRowCount, r.session)
			delete(m.prevSysHash, r.session)
		}
	}
	m.rows = append([]tuiRow(nil), m.rows[drop:]...)
	for i, r := range m.rows {
		m.rowIndex[r.id] = i
	}
}

// visibleRows applies the show* toggles and the active text filter, in that
// order, preserving row order.
func (m Model) visibleRows() []tuiRow {
	var out []tuiRow
	for _, r := range m.rows {
		if !m.passesToggles(r) {
			continue
		}
		if m.filter != "" && !strings.Contains(strings.ToLower(m.renderRow(r)), strings.ToLower(m.filter)) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (m Model) passesToggles(r tuiRow) bool {
	switch r.kind {
	case rowKindRequest, rowKindResponse:
		return m.showModel
	case rowKindTool:
		if !m.showTools {
			return false
		}
		if strings.HasPrefix(r.source, "mcp:") && !m.showMCP {
			return false
		}
		return true
	case rowKindContext:
		if !m.showContext {
			return false
		}
		if r.ctxKind == "thinking_level" && !m.showThinking {
			return false
		}
		return true
	default:
		return true
	}
}

// View renders the current state. PURE: no channel reads, no goroutines, no
// tea.Program access — see the Model doc comment and architecture.md 3.B
// ("View() must be a PURE function of Model state").
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render(m.headerLine()))
	b.WriteString("\n")
	switch {
	case m.filtering:
		b.WriteString(filterStyle.Render("filter> " + m.filterInput))
		b.WriteString("\n")
	case m.filter != "":
		b.WriteString(filterStyle.Render("filter: " + m.filter))
		b.WriteString("\n")
	}
	rows := m.visibleRows()
	if len(rows) == 0 {
		b.WriteString("(no events yet)\n")
		b.WriteString(fmt.Sprintf("waiting for a monitor-enabled sandbox on :%d — if nothing appears, the sandbox may predate the monitor extension (rebuild image / make load)\n", m.hubPort()))
	} else {
		for _, r := range rows {
			b.WriteString(m.renderRow(r))
			b.WriteString("\n")
			if m.expanded[r.id] {
				for _, line := range m.detailLines(r) {
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
		}
	}
	b.WriteString(footerStyle.Render(m.footerLine()))
	return b.String()
}

// hubPort returns cfg.Port, falling back to monitor.DefaultPort when unset
// (0) — the empty-state hint always names a concrete port even when a test
// or caller left TUIConfig.Port at its zero value.
func (m Model) hubPort() int {
	if m.cfg.Port != 0 {
		return m.cfg.Port
	}
	return monitor.DefaultPort
}

func (m Model) headerLine() string {
	sandbox := m.cfg.Filter
	if sandbox == "" {
		sandbox = "all"
	}
	if m.turnModel != "" {
		return fmt.Sprintf("pi-stack monitor  sandbox=%s  model=%s  events=%d", sandbox, m.turnModel, len(m.rows))
	}
	return fmt.Sprintf("pi-stack monitor  sandbox=%s  events=%d", sandbox, len(m.rows))
}

func (m Model) footerLine() string {
	return fmt.Sprintf(
		"f:full=%s m:model=%s t:tools=%s p:mcp=%s x:think=%s c:ctx=%s  /:filter  space:expand  q:quit",
		onoff(m.showFull), onoff(m.showModel), onoff(m.showTools), onoff(m.showMCP),
		onoff(m.showThinking), onoff(m.showContext))
}

func onoff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// renderRow renders one row's summary line (delta view — architecture.md
// 3.B / docs/design/monitor.md's `turn 12 opus-4-8 ▲ req sys=41KB(unchanged)
// msgs=+1 tools=14 ~38k tok` example).
func (m Model) renderRow(r tuiRow) string {
	switch r.kind {
	case rowKindRequest:
		return renderRequestRow(r)
	case rowKindResponse:
		return renderResponseRow(r)
	case rowKindTool:
		return renderToolRow(r)
	case rowKindContext:
		return renderContextRow(r)
	default:
		return ""
	}
}

func renderRequestRow(r tuiRow) string {
	label := "new"
	if r.sysUnchanged {
		label = "unchanged"
	}
	msgs := "0"
	if r.msgDelta > 0 {
		msgs = fmt.Sprintf("+%d", r.msgDelta)
	}
	return fmt.Sprintf("turn %s  %s  \u25b2 req  sys=%s(%s) msgs=%s tools=%d ~%s",
		r.turnID, r.model, humanBytes(int64(r.sysBytes)), label, msgs, r.toolCount, humanTok(r.estTokens))
}

func renderResponseRow(r tuiRow) string {
	in, out := "-", "-"
	if r.usage != nil {
		in = humanCount(r.usage.InputTokens)
		out = humanCount(r.usage.OutputTokens)
	}
	stop := r.stopReason
	if stop == "" {
		stop = "-"
	}
	return fmt.Sprintf("        \u25bc resp %d  stop=%s  in %s out %s", r.status, stop, in, out)
}

func renderToolRow(r tuiRow) string {
	base := fmt.Sprintf("   tool  %-10s source=%-12s %s", r.name, r.source, r.argsSummary)
	if r.toolDone {
		okLabel := "ok"
		if !r.ok {
			okLabel = "FAIL"
		}
		base += fmt.Sprintf("  \u2192 %s %s %.1fs", okLabel, humanBytes(int64(r.resultBytes)), float64(r.durationMs)/1000)
	} else {
		base += "  \u2026" // pending: tool_start seen, tool_end not yet
	}
	return base
}

func renderContextRow(r tuiRow) string {
	return fmt.Sprintf("   ctx   %-14s %s", r.ctxKind, r.detail)
}

// detailLines renders the expanded (space-toggled) detail for a row: the
// new-message list for a request row, and — only when showFull is also on —
// the full blob body, pre-resolved and sanitized by resolveRowBlobs (into
// r.sysPromptText / r.argsText / r.resultText) back when the row was
// expanded or the blob arrived. detailLines reads ONLY that stored state —
// never cfg.Blob — because View must be a pure function of Model (review
// finding R1-12); blob lookup happens in Update, not here.
func (m Model) detailLines(r tuiRow) []string {
	var lines []string
	switch r.kind {
	case rowKindRequest:
		for i, nm := range r.newMessages {
			lines = append(lines, fmt.Sprintf("      msg %-9s %-6s %s", nm.Role, humanBytes(int64(nm.Bytes)), nm.Preview))
			if m.showFull && i < len(r.newMessageTexts) {
				lines = append(lines, "        "+r.newMessageTexts[i])
			}
		}
		if m.showFull {
			lines = append(lines, "      system prompt:")
			lines = append(lines, "        "+r.sysPromptText)
			if r.toolCount > 0 {
				lines = append(lines, "      tool schema:")
				lines = append(lines, "        "+r.toolSchemaText)
			}
		}
	case rowKindTool:
		if m.showFull {
			lines = append(lines, "      args:   "+r.argsText)
			if r.toolDone {
				lines = append(lines, "      result: "+r.resultText)
			}
		}
	}
	return lines
}

// resolveRowBlobs looks up the row named by id and, for the fields its kind
// cares about (sysPromptText, each newMessage body, and toolSchemaText for a
// request row — review finding R2-6; argsText and — once toolDone —
// resultText for a tool row), fetches the body via cfg.Blob and
// stores the SANITIZED result (keepNewlines=true, since this is a full
// multi-line body, unlike the single-line summary fields) back onto the
// row. This is the only place cfg.Blob is called (review finding R1-12: it
// runs from Update — applyEvent, handleKey, toggleExpandLast — never from
// View). Every call site gates this on the row being both expanded AND
// showFull being on (review finding R3-2b — see clearRowBlobs/
// clearAllResolvedBlobs for the inverse: dropping this text again the
// moment either condition stops holding), so resolveRowBlobs itself does
// not re-check showFull. Called whenever a row becomes expanded while
// showFull is on, and again whenever a blob this row cares about might have
// just arrived (tool_end landing on an already-expanded pending tool row, a
// provider_request replacing an already-expanded row, or `f` toggling
// showFull on via refreshExpandedBlobs). A no-op if id isn't a known row.
func (m *Model) resolveRowBlobs(id string) {
	idx, ok := m.rowIndex[id]
	if !ok {
		return
	}
	r := m.rows[idx]
	switch r.kind {
	case rowKindRequest:
		r.sysPromptText = m.fetchBlobText(r.sysHash)
		r.toolSchemaText = m.fetchBlobText(r.toolSchemaHash)
		texts := make([]string, len(r.newMessages))
		for i, nm := range r.newMessages {
			texts[i] = m.fetchBlobText(nm.Hash)
		}
		r.newMessageTexts = texts
	case rowKindTool:
		r.argsText = m.fetchBlobText(r.argsHash)
		if r.toolDone {
			r.resultText = m.fetchBlobText(r.resultHash)
		}
	}
	m.rows[idx] = r
}

// refreshExpandedBlobs re-resolves every currently-expanded row's blob text.
// Called only when `f` toggles showFull ON: any row expanded while showFull
// was off was never resolved in the first place (applyEvent/
// toggleExpandLast gate on showFull too, review finding R3-2b), so turning
// showFull on is the one moment a batch of already-expanded rows needs
// their bodies fetched all at once. It's cheap (map lookups + string
// builds, no I/O beyond the in-process cache) and keeps the "expanded" set
// as the single source of truth for what to keep resolved. The showFull-OFF
// direction is clearAllResolvedBlobs, its inverse.
func (m *Model) refreshExpandedBlobs() {
	for id, exp := range m.expanded {
		if exp {
			m.resolveRowBlobs(id)
		}
	}
}

// clearRowBlobs drops the resolved full-body text (sysPromptText, argsText,
// resultText, newMessageTexts, toolSchemaText) stored on the row named by
// id, leaving its small summary fields (hashes, previews, byte counts)
// untouched. Called when a row is collapsed, so the multi-KB/MB body it may
// have resolved while expanded doesn't outlive the collapse and sit
// retained for the rest of the row's lifetime — up to maxRows rows, each
// potentially duplicating the same large system prompt, is how R3-2b's
// multi-GB retention happened. A no-op if id isn't a known row.
func (m *Model) clearRowBlobs(id string) {
	idx, ok := m.rowIndex[id]
	if !ok {
		return
	}
	m.clearRowBlobsAt(idx)
}

// clearRowBlobsAt is clearRowBlobs by row-slice index rather than id, so
// clearAllResolvedBlobs can sweep every row without a rowIndex lookup per
// row.
func (m *Model) clearRowBlobsAt(idx int) {
	r := m.rows[idx]
	r.sysPromptText = ""
	r.argsText = ""
	r.resultText = ""
	r.newMessageTexts = nil
	r.toolSchemaText = ""
	m.rows[idx] = r
}

// clearAllResolvedBlobs drops resolved full-body text on every row, not
// just the currently-expanded ones — cheap insurance so no row can be left
// holding a stale body no toggle path meant to keep (e.g. one expanded
// while showFull was on, then evicted-and-somehow-still-present, or any
// future path that stores resolved text without going through
// resolveRowBlobs). Called when `f` toggles showFull OFF: with showFull off,
// detailLines never renders a full body for ANY row, so nothing should
// retain one either (review finding R3-2b).
func (m *Model) clearAllResolvedBlobs() {
	for i := range m.rows {
		m.clearRowBlobsAt(i)
	}
}

// fetchBlobText fetches a blob's full text via cfg.Blob and sanitizes it
// (keepNewlines=true — a full body is expected to be multi-line) before it
// is ever stored on a row or rendered. It never touches the network —
// cfg.Blob is the in-process cache lookup injected by Unit C (hub.Blob) —
// but the result still depends on runtime cache state, which is why only
// resolveRowBlobs (called from Update) may call it, never View/detailLines.
func (m Model) fetchBlobText(hash string) string {
	if hash == "" || m.cfg.Blob == nil {
		return "(body not captured)"
	}
	bl, ok := m.cfg.Blob(hash)
	if !ok {
		return "(body not captured)"
	}
	return sanitizeText(bl.Text, true)
}

// humanBytes reuses the shared byte-formatter in task.go (DRY — same
// B/KB/MB/GB shape used elsewhere in the CLI) rather than duplicating it
// here; monitor rows just need an int->int64 widen at the call site.

func humanTok(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk tok", n/1000)
	}
	return fmt.Sprintf("%d tok", n)
}

func humanCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	footerStyle = lipgloss.NewStyle().Faint(true)
	filterStyle = lipgloss.NewStyle().Italic(true)
)

// RunTUI runs the bubbletea program to completion (blocking). Unit C calls
// this after wiring cfg from a live monitor.Hub.
func RunTUI(cfg TUIConfig) error {
	p := tea.NewProgram(NewModel(cfg))
	_, err := p.Run()
	return err
}
