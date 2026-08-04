package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T, cfg StoreConfig) *Store {
	t.Helper()
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func toolEvent(sandboxID, sessionID, turnID, toolID string) Event {
	return ToolStart{
		env:         Envelope{Kind: KindToolStart, SandboxID: sandboxID, SessionID: sessionID, TurnID: turnID, TS: 1},
		ToolID:      toolID,
		Source:      "builtin",
		Name:        "bash",
		ArgsSummary: "go test ./...",
	}
}

func TestStoreAppendThenTailRoundTrips(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	if err := s.Append(toolEvent("sbx", "sess", "1", "t1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(toolEvent("sbx", "sess", "1", "t2")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := s.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Tail() returned %d events, want 2", len(got))
	}
	ts0 := got[0].(ToolStart)
	ts1 := got[1].(ToolStart)
	if ts0.ToolID != "t1" || ts1.ToolID != "t2" {
		t.Errorf("Tail order = %q, %q, want t1, t2 (oldest-first)", ts0.ToolID, ts1.ToolID)
	}
}

func TestStoreTailNIsANewestWindow(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	for i := 0; i < 5; i++ {
		if err := s.Append(toolEvent("sbx", "sess", "1", string(rune('a'+i)))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := s.Tail("sbx", "sess", 2)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Tail(n=2) returned %d events, want 2", len(got))
	}
	if got[0].(ToolStart).ToolID != "d" || got[1].(ToolStart).ToolID != "e" {
		t.Errorf("Tail(n=2) = %v, want the newest 2 (d, e)", got)
	}
}

func TestStoreTailOfUnknownStreamReturnsEmptyNotError(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	got, err := s.Tail("never-seen", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Tail(unknown stream) = %v, want empty", got)
	}
}

func TestStoreDistinctSessionsGetDistinctStreams(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	if err := s.Append(toolEvent("sbx", "sess-a", "1", "ta")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(toolEvent("sbx", "sess-b", "1", "tb")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	a, err := s.Tail("sbx", "sess-a", 0)
	if err != nil || len(a) != 1 {
		t.Fatalf("Tail(sess-a) = %v, %v, want 1 event", a, err)
	}
	b, err := s.Tail("sbx", "sess-b", 0)
	if err != nil || len(b) != 1 {
		t.Fatalf("Tail(sess-b) = %v, %v, want 1 event", b, err)
	}
}

func TestStoreListReportsEveryStreamWithOriginalIDs(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	if err := s.Append(toolEvent("sbx-1", "sess-1", "1", "t1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(toolEvent("sbx-2", "sess-2", "1", "t2")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	metas, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("List() = %d streams, want 2", len(metas))
	}
	seen := map[string]bool{}
	for _, m := range metas {
		seen[m.SandboxID+"/"+m.SessionID] = true
		if m.Bytes <= 0 {
			t.Errorf("stream %s/%s: Bytes = %d, want > 0", m.SandboxID, m.SessionID, m.Bytes)
		}
	}
	if !seen["sbx-1/sess-1"] || !seen["sbx-2/sess-2"] {
		t.Errorf("List() = %+v, missing an expected stream", metas)
	}
}

func TestStoreAppendCapsEventsPerStream(t *testing.T) {
	s := newTestStore(t, StoreConfig{MaxEventsPerStream: 3, MaxBytesPerStream: 1 << 20})
	for i := 0; i < 10; i++ {
		if err := s.Append(toolEvent("sbx", "sess", "1", string(rune('a'+i)))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	got, err := s.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("retained %d events, want 3 (MaxEventsPerStream)", len(got))
	}
	// The three retained must be the NEWEST (drop-oldest, matching the
	// deleted Ring's eviction rule).
	want := []string{"h", "i", "j"}
	for i, w := range want {
		if got[i].(ToolStart).ToolID != w {
			t.Errorf("retained[%d] = %q, want %q (oldest evicted first)", i, got[i].(ToolStart).ToolID, w)
		}
	}
}

func TestStoreAppendCapsBytesPerStream(t *testing.T) {
	// A tiny byte budget forces eviction well before MaxEventsPerStream
	// would ever trigger it.
	s := newTestStore(t, StoreConfig{MaxEventsPerStream: 10_000, MaxBytesPerStream: 400})
	for i := 0; i < 20; i++ {
		ev := toolEvent("sbx", "sess", "1", string(rune('a'+i)))
		if err := s.Append(ev); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	path := filepath.Join(s.Root(), streamDirName("sbx", "sess"), streamEventsFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() > 400 {
		t.Errorf("stream file size = %d bytes, want <= 400 (MaxBytesPerStream)", fi.Size())
	}
	got, err := s.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) == 0 || len(got) >= 20 {
		t.Errorf("retained %d events, want some eviction to have happened (< 20, > 0)", len(got))
	}
	// Whatever's retained must be the tail end (newest), never "a" (oldest).
	if got[0].(ToolStart).ToolID == "a" {
		t.Errorf("oldest event %q survived the byte-budget trim", "a")
	}
}

func TestStoreEvictsOldestStreamWhenOverMaxStreams(t *testing.T) {
	s := newTestStore(t, StoreConfig{MaxStreams: 2})
	if err := s.Append(toolEvent("sbx", "sess-1", "1", "t1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(toolEvent("sbx", "sess-2", "1", "t2")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(toolEvent("sbx", "sess-3", "1", "t3")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	metas, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("List() = %d streams, want 2 (MaxStreams, oldest evicted)", len(metas))
	}
	got, err := s.Tail("sbx", "sess-1", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("sess-1 (oldest) should have been evicted entirely, still has %d events", len(got))
	}
}

// TestStoreAppendPersistsRedactedNotRawText is the end-to-end canary test:
// a secret planted in an event field must never reach the file on disk,
// even though it decodes back out fine (as the redaction marker).
func TestStoreAppendPersistsRedactedNotRawText(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	ev := ToolStart{
		env:         Envelope{Kind: KindToolStart, SandboxID: "sbx", SessionID: "sess", TurnID: "1"},
		ToolID:      "t1",
		ArgsSummary: "export AWS_ACCESS_KEY_ID=" + canaryAWSKey,
	}
	if err := s.Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	path := filepath.Join(s.Root(), streamDirName("sbx", "sess"), streamEventsFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stream file: %v", err)
	}
	if strings.Contains(string(raw), canaryAWSKey) {
		t.Fatalf("canary secret reached disk unredacted:\n%s", raw)
	}
	got, err := s.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Tail() = %d events, want 1", len(got))
	}
	if strings.Contains(got[0].(ToolStart).ArgsSummary, canaryAWSKey) {
		t.Fatalf("Tail() returned the canary secret unredacted: %+v", got[0])
	}
}

// TestStoreAppendPersistsUnknownKindsForwardCompatibly proves an
// unrecognized event kind (a newer extensions/monitor.ts talking to an
// older host) still round-trips through Append/Tail rather than being
// silently dropped.
func TestStoreAppendPersistsUnknownKindsForwardCompatibly(t *testing.T) {
	s := newTestStore(t, StoreConfig{})
	line := []byte(`{"kind":"future_kind","sandboxId":"sbx","sessionId":"sess","turnId":"1","seq":1,"ts":1,"newField":"value"}`)
	ev, err := Decode(line)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if err := s.Append(ev); err != nil {
		t.Fatalf("Append(UnknownEvent): %v", err)
	}
	got, err := s.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Tail() = %d events, want 1", len(got))
	}
	if got[0].Kind() != "future_kind" {
		t.Errorf("Tail() kind = %q, want %q", got[0].Kind(), "future_kind")
	}
}

func TestStoreAppendUnparseableLineSkippedByTailNotFatal(t *testing.T) {
	// A future/malformed line hand-appended directly to the stream file
	// (simulating corruption or a partial write) must not break Tail for
	// the rest of the stream.
	s := newTestStore(t, StoreConfig{})
	if err := s.Append(toolEvent("sbx", "sess", "1", "t1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	path := filepath.Join(s.Root(), streamDirName("sbx", "sess"), streamEventsFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteString("not json at all\n"); err != nil {
		t.Fatalf("write corruption: %v", err)
	}
	f.Close()
	if err := s.Append(toolEvent("sbx", "sess", "1", "t2")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := s.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Tail() = %d events, want 2 (corrupted line skipped, not fatal)", len(got))
	}
}

func TestNewStoreRequiresRoot(t *testing.T) {
	if _, err := NewStore(StoreConfig{}); err == nil {
		t.Fatal("NewStore(no Root) = nil error, want a requirement error")
	}
}

func TestNewStoreCreatesRootAt0700(t *testing.T) {
	root := filepath.Join(t.TempDir(), "monitor-store")
	if _, err := NewStore(StoreConfig{Root: root}); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("store root mode = %o, want 0700", fi.Mode().Perm())
	}
}
