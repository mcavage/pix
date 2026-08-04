package monitor

import "fmt"

// HumanBytes renders a byte count in the largest unit that keeps it under
// 1024. Used by the concise reader (cmd/pix/monitor.go) and by
// workflow/doctor's status renderer — a second copy in either place is how
// two renderers come to disagree about what "1.0MB" means, so it lives here
// (this package's only remaining formatting-adjacent export) rather than
// being duplicated. It previously lived in the now-deleted
// services/host/monitor/tui package (bytes.go), which existed only to feed
// the bubbletea TUI; this is the same function, moved.
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
