package monitor

// ingest.go is the loopback ingest constructor: an HTTP server that
// receives NDJSON events + blob bodies from the in-VM extension
// (extensions/monitor.ts) and persists them via Store/BlobStore. It
// replaces the deleted Hub's in-memory ring + SSE fan-out — there is no
// GET /stream, no in-process Subscribe, no rich live-view state here at
// all; a reader (cmd/pix/monitor.go's concise reader) discovers new events
// by polling Store.List/Tail, fully decoupled through the filesystem (see
// store.go's doc comment for why).
//
// THIS CONSTRUCTOR IS DELIBERATELY NOT WIRED INTO `pix-host serve` (see
// docs/design/monitor.md and AGENTS.md's serve-lifecycle invariants): it
// keeps the SAME bind/port defaults (DefaultPort 11437, loopback-only) the
// deleted Hub used, specifically so a later story can move it under serve
// without changing them out from under a monitor-enabled sandbox image
// (pi-kit/spec.yaml's host.docker.internal:11437 allowlist entry). For now
// `pix monitor` (cmd/pix/monitor.go) is the only caller.
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
	"time"
)

const (
	DefaultPort     = 11437
	DefaultBindAddr = "127.0.0.1"

	// maxIngestLine bounds a single NDJSON line read from POST /ingest —
	// a legitimate event is a tiny fraction of this; it exists only to
	// stop one monstrous line (malicious or buggy) from being buffered
	// unbounded in memory. An oversized line is discarded (not buffered)
	// and reading continues for subsequent lines — unlike bufio.Scanner,
	// which stops permanently once a token exceeds its buffer.
	maxIngestLine = 1 << 20 // 1MB

	// maxBlobPost bounds a single POST /blob request body, independent of
	// BlobStoreConfig.MaxBytes (which bounds total stored blob bytes): a
	// single oversized request must not exhaust memory before the
	// store's own accounting ever runs.
	maxBlobPost = 32 << 20 // 32MB

	// shutdownGrace bounds how long Start waits for in-flight requests to
	// drain after ctx is canceled before forcing the listener closed.
	shutdownGrace = 5 * time.Second
)

// errLineTooLong is returned by readLineBounded when a line exceeds maxLen.
var errLineTooLong = errors.New("monitor: ingest line exceeds max length")

// IngestConfig configures an IngestServer.
type IngestConfig struct {
	Port     int    // default DefaultPort (11437); 0 = OS-assigned ephemeral port
	BindAddr string // default DefaultBindAddr ("127.0.0.1")
	Store    *Store
	Blobs    *BlobStore
	// Filter, when non-empty, is a substring match against an incoming
	// event's sandboxId OR sessionId: anything that doesn't match is
	// acknowledged (200, so the extension's retry logic never spins) but
	// not persisted. "" persists everything.
	Filter string
}

// IngestServer is the host-side process that receives NDJSON events + blob
// bodies over loopback HTTP and persists them.
type IngestServer struct {
	cfg IngestConfig

	mu   sync.Mutex
	addr string
}

// NewIngestServer constructs an IngestServer. cfg.Store and cfg.Blobs are
// required (a nil store would silently discard every event, which is worse
// than failing fast at construction).
func NewIngestServer(cfg IngestConfig) (*IngestServer, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("monitor: NewIngestServer: Store is required")
	}
	if cfg.Blobs == nil {
		return nil, fmt.Errorf("monitor: NewIngestServer: Blobs is required")
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = DefaultBindAddr
	}
	return &IngestServer{cfg: cfg}, nil
}

// Addr returns the actual listen address (host:port), including the
// OS-assigned port when Port was 0. Empty until Start has bound the
// listener.
func (s *IngestServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Start binds cfg.BindAddr:cfg.Port and serves until ctx is done, then
// shuts the server down cleanly (releasing the port) and returns nil. It
// returns a non-nil error only if the initial bind fails or the server
// exits with an unexpected error before ctx is canceled.
func (s *IngestServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest", s.handleIngest)
	mux.HandleFunc("POST /blob", s.handleBlobPost)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	addr := net.JoinHostPort(s.cfg.BindAddr, strconv.Itoa(s.cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("monitor: listen %s: %w", addr, err)
	}

	s.mu.Lock()
	s.addr = dialableAddr(ln.Addr().String())
	s.mu.Unlock()

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

func (s *IngestServer) matchesFilter(env Envelope) bool {
	if s.cfg.Filter == "" {
		return true
	}
	return strings.Contains(env.SandboxID, s.cfg.Filter) || strings.Contains(env.SessionID, s.cfg.Filter)
}

// handleIngest reads NDJSON, one event per line, and persists each via
// Store.Append. Lines that fail to decode are skipped (logged) and never
// fatal — one bad line must not drop the rest of the stream nor fail the
// request.
func (s *IngestServer) handleIngest(w http.ResponseWriter, r *http.Request) {
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
		if !s.matchesFilter(ev.Envelope()) {
			continue
		}
		if err := s.cfg.Store.Append(ev); err != nil {
			log.Printf("monitor: store append: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// readLineBounded reads one '\n'-delimited line (delimiter stripped) from r,
// up to maxLen bytes. A line longer than maxLen is discarded up through its
// terminating newline, returning errLineTooLong for that line only — the
// caller can keep calling readLineBounded for subsequent lines in the same
// stream. A final line with no trailing newline before EOF is still
// returned (nil error); EOF with no pending data returns io.EOF.
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
			if overflow {
				return nil, errLineTooLong
			}
			return bytes.TrimRight(buf, "\r\n"), nil
		case err == bufio.ErrBufferFull:
			continue
		case len(buf) > 0 && !overflow:
			return bytes.TrimRight(buf, "\r\n"), nil
		case overflow:
			return nil, errLineTooLong
		default:
			return nil, err
		}
	}
}

// handleBlobPost stores a single JSON Blob body sent by the extension. The
// request body is capped at maxBlobPost so an oversized body can't exhaust
// host memory before BlobStore.Put's own accounting ever runs.
func (s *IngestServer) handleBlobPost(w http.ResponseWriter, r *http.Request) {
	var bl Blob
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBlobPost))
	if err := dec.Decode(&bl); err != nil {
		http.Error(w, "monitor: bad blob json", http.StatusBadRequest)
		return
	}
	ok, err := s.cfg.Blobs.Put(bl)
	if err != nil {
		log.Printf("monitor: blob store: %v", err)
		http.Error(w, "monitor: internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "monitor: blob hash does not match sha256(text)", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleHealthz reports liveness only.
func (s *IngestServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// dialableAddr rewrites a listener's advertised wildcard host (0.0.0.0 or
// ::, meaning "all interfaces") into a loopback address a client on the
// same host can actually dial. The real bound port is preserved; only the
// host part changes.
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
