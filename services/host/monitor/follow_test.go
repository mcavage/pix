package monitor

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func TestFollowPrintsConciseLinesForStoredEvents(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	if err := store.Append(toolEvent("sbx", "sess", "ls -l", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	out := followUntil(t, store, FollowConfig{}, "tool_end")
	if !strings.Contains(out, "sbx/sess") {
		t.Errorf("output %q, want the stream label", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("non-TTY output %q must carry no ANSI escapes", out)
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

func TestFollowJSONModeEmitsTheStoredEventVerbatim(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	if err := store.Append(toolEvent("sbx", "sess", "x", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	out := followUntil(t, store, FollowConfig{JSON: true}, `"kind":"tool_end"`)
	if !strings.Contains(out, `"sandboxId":"sbx"`) {
		t.Errorf("json output %q, want the raw stored event", out)
	}
}

func TestFollowFilterSelectsStreams(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	if err := store.Append(toolEvent("keep-me", "sess", "kept", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(toolEvent("other", "sess", "dropped", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	out := followUntil(t, store, FollowConfig{Filter: "keep"}, "keep-me/sess")
	if strings.Contains(out, "other/sess") {
		t.Errorf("output %q includes a non-matching stream", out)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0B", 512: "512B", 1024: "1.0KB", 1536: "1.5KB", 5 << 20: "5.0MB"}
	for n, want := range cases {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
