package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMachineConfig_AbsentIsZeroValue(t *testing.T) {
	home := t.TempDir()
	c, err := LoadMachineConfig(home)
	if err != nil {
		t.Fatalf("LoadMachineConfig: %v", err)
	}
	if c != (MachineConfig{}) {
		t.Fatalf("expected zero value, got %+v", c)
	}
}

func TestSaveThenLoadMachineConfig_RoundTrips(t *testing.T) {
	home := t.TempDir()
	want := MachineConfig{Environment: "work", InferenceBackend: InferenceBackendOllama}
	if err := SaveMachineConfig(home, want); err != nil {
		t.Fatalf("SaveMachineConfig: %v", err)
	}
	got, err := LoadMachineConfig(home)
	if err != nil {
		t.Fatalf("LoadMachineConfig: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSaveMachineConfig_RejectsInvalidBackend(t *testing.T) {
	home := t.TempDir()
	err := SaveMachineConfig(home, MachineConfig{InferenceBackend: "vllm"})
	if err == nil {
		t.Fatal("expected an error for an unsupported inference backend")
	}
}

func TestLoadMachineConfig_RejectsUnrecognizedKey(t *testing.T) {
	home := t.TempDir()
	path := MachineConfigPath(home)
	if err := os.WriteFile(path, []byte("packs = [\"foo\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadMachineConfig(home)
	if err == nil {
		t.Fatal("expected an error for a v1-only key (packs)")
	}
	if !strings.Contains(err.Error(), "packs") {
		t.Fatalf("error %q does not name the offending key", err)
	}
}

func TestLoadMachineConfig_RejectsInvalidBackend(t *testing.T) {
	home := t.TempDir()
	path := MachineConfigPath(home)
	if err := os.WriteFile(path, []byte("inference_backend = \"vllm\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMachineConfig(home); err == nil {
		t.Fatal("expected an error for an invalid inference_backend value read back from disk")
	}
}

func TestMachineConfigPath(t *testing.T) {
	home := "/tmp/example-pix-home"
	got := MachineConfigPath(home)
	want := filepath.Join(home, "config.toml")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSaveMachineConfig_FileMode0600(t *testing.T) {
	home := t.TempDir()
	if err := SaveMachineConfig(home, MachineConfig{Environment: "home"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(MachineConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSaveMachineConfig_OmitsEmptyFields(t *testing.T) {
	home := t.TempDir()
	if err := SaveMachineConfig(home, MachineConfig{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(MachineConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("expected an empty file for a zero-value config, got %q", data)
	}
}
