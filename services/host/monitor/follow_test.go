package monitor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// settlePolls mirrors Follow's own poll interval, so a caller can wait "a few
// more polls" and be confident every pending change was actually observed,
// not just the first one that happened to satisfy an earlier waitFor.
const settlePolls = 3 * 150 * time.Millisecond

// followUntil runs Follow against store until out contains want (or the
// deadline passes), then cancels — the reader is a poll loop, so a test has
// to wait for it rather than call it once.
func followUntil(t *testing.T, store *Store, cfg FollowConfig, want string) string {
	t.Helper()
	var buf lockedBuffer
	cfg.Out = &buf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Follow(ctx, store, cfg); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	return buf.String()
}

// waitFor blocks until buf's output contains want or 2 seconds pass, then
// fails the test. Unlike followUntil it does not start or stop Follow, so a
// test can call it repeatedly against one still-running Follow instance
// while it mutates the store out from under it (append, truncate, append
// again) — exactly the sequence a rotation bug needs to reproduce.
func waitFor(t *testing.T, buf *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output %q", want, buf.String())
}

// writeRawEvents overwrites path with exactly these events, encoded and
// redacted exactly as Store.Append would, but bypassing the store entirely
// — a plain os.WriteFile truncate, not the store's own atomic-rename trim.
// This is what an external rotator (logrotate copytruncate, an operator's
// `truncate -s0`, ...) does to a file the store also owns: replace its
// content out from under a reader with no coordination at all.
func writeRawEvents(t *testing.T, path string, events ...Event) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range events {
		line, err := Encode(redact(e))
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// lockedBuffer is a bytes.Buffer safe to read while Follow writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestFollowPrintsStoredEventsConciselyOrAsRawJSON(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	mustAppend(t, store, toolEvent("sbx", "sess", "ls -l", 1))
	out := followUntil(t, store, FollowConfig{}, "tool_end")
	if !strings.Contains(out, "sbx/sess") {
		t.Errorf("output %q, want the stream label", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("non-TTY output %q must carry no ANSI escapes", out)
	}
	raw := followUntil(t, store, FollowConfig{JSON: true}, `"kind":"tool_end"`)
	if !strings.Contains(raw, `"sandboxId":"sbx"`) {
		t.Errorf("json output %q, want the raw stored event", raw)
	}
}

// TTY styling may change how a line looks but never WHAT it says, so a piped
// capture and an interactive one stay diffable.
func TestFollowTTYStylingChangesNoContent(t *testing.T) {
	e := toolEvent("sbx", "sess", "ls", 1)
	plain, styled := concise(e, false), concise(e, true)
	if !strings.Contains(styled, "\x1b[1m") {
		t.Errorf("TTY line %q, want ANSI emphasis", styled)
	}
	if stripANSI(styled) != plain {
		t.Errorf("TTY line strips to %q, want %q", stripANSI(styled), plain)
	}
}

func stripANSI(s string) string {
	return strings.NewReplacer("\x1b[1m", "", "\x1b[0m", "").Replace(s)
}

func TestFollowFilterSelectsStreams(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	mustAppend(t, store, toolEvent("keep-me", "sess", "kept", 1))
	mustAppend(t, store, toolEvent("other", "sess", "dropped", 1))
	out := followUntil(t, store, FollowConfig{Filter: "keep"}, "keep-me/sess")
	if strings.Contains(out, "other/sess") {
		t.Errorf("output %q includes a non-matching stream", out)
	}
}

// TestFollowSurvivesExternalTruncationWithoutDropOrDuplicate reproduces the
// exact bug this test guards against: a file replaced out from under a
// running Follow with FEWER, entirely unrelated events — prior printed
// count (3) strictly greater than the new file's length (1) — exactly what
// an external log rotation or a `truncate -s0` looks like. Before the fix,
// Follow's cursor was a raw event COUNT: seeing a shorter file, it silently
// set printed = len(new file) and never emitted the rotated-in event at
// all (a drop). The fix must emit it exactly once, and a subsequent append
// past the rotation point must keep working normally.
func TestFollowSurvivesExternalTruncationWithoutDropOrDuplicate(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	mustAppend(t, store, toolEvent("sbx", "sess", "r1", 1))
	mustAppend(t, store, toolEvent("sbx", "sess", "r2", 2))
	mustAppend(t, store, toolEvent("sbx", "sess", "r3", 3))

	var buf lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Follow(ctx, store, FollowConfig{JSON: true, Out: &buf}); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, &buf, `"resultSummary":"r3"`)
	if n := strings.Count(buf.String(), `"kind":"tool_end"`); n != 3 {
		t.Fatalf("printed %d events before truncation, want 3", n)
	}

	// Rotate: replace the file with ONE new event, unrelated to r1..r3, and
	// shorter than the printed count.
	file := streamFile(store, "sbx", "sess")
	writeRawEvents(t, file, toolEvent("sbx", "sess", "post-rotate-1", 1))
	waitFor(t, &buf, `"resultSummary":"post-rotate-1"`)

	// A subsequent, ordinary append (through the store, past the rotation)
	// must also be picked up — the cursor must not have wedged itself.
	mustAppend(t, store, toolEvent("sbx", "sess", "post-rotate-2", 2))
	waitFor(t, &buf, `"resultSummary":"post-rotate-2"`)

	time.Sleep(settlePolls) // let a few more idle polls run; nothing should re-print

	for _, label := range []string{"r1", "r2", "r3", "post-rotate-1", "post-rotate-2"} {
		want := fmt.Sprintf(`"resultSummary":"%s"`, label)
		if n := strings.Count(buf.String(), want); n != 1 {
			t.Errorf("event %q printed %d times, want exactly 1 (out=%q)", label, n, buf.String())
		}
	}
}

// TestFollowSurvivesStoresOwnTrimWithoutDropOrDuplicate exercises the OTHER
// place a stream shrinks: the store's own bounded, drop-oldest trim() (an
// atomic rename to a shorter file — see store.go), triggered here by a burst
// of appends that outpaces Follow's poll interval so the previously-printed
// anchor event is evicted between two polls. This is the everyday path
// (every stream at capacity hits it), not just an exceptional external
// rotation, and it must be equally drop-free and duplicate-free.
func TestFollowSurvivesStoresOwnTrimWithoutDropOrDuplicate(t *testing.T) {
	store := newTestStore(t, StoreConfig{MaxEvents: 3})
	for i := 1; i <= 3; i++ {
		mustAppend(t, store, toolEvent("sbx", "sess", fmt.Sprintf("r%d", i), uint64(i)))
	}

	var buf lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Follow(ctx, store, FollowConfig{JSON: true, Out: &buf}); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, &buf, `"resultSummary":"r3"`)

	// Burst past the cap in one shot, before Follow's next poll can see the
	// intermediate states: each append trims to the newest 3, so by the time
	// Follow polls again, r1..r3 (its printed anchor, r3, included) are gone
	// and only r4..r6 remain.
	for i := 4; i <= 6; i++ {
		mustAppend(t, store, toolEvent("sbx", "sess", fmt.Sprintf("r%d", i), uint64(i)))
	}
	waitFor(t, &buf, `"resultSummary":"r6"`)

	// The stream keeps growing past the cap, exercising the steady-state
	// trim path (anchor still present, only the newest line is new) too.
	mustAppend(t, store, toolEvent("sbx", "sess", "r7", 7))
	waitFor(t, &buf, `"resultSummary":"r7"`)

	time.Sleep(settlePolls)

	for i := 1; i <= 7; i++ {
		want := fmt.Sprintf(`"resultSummary":"r%d"`, i)
		if n := strings.Count(buf.String(), want); n != 1 {
			t.Errorf("event r%d printed %d times, want exactly 1 (out=%q)", i, n, buf.String())
		}
	}
}

// rawUnknownLineExceedingRawCap returns a WELL-FORMED NDJSON line (valid
// JSON, unrecognized "kind") whose length exceeds decodeUnknown's raw cap
// (maxFieldBytes*4). Decode succeeds — the probe unmarshal sees the whole
// valid line before any truncation happens — but decodeUnknown then copies
// only the first maxRaw bytes into the returned UnknownEvent's Raw field,
// slicing it mid-string. UnknownEvent.MarshalJSON returns Raw verbatim, so
// re-Encode-ing that decoded event feeds encoding/json bytes that are no
// longer valid JSON, and Marshal errors. This is a real, reproducible way
// for a stored event to decode fine and then fail Encode on the way back
// out — not a synthetic type built just to break the interface.
func rawUnknownLineExceedingRawCap(seq uint64) []byte {
	blob := strings.Repeat("A", maxFieldBytes*4+8192)
	return []byte(fmt.Sprintf(
		`{"kind":"future_kind","sandboxId":"sbx","sessionId":"sess","turnId":"t1","seq":%d,"ts":1700000000000,"blob":"%s"}`,
		seq, blob))
}

// writeStreamFile overwrites a stream file with exactly these already-wire
// lines, one per line, bypassing the store entirely — the low-level
// counterpart of writeRawEvents for a line this package could never Encode
// in the first place (see rawUnknownLineExceedingRawCap above).
func writeStreamFile(t *testing.T, path string, lines ...[]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestEmitNewNeverEmptiesCursorOrDuplicatesAfterAnUnencodableEvent is the
// direct, non-timing-dependent regression for the cursor bug: emitNew used
// to anchor on lines[len(lines)-1] unconditionally, so a stream whose
// newest decoded event fails Encode (see rawUnknownLineExceedingRawCap)
// set cursors[dir] = "". An empty anchor can never re-match a real line on
// the next poll, so the anchor search below falls back to start=0 — a full
// re-print of everything already emitted. The fix anchors on the LAST
// SUCCESSFULLY encoded line instead. Exercised across two direct emitNew
// calls (not Follow's poll loop) so the assertion is deterministic: no
// sleep, no timeout, no goroutine race with the writer.
func TestEmitNewNeverEmptiesCursorOrDuplicatesAfterAnUnencodableEvent(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	file := streamFile(store, "sbx", "sess")

	before, err := Encode(redact(toolEvent("sbx", "sess", "before", 1)))
	if err != nil {
		t.Fatalf("Encode(before): %v", err)
	}
	bad := rawUnknownLineExceedingRawCap(2)

	// Sanity-check the fixture actually reproduces the shape the fix must
	// tolerate: decodes fine, re-encoding what decoded fails.
	decodedBad, err := Decode(bad)
	if err != nil {
		t.Fatalf("Decode(bad) = %v, want the oversized-but-well-formed line to decode successfully", err)
	}
	if _, err := Encode(decodedBad); err == nil {
		t.Fatal("Encode(decoded bad line) = nil error, want the truncated Raw to be unencodable")
	}

	writeStreamFile(t, file, before, bad)

	cursors := map[string]string{}
	var buf lockedBuffer
	cfg := FollowConfig{JSON: true, Out: &buf}
	if err := emitNew(store, cfg, cursors); err != nil {
		t.Fatalf("emitNew (poll 1): %v", err)
	}

	dir := filepath.Dir(file)
	if got, ok := cursors[dir]; !ok || got == "" {
		t.Fatalf("cursor after poll 1 = %q (present=%v), want the last successfully encoded line, never empty", got, ok)
	}
	if n := strings.Count(buf.String(), `"resultSummary":"before"`); n != 1 {
		t.Fatalf(`poll 1 printed "before" %d times, want exactly 1 (out=%q)`, n, buf.String())
	}

	// Poll 2: the same unencodable line is still there (nothing trimmed it),
	// and one more valid event lands after it. It must be emitted exactly
	// once, and "before" must not be re-emitted now that the anchor sits
	// behind an event this package can never re-locate by content.
	after, err := Encode(redact(toolEvent("sbx", "sess", "after", 3)))
	if err != nil {
		t.Fatalf("Encode(after): %v", err)
	}
	writeStreamFile(t, file, before, bad, after)

	if err := emitNew(store, cfg, cursors); err != nil {
		t.Fatalf("emitNew (poll 2): %v", err)
	}

	out := buf.String()
	if n := strings.Count(out, `"resultSummary":"before"`); n != 1 {
		t.Errorf(`"before" printed %d times across two polls, want exactly 1 (no duplicate) (out=%q)`, n, out)
	}
	if n := strings.Count(out, `"resultSummary":"after"`); n != 1 {
		t.Errorf(`"after" printed %d times across two polls, want exactly 1 (no drop) (out=%q)`, n, out)
	}
	if got, ok := cursors[dir]; !ok || got == "" {
		t.Errorf("cursor after poll 2 = %q (present=%v), want the last successfully encoded line, never empty", got, ok)
	}

	// Concise mode must skip the same unencodable event too. Printing it without
	// a usable anchor would repeat it on every 150ms Follow poll forever.
	writeStreamFile(t, file, before, bad)
	var human lockedBuffer
	humanCursors := map[string]string{}
	humanCfg := FollowConfig{Out: &human}
	if err := emitNew(store, humanCfg, humanCursors); err != nil {
		t.Fatalf("emitNew concise poll 1: %v", err)
	}
	if err := emitNew(store, humanCfg, humanCursors); err != nil {
		t.Fatalf("emitNew concise poll 2: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(human.String()), "\n") + 1; lines != 1 {
		t.Errorf("concise output has %d lines, want only the one encodable event: %q", lines, human.String())
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0B", 512: "512B", 1024: "1.0KB", 1536: "1.5KB", 5 << 20: "5.0MB"}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestOnceReadsExistingEventsExactlyOnceAndReturns is the one-shot half of
// the reader: unlike Follow it must return on its own — no context, no
// cancellation — as soon as it has printed what's stored.
func TestOnceReadsExistingEventsExactlyOnceAndReturns(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	mustAppend(t, store, toolEvent("sbx", "sess", "one", 1))
	mustAppend(t, store, toolEvent("sbx", "sess", "two", 2))

	var buf lockedBuffer
	if err := Once(store, FollowConfig{JSON: true, Out: &buf}); err != nil {
		t.Fatalf("Once: %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, `"kind":"tool_end"`); n != 2 {
		t.Fatalf("Once printed %d events, want exactly 2 (out=%q)", n, out)
	}

	// A second call against the SAME store (no cursor carried across calls,
	// unlike Follow) reprints everything again — Once has no notion of
	// "already seen", by design: it is a snapshot, not a subscription.
	buf = lockedBuffer{}
	if err := Once(store, FollowConfig{JSON: true, Out: &buf}); err != nil {
		t.Fatalf("Once (second call): %v", err)
	}
	if n := strings.Count(buf.String(), `"kind":"tool_end"`); n != 2 {
		t.Fatalf("second Once printed %d events, want exactly 2 (out=%q)", n, buf.String())
	}
}

// TestOnceSurfacesAListError is the one behavioral difference from Follow:
// Once has no next poll to self-correct on, so a List failure must come
// back as an error instead of being swallowed as merely transient.
func TestOnceSurfacesAListError(t *testing.T) {
	root := t.TempDir()
	// A plain file where List expects a directory it can os.ReadDir: not a
	// missing root (that's the legitimate "nothing yet" case, see
	// OpenStore's tests), a genuinely broken one.
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(blocker)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	var buf lockedBuffer
	if err := Once(store, FollowConfig{JSON: true, Out: &buf}); err == nil {
		t.Fatal("Once against a root that is a plain file = nil error, want the List failure surfaced")
	}
}
