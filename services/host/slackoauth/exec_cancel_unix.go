//go:build !windows

package slackoauth

import (
	"os/exec"
	"syscall"
	"time"
)

// configureCommandCancellation puts the command and any descendants in their
// own process group. CommandContext otherwise kills only the direct child; a
// grandchild can keep stdout/stderr pipes open and make Wait block until that
// grandchild exits. Killing the group closes the whole command tree promptly.
func configureCommandCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Defense in depth if a platform or child escapes the process group: stop
	// waiting on inherited pipes rather than hanging a Slack tool indefinitely.
	cmd.WaitDelay = 500 * time.Millisecond
}
