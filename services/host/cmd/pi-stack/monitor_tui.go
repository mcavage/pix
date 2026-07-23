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
	"github.com/mattn/go-runewidth"

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
	session string // sessionKey(env) this row belongs to — see Model.sessionRowCount and the group headers in bodyLayoutLines
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
	// trigger is monitor.ProviderRequest.Trigger ("user"|"tool_result"|
	// "compaction"|"unknown"): what caused this provider request. A
	// tool_result-triggered request is a tool's output being fed back to
	// the model — the SAME content the tool row right above it already
	// shows as `→ ok ...` — so passesToggles hides these request rows
	// entirely (see the rowKindRequest case) rather than rendering a
	// redundant `user "<tool output>"` row. user/compaction/unknown are
	// real conversational turns (an actual prompt, or a first/continuation
	// turn with no clear trigger) and stay visible.
	trigger string

	// response
	status     int
	stopReason string
	usage      *monitor.UsageSummary
	// textPreview/textBytes/textHash/toolCalls carry what the assistant
	// actually SAID this turn (monitor.ProviderResponse R6-1) — the
	// response row LEADS with these, so the reply reads at the response
	// instead of surfacing a turn later as the next request's msg
	// assistant. textHash resolves to the full reply via resolveRowBlobs
	// (into assistantText below), same pattern as sysHash/sysPromptText.
	textPreview string
	textBytes   int
	textHash    string
	toolCalls   []string

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
	detail  string // single-line (flattened) copy for the row summary line
	// detailFull is the sanitized MULTILINE context detail rendered by the
	// expanded view (split into physical lines like every other body).
	// The single-line `detail` above stays the row-summary/filter copy —
	// without this a multiline or long ctx detail was flattened to one
	// line with no way to see the rest (review finding: context detail
	// flattened).
	detailFull string

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
	// assistantText is the resolved full assistant reply (textHash) for an
	// expanded response row — same R1-12/R3-2b lifecycle as sysPromptText.
	assistantText string

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

	// prevSysHash is compared against each provider_request's
	// SystemPromptHash to render "(unchanged)" vs "(new)" — keyed by
	// sessionKey(env) (sandboxId+"/"+sessionId), NOT global, because the
	// default `monitor` watches every sandbox/session at once and a
	// single shared value would let one session's hash mark another
	// session's first-seen prompt as "(unchanged)" (review finding R1-7).
	prevSysHash map[string]string

	// sessionModel/sessionTitle are keyed by sessionKey(env) and feed the
	// per-session group header (groupHeaderText) so a multi-session feed
	// is self-explanatory — which model each session is on, and what its
	// user actually asked for — instead of a bare sandbox/session id. The
	// old global turnModel (last turn_start.Model across EVERY session,
	// shown in the top bar) was actively misleading once sessions with
	// different models interleave, so it's gone; each session now carries
	// its own model.
	//
	// sessionModel is set from turn_start.Model and provider_request.Model
	// (whichever arrives — they're consistent within one session, so last
	// non-empty wins). sessionTitle is set ONCE, from the first
	// user-triggered provider_request's newest NewMessages preview (the
	// message that actually caused the turn) — a later prompt in the same
	// session never overwrites it, and a tool_result-triggered request
	// (the tool's own output being replayed to the model, not a user ask)
	// never sets it at all.
	sessionModel map[string]string
	sessionTitle map[string]string

	// sessionRowCount tracks how many currently-retained rows belong to
	// each session (sessionKey(env)), so evictOldRows can tell when a
	// session's LAST row is gone and it's safe to drop that session's
	// prevSysHash entry too. Without this, prevSysHash accumulates one
	// entry per distinct session forever — even after every row for that
	// session has been evicted from rows/rowIndex/expanded — because
	// nothing ever removes it (review finding R4-2, same class as
	// R1-13). Incremented in upsertRow when a NEW row is inserted (never
	// on an in-place overwrite/mutate, which doesn't change row count),
	// decremented in evictOldRows for each dropped row. sessionModel and
	// sessionTitle are cleaned up the same way, for the same reason.
	sessionRowCount map[string]int

	expanded map[string]bool // row id -> expanded, toggled by `space`/`enter`

	filtering   bool // `/` was pressed; subsequent runes build filterInput
	filterInput string
	filter      string // committed filter (Enter); substring match on the rendered row line

	showFull     bool // f
	showModel    bool // m
	showTools    bool // t
	showMCP      bool // p
	showThinking bool // x
	showContext  bool // c

	showHelp bool // `?` overlay, closed by `?`/esc; replaces the body while open

	// width/height come from tea.WindowSizeMsg. `sized` records whether a
	// WindowSizeMsg has EVER arrived: before it does (every existing
	// headless unit test, which drives Update directly), View stays fully
	// unbounded — the pre-rework behavior the substring-based tests depend
	// on. Once sized, the numeric height is authoritative even at 0 (a
	// minimize/zero-height resize renders NOTHING rather than dumping the
	// entire retained feed — the height==0-as-sentinel overload this
	// replaces), and View clamps its total output to at most `height`
	// lines, shedding lower-priority chrome (filter line first, then
	// footer, then header) when the terminal is too short for all of it.
	width, height int
	sized         bool

	// cursor is a LINE cursor: an index into the flattened body-line list
	// (the same order bodyLayoutLines produces — one physical line per row
	// header, plus one per expanded-detail line). Navigation is
	// line-granular so a 60-line expanded payload can actually be read by
	// stepping/paging through it, not just glimpsed at its first lines
	// (the headline defect the live-terminal audit exposed in the old
	// per-ROW cursor). It is ignored while follow is true — the cursor is
	// then always the LAST body line, recomputed fresh every call
	// (clampedCursor), so a live stream auto-tracks new events with zero
	// bookkeeping in applyEvent. While detached, the stored index is
	// stable across appends (new lines only ever land BELOW it) and is
	// clamped defensively whenever the layout shrinks (toggle/filter/
	// eviction). follow starts true (NewModel) so a freshly attached
	// monitor opens already tracking the live feed.
	cursor int
	follow bool

	// scrollTop is the index (into the body's flattened line list, the
	// same order bodyLayoutLines produces) of the first line
	// shown, i.e. bubbles/viewport's YOffset by another name (see
	// reconcileScroll, which owns every write to this field). It is
	// PERSISTED rather than recomputed from scratch on every render
	// deliberately: recomputing "top" purely from the current total line
	// count would let a bottom-of-window clamp creep the window back down
	// every time a new row is appended below a detached, already-scrolled
	// selection — the exact bug this rework is fixing (a live sandbox
	// report confirmed the view jumping around while the user tried to
	// read something older). Reconciled after every Update path that can
	// change the row/line layout (a new event, a toggle/filter/expand/nav
	// key, or a resize) rather than inside View, so View stays a pure
	// read of already-settled state.
	scrollTop int
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
		sessionModel:    make(map[string]string),
		sessionTitle:    make(map[string]string),
		sessionRowCount: make(map[string]int),
		expanded:        make(map[string]bool),
		showModel:       true,
		showTools:       true,
		showMCP:         true,
		showContext:     true,
		follow:          true,
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
		// While the view is detached (paused), cursor and scrollTop are raw
		// LINE indices — but an event can shift every line index out from
		// under them: maxRows eviction drops lines off the TOP, and a
		// tool_end landing on an expanded pending tool row INSERTS result
		// lines above anything below it. Without remapping, the same index
		// silently points at a different row and a paused view walks
		// through the feed. So: capture semantic anchors (rowID + line
		// offset within that row) before the mutation, remap after.
		// follow==true needs none of this — clampedCursor/reconcileScroll
		// re-derive the bottom anchor fresh every call.
		var curAnchor, topAnchor lineAnchor
		anchored := !m.follow && !m.showHelp
		if anchored {
			lines := m.bodyLayoutLines()
			curAnchor = captureLineAnchor(m.clampedCursor(len(lines)), lines)
			top := m.scrollTop
			if top > len(lines)-1 {
				top = len(lines) - 1
			}
			if top < 0 {
				top = 0
			}
			topAnchor = captureLineAnchor(top, lines)
		}
		m.applyEvent(msg.event)
		if anchored {
			lines := m.bodyLayoutLines()
			if idx, ok := resolveLineAnchor(curAnchor, lines); ok {
				m.cursor = idx
			} else if curAnchor.valid {
				// Anchored row evicted: clamp to the nearest surviving
				// row's header (the oldest retained one — eviction only
				// ever drops from the top).
				m.cursor = firstHeaderLine(lines)
			}
			if idx, ok := resolveLineAnchor(topAnchor, lines); ok {
				m.scrollTop = idx
			} else if topAnchor.valid {
				m.scrollTop = firstHeaderLine(lines)
			}
		}
		m.reconcileScroll()
		return m, waitForEvent(m.cfg.Events)
	case eventsClosedMsg:
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		// Requirement 1: the alt-screen frame is sized off the real
		// terminal. Before this arrives (sized=false, e.g. every existing
		// headless unit test) View stays unbounded — see the width/height
		// doc comment on Model. sized (not height==0) is what flips the
		// clamp on, so a real zero-height resize clamps to nothing instead
		// of being mistaken for "no size yet" and dumping the whole feed.
		m.width = msg.Width
		m.height = msg.Height
		m.sized = true
		m.reconcileScroll()
		return m, nil
	default:
		// No tea.MouseMsg case: mouse capture is intentionally OFF (see
		// RunTUI), so no mouse events ever arrive.
		return m, nil
	}
}

// handleKey wraps handleKeyInner with a single reconcileScroll pass so
// every key path (toggle, filter, nav, expand, help) leaves scrollTop
// consistent with whatever it just changed, without threading a
// reconcileScroll call through each of handleKeyInner's many return
// points individually.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	nm, cmd := m.handleKeyInner(msg)
	if mm, ok := nm.(Model); ok {
		mm.reconcileScroll()
		return mm, cmd
	}
	return nm, cmd
}

// handleKeyInner implements the toggle/filter/expand/quit keymap from
// architecture.md 3.B. `ctrl+c` always quits, even while typing a filter (it
// is a control signal, never filter text). `q` only quits outside filter
// input mode, so a filter string containing "q" can actually be typed.
func (m Model) handleKeyInner(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
	// The help overlay swallows every key except quit, the two that close
	// it, and the line-navigation keys (so help taller than the viewport
	// can be scrolled with the exact same nav the row view uses) —
	// anything else (a toggle, expand, filter) is swallowed so a stray
	// keypress while reading the overlay can't silently flip state
	// underneath it.
	if m.showHelp {
		switch key {
		case "?", "esc":
			// Close and restore the row view. Simplest correct restore:
			// re-attach follow (cursor back to the newest line) rather
			// than trying to remember the pre-help cursor.
			m.showHelp = false
			m.follow = true
		case "q":
			return m, tea.Quit
		case "up", "ctrl+p":
			m.moveCursor(-1)
		case "down", "ctrl+n":
			m.moveCursor(1)
		case "g", "home", "alt+<":
			m.cursorToTop()
		case "G", "end", "alt+>":
			m.reattachFollow()
		case "pgup", "ctrl+u", "alt+v":
			m.moveCursor(-m.pageSize())
		case "pgdown", "ctrl+d", "ctrl+v":
			m.moveCursor(m.pageSize())
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
	case "?":
		// Help renders from its own top (the audit's frame F showed it
		// inheriting the row view's scrollTop and starting mid-list):
		// reset the cursor and scroll and detach follow so the first
		// help line is what the user sees.
		m.showHelp = true
		m.cursor = 0
		m.scrollTop = 0
		m.follow = false
	case " ", "enter":
		m.toggleExpandAtCursor()
	case "up", "ctrl+p":
		m.moveCursor(-1)
	case "down", "ctrl+n":
		m.moveCursor(1)
	case "g", "home", "alt+<":
		m.cursorToTop()
	case "G", "end", "alt+>":
		m.reattachFollow()
	case "pgup", "ctrl+u", "alt+v":
		m.moveCursor(-m.pageSize())
	case "pgdown", "ctrl+d", "ctrl+v":
		m.moveCursor(m.pageSize())
	case "left", "right":
		// Deliberate no-ops: the feed has no horizontal axis, but arrow
		// keys must always be safe to press (never mis-typed into a
		// toggle, never a crash).
	}
	return m, nil
}

// lineAnchor pins one body line SEMANTICALLY — by the row that owns it
// plus the line's offset within that row's contiguous block of lines —
// rather than by raw index, so eviction/insertion above it can be remapped
// (see the eventMsg case in Update). valid=false means the line was untied
// (empty-state hint, help) or out of range; nothing to remap then.
type lineAnchor struct {
	rowID  string
	offset int
	valid  bool
}

// captureLineAnchor anchors the line at idx in the given layout.
func captureLineAnchor(idx int, lines []bodyLine) lineAnchor {
	if idx < 0 || idx >= len(lines) || lines[idx].rowID == "" {
		return lineAnchor{}
	}
	first := idx
	for first > 0 && lines[first-1].rowID == lines[idx].rowID {
		first--
	}
	return lineAnchor{rowID: lines[idx].rowID, offset: idx - first, valid: true}
}

// resolveLineAnchor maps an anchor back to a line index in the NEW layout:
// the anchored row's first line plus the saved offset, clamped to the
// row's (possibly changed) line count. ok=false when the anchor is invalid
// or its row no longer exists in the layout (evicted / filtered out).
func resolveLineAnchor(a lineAnchor, lines []bodyLine) (int, bool) {
	if !a.valid {
		return 0, false
	}
	first, count := -1, 0
	for i, l := range lines {
		if l.rowID == a.rowID {
			if first < 0 {
				first = i
			}
			count++
		} else if first >= 0 {
			break // a row's lines are contiguous
		}
	}
	if first < 0 {
		return 0, false
	}
	off := a.offset
	if off > count-1 {
		off = count - 1
	}
	return first + off, true
}

// firstHeaderLine is the index of the first row-backed header line in the
// layout (0 when there is none) — the clamp target when an anchored row
// was evicted out from under a detached cursor.
func firstHeaderLine(lines []bodyLine) int {
	for i, l := range lines {
		if l.isHeader && l.rowID != "" {
			return i
		}
	}
	return 0
}

// clampedCursor resolves the effective cursor line against the CURRENT
// body-line total: the last line while follow is on (this is what makes
// follow-mode auto-track new events with zero extra bookkeeping in
// applyEvent), otherwise the stored m.cursor clamped defensively into
// [0, total) — the layout can shrink out from under it (toggle/filter
// change, eviction, collapse).
func (m Model) clampedCursor(total int) int {
	if total <= 0 {
		return 0
	}
	if m.follow {
		return total - 1
	}
	c := m.cursor
	if c < 0 {
		c = 0
	}
	if c > total-1 {
		c = total - 1
	}
	return c
}

// selectedRowID is the id of the row that OWNS the cursor's current body
// line (its header line or one of its expanded detail lines) — the row
// View highlights. "" when the cursor sits on an untied line (empty-state
// hint, help overlay).
func (m Model) selectedRowID() string {
	lines := m.bodyLayoutLines()
	if len(lines) == 0 {
		return ""
	}
	return lines[m.clampedCursor(len(lines))].rowID
}

// reconcileScroll updates m.scrollTop ("the body scrolls to keep the
// cursor LINE visible") after every Update path that can change the
// row/line layout — a new event, a toggle/filter/expand/nav key, or a
// resize. See the scrollTop field doc comment for why this is persisted
// state rather than something View recomputes from scratch every render.
//
// While follow is on, scrollTop always bottom-anchors (newest line
// visible). While detached, scrollTop only moves the minimum amount needed
// to bring the cursor line back inside [scrollTop, scrollTop+bodyHeight)
// — exactly bubbles/viewport's "ensure visible" behavior — so a detached
// view that's already showing the cursor line stays exactly where it is
// even as new rows keep appending below it.
func (m *Model) reconcileScroll() {
	bh := m.bodyHeight()
	if bh <= 0 {
		m.scrollTop = 0
		return
	}
	total := len(m.bodyLayoutLines())
	if total <= bh {
		m.scrollTop = 0
		return
	}
	if m.follow {
		m.scrollTop = total - bh
		return
	}
	cur := m.clampedCursor(total)
	if cur < m.scrollTop {
		m.scrollTop = cur
	} else if cur >= m.scrollTop+bh {
		m.scrollTop = cur - bh + 1
	}
	if m.scrollTop < 0 {
		m.scrollTop = 0
	}
	if m.scrollTop > total-bh {
		m.scrollTop = total - bh
	}
}

// moveCursor shifts the LINE cursor by delta physical lines (j/k/arrows:
// ±1 — so an expanded 60-line payload is read by stepping THROUGH it, the
// audit's headline defect; PgUp/PgDn/ctrl+u/ctrl+d: ±pageSize) and updates
// follow: landing anywhere but the last body line detaches it (so a live
// stream stops yanking the view while the user is reading); landing back
// on the last line (stepping/paging down to the bottom) re-attaches it,
// same as pressing G. Group-header lines (bodyLine.group) are NOT
// selectable: after the raw move the cursor skips over them in the
// direction of travel (falling back the other way at a boundary), so
// navigation only ever lands on real row/detail lines.
func (m *Model) moveCursor(delta int) {
	if !m.navigableBody() {
		return
	}
	lines := m.bodyLayoutLines()
	total := len(lines)
	if total == 0 {
		return
	}
	cur := m.clampedCursor(total) + delta
	if cur < 0 {
		cur = 0
	}
	if cur > total-1 {
		cur = total - 1
	}
	dir := 1
	if delta < 0 {
		dir = -1
	}
	cur = skipGroupLines(lines, cur, dir)
	m.cursor = cur
	m.follow = cur == total-1
}

// skipGroupLines returns the nearest selectable (non-group-header) line
// index to idx, searching first in direction dir (+1 down / -1 up), then
// the opposite way when the boundary is hit — e.g. pressing `up` from the
// first row of the first group lands back on that row, not on its header.
// A layout can never be ALL group headers (a header is only ever emitted
// before a row), so this always terminates on a selectable line.
func skipGroupLines(lines []bodyLine, idx, dir int) int {
	for i := idx; i >= 0 && i < len(lines); i += dir {
		if !lines[i].group {
			return i
		}
	}
	for i := idx; i >= 0 && i < len(lines); i -= dir {
		if !lines[i].group {
			return i
		}
	}
	return idx
}

// cursorToTop implements `g`/Home: jump to the first body line and detach
// follow (unless that line is also the only/last line). When grouping is
// active, line 0 is a group header — the cursor lands on the first
// SELECTABLE line below it instead (headers are non-selectable).
func (m *Model) cursorToTop() {
	if !m.navigableBody() {
		return
	}
	lines := m.bodyLayoutLines()
	total := len(lines)
	if total == 0 {
		return
	}
	m.cursor = skipGroupLines(lines, 0, 1)
	m.follow = m.cursor == total-1
}

// navigableBody reports whether navigation keys have anything to act on:
// the help overlay always does (its lines are the content), the row feed
// only when at least one visible row exists. On the EMPTY state (the two
// untied hint lines) nav must be a no-op that never detaches follow —
// otherwise an Up/wheel/Home pressed while waiting for the first event
// left the monitor opening PAUSED on the first events, looking stuck.
func (m Model) navigableBody() bool {
	return m.showHelp || len(m.visibleRows()) > 0
}

// reattachFollow implements `G`/End: jump to (and keep tracking) the last
// body line — clampedCursor derives the cursor from follow from here on,
// so no stored index needs updating per event.
func (m *Model) reattachFollow() {
	m.follow = true
	if total := len(m.bodyLayoutLines()); total > 0 {
		m.cursor = total - 1
	}
}

// pageSize is the line-count step for PgUp/PgDn/ctrl+u/ctrl+d: the current
// clamped body height when one is known (height>0), else a fixed fallback
// (no real terminal size yet — e.g. a test driving Update directly without
// a WindowSizeMsg).
func (m Model) pageSize() int {
	if bh := m.bodyHeight(); bh > 0 {
		return bh
	}
	return 10
}

// toggleExpandAtCursor expands/collapses the row that OWNS the cursor's
// current body line (its header line, or — for a collapse — any of its
// expanded detail lines). Bound to both `space` and `enter`. Keeps the
// R3-2b memory discipline exactly as before: resolve blobs on expand (only
// while showFull is also on), clear them on collapse. On collapse the
// cursor SNAPS to the row's header line, so it never lands on a
// now-removed detail line (which would silently jump it to whatever line
// slid up into that index).
func (m *Model) toggleExpandAtCursor() {
	lines := m.bodyLayoutLines()
	if len(lines) == 0 {
		return
	}
	id := lines[m.clampedCursor(len(lines))].rowID
	if id == "" {
		return // untied line (empty-state hint): nothing to expand
	}
	m.expanded[id] = !m.expanded[id]
	if m.expanded[id] {
		if m.showFull {
			m.resolveRowBlobs(id)
		}
		return
	}
	// Collapsing: drop this row's resolved full-body text so it can be
	// GC'd rather than retained for the rest of the row's lifetime
	// (review finding R3-2b). The small summary fields (hashes, previews,
	// byte counts) are untouched.
	m.clearRowBlobs(id)
	// Snap the cursor to the collapsed row's header line in the NEW
	// (post-collapse) layout.
	after := m.bodyLayoutLines()
	for i, l := range after {
		if l.rowID == id && l.isHeader {
			m.cursor = i
			m.follow = i == len(after)-1
			return
		}
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
		if model := sanitizeText(ev.Model, false); model != "" {
			m.sessionModel[sess] = model
		}

	case monitor.ProviderRequest:
		if model := sanitizeText(ev.Model, false); model != "" {
			m.sessionModel[sess] = model
		}
		// The group header's title is the FIRST real user ask for this
		// session, never a later one and never a tool_result request (that's
		// the tool's own output being replayed to the model, not something
		// the user typed) — so this only fires once, guarded by the
		// "not already set" check, on a user (or unknown/empty-trigger, e.g.
		// an initial turn with no clear cause) request.
		if _, captured := m.sessionTitle[sess]; !captured && (ev.Trigger == "user" || ev.Trigger == "") && len(ev.Summary.NewMessages) > 0 {
			// Newest entry last, same convention as renderRequestRow — that's
			// the message that actually triggered this turn.
			preview := ev.Summary.NewMessages[len(ev.Summary.NewMessages)-1].Preview
			m.sessionTitle[sess] = truncateLine(sanitizeText(preview, false), 40)
		}
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
			trigger:        sanitizeText(ev.Trigger, false),
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
			id:          id,
			session:     sess,
			turnID:      sanitizeText(env.TurnID, false),
			kind:        rowKindResponse,
			status:      ev.Status,
			stopReason:  sanitizeText(ev.StopReason, false),
			usage:       ev.Usage,
			textPreview: sanitizeText(ev.TextPreview, false),
			textBytes:   ev.TextBytes,
			textHash:    ev.TextHash,
			toolCalls:   sanitizeStrings(ev.ToolCalls),
		})
		// Same R3-2b gating as the request path: a response row the user
		// already expanded (e.g. re-delivered/overwritten) re-resolves its
		// assistant body only while showFull is on.
		if m.expanded[id] && m.showFull {
			m.resolveRowBlobs(id)
		}

	case monitor.ToolStart:
		// Keyed by turnId+toolId (review finding R2-2), not toolId alone: a
		// provider can reuse a toolId (e.g. "call_1") across two turns in
		// the SAME session, and without turnId in the key the second
		// turn's tool_start would overwrite the first turn's row, and a
		// late tool_end would mutate the wrong turn's row.
		id := sess + "/" + env.TurnID + "/tool:" + ev.ToolID
		m.upsertRow(tuiRow{
			id:      id,
			session: sess,
			turnID:  sanitizeText(env.TurnID, false),
			kind:    rowKindTool,
			// The row KEY above keeps the raw ToolID (correlation with the
			// later tool_end must be byte-exact and the key is never
			// rendered); the stored/displayed copy is sanitized like every
			// other event-derived string (R1-8 — the expanded detail line
			// renders it, so a raw OSC/CSI ToolID would drive the terminal).
			toolID:      sanitizeText(ev.ToolID, false),
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
				id:      id,
				session: sess,
				turnID:  sanitizeText(env.TurnID, false),
				kind:    rowKindTool,
				// Sanitized for display, same as the ToolStart path (R1-8);
				// the raw ToolID lives only in the row key.
				toolID:        sanitizeText(ev.ToolID, false),
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
			// keepNewlines=true: the expanded view renders this across
			// multiple physical lines (split by detailLines' block helper),
			// so a multiline ctx detail is actually readable instead of
			// being flattened+truncated into the single summary line.
			detailFull: sanitizeText(ev.Detail, true),
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
			delete(m.sessionModel, r.session)
			delete(m.sessionTitle, r.session)
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
	case rowKindRequest:
		// A tool_result-triggered request is the tool's own output being fed
		// back to the model as the next turn's "new message" — the exact
		// content the tool row right above it already rendered as
		// `→ ok ...`. Hiding it here (rather than at applyEvent/upsertRow)
		// keeps the row itself intact so the response row for this same
		// turn still renders, group headers still see a real row to key
		// off of, and toggling something else off/on never resurrects it.
		if r.trigger == "tool_result" {
			return false
		}
		return m.showModel
	case rowKindResponse:
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

// bodyLine is one PHYSICAL line of the scrollable body region — text never
// contains '\n' (multi-line blob bodies are split into one bodyLine per
// physical line by detailLines, so the height clamp and per-line width
// truncation both count what actually renders). Either a row's own summary
// line (isHeader=true, carries the caret/cursor gutter) or one of its
// expanded detail lines, or an untied line (help overlay / empty-state
// hint, rowID="").
type bodyLine struct {
	text     string
	rowID    string
	isHeader bool
	// group marks a session GROUP HEADER line (the `── sandbox … ──` rule
	// emitted when 2+ distinct sessions are in view). Group lines are
	// NON-selectable: they own no row (rowID==""), the line cursor skips
	// over them (skipGroupLines), and expand on one is a no-op.
	group bool
}

// View renders the current state. PURE: no channel reads, no goroutines, no
// tea.Program access — see the Model doc comment and architecture.md 3.B
// ("View() must be a PURE function of Model state"). Requirement 1's
// height==0 sentinel (no WindowSizeMsg yet) keeps this fully unbounded —
// every existing substring-based unit test still passes unmodified; only
// height>0 clamps the body to a fixed number of lines and scrolls it
// (requirements 1/3/4).
func (m Model) View() string {
	header, filter, footer := m.chrome()
	var parts []string
	if header {
		parts = append(parts, headerStyle.Render(m.truncate(m.headerLine())))
	}
	if filter {
		if m.filtering {
			parts = append(parts, filterStyle.Render(m.truncate("filter> "+m.filterInput)))
		} else {
			parts = append(parts, filterStyle.Render(m.truncate("filter: "+m.filter)))
		}
	}
	for _, l := range m.windowedBodyLines(m.renderBodyLines()) {
		parts = append(parts, l.text)
	}
	if footer {
		parts = append(parts, footerStyle.Render(m.truncate(m.footerLine())))
	}
	return strings.Join(parts, "\n")
}

// chrome decides which frame-chrome lines View renders. Pre-size
// (sized=false, every headless test) everything relevant renders,
// unbounded. Once sized, chrome is shed lowest-priority-first when the
// terminal is too short for all of it — the filter line drops first (it
// needs height>=3), then the footer (height>=2), then the header
// (height>=1) — so the TOTAL rendered line count (chrome + body, see
// bodyHeight) never exceeds the terminal height, even at height 0-3.
func (m Model) chrome() (header, filter, footer bool) {
	filterActive := m.filtering || m.filter != ""
	if !m.sized {
		return true, filterActive, true
	}
	return m.height >= 1, filterActive && m.height >= 3, m.height >= 2
}

// bodyHeight is the number of body lines that fit around the chrome lines
// View actually renders. 0 pre-size is the "unbounded" sentinel (sized
// false — callers treat it as no clamp); once sized it is an exact budget
// and MAY be 0 (a terminal of height 0-2 has no room for body lines —
// rendering "at least one" anyway would overflow the frame).
func (m Model) bodyHeight() int {
	if !m.sized {
		return 0
	}
	header, filter, footer := m.chrome()
	h := m.height
	if header {
		h--
	}
	if filter {
		h--
	}
	if footer {
		h--
	}
	if h < 0 {
		h = 0
	}
	return h
}

// bodyLayoutLines builds the flat physical-line layout of the scrollable
// body: one line per visible row header (with its ▸/▾ expand caret), plus
// one line per expanded-detail physical line (detailLines already split
// every multi-line body on '\n'). This is the single source of truth for
// line positions — the cursor, reconcileScroll, expand-at-cursor, and
// renderBodyLines all index into the SAME layout — so no line here ever
// contains a '\n'. Text is raw (no gutter, no truncation, no styling);
// renderBodyLines decorates it. The help overlay REPLACES this entirely
// rather than being one more toggle over the row feed.
func (m Model) bodyLayoutLines() []bodyLine {
	if m.showHelp {
		return m.helpBodyLines()
	}
	rows := m.visibleRows()
	if len(rows) == 0 {
		return []bodyLine{
			{text: "(no events yet)"},
			{text: fmt.Sprintf("waiting for a monitor-enabled sandbox on :%d — if nothing appears, the sandbox may predate the monitor extension (rebuild image / make load)", m.hubPort())},
		}
	}
	// Session grouping (live feedback: with 2 sandboxes running the feed
	// is an undifferentiated mess; and separately, "sandboxes aren't
	// labeled" — a single-sandbox feed showed no label at all). The
	// group-header line now ALWAYS renders, one per contiguous RUN of
	// same-session rows (chronological order is preserved — rows are
	// never reordered, so follow keeps tracking the true newest event and
	// a bursty feed reads as threads), so the user always sees which
	// sandbox/session they're watching, even with only one. Extra indent
	// under the header is reserved for when it's actually needed to tell
	// threads apart: 2+ distinct sessionKeys (multiSession) indents rows
	// 4 spaces under their header; a single session keeps the plain
	// 2-col gutter (no indent) with the one header line at top — it reads
	// cleaner than indenting an entire feed under a header it can never
	// need to distinguish from another.
	grouped := multiSession(rows)
	indent := ""
	if grouped {
		indent = "    "
	}
	var lines []bodyLine
	prevSession := ""
	for i, r := range rows {
		if i == 0 || r.session != prevSession {
			lines = append(lines, bodyLine{text: m.groupHeaderText(r.session), group: true})
		}
		prevSession = r.session
		// ▸/▾ is the expand affordance: every row kind has detail after
		// this rework, so every header carries a caret.
		caret := "\u25b8 "
		if m.expanded[r.id] {
			caret = "\u25be "
		}
		lines = append(lines, bodyLine{text: indent + caret + m.renderRow(r), rowID: r.id, isHeader: true})
		if m.expanded[r.id] {
			for _, dl := range m.detailLines(r) {
				lines = append(lines, bodyLine{text: indent + dl, rowID: r.id})
			}
		}
	}
	return lines
}

// multiSession reports whether the visible rows span more than one
// distinct sessionKey — the switch that turns session grouping on.
func multiSession(rows []tuiRow) bool {
	for _, r := range rows[1:] {
		if r.session != rows[0].session {
			return true
		}
	}
	return false
}

// groupHeaderText renders a session group's header rule at column 0 (no
// indent — the rows under it are indented instead, so the feed reads as
// threads): `── <sandbox> · <session> · <model> · "<title>" ──`, e.g.
// `── pi-stack-dev · 10f905c3 · claude-opus-4-8 · "can you send a test
// ping to gemini…" ──`. With several concurrent sessions streaming (a main
// conversation, child `pi -p` provider calls, other interactive sessions)
// the bare sandbox/session id told you nothing about what a session
// actually IS — the model and title segments make the header
// self-explanatory: which model this session is on, and what its user
// actually asked for. See sessionModel/sessionTitle. Either segment is
// omitted when not yet captured: a session with no turn_start/
// provider_request yet has no known model, and a session whose first
// user prompt hasn't landed (or was tool_result-only, e.g. a `pi -p`
// one-shot with no clear "user" trigger — empty trigger still counts)
// has no title.
//
// The sessionKey is split back into its sandbox/session halves
// (sessionKey joins them with "/"); sandbox/model/title are all
// event-derived attacker text, so they're sanitized here too (the model
// and title are already sanitized when stored, but this stays
// defense-in-depth rather than trusting the map) — the raw key is only
// ever a map/identity key, never rendered. An empty sandbox id reads
// "(local)"; the session id is shortened to its last 8 runes (the tail is
// the distinctive part of a generated id). The whole line is composed
// unbounded here; renderBodyLines' shared m.truncate pass clamps it (and
// every other body line) to the terminal width, so a long title
// ellipsizes instead of wrapping — it never gets its own truncation here.
func (m Model) groupHeaderText(session string) string {
	sandbox, sess, _ := strings.Cut(session, "/")
	sandbox = sanitizeText(sandbox, false)
	sess = sanitizeText(sess, false)
	if sandbox == "" {
		sandbox = "(local)"
	}
	if r := []rune(sess); len(r) > 8 {
		sess = string(r[len(r)-8:])
	}
	if sess == "" {
		sess = "-"
	}
	head := fmt.Sprintf("\u2500\u2500 %s \u00b7 %s", sandbox, sess)
	if model := sanitizeText(m.sessionModel[session], false); model != "" {
		head += " \u00b7 " + model
	}
	if title := sanitizeText(m.sessionTitle[session], false); title != "" {
		head += fmt.Sprintf(" \u00b7 \u201c%s\u201d", title)
	}
	return head + " \u2500\u2500"
}

// renderBodyLines decorates bodyLayoutLines for display: a "> "/"  " gutter
// on row lines (the marker lands on the header of the row that OWNS the
// cursor line — visible/assertable independent of terminal color support,
// since lipgloss renders plain whenever stdout isn't a TTY, e.g. every
// unit test), per-line truncation of the FULL composed line to the
// terminal width (so nothing — gutter included — escapes the frame), and
// reverse styling on the exact cursor line so line-granular position is
// legible on a real color terminal.
func (m Model) renderBodyLines() []bodyLine {
	lines := m.bodyLayoutLines()
	if len(lines) == 0 {
		return lines
	}
	cur := m.clampedCursor(len(lines))
	selID := lines[cur].rowID
	out := make([]bodyLine, len(lines))
	for i, l := range lines {
		text := l.text
		if l.rowID != "" {
			gutter := "  "
			if l.isHeader && selID != "" && l.rowID == selID {
				gutter = "> "
			}
			text = gutter + text
		}
		text = m.truncate(text)
		switch {
		case l.group:
			// Group headers stay at column 0 (no gutter) and are dimmed —
			// visually a rule between threads, never a selectable line.
			text = groupHeaderStyle.Render(text)
		case i == cur && l.rowID != "":
			text = selectedStyle.Render(text)
		}
		out[i] = bodyLine{text: text, rowID: l.rowID, isHeader: l.isHeader, group: l.group}
	}
	return out
}

// windowedBodyLines clamps+scrolls the body to bodyHeight lines (requirement
// 4: this is the ONE place full payloads/detail get bounded — nothing ever
// escapes the frame by being written outside this window). height==0
// (requirement 1's unbounded sentinel) and "everything already fits" both
// return every line unchanged. The actual scroll POSITION is decided by
// reconcileScroll (persisted in m.scrollTop, not recomputed here) — see its
// doc comment for why: recomputing "top" fresh from the current total line
// count on every render is what let a detached view get pulled back toward
// the bottom as new rows kept appending underneath it. This function just
// clamps that stored offset defensively (e.g. after a toggle/filter change
// shrinks the total out from under it) and slices.
func (m Model) windowedBodyLines(lines []bodyLine) []bodyLine {
	if !m.sized {
		return lines
	}
	bh := m.bodyHeight()
	if bh <= 0 {
		return nil // sized but no room for body lines (height 0-2)
	}
	total := len(lines)
	if bh >= total {
		return lines
	}
	top := m.scrollTop
	if top < 0 {
		top = 0
	}
	if top > total-bh {
		top = total - bh
	}
	return lines[top : top+bh]
}

// truncate clamps a single line to the real terminal width (requirement 4:
// "long lines should be truncated to width... so nothing escapes the
// frame"). width==0 (no WindowSizeMsg yet, or a caller that never sets it —
// every existing unit test) is unbounded, matching the pre-size sentinel.
func (m Model) truncate(s string) string {
	return truncateLine(s, m.width)
}

// truncateLine measures and truncates by terminal DISPLAY cells
// (go-runewidth), not runes: CJK/emoji occupy two cells each, so a
// rune-count clamp let wide text overflow the width, wrap, and corrupt the
// alt-screen frame. runewidth.Truncate never splits a rune (or a wide rune
// across the boundary), and the … ellipsis (1 cell, reserved inside the
// budget) marks a truncated line as distinct from a short one.
func truncateLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	if width == 1 {
		return runewidth.Truncate(s, 1, "")
	}
	return runewidth.Truncate(s, width, "…")
}

// helpBodyLines is the `?` overlay content: every key the keymap
// recognizes, grouped by purpose. Raw lines (rowID="") — renderBodyLines
// truncates them, and the shared line cursor scrolls them when help is
// taller than the viewport.
func (m Model) helpBodyLines() []bodyLine {
	text := []string{
		"pi-stack monitor — keys",
		"",
		"navigation (line-granular: expanded payloads scroll line by line)",
		"  up, down        move one line (emacs: ctrl+p / ctrl+n)",
		"  g/Home, alt+<   jump to the top",
		"  G/End, alt+>    jump to the bottom (re-attach follow)",
		"  PgUp/PgDn       page up/down (ctrl+u/ctrl+d; emacs: alt+v / ctrl+v)",
		"",
		"row detail",
		"  enter, space    expand/collapse the row owning the cursor line (▸/▾)",
		"",
		"toggles",
		"  f               full payloads (system prompt / message / assistant / tool bodies)",
		"  m               model request/response rows",
		"  t               tool rows",
		"  p               mcp tool rows",
		"  x               thinking-level context rows",
		"  c               context rows",
		"",
		"filter",
		"  /               start typing a filter; enter commits, esc cancels",
		"",
		"other",
		"  ?               toggle this help",
		"  q, ctrl+c       quit",
	}
	lines := make([]bodyLine, len(text))
	for i, t := range text {
		lines[i] = bodyLine{text: t}
	}
	return lines
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
	return fmt.Sprintf("pi-stack monitor  sandbox=%s  events=%d", sandbox, len(m.rows))
}

func (m Model) footerLine() string {
	follow := "[following]"
	if !m.follow {
		follow = "[paused]"
	}
	return fmt.Sprintf(
		"%s f:full=%s m:model=%s t:tools=%s p:mcp=%s x:think=%s c:ctx=%s  nav:\u2191\u2193  enter/space:expand  /:filter  ?:help  q:quit",
		follow,
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

// renderRequestRow is CONVERSATION-FIRST (live feedback: "the user message
// is buried"): the headline is the newest new-message content — the actual
// prompt that drove this turn — labeled by role; the model / system-prompt
// bytes / msg delta / tool count / est tokens are demoted to a `·`-separated
// diagnostics suffix. When the turn carried no new messages (pure re-send),
// the row falls back to a bare `req` label with the same suffix.
func renderRequestRow(r tuiRow) string {
	label := "new"
	if r.sysUnchanged {
		label = "unchanged"
	}
	msgs := "0"
	if r.msgDelta > 0 {
		msgs = fmt.Sprintf("+%d", r.msgDelta)
	}
	diag := fmt.Sprintf("turn %s  %s  sys=%s(%s) msgs=%s tools=%d ~%s",
		r.turnID, r.model, humanBytes(int64(r.sysBytes)), label, msgs, r.toolCount, humanTok(r.estTokens))
	if len(r.newMessages) == 0 {
		return "req        \u00b7 " + diag
	}
	// Newest entry last — that's the message that triggered this turn.
	nm := r.newMessages[len(r.newMessages)-1]
	return fmt.Sprintf("%-9s \u201c%s\u201d  \u00b7 %s", requestRoleLabel(nm.Role), nm.Preview, diag)
}

// requestRoleLabel maps a new-message role to the row's left-hand
// conversation label. A user message reads `user`; a tool-result-driven
// turn reads `(tool result)`; anything else is labeled by its (already
// sanitized) role verbatim so the eye can still scan the transcript.
func requestRoleLabel(role string) string {
	switch role {
	case "", "user":
		return "user"
	case "tool", "toolResult", "tool_result", "tool-result":
		return "(tool result)"
	default:
		return role
	}
}

// renderResponseRow LEADS with what the model actually said (textPreview)
// plus the tool calls it made (`→ name, name`), demoting status/stopReason/
// usage to a dim `·`-separated suffix. HTTP headers never appear here —
// they live at the very END of the expanded diagnostics section (live
// feedback: headers showed instead of the high-level LLM data).
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
	head := "assistant"
	if r.textPreview != "" {
		head += fmt.Sprintf("  \u201c%s\u201d", r.textPreview)
	}
	if len(r.toolCalls) > 0 {
		head += "  \u2192 " + strings.Join(r.toolCalls, ", ")
	}
	return fmt.Sprintf("%s  \u00b7 resp %d  stop=%s  in %s out %s", head, r.status, stop, in, out)
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

// diagnosticsMarker visually separates the conversation content (the
// prompt / the assistant's reply) from the plumbing (model, tokens,
// system prompt, status, usage, HTTP headers) in every expanded view —
// the conversation always comes FIRST, the marker second, diagnostics
// after it (live feedback: the row hierarchy led with diagnostics and
// buried the human-readable conversation).
const diagnosticsMarker = "      \u2014 diagnostics \u2014"

// detailLines renders the expanded (space-toggled) detail for a row as
// PHYSICAL lines — every stored body/preview is split on '\n' and each
// fragment becomes its own line, so no returned string ever contains a
// newline (the height clamp and per-line width truncation both depend on
// that). Every row kind returns at least one line even with showFull off
// (expand always has a visible effect — the audit found expanding a
// response/context row was a silent no-op): a request row shows the new
// messages (the prompt) first, then a `— diagnostics —` section with its
// system-prompt/tool/token summary; a response row shows the assistant's
// reply and tool calls first, then diagnostics — status, stop reason,
// usage tokens — with headers dead last; a
// tool row shows name/source/id, args and — once done — result summaries;
// a context row shows ctxKind + its full detail text. showFull adds the
// full blob bodies, pre-resolved and sanitized by resolveRowBlobs (into
// r.sysPromptText / r.argsText / r.resultText / …) back when the row was
// expanded or the blob arrived. detailLines reads ONLY that stored state —
// never cfg.Blob — because View must be a pure function of Model (review
// finding R1-12); blob lookup happens in Update, not here.
func (m Model) detailLines(r tuiRow) []string {
	var lines []string
	block := func(prefix, body string) {
		for _, ln := range strings.Split(body, "\n") {
			lines = append(lines, prefix+ln)
		}
	}
	switch r.kind {
	case rowKindRequest:
		// CONVERSATION FIRST: the actual prompt — every new message this
		// turn (preview line, plus the full resolved body under showFull) —
		// then a clearly separated diagnostics section for the plumbing
		// (model, tokens, system prompt, tool schema, tool name lists).
		for i, nm := range r.newMessages {
			lines = append(lines, fmt.Sprintf("      msg %-9s %-6s %s", nm.Role, humanBytes(int64(nm.Bytes)), nm.Preview))
			if m.showFull && i < len(r.newMessageTexts) {
				block("        ", r.newMessageTexts[i])
			}
		}
		label := "new"
		if r.sysUnchanged {
			label = "unchanged"
		}
		lines = append(lines, diagnosticsMarker)
		lines = append(lines, fmt.Sprintf("      model %s  system prompt %s (%s)  tools=%d  est ~%s",
			r.model, humanBytes(int64(r.sysBytes)), label, r.toolCount, humanTok(r.estTokens)))
		if len(r.toolNames) > 0 {
			lines = append(lines, "      tools: "+strings.Join(r.toolNames, ", "))
		}
		if len(r.mcpToolNames) > 0 {
			lines = append(lines, "      mcp tools: "+strings.Join(r.mcpToolNames, ", "))
		}
		if m.showFull {
			lines = append(lines, "      system prompt:")
			block("        ", r.sysPromptText)
			if r.toolCount > 0 {
				lines = append(lines, "      tool schema:")
				block("        ", r.toolSchemaText)
			}
		}
	case rowKindResponse:
		// CONVERSATION FIRST: the assistant's reply (full resolved body
		// under showFull, else the preview) and its tool calls, THEN the
		// diagnostics section — status, stop reason, usage — with HTTP
		// headers dead LAST (the least interesting data, per live user
		// feedback; they never appear on the summary line at all).
		if m.showFull && r.assistantText != "" {
			lines = append(lines, fmt.Sprintf("      assistant %s:", humanBytes(int64(r.textBytes))))
			block("        ", r.assistantText)
		} else if r.textPreview != "" {
			lines = append(lines, fmt.Sprintf("      assistant %s  %s", humanBytes(int64(r.textBytes)), r.textPreview))
		}
		if len(r.toolCalls) > 0 {
			lines = append(lines, "      tool calls: "+strings.Join(r.toolCalls, ", "))
		}
		stop := r.stopReason
		if stop == "" {
			stop = "-"
		}
		lines = append(lines, diagnosticsMarker)
		lines = append(lines, fmt.Sprintf("      status %d  stop=%s", r.status, stop))
		if r.usage != nil {
			lines = append(lines, fmt.Sprintf("      usage  in=%s out=%s total=%s",
				humanCount(r.usage.InputTokens), humanCount(r.usage.OutputTokens), humanCount(r.usage.TotalTokens)))
		} else {
			lines = append(lines, "      usage  (not reported)")
		}
	case rowKindTool:
		state := "pending"
		if r.toolDone {
			okLabel := "ok"
			if !r.ok {
				okLabel = "FAIL"
			}
			state = fmt.Sprintf("%s %s %.1fs", okLabel, humanBytes(int64(r.resultBytes)), float64(r.durationMs)/1000)
		}
		lines = append(lines, fmt.Sprintf("      tool %s  source=%s  id=%s  %s", r.name, r.source, r.toolID, state))
		if r.argsSummary != "" {
			lines = append(lines, "      args: "+r.argsSummary)
		}
		if r.toolDone && r.resultSummary != "" {
			lines = append(lines, "      result: "+r.resultSummary)
		}
		if m.showFull {
			lines = append(lines, "      args:")
			block("        ", r.argsText)
			if r.toolDone {
				lines = append(lines, "      result:")
				block("        ", r.resultText)
			}
		}
	case rowKindContext:
		kind := r.ctxKind
		if kind == "" {
			kind = "-"
		}
		// detailFull is the sanitized MULTILINE copy (keepNewlines=true);
		// block splits it into physical lines like every other body, so a
		// multiline ctx detail expands readably instead of staying the
		// flattened one-liner the row summary shows. Fall back to the
		// flattened copy for rows built before detailFull existed.
		detail := r.detailFull
		if detail == "" {
			detail = r.detail
		}
		block("      ", kind+": "+detail)
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
	case rowKindResponse:
		r.assistantText = m.fetchBlobText(r.textHash)
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
	r.assistantText = ""
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
	headerStyle      = lipgloss.NewStyle().Bold(true)
	footerStyle      = lipgloss.NewStyle().Faint(true)
	filterStyle      = lipgloss.NewStyle().Italic(true)
	selectedStyle    = lipgloss.NewStyle().Reverse(true).Bold(true)
	groupHeaderStyle = lipgloss.NewStyle().Faint(true).Bold(true)
)

// RunTUI runs the bubbletea program to completion (blocking). Unit C calls
// this after wiring cfg from a live monitor.Hub. Requirement 1: runs in the
// terminal's alt screen (so a live monitor session never scrolls into, or
// leaves its frame behind in, the caller's shell scrollback).
//
// Mouse capture (tea.WithMouseCellMotion) is intentionally OFF: capturing
// mouse events disables the terminal's NATIVE text selection, which made
// copy/paste out of the monitor impossible (live user feedback). Copy/paste
// beats wheel scroll — keyboard nav (arrows/j/k/PgUp/PgDn/emacs keys)
// covers scrolling, and with no capture the terminal handles selection,
// copy, and wheel itself.
func RunTUI(cfg TUIConfig) error {
	p := tea.NewProgram(NewModel(cfg), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
