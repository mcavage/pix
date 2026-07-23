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
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
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

	// ts is the row's timestamp (unix millis, client clock — envelope TS),
	// used to render the optional HH:MM:SS column (showTimestamps/`T`). It
	// is FIRST-SEEN/creation time: set once, when the row is first created
	// (upsertRow preserves it across any later in-place overwrite — see
	// its doc comment), never bumped by a later event that merely mutates
	// the row (e.g. tool_end completing a tool_start row keeps the
	// tool_start's ts). A response row is only ever created once, by its
	// own provider_response event, so "first-seen" and "the response
	// event's TS" are the same value for it. 0 means unknown (no envelope
	// TS reached this row, e.g. a hand-built event in a test that never
	// set it) and renders as a blank column rather than a bogus time.
	ts int64

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
	// latencyMs is the model round-trip: this response's envelope TS minus
	// the matching provider_request's envelope TS (same session+turnId),
	// looked up via Model.reqTS at the moment the response row is created.
	// 0 means unknown (no matching request TS on record — e.g. the TUI
	// attached mid-turn, or a hand-built test event with no ts) and is
	// omitted from the rendered summary rather than shown as "0ms".
	latencyMs int

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

	// assistantRendered / newMessageRendered are the glamour-rendered
	// (markdown -> styled terminal text) counterparts of assistantText /
	// newMessageTexts: the NATURAL-LANGUAGE bodies formatted as real
	// markdown (lists, bold, tables, code) instead of raw text. Rendered in
	// resolveRowBlobs (Update path, R1-12 — never from View), from the
	// ALREADY-SANITIZED text (sanitize first, THEN glamour: the input is
	// untrusted event text, the output ANSI is ours and trusted — see
	// renderMarkdownLines), pre-wrapped to the available body width. Each
	// entry is one physical line that may carry trusted ANSI styling; nil
	// means "no rendered form" (width unknown, glamour failed) and
	// detailLines falls back to the plain line-split of the raw text.
	// Tool args/results and the system prompt are deliberately NOT
	// markdown-rendered — they are shell/JSON/prompt plumbing, not prose.
	// Same R3-2b retention lifecycle as the raw text they mirror: cleared
	// on collapse/showFull-off (clearRowBlobsAt), re-rendered on resize
	// (Update's WindowSizeMsg path calls refreshExpandedBlobs).
	assistantRendered  []string
	newMessageRendered [][]string

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
	// SystemPromptHash to render "(unchanged)" vs "(changed)" — keyed by
	// sessionKey(env) (sandboxId+"/"+sessionId), NOT global, because the
	// default `monitor` watches every sandbox/session at once and a
	// single shared value would let one session's hash mark another
	// session's first-seen prompt as "(unchanged)" (review finding R1-7).
	prevSysHash map[string]string

	// sessionModel/sessionTitle are keyed by sessionKey(env) and feed the
	// per-session tree-node header (sessionNodeLabel) so a multi-session
	// feed is self-explanatory — which model each session is on, and what
	// its user actually asked for — instead of a bare sandbox/session id. The
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

	// sessionParentRow records, per sessionKey, the id of the TOOL row
	// that most likely SPAWNED that session (e.g. a bash tool running
	// `pi -p --model X ...` boots a child pi session), so bodyLayoutLines
	// can nest the child session's subtree directly under that tool row —
	// in place in the conversation flow — instead of piling every child
	// session at the bottom of the primary. "" means "no plausible
	// spawner" and falls back to the old primary-level child node.
	//
	// Correlation is ONE-SHOT: computed by spawnParentRow the first time a
	// session is sighted in applyEvent (map presence — even with a ""
	// value — is the seen marker), never re-run per event. That timing
	// works because the spawning tool's tool_start reaches the hub before
	// the child pi process boots and emits anything. Entries are cleaned
	// up in evictOldRows when the session's last retained row is evicted
	// (same class as prevSysHash/R4-2). If the PARENT tool row is evicted
	// while the child session lives on, the stale row id simply no longer
	// matches any emitted row and the child gracefully falls back to a
	// primary-level child node (bodyLayoutLines nests only under rows it
	// actually emits).
	sessionParentRow map[string]string

	// reqTS tracks the envelope TS of the most recent provider_request per
	// turn, keyed by sess+"/"+env.TurnID (the SAME composite turn key the
	// request/response row ids are built from, minus their ":req"/":resp"
	// suffix) — so a provider_response landing later in the same turn can
	// compute its model round-trip latency (response TS minus request TS)
	// without a fresh scan of rows/rowIndex. Cleaned up in evictOldRows
	// alongside every other per-session/per-turn map here (same class as
	// R4-2/R1-13): when a request row is evicted, its reqTS entry goes with
	// it, so this never accumulates one entry per turn forever.
	reqTS map[string]int64

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

	expanded map[string]bool // row id -> expanded, toggled by `space`/`enter` on an event row

	// collapsed tracks TREE-NODE collapse state, keyed by nodeID
	// (sessionNodeID — see bodyLayoutLines' tree): true hides the node's
	// subtree lines (a collapsed session hides its own event rows — for
	// the primary session, its child-session nodes stay visible: they are
	// the user's other conversations, not the primary's payload).
	// Absent/false means expanded — the default, so a fresh view always
	// shows the whole tree. Entries are cleaned up in evictOldRows when
	// the node's last retained row is evicted (same class as
	// prevSysHash/R4-2).
	collapsed map[string]bool

	// sandboxRowCount mirrors sessionRowCount one level up: how many
	// retained rows belong to each sandbox (the sessionKey's sandbox half,
	// rowSandbox), so evictOldRows can tell when a sandbox's LAST row is
	// gone and its TAB (sandboxOrder/sandboxUnread/activeSandbox below)
	// must be cleaned up too (same class as R4-2).
	sandboxRowCount map[string]int

	// activeSandbox is the sandbox whose session tree the body currently
	// shows. Sandboxes are TABS (one visible at a time), not tree nodes —
	// with 5-10 concurrent sandboxes, stacking them all as top-level nodes
	// was untenable. Defaults to the first-seen sandbox; tab/shift+tab
	// (aliases ]/[) cycle, digits 1-9 jump to the Nth tab. "" is only
	// ambiguous while sandboxOrder is empty (no rows yet) — the (local)
	// sandbox's id is also "" — so "is there an active tab" is always
	// answered by len(sandboxOrder), never by activeSandbox != "".
	activeSandbox string
	// sandboxOrder is the tab order: sandboxes by FIRST-SEEN row, appended
	// when a sandbox's first row is inserted (upsertRow) and removed when
	// its last row is evicted (evictOldRows). Invariant: a sandbox is in
	// sandboxOrder iff sandboxRowCount[it] > 0.
	sandboxOrder []string
	// sandboxUnread marks BACKGROUND tabs that received events since they
	// were last active (the tab bar's • marker): set in applyEvent when an
	// event's sandbox != activeSandbox, cleared when the user switches to
	// that tab (setActiveSandbox), deleted with the tab on eviction.
	sandboxUnread map[string]bool

	// showTimestamps (`T`, capital — lowercase `t` is showTools) renders a
	// dim HH:MM:SS column (local time, from each row's first-seen ts) at
	// the very start of every body line. Default TRUE: unlike the other
	// show* toggles (which start OFF/summary-only per docs/design/
	// monitor.md), wall-clock context is useful from the first frame, not
	// something you opt into after noticing you need it.
	showTimestamps bool

	filtering   bool // `/` was pressed; subsequent runes build filterInput
	filterInput string
	filter      string // committed filter (Enter); substring match on the rendered row line

	// focusSession is SESSION FOCUS (solo) mode: "" (the default) shows
	// every session; a non-empty sessionKey(env) collapses the feed down
	// to ONLY that session's rows (see visibleRows), so a single coherent
	// conversation reads cleanly instead of interleaving with every other
	// concurrent session reaching the hub. Toggled by `s`
	// (toggleFocusAtCursor) on the session that owns the cursor's current
	// row or session-node line; `esc` also clears it (outside filtering/
	// help). While focused, bodyLayoutLines renders just that session's
	// node + its events, flat — no sandbox level, no sibling sessions —
	// and the sandbox/session/model/title context also shows in the top
	// bar (focusHeaderLine).
	focusSession string

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
		cfg:              cfg,
		rowIndex:         make(map[string]int),
		prevSysHash:      make(map[string]string),
		sessionModel:     make(map[string]string),
		sessionTitle:     make(map[string]string),
		sessionParentRow: make(map[string]string),
		reqTS:            make(map[string]int64),
		sessionRowCount:  make(map[string]int),
		sandboxRowCount:  make(map[string]int),
		sandboxUnread:    make(map[string]bool),
		expanded:         make(map[string]bool),
		collapsed:        make(map[string]bool),
		showModel:        true,
		showTools:        true,
		showMCP:          true,
		showContext:      true,
		showTimestamps:   true,
		follow:           true,
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
			curAnchor = captureLineAnchor(m.clampedCursor(lines), lines)
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
		widthChanged := !m.sized || m.width != msg.Width
		m.width = msg.Width
		m.height = msg.Height
		m.sized = true
		// A width change invalidates the glamour-rendered bodies (they are
		// pre-wrapped to the old width): re-resolve every expanded row's
		// blobs — which re-renders their markdown at the new width — the
		// same way toggling `f` on does. Only meaningful while showFull is
		// on (with it off no rendered bodies are retained at all, R3-2b).
		if widthChanged && m.showFull {
			m.refreshExpandedBlobs()
		}
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
	case "s":
		m.toggleFocusAtCursor()
	case "T":
		m.showTimestamps = !m.showTimestamps
	case "esc":
		// Nice-to-have: `esc` also clears session focus outside filtering/
		// help (both of which already claim `esc` for their own purposes
		// above, so this case only ever fires in the main keymap). A no-op
		// when nothing is focused.
		m.focusSession = ""
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
		m.toggleAtCursor()
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
	case "tab", "]":
		m.cycleSandbox(1)
	case "shift+tab", "[":
		m.cycleSandbox(-1)
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Digit N jumps straight to the Nth sandbox tab (first-seen
		// order); out-of-range digits are safe no-ops. While filtering,
		// digits are filter text (the filtering branch returned above).
		m.jumpToSandbox(int(key[0] - '0'))
	case "left", "right":
		// Deliberate no-ops: the feed has no horizontal axis, but arrow
		// keys must always be safe to press (never mis-typed into a
		// toggle, never a crash).
	}
	return m, nil
}

// lineAnchor pins one body line SEMANTICALLY — by the row (or tree node)
// that owns it, plus the line's offset within that row's contiguous block
// of lines — rather than by raw index, so eviction/insertion above it can
// be remapped (see the eventMsg case in Update). valid=false means the
// line was untied (empty-state hint, help) or out of range; nothing to
// remap then.
type lineAnchor struct {
	rowID  string
	nodeID string
	offset int
	valid  bool
}

// captureLineAnchor anchors the line at idx in the given layout. A tree
// node header anchors by its nodeID (always a single line); a row-owned
// line anchors by rowID + offset within the row's block.
func captureLineAnchor(idx int, lines []bodyLine) lineAnchor {
	if idx < 0 || idx >= len(lines) {
		return lineAnchor{}
	}
	if lines[idx].nodeID != "" {
		return lineAnchor{nodeID: lines[idx].nodeID, valid: true}
	}
	if lines[idx].rowID == "" {
		return lineAnchor{}
	}
	first := idx
	for first > 0 && lines[first-1].rowID == lines[idx].rowID {
		first--
	}
	return lineAnchor{rowID: lines[idx].rowID, offset: idx - first, valid: true}
}

// resolveLineAnchor maps an anchor back to a line index in the NEW layout:
// a node anchor to its (single) node header line; a row anchor to the
// row's first line plus the saved offset, clamped to the row's (possibly
// changed) line count. ok=false when the anchor is invalid or its row/node
// no longer exists in the layout (evicted / filtered out / collapsed away).
func resolveLineAnchor(a lineAnchor, lines []bodyLine) (int, bool) {
	if !a.valid {
		return 0, false
	}
	if a.nodeID != "" {
		if i := nodeLineIndex(lines, a.nodeID); i >= 0 {
			return i, true
		}
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
// layout: the follow target line while follow is on (see followLineIndex —
// this is what makes follow-mode auto-track new events with zero extra
// bookkeeping in applyEvent), otherwise the stored m.cursor clamped
// defensively into [0, total) — the layout can shrink out from under it
// (toggle/filter change, eviction, node collapse).
func (m Model) clampedCursor(lines []bodyLine) int {
	total := len(lines)
	if total <= 0 {
		return 0
	}
	if m.follow {
		return m.followLineIndex(lines)
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

// followLineIndex is the line follow tracks: the LAST line belonging to
// the NEWEST visible event row (its header line, or — when expanded — its
// last detail line). The tree can place that line MID-layout (a child
// session's node+events render after the primary session's events), so
// this is NOT simply the last layout line. When the newest event's session
// or sandbox node is collapsed, follow lands on the nearest visible
// ancestor node line instead — collapse is user intent, never
// force-expanded out from under them. The help overlay (and an
// untied-lines-only layout) follows the last line, so End/G still
// bottom-anchor there.
func (m Model) followLineIndex(lines []bodyLine) int {
	total := len(lines)
	if total == 0 {
		return 0
	}
	if m.showHelp {
		return total - 1
	}
	rows := m.visibleRows()
	if len(rows) == 0 {
		return total - 1
	}
	newest := rows[len(rows)-1] // visibleRows preserves arrival order
	last := -1
	for i, l := range lines {
		if l.rowID == newest.id {
			last = i
		}
	}
	if last >= 0 {
		return last
	}
	// Hidden under its collapsed session node: land on that node instead
	// — collapse is user intent, never force-expanded out from under them.
	if i := nodeLineIndex(lines, sessionNodeID(newest.session)); i >= 0 {
		return i
	}
	return total - 1
}

// nodeLineIndex is the index of the node header line carrying nodeID, or
// -1 when the layout doesn't contain it (collapsed ancestor, filtered
// away, different focus).
func nodeLineIndex(lines []bodyLine, nodeID string) int {
	for i, l := range lines {
		if l.nodeID == nodeID {
			return i
		}
	}
	return -1
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
	return lines[m.clampedCursor(lines)].rowID
}

// reconcileScroll updates m.scrollTop ("the body scrolls to keep the
// cursor LINE visible") after every Update path that can change the
// row/line layout — a new event, a toggle/filter/expand/nav key, or a
// resize. See the scrollTop field doc comment for why this is persisted
// state rather than something View recomputes from scratch every render.
//
// Follow and detached both get bubbles/viewport's "ensure visible"
// behavior on the effective cursor line: scrollTop only moves the minimum
// amount needed to bring it back inside [scrollTop, scrollTop+bodyHeight).
// While following a single conversation the follow line is the last
// layout line, so this still bottom-anchors exactly like the pre-tree
// code; when the tree places the follow line mid-layout (child sessions
// render below the primary's newest event) the window tracks THAT line
// instead of blindly jumping to the bottom. A detached view that's
// already showing the cursor line stays exactly where it is even as new
// rows keep appending below it.
func (m *Model) reconcileScroll() {
	bh := m.bodyHeight()
	if bh <= 0 {
		m.scrollTop = 0
		return
	}
	lines := m.bodyLayoutLines()
	total := len(lines)
	if total <= bh {
		m.scrollTop = 0
		return
	}
	cur := m.clampedCursor(lines)
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

// moveCursor shifts the LINE cursor by delta physical lines (arrows/
// ctrl+p/ctrl+n: ±1 — so an expanded 60-line payload is read by stepping
// THROUGH it, the audit's headline defect; PgUp/PgDn/ctrl+u/ctrl+d:
// ±pageSize) and updates follow: landing anywhere but the follow target
// line (followLineIndex — the newest event's line, or its nearest visible
// ancestor node when collapsed away) detaches it (so a live stream stops
// yanking the view while the user is reading); landing back on the follow
// line re-attaches it, same as pressing G. EVERY line of the tree is
// navigable — session node headers included (they're how collapse is
// reached) — so no line-skipping is needed.
func (m *Model) moveCursor(delta int) {
	if !m.navigableBody() {
		return
	}
	lines := m.bodyLayoutLines()
	total := len(lines)
	if total == 0 {
		return
	}
	cur := m.clampedCursor(lines) + delta
	if cur < 0 {
		cur = 0
	}
	if cur > total-1 {
		cur = total - 1
	}
	m.cursor = cur
	m.follow = cur == m.followLineIndex(lines)
}

// cursorToTop implements `g`/Home: jump to the first body line — the
// primary session node of the active sandbox, which is navigable — and detach
// follow (unless that line is also the follow target).
func (m *Model) cursorToTop() {
	if !m.navigableBody() {
		return
	}
	lines := m.bodyLayoutLines()
	if len(lines) == 0 {
		return
	}
	m.cursor = 0
	m.follow = m.followLineIndex(lines) == 0
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

// reattachFollow implements `G`/End: jump to (and keep tracking) the
// follow target line — clampedCursor derives the cursor from follow from
// here on, so no stored index needs updating per event.
func (m *Model) reattachFollow() {
	m.follow = true
	if lines := m.bodyLayoutLines(); len(lines) > 0 {
		m.cursor = m.followLineIndex(lines)
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

// toggleAtCursor implements `enter`/`space`, CONTEXT-SENSITIVE on the
// selected line: a session NODE header toggles that node's tree
// collapse (m.collapsed); an EVENT row line toggles that row's payload
// expand (m.expanded — the pre-tree behavior, on the row that OWNS the
// cursor's line: its header, or any of its expanded detail lines). One
// key, does the right thing for what's selected. The row path keeps the
// R3-2b memory discipline exactly as before: resolve blobs on expand
// (only while showFull is also on), clear them on collapse. After a
// collapse (node or row) the cursor SNAPS to the toggled node's/row's
// header line in the NEW layout, so it never lands on a now-removed line
// (which would silently jump it to whatever line slid up into that
// index).
func (m *Model) toggleAtCursor() {
	lines := m.bodyLayoutLines()
	if len(lines) == 0 {
		return
	}
	l := lines[m.clampedCursor(lines)]
	if l.nodeID != "" {
		if m.collapsed[l.nodeID] {
			delete(m.collapsed, l.nodeID) // expanded is the default: keep the map sparse
		} else {
			m.collapsed[l.nodeID] = true
		}
		after := m.bodyLayoutLines()
		if i := nodeLineIndex(after, l.nodeID); i >= 0 {
			m.cursor = i
			m.follow = i == m.followLineIndex(after)
		}
		return
	}
	id := l.rowID
	if id == "" {
		return // untied line (empty-state hint): nothing to toggle
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
	for i, al := range after {
		if al.rowID == id && al.isHeader {
			m.cursor = i
			m.follow = i == m.followLineIndex(after)
			return
		}
	}
}

// toggleFocusAtCursor implements `s`: SESSION FOCUS (solo) mode — solo
// the selected session subtree. The session comes from whatever the
// cursor's line is: an event row's owning session, or a session NODE
// header itself (nodeID). A sandbox node has no single session to solo,
// so `s` there is a no-op (as is an untied line — empty-state hint, help
// overlay). Toggle, not set: pressing `s` again on the ALREADY-focused
// session clears focus (back to "show all"). Cursor/scroll are left to
// the existing generic clamp (clampedCursor/reconcileScroll — the
// handleKey wrapper runs reconcileScroll right after this returns): the
// visible set just changed size, and follow==true (the default) already
// re-derives the follow line fresh from whatever that set now is; a
// detached cursor is clamped defensively the same way every other toggle
// (f/m/t/p/x/c) already relies on.
func (m *Model) toggleFocusAtCursor() {
	lines := m.bodyLayoutLines()
	if len(lines) == 0 {
		return
	}
	l := lines[m.clampedCursor(lines)]
	var sess string
	switch {
	case strings.HasPrefix(l.nodeID, sessionNodePrefix):
		sess = strings.TrimPrefix(l.nodeID, sessionNodePrefix)
	case l.rowID != "":
		idx, ok := m.rowIndex[l.rowID]
		if !ok {
			return
		}
		sess = m.rows[idx].session
	default:
		return // untied line (empty-state hint, help): nothing to solo
	}
	if sess == "" {
		return
	}
	if m.focusSession == sess {
		m.focusSession = ""
	} else {
		m.focusSession = sess
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
		// The session node's title is the FIRST real user ask for this
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
		// reqTS records this request's envelope TS, keyed by the shared
		// turn key (sess+turnId, no ":req" suffix), so the matching
		// provider_response — landing later, same turn — can compute its
		// model round-trip latency below.
		m.reqTS[sess+"/"+env.TurnID] = env.TS
		m.upsertRow(tuiRow{
			id:             id,
			session:        sess,
			turnID:         sanitizeText(env.TurnID, false),
			kind:           rowKindRequest,
			ts:             env.TS,
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
		// Model round-trip latency: this response's envelope TS minus the
		// matching request's (same turn key), looked up in reqTS — 0/unknown
		// when there's no recorded request TS for this turn (e.g. the TUI
		// attached mid-turn, or a hand-built event with no ts) or the clock
		// delta comes out non-positive.
		var latencyMs int
		if reqTS, ok := m.reqTS[sess+"/"+env.TurnID]; ok && reqTS > 0 && env.TS > reqTS {
			latencyMs = int(env.TS - reqTS)
		}
		m.upsertRow(tuiRow{
			id:          id,
			session:     sess,
			turnID:      sanitizeText(env.TurnID, false),
			kind:        rowKindResponse,
			ts:          env.TS,
			status:      ev.Status,
			stopReason:  sanitizeText(ev.StopReason, false),
			usage:       ev.Usage,
			textPreview: sanitizeText(ev.TextPreview, false),
			textBytes:   ev.TextBytes,
			textHash:    ev.TextHash,
			toolCalls:   sanitizeStrings(ev.ToolCalls),
			latencyMs:   latencyMs,
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
			ts:      env.TS,
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
				ts:      env.TS,
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
			ts:      env.TS,
			ctxKind: sanitizeText(ev.CtxKind, false),
			detail:  sanitizeText(ev.Detail, false),
			// keepNewlines=true: the expanded view renders this across
			// multiple physical lines (split by detailLines' block helper),
			// so a multiline ctx detail is actually readable instead of
			// being flattened+truncated into the single summary line.
			detailFull: sanitizeText(ev.Detail, true),
		})
	}
	// SPAWN CORRELATION, one-shot per session: the first time this session
	// is sighted (no sessionParentRow entry yet — "" counts as an entry),
	// find the tool row that most likely spawned it. This runs AFTER the
	// switch above so a first event that carries the session's model
	// (turn_start / provider_request) has already populated sessionModel —
	// the strong correlation signal.
	if _, seen := m.sessionParentRow[sess]; !seen {
		m.sessionParentRow[sess] = m.spawnParentRow(sess, env.TS)
	}
	// TAB unread marker: an event for a BACKGROUND sandbox (one that has a
	// tab — at least one retained row — and isn't the active one) lights
	// its • in the tab bar. It never moves the active view: visibleRows is
	// scoped to activeSandbox, so the layout the cursor/scroll/follow are
	// derived from doesn't change at all.
	if env.SandboxID != m.activeSandbox && m.sandboxRowCount[env.SandboxID] > 0 {
		m.sandboxUnread[env.SandboxID] = true
	}
}

// piInvocationRe matches a `pi` or `pi-stack` COMMAND TOKEN — anchored at
// the start of the string or right after a shell separator (whitespace,
// `;`, `&`, `|`, `(`), and followed by whitespace/quote/end — so it never
// substring-matches inside another word (`pip`, `api`, `spider`,
// `--provider`). A literal newline (multi-line bash, e.g. a `for ... do pi
// --print ...; done` heredoc) is a separator too: \s covers \n.
var piInvocationRe = regexp.MustCompile(`(?i)(^|[\s;&|(])(pi|pi-stack)([\s"']|$)`)

// toolInvokesPi reports whether a tool's ArgsSummary actually runs `pi` or
// `pi-stack` as a command (as opposed to merely mentioning something that
// contains those letters as a substring). Used to gate the time-window
// fallback signal in spawnParentRow: only a tool that could plausibly have
// spawned a pi child process is eligible for window-based correlation.
func toolInvokesPi(argsSummary string) bool {
	return piInvocationRe.MatchString(argsSummary)
}

// spawnParentRow finds the tool row in the SAME sandbox that most likely
// spawned the session sess (whose first event just arrived, at envelope TS
// childTS), returning its row id — or "" when nothing plausible matches
// (never a wild guess; "" renders as the pre-existing primary-level child
// node fallback). Heuristic, in order:
//
//  1. STRONG signal: a tool row whose ArgsSummary contains the child
//     session's model id (any `provider/` prefix stripped, case-insensitive
//     substring) — e.g. child model `google/gemini-3.6-flash` matches bash
//     args `pi -p --model gemini-3.6-flash ...`. This path is always
//     eligible, independent of toolInvokesPi.
//  2. Among model matches (or, when none match, among all candidates),
//     prefer a tool row whose execution WINDOW contains childTS:
//     [tool.ts, tool.ts+durationMs], open-ended to now while the tool
//     hasn't ended yet. With NO model match, window containment is
//     REQUIRED — a merely-preceding tool row is not plausible on its own —
//     AND the tool must be a pi invocation (toolInvokesPi): a coincidental
//     curl/grep/etc. tool that happens to be running when an unrelated pi
//     session starts must not adopt it.
//  3. Tiebreak: the latest start ts that is still <= childTS (the most
//     recent preceding tool); equal ts falls to the latest-inserted row.
//
// Candidates are tool rows of OTHER sessions in the same sandbox that did
// not start after the child's first event (when both timestamps are
// known). Called from applyEvent (Update) only — never from View.
func (m *Model) spawnParentRow(sess string, childTS int64) string {
	sandbox := rowSandbox(sess)
	childModel := strings.ToLower(m.sessionModel[sess])
	if i := strings.LastIndex(childModel, "/"); i >= 0 {
		childModel = childModel[i+1:]
	}
	type candidate struct {
		idx        int
		ts         int64
		modelMatch bool
		inWindow   bool
		invokesPi  bool
	}
	var cands []candidate
	anyModel := false
	for i, r := range m.rows {
		if r.kind != rowKindTool || r.session == sess || rowSandbox(r.session) != sandbox {
			continue
		}
		if childTS > 0 && r.ts > 0 && r.ts > childTS {
			continue // started after the child's first event: can't have spawned it
		}
		c := candidate{idx: i, ts: r.ts, invokesPi: toolInvokesPi(r.argsSummary)}
		if childModel != "" && strings.Contains(strings.ToLower(r.argsSummary), childModel) {
			c.modelMatch = true
			anyModel = true
		}
		if r.ts > 0 && childTS > 0 {
			if !r.toolDone {
				c.inWindow = true // still running: window is open-ended to now
			} else {
				c.inWindow = childTS <= r.ts+int64(r.durationMs)
			}
		}
		cands = append(cands, c)
	}
	best := -1
	for i, c := range cands {
		if anyModel && !c.modelMatch {
			continue // model matches exist: only they are in the running
		}
		if !anyModel && (!c.inWindow || !c.invokesPi) {
			continue // no model signal anywhere: require window containment AND a pi-invoking tool
		}
		if best < 0 {
			best = i
			continue
		}
		b := cands[best]
		switch {
		case c.inWindow != b.inWindow:
			if c.inWindow {
				best = i
			}
		case c.ts != b.ts:
			if c.ts > b.ts {
				best = i
			}
		default:
			best = i // equal ts: the latest-inserted row wins
		}
	}
	if best < 0 {
		return ""
	}
	return m.rows[cands[best].idx].id
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
		// changes how many rows a session has retained). ts is first-seen/
		// creation time (see the tuiRow.ts doc comment): a re-fired event
		// for the same id (e.g. a replayed provider_request) must not bump
		// it, so the row's original ts wins whenever one was already
		// recorded.
		if old := m.rows[idx].ts; old != 0 {
			row.ts = old
		}
		m.rows[idx] = row
		return
	}
	m.rowIndex[row.id] = len(m.rows)
	m.rows = append(m.rows, row)
	if row.session != "" {
		sandbox := rowSandbox(row.session)
		if m.sandboxRowCount[sandbox] == 0 {
			// First retained row for this sandbox: it becomes a TAB, in
			// first-seen order. The very first sandbox seen becomes the
			// active tab (the default view).
			m.sandboxOrder = append(m.sandboxOrder, sandbox)
			if len(m.sandboxOrder) == 1 {
				m.activeSandbox = sandbox
			}
		}
		m.sessionRowCount[row.session]++
		m.sandboxRowCount[sandbox]++
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
// so delta ((unchanged) vs (changed)) computation for it keeps working.
func (m *Model) evictOldRows() {
	if len(m.rows) <= maxRows {
		return
	}
	drop := len(m.rows) - maxRows
	for _, r := range m.rows[:drop] {
		delete(m.rowIndex, r.id)
		delete(m.expanded, r.id)
		// A dropped request row's reqTS entry goes with it (same class as
		// R4-2/R1-13 below): otherwise reqTS keeps one entry per turn ever
		// seen, forever, even long after the request row itself is gone.
		// The matching response row (if retained) already computed its
		// latencyMs at creation time, so it needs no further lookups.
		if r.kind == rowKindRequest {
			delete(m.reqTS, r.session+"/"+r.turnID)
		}
		if r.session == "" {
			continue
		}
		m.sessionRowCount[r.session]--
		if m.sessionRowCount[r.session] <= 0 {
			delete(m.sessionRowCount, r.session)
			delete(m.prevSysHash, r.session)
			delete(m.sessionModel, r.session)
			delete(m.sessionTitle, r.session)
			// The session's spawn-correlation entry (and its one-shot
			// "seen" marker) dies with its last retained row too (same
			// class as R4-2). If the session comes back it re-correlates.
			delete(m.sessionParentRow, r.session)
			// The session's tree-node collapse state dies with its last
			// retained row too (same class as R4-2).
			delete(m.collapsed, sessionNodeID(r.session))
		}
		sandbox := rowSandbox(r.session)
		m.sandboxRowCount[sandbox]--
		if m.sandboxRowCount[sandbox] <= 0 {
			// The sandbox's last retained row is gone: its TAB dies with
			// it — drop it from the tab order and the unread set (same
			// class as the per-session cleanup above / R4-2).
			delete(m.sandboxRowCount, sandbox)
			delete(m.sandboxUnread, sandbox)
			for i, sb := range m.sandboxOrder {
				if sb == sandbox {
					m.sandboxOrder = append(m.sandboxOrder[:i], m.sandboxOrder[i+1:]...)
					break
				}
			}
			if m.activeSandbox == sandbox {
				// The ACTIVE tab was evicted: fall back to the first
				// remaining tab (or none), re-attached to its live feed.
				if len(m.sandboxOrder) > 0 {
					m.activeSandbox = m.sandboxOrder[0]
					delete(m.sandboxUnread, m.activeSandbox)
					m.follow = true
				} else {
					m.activeSandbox = ""
				}
			}
		}
	}
	m.rows = append([]tuiRow(nil), m.rows[drop:]...)
	for i, r := range m.rows {
		m.rowIndex[r.id] = i
	}
}

// visibleRows applies the ACTIVE SANDBOX TAB, the show* toggles, the
// active session focus, and the active text filter, in that order,
// preserving row order. Scoping to the active tab here — rather than in
// bodyLayoutLines — makes everything downstream (follow's "newest visible
// row", navigableBody, the focus empty-state) automatically per-tab: a new
// event in a background sandbox changes NOTHING the cursor/scroll/follow
// are derived from.
func (m Model) visibleRows() []tuiRow {
	var out []tuiRow
	for _, r := range m.rows {
		// Only the active sandbox's rows are in view — sandboxes are
		// tabs (Model.activeSandbox), not tree nodes.
		if rowSandbox(r.session) != m.activeSandbox {
			continue
		}
		if !m.passesToggles(r) {
			continue
		}
		// SESSION FOCUS (solo) mode (`s`): only the focused session's rows
		// pass, so a new event for any OTHER session never appears (and
		// never steals follow) while focused.
		if m.focusSession != "" && r.session != m.focusSession {
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
		// turn still renders, the tree's session nodes still see a real
		// row to key off of, and toggling something else off/on never
		// resurrects it.
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
	// nodeID marks a TREE NODE header line (a session — see
	// sessionNodeID) and is the key into Model.collapsed.
	// Node headers are NAVIGABLE (the line cursor lands on them; enter/
	// space toggles their collapse) but own no row (rowID=="").
	nodeID string
	// pre marks a glamour PRE-RENDERED markdown line: it carries TRUSTED
	// ANSI styling (generated by us from already-sanitized text — see
	// renderMarkdownLines) and is already word-wrapped to the body width,
	// so renderBodyLines must NOT run the plain rune/cell truncate on it
	// (that would clip mid-escape-sequence); the defensive width guard for
	// these lines is the ANSI-AWARE clampAnsiLine instead.
	pre bool
	// ts is the owning row's tuiRow.ts (for a node header: the newest ts
	// anywhere in its subtree — its latest activity; 0 for an untied line
	// — help overlay, empty-state hint), used by renderBodyLines to
	// render the showTimestamps HH:MM:SS column (or blanks) aligned across
	// every line kind.
	ts int64
}

// View renders the current state. PURE: no channel reads, no goroutines, no
// tea.Program access — see the Model doc comment and architecture.md 3.B
// ("View() must be a PURE function of Model state"). Requirement 1's
// height==0 sentinel (no WindowSizeMsg yet) keeps this fully unbounded —
// every existing substring-based unit test still passes unmodified; only
// height>0 clamps the body to a fixed number of lines and scrolls it
// (requirements 1/3/4).
func (m Model) View() string {
	header, tabs, filter, footer := m.chrome()
	var parts []string
	if header {
		parts = append(parts, headerStyle.Render(m.truncate(m.headerLine())))
	}
	if tabs {
		parts = append(parts, m.renderTabBar())
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
// terminal is too short for all of it — the sandbox tab bar drops first
// (it needs height>=4; it only exists with >=2 sandboxes), then the
// filter line (height>=3), then the footer (height>=2), then the header
// (height>=1) — so the TOTAL rendered line count (chrome + body, see
// bodyHeight) never exceeds the terminal height, even at height 0-3.
func (m Model) chrome() (header, tabs, filter, footer bool) {
	filterActive := m.filtering || m.filter != ""
	// The tab bar only renders with two or more sandbox tabs — a single
	// sandbox puts its name in the title line instead (headerLine).
	hasTabs := len(m.sandboxOrder) >= 2
	if !m.sized {
		return true, hasTabs, filterActive, true
	}
	return m.height >= 1, hasTabs && m.height >= 4, filterActive && m.height >= 3, m.height >= 2
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
	header, tabs, filter, footer := m.chrome()
	h := m.height
	if header {
		h--
	}
	if tabs {
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
// body by FLATTENING the collapsible tree (sandbox -> session -> events):
// one line per visible node header and row header (each with its ▸/▾
// caret), plus one line per expanded-detail physical line (detailLines
// already split every multi-line body on '\n'). This is the single source
// of truth for line positions — the cursor, reconcileScroll,
// toggle-at-cursor, and renderBodyLines all index into the SAME layout —
// so no line here ever contains a '\n'. Text is raw apart from the tree
// indent (no gutter, no truncation, no styling); renderBodyLines
// decorates it. The help overlay REPLACES this entirely rather than being
// one more toggle over the row feed.
func (m Model) bodyLayoutLines() []bodyLine {
	if m.showHelp {
		return m.helpBodyLines()
	}
	rows := m.visibleRows()
	if len(rows) == 0 {
		if m.focusSession != "" {
			// The focused session's rows all evicted out from under it
			// (maxRows churn) rather than the feed being empty overall —
			// stay focused (the user's `s` press isn't silently undone) and
			// say so, with the way back out right there in the message.
			return []bodyLine{
				{text: "(focused session has no events in view — press s to unfocus)"},
			}
		}
		return []bodyLine{
			{text: "(no events yet)"},
			{text: fmt.Sprintf("waiting for a monitor-enabled sandbox on :%d — if nothing appears, the sandbox may predate the monitor extension (rebuild image / make load)", m.hubPort())},
		}
	}
	// The body shows the ACTIVE SANDBOX ONLY (sandboxes are TABS, not tree
	// nodes — visibleRows already scoped rows to Model.activeSandbox), as a
	// COLLAPSIBLE SESSION TREE with no sandbox level: the FIRST-SEEN
	// session is the sandbox's PRIMARY conversation (depth 0), its events
	// at depth 1; every other session nests under the primary as a child
	// node (depth 1, its events at depth 2) — the user's model: any pi
	// session is a child of the top one in the sandbox. Each session's
	// event rows render CONTIGUOUSLY, in arrival order — never interleaved
	// with another session's, even when the events arrived interleaved.
	// Node headers are NAVIGABLE and collapsible (m.collapsed); a collapsed
	// subtree contributes no lines. Indent is 2 spaces per depth, applied
	// here (renderBodyLines adds the timestamp column + cursor gutter in
	// front of it). SESSION FOCUS (`s`) needs no special-casing any more:
	// visibleRows solos the focused session, which then renders as the
	// (single) primary at depth 0 — exactly the old solo layout.
	var lines []bodyLine
	emitRow := func(r tuiRow, depth int) {
		indent := strings.Repeat("  ", depth)
		// ▸/▾ is the payload-expand affordance: every row kind has
		// detail, so every header carries a caret.
		caret := "\u25b8 "
		if m.expanded[r.id] {
			caret = "\u25be "
		}
		lines = append(lines, bodyLine{text: indent + caret + m.renderRow(r), rowID: r.id, isHeader: true, ts: r.ts})
		if m.expanded[r.id] {
			for _, dl := range m.detailLines(r) {
				lines = append(lines, bodyLine{text: indent + dl.text, rowID: r.id, ts: r.ts, pre: dl.pre})
			}
		}
	}
	emitNode := func(nodeID, label string, depth int, ts int64) {
		caret := "\u25be "
		if m.collapsed[nodeID] {
			caret = "\u25b8 "
		}
		lines = append(lines, bodyLine{text: strings.Repeat("  ", depth) + caret + label, nodeID: nodeID, ts: ts})
	}
	sessions := groupSessionRows(rows)
	// SPAWN NESTING: a session whose sessionParentRow names a tool row is
	// rendered nested directly under that tool row — in place in the
	// spawning session's conversation flow — one level deeper than the
	// tool (tool at depth d → child session node at d+1 → its events at
	// d+2). byParent indexes the non-primary sessions by their recorded
	// parent tool row id, in first-seen session order; nesting only
	// happens under rows this layout pass actually EMITS, so a parent
	// tool row that was evicted, filtered out (`t` off, text filter), or
	// hidden under a collapsed node leaves its child to the fallback loop
	// at the bottom — a primary-level child node, exactly the old
	// behavior. The emitted set makes this cycle-proof (a session renders
	// at most once, however the correlation map is shaped).
	byParent := make(map[string][]*sessionGroup)
	for _, ses := range sessions[1:] {
		if pid := m.sessionParentRow[ses.session]; pid != "" {
			byParent[pid] = append(byParent[pid], ses)
		}
	}
	emitted := make(map[string]bool)
	var emitSession func(ses *sessionGroup, depth int)
	emitSession = func(ses *sessionGroup, depth int) {
		emitted[ses.session] = true
		nid := sessionNodeID(ses.session)
		emitNode(nid, m.sessionNodeLabel(ses.session), depth, ses.latestTS)
		if m.collapsed[nid] {
			// Collapsing a session hides only its OWN events; sessions
			// spawned from its tool rows are the user's other
			// conversations, not this session's payload — they fall back
			// to primary-level child nodes via the loop below.
			return
		}
		for _, r := range ses.rows {
			emitRow(r, depth+1)
			for _, child := range byParent[r.id] {
				if !emitted[child.session] {
					emitSession(child, depth+2)
				}
			}
		}
	}
	emitSession(sessions[0], 0)
	for _, ses := range sessions[1:] {
		// Fallback: any session not already nested under its spawning
		// tool row (no correlation, or the parent row isn't in this
		// layout) renders as a child node under the primary, as before.
		if !emitted[ses.session] {
			emitSession(ses, 1)
		}
	}
	return lines
}

// sessionGroup is the tree's grouping bucket, built fresh from the
// visible rows on every layout pass by groupSessionRows.
type sessionGroup struct {
	session  string // sessionKey — raw, identity only, never rendered
	rows     []tuiRow
	latestTS int64 // newest row ts in this session (node-header timestamp)
}

// groupSessionRows groups the (already tab/toggle/focus/filter-passed)
// rows by session, in FIRST-SEEN order — rows preserve arrival order in
// m.rows, so first appearance here IS earliest-seen. Events stay in
// arrival order within their session and are never reordered, so a
// session's conversation reads chronologically and contiguously.
func groupSessionRows(rows []tuiRow) []*sessionGroup {
	var out []*sessionGroup
	sesIdx := make(map[string]*sessionGroup)
	for _, r := range rows {
		ses, ok := sesIdx[r.session]
		if !ok {
			ses = &sessionGroup{session: r.session}
			sesIdx[r.session] = ses
			out = append(out, ses)
		}
		ses.rows = append(ses.rows, r)
		if r.ts > ses.latestTS {
			ses.latestTS = r.ts
		}
	}
	return out
}

// rowSandbox extracts the sandbox half of a sessionKey (sandboxId+"/"+
// sessionId — the same Cut convention focusHeaderLine uses).
func rowSandbox(session string) string {
	sandbox, _, _ := strings.Cut(session, "/")
	return sandbox
}

// sessionNodePrefix namespaces the collapse-state keys (Model.collapsed)
// so a future non-session node kind could never collide with a session
// node's key.
const sessionNodePrefix = "session:"

// sessionNodeID is the stable node id tree headers carry
// (bodyLine.nodeID) and collapse state is keyed by.
func sessionNodeID(session string) string { return sessionNodePrefix + session }

// sessionNodeLabel renders a session node's header text (after the
// caret): `<short-session-id> · <model> · “<first user prompt>”`, e.g.
// `a9bd1638 · claude-opus-4-8 · “hi there”`. Model/title come from
// sessionModel/sessionTitle; either segment is omitted when not yet
// captured (no turn_start/provider_request yet, or no user-triggered
// prompt yet). Session id, model, and title are all event-derived
// attacker text, so they're sanitized here too (the map values are
// already sanitized when stored — this stays defense-in-depth rather than
// trusting the map). The session id is shortened to its last 8 runes (the
// tail is the distinctive part of a generated id); empty reads "-".
func (m Model) sessionNodeLabel(session string) string {
	_, sess, _ := strings.Cut(session, "/")
	sess = sanitizeText(sess, false)
	if r := []rune(sess); len(r) > 8 {
		sess = string(r[len(r)-8:])
	}
	if sess == "" {
		sess = "-"
	}
	label := sess
	if model := sanitizeText(m.sessionModel[session], false); model != "" {
		label += " \u00b7 " + model
	}
	if title := sanitizeText(m.sessionTitle[session], false); title != "" {
		label += fmt.Sprintf(" \u00b7 \u201c%s\u201d", title)
	}
	return label
}

// renderBodyLines decorates bodyLayoutLines for display: a "> "/"  " gutter
// on row and node lines (the marker lands on the header of the row that
// OWNS the cursor line, or on the selected node header — visible/
// assertable independent of terminal color support, since lipgloss
// renders plain whenever stdout isn't a TTY, e.g. every unit test),
// per-line truncation of the FULL composed line to the terminal width (so
// nothing — gutter included — escapes the frame), and reverse styling on
// the exact cursor line so line-granular position is legible on a real
// color terminal.
func (m Model) renderBodyLines() []bodyLine {
	lines := m.bodyLayoutLines()
	if len(lines) == 0 {
		return lines
	}
	cur := m.clampedCursor(lines)
	selRow := lines[cur].rowID
	selNode := lines[cur].nodeID
	out := make([]bodyLine, len(lines))
	for i, l := range lines {
		text := l.text
		if l.rowID != "" || l.nodeID != "" {
			gutter := "  "
			if (l.isHeader && l.rowID != "" && l.rowID == selRow) ||
				(l.nodeID != "" && l.nodeID == selNode) {
				gutter = "> "
			}
			text = gutter + text
		}
		// The HH:MM:SS column goes at the VERY start of the line — before
		// the cursor gutter/indent — so it reads as its own fixed-width
		// column across every line kind: node headers, row headers, and
		// expanded detail lines alike (see the bodyLine.ts doc comment; a
		// line with no known ts, or the help overlay, renders blanks so
		// the column still aligns). It's part of the line like everything
		// else, so the width-truncation pass right below still clamps it
		// along with the rest.
		if m.showTimestamps && !m.showHelp {
			text = timestampPrefix(l.ts) + text
		}
		if l.pre {
			// Glamour pre-rendered markdown: already wrapped to the body
			// width and carrying trusted ANSI — the plain rune/cell
			// truncate would clip mid-escape-sequence, so these lines get
			// the ANSI-AWARE clamp instead (normally a no-op: they fit).
			text = clampAnsiLine(text, m.width)
		} else {
			text = m.truncate(text)
		}
		switch {
		case i == cur && (l.rowID != "" || l.nodeID != ""):
			// On a pre line the reverse highlight only survives up to the
			// first embedded style reset — the leading timestamp/indent
			// still flips, which is enough to read cursor position.
			text = selectedStyle.Render(text)
		case l.nodeID != "":
			// Node headers are dimmed+bold structure lines — legible as
			// the tree's skeleton without competing with event content.
			text = nodeHeaderStyle.Render(text)
		}
		out[i] = bodyLine{text: text, rowID: l.rowID, isHeader: l.isHeader, nodeID: l.nodeID, pre: l.pre}
	}
	return out
}

// timestampWidth is the rendered width of the showTimestamps column: an
// 8-char "HH:MM:SS" plus one trailing space, so it reads as a fixed column
// whether or not a given line has a known ts.
const timestampWidth = len("15:04:05") + 1

// timestampPrefix renders the showTimestamps column for one body line: the
// line's ts (unix millis — a row's first-seen ts, or a node's latest
// activity) formatted in LOCAL time as "HH:MM:SS ", or timestampWidth
// blanks when ts is 0/unknown (an untied line, or a row whose creating
// event carried no envelope ts) — so the column stays aligned across
// every line kind.
func timestampPrefix(ts int64) string {
	if ts <= 0 {
		return strings.Repeat(" ", timestampWidth)
	}
	return time.UnixMilli(ts).Format("15:04:05") + " "
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
		"sandbox tabs",
		"  each sandbox is a TAB in the bar under the title; the body shows",
		"  only the active sandbox's sessions. • marks a background tab",
		"  with unseen events.",
		"  tab / shift+tab   next / previous sandbox (aliases: ] and [)",
		"  1-9               jump to the Nth sandbox tab",
		"",
		"tree",
		"  the body is the active sandbox's session tree: session ▸ events.",
		"  the first-seen session is the sandbox's PRIMARY conversation;",
		"  a session spawned by a tool call (e.g. a shelled-out `pi -p`)",
		"  nests under that tool row; any other session nests under the",
		"  primary as a child node. session headers are navigable lines",
		"  like any event row.",
		"",
		"navigation (line-granular: expanded payloads scroll line by line)",
		"  up, down        move one line (emacs: ctrl+p / ctrl+n)",
		"  g/Home, alt+<   jump to the top",
		"  G/End, alt+>    jump to the newest event (re-attach follow)",
		"  PgUp/PgDn       page up/down (ctrl+u/ctrl+d; emacs: alt+v / ctrl+v)",
		"",
		"enter / space — context-sensitive on the selected line (▸/▾)",
		"  on a session node: collapse/expand that subtree",
		"  on an event row: expand/collapse its payload detail",
		"",
		"toggles",
		"  T               toggle timestamps",
		"  f               full payloads (system prompt / message / assistant / tool bodies)",
		"  m               model request/response rows",
		"  t               tool rows",
		"  p               mcp tool rows",
		"  x               thinking-level context rows",
		"  c               context rows",
		"  s               focus/unfocus the selected session (solo its subtree)",
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
	if m.focusSession != "" {
		return m.focusHeaderLine()
	}
	if len(m.sandboxOrder) == 1 {
		// ONE sandbox: no tab bar renders (chrome), so the sandbox name
		// lives in the title line instead.
		return fmt.Sprintf("pi-stack monitor  %s  events=%d", sandboxTabName(m.sandboxOrder[0]), len(m.rows))
	}
	sandbox := m.cfg.Filter
	if sandbox == "" {
		sandbox = "all"
	}
	return fmt.Sprintf("pi-stack monitor  sandbox=%s  events=%d", sandbox, len(m.rows))
}

// sandboxTabName is a sandbox id's display name in the tab bar / title
// line: sanitized, with the empty (local) sandbox id reading "(local)".
func sandboxTabName(sandbox string) string {
	name := sanitizeText(sandbox, false)
	if name == "" {
		name = "(local)"
	}
	return name
}

// tabBarLine composes the sandbox TAB BAR (one entry per sandbox,
// first-seen order): the ACTIVE tab bracketed `[name]` — a
// color-independent marker, assertable headless (renderTabBar additionally
// reverse/bold-styles it on a real terminal) — and a background tab with
// unseen events suffixed `•` (sandboxUnread). When the full bar exceeds
// the terminal width it is windowed around the active tab (the active tab
// always stays visible), with `…` marking hidden tabs on either side.
// Returns the plain, already width-clamped line plus the active tab's
// entry text so renderTabBar can style exactly that segment. PURE: reads
// Model state only — tab state changes happen exclusively in Update.
func (m Model) tabBarLine() (string, string) {
	entries := make([]string, len(m.sandboxOrder))
	active := 0
	for i, sb := range m.sandboxOrder {
		switch {
		case sb == m.activeSandbox:
			entries[i] = "[" + sandboxTabName(sb) + "]"
			active = i
		case m.sandboxUnread[sb]:
			entries[i] = sandboxTabName(sb) + "\u2022"
		default:
			entries[i] = sandboxTabName(sb)
		}
	}
	window := func(lo, hi int) string {
		s := strings.Join(entries[lo:hi+1], "  ")
		if lo > 0 {
			s = "\u2026 " + s
		}
		if hi < len(entries)-1 {
			s += " \u2026"
		}
		return s
	}
	full := window(0, len(entries)-1)
	if m.width <= 0 || runewidth.StringWidth(full) <= m.width {
		return full, entries[active]
	}
	// Too wide: grow a window outward from the active tab, right then
	// left, while it still fits.
	lo, hi := active, active
	for {
		grew := false
		if hi+1 < len(entries) && runewidth.StringWidth(window(lo, hi+1)) <= m.width {
			hi++
			grew = true
		}
		if lo > 0 && runewidth.StringWidth(window(lo-1, hi)) <= m.width {
			lo--
			grew = true
		}
		if !grew {
			break
		}
	}
	// The active tab alone can still exceed a very narrow terminal —
	// truncateLine is the final defensive clamp.
	return truncateLine(window(lo, hi), m.width), entries[active]
}

// renderTabBar decorates tabBarLine for display: the active tab's entry
// (already bracket-marked) additionally gets the reverse/bold selected
// style so it pops on a real color terminal; headless (no-color) renders
// leave just the brackets, which is what the tests assert on.
func (m Model) renderTabBar() string {
	line, active := m.tabBarLine()
	if i := strings.Index(line, active); i >= 0 {
		line = line[:i] + selectedStyle.Render(active) + line[i+len(active):]
	}
	return line
}

// setActiveSandbox switches the visible tab: clears the target's unread
// marker, drops a session focus that belonged to the OLD sandbox (focus
// is scoped to the active tab), and re-attaches follow so the switched-to
// feed opens at its newest event. Called only from Update paths (keys,
// eviction fallback) — View never mutates tab state.
func (m *Model) setActiveSandbox(sandbox string) {
	if sandbox == m.activeSandbox {
		return
	}
	m.activeSandbox = sandbox
	delete(m.sandboxUnread, sandbox)
	if m.focusSession != "" && rowSandbox(m.focusSession) != sandbox {
		m.focusSession = ""
	}
	m.reattachFollow()
}

// cycleSandbox moves the active tab by delta (tab/`]` = +1, shift+tab/`[`
// = -1), wrapping around the tab order. A no-op with fewer than two tabs.
func (m *Model) cycleSandbox(delta int) {
	n := len(m.sandboxOrder)
	if n < 2 {
		return
	}
	cur := 0
	for i, sb := range m.sandboxOrder {
		if sb == m.activeSandbox {
			cur = i
			break
		}
	}
	m.setActiveSandbox(m.sandboxOrder[((cur+delta)%n+n)%n])
}

// jumpToSandbox implements the digit keys: jump straight to the Nth tab
// (1-based, first-seen order). Out-of-range is a safe no-op.
func (m *Model) jumpToSandbox(n int) {
	if n < 1 || n > len(m.sandboxOrder) {
		return
	}
	m.setActiveSandbox(m.sandboxOrder[n-1])
}

// focusHeaderLine renders the top bar while SESSION FOCUS is active
// (m.focusSession != ""): the sandbox level of the tree is suppressed
// from the body when focused (see bodyLayoutLines), so the full
// sandbox/session/model/title context lives here. Same
// sandbox/session-short/model/title derivation as sessionNodeLabel (kept
// as its own function rather than shared, since the surrounding
// punctuation differs), same sanitize-again defense in depth even though
// the map values are already sanitized.
// events=<n> counts only the FOCUSED session's retained rows
// (sessionRowCount), not the global len(m.rows) the unfocused header
// shows — the number on screen should describe what's actually in view.
func (m Model) focusHeaderLine() string {
	sandbox, sess, _ := strings.Cut(m.focusSession, "/")
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
	label := sandbox + "/" + sess
	if model := sanitizeText(m.sessionModel[m.focusSession], false); model != "" {
		label += " " + model
	}
	if title := sanitizeText(m.sessionTitle[m.focusSession], false); title != "" {
		label += fmt.Sprintf(" \u201c%s\u201d", title)
	}
	return fmt.Sprintf("pi-stack monitor  focus %s  events=%d", label, m.sessionRowCount[m.focusSession])
}

func (m Model) footerLine() string {
	follow := "[following]"
	if !m.follow {
		follow = "[paused]"
	}
	// s:focus=off/[focus]: unfocused reads like every other toggle
	// ("key:name=state"); focused is called out with brackets like
	// follow/paused above it, since it's a mode change that reshapes the
	// WHOLE feed rather than a per-row display toggle.
	focus := "s:focus=off"
	if m.focusSession != "" {
		focus = "[focus]"
	}
	// With two or more sandbox tabs, a compact hint advertises the tab
	// switching keys; a single sandbox has nothing to switch to.
	tabHint := ""
	if len(m.sandboxOrder) >= 2 {
		tabHint = "tab:sandbox  "
	}
	return fmt.Sprintf(
		"%s %s T:time=%s f:full=%s m:model=%s t:tools=%s p:mcp=%s x:think=%s c:ctx=%s  %snav:\u2191\u2193  enter/space:toggle  /:filter  ?:help  q:quit",
		follow, focus, onoff(m.showTimestamps),
		onoff(m.showFull), onoff(m.showModel), onoff(m.showTools), onoff(m.showMCP),
		onoff(m.showThinking), onoff(m.showContext), tabHint)
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
	label := "changed"
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
	line := fmt.Sprintf("%s  \u00b7 resp %d  stop=%s  in %s out %s", head, r.status, stop, in, out)
	// Model round-trip latency (this response's TS minus the matching
	// request's TS, computed in applyEvent) tacks on as one more ·-
	// separated segment, same convention as the rest of this suffix.
	// Omitted entirely when unknown (0) — no "0ms"/bogus latency ever
	// renders.
	if r.latencyMs > 0 {
		line += "  \u00b7 " + humanDuration(r.latencyMs)
	}
	return line
}

func renderToolRow(r tuiRow) string {
	base := fmt.Sprintf("   tool  %-10s source=%-12s %s", r.name, r.source, r.argsSummary)
	if r.toolDone {
		okLabel := "ok"
		if !r.ok {
			okLabel = "FAIL"
		}
		base += fmt.Sprintf("  \u2192 %s %s %s", okLabel, humanBytes(int64(r.resultBytes)), humanDuration(r.durationMs))
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

// detailLine is one physical line of a row's expanded detail. pre marks a
// glamour pre-rendered markdown line (trusted ANSI, already wrapped — see
// bodyLine.pre for the truncation contract).
type detailLine struct {
	text string
	pre  bool
}

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
// finding R1-12); blob lookup happens in Update, not here. The
// NATURAL-LANGUAGE bodies (assistant reply, user/assistant message bodies)
// prefer their glamour-rendered form (assistantRendered/
// newMessageRendered, also stored by resolveRowBlobs) and fall back to the
// plain line-split when none exists (width unknown, glamour failed);
// tool args/results and the system prompt always render plain.
func (m Model) detailLines(r tuiRow) []detailLine {
	var lines []detailLine
	add := func(s string) {
		lines = append(lines, detailLine{text: s})
	}
	block := func(prefix, body string) {
		for _, ln := range strings.Split(body, "\n") {
			add(prefix + ln)
		}
	}
	// preBlock emits glamour pre-rendered lines (trusted ANSI, already
	// wrapped to the body width — renderBodyLines skips the plain
	// truncate on them, see bodyLine.pre).
	preBlock := func(prefix string, rendered []string) {
		for _, ln := range rendered {
			lines = append(lines, detailLine{text: prefix + ln, pre: true})
		}
	}
	switch r.kind {
	case rowKindRequest:
		// CONVERSATION FIRST: the actual prompt — every new message this
		// turn (preview line, plus the full resolved body under showFull) —
		// then a clearly separated diagnostics section for the plumbing
		// (model, tokens, system prompt, tool schema, tool name lists).
		for i, nm := range r.newMessages {
			add(fmt.Sprintf("      msg %-9s %-6s %s", nm.Role, humanBytes(int64(nm.Bytes)), nm.Preview))
			if m.showFull && i < len(r.newMessageTexts) {
				if i < len(r.newMessageRendered) && len(r.newMessageRendered[i]) > 0 {
					preBlock("        ", r.newMessageRendered[i])
				} else {
					block("        ", r.newMessageTexts[i])
				}
			}
		}
		label := "changed"
		if r.sysUnchanged {
			label = "unchanged"
		}
		add(diagnosticsMarker)
		add(fmt.Sprintf("      model %s  system prompt %s (%s)  tools=%d  est ~%s",
			r.model, humanBytes(int64(r.sysBytes)), label, r.toolCount, humanTok(r.estTokens)))
		if len(r.toolNames) > 0 {
			add("      tools: " + strings.Join(r.toolNames, ", "))
		}
		if len(r.mcpToolNames) > 0 {
			add("      mcp tools: " + strings.Join(r.mcpToolNames, ", "))
		}
		if m.showFull {
			// The system prompt stays PLAIN deliberately: it is prompt
			// plumbing, not conversational prose — markdown-rendering it
			// would reflow/beautify text whose exact shape matters.
			add("      system prompt:")
			block("        ", r.sysPromptText)
			if r.toolCount > 0 {
				add("      tool schema:")
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
			add(fmt.Sprintf("      assistant %s:", humanBytes(int64(r.textBytes))))
			if len(r.assistantRendered) > 0 {
				preBlock("        ", r.assistantRendered)
			} else {
				block("        ", r.assistantText)
			}
		} else if r.textPreview != "" {
			add(fmt.Sprintf("      assistant %s  %s", humanBytes(int64(r.textBytes)), r.textPreview))
		}
		if len(r.toolCalls) > 0 {
			add("      tool calls: " + strings.Join(r.toolCalls, ", "))
		}
		stop := r.stopReason
		if stop == "" {
			stop = "-"
		}
		add(diagnosticsMarker)
		add(fmt.Sprintf("      status %d  stop=%s", r.status, stop))
		if r.usage != nil {
			add(fmt.Sprintf("      usage  in=%s out=%s total=%s",
				humanCount(r.usage.InputTokens), humanCount(r.usage.OutputTokens), humanCount(r.usage.TotalTokens)))
		} else {
			add("      usage  (not reported)")
		}
	case rowKindTool:
		state := "pending"
		if r.toolDone {
			okLabel := "ok"
			if !r.ok {
				okLabel = "FAIL"
			}
			state = fmt.Sprintf("%s %s %s", okLabel, humanBytes(int64(r.resultBytes)), humanDuration(r.durationMs))
		}
		add(fmt.Sprintf("      tool %s  source=%s  id=%s  %s", r.name, r.source, r.toolID, state))
		if r.argsSummary != "" {
			add("      args: " + r.argsSummary)
		}
		if r.toolDone && r.resultSummary != "" {
			add("      result: " + r.resultSummary)
		}
		if m.showFull {
			// Tool args/results stay PLAIN deliberately: they are
			// shell/JSON payloads, not markdown prose — rendering them
			// would mangle the exact bytes the tool saw.
			add("      args:")
			block("        ", r.argsText)
			if r.toolDone {
				add("      result:")
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
		// Message bodies are natural-language markdown: render the
		// SANITIZED text through glamour here in the Update path (never
		// from View, R1-12). nil entries fall back to plain in detailLines.
		r.newMessageRendered = nil
		if w := m.markdownBodyWidth(); w > 0 {
			rendered := make([][]string, len(texts))
			for i, tx := range texts {
				rendered[i] = renderMarkdownLines(tx, w)
			}
			r.newMessageRendered = rendered
		}
	case rowKindResponse:
		r.assistantText = m.fetchBlobText(r.textHash)
		// The assistant reply is natural-language markdown too — same
		// sanitize-then-glamour order (fetchBlobText already sanitized).
		r.assistantRendered = renderMarkdownLines(r.assistantText, m.markdownBodyWidth())
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
	// The glamour-rendered forms follow the raw text's R3-2b lifecycle:
	// cleared the moment the row is collapsed or showFull goes off.
	r.assistantRendered = nil
	r.newMessageRendered = nil
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

// --- markdown rendering (glamour) for natural-language bodies ---

// markdownBodyWidth is the cell width available to an expanded
// natural-language body: the terminal width minus the timestamp column,
// the cursor gutter, the tree indent, and the 8-space body block prefix.
// The indent is approximated at depth 2 (a deeper spawn-nested row loses a
// couple of cells to clampAnsiLine's guard — acceptable). 0 means "no
// usable width" (unsized — e.g. a headless test before any WindowSizeMsg —
// or too narrow to be worth formatting): callers skip glamour entirely and
// detailLines falls back to the plain line-split path.
func (m Model) markdownBodyWidth() int {
	if m.width <= 0 {
		return 0
	}
	w := m.width - timestampWidth - 2 /* gutter */ - 4 /* indent */ - 8 /* block prefix */
	if w < 20 {
		return 0
	}
	return w
}

// renderMarkdownLines renders ALREADY-SANITIZED natural-language markdown
// through glamour into physical display lines, word-wrapped to width.
//
// SECURITY ORDER (critical): the input MUST already be sanitized
// (sanitizeText — every caller passes text that went through
// fetchBlobText), so no attacker escape sequence ever reaches glamour; the
// ANSI in glamour's OUTPUT is then OURS and trusted, and is deliberately
// NOT re-sanitized (that would strip the formatting this exists for).
// untrusted text -> sanitize -> glamour -> trusted styled lines.
//
// Style: the standard dark style with the document margin and blank-line
// block prefix/suffix zeroed (compact, so the body nests cleanly in the
// tree), degraded to the real terminal's color profile via lipgloss —
// headless (tests, pipes) renders essentially plain. Single newlines are
// preserved so the rendered body keeps the reply's own line structure.
// Returns nil on any failure (or width<=0, or a blank body): callers fall
// back to the plain line-split of the raw sanitized text, so a headless
// test with no WindowSizeMsg behaves exactly as before.
func renderMarkdownLines(md string, width int) []string {
	if width <= 0 || strings.TrimSpace(md) == "" {
		return nil
	}
	sc := styles.DarkStyleConfig
	zero := uint(0)
	sc.Document.Margin = &zero
	sc.Document.BlockPrefix = ""
	sc.Document.BlockSuffix = ""
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(sc),
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(),
		glamour.WithColorProfile(lipgloss.ColorProfile()),
	)
	if err != nil {
		return nil
	}
	out, err := r.Render(md)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	// Trim the padding/blank framing glamour adds: trailing whitespace per
	// line (it right-pads to the wrap width), then leading/trailing blank
	// lines (blank = no printable content once ANSI is ignored —
	// sanitizeText doubles as the ANSI stripper here, on OUR OWN output,
	// purely to measure blankness; the kept lines stay styled).
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " ")
	}
	blank := func(s string) bool { return strings.TrimSpace(sanitizeText(s, false)) == "" }
	for len(lines) > 0 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	return lines
}

// clampAnsiLine is the ANSI-AWARE defensive width clamp for glamour
// pre-rendered lines (bodyLine.pre): normally a no-op — they were wrapped
// to fit — but an over-wide line (deep tree nesting eating more indent
// than markdownBodyWidth budgeted) is truncated by DISPLAY cells without
// ever splitting an escape sequence, unlike the plain truncateLine.
func clampAnsiLine(s string, width int) string {
	if width <= 0 || xansi.StringWidth(s) <= width {
		return s
	}
	return xansi.Truncate(s, width, "…")
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

// humanDuration formats a millisecond duration compactly for inline display
// (a tool row's `→ ok 2.1KB 4.3s`, a response row's model round-trip
// latency): sub-second as e.g. "820ms" (a decimal-seconds value under 1.0
// reads worse than a plain millisecond count), 1s-<1m as e.g. "1.3s", and
// 1m+ as e.g. "1m2s" (no sub-second remainder at that scale — it's noise).
// ms<=0 (unknown/not-yet-elapsed) renders "0ms" — callers that want to omit
// an unknown duration entirely (e.g. renderResponseRow's latency segment)
// check ms>0 themselves before calling this.
func humanDuration(ms int) string {
	if ms <= 0 {
		return "0ms"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) - mins*60
	return fmt.Sprintf("%dm%ds", mins, secs)
}

var (
	headerStyle     = lipgloss.NewStyle().Bold(true)
	footerStyle     = lipgloss.NewStyle().Faint(true)
	filterStyle     = lipgloss.NewStyle().Italic(true)
	selectedStyle   = lipgloss.NewStyle().Reverse(true).Bold(true)
	nodeHeaderStyle = lipgloss.NewStyle().Faint(true).Bold(true)
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
