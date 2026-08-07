package monitor

// follow.go is the reader half: poll the store, print each new event as one
// concise line (or its raw stored JSON). It lives here, not in cmd/pix, so
// the CLI stays argv parsing plus wiring and the printed form is testable
// without a process.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// FollowConfig configures Follow. Filter keeps only streams whose sandbox or
// session id contains it (case-sensitive). JSON prints the raw stored event;
// TTY adds ANSI emphasis to the kind token and NOTHING else, so a piped run
// and an interactive run of one capture stay diffable.
type FollowConfig struct {
	Filter string
	JSON   bool
	TTY    bool
	Out    io.Writer
}

// emitNew lists store's streams and prints whatever comes after cursors for
// each one it keeps (cfg.Filter applied), advancing cursors in place. It is
// the one pass both Follow (repeatedly, tolerating a transient error) and
// Once (a single time, surfacing that same error) build on, so the two never
// drift on what "new" means.
//
// cursors tracks, per stream directory, the canonical wire bytes of the LAST
// event this loop has printed. A byte identity survives the store's own
// drop-oldest trim() (an atomic rename to a shorter file) and any external
// truncate/rotate of the file underneath it, which a plain "count already
// printed" cursor does not: once the file shrinks, an index-based cursor
// either re-prints everything before it (duplicates) or silently adopts the
// new, shorter length as "already printed" and never emits the events that
// are actually new (drops). Anchoring on the last event's own bytes instead
// means every poll re-locates that exact event in the current file —
// wherever it now sits, or its absence — and only the events strictly after
// it (by position) are new.
func emitNew(store *Store, cfg FollowConfig, cursors map[string]string) error {
	metas, err := store.List()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range metas {
		seen[m.Dir] = true
		if f := cfg.Filter; f != "" && !strings.Contains(m.SandboxID, f) && !strings.Contains(m.SessionID, f) {
			continue
		}
		events, err := store.Tail(m.SandboxID, m.SessionID, 0)
		if err != nil {
			continue
		}
		lines := make([]string, len(events))
		for i, e := range events {
			line, err := Encode(e)
			if err != nil {
				continue
			}
			lines[i] = string(line)
		}
		start := 0
		if last, ok := cursors[m.Dir]; ok {
			// Anchor found: only what comes after it is new. Anchor gone
			// (evicted by trim, or the file was replaced outright): every
			// event now present was appended after our anchor — drop-oldest
			// is the only way lines disappear, so nothing left can predate
			// what we already printed — so all of it is new, start stays 0.
			for i := len(lines) - 1; i >= 0; i-- {
				if lines[i] == last {
					start = i + 1
					break
				}
			}
		}
		for i := start; i < len(events); i++ {
			e := events[i]
			if !cfg.JSON {
				fmt.Fprintln(cfg.Out, concise(e, cfg.TTY))
			} else if lines[i] != "" {
				fmt.Fprintln(cfg.Out, lines[i])
			}
		}
		// Advance the anchor to the LAST successfully encoded line, not simply
		// the last event: an Encode failure (e.g. an unknown-kind event whose
		// raw bytes decodeUnknown had to truncate to its cap, leaving them no
		// longer valid JSON) leaves lines[len(lines)-1] == "", and anchoring on
		// that empty string is a double bug — it can never re-match a real
		// line on the next poll (a false miss forces start back to 0, i.e. a
		// full re-print/duplicate of everything already emitted), and if two
		// unrelated streams both hit it, both empty cursors read as "already
		// seen" against each other on the very next poll below. Skipping
		// unencodable lines here means the anchor is always either absent or a
		// real, re-locatable line, so an unencodable event can only ever cost
		// itself a possible re-emit on the next poll — the events after it are
		// unaffected either way, per the anchor loop above.
		for i := len(lines) - 1; i >= 0; i-- {
			if lines[i] != "" {
				cursors[m.Dir] = lines[i]
				break
			}
		}
	}
	// A stream that fell out of List() (evicted for good) can never come back
	// under the same directory, so its cursor is dead weight; drop it rather
	// than let a long session accumulate one entry per churned short-lived
	// stream forever.
	for dir := range cursors {
		if !seen[dir] {
			delete(cursors, dir)
		}
	}
	return nil
}

// Once prints whatever is already stored, exactly once, and returns — the
// one-shot half of the reader (`pix monitor --json | head -5` must
// terminate). Unlike Follow it does NOT tolerate a List failure: there is no
// next poll to self-correct on, so the caller needs to know.
func Once(store *Store, cfg FollowConfig) error {
	return emitNew(store, cfg, map[string]string{})
}

// Follow prints every stored event, then each new one until ctx is done.
// Read errors are transient by nature (a file trimmed under us, or a stream
// briefly missing mid-rotation), so — unlike Once — they are skipped here,
// not fatal: the next poll tries again.
func Follow(ctx context.Context, store *Store, cfg FollowConfig) {
	const pollInterval = 150 * time.Millisecond
	cursors := map[string]string{}
	poll := func() { _ = emitNew(store, cfg, cursors) }
	poll() // whatever is already there, before the first sleep
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
			poll()
		}
	}
}

// concise renders one event as a single readable line.
func concise(e Event, tty bool) string {
	env := e.Envelope()
	kind, detail := string(env.Kind), ""
	switch v := e.(type) {
	case TurnStart:
		detail = fmt.Sprintf("model=%s trigger=%s", v.Model, v.Trigger)
	case ProviderRequest:
		kind = "request"
		detail = fmt.Sprintf("model=%s msgs=+%d tools=%d ~%dtok",
			v.Model, len(v.Summary.NewMessages), v.Summary.ToolCount, v.Summary.EstTokens)
	case ProviderResponse:
		kind = "response"
		usage := ""
		if v.Usage != nil {
			usage = fmt.Sprintf(" in=%d out=%d", v.Usage.InputTokens, v.Usage.OutputTokens)
		}
		detail = fmt.Sprintf("status=%d stop=%s%s", v.Status, v.StopReason, usage)
	case ToolStart:
		kind = "tool"
		detail = fmt.Sprintf("%s (%s) %s", v.Name, v.Source, v.ArgsSummary)
	case ToolEnd:
		kind = "tool_end"
		ok := "ok"
		if !v.OK {
			ok = "FAIL"
		}
		detail = fmt.Sprintf("%s %s %dms", ok, humanBytes(int64(v.ResultBytes)), v.DurationMs)
	case ContextEvent:
		kind = "ctx"
		detail = fmt.Sprintf("%s %s", v.CtxKind, v.Detail)
	case UnknownEvent:
		detail = "(unrecognized kind)"
	}
	label := env.SandboxID
	if env.SessionID != "" {
		label += "/" + env.SessionID
	}
	if tty {
		kind = "\x1b[1m" + kind + "\x1b[0m"
	}
	return fmt.Sprintf("%s %s %s %s", time.UnixMilli(env.TS).UTC().Format("15:04:05"), label, kind, detail)
}

// humanBytes renders a byte count in the largest unit under 1024.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
