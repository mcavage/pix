package monitor

// follow.go is the reader half: poll the store, print each new event as one
// concise line (or its raw stored JSON). It lives here, not in cmd/pix, so
// the CLI stays argv parsing plus wiring and the printed form is testable
// against the store without a process.

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

// Follow prints every event already stored, then each new one until ctx is
// done. Read errors are transient by nature (a file trimmed under us) and
// are skipped, not fatal.
func Follow(ctx context.Context, store *Store, cfg FollowConfig) {
	const pollInterval = 150 * time.Millisecond
	printed := map[string]int{} // stream -> events already printed
	poll := func() {
		metas, err := store.List()
		if err != nil {
			return
		}
		for _, m := range metas {
			if cfg.Filter != "" && !strings.Contains(m.SandboxID, cfg.Filter) && !strings.Contains(m.SessionID, cfg.Filter) {
				continue
			}
			events, err := store.Tail(m.SandboxID, m.SessionID, 0)
			if err != nil {
				continue
			}
			already := printed[m.Dir]
			for _, e := range events[min(already, len(events)):] {
				if cfg.JSON {
					if line, err := Encode(e); err == nil {
						fmt.Fprintln(cfg.Out, string(line))
					}
					continue
				}
				fmt.Fprintln(cfg.Out, concise(e, cfg.TTY))
			}
			printed[m.Dir] = len(events)
		}
	}
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
		detail = fmt.Sprintf("%s %s %dms", ok, HumanBytes(int64(v.ResultBytes)), v.DurationMs)
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

// HumanBytes renders a byte count in the largest unit that keeps it under
// 1024. Shared with workflow/doctor and workflow/reset, because a second
// copy is how two renderers come to disagree about what "1.0MB" means.
func HumanBytes(n int64) string {
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
