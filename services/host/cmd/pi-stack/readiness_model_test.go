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

	"pi-stack/host/config"
)

func TestModelReadiness_Verdicts(t *testing.T) {
	pulled := ollamaProbe{installed: true, daemonUp: true, listOK: true, listOut: "gemma4:latest\n"}
	cases := []struct {
		name      string
		p         ollamaProbe
		model     string
		want      verdict
		installed bool
	}{
		{"pulled -> ready", pulled, "gemma4", verdictReady, true},
		{"clean list without tag -> verified todo", pulled, "nomic-embed-text", verdictTodo, true},
		{"list failed -> unverifiable", ollamaProbe{installed: true, daemonUp: false}, "gemma4", verdictUnverifiable, true},
		{"not installed -> not configured", ollamaProbe{}, "gemma4", verdict(""), false},
	}
	for _, tc := range cases {
		m := modelReadiness("watcher", tc.model, "fact capture", tc.p, requirementOptional)
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
		modelReadiness("watcher", "gemma4", "fact capture", down, requirementOptional),
		modelReadiness("embed", "nomic-embed-text", "semantic recall", down, requirementOptional),
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
		modelReadiness("watcher", "qwen3.5:9b", "fact capture", p, requirementOptional),
		modelReadiness("bridge", "qwen3.5:9b", "local chat", p, requirementOptional),
		modelReadiness("embed", "nomic-embed-text", "semantic recall", p, requirementOptional),
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
		modelReadiness("watcher", "gemma4", "fact capture", none, requirementOptional),
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
	env := shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "ollama" {
				return "/usr/bin/ollama", nil
			}
			return "", fmt.Errorf("not found")
		},
		dial: func(int) bool { return true },
		probe: func(name string, args ...string) (string, bool, error) {
			probed = append(probed, strings.Join(append([]string{name}, args...), " "))
			return "", true, fmt.Errorf("context deadline exceeded") // wedged ollama
		},
		run: func(name string, args ...string) (string, error) {
			t.Fatalf("ollama list must go through the bounded probe, not raw run")
			return "", nil
		},
	}
	p := probeOllama(env)
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
	if got := ollamaVerifyFailureReason(ollamaProbe{installed: true, daemonUp: false}); !strings.Contains(got, ":11434 down") {
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
	env := shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "ollama" {
				return "/usr/bin/ollama", nil
			}
			return "", fmt.Errorf("not found")
		},
		dial: func(port int) bool { return port == 11434 },
		run:  func(name string, args ...string) (string, error) { return "", fmt.Errorf("boom") },
	}
	g := ollamaGroup(cfg, env)
	for _, c := range g.checks {
		if !strings.HasPrefix(strings.TrimSpace(c.label), "watcher") &&
			!strings.HasPrefix(strings.TrimSpace(c.label), "embed") {
			continue
		}
		if c.result() != verdictUnverifiable {
			t.Errorf("%s: a failed `ollama list` must be unverifiable, got %+v", c.label, c)
		}
		if c.todo != "" {
			t.Errorf("%s: an unverifiable model must not offer a pull todo, got %q", c.label, c.todo)
		}
	}
}

// TestDoctorOllama_DaemonDownIsOptionalTodo: installed but the daemon is
// down — a verified OPTIONAL todo naming the daemon start, which never blocks
// doctor's exit code.
func TestDoctorOllama_DaemonDownIsOptionalTodo(t *testing.T) {
	cfg := defaultCfg()
	env := shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "ollama" {
				return "/usr/bin/ollama", nil
			}
			return "", fmt.Errorf("not found")
		},
		dial: func(int) bool { return false },
		run:  func(name string, args ...string) (string, error) { return "", fmt.Errorf("daemon down") },
	}
	g := ollamaGroup(cfg, env)
	if len(g.checks) == 0 || g.checks[0].result() != verdictTodo {
		t.Fatalf("daemon down must be a verified todo, got %+v", g.checks)
	}
	if !strings.Contains(g.checks[0].todo, "ollama serve") {
		t.Errorf("the fix is starting the daemon, got %q", g.checks[0].todo)
	}
	if blockingCheck(g.checks[0].req(), g.checks[0].result()) {
		t.Error("ollama is optional — its todo must never block")
	}
}

// TestDoctorOllama_NotInstalledUnconfiguredIsNote: nothing configured depends
// on local models (memory NOT in services) — a missing ollama is an expected
// absence: a note, no todo. With memory enabled it becomes an install todo
// (covered by TestDoctor_SbxAbsent).
func TestDoctorOllama_NotInstalledUnconfiguredIsNote(t *testing.T) {
	cfg := &config.Config{MemoryWatcherModel: "gemma4", MemoryEmbedModel: "nomic-embed-text"}
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
		dial:     func(int) bool { return false },
	}
	g := ollamaGroup(cfg, env)
	if len(g.checks) == 0 || !g.checks[0].note {
		t.Fatalf("an uninstalled ollama with no configured dependents must be a note, got %+v", g.checks)
	}
	for _, c := range g.checks {
		if c.todo != "" && c.result() == verdictTodo {
			t.Errorf("no todos expected when nothing depends on ollama, got %+v", c)
		}
	}
}
