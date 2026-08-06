// run_stdio_seams_test.go — a grep-based anti-drift sentinel (mirroring
// hostmode_gone_test.go's pattern) pinning requirement (2) of the CLI fix
// pass: run's onboarding/reconcile prompts must read/write through
// cli.Deps.In/Out, never os.Stdin/os.Stdout directly, so a test that swaps
// Deps.In/Out (e.g. to feed a scripted answer or capture a prompt) actually
// observes what the command does. The ONE exception is the real `sbx` exec
// handoff at the end of Run, which must inherit the process's actual
// terminal — that line is named explicitly below and is the only os.Stdin/
// os.Stdout reference this file allows.
package main

import (
	"os"
	"strings"
	"testing"
)

func TestRunCmd_OnlyExecHandoffUsesRawOSStdio(t *testing.T) {
	src, err := os.ReadFile("run_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	const allowed = "cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr"
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "os.Stdin") && !strings.Contains(trimmed, "os.Stdout") {
			continue
		}
		if trimmed == allowed {
			continue
		}
		t.Errorf("run_cmd.go:%d uses os.Stdin/os.Stdout outside the exec handoff — use d.In/d.Out instead:\n  %s", i+1, trimmed)
	}
}
