package hosttrust

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"pix/host/sys"
)

// Record is the ONE acceptance-record shape every subject kind uses: the
// fingerprint an operator approved for that subject, plus enough provenance
// to explain the record's own hygiene. A future subject kind (an
// environment) reuses this exact type rather than growing a parallel record
// shape — F6 of this extraction.
type Record struct {
	Path        string `json:"path,omitempty"`
	Remote      string `json:"remote,omitempty"`
	Commit      string `json:"commit,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

// AcceptanceStore is a Subject-keyed map of Record — the launcher-owned
// acceptance data, never derived from anything inside a pack or environment
// payload. Embed it (or hold its Accepted map directly, as
// workflow/pack.PackTrustStore does for on-disk compatibility with its
// pre-extraction shape) in a domain-specific document that also carries that
// domain's own fields.
type AcceptanceStore struct {
	Accepted map[string]Record `json:"accepted,omitempty"`
}

// Get returns the record accepted for subj, if any and non-empty.
func (s *AcceptanceStore) Get(subj Subject) (Record, bool) {
	if s == nil || s.Accepted == nil {
		return Record{}, false
	}
	r, ok := s.Accepted[subj.Key()]
	if !ok || r.Fingerprint == "" {
		return Record{}, false
	}
	return r, true
}

// Put records rec under subj.
func (s *AcceptanceStore) Put(subj Subject, rec Record) {
	if s.Accepted == nil {
		s.Accepted = map[string]Record{}
	}
	s.Accepted[subj.Key()] = rec
}

// WithLock runs fn holding the exclusive cross-process flock at lockPath —
// the single lock every trust-document read-modify-write serializes on. fn
// must not call WithLock again for the SAME lockPath: flock is per open file
// description, so a nested acquire self-deadlocks (or times out against
// sys's bounded wait) rather than erroring cleanly. The sanctioned way to
// mutate a document from INSIDE fn is LoadMutateSave (mutate.go), which never
// acquires a lock at all — see its doc comment for why that makes nesting
// impossible by construction rather than merely forbidden by convention.
func WithLock(lockPath string, fn func() error) error {
	return sys.Lock(lockPath, fn)
}

// ReadDocumentBytes reads path's raw bytes, refusing a symlinked source
// rather than following it. Absent is reported via the plain os.ReadFile
// error (os.IsNotExist), so a caller can supply its own fresh-document
// default; any other error (unreadable, a symlink) is the caller's to
// propagate — never a partial or substituted document.
func ReadDocumentBytes(path string) ([]byte, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read through it", path)
	}
	return os.ReadFile(path)
}

// SaveDocument marshals doc as indented JSON and writes it to dir/name
// symlink-safe + atomic: Lstat-refuse a symlinked destination, then a
// same-dir temp file + rename (sys.AtomicWriteInDir).
func SaveDocument(dir, name string, doc any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, name)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return sys.AtomicWriteInDir(dir, name, append(b, '\n'), 0o644)
}
