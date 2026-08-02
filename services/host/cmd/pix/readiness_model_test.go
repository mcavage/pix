package main

// modelreadiness_test.go covers the shared local-model readiness seam:
// verdicts derived from ONE bounded ollama probe, and — the load-bearing
// invariant ported from the prior branch (R1-09) — an UNVERIFIABLE tag
// (daemon down / `ollama list` failed) is NEVER reported or acted on as
// "missing": the two sets are disjoint, so nothing can ever offer to re-pull
// a model it could not actually check.

import (
	"fmt"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/sys/systest"
)

func TestModelReadiness_Verdicts(t *testing.T) {
	pulled := ollamaProbe{installed: true, daemonUp: true, listOK: true, listOut: "gemma4:latest\n"}
	cases := []struct {
		name      string
		p         ollamaProbe
		model     string
		want      readiness.Verdict
		installed bool
	}{
		{"pulled -> ready", pulled, "gemma4", readiness.VerdictReady, true},
		{"clean list without tag -> verified todo", pulled, "nomic-embed-text", readiness.VerdictTodo, true},
		{"list failed -> unverifiable", ollamaProbe{installed: true, daemonUp: false}, "gemma4", readiness.VerdictUnverifiable, true},
		{"not installed -> not configured", ollamaProbe{}, "gemma4", readiness.Verdict(""), false},
	}
	for _, tc := range cases {
		m := modelReadiness("watcher", tc.model, "fact capture", tc.p, readiness.RequirementOptional)
		if m.Installed != tc.installed {
			t.Errorf("%s: Installed = %v, want %v", tc.name, m.Installed, tc.installed)
		}
		if tc.installed && m.Verdict != tc.want {
			t.Errorf("%s: verdict = %q, want %q", tc.name, m.Verdict, tc.want)
		}
		if m.PullCmd != "ollama pull "+tc.model {
			t.Errorf("%s: PullCmd = %q", tc.name, m.PullCmd)
		}
	}
}

// TestComputeMissingModels_UnverifiableIsNeverMissing is the R1-09 port: a
// stopped daemon / failed `ollama list` proves nothing about whether a tag is
// pulled, so unverifiable tags must never enter the missing set — they land
// in the DISJOINT unverifiable set instead.
func TestComputeMissingModels_UnverifiableIsNeverMissing(t *testing.T) {
	down := ollamaProbe{installed: true, daemonUp: false} // list failed
	rs := []ModelReadiness{
		modelReadiness("watcher", "gemma4", "fact capture", down, readiness.RequirementOptional),
		modelReadiness("embed", "nomic-embed-text", "semantic recall", down, readiness.RequirementOptional),
	}
	if missing := computeMissingModels(rs); len(missing) != 0 {
		t.Fatalf("unverifiable tags must NEVER be reported as missing, got %+v", missing)
	}
	unv := computeUnverifiableModels(rs)
	if len(unv) != 2 {
		t.Fatalf("expected both tags in the unverifiable set, got %+v", unv)
	}
}

// TestComputeMissingModels_ConfirmedMissingAndRoleDedup: a clean `ollama
// list` without the tags IS a confirmed gap; a tag shared by two roles is
// named once with both roles.
func TestComputeMissingModels_ConfirmedMissingAndRoleDedup(t *testing.T) {
	p := ollamaProbe{installed: true, daemonUp: true, listOK: true, listOut: "other:latest\n"}
	rs := []ModelReadiness{
		modelReadiness("watcher", "qwen3.5:9b", "fact capture", p, readiness.RequirementOptional),
		modelReadiness("bridge", "qwen3.5:9b", "local chat", p, readiness.RequirementOptional),
		modelReadiness("embed", "nomic-embed-text", "semantic recall", p, readiness.RequirementOptional),
	}
	missing := computeMissingModels(rs)
	if len(missing) != 2 {
		t.Fatalf("expected 2 distinct missing tags, got %+v", missing)
	}
	if missing[0].tag != "qwen3.5:9b" || strings.Join(missing[0].roles, ",") != "watcher,bridge" {
		t.Errorf("shared tag must be named once with every dependent role, got %+v", missing[0])
	}
	if len(computeUnverifiableModels(rs)) != 0 {
		t.Errorf("confirmed-missing tags must not also be unverifiable")
	}
}

// TestComputeModels_NotInstalledIsNeitherMissingNorUnverifiable: with ollama
// absent nothing is claimed about any tag — not-configured entries stay out
// of BOTH sets.
func TestComputeModels_NotInstalledIsNeitherMissingNorUnverifiable(t *testing.T) {
	none := ollamaProbe{}
	rs := []ModelReadiness{
		modelReadiness("watcher", "gemma4", "fact capture", none, readiness.RequirementOptional),
	}
	if len(computeMissingModels(rs)) != 0 || len(computeUnverifiableModels(rs)) != 0 {
		t.Fatalf("not-installed must be excluded from both sets, got missing=%v unv=%v",
			computeMissingModels(rs), computeUnverifiableModels(rs))
	}
}

// TestProbeOllama_Bounded: the `ollama list` exec goes through the bounded
// probe machinery when wired, and a timeout classifies as list-unverified.
func TestProbeOllama_Bounded(t *testing.T) {
	var probed []string
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "ollama" {
			return "/usr/bin/ollama", nil
		}
		return "", fmt.Errorf("not found")
	}, DialLocalFn: func(int) bool { return true }, RunTimedFn: func(name string, args ...string) (string, bool, error) {
		probed = append(probed, strings.Join(append([]string{name}, args...), " "))
		return "", true, fmt.Errorf("context deadline exceeded") // wedged ollama
	}, RunFn: func(name string, args ...string) (string, error) {
		t.Fatalf("ollama list must go through the bounded probe, not raw run")
		return "", nil
	}}}
	p := probeOllamaAt(env, effectiveOllamaEndpoint(&config.Config{}, env))
	if !p.installed || p.listOK {
		t.Fatalf("expected installed with an unverified list, got %+v", p)
	}
	if len(probed) != 1 || probed[0] != "ollama list" {
		t.Errorf("expected exactly one bounded `ollama list` probe, got %v", probed)
	}
}

// TestOllamaVerifyFailureReason distinguishes a down daemon from a daemon
// that answered but whose `ollama list` call itself failed — the receipt
// diagnostic setup (S08) will print for unverifiable tags.
func TestOllamaVerifyFailureReason(t *testing.T) {
	if got := ollamaVerifyFailureReason(ollamaProbe{installed: true, daemonUp: false}); !strings.Contains(got, "not answering at http://127.0.0.1:11434") {
		t.Errorf("down daemon reason wrong: %q", got)
	}
	if got := ollamaVerifyFailureReason(ollamaProbe{installed: true, daemonUp: true}); !strings.Contains(got, "ollama list") {
		t.Errorf("list-failure reason wrong: %q", got)
	}
}

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
