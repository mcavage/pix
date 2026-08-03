// Moved from cmd/pix: the subject is a doctor internal.
package doctor

import (
	"fmt"
	"path/filepath"
	"pix/host/readiness"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

// TestMcpLocalCheck_RejectedWrapperNeverProbed: doctor reads back a
// registration whose op-run wrapper deviates from the launcher grammar
// (alternate env file). The registration must classify unverifiable and the
// command must NEVER be executed as a probe.
func TestMcpLocalCheck_RejectedWrapperNeverProbed(t *testing.T) {
	env := f2Env()
	reg := f2Op + " run --no-masking --env-file=/tmp/evil.env -- " + f2Host + " mcp slack"
	systest.Of(env.System).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if strings.Contains(key, "--list-tools") {
			t.Fatalf("doctor must never probe a rejected registration: %s", key)
		}
		if key == "sbx mcp get slack" {
			return "name: slack\ncommand: " + reg + "\n", false, nil
		}
		return "", false, fmt.Errorf("no fake output for %q", key)
	}
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		t.Fatalf("rejected registration must never be exec'd: %s %v", name, args)
		return "", nil
	}
	c := mcpLocalCheck(env, "slack", "slack\n")
	if c.Result() != readiness.VerdictUnverifiable || !strings.Contains(c.Detail, "never executed") {
		t.Errorf("rejected wrapper = %+v, want unverifiable + never-executed note", c)
	}
}

// TestGogParse_RejectedWrapperFallsThrough: an op-wrapped gog registration
// with a non-launcher env file no longer parses as a confident registered
// command, so doctor falls back to the reconstruction — the registered
// command itself is never probed.

func TestGogParse_RejectedWrapperFallsThrough(t *testing.T) {
	env := f2Env()
	line := "name: gog\ncommand: " + f2Op + " run --no-masking --env-file=/tmp/evil.env -- " + f2Gog + " --account you@example.com mcp\n"
	if argv, ok := parseGogCommandLine(env, line); ok {
		t.Errorf("a non-launcher wrapper must not parse as a confident gog command, got %v", argv)
	}
	canonical := "name: gog\ncommand: " + f2Op + " run --no-masking --env-file=" + f2Refs + " -- " + f2Gog + " --account you@example.com mcp\n"
	if _, ok := parseGogCommandLine(env, canonical); !ok {
		t.Error("the canonical launcher wrapper must still parse")
	}
}

var _ = filepath.Clean // keep filepath imported if fixtures above change
