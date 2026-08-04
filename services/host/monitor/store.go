package monitor

// store.go is the on-disk event domain: append/decode/tail/list. It
// replaces the in-memory-only Ring (deleted with the bubbletea TUI it fed)
// with a bounded, per-(sandboxId,sessionId) NDJSON file store — every event
// is redacted (see redact.go) before it ever touches disk, and every
// stream is bounded independently by both event count and byte size so a
// single noisy sandbox can't grow the store without limit. It has no
// network code and no in-process fan-out; ingest.go is the (separate,
// loopback-only) HTTP layer that calls Append, and cmd/pix/monitor.go's
// concise reader is the (separate) poller that calls Tail/List. Decoupling
// them through the filesystem, rather than a shared in-process channel, is
// what lets `--path DIR` review an already-captured directory with no
// listener running at all (see monitor_test.go / cmd/pix's reader).
import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultMaxEventsPerStream / DefaultMaxBytesPerStream bound ONE
	// stream's events.ndjson file, enforced by Append's trim pass. A
	// single event is capped independently at maxIngestLine by the
	// ingest server before it ever reaches Append, so these bounds are
	// about cumulative retained size across many events, the same
	// concern the deleted Ring's byte budget addressed.
	DefaultMaxEventsPerStream = 4000
	DefaultMaxBytesPerStream  = 8 << 20 // 8MB per stream

	// DefaultMaxStreams bounds the NUMBER of distinct (sandboxId,
	// sessionId) streams the store retains at once: without this, a
	// churn of short-lived sessions (or a hostile ingest client sending
	// a fresh sessionId per event) would grow the store directory
	// without limit even though each individual stream stays small.
	// Appending to a stream beyond this cap evicts the oldest stream
	// (by its file's mtime) entirely.
	DefaultMaxStreams = 200

	streamEventsFile = "events.ndjson"
)

// StoreConfig configures a Store. Zero-valued Max* fields fall back to
// their DefaultXxx constant (see NewStore); Root has no default and is
// required.
type StoreConfig struct {
	Root               string
	MaxEventsPerStream int
	MaxBytesPerStream  int
	MaxStreams         int
}

// Store is the bounded, file-backed event domain. Safe for concurrent use;
// Append serializes writes with an internal mutex (a debug wiretap's
// ingest rate does not need finer-grained locking than that).
type Store struct {
	cfg StoreConfig
	mu  sync.Mutex
}

// NewStore constructs a Store rooted at cfg.Root, creating it (0700) if
// absent. Root is required (a zero-value StoreConfig is refused rather
// than silently writing into the process's current directory).
func NewStore(cfg StoreConfig) (*Store, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("monitor: NewStore: Root is required")
	}
	if cfg.MaxEventsPerStream <= 0 {
		cfg.MaxEventsPerStream = DefaultMaxEventsPerStream
	}
	if cfg.MaxBytesPerStream <= 0 {
		cfg.MaxBytesPerStream = DefaultMaxBytesPerStream
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = DefaultMaxStreams
	}
	if err := ensureDir0700(cfg.Root); err != nil {
		return nil, err
	}
	return &Store{cfg: cfg}, nil
}

// Root returns the store's root directory.
func (s *Store) Root() string { return s.cfg.Root }

// streamDir returns (creating it, 0700, if absent) the directory for one
// (sandboxID, sessionID) stream. It also enforces DefaultMaxStreams: if
// creating a NEW stream would exceed the cap, the oldest existing stream
// (by its events file's mtime) is removed first.
func (s *Store) streamDir(sandboxID, sessionID string) (string, error) {
	dir := filepath.Join(s.cfg.Root, streamDirName(sandboxID, sessionID))
	if _, err := os.Stat(dir); err == nil {
		return dir, ensureDir0700(dir) // existing stream: just make sure perms still hold
	}
	if err := s.evictOldestStreamIfAtCapacityLocked(); err != nil {
		return "", err
	}
	if err := ensureDir0700(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// evictOldestStreamIfAtCapacityLocked removes the least-recently-appended
// stream directory when the store already holds cfg.MaxStreams streams —
// called only from streamDir, which only reaches it right before creating a
// genuinely NEW stream (so it never evicts the stream about to be (re)used).
func (s *Store) evictOldestStreamIfAtCapacityLocked() error {
	metas, err := s.List()
	if err != nil {
		return err
	}
	if len(metas) < s.cfg.MaxStreams {
		return nil
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].ModTime.Before(metas[j].ModTime) })
	return os.RemoveAll(metas[0].Dir)
}

// Append encodes e (after Redact — see redact.go) and appends it as one
// NDJSON line to its (sandboxId, sessionId) stream file, then trims that
// file back within budget if the append pushed it over either bound.
func (s *Store) Append(e Event) error {
	env := e.Envelope()
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.streamDir(env.SandboxID, env.SessionID)
	if err != nil {
		return err
	}
	line, err := Encode(Redact(e))
	if err != nil {
		return err
	}
	path := filepath.Join(dir, streamEventsFile)
	f, err := openAppend0600(path)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("monitor: append %s: %w", path, werr)
	}
	if cerr != nil {
		return fmt.Errorf("monitor: close %s: %w", path, cerr)
	}
	return s.trimIfOverBudget(path)
}

// trimIfOverBudget rewrites path to keep only its newest lines when it
// exceeds either DefaultMaxBytesPerStream or DefaultMaxEventsPerStream — the
// per-stream analogue of the deleted Ring's drop-oldest eviction, just
// applied to a file instead of an in-memory slice. It reads the whole file
// (acceptable: bounded by MaxBytesPerStream itself, a few MB at most) and
// rewrites it atomically via writeFileAtomic0600, so a concurrent Tail/List
// never observes a partially-trimmed file.
func (s *Store) trimIfOverBudget(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("monitor: stat %s: %w", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("monitor: read %s: %w", path, err)
	}
	lines := splitNDJSONLines(raw)
	if len(lines) <= s.cfg.MaxEventsPerStream && fi.Size() <= int64(s.cfg.MaxBytesPerStream) {
		return nil
	}
	// Drop oldest lines until both bounds are satisfied.
	start := 0
	if len(lines) > s.cfg.MaxEventsPerStream {
		start = len(lines) - s.cfg.MaxEventsPerStream
	}
	kept := lines[start:]
	total := 0
	for _, l := range kept {
		total += len(l) + 1
	}
	for total > s.cfg.MaxBytesPerStream && len(kept) > 1 {
		total -= len(kept[0]) + 1
		kept = kept[1:]
	}
	var buf bytes.Buffer
	for _, l := range kept {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	return writeFileAtomic0600(path, buf.Bytes())
}

// splitNDJSONLines splits raw on '\n', dropping the trailing empty element a
// well-formed (every line newline-terminated) file produces, and any blank
// line.
func splitNDJSONLines(raw []byte) [][]byte {
	var lines [][]byte
	for _, l := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

// Tail returns the newest n decoded events (oldest-first) for one
// (sandboxID, sessionID) stream. n <= 0 returns every retained event. A
// stream that doesn't exist returns an empty slice, not an error (an
// unstarted/never-seen stream is a normal state for a reader, not a
// failure). A line that fails to decode is skipped rather than failing the
// whole Tail — matches the ingest server's existing "one bad line must not
// drop the rest of the stream" rule.
func (s *Store) Tail(sandboxID, sessionID string, n int) ([]Event, error) {
	dir := filepath.Join(s.cfg.Root, streamDirName(sandboxID, sessionID))
	path := filepath.Join(dir, streamEventsFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("monitor: read %s: %w", path, err)
	}
	lines := splitNDJSONLines(raw)
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	events := make([]Event, 0, len(lines))
	for _, l := range lines {
		ev, err := Decode(l)
		if err != nil {
			continue
		}
		events = append(events, ev)
	}
	return events, nil
}

// StreamMeta describes one retained stream, for List's callers (the
// concise reader enumerates streams this way to know what to Tail).
type StreamMeta struct {
	SandboxID string
	SessionID string
	Dir       string
	Bytes     int64
	ModTime   time.Time
}

// List enumerates every retained stream under the store's root. SandboxID
// and SessionID are recovered from the FIRST event actually stored in each
// stream's file (every event in one stream carries the same pair, by
// construction of streamDirName) rather than from a separate metadata
// sidecar file — one less thing to keep in sync, and the file is always
// there once a stream has at least one event (List skips an empty/
// unreadable stream directory rather than failing the whole listing, since
// a stream directory only ever exists because Append just created it and
// is about to write its first line — a race, not corruption).
func (s *Store) List() ([]StreamMeta, error) {
	entries, err := os.ReadDir(s.cfg.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("monitor: read dir %s: %w", s.cfg.Root, err)
	}
	var metas []StreamMeta
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		dir := filepath.Join(s.cfg.Root, ent.Name())
		path := filepath.Join(dir, streamEventsFile)
		fi, err := os.Stat(path)
		if err != nil {
			continue // no events file yet (mid-creation race) or removed concurrently
		}
		meta := StreamMeta{Dir: dir, Bytes: fi.Size(), ModTime: fi.ModTime()}
		if first, err := firstLine(path); err == nil {
			if ev, err := Decode(first); err == nil {
				env := ev.Envelope()
				meta.SandboxID, meta.SessionID = env.SandboxID, env.SessionID
			}
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// firstLine reads only the first '\n'-delimited line of path, without
// reading the whole (possibly several-MB) file.
func firstLine(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	return bytes.TrimRight(line, "\r\n"), nil
}
