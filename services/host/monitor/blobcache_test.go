package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// blob builds a valid content-addressed Blob: Hash is always the real
// sha256 of Text (BlobCache.Put now requires this — see R1-11), and Text is
// exactly n bytes so the byte-budget math every test below asserts against
// still lines up. id only seeds distinct content per call (so two calls
// with the same n still get distinct hashes/identities, the way distinct
// sandboxIds would in production) — it never appears in the Hash itself.
func blob(id string, n int) Blob {
	text := contentOfLen(id, n)
	sum := sha256.Sum256([]byte(text))
	return Blob{Hash: hex.EncodeToString(sum[:]), Bytes: n, Text: text}
}

// contentOfLen returns an n-byte string derived from id (truncated or
// padded with 'x' as needed) so every blob() call gets exactly n bytes of
// Text regardless of id's length.
func contentOfLen(id string, n int) string {
	if len(id) >= n {
		return id[:n]
	}
	return id + strings.Repeat("x", n-len(id))
}

func TestBlobCachePutGetRoundTrip(t *testing.T) {
	c := NewBlobCache(1024)
	a := blob("a", 10)
	if ok := c.Put(a); !ok {
		t.Fatalf("Put(a) = false, want true")
	}
	got, ok := c.Get(a.Hash)
	if !ok {
		t.Fatalf("Get(a) ok=false, want true")
	}
	if got.Bytes != 10 || got.Text != a.Text {
		t.Fatalf("Get(a) = %+v, want bytes=10 text=%q", got, a.Text)
	}
}

// TestBlobCachePutRejectsMismatchedHash is R1-11: a blob whose Hash does not
// equal sha256(Text) must be dropped, not stored — the hash is the lookup
// key an unauthenticated GET /blob/{hash} later trusts, so it has to be
// verified before anything is cached under it.
func TestBlobCachePutRejectsMismatchedHash(t *testing.T) {
	c := NewBlobCache(1024)
	bl := Blob{Hash: "not-the-real-sha256", Bytes: 5, Text: "hello"}
	if ok := c.Put(bl); ok {
		t.Fatalf("Put(mismatched hash) = true, want false (rejected)")
	}
	if _, ok := c.Get("not-the-real-sha256"); ok {
		t.Fatalf("Get(not-the-real-sha256) ok=true, mismatched blob must never be stored")
	}
}

// TestBlobCachePutStoresValidContentAddressedBlob is the positive case: a
// blob whose Hash correctly equals sha256(Text) is accepted and retrievable.
func TestBlobCachePutStoresValidContentAddressedBlob(t *testing.T) {
	c := NewBlobCache(1024)
	sum := sha256.Sum256([]byte("hello world"))
	bl := Blob{Hash: hex.EncodeToString(sum[:]), Bytes: len("hello world"), Text: "hello world"}
	if ok := c.Put(bl); !ok {
		t.Fatalf("Put(valid) = false, want true (accepted)")
	}
	got, ok := c.Get(bl.Hash)
	if !ok || got.Text != "hello world" {
		t.Fatalf("Get(%s) = %+v ok=%v, want the stored blob", bl.Hash, got, ok)
	}
}

// TestBlobCachePutIgnoresSecondWriteWithDifferentContent is R1-11's
// immutability requirement: a later Put claiming an EXISTING hash but with
// different Text must be ignored (dropped), not overwrite the original —
// content-addressed blobs are immutable by hash.
func TestBlobCachePutIgnoresSecondWriteWithDifferentContent(t *testing.T) {
	c := NewBlobCache(1024)
	sum := sha256.Sum256([]byte("original text"))
	first := Blob{Hash: hex.EncodeToString(sum[:]), Bytes: len("original text"), Text: "original text"}
	if ok := c.Put(first); !ok {
		t.Fatalf("Put(first) = false, want true")
	}

	// Same Hash as `first`, but different Text — this can only reach Put via
	// a stale/forged hash, since a legitimate sha256(Text) would differ.
	second := Blob{Hash: first.Hash, Bytes: len("different text"), Text: "different text"}
	if ok := c.Put(second); ok {
		t.Fatalf("Put(second, different content under first's hash) = true, want false (rejected)")
	}

	got, ok := c.Get(first.Hash)
	if !ok || got.Text != "original text" {
		t.Fatalf("Get(%s) = %+v ok=%v, want the original blob unchanged", first.Hash, got, ok)
	}
}

// TestBlobCachePutIsIdempotentForIdenticalReput proves a re-Put of the exact
// same (hash, text) pair — which content-addressing legitimately produces if
// a client re-sends a blob it already sent — is accepted as a no-op rather
// than rejected as if it were a mutation attempt.
func TestBlobCachePutIsIdempotentForIdenticalReput(t *testing.T) {
	c := NewBlobCache(1024)
	a := blob("a", 10)
	if ok := c.Put(a); !ok {
		t.Fatalf("Put(a) first = false, want true")
	}
	if ok := c.Put(a); !ok {
		t.Fatalf("Put(a) identical re-Put = false, want true (no-op success)")
	}
	got, ok := c.Get(a.Hash)
	if !ok || got != a {
		t.Fatalf("Get(a) = %+v ok=%v, want %+v ok=true", got, ok, a)
	}
}

// TestBlobCachePutIgnoresClientSuppliedBytes proves SEC-2: Put derives the
// real byte cost from len(bl.Text), never from the caller-supplied bl.Bytes.
// A blob that lies about its size (Bytes:1 for a 1000-byte Text) must still
// be accounted — and evict earlier entries — at its true size. The Hash is
// still a valid, honest sha256(Text); only Bytes lies.
func TestBlobCachePutIgnoresClientSuppliedBytes(t *testing.T) {
	c := NewBlobCache(1500)
	a := blob("a", 1000)
	if ok := c.Put(a); !ok { // honest: total = 1000
		t.Fatalf("Put(a) = false, want true")
	}

	// "lie": claims Bytes:1 but Text is actually 1000 bytes, and Hash is the
	// real sha256 of that 1000-byte Text (only Bytes is a lie). If Put
	// trusted the claimed size, total would read as 1001 (comfortably under
	// the 1500 budget) and "a" would survive. With SEC-2 fixed, the real
	// size (1000) is used, so total becomes 2000 > 1500 and "a" is evicted.
	honestText := contentOfLen("b", 1000)
	sum := sha256.Sum256([]byte(honestText))
	lying := Blob{Hash: hex.EncodeToString(sum[:]), Bytes: 1, Text: honestText}
	if ok := c.Put(lying); !ok {
		t.Fatalf("Put(lying) = false, want true")
	}

	gotB, ok := c.Get(lying.Hash)
	if !ok {
		t.Fatalf("Get(b) ok=false, want true")
	}
	if gotB.Bytes != 1000 {
		t.Fatalf("Get(b).Bytes = %d, want 1000 (real len(Text), not the claimed 1)", gotB.Bytes)
	}
	if _, ok := c.Get(a.Hash); ok {
		t.Fatalf("Get(a) ok=true, want evicted — accounting must use real size (1000), not the claimed 1, or eviction under-fires and the budget is defeated")
	}
}

func TestBlobCacheGetMissing(t *testing.T) {
	c := NewBlobCache(1024)
	if _, ok := c.Get("missing"); ok {
		t.Fatalf("Get(missing) ok=true, want false")
	}
}

func TestBlobCacheEvictsOldestOverBudget(t *testing.T) {
	c := NewBlobCache(25) // budget for ~2.5 10-byte blobs
	a, b, cc := blob("a", 10), blob("b", 10), blob("c", 10)
	c.Put(a)
	c.Put(b)
	c.Put(cc) // total would be 30 > 25 -> evict "a"

	if _, ok := c.Get(a.Hash); ok {
		t.Fatalf("Get(a) ok=true after eviction, want false")
	}
	if _, ok := c.Get(b.Hash); !ok {
		t.Fatalf("Get(b) ok=false, want true (should survive)")
	}
	if _, ok := c.Get(cc.Hash); !ok {
		t.Fatalf("Get(c) ok=false, want true (just inserted)")
	}
}

func TestBlobCacheEvictsMultipleToFitOneLargeBlob(t *testing.T) {
	c := NewBlobCache(30)
	a, b, cc := blob("a", 10), blob("b", 10), blob("c", 25)
	c.Put(a)
	c.Put(b)
	c.Put(cc) // 10+10+25=45 > 30 -> evict a, then b (10+25=35>30) -> only c fits (25<=30)

	if _, ok := c.Get(a.Hash); ok {
		t.Fatalf("Get(a) ok=true, want evicted")
	}
	if _, ok := c.Get(b.Hash); ok {
		t.Fatalf("Get(b) ok=true, want evicted")
	}
	if _, ok := c.Get(cc.Hash); !ok {
		t.Fatalf("Get(c) ok=false, want true")
	}
}

func TestBlobCacheOversizedBlobIsNotRetained(t *testing.T) {
	c := NewBlobCache(10)
	huge := blob("huge", 100) // exceeds the whole budget on its own
	c.Put(huge)
	if _, ok := c.Get(huge.Hash); ok {
		t.Fatalf("Get(huge) ok=true, an over-budget single blob should not be retained")
	}
}

func TestBlobCacheZeroMaxBytesTreatedAsOne(t *testing.T) {
	c := NewBlobCache(0)
	a := blob("a", 1)
	c.Put(a)
	if _, ok := c.Get(a.Hash); !ok {
		t.Fatalf("Get(a) ok=false, a 1-byte blob should fit in the minimum budget of 1")
	}
}

// TestBlobCacheConcurrentPutGet exercises BlobCache under -race.
func TestBlobCacheConcurrentPutGet(t *testing.T) {
	c := NewBlobCache(10_000)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("h%d", i)
			bl := blob(id, 10)
			c.Put(bl)
			c.Get(bl.Hash)
		}(i)
	}
	wg.Wait()
}
