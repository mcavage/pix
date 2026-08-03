package axis

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
	pulled := OllamaProbe{Installed: true, DaemonUp: true, ListOK: true, ListOut: "gemma4:latest\n"}
	cases := []struct {
		name      string
		p         OllamaProbe
		model     string
		want      readiness.Verdict
		Installed bool
	}{
		{"pulled -> ready", pulled, "gemma4", readiness.VerdictReady, true},
		{"clean list without tag -> verified todo", pulled, "nomic-embed-text", readiness.VerdictTodo, true},
		{"list failed -> unverifiable", OllamaProbe{Installed: true, DaemonUp: false}, "gemma4", readiness.VerdictUnverifiable, true},
		{"not installed -> not configured", OllamaProbe{}, "gemma4", readiness.Verdict(""), false},
	}
	for _, tc := range cases {
		m := EvalModel("watcher", tc.model, "fact capture", tc.p, readiness.RequirementOptional)
		if m.Installed != tc.Installed {
			t.Errorf("%s: Installed = %v, want %v", tc.name, m.Installed, tc.Installed)
		}
		if tc.Installed && m.Verdict != tc.want {
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
	down := OllamaProbe{Installed: true, DaemonUp: false} // list failed
	rs := []ModelReadiness{
		EvalModel("watcher", "gemma4", "fact capture", down, readiness.RequirementOptional),
		EvalModel("embed", "nomic-embed-text", "semantic recall", down, readiness.RequirementOptional),
	}
	if missing := ComputeMissingModels(rs); len(missing) != 0 {
		t.Fatalf("unverifiable tags must NEVER be reported as missing, got %+v", missing)
	}
	unv := ComputeUnverifiableModels(rs)
	if len(unv) != 2 {
		t.Fatalf("expected both tags in the unverifiable set, got %+v", unv)
	}
}

// TestComputeMissingModels_ConfirmedMissingAndRoleDedup: a clean `ollama
// list` without the tags IS a confirmed gap; a tag shared by two roles is
// named once with both roles.
func TestComputeMissingModels_ConfirmedMissingAndRoleDedup(t *testing.T) {
	p := OllamaProbe{Installed: true, DaemonUp: true, ListOK: true, ListOut: "other:latest\n"}
	rs := []ModelReadiness{
		EvalModel("watcher", "qwen3.5:9b", "fact capture", p, readiness.RequirementOptional),
		EvalModel("bridge", "qwen3.5:9b", "local chat", p, readiness.RequirementOptional),
		EvalModel("embed", "nomic-embed-text", "semantic recall", p, readiness.RequirementOptional),
	}
	missing := ComputeMissingModels(rs)
	if len(missing) != 2 {
		t.Fatalf("expected 2 distinct missing tags, got %+v", missing)
	}
	if missing[0].Tag != "qwen3.5:9b" || strings.Join(missing[0].Roles, ",") != "watcher,bridge" {
		t.Errorf("shared tag must be named once with every dependent role, got %+v", missing[0])
	}
	if len(ComputeUnverifiableModels(rs)) != 0 {
		t.Errorf("confirmed-missing tags must not also be unverifiable")
	}
}

// TestComputeModels_NotInstalledIsNeitherMissingNorUnverifiable: with ollama
// absent nothing is claimed about any tag — not-configured entries stay out
// of BOTH sets.
func TestComputeModels_NotInstalledIsNeitherMissingNorUnverifiable(t *testing.T) {
	none := OllamaProbe{}
	rs := []ModelReadiness{
		EvalModel("watcher", "gemma4", "fact capture", none, readiness.RequirementOptional),
	}
	if len(ComputeMissingModels(rs)) != 0 || len(ComputeUnverifiableModels(rs)) != 0 {
		t.Fatalf("not-installed must be excluded from both sets, got missing=%v unv=%v",
			ComputeMissingModels(rs), ComputeUnverifiableModels(rs))
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
	p := ProbeOllamaAt(env, EffectiveOllamaEndpoint(&config.Config{}, env))
	if !p.Installed || p.ListOK {
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
	if got := OllamaVerifyFailureReason(OllamaProbe{Installed: true, DaemonUp: false}); !strings.Contains(got, "not answering at http://127.0.0.1:11434") {
		t.Errorf("down daemon reason wrong: %q", got)
	}
	if got := OllamaVerifyFailureReason(OllamaProbe{Installed: true, DaemonUp: true}); !strings.Contains(got, "ollama list") {
		t.Errorf("list-failure reason wrong: %q", got)
	}
}
