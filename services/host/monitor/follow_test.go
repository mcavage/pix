package monitor

import (
	"bytes"
	"context"
	"fmt"
	"os"
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

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0B", 512: "512B", 1024: "1.0KB", 1536: "1.5KB", 5 << 20: "5.0MB"}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
