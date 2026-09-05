package pixhome

import (
	"path/filepath"
	"testing"
)

// TestDir_EnvOverride pins $PIX_HOME as the sole override, made absolute and
// cleaned.
func TestDir_EnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(EnvVar, tmp)
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	want := filepath.Clean(tmp)
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// TestDir_EnvOverride_RelativeMadeAbsolute proves a relative $PIX_HOME still
// resolves to a usable absolute path rather than being used verbatim.
func TestDir_EnvOverride_RelativeMadeAbsolute(t *testing.T) {
	t.Setenv(EnvVar, "relative-pix-home")
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Dir() = %q, want an absolute path", got)
	}
}

// TestDir_DefaultUnderHome proves the no-override default is exactly
// $HOME/.pix.
func TestDir_DefaultUnderHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(EnvVar, "")
	t.Setenv("HOME", tmp)
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	want := filepath.Join(tmp, ".pix")
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// TestDir_NoXDGFallback is the load-bearing regression: architecture §5 is
// explicit that v2 has no XDG fallback at all. Setting every XDG_* variable
// to a decoy location must have zero effect on Dir()'s answer.
func TestDir_NoXDGFallback(t *testing.T) {
	home := t.TempDir()
	decoy := t.TempDir()
	t.Setenv(EnvVar, "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", decoy)
	t.Setenv("XDG_DATA_HOME", decoy)
	t.Setenv("XDG_STATE_HOME", decoy)
	t.Setenv("XDG_CACHE_HOME", decoy)

	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	want := filepath.Join(home, ".pix")
	if got != want {
		t.Errorf("Dir() = %q, want %q (XDG_* must never influence PIX_HOME resolution)", got, want)
	}
}

// TestNew_LayoutMatchesArchitecture pins New(home)'s output against
// docs/design/pix-v2-architecture.md §5's literal tree, so a future edit to
// the layout has to change this test deliberately rather than by accident.
func TestNew_LayoutMatchesArchitecture(t *testing.T) {
	home := "/home/u/.pix"
	p := New(home)

	cases := map[string]string{
		"Git":                    "/home/u/.pix/.git",
		"Gitignore":              "/home/u/.pix/.gitignore",
		"README":                 "/home/u/.pix/README.md",
		"Context":                "/home/u/.pix/context",
		"ContextSkills":          "/home/u/.pix/context/skills",
		"ContextOutputStyles":    "/home/u/.pix/context/output-styles",
		"Envs":                   "/home/u/.pix/envs",
		"ConfigTOML":             "/home/u/.pix/config.toml",
		"SecretsEnv":             "/home/u/.pix/secrets.env",
		"Runtime":                "/home/u/.pix/runtime",
		"State":                  "/home/u/.pix/.state",
		"StateEffective":         "/home/u/.pix/.state/effective",
		"StateMemory":            "/home/u/.pix/.state/memory",
		"StateMemoryBackups":     "/home/u/.pix/.state/memory/backups",
		"StateSandboxes":         "/home/u/.pix/.state/sandboxes",
		"StateSessions":          "/home/u/.pix/.state/sessions",
		"StateTasks":             "/home/u/.pix/.state/tasks",
		"StateTrust":             "/home/u/.pix/.state/trust",
		"StateTrustEnvironments": "/home/u/.pix/.state/trust/environments",
	}
	got := map[string]string{
		"Git": p.Git, "Gitignore": p.Gitignore, "README": p.README,
		"Context": p.Context, "ContextSkills": p.ContextSkills, "ContextOutputStyles": p.ContextOutputStyles, "Envs": p.Envs,
		"ConfigTOML": p.ConfigTOML, "SecretsEnv": p.SecretsEnv, "Runtime": p.Runtime,
		"State": p.State, "StateEffective": p.StateEffective, "StateMemory": p.StateMemory,
		"StateMemoryBackups": p.StateMemoryBackups, "StateSandboxes": p.StateSandboxes,
		"StateSessions": p.StateSessions, "StateTasks": p.StateTasks,
		"StateTrust": p.StateTrust, "StateTrustEnvironments": p.StateTrustEnvironments,
	}
	for field, want := range cases {
		if got[field] != filepath.FromSlash(want) {
			t.Errorf("Paths.%s = %q, want %q", field, got[field], filepath.FromSlash(want))
		}
	}
}

func TestPaths_EnvironmentDirAndRuntimeVersionDir(t *testing.T) {
	p := New(filepath.FromSlash("/home/u/.pix"))
	if got, want := p.EnvironmentDir("work"), filepath.FromSlash("/home/u/.pix/envs/work"); got != want {
		t.Errorf("EnvironmentDir(work) = %q, want %q", got, want)
	}
	if got, want := p.RuntimeVersionDir("2.0.0"), filepath.FromSlash("/home/u/.pix/runtime/2.0.0"); got != want {
		t.Errorf("RuntimeVersionDir(2.0.0) = %q, want %q", got, want)
	}
}
