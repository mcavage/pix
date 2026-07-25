package monitor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Documented defaults (architecture.md Section 3.A). RingSize and BlobBytes
// are applied by NewHub when the caller leaves the field at its zero value.
// Port is intentionally NOT defaulted by NewHub/Start — 0 is a legitimate,
// deliberate value meaning "let the OS pick an ephemeral port" (plain
// net.Listen semantics), which is exactly what hub_test.go needs to run many
// hubs in parallel without colliding on the real port. The CLI (Unit C) is
// responsible for passing DefaultPort as its --port flag default when the
// user doesn't override it.
const (
	DefaultPort      = 11437
	DefaultRingSize  = 2000
	DefaultBlobBytes = 64 << 20 // 64MB

	// DefaultRingBytes bounds the ring's TOTAL cumulative estimated size
	// (see Ring.maxBytes / eventSize), independent of DefaultRingSize's
	// event-count cap (R2-7). Without this, 2000 events at the old 8MB
	// maxIngestLine could retain up to ~16GB before the count cap ever
	// kicks in — an OOM vector. 32MB is comfortably above what 2000
	// normal (field-capped, see maxFieldBytes) events need, while still
	// bounding a worst case flood of maximally-sized events.
	DefaultRingBytes = 32 << 20 // 32MB

	// DefaultBindAddr is the SECURE default listen host: loopback-only. It
	// works for local UAT and, on Docker Desktop for macOS/Windows (where
	// host.docker.internal forwards into the host's loopback interface),
	// also works for watching a real sandbox. Only Linux hosts — where
	// host.docker.internal resolves to the bridge gateway, not loopback —
	// need to opt in to a wider bind (see cmd/pix/monitor.go --bind).
	DefaultBindAddr = "127.0.0.1"
)

// maxBlobPost bounds a single POST /blob request body. This is independent
// of BlobBytes (which bounds total cache size across all stored blobs): a
// single oversized request must not be able to exhaust host memory before
// the accounting/eviction logic in BlobCache.Put ever runs.
const maxBlobPost = 32 << 20 // 32MB

// maxIngestLine bounds a single NDJSON line read from POST /ingest. A
// legitimate event or blob-hash line is a tiny fraction of this; the cap
// exists only to stop a single monstrous line (malicious or buggy) from
// being buffered unbounded in memory. Unlike bufio.Scanner (which stops
// permanently once a token exceeds its buffer), readLineBounded discards an
// oversized line and keeps reading — one bad line must not drop the rest of
// the stream.
//
// R2-7: lowered from 8MB to 1MB. 8MB per line, retained across up to
// DefaultRingSize (2000) ring slots, was the bulk of the ~16GB OOM exposure
// this bounds; 1MB is still far more than any real event needs (individual
// free-form fields are additionally capped at decode time — see
// event.go's maxFieldBytes), and the Ring's own byte budget (DefaultRingBytes)
// is the second, independent backstop.
const maxIngestLine = 1 << 20 // 1MB

// errLineTooLong is returned by readLineBounded when a line exceeds maxLen.
var errLineTooLong = errors.New("monitor: ingest line exceeds max length")

// shutdownGrace bounds how long Start waits for in-flight requests to drain
// after ctx is canceled before forcing the listener closed.
const shutdownGrace = 5 * time.Second

// HubConfig configures a Hub. See the DefaultXxx constants above.
type HubConfig struct {
	Port      int    // default DefaultPort (11437); 0 = OS-assigned ephemeral port
	BindAddr  string // default DefaultBindAddr ("127.0.0.1"); SECURITY: only widen this deliberately, see DefaultBindAddr's doc
	RingSize  int    // default DefaultRingSize (2000)
	RingBytes int    // default DefaultRingBytes (32<<20); total byte budget for the ring, see R2-7
	BlobBytes int    // default DefaultBlobBytes (64<<20)
	Filter    string // sandbox name/id substring filter; "" = all
}

// Hub is the host-side process for `pix monitor`: an HTTP server that
// ingests NDJSON events from the in-VM extension, keeps a bounded ring +
// blob cache, and fans events out to subscribers (the TUI in-process, or
// GET /stream for external/synthetic consumers).
type Hub struct {
	cfg   HubConfig
	ring  *Ring
	blobs *BlobCache

	// mu guards subs/nextSubID together with ring.Add, so a new subscriber's
	// ring-replay + registration is atomic with respect to concurrently
	// published events: no event can be lost (published between replay and
	// registration) or duplicated (delivered via both replay and live fan-out).
	mu        sync.Mutex
	subs      map[int]chan Event
	nextSubID int
	addr      string

	// dropped counts live events actually discarded across ALL subscribers
	// due to backpressure (R2-5): publish's drop-oldest path evicts the
	// oldest queued event to make room for the newest one, and every such
	// eviction is a real, permanent loss for that subscriber — counted here
	// as a single global atomic (not per-subscriber; good enough to answer
	// "is any subscriber falling behind" without extra bookkeeping per sub).
	dropped uint64
}

// NewHub constructs a Hub. RingSize and BlobBytes fall back to their
// documented defaults when left at zero; see the Port note on HubConfig.
func NewHub(cfg HubConfig) *Hub {
	if cfg.BindAddr == "" {
		cfg.BindAddr = DefaultBindAddr
	}
	if cfg.RingSize <= 0 {
		cfg.RingSize = DefaultRingSize
	}
	if cfg.RingBytes <= 0 {
		cfg.RingBytes = DefaultRingBytes
	}
	if cfg.BlobBytes <= 0 {
		cfg.BlobBytes = DefaultBlobBytes
	}
	return &Hub{
		cfg:   cfg,
		ring:  NewRingBytes(cfg.RingSize, cfg.RingBytes),
		blobs: NewBlobCache(cfg.BlobBytes),
		subs:  make(map[int]chan Event),
	}
}

// Start binds HubConfig.BindAddr:Port (loopback-only by default — see
// DefaultBindAddr) and serves until ctx is done, then shuts the server down
// cleanly (releasing the port) and returns nil. It returns a non-nil error
// only if the initial bind fails or the server exits with an unexpected
// error before ctx is canceled.
func (h *Hub) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest", h.handleIngest)
	mux.HandleFunc("POST /blob", h.handleBlobPost)
	mux.HandleFunc("GET /blob/{hash}", h.handleBlobGet)
	mux.HandleFunc("GET /stream", h.handleStream)
	mux.HandleFunc("GET /healthz", h.handleHealthz)

	bindAddr := h.cfg.BindAddr
	if bindAddr == "" {
		bindAddr = DefaultBindAddr
	}
	addr := net.JoinHostPort(bindAddr, strconv.Itoa(h.cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("monitor: listen %s: %w", addr, err)
	}

	h.mu.Lock()
	h.addr = dialableAddr(ln.Addr().String())
	h.mu.Unlock()

	srv := &http.Server{
		Handler:     mux,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Graceful drain didn't finish within the grace period (e.g. a
			// client-side connection accepted but never used to send a
			// request — Shutdown only closes IDLE conns, so a stray one in
			// that state blocks it for the whole window). Force-close
			// everything so Start still returns promptly and the port is
			// unambiguously released.
			_ = srv.Close()
		}
		<-serveErr
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// Addr returns the actual listen address (host:port), including the
// OS-assigned port when Port was 0. Empty until Start has bound the
// listener.
func (h *Hub) Addr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addr
}

// subscriberLiveHeadroom is added on top of the ring-replay length when
// sizing a new subscriber's channel buffer (see Subscribe). It's the room
// available for LIVE events published after the subscriber registers but
// before it (or the TUI) drains any of the replay — the same budget the
// buffer used to be hardcoded to before R1-9.
const subscriberLiveHeadroom = 256

// Subscribe returns a channel that first replays the ENTIRE current ring
// (oldest->newest, lossless — see R1-9) and then streams live events as
// they're published. The channel is sized to len(ring snapshot) +
// subscriberLiveHeadroom specifically so the replay loop below can never
// hit a full buffer: every replayed event is guaranteed delivered, not just
// best-effort. Only LIVE events published after Subscribe returns can ever
// be dropped under sustained backpressure (see publish's drop-oldest
// handling). The returned func unsubscribes and closes the channel; callers
// must call it to avoid leaking the subscription.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	h.mu.Lock()
	snap := h.ring.Snapshot()
	ch := make(chan Event, len(snap)+subscriberLiveHeadroom)
	for _, e := range snap {
		ch <- e // never blocks: buffer is sized to fit the whole replay
	}
	id := h.nextSubID
	h.nextSubID++
	h.subs[id] = ch
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			if c, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(c)
			}
			h.mu.Unlock()
		})
	}
	return ch, unsubscribe
}

// Blob looks up a cached blob body in-process (no HTTP round-trip) — this is
// the seam the TUI uses.
func (h *Hub) Blob(hash string) (Blob, bool) {
	return h.blobs.Get(hash)
}

// Events returns a snapshot of the current ring (oldest->newest), e.g. for
// tests or an initial paint.
func (h *Hub) Events() []Event {
	return h.ring.Snapshot()
}

// publish applies the sandboxId filter, then atomically adds e to the ring
// and fans it out to every current subscriber.
func (h *Hub) publish(e Event) {
	if !h.matchesFilter(e) {
		return
	}
	h.mu.Lock()
	if !h.ring.Add(e) {
		// R4-3: Ring.Add returns false when e was rejected outright for
		// exceeding the ring's byte budget alone (R3-3) — it was NEVER
		// stored. Fanning it out to subscribers anyway would let an
		// oversized event bypass exactly the byte protection this exists
		// to enforce: subscriber channel buffers hold arbitrary Events with
		// no size bound of their own. Ring.Dropped() is already the single
		// source of truth for this count (Ring.dropped, incremented inside
		// Add itself) — do NOT also bump h.dropped here, which counts a
		// different thing (live subscriber backpressure evictions, R2-5);
		// incrementing both would double-count the same drop under two
		// unrelated metrics.
		h.mu.Unlock()
		return
	}
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default:
			// Slow subscriber: its buffer is full. Real drop-OLDEST (R1-9),
			// matching the VM-side extension's own drop-oldest backpressure
			// rule (architecture.md risk 3): evict the head of the queue (a
			// buffered channel is FIFO, so a non-blocking receive removes the
			// oldest queued event) to make room, then deliver e. This keeps
			// the newest state — critical for e.g. tool_end, where losing the
			// newest event instead would leave a tool looking permanently
			// pending to the subscriber.
			select {
			case <-ch:
				// The evicted event is really gone now (R2-5): count it. This
				// is the normal drop-oldest path, not a rare race — a slow
				// subscriber backing up hits this on every subsequent publish.
				atomic.AddUint64(&h.dropped, 1)
			default:
				// Buffer was already empty by the time we got here — nothing
				// to evict, so e below lands in a genuinely free slot.
			}
			select {
			case ch <- e:
			default:
				// Another goroutine (the subscriber itself draining) raced us
				// for the slot we just freed. Extremely unlikely (mu is held
				// for the whole publish, so only the subscriber's own receive
				// side can race here), but e is now also dropped — count it.
				atomic.AddUint64(&h.dropped, 1)
			}
		}
	}
	h.mu.Unlock()
}

// Dropped reports the number of live events actually discarded across all
// subscribers by publish's drop-oldest backpressure handling (R2-5) — a
// global count, not per-subscriber. A non-zero value in production means at
// least one subscriber's consumer is falling behind badly enough that its
// channel buffer filled and the oldest queued event(s) had to be evicted to
// make room for newer ones.
func (h *Hub) Dropped() uint64 {
	return atomic.LoadUint64(&h.dropped)
}

func (h *Hub) matchesFilter(e Event) bool {
	if h.cfg.Filter == "" {
		return true
	}
	return strings.Contains(e.Envelope().SandboxID, h.cfg.Filter)
}

// handleIngest reads NDJSON, one event per line, and publishes each. Lines
// that fail to decode are skipped (logged) and never fatal — a single bad
// line must not drop the rest of the stream nor fail the request. Each line
// is bounded to maxIngestLine via readLineBounded: an oversized line is
// skipped (not buffered unbounded), and — unlike bufio.Scanner, which stops
// permanently on ErrTooLong — reading continues for subsequent lines.
func (h *Hub) handleIngest(w http.ResponseWriter, r *http.Request) {
	reader := bufio.NewReaderSize(r.Body, 64*1024)
	for {
		line, err := readLineBounded(reader, maxIngestLine)
		if err == errLineTooLong {
			log.Printf("monitor: skip oversized /ingest line (> %d bytes)", maxIngestLine)
			continue
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("monitor: /ingest read error: %v", err)
			}
			break
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		ev, decErr := Decode(line)
		if decErr != nil {
			log.Printf("monitor: skip unparseable /ingest line: %v", decErr)
			continue
		}
		h.publish(ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// readLineBounded reads one '\n'-delimited line (delimiter stripped) from r,
// up to maxLen bytes. A line longer than maxLen is discarded (its bytes are
// never accumulated in memory beyond maxLen) up through its terminating
// newline, and readLineBounded returns errLineTooLong for that line only —
// the caller can keep calling readLineBounded to process the lines after it
// in the same stream. A final line with no trailing newline before EOF is
// still returned (with a nil error); EOF with no pending data returns
// io.EOF.
func readLineBounded(r *bufio.Reader, maxLen int) ([]byte, error) {
	var buf []byte
	overflow := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !overflow {
			if len(buf)+len(chunk) > maxLen {
				overflow = true
				buf = nil
			} else if len(chunk) > 0 {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			// ReadSlice found the delimiter; chunk ends with it.
			if overflow {
				return nil, errLineTooLong
			}
			return bytes.TrimRight(buf, "\r\n"), nil
		case err == bufio.ErrBufferFull:
			// No delimiter within the buffer yet; ReadSlice again to keep
			// consuming (and, if overflow, discarding) the rest of the line.
			continue
		case len(buf) > 0 && !overflow:
			// EOF (or another read error) after partial data with no trailing
			// newline: return what we have, matching bufio.Scanner's final-
			// line-without-newline behavior.
			return bytes.TrimRight(buf, "\r\n"), nil
		case overflow:
			return nil, errLineTooLong
		default:
			return nil, err
		}
	}
}

// handleBlobPost stores a single JSON Blob body sent by the extension (or a
// synthetic UAT client). The request body is capped at maxBlobPost so an
// oversized (or unbounded) body can't be used to exhaust host memory before
// BlobCache.Put's own accounting ever runs.
func (h *Hub) handleBlobPost(w http.ResponseWriter, r *http.Request) {
	var bl Blob
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBlobPost))
	if err := dec.Decode(&bl); err != nil {
		http.Error(w, "monitor: bad blob json", http.StatusBadRequest)
		return
	}
	// R1-11: BlobCache.Put verifies bl.Hash == sha256(bl.Text) itself and
	// refuses to store (or overwrite) anything that doesn't match. Surface
	// that as 400 rather than a silent 200 that lied about storing it.
	if ok := h.blobs.Put(bl); !ok {
		http.Error(w, "monitor: blob hash does not match sha256(text)", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleBlobGet serves a cached blob by hash: 200 + JSON body, or 404.
func (h *Hub) handleBlobGet(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		http.NotFound(w, r)
		return
	}
	bl, ok := h.blobs.Get(hash)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, bl)
}

// handleStream serves GET /stream as SSE: replay the ring, then live events,
// each framed as "data: <json>\n\n".
func (h *Hub) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "monitor: streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsubscribe := h.Subscribe()
	defer unsubscribe()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, err := Encode(e)
			if err != nil {
				log.Printf("monitor: /stream encode error: %v", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleHealthz reports liveness only. It intentionally does NOT report the
// sandboxIds observed (that was an info leak on an unauthenticated endpoint
// — the seen/markSeen machinery that used to track them for /healthz was
// removed entirely, see R1-10: nothing in-process ever read it, so it was
// just an unbounded map an unauthenticated /ingest client could grow
// forever by sending unique sandboxIds); callers that need the observed
// sandbox set should use Hub.Events() in-process instead.
func (h *Hub) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// dialableAddr rewrites a listener's advertised wildcard host (0.0.0.0 or ::,
// meaning "all interfaces") into a loopback address a client on the same
// host can actually dial. The default bind is loopback-only (see
// DefaultBindAddr), but an operator can still opt in to a wildcard bind
// (--bind 0.0.0.0, e.g. on Linux where host.docker.internal maps to the
// bridge gateway rather than loopback), and Addr() exists so tests (Port:0)
// and local synthetic UAT can connect back to the hub on the same machine —
// connecting to the literal wildcard address is not portable (observed to
// fail here for the IPv6 form "[::]"). The real bound port is preserved;
// only the host part changes.
func dialableAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "0.0.0.0", "::", "":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
