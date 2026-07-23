package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// BlobCache is a thread-safe, content-addressed cache of Blob bodies bounded
// by total bytes (not count): when Put would push the total over maxBytes, the
// oldest-inserted blobs are evicted first (until back under budget or the
// cache is empty). Content-addressing means the extension only ever sends a
// blob body once per hash, so most traffic is envelope + hash-only events;
// this cache holds the bodies the TUI resolves hashes against.
type BlobCache struct {
	mu       sync.Mutex
	maxBytes int
	order    []string // insertion order of hashes, oldest first
	entries  map[string]Blob
	total    int
}

// NewBlobCache creates a BlobCache bounded to maxBytes of blob text. A
// non-positive maxBytes is treated as 1 (never store an unbounded cache from
// a zero-value config).
func NewBlobCache(maxBytes int) *BlobCache {
	if maxBytes <= 0 {
		maxBytes = 1
	}
	return &BlobCache{
		maxBytes: maxBytes,
		entries:  make(map[string]Blob),
	}
}

// Put stores bl, evicting the oldest blobs if needed to stay within budget.
// It returns false — and does NOT store bl — in two cases (R1-11):
//
//  1. bl.Hash is not the lowercase hex sha256 digest of bl.Text. Blobs are
//     content-addressed: the hash is the lookup key an unauthenticated GET
//     /blob/{hash} later trusts, so a client-supplied hash that doesn't
//     actually describe the body it's paired with must be rejected outright,
//     not stored under a false label.
//  2. bl.Hash already has a stored entry whose Text differs from bl.Text.
//     Content-addressed blobs are immutable by hash — this should be
//     unreachable given check 1 (two different texts can't legitimately
//     share a sha256 digest), but Put defends the invariant directly rather
//     than relying solely on the hash check to prevent it. A second Put
//     under an existing hash with the SAME text is a harmless no-op (true).
//
// bl.Bytes is never trusted as sent — it is overridden with len(bl.Text)
// before any accounting, hashing, or eviction runs, so a client cannot lie
// about a blob's size (e.g. claiming Bytes:1 for a multi-megabyte Text) to
// defeat the byte budget and force unbounded memory growth. The stored blob
// (and what Get later returns) always carries the real length.
func (b *BlobCache) Put(bl Blob) bool {
	bl.Bytes = len(bl.Text)
	sum := sha256.Sum256([]byte(bl.Text))
	if bl.Hash != hex.EncodeToString(sum[:]) {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, ok := b.entries[bl.Hash]; ok {
		if existing.Text != bl.Text {
			return false
		}
		return true // identical re-Put: already stored, nothing to do
	}
	b.order = append(b.order, bl.Hash)
	b.entries[bl.Hash] = bl
	b.total += bl.Bytes
	b.evictLocked()
	return true
}

// Get looks up a blob by hash. ok is false if the hash was never stored or
// has since been evicted.
func (b *BlobCache) Get(hash string) (Blob, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bl, ok := b.entries[hash]
	return bl, ok
}

// evictLocked drops the oldest-inserted blobs until total <= maxBytes or the
// cache is empty. Caller must hold b.mu.
func (b *BlobCache) evictLocked() {
	for b.total > b.maxBytes && len(b.order) > 0 {
		oldest := b.order[0]
		b.order = b.order[1:]
		if bl, ok := b.entries[oldest]; ok {
			b.total -= bl.Bytes
			delete(b.entries, oldest)
		}
	}
}
