//go:build windows

package slackoauth

import (
	"os/exec"
	"time"
)

// Windows CommandContext terminates the direct process. WaitDelay bounds any
// remaining wait on pipes inherited by descendants.
func configureCommandCancellation(cmd *exec.Cmd) {
	cmd.WaitDelay = 500 * time.Millisecond
}
