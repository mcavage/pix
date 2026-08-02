package tui

import "fmt"

// HumanBytes renders a byte count in the largest unit that keeps it under 1024.
//
// It lives here rather than in cmd/pix because two packages now need it and a
// second copy is how two renderers come to disagree about what "1.0MB" means.
// If a third caller appears, move it to a shared format package rather than
// copying it again.
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
