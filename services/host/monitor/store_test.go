package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canaryAWSKey is a deliberately fake-but-realistic AWS access key id: a
// known secret-shaped token planted in input whose ABSENCE from the stored
// bytes is what these tests prove. TestRedactionCanaryIsRealSecretShape
// keeps it honest — if the canary stopped matching a pattern, every
// "redaction worked" assertion below would pass for the wrong reason.
const canaryAWSKey = "AKIAABCDEFGHIJKLMNOP"

func newTestStore(t *testing.T, cfg StoreConfig) *Store {
	t.Helper()
	if cfg.Root == "" {
		cfg.Root = filepath.Join(t.TempDir(), "monitor")
	}
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func toolEvent(sandboxID, sessionID, summary string, seq uint64) ToolEnd {
	return ToolEnd{
		env:           env{Kind: KindToolEnd, SandboxID: sandboxID, SessionID: sessionID, TurnID: "t1", Seq: seq, TS: 1700000000000},
		ToolID:        fmt.Sprintf("tool-%d", seq),
		OK:            true,
		ResultBytes:   len(summary),
		ResultSummary: summary,
	}
}

func TestNewStoreRequiresRootAndCreatesItAt0700(t *testing.T) {
	if _, err := NewStore(StoreConfig{}); err == nil {
		t.Fatal("NewStore with no Root = nil error, want an error")
	}
	s := newTestStore(t, StoreConfig{})
	fi, err := os.Stat(s.cfg.Root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("root perms = %o, want 0700", fi.Mode().Perm())
	}
}

func TestAppendTailListRoundTrip(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	for i := 1; i <= 3; i++ {
		if err := s.Append(toolEvent("sbx-1", "sess-1", fmt.Sprintf("r%d", i), uint64(i))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := s.Append(toolEvent("sbx-2", "sess-2", "other", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Tail("sbx-1", "sess-1", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Tail returned %d events, want 3", len(got))
	}
	if sum := got[2].(ToolEnd).ResultSummary; sum != "r3" {
		t.Fatalf("last event summary = %q, want r3 (oldest-first order)", sum)
	}
	newest, err := s.Tail("sbx-1", "sess-1", 2)
	if err != nil {
		t.Fatalf("Tail(2): %v", err)
	}
	if len(newest) != 2 || newest[0].(ToolEnd).ResultSummary != "r2" {
		t.Fatalf("Tail(2) = %+v, want the newest two", newest)
	}
	if empty, err := s.Tail("sbx-1", "never-seen", 0); err != nil || len(empty) != 0 {
		t.Fatalf("Tail(unknown stream) = %v, %v; want empty and no error", empty, err)
	}

	metas, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("List returned %d streams, want 2", len(metas))
	}
	seen := map[string]bool{}
	for _, m := range metas {
		seen[m.SandboxID+"/"+m.SessionID] = true
		if m.Bytes <= 0 {
			t.Errorf("stream %s has Bytes=%d, want > 0", m.Dir, m.Bytes)
		}
	}
	if !seen["sbx-1/sess-1"] || !seen["sbx-2/sess-2"] {
		t.Fatalf("List ids = %v, want the original (sandbox, session) pairs", seen)
	}
}

// TestAppendRejectsInvalidIDsStrictly is the strict-name regression: a
// traversal, a separator, a control byte, a dotfile, an over-long id, or the
// stream separator itself must be REFUSED — never slugified into some
// neighbouring directory — and nothing may be written outside the root.
func TestAppendRejectsInvalidIDsStrictly(t *testing.T) {
	bad := []string{
		"..", ".", "../../etc", "a/b", `a\b`, "/abs", ".hidden", "-lead", "_lead",
		"a=b", "nul\x00byte", "sp ace", "tab\tchar", "new\nline", "emoji🙂",
		strings.Repeat("a", 97),
	}
	for _, id := range bad {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			s := newTestStore(t, StoreConfig{})
			if err := s.Append(toolEvent(id, "sess", "x", 1)); err == nil {
				t.Errorf("Append(sandboxId=%q) = nil error, want refusal", id)
			}
			if err := s.Append(toolEvent("sbx", id, "x", 1)); err == nil {
				t.Errorf("Append(sessionId=%q) = nil error, want refusal", id)
			}
			if _, err := s.Tail(id, "sess", 0); err == nil {
				t.Errorf("Tail(sandboxId=%q) = nil error, want refusal", id)
			}
			entries, err := os.ReadDir(s.cfg.Root)
			if err != nil {
				t.Fatalf("read root: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("root contains %d entries after refused appends, want 0", len(entries))
			}
		})
	}
}

// An EMPTY id is the one non-conforming value that is not hostile (the tap
// sends sandboxId "" outside a sandbox): it maps to one fixed constant.
func TestAppendMapsEmptyIDsToOneFixedStream(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	if err := s.Append(toolEvent("", "", "x", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	metas, err := s.List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("List = %+v, %v; want one stream", metas, err)
	}
	if metas[0].SandboxID != unattributed || metas[0].SessionID != unattributed {
		t.Fatalf("ids = %q/%q, want %q for both", metas[0].SandboxID, metas[0].SessionID, unattributed)
	}
}

func TestAppendBoundsEventsBytesAndStreams(t *testing.T) {
	t.Run("events per stream", func(t *testing.T) {
		s := newTestStore(t, StoreConfig{MaxEvents: 5})
		for i := 1; i <= 20; i++ {
			if err := s.Append(toolEvent("sbx", "sess", fmt.Sprintf("r%d", i), uint64(i))); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		got, err := s.Tail("sbx", "sess", 0)
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("retained %d events, want the 5 newest", len(got))
		}
		if sum := got[0].(ToolEnd).ResultSummary; sum != "r16" {
			t.Fatalf("oldest retained = %q, want r16 (drop-oldest)", sum)
		}
	})
	t.Run("bytes per stream", func(t *testing.T) {
		s := newTestStore(t, StoreConfig{MaxBytes: 2048})
		for i := 1; i <= 40; i++ {
			if err := s.Append(toolEvent("sbx", "sess", strings.Repeat("z", 200), uint64(i))); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		metas, err := s.List()
		if err != nil || len(metas) != 1 {
			t.Fatalf("List = %+v, %v", metas, err)
		}
		if metas[0].Bytes > 2048 {
			t.Fatalf("stream is %d bytes, want <= 2048", metas[0].Bytes)
		}
	})
	t.Run("number of streams", func(t *testing.T) {
		s := newTestStore(t, StoreConfig{})
		for i := 0; i < maxStreams+10; i++ {
			if err := s.Append(toolEvent("sbx", fmt.Sprintf("sess-%d", i), "x", 1)); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		metas, err := s.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(metas) > maxStreams {
			t.Fatalf("retained %d streams, want <= %d", len(metas), maxStreams)
		}
	})
}

func TestRedactionCanaryIsRealSecretShape(t *testing.T) {
	if redactText(canaryAWSKey) == canaryAWSKey {
		t.Fatalf("canary %q is not matched by any pattern — every redaction assertion would pass vacuously", canaryAWSKey)
	}
}

func TestRedactTextScrubsKnownSecretShapesAndLeavesProseAlone(t *testing.T) {
	secrets := map[string]string{
		"aws":        "export AWS_ACCESS_KEY_ID=" + canaryAWSKey,
		"github":     "token: ghp_1234567890abcdefghijklmnopqrstuvwxyz",
		"slack":      "posted with xoxb-1234567890-abcdefghijklmnop",
		"openai":     "sk-abcdefghijklmnopqrstuvwxyz012345",
		"pem":        "-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
		"assignment": `api_key = "abcdefghijklmnop12345"`,
	}
	for name, in := range secrets {
		out := redactText(in)
		if !strings.Contains(out, redactionMarker) {
			t.Errorf("%s: redactText(%q) = %q, want a %s", name, in, out, redactionMarker)
		}
	}
	for _, ok := range []string{"", "ran ls -l in /tmp and read 42 files", "model=claude-opus-5 tokens=1234"} {
		if got := redactText(ok); got != ok {
			t.Errorf("redactText(%q) = %q, want it unchanged", ok, got)
		}
	}
}

// TestAppendPersistsRedactedBytesOnly is the end-to-end security property:
// the canary must never reach the file, in ANY free-text field, including
// the whole raw line of an unknown kind.
func TestAppendPersistsRedactedBytesOnly(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	events := []Event{
		toolEvent("sbx", "sess", "leaked "+canaryAWSKey, 1),
		ToolStart{env: env{Kind: KindToolStart, SandboxID: "sbx", SessionID: "sess"}, ArgsSummary: "env | grep " + canaryAWSKey},
		ContextEvent{env: env{Kind: KindContextEvent, SandboxID: "sbx", SessionID: "sess"}, Detail: canaryAWSKey},
		ProviderResponse{env: env{Kind: KindProviderResponse, SandboxID: "sbx", SessionID: "sess"}, TextPreview: canaryAWSKey},
		ProviderRequest{
			env:     env{Kind: KindProviderRequest, SandboxID: "sbx", SessionID: "sess"},
			Summary: RequestSummary{NewMessages: []MessageSummary{{Role: "user", Preview: canaryAWSKey}}},
		},
	}
	unknown, err := Decode([]byte(`{"kind":"future","sandboxId":"sbx","sessionId":"sess","detail":"` + canaryAWSKey + `"}`))
	if err != nil {
		t.Fatalf("Decode unknown: %v", err)
	}
	for _, e := range append(events, unknown) {
		if err := s.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(s.cfg.Root, "sbx=sess", eventsFile))
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if strings.Contains(string(raw), canaryAWSKey) {
		t.Fatalf("stream file contains the unredacted canary:\n%s", raw)
	}
	if !strings.Contains(string(raw), redactionMarker) {
		t.Fatalf("stream file has no %s marker, so nothing was scrubbed:\n%s", redactionMarker, raw)
	}
	// The unknown kind must still be RETAINED (forward compatibility), just
	// scrubbed, and a reader must get it back.
	got, err := s.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("Tail returned %d events, want all 6 including the unknown kind", len(got))
	}
	if _, ok := got[5].(UnknownEvent); !ok {
		t.Fatalf("last event is %T, want UnknownEvent", got[5])
	}
}

func TestTailSkipsCorruptLinesInsteadOfFailing(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	if err := s.Append(toolEvent("sbx", "sess", "good", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	path := filepath.Join(s.cfg.Root, "sbx=sess", eventsFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("{not json\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	got, err := s.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Tail returned %d events, want the 1 decodable one", len(got))
	}
}

// ─── blobs ──────────────────────────────────────────────────────────────────

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func readStoredBlobs(t *testing.T, s *Store) []storedBlob {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.cfg.Root, "blobs.ndjson"))
	if err != nil {
		t.Fatalf("read blobs: %v", err)
	}
	var out []storedBlob
	for _, line := range splitLines(raw) {
		var b storedBlob
		if err := json.Unmarshal(line, &b); err != nil {
			t.Fatalf("decode blob line: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// TestAppendBlobStoredContentMatchesItsOwnAccounting is the stored
// content/hash consistency regression: Bytes is always len(Text), and the
// ONLY way Text stops being the preimage of Hash is redaction, which the
// record must declare.
func TestAppendBlobStoredContentMatchesItsOwnAccounting(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	clean := "the quick brown fox"
	secret := "AWS_ACCESS_KEY_ID=" + canaryAWSKey
	for _, text := range []string{clean, secret} {
		ok, err := s.AppendBlob(hashOf(text), text)
		if err != nil || !ok {
			t.Fatalf("AppendBlob(%q) = %v, %v; want stored", text, ok, err)
		}
	}
	blobs := readStoredBlobs(t, s)
	if len(blobs) != 2 {
		t.Fatalf("stored %d blobs, want 2", len(blobs))
	}
	for _, b := range blobs {
		if b.Bytes != len(b.Text) {
			t.Errorf("blob %s: Bytes=%d but len(Text)=%d", b.Hash, b.Bytes, len(b.Text))
		}
		if !b.Redacted && hashOf(b.Text) != b.Hash {
			t.Errorf("blob %s claims Redacted=false but sha256(Text)=%s", b.Hash, hashOf(b.Text))
		}
	}
	if blobs[0].Redacted || blobs[0].Text != clean {
		t.Errorf("clean blob = %+v, want stored verbatim and not marked redacted", blobs[0])
	}
	if !blobs[1].Redacted {
		t.Errorf("secret blob = %+v, want Redacted=true", blobs[1])
	}
	if strings.Contains(blobs[1].Text, canaryAWSKey) {
		t.Errorf("secret blob kept the canary: %q", blobs[1].Text)
	}
	if blobs[1].Hash != hashOf(secret) {
		t.Errorf("blob hash = %s, want the ORIGINAL hash %s so events can still reference it", blobs[1].Hash, hashOf(secret))
	}
}

func TestAppendBlobRejectsHashMismatchAndWritesNothing(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	for name, hash := range map[string]string{
		"wrong hash": hashOf("something else"),
		"empty hash": "",
		"not hex":    "../../../etc/passwd",
	} {
		ok, err := s.AppendBlob(hash, "payload")
		if ok || err != nil {
			t.Errorf("%s: AppendBlob = %v, %v; want (false, nil)", name, ok, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Root, "blobs.ndjson")); !os.IsNotExist(err) {
		t.Fatalf("blobs file exists after only-rejected puts (err=%v)", err)
	}
}

func TestAppendBlobIsBounded(t *testing.T) {
	s := newTestStore(t, StoreConfig{MaxEvents: 4})
	for i := 0; i < 12; i++ {
		text := fmt.Sprintf("payload-%d", i)
		if ok, err := s.AppendBlob(hashOf(text), text); err != nil || !ok {
			t.Fatalf("AppendBlob: %v, %v", ok, err)
		}
	}
	if got := len(readStoredBlobs(t, s)); got != 4 {
		t.Fatalf("retained %d blobs, want 4 (drop-oldest)", got)
	}
}

// ─── filesystem safety ──────────────────────────────────────────────────────

func TestStoredFilesAre0700DirsAnd0600Files(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	if err := s.Append(toolEvent("sbx", "sess", "x", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if ok, err := s.AppendBlob(hashOf("b"), "b"); err != nil || !ok {
		t.Fatalf("AppendBlob: %v, %v", ok, err)
	}
	dirs := []string{s.cfg.Root, filepath.Join(s.cfg.Root, "sbx=sess")}
	files := []string{filepath.Join(s.cfg.Root, "sbx=sess", eventsFile), filepath.Join(s.cfg.Root, "blobs.ndjson")}
	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s perms = %o, want 0700", d, fi.Mode().Perm())
		}
	}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perms = %o, want 0600", f, fi.Mode().Perm())
		}
	}
}

func TestLooseModesAreTightenedOnUse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "monitor")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := newTestStore(t, StoreConfig{Root: root})
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("root perms = %o after NewStore, want tightened to 0700", fi.Mode().Perm())
	}
	path := filepath.Join(s.cfg.Root, "loose.ndjson")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := openAppend0600(path)
	if err != nil {
		t.Fatalf("openAppend0600: %v", err)
	}
	f.Close()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file perms = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
}

// A symlink planted at any path this package writes must be REFUSED, never
// followed — otherwise a capture could be redirected over an arbitrary file.
func TestWritesRefuseToFollowASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("victim\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ensureDir0700(link); err == nil {
		t.Error("ensureDir0700(symlink) = nil error, want refusal")
	}
	if _, err := openAppend0600(link); err == nil {
		t.Error("openAppend0600(symlink) = nil error, want refusal")
	}
	if err := writeFileAtomic0600(link, []byte("overwritten")); err == nil {
		t.Error("writeFileAtomic0600(symlink) = nil error, want refusal")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "victim\n" {
		t.Fatalf("symlink target was modified: %q", got)
	}
}

// A store whose stream directory is a symlink must not be appendable either
// (the check runs on the directory this package is about to create).
func TestAppendRefusesSymlinkedStreamDir(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(s.cfg.Root, "sbx=sess")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := s.Append(toolEvent("sbx", "sess", "x", 1)); err == nil {
		t.Fatal("Append into a symlinked stream dir = nil error, want refusal")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("wrote %d entries through the symlink", len(entries))
	}
}
