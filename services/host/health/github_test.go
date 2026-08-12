package health

import (
	"context"
	"strings"
	"testing"
)

// TestGitHubRowSaysWhatIsWrong: the three answers need three different messages,
// because they are three different user actions.
func TestGitHubRowSaysWhatIsWrong(t *testing.T) {
	fix := "gh auth token | sbx secret set github"

	global := GitHubSecretProbe{Fix: fix, Scope: func() (int, []string) { return GitHubGlobal, nil }}
	if r := global.Check(context.Background()); r.Status != StatusReady {
		t.Errorf("a global secret must be ready, got %v", r.Status)
	}

	// SCOPED is the case that was silently passing. It is stored, and it is a gap.
	scoped := GitHubSecretProbe{Fix: fix, Scope: func() (int, []string) {
		return GitHubScoped, []string{"pix-runmylife-b9fccda4"}
	}}
	r := scoped.Check(context.Background())
	if r.Status == StatusReady {
		t.Error("a secret pinned to one sandbox must NOT report ready: every other sandbox cannot push")
	}
	if !strings.Contains(r.Evidence, "pix-runmylife-b9fccda4") {
		t.Errorf("the evidence must name the sandbox it is pinned to: %q", r.Evidence)
	}
	if !strings.Contains(r.Evidence, "created later") {
		t.Errorf("the evidence must say a later sandbox gets nothing, which is why this matters: %q", r.Evidence)
	}
	if r.Fix != fix {
		t.Errorf("Fix = %q, want the one command that repairs it", r.Fix)
	}

	absent := GitHubSecretProbe{Fix: fix, Scope: func() (int, []string) { return GitHubAbsent, nil }}
	if r := absent.Check(context.Background()); r.Status == StatusReady || r.Fix != fix {
		t.Errorf("an absent secret must be a gap with the fix, got status=%v fix=%q", r.Status, r.Fix)
	}
}

// TestGitHubRowNeverInventsAGap: sbx not answering is unknown, never "you have
// no credential". Reporting a gap here would send someone to re-set a secret
// that is already correct.
func TestGitHubRowNeverInventsAGap(t *testing.T) {
	unknown := GitHubSecretProbe{Fix: "x", Scope: func() (int, []string) { return GitHubUnknown, nil }}
	r := unknown.Check(context.Background())
	if r.Status != StatusUnknown {
		t.Errorf("status = %v, want StatusUnknown", r.Status)
	}
	if r.Fix != "" {
		t.Errorf("an unknown must not print a repair for a problem it did not prove: %q", r.Fix)
	}

	// And an unwired probe is unknown too, not ready.
	if r := (GitHubSecretProbe{}).Check(context.Background()); r.Status != StatusUnknown {
		t.Errorf("an unwired probe must be unknown, got %v", r.Status)
	}
}

// TestGitHubRowIsOptional: plenty of work never pushes, so this must not fail
// the exit code of every script that runs doctor.
func TestGitHubRowIsOptional(t *testing.T) {
	if (GitHubSecretProbe{}).Required() {
		t.Error("the github row must be optional")
	}
}
