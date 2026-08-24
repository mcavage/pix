package hosttrust

import (
	"encoding/json"
	"io"
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
// rather than following it. It opens path with O_NOFOLLOW (openNoFollow)
// instead of Lstat-then-ReadFile: that two-step sequence leaves a TOCTOU
// window in which a symlink swapped in between the Lstat and the ReadFile is
// silently followed and its target's bytes returned as if they were the
// document's own — exactly the confused-deputy read this gate exists to
// prevent. Absent is reported via os.IsNotExist (openNoFollow wraps ENOENT
// in a *os.PathError, which satisfies it), so a caller can supply its own
// fresh-document default; any other error (unreadable, a symlink) is the
// caller's to propagate — never a partial or substituted document.
func ReadDocumentBytes(path string) ([]byte, error) {
	f, err := openNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// SaveDocument marshals doc as indented JSON and writes it to dir/name
// symlink-safe + atomic: refuse an already-symlinked destination (checked
// with openNoFollow, not a standalone Lstat — see below), then a same-dir
// temp file + rename (sys.AtomicWriteInDir).
//
// The refusal check is a fail-fast policy decision, not the sole line of
// defense: sys.AtomicWriteInDir's final step is os.Rename(tmp, dest), and
// POSIX rename(2) never follows a symlink at its destination argument — it
// atomically replaces the DIRECTORY ENTRY named dest, whatever it currently
// is, rather than writing through to whatever a symlink there points at. So
// even a destination that becomes a symlink in the (now much narrower)
// window between this check and the rename cannot result in this package
// writing an attacker-chosen document through to an arbitrary target; the
// worst case is the rename silently clobbering the symlink instead of this
// function refusing it up front. openNoFollow closes the check itself
// against the same class of TOCTOU ReadDocumentBytes closes: a plain Lstat
// followed by a later, separate operation on the same path is racy in
// principle even when (as here) the final operation happens to be safe on
// its own.
func SaveDocument(dir, name string, doc any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, name)
	if err := refuseSymlinkedDestination(dest); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return sys.AtomicWriteInDir(dir, name, append(b, '\n'), 0o644)
}

// refuseSymlinkedDestination reports an error iff dest currently exists and
// is a symlink, using the no-follow open itself (rather than Lstat) as the
// existence+symlink probe so the check cannot be fooled by a symlink
// appearing after a separate stat call returns.
func refuseSymlinkedDestination(dest string) error {
	f, err := openNoFollow(dest, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return f.Close()
}
