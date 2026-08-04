package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func newTestBlobStore(t *testing.T, cfg BlobStoreConfig) *BlobStore {
	t.Helper()
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	b, err := NewBlobStore(cfg)
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	return b
}

func TestBlobStorePutGetRoundTrip(t *testing.T) {
	b := newTestBlobStore(t, BlobStoreConfig{})
	text := "the quick brown fox"
	ok, err := b.Put(Blob{Hash: hashOf(text), Text: text})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !ok {
		t.Fatal("Put() = false, want true")
	}
	got, ok, err := b.Get(hashOf(text))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get() ok = false, want true")
	}
	if got.Text != text {
		t.Errorf("Get().Text = %q, want %q", got.Text, text)
	}
}

func TestBlobStorePutRejectsMismatchedHash(t *testing.T) {
	b := newTestBlobStore(t, BlobStoreConfig{})
	ok, err := b.Put(Blob{Hash: strings.Repeat("a", 64), Text: "hello"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok {
		t.Fatal("Put() = true for a mismatched hash, want false")
	}
	if _, ok, _ := b.Get(strings.Repeat("a", 64)); ok {
		t.Fatal("Get() found a blob that should have been rejected")
	}
}

func TestBlobStoreGetMissingOrMalformedHash(t *testing.T) {
	b := newTestBlobStore(t, BlobStoreConfig{})
	if _, ok, err := b.Get(strings.Repeat("a", 64)); ok || err != nil {
		t.Errorf("Get(never stored) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	if _, ok, err := b.Get("not-a-hash"); ok || err != nil {
		t.Errorf("Get(malformed hash) = ok=%v err=%v, want ok=false err=nil (never touches disk)", ok, err)
	}
}

func TestBlobStorePutIgnoresClientSuppliedBytes(t *testing.T) {
	b := newTestBlobStore(t, BlobStoreConfig{})
	text := "0123456789"
	ok, err := b.Put(Blob{Hash: hashOf(text), Text: text, Bytes: 1})
	if err != nil || !ok {
		t.Fatalf("Put: ok=%v err=%v", ok, err)
	}
	got, _, err := b.Get(hashOf(text))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Bytes != len(text) {
		t.Errorf("stored Bytes = %d, want %d (recomputed from len(Text), not client-supplied)", got.Bytes, len(text))
	}
}

// TestBlobStorePutStoresRedactedNotRawText is the blob-domain canary test:
// the ORIGINAL (unredacted) text must verify against the client hash, but
// what actually lands on disk (and what Get returns) is redacted.
func TestBlobStorePutStoresRedactedNotRawText(t *testing.T) {
	b := newTestBlobStore(t, BlobStoreConfig{})
	text := "aws creds: AWS_ACCESS_KEY_ID=" + canaryAWSKey
	ok, err := b.Put(Blob{Hash: hashOf(text), Text: text})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !ok {
		t.Fatal("Put() = false, want true (hash matches the ORIGINAL text)")
	}
	path, err := blobPath(b.cfg.Root, hashOf(text))
	if err != nil {
		t.Fatalf("blobPath: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read blob file: %v", err)
	}
	if strings.Contains(string(raw), canaryAWSKey) {
		t.Fatalf("canary secret reached disk unredacted:\n%s", raw)
	}
	got, ok, err := b.Get(hashOf(text))
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if strings.Contains(got.Text, canaryAWSKey) {
		t.Fatalf("Get() returned the canary secret unredacted: %q", got.Text)
	}
}

func TestBlobStoreFilesAre0700DirsAnd0600Files(t *testing.T) {
	b := newTestBlobStore(t, BlobStoreConfig{})
	text := "hello"
	if _, err := b.Put(Blob{Hash: hashOf(text), Text: text}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	shard := hashOf(text)[:2]
	shardDir := filepath.Join(b.cfg.Root, shard)
	fi, err := os.Stat(shardDir)
	if err != nil {
		t.Fatalf("stat shard dir: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("shard dir mode = %o, want 0700", fi.Mode().Perm())
	}
	path, err := blobPath(b.cfg.Root, hashOf(text))
	if err != nil {
		t.Fatalf("blobPath: %v", err)
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat blob file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("blob file mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestBlobStoreEvictsOldestOverMaxBlobs(t *testing.T) {
	b := newTestBlobStore(t, BlobStoreConfig{MaxBlobs: 2, MaxBytes: 1 << 20})
	texts := []string{"first-blob-text", "second-blob-text", "third-blob-text"}
	for _, txt := range texts {
		if ok, err := b.Put(Blob{Hash: hashOf(txt), Text: txt}); err != nil || !ok {
			t.Fatalf("Put(%q): ok=%v err=%v", txt, ok, err)
		}
	}
	if _, ok, _ := b.Get(hashOf(texts[0])); ok {
		t.Error("oldest blob should have been evicted under MaxBlobs=2")
	}
	if _, ok, _ := b.Get(hashOf(texts[2])); !ok {
		t.Error("newest blob should still be present")
	}
}

func TestBlobStoreEvictsOldestOverMaxBytes(t *testing.T) {
	// Each stored blob is a JSON object (hash + bytes + text), so the
	// budget must be several multiples of ONE serialized blob's size for
	// this test to distinguish "some eviction happened" from "everything
	// got evicted because even one blob alone busts the budget". Put one
	// blob first to measure the real on-disk size, then size the budget
	// off that measurement instead of guessing at JSON overhead.
	b := newTestBlobStore(t, BlobStoreConfig{MaxBlobs: 1000, MaxBytes: 1 << 20})
	texts := []string{"aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccc"}
	if ok, err := b.Put(Blob{Hash: hashOf(texts[0]), Text: texts[0]}); err != nil || !ok {
		t.Fatalf("Put(%q): ok=%v err=%v", texts[0], ok, err)
	}
	path, err := blobPath(b.cfg.Root, hashOf(texts[0]))
	if err != nil {
		t.Fatalf("blobPath: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	perBlobBytes := fi.Size()

	b2 := newTestBlobStore(t, BlobStoreConfig{MaxBlobs: 1000, MaxBytes: perBlobBytes*2 + perBlobBytes/2})
	for _, txt := range texts {
		if ok, err := b2.Put(Blob{Hash: hashOf(txt), Text: txt}); err != nil || !ok {
			t.Fatalf("Put(%q): ok=%v err=%v", txt, ok, err)
		}
	}
	if _, ok, _ := b2.Get(hashOf(texts[0])); ok {
		t.Error("oldest blob should have been evicted under the byte budget")
	}
	if _, ok, _ := b2.Get(hashOf(texts[2])); !ok {
		t.Error("newest blob should still be present")
	}
}

func TestNewBlobStoreRequiresRoot(t *testing.T) {
	if _, err := NewBlobStore(BlobStoreConfig{}); err == nil {
		t.Fatal("NewBlobStore(no Root) = nil error, want a requirement error")
	}
}
