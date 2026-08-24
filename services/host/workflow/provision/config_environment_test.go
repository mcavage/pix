package provision

import (
	"strings"
	"testing"

	"pix/host/config"
)

// config_environment_test.go: `environment` and every `environments.<name>`
// key are real, recognized config-schema fields (config.Config.Environment /
// .Environments, E1.5), but they have NO hand-edit path — `pix env use` and
// `pix env add`/`pix env forget` (Wave C) are the only writers. ApplyConfigChange
// must refuse both set and unset for both shapes, name the correct `pix env`
// command, and leave cfg untouched (a refusal is not a partial write).

func TestApplyConfigChange_RefusesEnvironmentSet(t *testing.T) {
	cfg := &config.Config{}
	_, err := ApplyConfigChange(cfg, false, "environment", []string{"home"})
	if err == nil {
		t.Fatal("expected a refusal for `config set environment`")
	}
	if !strings.Contains(err.Error(), "pix env use") {
		t.Errorf("error should direct to `pix env use`, got %v", err)
	}
}

func TestApplyConfigChange_RefusesEnvironmentUnset(t *testing.T) {
	cfg := &config.Config{}
	_, err := ApplyConfigChange(cfg, true, "environment", nil)
	if err == nil {
		t.Fatal("expected a refusal for `config unset environment`")
	}
	if !strings.Contains(err.Error(), "pix env use") {
		t.Errorf("error should direct to `pix env use`, got %v", err)
	}
}

func TestApplyConfigChange_RefusesEnvironmentsRegistrySet(t *testing.T) {
	cfg := &config.Config{}
	_, err := ApplyConfigChange(cfg, false, "environments.home", []string{"/abs/home"})
	if err == nil {
		t.Fatal("expected a refusal for `config set environments.<name>`")
	}
	if !strings.Contains(err.Error(), "pix env add") {
		t.Errorf("error should direct to `pix env add`, got %v", err)
	}
	if !strings.Contains(err.Error(), "home") {
		t.Errorf("error should name the environment, got %v", err)
	}
}

func TestApplyConfigChange_RefusesEnvironmentsRegistryUnset(t *testing.T) {
	cfg := &config.Config{}
	_, err := ApplyConfigChange(cfg, true, "environments.home", nil)
	if err == nil {
		t.Fatal("expected a refusal for `config unset environments.<name>`")
	}
	if !strings.Contains(err.Error(), "pix env forget") {
		t.Errorf("error should direct to `pix env forget`, got %v", err)
	}
	if !strings.Contains(err.Error(), "home") {
		t.Errorf("error should name the environment, got %v", err)
	}
}

// A refused set/unset must not mutate cfg — no partial writes for a key that
// has no hand-edit path at all.
func TestApplyConfigChange_EnvironmentRefusalDoesNotMutate(t *testing.T) {
	cfg := &config.Config{Environment: "existing", Environments: map[string]string{"existing": "/abs/existing"}}

	if _, err := ApplyConfigChange(cfg, false, "environment", []string{"new"}); err == nil {
		t.Fatal("expected a refusal")
	}
	if cfg.Environment != "existing" {
		t.Errorf("Environment mutated by a refused set: got %q, want unchanged %q", cfg.Environment, "existing")
	}

	if _, err := ApplyConfigChange(cfg, true, "environments.existing", nil); err == nil {
		t.Fatal("expected a refusal")
	}
	if _, ok := cfg.Environments["existing"]; !ok {
		t.Error("environments map mutated by a refused unset")
	}
}

// ConfigKeysHelp must mention environment/environments so `pix config --help`
// stays discoverable even though the keys are refused, and must not claim
// they are settable.
func TestConfigKeysHelp_MentionsEnvironmentRefusal(t *testing.T) {
	if !strings.Contains(ConfigKeysHelp, "pix env") {
		t.Errorf("ConfigKeysHelp should point environment/environments readers at `pix env`, got:\n%s", ConfigKeysHelp)
	}
}

// The exact key `environments` (no `.<name>` suffix — e.g. a hand-typed
// `pix config set environments ...` groping for the registry) must hit the
// SAME owned-key refusal as `environments.<name>`, not fall through to the
// generic "unknown key" path: it is a real config-schema field name, and the
// generic path would hide that Pix recognizes it at all.
func TestApplyConfigChange_RefusesExactEnvironmentsKeySet(t *testing.T) {
	cfg := &config.Config{}
	_, err := ApplyConfigChange(cfg, false, "environments", []string{"home", "/abs/home"})
	if err == nil {
		t.Fatal("expected a refusal for `config set environments`")
	}
	if !strings.Contains(err.Error(), "pix env add") {
		t.Errorf("error should direct to `pix env add`, got %v", err)
	}
	if strings.Contains(err.Error(), "unknown key") {
		t.Errorf("exact `environments` key must hit the owned-key refusal, not the generic unknown-key path, got %v", err)
	}
}

func TestApplyConfigChange_RefusesExactEnvironmentsKeyUnset(t *testing.T) {
	cfg := &config.Config{}
	_, err := ApplyConfigChange(cfg, true, "environments", nil)
	if err == nil {
		t.Fatal("expected a refusal for `config unset environments`")
	}
	if !strings.Contains(err.Error(), "pix env forget") {
		t.Errorf("error should direct to `pix env forget`, got %v", err)
	}
	if strings.Contains(err.Error(), "unknown key") {
		t.Errorf("exact `environments` key must hit the owned-key refusal, not the generic unknown-key path, got %v", err)
	}
}

// An environment literally NAMED "environments" produces the registry key
// `environments.environments` — a real per-name entry, not the bare
// `environments` key. environmentKeyRefusal must tell these apart by the
// EXACT key string (`key == "environments"` vs `environments.<name>`), never
// by inferring exactness from the trimmed suffix: trimming the
// "environments." prefix off "environments.environments" leaves "environments"
// too, so a suffix-equality check collapses the two and silently drops the
// specific env name from the guidance, pointing at `pix env add <name>`
// instead of `pix env add environments`.
func TestApplyConfigChange_RefusesEnvironmentNamedEnvironmentsSet(t *testing.T) {
	cfg := &config.Config{}
	_, err := ApplyConfigChange(cfg, false, "environments.environments", []string{"/abs/environments"})
	if err == nil {
		t.Fatal("expected a refusal for `config set environments.environments`")
	}
	if !strings.Contains(err.Error(), "environments.environments") {
		t.Errorf("error should name the specific key `environments.environments`, got %v", err)
	}
	if !strings.Contains(err.Error(), "pix env add environments") {
		t.Errorf("error should direct to `pix env add environments` (the specific env name), got %v", err)
	}
	if strings.Contains(err.Error(), "<name>") {
		t.Errorf("error must not fall back to the generic bare-key placeholder, got %v", err)
	}
}

func TestApplyConfigChange_RefusesEnvironmentNamedEnvironmentsUnset(t *testing.T) {
	cfg := &config.Config{}
	_, err := ApplyConfigChange(cfg, true, "environments.environments", nil)
	if err == nil {
		t.Fatal("expected a refusal for `config unset environments.environments`")
	}
	if !strings.Contains(err.Error(), "environments.environments") {
		t.Errorf("error should name the specific key `environments.environments`, got %v", err)
	}
	if !strings.Contains(err.Error(), "pix env forget environments") {
		t.Errorf("error should direct to `pix env forget environments` (the specific env name), got %v", err)
	}
	if strings.Contains(err.Error(), "<name>") {
		t.Errorf("error must not fall back to the generic bare-key placeholder, got %v", err)
	}
}
