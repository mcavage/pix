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
// bytes is what these tests prove. TestRedactTextScrubsSecretShapes keeps it
// honest — if the canary stopped matching a pattern, every "redaction
// worked" assertion below would pass for the wrong reason.
const canaryAWSKey = "AKIAABCDEFGHIJKLMNOP"

// canaryGoogleKey, canaryJWT and canaryBearer extend the same trick to the
// Google AIza, JWT and Authorization: Bearer shapes. All are synthetic.
const (
	canaryGoogleKey = "AIzaSyCanary0123456789_abcdefghijklmnoq"
	canaryJWT       = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjYW5hcnkifQ.c2lnLWNhbmFyeQ"
	canaryBearer    = "Authorization: Bearer canary.bearer-token-0123456789"
)

// allCanaries is every secret-shaped canary the redaction tests plant.
var allCanaries = []string{canaryAWSKey, canaryGoogleKey, canaryJWT, canaryBearer}

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

func streamFile(s *Store, sandboxID, sessionID string) string {
	return filepath.Join(s.cfg.Root, sandboxID+idSep+sessionID, eventsFile)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// mustAppend, mustTail and mustList keep the store's error contract out of
// every assertion: these calls must not fail, and a failure is fatal here,
// not three lines at each call site.
func mustAppend(t *testing.T, s *Store, e Event) {
	t.Helper()
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func mustTail(t *testing.T, s *Store, sandboxID, sessionID string, n int) []Event {
	t.Helper()
	got, err := s.Tail(sandboxID, sessionID, n)
	if err != nil {
		t.Fatalf("Tail(%s/%s): %v", sandboxID, sessionID, err)
	}
	return got
}

func mustList(t *testing.T, s *Store) []StreamMeta {
	t.Helper()
	metas, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return metas
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
		mustAppend(t, s, toolEvent("sbx-1", "sess-1", fmt.Sprintf("r%d", i), uint64(i)))
	}
	mustAppend(t, s, toolEvent("sbx-2", "sess-2", "other", 1))

	got := mustTail(t, s, "sbx-1", "sess-1", 0)
	if len(got) != 3 || got[2].(ToolEnd).ResultSummary != "r3" {
		t.Fatalf("Tail = %+v, want 3 events, oldest-first", got)
	}
	newest := mustTail(t, s, "sbx-1", "sess-1", 2)
	if len(newest) != 2 || newest[0].(ToolEnd).ResultSummary != "r2" {
		t.Fatalf("Tail(2) = %+v, want the newest two", newest)
	}
	if empty, err := s.Tail("sbx-1", "never-seen", 0); err != nil || len(empty) != 0 {
		t.Fatalf("Tail(unknown stream) = %v, %v; want empty and no error", empty, err)
	}

	metas := mustList(t, s)
	seen := map[string]bool{}
	for _, m := range metas {
		seen[m.SandboxID+"/"+m.SessionID] = true
	}
	if len(metas) != 2 || !seen["sbx-1/sess-1"] || !seen["sbx-2/sess-2"] {
		t.Fatalf("List ids = %v, want the original (sandbox, session) pairs", seen)
	}
}

// TestAppendRejectsInvalidIDsStrictly is the strict-name regression: a
// traversal, a separator, a control byte, a dotfile, an over-long id, or the
// stream separator itself must be REFUSED — never slugified into some
// neighbouring directory — and nothing may be written outside the root. An
// EMPTY id is the one non-conforming value that is not hostile (the tap
// sends sandboxId "" outside a sandbox): it maps to one fixed constant.
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
	t.Run("empty ids", func(t *testing.T) {
		s := newTestStore(t, StoreConfig{})
		mustAppend(t, s, toolEvent("", "", "x", 1))
		metas := mustList(t, s)
		if len(metas) != 1 {
			t.Fatalf("List = %+v, want one stream", metas)
		}
		if metas[0].SandboxID != unattributed || metas[0].SessionID != unattributed {
			t.Fatalf("ids = %q/%q, want %q for both", metas[0].SandboxID, metas[0].SessionID, unattributed)
		}
	})
}

func TestAppendBoundsEventsBytesAndStreams(t *testing.T) {
	t.Run("events per stream", func(t *testing.T) {
		s := newTestStore(t, StoreConfig{MaxEvents: 5})
		for i := 1; i <= 20; i++ {
			mustAppend(t, s, toolEvent("sbx", "sess", fmt.Sprintf("r%d", i), uint64(i)))
		}
		got := mustTail(t, s, "sbx", "sess", 0)
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
			mustAppend(t, s, toolEvent("sbx", "sess", strings.Repeat("z", 200), uint64(i)))
		}
		if n := fileSize(t, streamFile(s, "sbx", "sess")); n > 2048 {
			t.Fatalf("stream is %d bytes, want <= 2048", n)
		}
	})
	t.Run("number of streams", func(t *testing.T) {
		s := newTestStore(t, StoreConfig{})
		for i := 0; i < maxStreams+10; i++ {
			mustAppend(t, s, toolEvent("sbx", fmt.Sprintf("sess-%d", i), "x", 1))
		}
		metas := mustList(t, s)
		if len(metas) > maxStreams {
			t.Fatalf("retained %d streams, want <= %d", len(metas), maxStreams)
		}
	})
}

func TestRedactTextScrubsSecretShapes(t *testing.T) {
	for _, c := range allCanaries {
		if redactText(c) == c {
			t.Fatalf("canary %q is not matched by any pattern — every redaction assertion would pass vacuously", c)
		}
	}
	secrets := map[string]string{
		"aws":        "export AWS_ACCESS_KEY_ID=" + canaryAWSKey,
		"github":     "token: ghp_1234567890abcdefghijklmnopqrstuvwxyz",
		"slack":      "posted with xoxb-1234567890-abcdefghijklmnop",
		"openai":     "sk-abcdefghijklmnopqrstuvwxyz012345",
		"google":     "called the maps api with " + canaryGoogleKey,
		"jwt":        "session cookie " + canaryJWT,
		"bearer":     "-H '" + canaryBearer + "'",
		"pem":        "-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
		"assignment": `api_key = "abcdefghijklmnop12345"`,
	}
	for name, in := range secrets {
		if out := redactText(in); !strings.Contains(out, redactionMarker) {
			t.Errorf("%s: redactText(%q) = %q, want a %s", name, in, out, redactionMarker)
		}
	}
	ordinary := []string{
		"",
		"ran ls -l in /tmp and read 42 files",
		"model=claude-opus-5 tokens=1234",
		"the bearer of good news brought authorization for the plan",
		"Authorization headers were discussed in the design review",
		"GET https://api.example.com/v1/users?page=2 returned 200",
		`{"maxTokens":1234567890123456,"seq":9}`,
		`{"tokens": 99998888777766554433, "ts":1700000000123}`,
	}
	for _, ok := range ordinary {
		if got := redactText(ok); got != ok {
			t.Errorf("redactText(%q) = %q, want it unchanged", ok, got)
		}
	}
}

// TestAppendPersistsRedactedBytesOnly is the end-to-end security property:
// the canary must never reach the file, in ANY free-text field, including
// the whole raw line of an unknown kind — which must still be RETAINED
// (forward compatibility), just scrubbed.
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
		TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx", SessionID: "sess"}, Model: canaryAWSKey},
	}
	unknown, err := Decode([]byte(`{"kind":"future","sandboxId":"sbx","sessionId":"sess","detail":"` + canaryAWSKey + `"}`))
	if err != nil {
		t.Fatalf("Decode unknown: %v", err)
	}
	for _, e := range append(events, unknown) {
		mustAppend(t, s, e)
	}
	raw, err := os.ReadFile(streamFile(s, "sbx", "sess"))
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if strings.Contains(string(raw), canaryAWSKey) {
		t.Fatalf("stream file contains the unredacted canary:\n%s", raw)
	}
	if !strings.Contains(string(raw), redactionMarker) {
		t.Fatalf("stream file has no %s marker, so nothing was scrubbed:\n%s", redactionMarker, raw)
	}
	got := mustTail(t, s, "sbx", "sess", 0)
	if len(got) != len(events)+1 {
		t.Fatalf("Tail returned %d events, want all %d including the unknown kind", len(got), len(events)+1)
	}
	if _, ok := got[len(got)-1].(UnknownEvent); !ok {
		t.Fatalf("last event is %T, want UnknownEvent", got[len(got)-1])
	}
}

// TestUnknownNumericTokenRoundTrip pins the R1-2 regression: an unknown
// kind whose raw JSON holds a secret-named key with an UNQUOTED numeric
// value must survive Append -> disk -> Tail. The scrub replaces the number
// with a QUOTED marker; a bare marker would make Encode fail (the event is
// lost) or leave an undecodable line (Tail skips it) — either way readback
// returns nothing, which is what this test refuses.
func TestUnknownNumericTokenRoundTrip(t *testing.T) {
	const numericCanary = "31337133713371337"
	s := newTestStore(t, StoreConfig{})
	line := `{"kind":"tap_v99","sandboxId":"sbx","sessionId":"sess",` +
		`"token": ` + numericCanary + `,"password":"` + numericCanary + `","kept":42}`
	ev, err := Decode([]byte(line))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	mustAppend(t, s, ev)
	raw, err := os.ReadFile(streamFile(s, "sbx", "sess"))
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if strings.Contains(string(raw), numericCanary) {
		t.Fatalf("stream file contains the numeric canary:\n%s", raw)
	}
	got := mustTail(t, s, "sbx", "sess", 0)
	if len(got) != 1 {
		t.Fatalf("Tail returned %d events, want 1 — 0 means redaction corrupted or dropped the line", len(got))
	}
	u, ok := got[0].(UnknownEvent)
	if !ok {
		t.Fatalf("Tail returned %T, want UnknownEvent", got[0])
	}
	var m map[string]any
	if err := json.Unmarshal(u.Raw, &m); err != nil {
		t.Fatalf("read-back raw is not valid JSON: %v\n%s", err, u.Raw)
	}
	if m["token"] != redactionMarker || m["password"] != redactionMarker {
		t.Errorf("token/password = %v/%v, want both the %q string", m["token"], m["password"], redactionMarker)
	}
	if m["kept"] != float64(42) {
		t.Errorf("kept = %v, want 42 retained", m["kept"])
	}
}

func TestTailSkipsCorruptLinesInsteadOfFailing(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	mustAppend(t, s, toolEvent("sbx", "sess", "good", 1))
	f, err := os.OpenFile(streamFile(s, "sbx", "sess"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("{not json\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	got := mustTail(t, s, "sbx", "sess", 0)
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
	clean, secret := "the quick brown fox", "AWS_ACCESS_KEY_ID="+canaryAWSKey
	for _, text := range []string{clean, secret} {
		if ok, err := s.AppendBlob(hashOf(text), text); err != nil || !ok {
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
	if !blobs[1].Redacted || strings.Contains(blobs[1].Text, canaryAWSKey) {
		t.Errorf("secret blob = %+v, want Redacted=true and no canary", blobs[1])
	}
	if blobs[1].Hash != hashOf(secret) {
		t.Errorf("blob hash = %s, want the ORIGINAL hash %s so events can still reference it", blobs[1].Hash, hashOf(secret))
	}
}

func TestAppendBlobRejectsHashMismatchAndStaysBounded(t *testing.T) {
	s := newTestStore(t, StoreConfig{MaxEvents: 4})
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

func TestStoredFilesAre0700DirsAnd0600FilesAndLooseModesAreTightened(t *testing.T) {
	root := filepath.Join(t.TempDir(), "monitor")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := newTestStore(t, StoreConfig{Root: root}) // NewStore must tighten 0755 -> 0700
	mustAppend(t, s, toolEvent("sbx", "sess", "x", 1))
	if ok, err := s.AppendBlob(hashOf("b"), "b"); err != nil || !ok {
		t.Fatalf("AppendBlob: %v, %v", ok, err)
	}
	loose := filepath.Join(root, "loose.ndjson")
	if err := os.WriteFile(loose, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := openAppend0600(loose)
	if err != nil {
		t.Fatalf("openAppend0600: %v", err)
	}
	f.Close()

	want := map[string]os.FileMode{
		root:                                    0o700,
		filepath.Join(root, "sbx"+idSep+"sess"): 0o700,
		streamFile(s, "sbx", "sess"):            0o600,
		filepath.Join(root, "blobs.ndjson"):     0o600,
		loose:                                   0o600,
	}
	for path, mode := range want {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if fi.Mode().Perm() != mode {
			t.Errorf("%s perms = %o, want %o", path, fi.Mode().Perm(), mode)
		}
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
	if got, err := os.ReadFile(target); err != nil || string(got) != "victim\n" {
		t.Fatalf("symlink target = %q (err %v), want it untouched", got, err)
	}

	// The same refusal must hold through Append, whose stream directory is
	// the path an attacker would plant.
	s := newTestStore(t, StoreConfig{})
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(s.cfg.Root, "sbx"+idSep+"sess")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := s.Append(toolEvent("sbx", "sess", "x", 1)); err == nil {
		t.Fatal("Append into a symlinked stream dir = nil error, want refusal")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("wrote %d entries through the symlink", len(entries))
	}
}
