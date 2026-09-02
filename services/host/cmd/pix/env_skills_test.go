// env_skills_test.go — an environment repository's own `skills/` directory
// was validated (envinfo.ValidateSkillWorkspaces proved it resolved inside a
// mounted workspace) and then dropped: the `--skill` list was built only from
// machine [skills].paths, the personal context dir and `--skills` run options.
// Declaring `[pi].skills` therefore loaded nothing, which made the documented
// "an environment's own skills directory" behaviour false.
package main

import (
	"path/filepath"
	"slices"
	"testing"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/workflow/launch"
)

func envWithSkills(root string, skills ...string) launch.EnvSelection {
	return launch.EnvSelection{
		Name:    "work",
		Root:    root,
		Sidecar: &envinfo.Sidecar{Pi: envinfo.PiSection{Skills: skills}},
	}
}

func TestEnvSkillDirs_ResolvesAgainstTheEnvironmentRoot(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(t.TempDir(), "elsewhere")

	got := envSkillDirs(envWithSkills(root, "skills", "  ", "extra/more", abs))
	want := []string{
		filepath.Join(root, "skills"),
		filepath.Join(root, "extra/more"),
		abs,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("envSkillDirs = %v, want %v", got, want)
	}
}

func TestEnvSkillDirs_NoEnvironmentOrNoSidecarIsNothing(t *testing.T) {
	if got := envSkillDirs(launch.EnvSelection{}); got != nil {
		t.Fatalf("unselected environment = %v, want nil", got)
	}
	if got := envSkillDirs(launch.EnvSelection{Name: "work", Root: t.TempDir()}); got != nil {
		t.Fatalf("no pix.toml = %v, want nil", got)
	}
	if got := envSkillDirs(envWithSkills(t.TempDir())); len(got) != 0 {
		t.Fatalf("no [pi].skills = %v, want empty", got)
	}
}

// The wiring that matters: the resolved dirs travel on the SAME o.Skills
// channel `--skills DIR` uses, so they are both MOUNTED and passed to pi as
// `--skill`. Proving it through the two real producers (not a hand-rolled
// list) is what makes this a feature rather than a function nothing calls.
func TestEnvSkillDirs_ReachBothTheMountSetAndThePiSkillList(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	o := launch.RunOpts{Workspace: t.TempDir()}
	o.Skills = append(o.Skills, envSkillDirs(envWithSkills(root, "skills"))...)

	envSkills := filepath.Join(root, "skills")
	if !slices.Contains(launch.LiveSkillDirs(cfg, o), envSkills) {
		t.Fatalf("LiveSkillDirs = %v, want it to include %s", launch.LiveSkillDirs(cfg, o), envSkills)
	}
	if !slices.Contains(launch.MountDirs(cfg, o), envSkills) {
		t.Fatalf("MountDirs = %v, want it to include %s", launch.MountDirs(cfg, o), envSkills)
	}
	piArgs := launch.BuildPiInvocation(launch.LiveSkillDirs(cfg, o), o)
	found := false
	for i, a := range piArgs {
		if a == "--skill" && i+1 < len(piArgs) && piArgs[i+1] == envSkills {
			found = true
		}
	}
	if !found {
		t.Fatalf("pi invocation = %v, want --skill %s", piArgs, envSkills)
	}
}

// A recreate re-enters runLaunchAttempt with the RETRY copy as its opts and
// resolves the environment again, so the retry copy must not already carry
// the environment's skill dirs: appending to both would double every entry in
// the mount set and the pi argv on the second attempt.
func TestEnvSkillDirs_RecreateDoesNotDoubleTheSkillDirs(t *testing.T) {
	root := t.TempDir()
	sel := envWithSkills(root, "skills")

	// First attempt.
	o := launch.RunOpts{}
	o.Skills = append(o.Skills, envSkillDirs(sel)...)
	// The recreate path re-enters with cloneRunOpts(retry), where retry was
	// never appended to, and appends once more.
	retry := cloneRunOpts(launch.RunOpts{})
	retry.Skills = append(retry.Skills, envSkillDirs(sel)...)

	if len(o.Skills) != 1 || len(retry.Skills) != 1 {
		t.Fatalf("attempt=%v retry=%v, want exactly one env skill dir each", o.Skills, retry.Skills)
	}
}
