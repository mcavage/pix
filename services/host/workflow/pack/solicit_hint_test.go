package pack

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/packinfo"
	"pix/host/sys/systest"
)

// solicit_hint_test.go — the moment a new user has to DO something.
//
// From Mark, after a successful run: "i got asked for gog keyring password, but
// like how would a new user even get to that point as a prereq?"
//
// They would not have. The pack authors guidance for exactly this — invent a
// passphrase, make a 1Password item, copy its secret reference — and that text
// was rendered only on the adoption screen, dozens of lines above the prompt, and
// then (once the screen was summarised) behind the detail key. The guidance
// existed and the user never met it where it was actionable.

func hintPack() *packinfo.Info {
	return &packinfo.Info{Manifest: packinfo.Manifest{
		Name: "work", Schema: 1,
		Integrations: []packinfo.Integration{
			{Name: "Google Workspace", MCP: "gw", Command: "gog", Env: "GOG_KEYRING_PASSWORD"},
			{Name: "BambooHR", MCP: "hr", Image: "hr:1", Env: "BAMBOOHR_API_KEY"},
		},
		Setup: []packinfo.SetupStep{{
			ID: "google-workspace", Required: true,
			Require: []packinfo.SetupRequire{{
				Kind: "op-ref", Env: "GOG_KEYRING_PASSWORD",
				Hint: "A passphrase YOU invent.\nIn 1Password: New Item -> Password -> Save.\nThen Copy Secret Reference.",
			}},
		}},
	}}
}

// fakeSecretEnv is a host with no `op` refs on disk. hasOp controls whether the
// 1Password CLI is installed.
func fakeSecretEnv(hasOp bool) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		LookPathFn: func(name string) (string, error) {
			if name == "op" && hasOp {
				return "/usr/local/bin/op", nil
			}
			if name == "op" {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + name, nil
		},
		// No op-refs.env, so every reference is unset.
		ReadFileFn: func(string) (string, error) { return "", errors.New("no such file") },
		GetenvFn:   func(string) string { return "" },
		HomeDirFn:  func() string { return "/tmp/nohome" },
	}}
}

// TestSolicitShowsThePacksHintAtThePrompt: the guidance belongs where the user
// has to act, not on a screen they already scrolled past.
func TestSolicitShowsThePacksHintAtThePrompt(t *testing.T) {
	var out bytes.Buffer
	// Skip both prompts; the hint must still have been printed before each.
	solicitPackCredentials(fakeSecretEnv(true), strings.NewReader("\n\n"), &out, true, hintPack())
	screen := out.String()

	for _, want := range []string{
		"A passphrase YOU invent.",
		"New Item -> Password -> Save.",
		"Copy Secret Reference.",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("the prompt omits the pack's guidance %q:\n%s", want, screen)
		}
	}
	// ORDER matters: guidance above, question last, so the cursor stops under the
	// thing being asked rather than under a paragraph.
	hintAt := strings.Index(screen, "A passphrase YOU invent.")
	askAt := strings.Index(screen, "op:// ref for GOG_KEYRING_PASSWORD")
	if hintAt < 0 || askAt < 0 || hintAt > askAt {
		t.Errorf("the guidance must print BEFORE the question (hint=%d ask=%d):\n%s", hintAt, askAt, screen)
	}
}

// TestSolicitDoesNotInventAHint: a credential the pack said nothing about gets no
// invented guidance. BAMBOOHR_API_KEY has no op-ref requirement in this fixture,
// so its prompt must stand alone rather than borrow the other one's text.
func TestSolicitDoesNotInventAHint(t *testing.T) {
	var out bytes.Buffer
	solicitPackCredentials(fakeSecretEnv(true), strings.NewReader("\n\n"), &out, true, hintPack())
	screen := out.String()
	bamboo := screen[strings.Index(screen, "BambooHR"):]
	if strings.Contains(bamboo, "passphrase") {
		t.Errorf("guidance leaked onto a credential that has none:\n%s", bamboo)
	}
}

// TestSolicitSaysWhyItCannotAskWithoutOp is the silence this used to be.
//
// The function returned early when the 1Password CLI was absent: no prompt, no
// reason. The user then met a setup failure naming `pix secret set`, which also
// needs `op` — every message pointed at a tool nothing had mentioned.
func TestSolicitSaysWhyItCannotAskWithoutOp(t *testing.T) {
	var out bytes.Buffer
	solicitPackCredentials(fakeSecretEnv(false), strings.NewReader(""), &out, true, hintPack())
	screen := out.String()
	if screen == "" {
		t.Fatal("a host without `op` was told nothing at all")
	}
	for _, want := range []string{
		"GOG_KEYRING_PASSWORD",
		"BAMBOOHR_API_KEY",
		"not on PATH",
		"op signin",
		"pix secret set GOG_KEYRING_PASSWORD",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("the no-op message omits %q:\n%s", want, screen)
		}
	}
	// It must NOT prompt: there is nothing to store into.
	if strings.Contains(screen, "Enter to skip") {
		t.Errorf("pix asked for a reference it cannot store:\n%s", screen)
	}
}

// TestSolicitSaysNothingWhenThereIsNothingToSay: a pack whose references are all
// set must not print a paragraph about `op` on every activation.
func TestSolicitSaysNothingWhenThereIsNothingToSay(t *testing.T) {
	filled := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "", errors.New("no op") },
		ReadFileFn: func(string) (string, error) {
			return "GOG_KEYRING_PASSWORD = op://v/i/f\nBAMBOOHR_API_KEY = op://v/i/g\n", nil
		},
		GetenvFn:  func(string) string { return "" },
		HomeDirFn: func() string { return "/tmp/nohome" },
	}}
	var out bytes.Buffer
	solicitPackCredentials(filled, strings.NewReader(""), &out, true, hintPack())
	if out.String() != "" {
		t.Errorf("nothing was missing, so nothing should be said:\n%s", out.String())
	}
}
