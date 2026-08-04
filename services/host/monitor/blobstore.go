package monitor

// blobstore.go replaces the deleted in-memory BlobCache with a bounded,
// content-addressed, file-backed store for full blob bodies (system
// prompt, tool args/result text, assistant replies) — the ONE place full
// text crosses the wire at all (POST /blob, on first sight of a hash; see
// ingest.go). Content-addressing means a blob is only ever POSTed once per
// hash; every subsequent reference to it is hash-only on the event stream.
//
// SECURITY NOTE (deliberate, not an oversight): the client-asserted hash is
// verified against the ORIGINAL text before anything is stored (same
// invariant the deleted BlobCache enforced) — but the byte content actually
// WRITTEN to disk is Redact(text), not the original. That means a blob's
// stored bytes do not literally hash back to its own filename/key. This is
// intentional: this store exists specifically to hold the highest-risk
// text in the whole pipeline (real tool output, unsummarized), and
// redaction must win over strict content-addressing purity here. Nothing
// in this story reads a blob back out over any network path (see
// ingest.go: there is no GET /blob/{hash}), so the mismatch has no
// user-visible effect yet; it is recorded here for whoever wires a reader
// to this store later (Story07+).
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// DefaultBlobStoreMaxBytes / DefaultBlobStoreMaxBlobs bound the WHOLE
	// store (unlike Store's per-stream bounds): a blob is content-addressed
	// and can be referenced across many streams, so it doesn't belong to
	// any one of them.
	DefaultBlobStoreMaxBytes = 64 << 20 // 64MB
	DefaultBlobStoreMaxBlobs = 5000
)

// BlobStoreConfig configures a BlobStore. Zero-valued fields fall back to
// their DefaultXxx constant; Root is required.
type BlobStoreConfig struct {
	Root     string
	MaxBytes int64
	MaxBlobs int
}

// BlobStore is the bounded, content-addressed, file-backed blob domain.
type BlobStore struct {
	cfg BlobStoreConfig
}

// NewBlobStore constructs a BlobStore rooted at cfg.Root, creating it
// (0700) if absent.
func NewBlobStore(cfg BlobStoreConfig) (*BlobStore, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("monitor: NewBlobStore: Root is required")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultBlobStoreMaxBytes
	}
	if cfg.MaxBlobs <= 0 {
		cfg.MaxBlobs = DefaultBlobStoreMaxBlobs
	}
	if err := ensureDir0700(cfg.Root); err != nil {
		return nil, err
	}
	return &BlobStore{cfg: cfg}, nil
}

// Put verifies bl.Hash == sha256(bl.Text) (R1-11's invariant, carried over
// from the deleted BlobCache) and, only if it matches, stores Redact(bl.Text)
// under that hash (see the security note above for why the stored bytes are
// redacted rather than the literal preimage of the hash). ok is false, and
// nothing is written, if the hash doesn't match. bl.Bytes is never trusted
// as sent — recomputed from len(bl.Text) — so a client can't lie about size
// to defeat MaxBytes accounting.
func (b *BlobStore) Put(bl Blob) (ok bool, err error) {
	bl.Bytes = len(bl.Text)
	sum := sha256.Sum256([]byte(bl.Text))
	if bl.Hash != hex.EncodeToString(sum[:]) {
		return false, nil
	}
	if !validHash(bl.Hash) {
		return false, nil // defensive; unreachable given the sha256 check above
	}
	path, err := blobPath(b.cfg.Root, bl.Hash)
	if err != nil {
		return false, err
	}
	if err := ensureDir0700(filepath.Dir(path)); err != nil {
		return false, err
	}
	stored := Blob{Hash: bl.Hash, Text: RedactText(bl.Text)}
	stored.Bytes = len(stored.Text)
	data, err := json.Marshal(stored)
	if err != nil {
		return false, fmt.Errorf("monitor: marshal blob %s: %w", bl.Hash, err)
	}
	// Idempotent: re-Put under an existing hash is a cheap no-op rewrite
	// (same content by construction of content-addressing), so no need to
	// special-case "already exists" — writeFileAtomic0600 just rewrites it.
	if err := writeFileAtomic0600(path, data); err != nil {
		return false, err
	}
	return true, b.evictIfOverBudget()
}

// Get looks up a stored blob by hash. ok is false if hash was never stored,
// has since been evicted, or is not a well-formed sha256 hex digest (the
// last case never touches disk at all).
func (b *BlobStore) Get(hash string) (Blob, bool, error) {
	if !validHash(hash) {
		return Blob{}, false, nil
	}
	path, err := blobPath(b.cfg.Root, hash)
	if err != nil {
		return Blob{}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Blob{}, false, nil
		}
		return Blob{}, false, fmt.Errorf("monitor: read blob %s: %w", hash, err)
	}
	var bl Blob
	if err := json.Unmarshal(data, &bl); err != nil {
		return Blob{}, false, fmt.Errorf("monitor: decode blob %s: %w", hash, err)
	}
	return bl, true, nil
}

// blobFileInfo is one on-disk blob file's accounting info, used by
// evictIfOverBudget.
type blobFileInfo struct {
	path    string
	size    int64
	modTime time.Time
}

// evictIfOverBudget removes the oldest-by-mtime blob files until the store
// is back within both MaxBytes and MaxBlobs — the file-backed analogue of
// the deleted BlobCache's drop-oldest eviction.
func (b *BlobStore) evictIfOverBudget() error {
	var files []blobFileInfo
	var total int64
	shardDirs, err := os.ReadDir(b.cfg.Root)
	if err != nil {
		return fmt.Errorf("monitor: read blob store root: %w", err)
	}
	for _, shard := range shardDirs {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(b.cfg.Root, shard.Name())
		ents, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			p := filepath.Join(shardPath, e.Name())
			files = append(files, blobFileInfo{path: p, size: fi.Size(), modTime: fi.ModTime()})
			total += fi.Size()
		}
	}
	if total <= b.cfg.MaxBytes && len(files) <= b.cfg.MaxBlobs {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, f := range files {
		if total <= b.cfg.MaxBytes && len(files) <= b.cfg.MaxBlobs {
			break
		}
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("monitor: evict blob %s: %w", f.path, err)
		}
		total -= f.size
		files = files[1:]
	}
	return nil
}
