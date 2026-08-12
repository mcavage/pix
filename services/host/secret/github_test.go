package secret

import (
	"errors"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// github_test.go — the scope distinction, which is the whole point.
//
// An agent in a fresh sandbox committed a change and could not push, while pix's
// own host state told it github was available. The secret existed, pinned to a
// DIFFERENT sandbox, and a substring search on `sbx secret ls` cannot tell the
// difference.

const scopedListing = `SCOPE                    TYPE      NAME     SECRET
pix-runmylife-b9fccda4   service   github   (stored)

Note: 1 additional environment variable found. Run ` + "`sbx setup`" + ` to review and import.
`

const globalListing = `SCOPE    TYPE      NAME       SECRET
global   service   github     (stored)
global   service   anthropic  (stored)
`

// fakeSbx answers `secret ls` and `secret ls --global` separately, which is what
// the real command does.
func fakeSbx(t *testing.T, global, all string, fail bool) hostenv.Env {
	t.Helper()
	return hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/local/bin/sbx", nil },
		RunTimedFn: func(_ string, args ...string) (string, bool, error) {
			if fail {
				return "", false, errors.New("sbx exploded")
			}
			for _, a := range args {
				if a == "--global" {
					return global, false, nil
				}
			}
			return all, false, nil
		},
	}}
}

// TestGitHubSecretScopedIsNotGlobal is the reported failure, as a test. The
// credential is stored, and every sandbox but one still cannot push.
func TestGitHubSecretScopedIsNotGlobal(t *testing.T) {
	state, boxes := ProbeGitHubSecret(fakeSbx(t, "", scopedListing, false))
	if state != GitHubSecretScoped {
		t.Fatalf("state = %v, want GitHubSecretScoped: a secret pinned to one sandbox is not a credential every sandbox has", state)
	}
	if len(boxes) != 1 || boxes[0] != "pix-runmylife-b9fccda4" {
		t.Errorf("sandboxes = %v, want the one box it is pinned to, so the message can name it", boxes)
	}
}

func TestGitHubSecretGlobalIsReady(t *testing.T) {
	if state, _ := ProbeGitHubSecret(fakeSbx(t, globalListing, globalListing, false)); state != GitHubSecretGlobal {
		t.Errorf("state = %v, want GitHubSecretGlobal", state)
	}
}

func TestGitHubSecretAbsent(t *testing.T) {
	empty := "SCOPE    TYPE      NAME     SECRET\n"
	if state, _ := ProbeGitHubSecret(fakeSbx(t, empty, empty, false)); state != GitHubSecretAbsent {
		t.Errorf("state = %v, want GitHubSecretAbsent", state)
	}
}

// TestGitHubSecretUnknownWhenSbxCannotAnswer: a probe that did not run is not
// evidence that a credential is missing. Same rule the model-key probe follows.
func TestGitHubSecretUnknownWhenSbxCannotAnswer(t *testing.T) {
	if state, _ := ProbeGitHubSecret(fakeSbx(t, "", "", true)); state != GitHubSecretUnknown {
		t.Errorf("state = %v, want GitHubSecretUnknown when sbx errors", state)
	}
	absent := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "", errors.New("no sbx") },
	}}
	if state, _ := ProbeGitHubSecret(absent); state != GitHubSecretUnknown {
		t.Errorf("state = %v, want GitHubSecretUnknown when sbx is not installed", state)
	}
}

// TestGitHubSecretIgnoresLookalikeRows: the NAME column decides, not the line.
// A sandbox called "github-actions-runner" must not read as the credential.
func TestGitHubSecretIgnoresLookalikeRows(t *testing.T) {
	lookalike := `SCOPE                   TYPE      NAME       SECRET
github-actions-runner   service   anthropic  (stored)
`
	if state, _ := ProbeGitHubSecret(fakeSbx(t, lookalike, lookalike, false)); state != GitHubSecretAbsent {
		t.Errorf("state = %v, want GitHubSecretAbsent: a sandbox NAMED github holds no github secret", state)
	}
}
