package slackoauth

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ExecRunner is the production CommandRunner: it shells out to the real `op`
// CLI via exec.CommandContext, so ctx cancellation/deadline kills the
// subprocess instead of leaking it. stdin (the credential JSON) is piped to
// the child's standard input only — it is NEVER placed on the command line,
// and it is NEVER included in a returned error. A returned error carries only
// the command name/args (item/vault/title — never a secret) and the child's
// own stderr text.
type ExecRunner struct{}

// Run executes name with args, writing stdin to the child's standard input
// (skipped entirely when empty, so a child that never reads stdin is never
// blocked waiting for EOF that would arrive anyway) and returns its captured
// standard output.
func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}
