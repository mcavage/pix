package monitor

// ingest.go is the loopback ingest server: NDJSON events and blob bodies
// from the in-VM tap, persisted through Store. No /stream, no subscribe, no
// live-view state — a reader tails the files (follow.go), decoupled through
// the filesystem. The loopback bind + :11437 default are the ones the
// deleted Hub used, so pi-kit's allowlist entry still holds now that
// `pix-host serve` owns this. See docs/design/monitor.md.

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
	"time"
)

const (
	DefaultPort     = 11437
	DefaultBindAddr = "127.0.0.1"

	// maxIngestLine bounds one NDJSON line. An oversized line is discarded
	// (never buffered) and reading CONTINUES with the next — unlike
	// bufio.Scanner, which stops permanently once a token overruns.
	maxIngestLine = 1 << 20
)

// errLineTooLong is returned by readLine for a line over maxIngestLine.
var errLineTooLong = errors.New("monitor: ingest line exceeds max length")

// IngestConfig configures an IngestServer. There is no write-side stream
// filter: the writer persists everything the tap sends and the READER picks
// (`pix monitor [name]`), so a capture can never be missing what you did not
// know to ask for.
type IngestConfig struct {
	Port     int    // 0 = OS-assigned ephemeral port
	BindAddr string // default DefaultBindAddr
	Store    *Store
}

// IngestServer receives events over loopback HTTP and persists them.
type IngestServer struct {
	cfg IngestConfig
	ln  net.Listener
}

// NewIngestServer BINDS the listener immediately, so a port conflict is
// reported here instead of asynchronously from Serve (which forced callers
// to poll "did it bind yet"). Store is required: nil would silently discard
// every event.
func NewIngestServer(cfg IngestConfig) (*IngestServer, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("monitor: NewIngestServer: Store is required")
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = DefaultBindAddr
	}
	addr := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("monitor: listen %s: %w", addr, err)
	}
	return &IngestServer{cfg: cfg, ln: ln}, nil
}

// Addr returns the bound host:port, with a wildcard host rewritten to
// loopback so it is dialable as printed.
func (s *IngestServer) Addr() string {
	host, port, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		return s.ln.Addr().String()
	}
	switch host {
	case "0.0.0.0", "::", "":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// Serve serves until ctx is done, then drains in-flight requests and
// releases the port. It returns nil on a clean shutdown.
func (s *IngestServer) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest", s.handleIngest)
	mux.HandleFunc("POST /blob", s.handleBlob)
	srv := &http.Server{
		Handler:     mux,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(s.ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

// handleIngest reads NDJSON, one event per line, and persists each. A line
// that fails to decode, or carries ids the store refuses, is logged and
// skipped: one bad line must not drop the rest nor fail the request.
func (s *IngestServer) handleIngest(w http.ResponseWriter, r *http.Request) {
	reader := bufio.NewReaderSize(r.Body, 64*1024)
	for {
		line, err := readLine(reader, maxIngestLine)
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
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ev, err := Decode(line)
		if err != nil {
			log.Printf("monitor: skip unparseable /ingest line: %v", err)
			continue
		}
		if err := s.cfg.Store.Append(ev); err != nil {
			log.Printf("monitor: store append: %v", err)
		}
	}
	writeOK(w)
}

// handleBlob stores one full payload body, capping the request body
// independently of the store so an oversized one cannot exhaust memory
// first.
func (s *IngestServer) handleBlob(w http.ResponseWriter, r *http.Request) {
	var bl struct {
		Hash string `json:"hash"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&bl); err != nil {
		http.Error(w, "monitor: bad blob json", http.StatusBadRequest)
		return
	}
	ok, err := s.cfg.Store.AppendBlob(bl.Hash, bl.Text)
	switch {
	case err != nil:
		log.Printf("monitor: blob store: %v", err)
		http.Error(w, "monitor: internal error", http.StatusInternalServerError)
	case !ok:
		http.Error(w, "monitor: blob hash does not match sha256(text)", http.StatusBadRequest)
	default:
		writeOK(w)
	}
}

// readLine reads one '\n'-delimited line (delimiter stripped) up to maxLen
// bytes; a longer one is discarded through its newline and reported as
// errLineTooLong for that line only. A final unterminated line is still
// returned; EOF with nothing pending returns io.EOF.
func readLine(r *bufio.Reader, maxLen int) ([]byte, error) {
	var buf []byte
	overflow := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !overflow {
			if len(buf)+len(chunk) > maxLen {
				overflow, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case overflow:
			return nil, errLineTooLong
		case err == nil, len(buf) > 0:
			return bytes.TrimRight(buf, "\r\n"), nil
		default:
			return nil, err
		}
	}
}

// writeOK is the only success body either endpoint returns.
func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}
