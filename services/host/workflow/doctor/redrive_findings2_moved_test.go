// Moved from cmd/pix: the subject is a doctor internal.
package doctor

import (
	"errors"
	"fmt"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/sys/systest"
	"strings"
	"testing"
	"time"
)

func TestMcpLocalCheck_PolicyDeniedVerdict(t *testing.T) {
	const hostBin = "/usr/local/bin/pix-host"
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" {
			return "/usr/bin/sbx", nil
		}
		return "", fmt.Errorf("%q not found", name)
	}, RunTimedFn: func(name string, args ...string) (string, bool, error) {
		switch strings.Join(append([]string{name}, args...), " ") {
		case "sbx mcp get slack":
			return "name: slack\ncommand: " + hostBin + " mcp slack\n", false, nil
		case hostBin + " mcp slack --list-tools":
			return "403 forbidden: access denied by org policy", false, errors.New("exit status 1")
		}
		return "", false, fmt.Errorf("no fake probe")
	}}, HostBinary: func() (string, error) { return hostBin, nil }}
	c := mcpLocalCheck(env, "slack", "slack\n")
	if c.Result() != readiness.VerdictDenied {
		t.Errorf("an explicit policy denial from the local probe must be readiness.VerdictDenied, got %+v", c)
	}
}

func TestGogRegistrationCheck_TriState(t *testing.T) {
	// Present in a successful listing -> ready.
	if c := gogRegistrationCheck("google-workspace\nslack\n", true, true); c.Result() != readiness.VerdictReady {
		t.Errorf("registered gog = %+v, want ready", c)
	}
	// Positively missing from a successful listing -> verified register TODO.
	c := gogRegistrationCheck("notion\n", true, true)
	if c.Result() != readiness.VerdictTodo || c.Todo != "pix mcp register" {
		t.Errorf("unregistered gog = %+v, want the register todo", c)
	}
	// Listing failed with sbx PRESENT -> unverifiable (daemon guidance), and
	// NEVER a false outstanding item.
	c = gogRegistrationCheck("", false, true)
	if c.Result() != readiness.VerdictUnverifiable || c.Todo != "" {
		t.Errorf("gog with failed listing (sbx present) = %+v, want unverifiable with no todo", c)
	}
	if !strings.Contains(c.Detail, "sbx daemon") {
		t.Errorf("sbx-present degrade should point at the daemon, got %q", c.Detail)
	}
	// sbx absent entirely -> unverifiable in-sandbox degrade, no todo.
	c = gogRegistrationCheck("", false, false)
	if c.Result() != readiness.VerdictUnverifiable || c.Todo != "" {
		t.Errorf("gog with sbx absent = %+v, want unverifiable with no todo", c)
	}
	if !strings.Contains(c.Detail, "sbx unavailable") {
		t.Errorf("sbx-absent degrade should say sbx unavailable, got %q", c.Detail)
	}
}

// --- finding 10: bounded probes (hanging fake executable) ------------------

// hangingExe writes an executable that sleeps far longer than any test
// deadline, standing in for a wedged sbx.

func TestRunDoctor_HangingMcpLsUnverifiable(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	hang := hangingProbe(t, 100*time.Millisecond)
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" {
			return "/usr/bin/sbx", nil
		}
		return "", fmt.Errorf("%q not found", name)
	}, RunTimedFn: func(name string, args ...string) (string, bool, error) {
		if name == "sbx" && len(args) == 2 && args[0] == "secret" && args[1] == "ls" {
			return "anthropic\nopenai\ngoogle\n", false, nil
		}
		return hang(name, args...)
	}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }, GetenvFn: func(string) string { return "" }, HomeDirFn: func() string { return "" }}}
	start := time.Now()
	r := RunDoctor(cfg, env)
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("RunDoctor took %s with a hanging `sbx mcp ls` — unbounded", el)
	}
	if r.SbxAbsent {
		t.Error("sbx is on PATH — a hanging mcp ls must not read as sbx absent")
	}
	c := findCheck(t, r.Groups[len(r.Groups)-1], "slack")
	if c.Result() != readiness.VerdictUnverifiable {
		t.Errorf("hanging mcp ls must render the server unverifiable, got %+v", c)
	}
}

// --- finding 11: sbxAbsent means POSITIVELY absent --------------------------
