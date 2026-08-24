package main

// config_environment_cmd_test.go: end-to-end proof that `pix config set|unset
// environment` and `environments.<name>` refuse through the SHIPPED root
// (same runConfigParse helper as config_cmd_test.go), exit 2, and name the
// correct `pix env` command. There is no read-side assertion here: Wave C owns
// `pix env show|ls`, and `pix config get` for these keys already exits 2 via
// the generic unknown-key path (config_cmd_test.go's TestConfigCmd_GetRemovedKey
// pins that contract).

import (
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
)

func TestConfigCmd_SetEnvironmentRefused(t *testing.T) {
	d, out, _ := configDeps(t)
	err := runConfigParse([]string{"config", "set", "environment", "home"}, d)
	if err == nil || cli.ExitCode(err) != 2 {
		t.Fatalf("config set environment: err=%v, want a refusal (exit 2)", err)
	}
	if !strings.Contains(err.Error(), "pix env use") {
		t.Errorf("error should direct to `pix env use`, got %v", err)
	}
	if out.String() != "" {
		t.Errorf("stdout must stay clean on a refusal, got %q", out.String())
	}
}

func TestConfigCmd_UnsetEnvironmentRefused(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "unset", "environment"}, d)
	if err == nil || cli.ExitCode(err) != 2 {
		t.Fatalf("config unset environment: err=%v, want a refusal (exit 2)", err)
	}
	if !strings.Contains(err.Error(), "pix env use") {
		t.Errorf("error should direct to `pix env use`, got %v", err)
	}
}

func TestConfigCmd_SetEnvironmentsRegistryRefused(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "set", "environments.home", "/abs/home"}, d)
	if err == nil || cli.ExitCode(err) != 2 {
		t.Fatalf("config set environments.home: err=%v, want a refusal (exit 2)", err)
	}
	if !strings.Contains(err.Error(), "pix env add") {
		t.Errorf("error should direct to `pix env add`, got %v", err)
	}
}

func TestConfigCmd_UnsetEnvironmentsRegistryRefused(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "unset", "environments.home"}, d)
	if err == nil || cli.ExitCode(err) != 2 {
		t.Fatalf("config unset environments.home: err=%v, want a refusal (exit 2)", err)
	}
	if !strings.Contains(err.Error(), "pix env rm") {
		t.Errorf("error should direct to `pix env rm`, got %v", err)
	}
}

// A refusal must never write config.toml — the whole point is "no hand-edit
// path", so a refused set/unset leaving a mutated file would defeat it.
func TestConfigCmd_EnvironmentRefusalDoesNotWriteFile(t *testing.T) {
	d, _, _ := configDeps(t)
	_ = runConfigParse([]string{"config", "set", "environment", "home"}, d)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != "" {
		t.Errorf("Environment = %q after a refused set, want empty (nothing persisted)", cfg.Environment)
	}
}
