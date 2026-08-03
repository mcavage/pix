// Moved from readiness/axis: the subject is a doctor GROUP builder, which
// composes axis checks rather than being one.
package doctor

import (
	"fmt"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/sys/systest"
)

// TestDoctorOllama_ListFailureIsUnverifiableNotMissing: ollama installed and
// the daemon dial answers, but `ollama list` itself fails — the model lines
// must be UNVERIFIABLE (⚠, no pull todo), never a confirmed "not pulled".
func TestDoctorOllama_ListFailureIsUnverifiableNotMissing(t *testing.T) {
	cfg := defaultCfg()
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "ollama" {
			return "/usr/bin/ollama", nil
		}
		return "", fmt.Errorf("not found")
	}, DialLocalFn: func(port int) bool { return port == 11434 }, RunFn: func(name string, args ...string) (string, error) { return "", fmt.Errorf("boom") }}}
	g := ollamaGroup(cfg, env)
	for _, c := range g.Checks {
		if !strings.HasPrefix(strings.TrimSpace(c.Label), "watcher") &&
			!strings.HasPrefix(strings.TrimSpace(c.Label), "embed") {
			continue
		}
		if c.Result() != readiness.VerdictUnverifiable {
			t.Errorf("%s: a failed `ollama list` must be unverifiable, got %+v", c.Label, c)
		}
		if c.Todo != "" {
			t.Errorf("%s: an unverifiable model must not offer a pull todo, got %q", c.Label, c.Todo)
		}
	}
}

// TestDoctorOllama_DaemonDownIsOptionalTodo: installed but the daemon is
// down — a verified OPTIONAL todo naming the daemon start, which never blocks
// doctor's exit code.

// TestDoctorOllama_DaemonDownIsOptionalTodo: installed but the daemon is
// down — a verified OPTIONAL todo naming the daemon start, which never blocks
// doctor's exit code.
func TestDoctorOllama_DaemonDownIsOptionalTodo(t *testing.T) {
	cfg := defaultCfg()
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "ollama" {
			return "/usr/bin/ollama", nil
		}
		return "", fmt.Errorf("not found")
	}, DialLocalFn: func(int) bool { return false }, RunFn: func(name string, args ...string) (string, error) { return "", fmt.Errorf("daemon down") }}}
	g := ollamaGroup(cfg, env)
	if len(g.Checks) == 0 || g.Checks[0].Result() != readiness.VerdictTodo {
		t.Fatalf("daemon down must be a verified todo, got %+v", g.Checks)
	}
	if !strings.Contains(g.Checks[0].Todo, "ollama serve") {
		t.Errorf("the fix is starting the daemon, got %q", g.Checks[0].Todo)
	}
	if readiness.BlockingCheck(g.Checks[0].Req(), g.Checks[0].Result()) {
		t.Error("ollama is optional — its todo must never block")
	}
}

// TestDoctorOllama_NotInstalledUnconfiguredIsNote: nothing configured depends
// on local models (memory NOT in services) — a missing ollama is an expected
// absence: a note, no todo. With memory enabled it becomes an install todo
// (covered by TestDoctor_SbxAbsent).

// TestDoctorOllama_NotInstalledUnconfiguredIsNote: nothing configured depends
// on local models (memory NOT in services) — a missing ollama is an expected
// absence: a note, no todo. With memory enabled it becomes an install todo
// (covered by TestDoctor_SbxAbsent).
func TestDoctorOllama_NotInstalledUnconfiguredIsNote(t *testing.T) {
	cfg := &config.Config{MemoryWatcherModel: "gemma4", MemoryEmbedModel: "nomic-embed-text"}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }, DialLocalFn: func(int) bool { return false }}}
	g := ollamaGroup(cfg, env)
	if len(g.Checks) == 0 || !g.Checks[0].Note {
		t.Fatalf("an uninstalled ollama with no configured dependents must be a note, got %+v", g.Checks)
	}
	for _, c := range g.Checks {
		if c.Todo != "" && c.Result() == readiness.VerdictTodo {
			t.Errorf("no todos expected when nothing depends on ollama, got %+v", c)
		}
	}
}
