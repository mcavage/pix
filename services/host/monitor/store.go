package monitor

// store.go is the whole on-disk domain: one bounded, redacted NDJSON file
// per (sandboxId, sessionId) stream, plus the filesystem safety layer every
// write goes through (0700 dirs, 0600 files, no symlink followed, no
// wire-supplied string used as a path component unless validID accepts it).
// See docs/design/monitor.md.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// maxStreams bounds the NUMBER of retained streams, so a churn of
	// short-lived sessions cannot grow the root forever; one past the cap
	// evicts the oldest by mtime.
	maxStreams = 200
	// idSep is outside validID's charset, so splitting a stream directory
	// name back into its two ids is unambiguous.
	idSep      = "="
	eventsFile = "events.ndjson"
	// unattributed replaces an EMPTY id (the tap sends sandboxId "" when
	// SANDBOX_VM_ID is unset). A fixed constant, not a transform of input.
	unattributed = "unattributed"
)

// StoreConfig configures a Store. Root is required; the bounds default to
// 4000 events / 8MB per stream (one event is already capped at
// maxIngestLine before it gets here).
type StoreConfig struct {
	Root      string
	MaxEvents int
	MaxBytes  int
}

// Store is the bounded, file-backed event domain. Safe for concurrent use:
// writes serialize on one mutex, ample for a debug wiretap.
type Store struct {
	cfg StoreConfig
	mu  sync.Mutex
}

// NewStore roots a Store at cfg.Root, creating it (0700) if absent. Root is
// never defaulted, so a zero-value config cannot write into the process's
// working directory.
func NewStore(cfg StoreConfig) (*Store, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("monitor: NewStore: Root is required")
	}
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = 4000
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 8 << 20
	}
	if err := ensureDir0700(cfg.Root); err != nil {
		return nil, err
	}
	return &Store{cfg: cfg}, nil
}

// validID reports whether an id is safe verbatim in a directory name:
// 1..96 bytes, leading alphanumeric (no ".", "..", or dotfile), thereafter
// only [A-Za-z0-9._-] (no separator, NUL, control byte, or idSep). Strict
// allowlist, no repair: slugifying bad input would silently accept hostile
// ids and collapse distinct ones onto one directory.
func validID(id string) bool {
	if len(id) == 0 || len(id) > 96 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// streamPath maps one (sandboxID, sessionID) pair to its directory and
// events file, erroring if either id is invalid. The ONLY place wire input
// becomes a path component: writer and reader both go through it.
func (s *Store) streamPath(sandboxID, sessionID string) (dir, file string, err error) {
	if sandboxID == "" {
		sandboxID = unattributed
	}
	if sessionID == "" {
		sessionID = unattributed
	}
	if !validID(sandboxID) || !validID(sessionID) {
		return "", "", fmt.Errorf("monitor: refusing invalid stream id (sandbox %q, session %q)", sandboxID, sessionID)
	}
	dir = filepath.Join(s.cfg.Root, sandboxID+idSep+sessionID)
	return dir, filepath.Join(dir, eventsFile), nil
}

// Append redacts e and appends it as one line to its stream file.
func (s *Store) Append(e Event) error {
	env := e.Envelope()
	dir, file, err := s.streamPath(env.SandboxID, env.SessionID)
	if err != nil {
		return err
	}
	line, err := Encode(redact(e))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(dir); err != nil {
		if err := s.evictOldestStreamIfFull(); err != nil {
			return err
		}
	}
	if err := ensureDir0700(dir); err != nil {
		return err
	}
	return s.appendLine(file, line)
}

// storedBlob is one full payload body as persisted: Bytes is always
// len(Text), and Redacted=false implies sha256(Text) == Hash. Not a separate
// content-addressed subsystem — one more bounded NDJSON file under the same
// root, trimmed by the same pass.
type storedBlob struct {
	Hash     string `json:"hash"`
	Bytes    int    `json:"bytes"`
	Text     string `json:"text"`
	Redacted bool   `json:"redacted,omitempty"`
}

// AppendBlob verifies the client-asserted hash against sha256(text) (on a
// mismatch nothing is written and ok is false) and appends the REDACTED
// text. Redaction wins over content-addressing purity — this is raw tool
// output, the highest-risk text in the pipeline — so the record states
// whether its bytes are still the preimage of Hash.
func (s *Store) AppendBlob(hash, text string) (bool, error) {
	sum := sha256.Sum256([]byte(text))
	if hash == "" || hash != hex.EncodeToString(sum[:]) {
		return false, nil
	}
	scrubbed := redactText(text)
	line, err := json.Marshal(storedBlob{
		Hash: hash, Bytes: len(scrubbed), Text: scrubbed, Redacted: scrubbed != text,
	})
	if err != nil {
		return false, fmt.Errorf("monitor: encode blob %s: %w", hash, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return true, s.appendLine(filepath.Join(s.cfg.Root, "blobs.ndjson"), line)
}

// appendLine appends one line to path and trims it. Callers hold s.mu.
func (s *Store) appendLine(path string, line []byte) error {
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
	return s.trim(path)
}

// trim rewrites path with only its newest lines once it exceeds either
// bound: drop-oldest applied to a file, atomically so a concurrent reader
// never sees a half-trimmed one.
func (s *Store) trim(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("monitor: read %s: %w", path, err)
	}
	lines := splitLines(raw)
	if len(lines) <= s.cfg.MaxEvents && len(raw) <= s.cfg.MaxBytes {
		return nil
	}
	if len(lines) > s.cfg.MaxEvents {
		lines = lines[len(lines)-s.cfg.MaxEvents:]
	}
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	for buf.Len() > s.cfg.MaxBytes && len(lines) > 1 {
		buf.Next(len(lines[0]) + 1) // drop the oldest retained line
		lines = lines[1:]
	}
	return writeFileAtomic0600(path, buf.Bytes())
}

// evictOldestStreamIfFull removes the least-recently-appended stream when
// the store already holds maxStreams. Callers hold s.mu and reach it only
// before creating a genuinely new stream.
func (s *Store) evictOldestStreamIfFull() error {
	metas, err := s.List()
	if err != nil || len(metas) < maxStreams {
		return err
	}
	oldest := metas[0]
	for _, m := range metas[1:] {
		if m.ModTime.Before(oldest.ModTime) {
			oldest = m
		}
	}
	return os.RemoveAll(oldest.Dir)
}

// splitLines splits raw on '\n', dropping blank lines.
func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	for _, l := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			lines = append(lines, l)
		}
	}
	return lines
}

// Tail returns the newest n decoded events (oldest-first) for one stream;
// n <= 0 returns all. A missing stream yields nothing and no error (normal
// for a reader), and an undecodable line is skipped, not fatal.
func (s *Store) Tail(sandboxID, sessionID string, n int) ([]Event, error) {
	_, file, err := s.streamPath(sandboxID, sessionID)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("monitor: read %s: %w", file, err)
	}
	lines := splitLines(raw)
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	events := make([]Event, 0, len(lines))
	for _, l := range lines {
		if ev, err := Decode(l); err == nil {
			events = append(events, ev)
		}
	}
	return events, nil
}

// StreamMeta describes one retained stream. The ids come from the directory
// name, exact because only validID-approved ids built it.
type StreamMeta struct {
	SandboxID string
	SessionID string
	Dir       string
	ModTime   time.Time
}

// List enumerates every retained stream, skipping anything that is not one
// (blobs.ndjson, a stray file, a directory with no events yet).
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
		sandboxID, sessionID, ok := strings.Cut(ent.Name(), idSep)
		if !ent.IsDir() || !ok {
			continue
		}
		dir := filepath.Join(s.cfg.Root, ent.Name())
		fi, err := os.Stat(filepath.Join(dir, eventsFile))
		if err != nil {
			continue
		}
		metas = append(metas, StreamMeta{
			SandboxID: sandboxID, SessionID: sessionID, Dir: dir, ModTime: fi.ModTime(),
		})
	}
	return metas, nil
}

// The filesystem helpers below mirror services/host/lease/paths.go, but are
// Lstat-based rather than O_NOFOLLOW so this package stays portable; the
// residual check-to-open TOCTOU window is an accepted tradeoff here.

// refuseSymlink errors if any path exists and is a symlink; missing is fine.
func refuseSymlink(paths ...string) error {
	for _, path := range paths {
		fi, err := os.Lstat(path)
		switch {
		case os.IsNotExist(err):
		case err != nil:
			return err
		case fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("monitor: refusing to follow symlink at %s", path)
		}
	}
	return nil
}

// ensureDir0700 creates dir at 0700 if absent, refuses a symlink or
// non-directory in its place, and tightens a loose mode back.
func ensureDir0700(dir string) error {
	if err := refuseSymlink(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("monitor: create dir %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	switch {
	case err != nil:
		return fmt.Errorf("monitor: stat dir %s: %w", dir, err)
	case !fi.IsDir():
		return fmt.Errorf("monitor: %s exists and is not a directory", dir)
	case fi.Mode().Perm() != 0o700:
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// openAppend0600 opens path for append at 0600, refusing an existing
// symlink. O_CREATE does not re-apply the mode to an existing file, so a
// loose one is tightened explicitly.
func openAppend0600(path string) (*os.File, error) {
	if err := refuseSymlink(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm() != 0o600 {
		_ = f.Chmod(0o600)
	}
	return f, nil
}

// writeFileAtomic0600 writes via temp file plus rename, so a concurrent
// reader never observes a partial file.
func writeFileAtomic0600(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := refuseSymlink(path, tmp); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("monitor: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("monitor: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
